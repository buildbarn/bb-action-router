package main

import (
	"context"
	"testing"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-storage/pkg/auth"
	auth_pb "github.com/buildbarn/bb-storage/pkg/proto/auth"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestExtractInvocationKeys(t *testing.T) {
	authenticationMetadata := &auth_pb.AuthenticationMetadata{
		Public: structpb.NewStringValue("test-user"),
	}
	metadata, err := auth.NewAuthenticationMetadataFromProto(authenticationMetadata)
	if err != nil {
		t.Fatal(err)
	}
	ctx := auth.NewContextWithAuthenticationMetadata(context.Background(), metadata)
	wantAuthKey, err := anypb.New(authenticationMetadata)
	if err != nil {
		t.Fatal(err)
	}

	for _, testCase := range []struct {
		name            string
		requestMetadata *remoteexecution.RequestMetadata
		invocationID    string
	}{
		{name: "MissingMetadata"},
		{name: "EmptyMetadata", requestMetadata: &remoteexecution.RequestMetadata{}},
		{
			name: "IdentifiedInvocation",
			requestMetadata: &remoteexecution.RequestMetadata{
				ToolInvocationId: "test-invocation",
				ActionId:         "test-action",
				TargetId:         "//test:target",
			},
			invocationID: "test-invocation",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			keys, err := extractInvocationKeys(ctx, testCase.requestMetadata)
			if err != nil {
				t.Fatal(err)
			}
			if len(keys) != 2 {
				t.Fatalf("Got %d invocation keys, want 2", len(keys))
			}
			if !proto.Equal(keys[0], wantAuthKey) {
				t.Errorf("Authentication key = %v, want %v", keys[0], wantAuthKey)
			}
			wantInvocationKey, err := anypb.New(&remoteexecution.RequestMetadata{
				ToolInvocationId: testCase.invocationID,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(keys[1], wantInvocationKey) {
				t.Errorf("Tool invocation key = %v, want %v", keys[1], wantInvocationKey)
			}
		})
	}
}
