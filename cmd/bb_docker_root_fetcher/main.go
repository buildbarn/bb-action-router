package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/buildbarn/bb-action-router/pkg/actionrouter"
	"github.com/buildbarn/bb-action-router/pkg/blobstore"
	"github.com/buildbarn/bb-action-router/pkg/docker"
	"github.com/buildbarn/bb-action-router/pkg/ephemeralcas"
	"github.com/buildbarn/bb-action-router/pkg/fetcher"
	"github.com/buildbarn/bb-action-router/pkg/proto/configuration/docker_root_fetcher"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-remote-execution/pkg/builder"
	"github.com/buildbarn/bb-remote-execution/pkg/cas"
	bb_blobstore "github.com/buildbarn/bb-storage/pkg/blobstore"
	bb_digest "github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/buildbarn/bb-storage/pkg/filesystem"
	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
	"github.com/buildbarn/bb-storage/pkg/global"
	"github.com/buildbarn/bb-storage/pkg/program"
	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/google/go-containerregistry/pkg/name"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type connectionHandler struct {
	server  *fetcher.Server
	metrics *fetcher.Metrics
}

func (h *connectionHandler) handleConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	fmt.Fprintf(conn, "HI\n")

	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		log.Printf("Error reading from connection: %v", err)
		return
	}
	line = strings.TrimSpace(line)

	parts := strings.SplitN(line, " ", 2)
	if len(parts) != 2 || parts[0] != "ACQUIRE" {
		fmt.Fprintf(conn, "ERROR invalid request: %s\n", line)
		return
	}
	imageRef := parts[1]

	log.Printf("ACQUIRE: %s", imageRef)

	start := time.Now()
	path, release, err := h.server.Acquire(ctx, imageRef)
	h.metrics.AcquireDuration.Record(h.metrics.Ctx, time.Since(start).Seconds())

	if err != nil {
		log.Printf("ACQUIRE failed for %s: %v", imageRef, err)
		fmt.Fprintf(conn, "ERROR %v\n", err)
		return
	}
	defer release()

	log.Printf("ACQUIRE OK for %s: %s", imageRef, path)
	fmt.Fprintf(conn, "OK %s\n", path)

	// Block until the client closes the connection. The helper keeps the
	// socket open for the lifetime of the action, so EOF means the action
	// has exited and we can release the root.
	buf := make([]byte, 1)
	_, _ = conn.Read(buf)

	log.Printf("Connection closed — releasing %s", imageRef)
}

// materializer pulls a docker image into a per-call ephemeral CAS, then
// materializes the resulting tree into a directory under rootsDir. The
// ephemeral CAS is removed once materialization completes (success or
// failure), leaving only the materialized root on disk.
type materializer struct {
	rootsDir string
	// openDirLimit is retained for configuration compatibility, but is
	// currently unused: the naive build directory used to materialize
	// trees does not expose an open-directory limit.
	openDirLimit     uint32
	fetchParallelism uint32
	maxMessageSize   int
	digestFunction   bb_digest.Function
	puller           *docker.ImagePuller
	buildUser        blobstore.UnixUser
	metrics          *fetcher.Metrics
}

func (m *materializer) Materialize(ctx context.Context, imageRef string) (string, error) {
	st := "success"
	path, err := m.materialize(ctx, imageRef)
	if err != nil {
		st = "error"
	}
	m.metrics.ImagePullCount.Add(m.metrics.Ctx, 1, metric.WithAttributes(
		attribute.String("image", imageRef),
		attribute.String("status", st),
	))
	return path, err
}

// rootFetcher bundles the CAS fetchers used to materialize an image
// tree (rooted at a Directory digest) into a local directory via a
// naive build directory.
type rootFetcher struct {
	directoryFetcher cas.DirectoryFetcher
	fileFetcher      cas.FileFetcher
	semaphore        *semaphore.Weighted
	cas              bb_blobstore.BlobAccess
}

func (m *materializer) newRootFetcher(blobAccess bb_blobstore.BlobAccess) *rootFetcher {
	return &rootFetcher{
		directoryFetcher: cas.NewBlobAccessDirectoryFetcher(blobAccess, m.maxMessageSize, 0),
		fileFetcher:      cas.NewBlobAccessFileFetcher(blobAccess),
		semaphore:        semaphore.NewWeighted(int64(m.fetchParallelism)),
		cas:              blobAccess,
	}
}

