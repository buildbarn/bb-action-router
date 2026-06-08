package blobstore

import (
	"context"
	"log"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	bb_blobstore "github.com/buildbarn/bb-storage/pkg/blobstore"
	"github.com/buildbarn/bb-storage/pkg/blobstore/buffer"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/buildbarn/bb-storage/pkg/util"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ActionCacheRefStore allows us to store the mapping between
// (docker image ref) -> (root dir digest) in the ActionCache.
type ActionCacheRefStore struct {
	actionCache             bb_blobstore.BlobAccess
	maximumMessageSizeBytes int
}

// NewActionCacheRefStore creates a new ActionCacheRefStore that stores Docker image ref to digest mappings in the action cache.
func NewActionCacheRefStore(
	actionCache bb_blobstore.BlobAccess,
	maximumMessageSizeBytes int,
) *ActionCacheRefStore {
	return &ActionCacheRefStore{
		actionCache:             actionCache,
		maximumMessageSizeBytes: maximumMessageSizeBytes,
	}
}

func (ActionCacheRefStore) computeCacheKeyDigest(ref string, digestFunction digest.Function) (digest.Digest, error) {
	cacheKey := &remoteexecution.Action{
		CommandDigest: &remoteexecution.Digest{
			// We just need some hash here to make the storage layer think the Action proto is well-formed. We use
			// a l337-speak "docker ac" prefix to distinguish these entries from other keys.
			Hash:      "d0c4e70ac0000000000000000000000000000000000000000000000000000000",
			SizeBytes: 0,
		},
		Platform: &remoteexecution.Platform{
			Properties: []*remoteexecution.Platform_Property{
				{Name: "container-image", Value: ref},
			},
		},
	}

	cacheKeyData, err := proto.Marshal(cacheKey)
	if err != nil {
		return digest.BadDigest, util.StatusWrap(err, "Failed to marshal cache key")
	}

	cacheKeyDigestGen := digestFunction.NewGenerator(int64(len(cacheKeyData)))
	if _, err := cacheKeyDigestGen.Write(cacheKeyData); err != nil {
		return digest.BadDigest, util.StatusWrap(err, "Failed to generate cache key digest")
	}

	return cacheKeyDigestGen.Sum(), nil
}

// Put stores the mapping between a Docker image reference and its root directory digest in the action cache.
func (rs *ActionCacheRefStore) Put(ctx context.Context, ref string, rootDir *remoteexecution.Digest, digestFunction digest.Function) error {
	cacheKeyDigest, err := rs.computeCacheKeyDigest(ref, digestFunction)
	if err != nil {
		return err
	}
	log.Printf("Storing image %s with key %s ..", ref, cacheKeyDigest.String())

	// The ActionCache BlobAccess implementation assumes we're writing ActionResults.
	actionResult := &remoteexecution.ActionResult{
		StderrDigest: rootDir,
	}

	// Create a buffer from the proto directly
	// The action cache digest key is based on the Action, not the ActionResult
	if err := rs.actionCache.Put(
		ctx,
		cacheKeyDigest,
		buffer.NewProtoBufferFromProto(actionResult, buffer.UserProvided),
	); err != nil {
		return util.StatusWrap(err, "Failed to store action result")
	}

	return nil
}

// Get retrieves the root directory digest for a Docker image reference from the action cache.
func (rs *ActionCacheRefStore) Get(ctx context.Context, ref string, digestFunction digest.Function) (*remoteexecution.Digest, error) {
	cacheKeyDigest, err := rs.computeCacheKeyDigest(ref, digestFunction)
	if err != nil {
		return nil, err
	}
	log.Printf("Looking up image %s with key %s ..", ref, cacheKeyDigest.String())

	buf := rs.actionCache.Get(ctx, cacheKeyDigest)
	cachedData, err := buf.ToByteSlice(rs.maximumMessageSizeBytes)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, status.Errorf(codes.NotFound, "Cache entry not found for ref %s", ref)
		}
		return nil, util.StatusWrap(err, "Failed to get action result")
	}

	var actionResult remoteexecution.ActionResult
	if err := proto.Unmarshal(cachedData, &actionResult); err != nil {
		return nil, util.StatusWrap(err, "Failed to unmarshal action result")
	}

	if actionResult.StderrDigest == nil {
		return nil, status.Errorf(codes.Internal, "Cache entry for ref %s has no output directory", ref)
	}

	return actionResult.StderrDigest, nil
}
