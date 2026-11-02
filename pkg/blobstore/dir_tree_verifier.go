package blobstore

import (
	"context"
	"time"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	blobstore_configuration "github.com/buildbarn/bb-storage/pkg/blobstore/configuration"
	"github.com/buildbarn/bb-storage/pkg/cas"
	"github.com/buildbarn/bb-storage/pkg/cas/reader"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var dirTreeVerificationDuration = promauto.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "bb_docker_action_router",
		Subsystem: "blobstore",
		Name:      "dir_tree_verification_duration_seconds",
		Help:      "Duration of directory tree verification operations",
		Buckets:   prometheus.ExponentialBuckets(0.001, 2, 15), // 1ms to ~32s
	},
	[]string{"status"},
)

// DirTreeVerifier makes sure that all digests for a given dir hierarchy are present in the CAS.
type DirTreeVerifier struct {
	contentAddressableStorage blobstore_configuration.ContentAddressableStorageInfo
	directoryReader           reader.Reader[*remoteexecution.Directory]
}

// NewDirTreeVerifier returns a new DirTreeVerifier instance.
func NewDirTreeVerifier(contentAddressableStorage blobstore_configuration.ContentAddressableStorageInfo, maximumMessageSizeBytes int) *DirTreeVerifier {
	return &DirTreeVerifier{
		contentAddressableStorage: contentAddressableStorage,
		directoryReader:           cas.NewMessageReader[remoteexecution.Directory](contentAddressableStorage.ReadInfo, maximumMessageSizeBytes),
	}
}

// Verify asserts that all digests in the supplied directory (and its
// sub-directories, and theirs, etc..) are present in the CAS.
func (v *DirTreeVerifier) Verify(ctx context.Context, rootDirDigest *remoteexecution.Digest, digestFunction digest.Function) error {
	start := time.Now()

	err := v.verifyDirectory(ctx, rootDirDigest, digestFunction)

	status := "success"
	if err != nil {
		status = "error"
	}

	dirTreeVerificationDuration.WithLabelValues(status).Observe(time.Since(start).Seconds())

	return err
}

func (v *DirTreeVerifier) verifyDirectory(ctx context.Context, dirRawDigest *remoteexecution.Digest, digestFunction digest.Function) error {
	dirDigest, err := digestFunction.NewDigestFromProto(dirRawDigest)
	if err != nil {
		return util.StatusWrapf(err, "Failed to load directory digest")
	}

	directory, err := v.directoryReader.Read(ctx, dirDigest)
	if err != nil {
		return util.StatusWrapf(err, "Failed to get directory with digest %s", dirDigest)
	}

	// TODO: the implementation here could be easily parallelized, but for
	// maximum efficiency we should store a REv2.Tree instead of a directory
	// digest. That way we would be have immediate access to all of the digests
	// and could perform the check in a single FindMissing call.

	if len(directory.Files) > 0 {
		if err := v.verifyFiles(ctx, directory.Files, digestFunction); err != nil {
			return err
		}
	}

	for _, subDirNode := range directory.Directories {
		if err := v.verifyDirectory(ctx, subDirNode.Digest, digestFunction); err != nil {
			return err
		}
	}

	return nil
}

func (v *DirTreeVerifier) verifyFiles(ctx context.Context, files []*remoteexecution.FileNode, digestFunction digest.Function) error {
	if len(files) == 0 {
		return nil
	}

	digests := digest.NewSetBuilder(len(files))
	for _, file := range files {
		fileDigest, err := digestFunction.NewDigestFromProto(file.Digest)
		if err != nil {
			return util.StatusWrap(err, "Failed to extract digest for file")
		}
		digests.Add(fileDigest)
	}

	params, err := v.contentAddressableStorage.CDCParametersFetcher.FetchCDCParameters(ctx, digestFunction.GetInstanceName())
	if err != nil {
		return util.StatusWrap(err, "Failed to fetch CDC parameters")
	}
	missingDigests, err := cas.FindMissing(ctx, v.contentAddressableStorage.ChunkStorage, v.contentAddressableStorage.ChunkMappingStorage, params, digests.Build())
	if err != nil {
		return util.StatusWrap(err, "Failed to check for missing files")
	}

	if !missingDigests.Empty() {
		return status.Errorf(codes.NotFound, "Missing child digests in directory")
	}

	return nil
}