func (m *materializer) materialize(ctx context.Context, imageRef string) (string, error) {
	start := time.Now()

	ephemeralCasDir, err := os.MkdirTemp(m.rootsDir, ".tmp-blobs-")
	if err != nil {
		return "", fmt.Errorf("create ephemeral blob dir: %w", err)
	}
	defer os.RemoveAll(ephemeralCasDir)

	blobAccess := ephemeralcas.New(ephemeralCasDir)
	uploader := actionrouter.NewImageToCasUploader(m.puller, blobAccess, m.buildUser)
	rootDigest, err := uploader.UploadImageToCAS(ctx, imageRef, m.digestFunction)
	if err != nil {
		return "", fmt.Errorf("upload image to ephemeral CAS: %w", err)
	}

	rootBBDigest, err := m.digestFunction.NewDigestFromProto(rootDigest)
	if err != nil {
		return "", fmt.Errorf("parse root digest: %w", err)
	}

	fetchCoordinator := m.newRootFetcher(blobAccess)

	inProgressRoot, err := m.buildRoot(ctx, fetchCoordinator, rootBBDigest)
	if err != nil {
		return "", err
	}

	// Derive the name of the directory where we store the image from the ref.
	if err := docker.ValidateImageReferenceIsShaDigest(imageRef); err != nil {
		return "", fmt.Errorf("invalid image ref: %s", imageRef)
	}
	ref, err := name.NewDigest(imageRef)
	if err != nil {
		return "", fmt.Errorf("invalid image ref %q: %w", imageRef, err)
	}
	finalPath := filepath.Join(m.rootsDir, strings.TrimPrefix(ref.DigestStr(), "sha256:"))
	if err := os.Rename(inProgressRoot, finalPath); err != nil {
		os.RemoveAll(inProgressRoot)
		return "", fmt.Errorf("rename to final path: %w", err)
	}

	m.metrics.ImagePrepDuration.Record(m.metrics.Ctx, time.Since(start).Seconds())
	return finalPath, nil
}

// buildRoot creates a fresh tempdir under rootsDir, populates it with the
// tree rooted at `rootDigest`, and returns its path. The tempdir is
// removed if anything goes wrong; on success the caller owns it (and is
// responsible for renaming it into place).
func (m *materializer) buildRoot(ctx context.Context, fetchCoordinator *rootFetcher, rootDigest bb_digest.Digest) (string, error) {
	inProgressRoot, err := os.MkdirTemp(m.rootsDir, ".tmp-root-")
	if err != nil {
		return "", fmt.Errorf("create in-progress root dir: %w", err)
	}
	if err := os.Chmod(inProgressRoot, 0o755); err != nil {
		os.RemoveAll(inProgressRoot)
		return "", fmt.Errorf("chmod in-progress root dir: %w", err)
	}
	if err := m.populateRoot(ctx, inProgressRoot, fetchCoordinator, rootDigest); err != nil {
		os.RemoveAll(inProgressRoot)
		return "", err
	}
	return inProgressRoot, nil
}

// populateRoot fetches the tree at `rootDigest` into `dirPath` and ensures
// the mountpoint directories required by the chroot helper exist.
func (materializer) populateRoot(ctx context.Context, dirPath string, fetchCoordinator *rootFetcher, rootDigest bb_digest.Digest) error {
	dir, err := filesystem.NewLocalDirectory(path.LocalFormat.NewParser(dirPath))
	if err != nil {
		return fmt.Errorf("open in-progress root dir: %w", err)
	}
	defer dir.Close()

	// Materialize the tree rooted at rootDigest into dir. The naive build
	// directory recursively loads all directories from the CAS and copies
	// their files in, bounded by fetchCoordinator.semaphore. Errors are
	// returned synchronously, so the no-op monitor and DefaultErrorLogger
	// are not load-bearing here.
	buildDir := builder.NewNaiveBuildDirectory(
		dir,
		fetchCoordinator.directoryFetcher,
		fetchCoordinator.fileFetcher,
		fetchCoordinator.semaphore,
		fetchCoordinator.cas,
	)
	if err := buildDir.MergeDirectoryContents(ctx, util.DefaultErrorLogger, rootDigest, nil); err != nil {
		return fmt.Errorf("materialize: %w", err)
	}

	// Guarantee /bin and /var exist — the chroot helper requires them as
	// mount points on the runner container. Images that don't ship one or
	// both get an empty directory here.
	for _, sub := range []string{"bin", "var"} {
		p := filepath.Join(dirPath, sub)
		if err := os.MkdirAll(p, 0o755); err != nil {
			return fmt.Errorf("ensure %s exists: %w", p, err)
		}
	}
	return nil
}

