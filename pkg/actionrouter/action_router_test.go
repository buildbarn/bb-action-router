package actionrouter_test

import (
	"context"
	"testing"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-remote-execution/internal/mock"
	"github.com/buildbarn/bb-remote-execution/pkg/actionrouter"
	pb "github.com/buildbarn/bb-remote-execution/pkg/proto/configuration/bb_action_router"
	"github.com/buildbarn/bb-remote-execution/pkg/proto/remoteactionrouter"
	"github.com/buildbarn/bb-storage/pkg/blobstore/buffer"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
	"github.com/buildbarn/bb-storage/pkg/testutil"
	"github.com/stretchr/testify/require"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.uber.org/mock/gomock"
)

type recordingImageRootResolver struct {
	digestFunction digest.Function
	image          string
	digest         digest.Digest
	err            error
}

func (r *recordingImageRootResolver) ResolveImageRoot(ctx context.Context, digestFunction digest.Function, image string) (digest.Digest, error) {
	r.digestFunction = digestFunction
	r.image = image
	return r.digest, r.err
}

func TestActionRouterServerRouteAction(t *testing.T) {
	ctx := context.Background()
	workspaceDirectory := path.MustNewComponent("workspace")
	containerImage := "image"
	inputRootDigest := digest.MustNewDigest(
		"main",
		remoteexecution.DigestFunction_SHA256,
		"77c86e4412cb6e47e92242229ced641e5f1205b265882d8ffc81a792789198b5",
		123)

	newActionRouterServer := func(missingImagePolicy pb.MissingImagePolicy, imageRootResolver actionrouter.ImageRootResolver) remoteactionrouter.ActionRouterServer {
		return actionrouter.NewActionRouterServer(
			nil,
			1000,
			workspaceDirectory,
			missingImagePolicy,
			imageRootResolver,
			nil)
	}

	t.Run("RequestRequired", func(t *testing.T) {
		actionRouterServer := newActionRouterServer(pb.MissingImagePolicy_MISSING_IMAGE_POLICY_REJECT, nil)

		_, err := actionRouterServer.RouteAction(ctx, nil)

		testutil.RequireEqualStatus(t, status.Error(codes.InvalidArgument, "No route action request provided"), err)
	})

	t.Run("MissingImagePolicy", func(t *testing.T) {
		actionRouterServer := newActionRouterServer(pb.MissingImagePolicy_MISSING_IMAGE_POLICY_UNSPECIFIED, nil)

		_, err := actionRouterServer.RouteAction(ctx, &remoteactionrouter.RouteActionRequest{
			Action: &remoteexecution.Action{},
		})

		testutil.RequireEqualStatus(t, status.Error(codes.Internal, "Action router server was created with an unsupported missing image policy"), err)
	})

	t.Run("MissingImageReject", func(t *testing.T) {
		actionRouterServer := newActionRouterServer(pb.MissingImagePolicy_MISSING_IMAGE_POLICY_REJECT, nil)

		_, err := actionRouterServer.RouteAction(ctx, &remoteactionrouter.RouteActionRequest{
			Action: &remoteexecution.Action{},
		})

		testutil.RequireEqualStatus(t, status.Error(codes.InvalidArgument, "Action does not contain a container-image platform property"), err)
	})

	t.Run("MissingImagePassthrough", func(t *testing.T) {
		actionRouterServer := newActionRouterServer(pb.MissingImagePolicy_MISSING_IMAGE_POLICY_PASSTHROUGH, nil)
		action := &remoteexecution.Action{
			CommandDigest: &remoteexecution.Digest{
				Hash:      "e495b6cfddea752f35c74273402e38fcbff01f481394089868b9d26bf7fadd18",
				SizeBytes: 123,
			},
		}

		response, err := actionRouterServer.RouteAction(ctx, &remoteactionrouter.RouteActionRequest{
			Action: action,
		})

		require.NoError(t, err)
		testutil.RequireEqualProto(t, &remoteactionrouter.RouteActionResponse{
			Action: &remoteexecution.Action{
				CommandDigest: action.CommandDigest,
				Platform:      &remoteexecution.Platform{},
			},
		}, response)
	})

	t.Run("ActionRequired", func(t *testing.T) {
		actionRouterServer := newActionRouterServer(pb.MissingImagePolicy_MISSING_IMAGE_POLICY_REJECT, nil)

		_, err := actionRouterServer.RouteAction(ctx, &remoteactionrouter.RouteActionRequest{})

		testutil.RequireEqualStatus(t, status.Error(codes.InvalidArgument, "Action required"), err)
	})

	t.Run("MultipleContainerImages", func(t *testing.T) {
		actionRouterServer := newActionRouterServer(pb.MissingImagePolicy_MISSING_IMAGE_POLICY_REJECT, nil)

		_, err := actionRouterServer.RouteAction(ctx, &remoteactionrouter.RouteActionRequest{
			Action: &remoteexecution.Action{
				Platform: &remoteexecution.Platform{
					Properties: []*remoteexecution.Platform_Property{
						{Name: "container-image", Value: "image-1"},
						{Name: "container-image", Value: "image-2"},
					},
				},
			},
		})

		testutil.RequireEqualStatus(t, status.Error(codes.InvalidArgument, "Action contains multiple container-image platform properties"), err)
	})

	t.Run("EmptyContainerImage", func(t *testing.T) {
		actionRouterServer := newActionRouterServer(pb.MissingImagePolicy_MISSING_IMAGE_POLICY_REJECT, nil)

		_, err := actionRouterServer.RouteAction(ctx, &remoteactionrouter.RouteActionRequest{
			Action: &remoteexecution.Action{
				Platform: &remoteexecution.Platform{
					Properties: []*remoteexecution.Platform_Property{
						{Name: "container-image"},
					},
				},
			},
		})

		testutil.RequireEqualStatus(t, status.Error(codes.InvalidArgument, "Action contains an empty container-image platform property"), err)
	})

	t.Run("ImageRootResolverFailure", func(t *testing.T) {
		imageRootResolver := &recordingImageRootResolver{
			err: status.Error(codes.NotFound, "Image not found"),
		}
		actionRouterServer := newActionRouterServer(pb.MissingImagePolicy_MISSING_IMAGE_POLICY_REJECT, imageRootResolver)

		_, err := actionRouterServer.RouteAction(ctx, &remoteactionrouter.RouteActionRequest{
			InstanceName:   "main",
			DigestFunction: remoteexecution.DigestFunction_SHA256,
			Action: &remoteexecution.Action{
				InputRootDigest: inputRootDigest.GetProto(),
				Platform: &remoteexecution.Platform{
					Properties: []*remoteexecution.Platform_Property{
						{Name: "container-image", Value: containerImage},
					},
				},
			},
		})

		testutil.RequireEqualStatus(t, status.Error(codes.NotFound, "Failed to resolve image root: Image not found"), err)
		require.Equal(t, "main", imageRootResolver.digestFunction.GetInstanceName().String())
		require.Equal(t, remoteexecution.DigestFunction_SHA256, imageRootResolver.digestFunction.GetEnumValue())
		require.Equal(t, containerImage, imageRootResolver.image)
	})
}

