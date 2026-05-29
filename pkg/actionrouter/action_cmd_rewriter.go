package actionrouter

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-action-router/pkg/docker"
	bb_blobstore "github.com/buildbarn/bb-storage/pkg/blobstore"
	"github.com/buildbarn/bb-storage/pkg/blobstore/buffer"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	// ContainerBaseImageProperty is the platform property specifying a Docker image reference.
	ContainerBaseImageProperty = "ContainerBaseImage"
	// BaseImageProtocol is the required prefix for Docker image references.
	BaseImageProtocol = "docker://"
)

// ActionCmdRewriter extracts the Docker image reference from an
// action's platform properties and rewrites the command to prepend the
// chroot helper. The actual image pull and materialization is handled
// by the docker_root_fetcher sidecar.
type ActionCmdRewriter struct {
	cas                        bb_blobstore.BlobAccess
	maximumMessageSizeBytes    int
	chrootHelperPath           string
	fetcherSocketPath          string
	containerImageReplacements map[string]string
}

// NewActionCmdRewriter creates a new ActionCmdRewriter.
func NewActionCmdRewriter(
	cas bb_blobstore.BlobAccess,
	maximumMessageSizeBytes int,
	chrootHelperPath string,
	fetcherSocketPath string,
	containerImageReplacements map[string]string,
) (*ActionCmdRewriter, error) {
	if chrootHelperPath == "" {
		return nil, status.Error(codes.InvalidArgument, "chrootHelperPath must be set")
	}
	if fetcherSocketPath == "" {
		return nil, status.Error(codes.InvalidArgument, "fetcherSocketPath must be set")
	}
	return &ActionCmdRewriter{
		cas:                        cas,
		maximumMessageSizeBytes:    maximumMessageSizeBytes,
		chrootHelperPath:           chrootHelperPath,
		fetcherSocketPath:          fetcherSocketPath,
		containerImageReplacements: containerImageReplacements,
	}, nil
}

// HandleAction extracts the Docker image reference from the action's
// platform properties and calls RewriteAction to prepend the chroot
// helper. If no ContainerBaseImage property is present, the action is
// returned as-is.
func (r *ActionCmdRewriter) HandleAction(ctx context.Context, action *remoteexecution.Action, digestFunction digest.Function) (*remoteexecution.Action, error) {
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

	return r.rewriteAction(ctx, action, ref, digestFunction)
}

// rewriteAction prepends the chroot helper to the command with the
// docker image reference as an argument and updates platform properties.
func (r *ActionCmdRewriter) rewriteAction(ctx context.Context, action *remoteexecution.Action, imageRef string, digestFunction digest.Function) (*remoteexecution.Action, error) {
	err := docker.ValidateImageReferenceIsShaDigest(imageRef)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	command, err := r.loadCommand(ctx, action, digestFunction)
	if err != nil {
		return nil, err
	}

	// Determine network isolation mode.
	networkIsolationRequested := false
	requiresExternalSpecified := false
	for _, prop := range action.Platform.Properties {
		if prop.Name == "requires-network" && prop.Value == "false" {
			networkIsolationRequested = true
		}
		if prop.Name == "requires-external" && prop.Value == "true" {
			requiresExternalSpecified = true
		}
	}
	networkFlag := "--no-network-isolation"
	if networkIsolationRequested && !requiresExternalSpecified {
		networkFlag = "--network-isolation"
	}

	// Prepend chroot helper with image ref and fetcher socket.
	command.Arguments = append([]string{
		r.chrootHelperPath,
		fmt.Sprintf("--docker-image-ref=%s", imageRef),
		fmt.Sprintf("--fetcher-socket=%s", r.fetcherSocketPath),
		networkFlag,
	}, command.Arguments...)

	commandDigest, err := r.putCommand(ctx, command, digestFunction)
	if err != nil {
		return nil, err
	}

	// Update properties. Drop properties consumed by the rewriter.
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

	action.CommandDigest = commandDigest.GetProto()
	action.Platform.Properties = updatedProperties

	return action, nil
}

func (r *ActionCmdRewriter) loadCommand(ctx context.Context, action *remoteexecution.Action, digestFunction digest.Function) (*remoteexecution.Command, error) {
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

func (r *ActionCmdRewriter) putCommand(ctx context.Context, command *remoteexecution.Command, digestFunction digest.Function) (digest.Digest, error) {
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
