package blobstore

import (
	"context"
	"testing"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-action-router/internal/mock"
	"github.com/buildbarn/bb-storage/pkg/blobstore/buffer"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestDirTreeVerifier_Happy_Path(t *testing.T) {
	ctrl := gomock.NewController(t)
	ctx := context.Background()

	cas := mock.NewMockBlobAccess(ctrl)
	verifier := NewDirTreeVerifier(cas, 1024)

	df := digest.MustNewFunction("test", remoteexecution.DigestFunction_SHA256)

	fileHash := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	testDir := &remoteexecution.Directory{
		Files: []*remoteexecution.FileNode{
			{
				Name: "test.txt",
				Digest: &remoteexecution.Digest{
					Hash:      fileHash,
					SizeBytes: 0,
				},
			},
		},
	}

	dirData, _ := proto.Marshal(testDir)
	dirDigest, _ := df.NewDigest("888ddbf1e9cd5b201f67629d4efa9a4a0fb1cf5a910e9a44209eb07485f5f99d", int64(len(dirData)))
	cas.EXPECT().Get(ctx, dirDigest).Return(buffer.NewValidatedBufferFromByteSlice(dirData))

	fileDigest, _ := df.NewDigest(fileHash, 0)
	fileDigestSet := digest.NewSetBuilder(1).Add(fileDigest).Build()
	cas.EXPECT().FindMissing(ctx, fileDigestSet).Return(digest.EmptySet, nil)

	err := verifier.Verify(ctx, dirDigest.GetProto(), df)
	require.NoError(t, err)
}

func TestDirTreeVerifier_MissingSubdirectory(t *testing.T) {
	ctrl := gomock.NewController(t)
	ctx := context.Background()

	cas := mock.NewMockBlobAccess(ctrl)
	verifier := NewDirTreeVerifier(cas, 1024)

	df := digest.MustNewFunction("test", remoteexecution.DigestFunction_SHA256)

	dirHash := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testDir := &remoteexecution.Directory{
		Directories: []*remoteexecution.DirectoryNode{
			{
				Name: "subdir",
				Digest: &remoteexecution.Digest{
					Hash:      dirHash,
					SizeBytes: 100,
				},
			},
		},
	}

	dirData, _ := proto.Marshal(testDir)
	dirDigest, _ := df.NewDigest("999eebf2e9cd5b201f67629d4efa9a4a0fb1cf5a910e9a44209eb07485f5f88e", int64(len(dirData)))
	cas.EXPECT().Get(ctx, dirDigest).Return(buffer.NewValidatedBufferFromByteSlice(dirData))

	subDirDigest, _ := df.NewDigest(dirHash, 100)
	cas.EXPECT().Get(ctx, subDirDigest).Return(buffer.NewBufferFromError(status.Error(codes.NotFound, "Subdirectory not found")))

	// Verify should fail with subdirectory not found
	err := verifier.Verify(ctx, dirDigest.GetProto(), df)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Failed to get directory")
	require.Contains(t, err.Error(), "Subdirectory not found")
}
