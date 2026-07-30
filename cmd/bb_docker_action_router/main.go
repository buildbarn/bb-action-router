package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-action-router/pkg/actionrouter"
	"github.com/buildbarn/bb-action-router/pkg/logging"
	bb_docker_action_router "github.com/buildbarn/bb-action-router/pkg/proto/configuration/bb_docker_action_router"
	"github.com/buildbarn/bb-remote-execution/pkg/proto/remoteactionrouter"
	"github.com/buildbarn/bb-storage/pkg/auth"
	blobstore_configuration "github.com/buildbarn/bb-storage/pkg/blobstore/configuration"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/buildbarn/bb-storage/pkg/global"
	bb_grpc "github.com/buildbarn/bb-storage/pkg/grpc"
	"github.com/buildbarn/bb-storage/pkg/program"
	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/buildbarn/bb-storage/pkg/zstd"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

func main() {
	program.RunMain(func(ctx context.Context, siblingsGroup, dependenciesGroup program.Group) error {
		if err := logging.Configure(); err != nil {
			return util.StatusWrapWithCode(err, codes.InvalidArgument, "Failed to configure logging")
		}
		if len(os.Args) != 2 {
			return status.Error(codes.InvalidArgument, "Usage: bb_docker_action_router bb_docker_action_router_config")
		}
		var configuration bb_docker_action_router.ApplicationConfiguration
		if err := util.UnmarshalConfigurationFromFile(os.Args[1], &configuration); err != nil {
			return util.StatusWrapf(err, "Failed to read configuration from %s", os.Args[1])
		}

		lifecycleState, grpcClientFactory, err := global.ApplyConfiguration(configuration.Global, dependenciesGroup)
		if err != nil {
			return util.StatusWrap(err, "Failed to apply global configuration options")
		}

		contentAddressableStorage, actionCache, err := blobstore_configuration.NewCASAndACBlobAccessFromConfiguration(
			dependenciesGroup,
			configuration.Blobstore,
			grpcClientFactory,
			int(configuration.MaximumMessageSizeBytes),
			zstd.NewPoolFromConfiguration(nil),
		)
		if err != nil {
			return util.StatusWrap(err, "Failed to create blobstore")
		}

		pipeline, err := actionrouter.NewPipeline(&configuration, contentAddressableStorage, actionCache)
		if err != nil {
			return util.StatusWrap(err, "Failed to create action pipeline")
		}

		service := &dockerActionRouterServer{
			pipeline: pipeline,
		}

		if err := bb_grpc.NewServersFromConfigurationAndServe(
			configuration.GrpcServers,
			func(s grpc.ServiceRegistrar) {
				remoteactionrouter.RegisterActionRouterServer(s, service)
			},
			siblingsGroup,
			grpcClientFactory,
		); err != nil {
			return util.StatusWrap(err, "gRPC server failure")
		}

		slog.Info("Running...")
		lifecycleState.MarkReadyAndWait(siblingsGroup)
		return nil
	})
}

type dockerActionRouterServer struct {
	remoteactionrouter.UnimplementedActionRouterServer
	pipeline *actionrouter.Pipeline
}

var (
	routeActionDurationSeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "buildbarn",
			Subsystem: "docker_ar",
			Name:      "route_action_duration_seconds",
			Help:      "Time taken to process an action in RouteAction",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"status"},
	)

	actionsProcessedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "buildbarn",
			Subsystem: "docker_ar",
			Name:      "actions_processed_total",
			Help:      "Total number of actions processed",
		},
		[]string{"status"},
	)
)

func (s *dockerActionRouterServer) RouteAction(ctx context.Context, request *remoteactionrouter.RouteActionRequest) (*remoteactionrouter.RouteActionResponse, error) {
	startTime := time.Now()

	response, err := s.routeActionImpl(ctx, request)

	status := "success"
	if err != nil {
		status = "error"
	}

	routeActionDurationSeconds.WithLabelValues(status).Observe(time.Since(startTime).Seconds())
	actionsProcessedTotal.WithLabelValues(status).Inc()

	return response, err
}

func (s *dockerActionRouterServer) routeActionImpl(ctx context.Context, request *remoteactionrouter.RouteActionRequest) (*remoteactionrouter.RouteActionResponse, error) {
	action := request.Action
	if action == nil {
		return nil, status.Error(codes.InvalidArgument, "Action cannot be nil")
	}

	instanceName, err := digest.NewInstanceName(request.InstanceName)
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to parse instance name")
	}

	digestFunction, err := instanceName.GetDigestFunction(
		remoteexecution.DigestFunction_Value(request.DigestFunction), 0,
	)
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to create digest function")
	}

	updatedAction, err := s.pipeline.HandleAction(ctx, action, request.RequestMetadata, digestFunction)
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to handle Docker action")
	}

	invocationKeys, err := extractInvocationKeys(ctx, request.RequestMetadata)
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to extract invocation keys")
	}

	return &remoteactionrouter.RouteActionResponse{
		Action:         updatedAction,
		InvocationKeys: invocationKeys,
	}, nil
}

func extractInvocationKeys(ctx context.Context, requestMetadata *remoteexecution.RequestMetadata) ([]*anypb.Any, error) {
	invocationKeys := make([]*anypb.Any, 0, 2)

	// First group by user...
	authenticationMetadata, _ := auth.AuthenticationMetadataFromContext(ctx).GetPublicProto()
	authKey, err := anypb.New(authenticationMetadata)
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to create authentication metadata key")
	}
	invocationKeys = append(invocationKeys, authKey)

	// ..., then group by build invocation.
	if requestMetadata != nil {
		toolInvocationKey, err := anypb.New(&remoteexecution.RequestMetadata{
			ToolInvocationId: requestMetadata.GetToolInvocationId(),
		})
		if err != nil {
			return nil, util.StatusWrap(err, "Failed to create tool invocation ID key")
		}
		invocationKeys = append(invocationKeys, toolInvocationKey)
	}

	return invocationKeys, nil
}
