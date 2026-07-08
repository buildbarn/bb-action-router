package actionrouter

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-action-router/pkg/blobstore"
	"github.com/buildbarn/bb-action-router/pkg/docker"
	bb_blobstore "github.com/buildbarn/bb-storage/pkg/blobstore"
	"github.com/buildbarn/bb-storage/pkg/blobstore/buffer"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/patrickmn/go-cache"
	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// BazelInputRootDirectoryName is the directory inside the merged input
// root where the action's original input root is placed. Docker images
// must not contain a top-level entry with this name.
const BazelInputRootDirectoryName = "bazel_exec_root"

// ActionInputRewriter merges a Docker image's contents into the
// action's input root and rewrites the command to invoke the chroot
// helper. The merged tree is uploaded to the cluster CAS and the
// resulting root digest replaces the action's original input root.
//
// Use this for deployments where actions run inside a real chroot
// (privileged, e.g. inside a VM). For unprivileged sideloaded based
// runners use ActionCmdRewriter instead.
type ActionInputRewriter struct {
	cas                        bb_blobstore.BlobAccess
	maximumMessageSizeBytes    int
	chrootHelperPath           string
	containerImageReplacements map[string]string

	uploader        *ImageToCASUploader
	refStore        *blobstore.ActionCacheRefStore
	dirTreeVerifier *blobstore.DirTreeVerifier

	// Bazel client defaults to a 3 hour TTL so it's reasonable to expect
	// that CAS items will not expire sooner than that.
	cache *cache.Cache
	sf    singleflight.Group
}

// NewActionInputRewriter creates a new ActionInputRewriter.
func NewActionInputRewriter(
	cas bb_blobstore.BlobAccess,
	actionCache bb_blobstore.BlobAccess,
	maximumMessageSizeBytes int,
	chrootHelperPath string,
	imagePuller *docker.ImagePuller,
	containerImageReplacements map[string]string,
	buildUser blobstore.UnixUser,
) (*ActionInputRewriter, error) {
	if chrootHelperPath == "" {
		return nil, status.Error(codes.InvalidArgument, "chrootHelperPath must be set")
	}
	return &ActionInputRewriter{
		cas:                        cas,
		maximumMessageSizeBytes:    maximumMessageSizeBytes,
		chrootHelperPath:           chrootHelperPath,
		containerImageReplacements: containerImageReplacements,
		uploader:                   NewImageToCasUploader(imagePuller, cas, buildUser),
		refStore:                   blobstore.NewActionCacheRefStore(actionCache, maximumMessageSizeBytes),
		dirTreeVerifier:            blobstore.NewDirTreeVerifier(cas, maximumMessageSizeBytes),
		cache:                      cache.New(3*time.Hour, 1*time.Minute),
	}, nil
}

// HandleAction extracts the Docker image reference from the action's
// platform properties, ensures the image's directory tree is in the
// cluster CAS, merges it with the action's input root, and prepends the
// chroot helper to the command. Actions without a ContainerBaseImage
// property are returned unchanged.
func (r *ActionInputRewriter) HandleAction(ctx context.Context, action *remoteexecution.Action, digestFunction digest.Function) (*remoteexecution.Action, error) {
	if action.Platform == nil {
		return action, nil
	}
	var ref string
	for _, property := range action.Platform.Properties {
		if property.Name == ContainerBaseImageProperty {
			ref = property.Value
		}
	}
	if len(ref) == 0 {
		return action, nil
	}
	if newRef, ok := r.containerImageReplacements[ref]; ok {
		ref = newRef
	}
	if newRef, found := strings.CutPrefix(ref, BaseImageProtocol); found {
		ref = newRef
	} else {
		return nil, status.Errorf(codes.InvalidArgument, "ContainerBaseImage %s must have a %s prefix", ref, BaseImageProtocol)
	}

	rootDirDigest, err := r.getDirDigest(ctx, ref, digestFunction)
	if err != nil {
		return nil, err
	}
	return r.rewriteAction(ctx, action, rootDirDigest, digestFunction)
}