func TestActionRouterServerRouteActionImageRootResolverSuccess(t *testing.T) {
	ctrl, ctx := gomock.WithContext(context.Background(), t)

	workspaceDirectory := path.MustNewComponent("workspace")
	containerImage := "image"
	inputRootDigest := digest.MustNewDigest(
		"main",
		remoteexecution.DigestFunction_SHA256,
		"77c86e4412cb6e47e92242229ced641e5f1205b265882d8ffc81a792789198b5",
		123)
	commandDigest := digest.MustNewDigest(
		"main",
		remoteexecution.DigestFunction_SHA256,
		"3e992c9ba65277c354080296acdbc5160e287cd217c39516ff608c267b61a03b",
		93)
	imageRootDigest := digest.MustNewDigest(
		"main",
		remoteexecution.DigestFunction_SHA256,
		"14a8b79eb2e5939e0e83b10829a5c0a356a21e070411784830e75e51c015cb1e",
		123)
	mergedRootDigest := digest.MustNewDigest(
		"main",
		remoteexecution.DigestFunction_SHA256,
		"851f6c24b5b9caaf774da528e04c98be89100d0fe0e293517bcfd930b4c7c1f9",
		160)
	imageBinDigest := digest.MustNewDigest(
		"main",
		remoteexecution.DigestFunction_SHA256,
		"bd36d6d727ea643f7d26d4dc975c2c516535e4b4b77d4f604312b7af3704e016",
		89)
	rewrittenCommandDigest := digest.MustNewDigest(
		"main",
		remoteexecution.DigestFunction_SHA256,
		"dae76026beac96d7b82bc336230af56426e347f1fce0a3b34759f8cd66beb3b2",
		20)

	contentAddressableStorage := mock.NewMockBlobAccess(ctrl)
	imageRootResolver := &recordingImageRootResolver{
		digest: imageRootDigest,
	}
	actionRouterServer := actionrouter.NewActionRouterServer(
		contentAddressableStorage,
		1000,
		workspaceDirectory,
		pb.MissingImagePolicy_MISSING_IMAGE_POLICY_REJECT,
		imageRootResolver,
		nil)
	contentAddressableStorage.EXPECT().Get(ctx, imageRootDigest).Return(buffer.NewProtoBufferFromProto(&remoteexecution.Directory{
		Directories: []*remoteexecution.DirectoryNode{
			{
				Name:   "bin",
				Digest: imageBinDigest.GetProto(),
			},
		},
	}, buffer.UserProvided))
	contentAddressableStorage.EXPECT().Get(ctx, commandDigest).Return(buffer.NewProtoBufferFromProto(&remoteexecution.Command{
		Arguments:        []string{"gcc"},
		WorkingDirectory: "src",
	}, buffer.UserProvided))
	contentAddressableStorage.EXPECT().Put(ctx, mergedRootDigest, gomock.Any()).
		DoAndReturn(func(ctx context.Context, d digest.Digest, b buffer.Buffer) error {
			m, err := b.ToProto(&remoteexecution.Directory{}, 1000)
			require.NoError(t, err)
			testutil.RequireEqualProto(t, &remoteexecution.Directory{
				Directories: []*remoteexecution.DirectoryNode{
					{
						Name:   "bin",
						Digest: imageBinDigest.GetProto(),
					},
					{
						Name:   "workspace",
						Digest: inputRootDigest.GetProto(),
					},
				},
			}, m)
			return nil
		})
	contentAddressableStorage.EXPECT().Put(ctx, rewrittenCommandDigest, gomock.Any()).
		DoAndReturn(func(ctx context.Context, d digest.Digest, b buffer.Buffer) error {
			m, err := b.ToProto(&remoteexecution.Command{}, 1000)
			require.NoError(t, err)
			testutil.RequireEqualProto(t, &remoteexecution.Command{
				Arguments:        []string{"gcc"},
				WorkingDirectory: "workspace/src",
			}, m)
			return nil
		})

	response, err := actionRouterServer.RouteAction(ctx, &remoteactionrouter.RouteActionRequest{
		InstanceName:   "main",
		DigestFunction: remoteexecution.DigestFunction_SHA256,
		Action: &remoteexecution.Action{
			InputRootDigest: inputRootDigest.GetProto(),
			CommandDigest:   commandDigest.GetProto(),
			Platform: &remoteexecution.Platform{
				Properties: []*remoteexecution.Platform_Property{
					{Name: "container-image", Value: containerImage},
				},
			},
		},
	})

	require.NoError(t, err)
	testutil.RequireEqualProto(t, &remoteactionrouter.RouteActionResponse{
		Action: &remoteexecution.Action{
			InputRootDigest: mergedRootDigest.GetProto(),
			CommandDigest:   rewrittenCommandDigest.GetProto(),
			Platform: &remoteexecution.Platform{
				Properties: []*remoteexecution.Platform_Property{
					{Name: "container-image", Value: containerImage},
				},
			},
		},
	}, response)
	require.Equal(t, "main", imageRootResolver.digestFunction.GetInstanceName().String())
	require.Equal(t, remoteexecution.DigestFunction_SHA256, imageRootResolver.digestFunction.GetEnumValue())
	require.Equal(t, containerImage, imageRootResolver.image)
}

func TestNewActionRouterServerFromConfiguration(t *testing.T) {
	t.Run("NoInvocationKeyExtractors", func(t *testing.T) {
		_, err := actionrouter.NewActionRouterServerFromConfiguration(&pb.ActionRouterConfiguration{
			ImageRouter: &pb.ImageRouterConfiguration{
				WorkspaceDirectory: "workspace",
				MissingImagePolicy: pb.MissingImagePolicy_MISSING_IMAGE_POLICY_REJECT,
				ImageRootResolver:  &pb.ImageRootResolverConfiguration{},
			},
		}, nil, 1000)

		testutil.RequireEqualStatus(
			t,
			status.Error(codes.InvalidArgument, "No invocation key extractors provided"),
			err)
	})
}
