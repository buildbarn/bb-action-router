package ephemeralcas

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	bb_blobstore "github.com/buildbarn/bb-storage/pkg/blobstore"
	"github.com/buildbarn/bb-storage/pkg/blobstore/buffer"
	"github.com/buildbarn/bb-storage/pkg/blobstore/slicing"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// DirectoryBackedBlobAccess is a simple filesystem-backed BlobAccess.
type DirectoryBackedBlobAccess struct {
	dir string
}

// New creates a DirectoryBackedBlobAccess backed by `dir`. The directory
// must already exist; the caller owns its lifetime.
func New(dir string) *DirectoryBackedBlobAccess {
	return &DirectoryBackedBlobAccess{dir: dir}
}

func (b *DirectoryBackedBlobAccess) blobPath(d digest.Digest) string {
	return filepath.Join(b.dir, d.GetKey(digest.KeyWithoutInstance))
}

// Put writes `buf` directly to its digest-named path in `dir`.
func (b *DirectoryBackedBlobAccess) Put(ctx context.Context, d digest.Digest, buf buffer.Buffer) error {
	path := b.blobPath(d)
	f, err := os.Create(path)
	if err != nil {
		buf.Discard()
		return err
	}

	writeErr := buf.IntoWriter(f)
	closeErr := f.Close()
	if writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		os.Remove(path)
		return writeErr
	}
	return nil
}

// Get returns a Buffer that streams from the on-disk blob.
func (b *DirectoryBackedBlobAccess) Get(ctx context.Context, d digest.Digest) buffer.Buffer {
	f, err := os.Open(b.blobPath(d))
	if err != nil {
		if os.IsNotExist(err) {
			return buffer.NewBufferFromError(status.Errorf(codes.NotFound, "blob %s not found", d))
		}
		return buffer.NewBufferFromError(err)
	}
	return buffer.NewCASBufferFromReader(d, f, buffer.UserProvided)
}

// FindMissing reports digests that are not yet present on disk so that
// BatchedStoreBlobAccess can skip duplicate uploads within a batch.
func (b *DirectoryBackedBlobAccess) FindMissing(ctx context.Context, digests digest.Set) (digest.Set, error) {
	missing := digest.NewSetBuilder(digests.Length())
	for _, d := range digests.Items() {
		_, err := os.Stat(b.blobPath(d))
		if err == nil {
			continue
		}
		if !os.IsNotExist(err) {
			return digest.EmptySet, fmt.Errorf("stat blob %s: %w", d, err)
		}
		missing.Add(d)
	}
	return missing.Build(), nil
}

// GetFromComposite is part of the BlobAccess interface but isn't exercised
// by the docker root materialization path.
func (b *DirectoryBackedBlobAccess) GetFromComposite(ctx context.Context, parentDigest, childDigest digest.Digest, slicer slicing.BlobSlicer) buffer.Buffer {
	return buffer.NewBufferFromError(status.Error(codes.Unimplemented, "GetFromComposite not supported by ephemeralcas"))
}

// GetCapabilities is part of the BlobAccess interface but isn't exercised
// by the docker root materialization path.
func (b *DirectoryBackedBlobAccess) GetCapabilities(ctx context.Context, instanceName digest.InstanceName) (*remoteexecution.ServerCapabilities, error) {
	return nil, status.Error(codes.Unimplemented, "GetCapabilities not supported by ephemeralcas")
}

// Compile-time check that DirectoryBackedBlobAccess satisfies the interface.
var _ bb_blobstore.BlobAccess = (*DirectoryBackedBlobAccess)(nil)