func main() {
	program.RunMain(func(ctx context.Context, siblingsGroup, dependenciesGroup program.Group) error {
		if len(os.Args) != 2 {
			return status.Error(codes.InvalidArgument, "Usage: docker_root_fetcher config.jsonnet")
		}
		var config docker_root_fetcher.ApplicationConfiguration
		if err := util.UnmarshalConfigurationFromFile(os.Args[1], &config); err != nil {
			return util.StatusWrapf(err, "Failed to read configuration from %s", os.Args[1])
		}

		lifecycleState, _, err := global.ApplyConfiguration(config.Global, dependenciesGroup)
		if err != nil {
			return util.StatusWrap(err, "Failed to apply global configuration options")
		}

		metrics, metricsShutdown, err := fetcher.InitOTLPMetrics(ctx, "docker_root_fetcher", config.OtlpCollectorEndpoints)
		if err != nil {
			return util.StatusWrap(err, "Failed to initialize metrics")
		}
		if metricsShutdown != nil {
			defer metricsShutdown(ctx)
		}

		// Refuse to start if RootsDirectoryPath is something silly.
		cleanedRoots := filepath.Clean(config.RootsDirectoryPath)
		if cleanedRoots == "/" || cleanedRoots == "." {
			return status.Errorf(codes.InvalidArgument, "RootsDirectoryPath %q must contain at least one path segment", config.RootsDirectoryPath)
		}

		// Wipe leftover state from previous runs. After a fetcher restart
		// there are no live runners holding any of these directories, so
		// anything we find is either an orphaned root from before the
		// restart or a crashed-mid-materialize tempdir.
		if err := os.RemoveAll(config.RootsDirectoryPath); err != nil {
			return util.StatusWrapf(err, "Failed to clear roots directory %v", config.RootsDirectoryPath)
		}
		if err := os.MkdirAll(config.RootsDirectoryPath, 0o755); err != nil {
			return util.StatusWrapf(err, "Failed to create roots directory %v", config.RootsDirectoryPath)
		}

		authConfig, err := fetcher.ParseRegistryAuth(config.RegistryAuthentication)
		if err != nil {
			return util.StatusWrap(err, "Failed to parse registry authentication")
		}
		var pullTimeout time.Duration
		if config.ImagePullTimeout != nil {
			pullTimeout = config.ImagePullTimeout.AsDuration()
		}
		puller := docker.NewImagePuller(authConfig, config.MaximumImageSizeBytes, pullTimeout)

		fetchParallelism := config.FetchParallelism
		if fetchParallelism == 0 {
			return status.Errorf(codes.InvalidArgument, "fetchParallelism not set")
		}
		openDirLimit := config.FetchOpenDirectoryLimit
		if openDirLimit == 0 {
			return status.Errorf(codes.InvalidArgument, "openDirLimit not set")
		}
		maxRoots := int(config.MaximumMaterializedRoots)
		if maxRoots == 0 {
			return status.Errorf(codes.InvalidArgument, "maxRoots not set")
		}
		maxConcurrentFetches := int(config.MaximumConcurrentFetches)
		if maxConcurrentFetches == 0 {
			return status.Errorf(codes.InvalidArgument, "maxConcurrentFetches not set")
		}

		instanceName, _ := bb_digest.NewInstanceName("")
		digestFunction, _ := instanceName.GetDigestFunction(remoteexecution.DigestFunction_SHA256, 0)

		var acquireTimeout time.Duration
		if config.AcquireTimeout != nil {
			acquireTimeout = config.AcquireTimeout.AsDuration()
		}

		buildUser, err := actionrouter.BuildUserFromProto(config.BuildUser)
		if err != nil {
			return util.StatusWrap(err, "Invalid build user")
		}

		m := &materializer{
			rootsDir:         config.RootsDirectoryPath,
			openDirLimit:     openDirLimit,
			fetchParallelism: fetchParallelism,
			maxMessageSize:   int(config.MaximumMessageSizeBytes),
			digestFunction:   digestFunction,
			puller:           puller,
			buildUser:        buildUser,
			metrics:          metrics,
		}

		server := fetcher.NewServer(m, metrics, maxRoots, maxConcurrentFetches, acquireTimeout)
		handler := &connectionHandler{server: server, metrics: metrics}

		go server.PrefetchImages(ctx, config.PrefetchImages, pullTimeout)

		socketPath := config.SocketPath
		os.Remove(socketPath)
		listener, err := net.Listen("unix", socketPath)
		if err != nil {
			return util.StatusWrapf(err, "Failed to listen on %s", socketPath)
		}
		defer listener.Close()

		go lifecycleState.MarkReadyAndWait(siblingsGroup)
		log.Printf("docker_root_fetcher: listening on %s", socketPath)

		var inFlight atomic.Int64
		go func() {
			<-ctx.Done()
			log.Printf("docker_root_fetcher: shutdown initiated...")
			for inFlight.Load() > 0 {
				time.Sleep(time.Second)
			}
			log.Printf("docker_root_fetcher: closing listener")
			listener.Close()
		}()

		for {
			conn, err := listener.Accept()
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			inFlight.Add(1)
			go func() {
				defer inFlight.Add(-1)
				// Pass background so that clients can finish when we're
				// being terminated.
				handler.handleConnection(context.Background(), conn)
			}()
		}
	})
}