func (r *ActionInputRewriter) getDirDigest(ctx context.Context, ref string, digestFunction digest.Function) (*remoteexecution.Digest, error) {
	instanceName := digestFunction.GetInstanceName().String()
	key := instanceName + ":" + ref

	if cachedDigest, found := r.cache.Get(key); found {
		if d, ok := cachedDigest.(*remoteexecution.Digest); ok {
			return d, nil
		}
	}

	// TODO: we probably want to negative-cache errors (at least for a short time), otherwise running a build with an
	// invalid ref will result in each action trying to separately pull the bad ref.
	result, err, _ := r.sf.Do(key, func() (interface{}, error) {
		reDigest, err := r.getVerifiedFromRefStore(ctx, ref, digestFunction)
		if status.Code(err) == codes.NotFound {
			reDigest, err = r.uploader.UploadImageToCAS(ctx, ref, digestFunction)
			if err != nil {
				return nil, err
			}

			if err := r.refStore.Put(ctx, ref, reDigest, digestFunction); err != nil {
				return nil, util.StatusWrap(err, "Error associating digest with ref")
			}
		} else if err != nil {
			return nil, util.StatusWrap(err, "Error reading from action cache docker ref store")
		}

		r.cache.Set(key, reDigest, cache.DefaultExpiration)

		return reDigest, nil
	})

	if err != nil {
		return nil, err
	}
	return result.(*remoteexecution.Digest), nil
}

func (r *ActionInputRewriter) getVerifiedFromRefStore(ctx context.Context, ref string, digestFunction digest.Function) (*remoteexecution.Digest, error) {
	imgRootDigest, err := r.refStore.Get(ctx, ref, digestFunction)
	if err != nil {
		return nil, err
	}

	// If the refStore has the root dir digest we still need to check
	// that the root digest and all of its descendants are present in
	// the CAS.
	if err := r.dirTreeVerifier.Verify(ctx, imgRootDigest, digestFunction); err != nil {
		return nil, err
	}

	return imgRootDigest, nil
}

// rewriteAction creates a new action with the input root replaced by
// the merged tree of the docker image and the original input root.
func (r *ActionInputRewriter) rewriteAction(ctx context.Context, action *remoteexecution.Action, dockerRootDigest *remoteexecution.Digest, digestFunction digest.Function) (*remoteexecution.Action, error) {
	command, err := r.loadCommand(ctx, action, digestFunction)
	if err != nil {
		return nil, err
	}

	dockerImageDir, err := r.loadDirectory(ctx, dockerRootDigest, digestFunction)
	if err != nil {
		return nil, err
	}

	mergedDir := &remoteexecution.Directory{
		Files:       dockerImageDir.Files,
		Directories: dockerImageDir.Directories,
		Symlinks:    dockerImageDir.Symlinks,
	}
	if err := assertDirectoryDoesntContainName(mergedDir, BazelInputRootDirectoryName); err != nil {
		return nil, err
	}
	mergedDir.Directories = append(mergedDir.Directories, &remoteexecution.DirectoryNode{
		Name:   BazelInputRootDirectoryName,
		Digest: action.InputRootDigest,
	})

	mergedDirDigest, err := r.putDirectory(ctx, mergedDir, digestFunction)
	if err != nil {
		return nil, err
	}

	command.WorkingDirectory = fmt.Sprintf("%s/%s", BazelInputRootDirectoryName, command.WorkingDirectory)
	command.Arguments = append([]string{r.chrootHelperPath}, command.Arguments...)

	commandDigest, err := r.putCommand(ctx, command, digestFunction)
	if err != nil {
		return nil, err
	}

	// Rewrite platform properties identically to the sideloaded rewriter so
	// that both modes target the same worker platform queue
	// ({Flavor=chroot, Version=generic}, plus any pass-through properties).
	updatedProperties := make([]*remoteexecution.Platform_Property, 0, len(action.Platform.Properties))
	for _, prop := range action.Platform.Properties {
		switch prop.Name {
		case ContainerBaseImageProperty, "requires-network", "requires-external", "Flavor", "Version":
			continue
		}
		updatedProperties = append(updatedProperties, prop)
	}
	updatedProperties = append(updatedProperties, &remoteexecution.Platform_Property{
		Name:  "Flavor",
		Value: "chroot",
	})
	updatedProperties = append(updatedProperties, &remoteexecution.Platform_Property{
		Name:  "Version",
		Value: "generic",
	})
	slices.SortFunc(
		updatedProperties,
		func(a, b *remoteexecution.Platform_Property) int {
			return cmp.Compare(a.Name, b.Name)
		},
	)

	action.InputRootDigest = mergedDirDigest.GetProto()
	action.CommandDigest = commandDigest.GetProto()
	action.Platform.Properties = updatedProperties

	return action, nil
}

