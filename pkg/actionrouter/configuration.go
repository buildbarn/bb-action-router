package actionrouter

import (
	pb "github.com/buildbarn/bb-remote-execution/pkg/proto/configuration/bb_action_router"
	"github.com/buildbarn/bb-remote-execution/pkg/proto/remoteactionrouter"
	"github.com/buildbarn/bb-remote-execution/pkg/scheduler/invocation"
	"github.com/buildbarn/bb-storage/pkg/blobstore"
	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
	"github.com/buildbarn/bb-storage/pkg/util"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// NewActionRouterServerFromConfiguration creates an action router server based
// on options specified in a configuration file.
func NewActionRouterServerFromConfiguration(configuration *pb.ActionRouterConfiguration, contentAddressableStorage blobstore.BlobAccess, maximumMessageSizeBytes int) (remoteactionrouter.ActionRouterServer, error) {
	if configuration == nil {
		return nil, status.Error(codes.InvalidArgument, "No action router configuration provided")
	}
	imageRouterConfiguration := configuration.GetImageRouter()
	if imageRouterConfiguration == nil {
		return nil, status.Error(codes.InvalidArgument, "No image router configuration provided")
	}
	workspaceDirectory, ok := path.NewComponent(imageRouterConfiguration.WorkspaceDirectory)
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid workspace directory %#v", imageRouterConfiguration.WorkspaceDirectory)
	}
	imageRootResolver, err := newImageRootResolverFromConfiguration(imageRouterConfiguration.GetImageRootResolver(), contentAddressableStorage)
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to create image root resolver")
	}
	switch imageRouterConfiguration.MissingImagePolicy {
	case pb.MissingImagePolicy_MISSING_IMAGE_POLICY_REJECT,
		pb.MissingImagePolicy_MISSING_IMAGE_POLICY_PASSTHROUGH:
	default:
		return nil, status.Error(codes.InvalidArgument, "Configuration did not contain a supported missing image policy")
	}
	if len(configuration.InvocationKeyExtractors) == 0 {
		return nil, status.Error(codes.InvalidArgument, "No invocation key extractors provided")
	}
	invocationKeyExtractors := make([]invocation.KeyExtractor, 0, len(configuration.InvocationKeyExtractors))
	for i, entry := range configuration.InvocationKeyExtractors {
		invocationKeyExtractor, err := invocation.NewKeyExtractorFromConfiguration(entry)
		if err != nil {
			return nil, util.StatusWrapf(err, "Failed to create invocation key extractor at index %d", i)
		}
		invocationKeyExtractors = append(invocationKeyExtractors, invocationKeyExtractor)
	}
	return NewActionRouterServer(
		contentAddressableStorage,
		maximumMessageSizeBytes,
		workspaceDirectory,
		imageRouterConfiguration.MissingImagePolicy,
		imageRootResolver,
		invocationKeyExtractors), nil
}
