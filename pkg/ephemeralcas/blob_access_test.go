package ephemeralcas

import (
	"context"
	"testing"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-storage/pkg/blobstore/chunk"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/stretchr/testify/require"
)

// digestOf computes the SHA-256 digest of `content` under the test
// instance name.
func digestOf(t *testing.T, content string) digest.Digest {
	t.Helper()
	df := digest.MustNewFunction("test", remoteexecution.DigestFunction_SHA256)
	g := df.NewGenerator(int64(len(content)))
	if _, err := g.Write([]byte(content)); err != nil {
		t.Fatalf("digest generator write: %v", err)
	}
	return g.Sum()
}

func TestPutThenGet(t *testing.T) {
	ctx := context.Background()
	ba := New(t.TempDir())

	content := "hello, world"
	d := digestOf(t, content)

	err := ba.Put(ctx, d, chunk.NewChunk([]byte(content)))
	require.NoError(t, err)

	got, err := ba.Get(ctx, d)
	require.NoError(t, err)
	require.Equal(t, content, string(got.GetBytes()))
}

func TestFindMissing(t *testing.T) {
	ctx := context.Background()
	ba := New(t.TempDir())

	stored := digestOf(t, "stored blob")
	absent := digestOf(t, "never stored")

	err := ba.Put(ctx, stored, chunk.NewChunk([]byte("stored blob")))
	require.NoError(t, err)

	missing, err := ba.FindMissing(ctx, digest.NewSetBuilder(2).Add(stored).Add(absent).Build())
	require.NoError(t, err)
	require.Equal(t, []digest.Digest{absent}, missing.Items())
}
