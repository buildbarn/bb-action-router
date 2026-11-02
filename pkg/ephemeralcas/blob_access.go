package ephemeralcas

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	bb_blobstore "github.com/buildbarn/bb-storage/pkg/blobstore"
	"github.com/buildbarn/bb-storage/pkg/blobstore/chunk"
	blobstore_configuration "github.com/buildbarn/bb-storage/pkg/blobstore/configuration"
	"github.com/buildbarn/bb-storage/pkg/cas"
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

// Put writes a chunk to its digest-named path.
func (b *DirectoryBackedBlobAccess) Put(ctx context.Context, d digest.Digest, value *chunk.Chunk) error {
	return os.WriteFile(b.blobPath(d), value.GetBytes(), 0o600)
}

// Get reads a chunk from disk.
func (b *DirectoryBackedBlobAccess) Get(ctx context.Context, d digest.Digest) (*chunk.Chunk, error) {
	data, err := os.ReadFile(b.blobPath(d))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, status.Errorf(codes.NotFound, "blob %s not found", d)
		}
		return nil, err
	}
	c := chunk.NewChunk(data)
	if c.ComputeDigest(d.GetDigestFunction()) != d {
		return nil, status.Errorf(codes.DataLoss, "blob %s is corrupt", d)
	}
	return c, nil
}

// FindMissing reports chunks that are not yet present on disk.
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

// GetCapabilities returns capabilities for the local CAS.
func (b *DirectoryBackedBlobAccess) GetCapabilities(ctx context.Context, instanceName digest.InstanceName) (*remoteexecution.ServerCapabilities, error) {
	params, _ := b.FetchCDCParameters(ctx, instanceName)
	return &remoteexecution.ServerCapabilities{CacheCapabilities: &remoteexecution.CacheCapabilities{RepMaxCdcParams: params}}, nil
}

// FetchCDCParameters returns the local CAS chunking parameters.
func (DirectoryBackedBlobAccess) FetchCDCParameters(ctx context.Context, instanceName digest.InstanceName) (*remoteexecution.RepMaxCdcParams, error) {
	return &remoteexecution.RepMaxCdcParams{MinChunkSizeBytes: 256 << 10, HorizonSizeBytes: 2 << 20}, nil
}

// CAS returns the components for accessing the local CAS.
func (b *DirectoryBackedBlobAccess) CAS() blobstore_configuration.ContentAddressableStorageInfo {
	cms := &directoryBackedMappingStorage{DirectoryBackedBlobAccess: b}
	return blobstore_configuration.ContentAddressableStorageInfo{
		ReadInfo: cas.ReadInfo{
			ChunkBytesReader:     cas.NewChunkBytesReader(b),
			ChunkMappingFetcher:  bb_blobstore.NewBlobAccessMappingFetcher(cms),
			CDCParametersFetcher: b,
		},
		ChunkStorage:        b,
		ChunkMappingStorage: cms,
		DigestKeyFormat:     digest.KeyWithoutInstance,
	}
}

var _ bb_blobstore.BlobAccess[*chunk.Chunk] = (*DirectoryBackedBlobAccess)(nil)

type directoryBackedMappingStorage struct {
	*DirectoryBackedBlobAccess
}

func (b *directoryBackedMappingStorage) blobPath(d digest.Digest) string {
	return b.DirectoryBackedBlobAccess.blobPath(d) + ".mapping"
}

func (b *directoryBackedMappingStorage) Get(ctx context.Context, d digest.Digest) (chunk.Mapping, error) {
	data, err := os.ReadFile(b.blobPath(d))
	if err != nil {
		if os.IsNotExist(err) {
			return chunk.Mapping{}, status.Errorf(codes.NotFound, "chunk mapping %s not found", d)
		}
		return chunk.Mapping{}, err
	}
	return chunk.NewMappingFromCompactBinary(data, d.GetDigestFunction(), true)
}

func (b *directoryBackedMappingStorage) Put(ctx context.Context, d digest.Digest, value chunk.Mapping) error {
	data, err := value.ToCompactBinary()
	if err != nil {
		return err
	}
	return os.WriteFile(b.blobPath(d), data, 0o600)
}

func (b *directoryBackedMappingStorage) FindMissing(ctx context.Context, digests digest.Set) (digest.Set, error) {
	missing := digest.NewSetBuilder(digests.Length())
	for _, d := range digests.Items() {
		if _, err := os.Stat(b.blobPath(d)); os.IsNotExist(err) {
			missing.Add(d)
		} else if err != nil {
			return digest.EmptySet, err
		}
	}
	return missing.Build(), nil
}
