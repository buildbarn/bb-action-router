package blobstore

import (
	"context"
	"testing"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-action-router/internal/mock"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
)

func TestActionCacheRefStore(t *testing.T) {
	t.Run("Put", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		ctx := context.Background()

		actionCache := mock.NewMockBlobAccess(ctrl)

		store := NewActionCacheRefStore(actionCache, 1024, UnixUser{UID: 1000, GID: 1000, Name: "build"})

		df := digest.MustNewFunction("test", remoteexecution.DigestFunction_SHA256)

		rootDigest := &remoteexecution.Digest{
			Hash:      "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			SizeBytes: 0,
		}

		ref := "docker.io/buildbarn/test:latest"

		actionCache.EXPECT().Put(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)

		err := store.Put(ctx, ref, rootDigest, df)
		require.NoError(t, err)
	})
}
