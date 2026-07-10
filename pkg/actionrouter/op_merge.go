package actionrouter

import (
	"context"
	"fmt"
	"text/template"
	"time"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-action-router/pkg/blobstore"
	"github.com/buildbarn/bb-action-router/pkg/docker"
	"github.com/buildbarn/bb-action-router/pkg/fetcher"
	pb "github.com/buildbarn/bb-action-router/pkg/proto/configuration/bb_docker_action_router"
	bb_blobstore "github.com/buildbarn/bb-storage/pkg/blobstore"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/patrickmn/go-cache"
	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultMaximumImageSizeBytes = 10 * 1024 * 1024 * 1024
	defaultImagePullTimeout      = 5 * time.Minute
)

// mergeDockerRootOp pulls a docker image, uploads its tree to the CAS, and
// merges it into the action's input root (with the original inputs nested under
// BazelInputRootDirectoryName, which is where the helpers expect them). It also
// rewrites the working directory to sit under that subdirectory.
type mergeDockerRootOp struct {
	imageRef *template.Template

	cas             bb_blobstore.BlobAccess
	maxMessageSize  int
	uploader        *ImageToCASUploader
	refStore        *blobstore.ActionCacheRefStore
	dirTreeVerifier *blobstore.DirTreeVerifier

	// Bazel clients default to a 3 hour TTL, so CAS items shouldn't expire
	// sooner than that.
	cache *cache.Cache
	sf    singleflight.Group
}

func newMergeDockerRootOp(config *pb.MergeDockerRoot, cas, actionCache bb_blobstore.BlobAccess, maxMessageSize int) (operation, error) {
	if config.GetImageRef() == "" {
		return nil, status.Error(codes.InvalidArgument, "merge_docker_root.image_ref must be set")
	}
	imageRef, err := parseTemplate("merge_docker_root.image_ref", config.GetImageRef())
	if err != nil {
		return nil, err
	}

	maxImageSize := config.GetMaximumImageSizeBytes()
	if maxImageSize == 0 {
		maxImageSize = defaultMaximumImageSizeBytes
	}
	pullTimeout := defaultImagePullTimeout
	if config.GetImagePullTimeout() != nil {
		pullTimeout = config.GetImagePullTimeout().AsDuration()
	}
	authConfig, err := fetcher.ParseRegistryAuth(config.GetRegistryAuthentication())
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to parse registry authentication")
	}
	puller := docker.NewImagePuller(authConfig, maxImageSize, pullTimeout)

	buildUser, err := BuildUserFromProto(config.GetBuildUser())
	if err != nil {
		return nil, util.StatusWrap(err, "Invalid merge_docker_root.build_user")
	}

	return &mergeDockerRootOp{
		imageRef:        imageRef,
		cas:             cas,
		maxMessageSize:  maxMessageSize,
		uploader:        NewImageToCasUploader(puller, cas, buildUser),
		refStore:        blobstore.NewActionCacheRefStore(actionCache, maxMessageSize),
		dirTreeVerifier: blobstore.NewDirTreeVerifier(cas, maxMessageSize),
		cache:           cache.New(3*time.Hour, 1*time.Minute),
	}, nil
}

func (o *mergeDockerRootOp) apply(s *pipelineState) error {
	ref, err := render(o.imageRef, s)
	if err != nil {
		return status.Errorf(codes.Internal, "Failed to render image ref: %v", err)
	}
	if ref == "" {
		return status.Error(codes.InvalidArgument, "merge_docker_root.image_ref rendered empty")
	}

	command, err := s.getCommand()
	if err != nil {
		return err
	}

	dockerRootDigest, err := o.getDirDigest(s.ctx, ref, s.digestFunction)
	if err != nil {
		return err
	}
	dockerImageDir, err := loadDirectory(s.ctx, o.cas, o.maxMessageSize, dockerRootDigest, s.digestFunction)
	if err != nil {
		return err
	}

	mergedDir := &remoteexecution.Directory{
		Files:       dockerImageDir.Files,
		Directories: dockerImageDir.Directories,
		Symlinks:    dockerImageDir.Symlinks,
	}
	if err := assertDirectoryDoesntContainName(mergedDir, BazelInputRootDirectoryName); err != nil {
		return err
	}
	mergedDir.Directories = append(mergedDir.Directories, &remoteexecution.DirectoryNode{
		Name:   BazelInputRootDirectoryName,
		Digest: s.action.InputRootDigest,
	})

	mergedDirDigest, err := putDirectory(s.ctx, o.cas, mergedDir, s.digestFunction)
	if err != nil {
		return err
	}

	s.action.InputRootDigest = mergedDirDigest.GetProto()
	command.WorkingDirectory = fmt.Sprintf("%s/%s", BazelInputRootDirectoryName, command.WorkingDirectory)
	s.commandChanged = true
	return nil
}

func (o *mergeDockerRootOp) getDirDigest(ctx context.Context, ref string, digestFunction digest.Function) (*remoteexecution.Digest, error) {
	key := fmt.Sprintf(
		"%s:%d:%s",
		digestFunction.GetInstanceName().String(),
		digestFunction.GetEnumValue(),
		ref,
	)
	if cached, found := o.cache.Get(key); found {
		if d, ok := cached.(*remoteexecution.Digest); ok {
			return d, nil
		}
	}

	// TODO: negative-cache errors so a bad ref doesn't get re-pulled by every action.
	result, err, _ := o.sf.Do(key, func() (interface{}, error) {
		reDigest, err := o.getVerifiedFromRefStore(ctx, ref, digestFunction)
		if status.Code(err) == codes.NotFound {
			reDigest, err = o.uploader.UploadImageToCAS(ctx, ref, digestFunction)
			if err != nil {
				return nil, err
			}
			if err := o.refStore.Put(ctx, ref, reDigest, digestFunction); err != nil {
				return nil, util.StatusWrap(err, "Error associating digest with ref")
			}
		} else if err != nil {
			return nil, util.StatusWrap(err, "Error reading from action cache docker ref store")
		}
		o.cache.Set(key, reDigest, cache.DefaultExpiration)
		return reDigest, nil
	})
	if err != nil {
		return nil, err
	}
	return result.(*remoteexecution.Digest), nil
}

func (o *mergeDockerRootOp) getVerifiedFromRefStore(ctx context.Context, ref string, digestFunction digest.Function) (*remoteexecution.Digest, error) {
	imgRootDigest, err := o.refStore.Get(ctx, ref, digestFunction)
	if err != nil {
		return nil, err
	}
	// The refStore has the root digest, but we still need to confirm the root
	// and all its descendants are present in the CAS.
	if err := o.dirTreeVerifier.Verify(ctx, imgRootDigest, digestFunction); err != nil {
		return nil, err
	}
	return imgRootDigest, nil
}

func assertDirectoryDoesntContainName(dir *remoteexecution.Directory, name string) error {
	for _, node := range dir.Directories {
		if node.Name == name {
			return status.Errorf(codes.InvalidArgument, "%s directory must not be present on filesystem overlay", name)
		}
	}
	for _, node := range dir.Files {
		if node.Name == name {
			return status.Errorf(codes.InvalidArgument, "%s file must not be present on filesystem overlay", name)
		}
	}
	for _, node := range dir.Symlinks {
		if node.Name == name {
			return status.Errorf(codes.InvalidArgument, "%s symlink must not be present on filesystem overlay", name)
		}
	}
	return nil
}