func assertDirectoryDoesntContainName(dir *remoteexecution.Directory, name string) error {
	for _, node := range dir.Directories {
		if node.Name == name {
			return status.Errorf(codes.InvalidArgument, "%s directory must not be present on filesystem overlay", BazelInputRootDirectoryName)
		}
	}
	for _, node := range dir.Files {
		if node.Name == name {
			return status.Errorf(codes.InvalidArgument, "%s file must not be present on filesystem overlay", BazelInputRootDirectoryName)
		}
	}
	for _, node := range dir.Symlinks {
		if node.Name == name {
			return status.Errorf(codes.InvalidArgument, "%s symlink must not be present on filesystem overlay", BazelInputRootDirectoryName)
		}
	}
	return nil
}

func (r *ActionInputRewriter) loadCommand(ctx context.Context, action *remoteexecution.Action, digestFunction digest.Function) (*remoteexecution.Command, error) {
	commandDigest, err := digestFunction.NewDigestFromProto(action.CommandDigest)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Failed to load Command digest: %v", err)
	}
	m, err := r.cas.Get(ctx, commandDigest).ToProto(&remoteexecution.Command{}, r.maximumMessageSizeBytes)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to fetch Command: %v", err)
	}
	return m.(*remoteexecution.Command), nil
}

func (r *ActionInputRewriter) loadDirectory(ctx context.Context, digestProto *remoteexecution.Digest, digestFunction digest.Function) (*remoteexecution.Directory, error) {
	directory := &remoteexecution.Directory{}
	digestParsed, err := digestFunction.NewDigestFromProto(digestProto)
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to parse directory digest")
	}
	directoryData, err := r.cas.Get(ctx, digestParsed).ToByteSlice(r.maximumMessageSizeBytes)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to fetch directory %s from CAS: %v", digestProto, err)
	}
	if err := proto.Unmarshal(directoryData, directory); err != nil {
		return nil, util.StatusWrap(err, "Failed to unmarshal directory")
	}
	return directory, nil
}

func (r *ActionInputRewriter) putCommand(ctx context.Context, command *remoteexecution.Command, digestFunction digest.Function) (digest.Digest, error) {
	commandData, err := proto.Marshal(command)
	if err != nil {
		return digest.Digest{}, status.Errorf(codes.Internal, "Failed to marshal command: %v", err)
	}

	commandDigestGen := digestFunction.NewGenerator(int64(len(commandData)))
	if _, err := commandDigestGen.Write(commandData); err != nil {
		return digest.Digest{}, status.Errorf(codes.Internal, "Failed to generate digest for command: %v", err)
	}
	commandDigest := commandDigestGen.Sum()

	if err := r.cas.Put(
		ctx,
		commandDigest,
		buffer.NewCASBufferFromByteSlice(commandDigest, commandData, buffer.UserProvided),
	); err != nil {
		return digest.Digest{}, status.Errorf(codes.Internal, "Failed to store Command to CAS: %v", err)
	}

	return commandDigest, nil
}

func (r *ActionInputRewriter) putDirectory(ctx context.Context, directory *remoteexecution.Directory, digestFunction digest.Function) (digest.Digest, error) {
	directoryData, err := proto.Marshal(directory)
	if err != nil {
		return digest.Digest{}, status.Errorf(codes.Internal, "Failed to marshal directory: %v", err)
	}

	directoryDigestGen := digestFunction.NewGenerator(int64(len(directoryData)))
	if _, err := directoryDigestGen.Write(directoryData); err != nil {
		return digest.Digest{}, status.Errorf(codes.Internal, "Failed to generate digest for directory: %v", err)
	}
	directoryDigest := directoryDigestGen.Sum()

	if err := r.cas.Put(
		ctx,
		directoryDigest,
		buffer.NewCASBufferFromByteSlice(directoryDigest, directoryData, buffer.UserProvided),
	); err != nil {
		return digest.Digest{}, status.Errorf(codes.Internal, "Failed to store directory %s to CAS: %v", directoryDigest, err)
	}

	return directoryDigest, nil
}
