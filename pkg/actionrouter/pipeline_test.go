package actionrouter

import (
	"context"
	"slices"
	"testing"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-action-router/internal/mock"
	"github.com/buildbarn/bb-action-router/pkg/ephemeralcas"
	re_cas "github.com/buildbarn/bb-remote-execution/pkg/cas"
	"github.com/buildbarn/bb-storage/pkg/blobstore/chunk"
	bb_cas "github.com/buildbarn/bb-storage/pkg/cas"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/stretchr/testify/require"

	"go.uber.org/mock/gomock"

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

	mockCas := mock.NewMockBlobAccess[*chunk.Chunk](gomock.NewController(t))
	casInfo := ephemeralcas.New(t.TempDir()).CAS()
	casInfo.ChunkStorage = mockCas
	mockCas.EXPECT().FindMissing(ctx, gomock.Any()).DoAndReturn(func(_ context.Context, ds digest.Set) (digest.Set, error) { return ds, nil })
	mockCas.EXPECT().Put(ctx, gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, d digest.Digest, b *chunk.Chunk) error {
		got := b.GetBytes()
		require.Equal(t, want, got)
		gen := df.NewGenerator(int64(len(want)))
		_, err = gen.Write(want)
		require.NoError(t, err)
		require.Equal(t, gen.Sum(), d)
		return nil
	})
	protoPutter := re_cas.NewProtoPutter(bb_cas.NewReaderPutter(casInfo.ChunkStorage, casInfo.ChunkMappingStorage), casInfo.CDCParametersFetcher)
	_, err = putDirectory(ctx, protoPutter, dir, df)
	require.NoError(t, err)
}
