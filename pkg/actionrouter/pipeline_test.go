package actionrouter

import (
	"context"
	"slices"
	"testing"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-action-router/internal/mock"
	"github.com/buildbarn/bb-storage/pkg/blobstore/buffer"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestPutDirectorySortedEntries(t *testing.T) {
	ctx := context.Background()
	df := digest.MustNewFunction("test", remoteexecution.DigestFunction_SHA256)
	emptyDigest := df.NewGenerator(0).Sum().GetProto()
	dir := &remoteexecution.Directory{
		Files: []*remoteexecution.FileNode{
			{Name: "a", Digest: emptyDigest},
			{Name: "z", Digest: emptyDigest},
		},
		Directories: []*remoteexecution.DirectoryNode{
			{Name: "bazel_exec_root", Digest: emptyDigest},
			{Name: "usr", Digest: emptyDigest},
		},
		Symlinks: []*remoteexecution.SymlinkNode{
			{Name: "bin", Target: "usr/bin"},
			{Name: "lib", Target: "usr/lib"},
		},
	}
	want, err := proto.Marshal(dir)
	require.NoError(t, err)
	slices.Reverse(dir.Files)
	slices.Reverse(dir.Directories)
	slices.Reverse(dir.Symlinks)

	mockCas := mock.NewMockBlobAccess(gomock.NewController(t))
	mockCas.EXPECT().Put(ctx, gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, d digest.Digest, b buffer.Buffer) error {
		got, err := b.ToByteSlice(10000)
		require.NoError(t, err)
		require.Equal(t, want, got)
		gen := df.NewGenerator(int64(len(want)))
		_, err = gen.Write(want)
		require.NoError(t, err)
		require.Equal(t, gen.Sum(), d)
		return nil
	})
	_, err = putDirectory(ctx, mockCas, dir, df)
	require.NoError(t, err)
}
