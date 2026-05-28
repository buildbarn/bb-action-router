package actionrouter

import (
	"context"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-remote-execution/pkg/cas"
	pb "github.com/buildbarn/bb-remote-execution/pkg/proto/configuration/bb_action_router"
	"github.com/buildbarn/bb-remote-execution/pkg/proto/remoteactionrouter"
	"github.com/buildbarn/bb-remote-execution/pkg/scheduler/invocation"
	"github.com/buildbarn/bb-storage/pkg/blobstore"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
	"github.com/buildbarn/bb-storage/pkg/util"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

const containerImagePlatformPropertyName = "container-image"

type actionRouterServer struct {
	contentAddressableStorage blobstore.BlobAccess
	maximumMessageSizeBytes   int
	workspaceDirectory        path.Component
	missingImagePolicy        pb.MissingImagePolicy
	imageRootResolver         ImageRootResolver
	invocationKeyExtractors   []invocation.KeyExtractor
}

// NewActionRouterServer creates a remote action router service.
func NewActionRouterServer(
	contentAddressableStorage blobstore.BlobAccess,
	maximumMessageSizeBytes int,
	workspaceDirectory path.Component,
	missingImagePolicy pb.MissingImagePolicy,
	imageRootResolver ImageRootResolver,
	invocationKeyExtractors []invocation.KeyExtractor,
) remoteactionrouter.ActionRouterServer {
	return &actionRouterServer{
		contentAddressableStorage: contentAddressableStorage,
		maximumMessageSizeBytes:   maximumMessageSizeBytes,
		workspaceDirectory:        workspaceDirectory,
		missingImagePolicy:        missingImagePolicy,
		imageRootResolver:         imageRootResolver,
		invocationKeyExtractors:   invocationKeyExtractors,
	}
}

func (s *actionRouterServer) RouteAction(ctx context.Context, request *remoteactionrouter.RouteActionRequest) (*remoteactionrouter.RouteActionResponse, error) {
	if request == nil {
		return nil, status.Error(codes.InvalidArgument, "No route action request provided")
	}
	action := request.GetAction()
	if action == nil {
		return nil, status.Error(codes.InvalidArgument, "Action required")
	}

	invocationKeys, err := s.extractInvocationKeys(ctx, request.GetRequestMetadata())
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to extract invocation key")
	}

	containerImageProperty, err := getContainerImageProperty(action.Platform)
	if err != nil {
		return nil, err
	}
	if containerImageProperty == nil {
		switch s.missingImagePolicy {
		case pb.MissingImagePolicy_MISSING_IMAGE_POLICY_REJECT:
			return nil, status.Error(codes.InvalidArgument, "Action does not contain a container-image platform property")
		case pb.MissingImagePolicy_MISSING_IMAGE_POLICY_PASSTHROUGH:
			return &remoteactionrouter.RouteActionResponse{
				Action:         ensureActionPlatform(action),
				InvocationKeys: invocationKeys,
			}, nil
		default:
			return nil, status.Error(codes.Internal, "Action router server was created with an unsupported missing image policy")
		}
	}
	if containerImageProperty.Value == "" {
		return nil, status.Error(codes.InvalidArgument, "Action contains an empty container-image platform property")
	}

	instanceName, err := digest.NewInstanceName(request.GetInstanceName())
	if err != nil {
		return nil, util.StatusWrapf(err, "Invalid instance name %#v", request.GetInstanceName())
	}
	digestFunction, err := instanceName.GetDigestFunction(request.GetDigestFunction(), 0)
	if err != nil {
		return nil, err
	}
	inputRootDigest, err := digestFunction.NewDigestFromProto(action.InputRootDigest)
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to extract digest for input root")
	}
	imageRootDigest, err := s.imageRootResolver.ResolveImageRoot(ctx, digestFunction, containerImageProperty.Value)
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to resolve image root")
	}
	// The resolver returns a complete image root. Add the original action input
	// root below the configured workspace directory.
	imageRootDirectory, err := cas.NewBlobAccessDirectoryFetcher(
		s.contentAddressableStorage,
		s.maximumMessageSizeBytes,
		int64(s.maximumMessageSizeBytes),
	).GetDirectory(ctx, imageRootDigest)
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to fetch image root directory")
	}
	mergedRootDirectory, err := mergeImageRootDirectory(
		imageRootDirectory,
		s.workspaceDirectory,
		inputRootDigest)
	if err != nil {
		return nil, err
	}

	// Rewrite the command working directory so it stays below the configured
	// workspace directory.
	commandDigest, err := digestFunction.NewDigestFromProto(action.CommandDigest)
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to extract digest for command")
	}
	commandMessage, err := s.contentAddressableStorage.Get(ctx, commandDigest).ToProto(&remoteexecution.Command{}, s.maximumMessageSizeBytes)
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to obtain command")
	}
	command := commandMessage.(*remoteexecution.Command)
	rewrittenCommand, err := rewriteCommandWorkingDirectory(command, s.workspaceDirectory)
	if err != nil {
		return nil, err
	}

	// Store the merged input root and rewritten command in the CAS.
	mergedRootDigest, err := blobstore.CASPutProto(
		ctx,
		s.contentAddressableStorage,
		mergedRootDirectory,
		digestFunction)
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to store merged input root directory")
	}
	rewrittenCommandDigest, err := blobstore.CASPutProto(
		ctx,
		s.contentAddressableStorage,
		rewrittenCommand,
		digestFunction)
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to store rewritten command")
	}

	routedAction := remoteexecution.Action{}
	proto.Merge(&routedAction, action)
	routedAction.InputRootDigest = mergedRootDigest.GetProto()
	routedAction.CommandDigest = rewrittenCommandDigest.GetProto()
	return &remoteactionrouter.RouteActionResponse{
		Action:         &routedAction,
		InvocationKeys: invocationKeys,
	}, nil
}

func (s *actionRouterServer) extractInvocationKeys(ctx context.Context, requestMetadata *remoteexecution.RequestMetadata) ([]*anypb.Any, error) {
	invocationKeys := make([]*anypb.Any, 0, len(s.invocationKeyExtractors))
	for _, invocationKeyExtractor := range s.invocationKeyExtractors {
		invocationKey, err := invocationKeyExtractor.ExtractKey(ctx, requestMetadata)
		if err != nil {
			return nil, err
		}
		invocationKeys = append(invocationKeys, invocationKey.GetID())
	}
	return invocationKeys, nil
}

func getContainerImageProperty(platform *remoteexecution.Platform) (*remoteexecution.Platform_Property, error) {
	var containerImageProperty *remoteexecution.Platform_Property
	for _, property := range platform.GetProperties() {
		if property.Name != containerImagePlatformPropertyName {
			continue
		}
		// Only support one image per action.
		if containerImageProperty != nil {
			return nil, status.Error(codes.InvalidArgument, "Action contains multiple container-image platform properties")
		}
		containerImageProperty = property
	}
	return containerImageProperty, nil
}

func ensureActionPlatform(action *remoteexecution.Action) *remoteexecution.Action {
	if action.GetPlatform() != nil {
		return action
	}
	actionWithPlatform := remoteexecution.Action{}
	proto.Merge(&actionWithPlatform, action)
	actionWithPlatform.Platform = &remoteexecution.Platform{}
	return &actionWithPlatform
}
