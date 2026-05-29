package blobstore

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	bb_blobstore "github.com/buildbarn/bb-storage/pkg/blobstore"
	"github.com/buildbarn/bb-storage/pkg/blobstore/buffer"
	"github.com/buildbarn/bb-storage/pkg/blobstore/slicing"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	"github.com/buildbarn/bb-action-router/internal/mock"
	"google.golang.org/protobuf/proto"
)

func TestCASUploadingLayerExtractor(t *testing.T) {
	t.Run("SimpleUpload", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		ctx := context.Background()

		mockCas := mock.NewMockBlobAccess(ctrl)
		df := digest.MustNewFunction("test", remoteexecution.DigestFunction_SHA256)
		trackingCas := newFilesystemTrackingBlobAccess(mockCas, df)

		extractor := NewCASUploadingLayerExtractor(trackingCas, df)

		// Expect Put calls: 3 files + 2 subdirectories (dir1, dir2) + 1 root directory
		mockCas.EXPECT().Put(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).Times(6)

		// Visit files
		require.NoError(t, extractor.OnDirectorySeen(ctx, "dir1"))
		require.NoError(t, extractor.OnFileSeen(ctx, "dir1/file1.txt", strings.NewReader("hello"), 0o644))
		require.NoError(t, extractor.OnFileSeen(ctx, "dir1/file2.txt", strings.NewReader("everyone"), 0o755))

		require.NoError(t, extractor.OnDirectorySeen(ctx, "dir2"))
		require.NoError(t, extractor.OnFileSeen(ctx, "dir2/file3.txt", strings.NewReader("test"), 0o644))

		require.NoError(t, extractor.OnSymlinkSeen(ctx, "dir2/link", "../dir1/file1.txt"))

		rootDigest, err := extractor.UploadDirectories(ctx)
		require.NoError(t, err)
		require.NotNil(t, rootDigest)

		expected := []string{
			"dir:(1 files: [file3.txt], symlinks: [link->../dir1/file1.txt])",
			"dir:(2 dirs: [dir1, dir2])",
			"dir:(2 files: [file1.txt, file2.txt*])",
			"file:blob[4 bytes]",
			"file:blob[5 bytes]",
			"file:blob[8 bytes]",
		}
		require.Equal(t, expected, trackingCas.GetEntries())
	})

	t.Run("HardLinks", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		ctx := context.Background()

		mockCas := mock.NewMockBlobAccess(ctrl)
		df := digest.MustNewFunction("test", remoteexecution.DigestFunction_SHA256)
		trackingCas := newFilesystemTrackingBlobAccess(mockCas, df)

		extractor := NewCASUploadingLayerExtractor(trackingCas, df)

		// Expect Put calls: 1 file + 1 dir + 1 root directory
		mockCas.EXPECT().Put(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).Times(3)

		require.NoError(t, extractor.OnFileSeen(ctx, "dir1/original.txt", strings.NewReader("content"), 0o644))
		require.NoError(t, extractor.OnLinkSeen(ctx, "dir1/hardlink.txt", "dir1/original.txt"))

		// Complete the layer to resolve hardlinks
		require.NoError(t, extractor.OnLayerComplete(ctx))

		rootDigest, err := extractor.UploadDirectories(ctx)
		require.NoError(t, err)
		require.NotNil(t, rootDigest)

		expected := []string{
			"dir:(1 dirs: [dir1])",
			"dir:(2 files: [hardlink.txt, original.txt])",
			"file:blob[7 bytes]",
		}
		require.Equal(t, expected, trackingCas.GetEntries())
	})

	t.Run("RegularWhiteout", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		ctx := context.Background()

		mockCas := mock.NewMockBlobAccess(ctrl)
		df := digest.MustNewFunction("test", remoteexecution.DigestFunction_SHA256)
		trackingCas := newFilesystemTrackingBlobAccess(mockCas, df)

		extractor := NewCASUploadingLayerExtractor(trackingCas, df)

		// Expect Put calls: 2 files + 1 subdirectory + 1 root directory
		// The whiteout file itself is not uploaded
		mockCas.EXPECT().Put(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).Times(4)

		require.NoError(t, extractor.OnFileSeen(ctx, "dir/file1.txt", strings.NewReader("content1"), 0o644))
		require.NoError(t, extractor.OnFileSeen(ctx, "dir/file2.txt", strings.NewReader("content22"), 0o644))

		// Add a whiteout file that deletes file1.txt
		require.NoError(t, extractor.OnFileSeen(ctx, "dir/.wh.file1.txt", strings.NewReader(""), 0o644))

		// Only file2.txt should remain, file1.txt should be deleted
		rootDigest, err := extractor.UploadDirectories(ctx)
		require.NoError(t, err)
		require.NotNil(t, rootDigest)

		expected := []string{
			"dir:(1 dirs: [dir])",
			"dir:(1 files: [file2.txt])",
			"file:blob[8 bytes]", // It's OK that file1 is uploaded to CAS.
			"file:blob[9 bytes]",
		}
		require.Equal(t, expected, trackingCas.GetEntries())
	})

	t.Run("OpaqueWhiteout", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		ctx := context.Background()

		mockCas := mock.NewMockBlobAccess(ctrl)
		df := digest.MustNewFunction("test", remoteexecution.DigestFunction_SHA256)
		trackingCas := newFilesystemTrackingBlobAccess(mockCas, df)

		extractor := NewCASUploadingLayerExtractor(trackingCas, df)

		// Expect Put calls: 2 initial files + 1 new file after whiteout + 1 subdirectory + 1 root directory
		// The whiteout file itself is not uploaded
		mockCas.EXPECT().Put(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).Times(5)

		// Add file1.txt and file2.txt
		require.NoError(t, extractor.OnFileSeen(ctx, "dir/file1.txt", strings.NewReader("content1"), 0o644))
		require.NoError(t, extractor.OnFileSeen(ctx, "dir/file2.txt", strings.NewReader("content22"), 0o644))

		// Add opaque whiteout that clears the entire directory
		require.NoError(t, extractor.OnFileSeen(ctx, "dir/.wh..wh..opq", strings.NewReader(""), 0o644))
		require.NoError(t, extractor.OnFileSeen(ctx, "dir/file3.txt", strings.NewReader("content333"), 0o644))

		// Only file3.txt should remain, file1.txt and file2.txt should be deleted
		rootDigest, err := extractor.UploadDirectories(ctx)
		require.NoError(t, err)
		require.NotNil(t, rootDigest)

		expected := []string{
			"dir:(1 dirs: [dir])",
			"dir:(1 files: [file3.txt])",
			"file:blob[10 bytes]",
			"file:blob[8 bytes]",
			"file:blob[9 bytes]",
		}
		require.Equal(t, expected, trackingCas.GetEntries())
	})

	t.Run("DuplicateSymlinks", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		ctx := context.Background()

		mockCas := mock.NewMockBlobAccess(ctrl)
		df := digest.MustNewFunction("test", remoteexecution.DigestFunction_SHA256)
		trackingCas := newFilesystemTrackingBlobAccess(mockCas, df)

		extractor := NewCASUploadingLayerExtractor(trackingCas, df)

		// Expect Put calls: 1 subdirectory + 1 root directory
		mockCas.EXPECT().Put(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).Times(2)

		// Layer 1: Create a symlink
		require.NoError(t, extractor.OnSymlinkSeen(ctx, "dir/link.txt", "/target1"))

		// Layer 2: Same symlink with different target (should overwrite, not duplicate)
		require.NoError(t, extractor.OnSymlinkSeen(ctx, "dir/link.txt", "/target2"))

		rootDigest, err := extractor.UploadDirectories(ctx)
		require.NoError(t, err)
		require.NotNil(t, rootDigest)

		expected := []string{
			"dir:(1 dirs: [dir])",
			"dir:(symlinks: [link.txt->/target2])",
		}
		require.Equal(t, expected, trackingCas.GetEntries())
	})

	t.Run("FileHardlink", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		ctx := context.Background()

		mockCas := mock.NewMockBlobAccess(ctrl)
		df := digest.MustNewFunction("test", remoteexecution.DigestFunction_SHA256)
		trackingCas := newFilesystemTrackingBlobAccess(mockCas, df)

		extractor := NewCASUploadingLayerExtractor(trackingCas, df)

		// Expect Put calls: 1 file + 2 dirs + 1 root directory
		mockCas.EXPECT().Put(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).Times(4)

		// Create original file
		require.NoError(t, extractor.OnFileSeen(ctx, "dir1/original.txt", strings.NewReader("content"), 0o644))

		// Create hardlink to the file in a different directory
		require.NoError(t, extractor.OnLinkSeen(ctx, "dir2/hardlink.txt", "dir1/original.txt"))

		require.NoError(t, extractor.OnLayerComplete(ctx))

		rootDigest, err := extractor.UploadDirectories(ctx)
		require.NoError(t, err)
		require.NotNil(t, rootDigest)

		expected := []string{
			"dir:(1 files: [hardlink.txt])",
			"dir:(1 files: [original.txt])",
			"dir:(2 dirs: [dir1, dir2])",
			"file:blob[7 bytes]",
		}
		require.Equal(t, expected, trackingCas.GetEntries())
	})

	t.Run("DirectoryHardlink", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		ctx := context.Background()

		mockCas := mock.NewMockBlobAccess(ctrl)
		df := digest.MustNewFunction("test", remoteexecution.DigestFunction_SHA256)
		trackingCas := newFilesystemTrackingBlobAccess(mockCas, df)

		extractor := NewCASUploadingLayerExtractor(trackingCas, df)

		// Expect Put calls: 1 file + 1 dir + 1 root directory
		mockCas.EXPECT().Put(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).Times(3)

		// Create a directory with a file
		require.NoError(t, extractor.OnDirectorySeen(ctx, "original"))
		require.NoError(t, extractor.OnFileSeen(ctx, "original/file.txt", strings.NewReader("content"), 0o644))

		// Create a hardlink to the directory
		require.NoError(t, extractor.OnLinkSeen(ctx, "hardlink", "original"))

		require.NoError(t, extractor.OnLayerComplete(ctx))
		rootDigest, err := extractor.UploadDirectories(ctx)
		require.NoError(t, err)
		require.NotNil(t, rootDigest)

		// Verify the filesystem structure - directory hardlink becomes a symlink
		entries := trackingCas.GetEntries()
		expected := []string{
			"dir:(1 dirs: [original], symlinks: [hardlink->original])",
			"dir:(1 files: [file.txt])",
			"file:blob[7 bytes]",
		}
		require.Equal(t, expected, entries)
	})

	t.Run("SymlinkOverwritesFile", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		ctx := context.Background()

		mockCas := mock.NewMockBlobAccess(ctrl)
		df := digest.MustNewFunction("test", remoteexecution.DigestFunction_SHA256)
		trackingCas := newFilesystemTrackingBlobAccess(mockCas, df)

		extractor := NewCASUploadingLayerExtractor(trackingCas, df)

		// Expect Put calls: 1 file + 1 subdirectory + 1 root directory
		mockCas.EXPECT().Put(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).Times(3)

		require.NoError(t, extractor.OnFileSeen(ctx, "dir/item", strings.NewReader("content"), 0o644))
		// Overwrite with a symlink
		require.NoError(t, extractor.OnSymlinkSeen(ctx, "dir/item", "/target"))

		rootDigest, err := extractor.UploadDirectories(ctx)
		require.NoError(t, err)
		require.NotNil(t, rootDigest)

		// Verify the filesystem structure - file should be overwritten by symlink
		entries := trackingCas.GetEntries()
		expected := []string{
			"dir:(1 dirs: [dir])",
			"dir:(symlinks: [item->/target])",
			"file:blob[7 bytes]",
		}
		require.Equal(t, expected, entries)
	})

	t.Run("FileOverwritesSymlink", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		ctx := context.Background()

		mockCas := mock.NewMockBlobAccess(ctrl)
		df := digest.MustNewFunction("test", remoteexecution.DigestFunction_SHA256)
		trackingCas := newFilesystemTrackingBlobAccess(mockCas, df)

		extractor := NewCASUploadingLayerExtractor(trackingCas, df)

		// Expect Put calls: 1 file + 1 subdirectory + 1 root directory
		mockCas.EXPECT().Put(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).Times(3)

		require.NoError(t, extractor.OnSymlinkSeen(ctx, "dir/item", "/target"))
		// Overwrite with a file
		require.NoError(t, extractor.OnFileSeen(ctx, "dir/item", strings.NewReader("content"), 0o644))

		rootDigest, err := extractor.UploadDirectories(ctx)
		require.NoError(t, err)
		require.NotNil(t, rootDigest)

		// Verify the filesystem structure - symlink should be overwritten by file
		entries := trackingCas.GetEntries()
		expected := []string{
			"dir:(1 dirs: [dir])",
			"dir:(1 files: [item])",
			"file:blob[7 bytes]",
		}
		require.Equal(t, expected, entries)
	})

	t.Run("HardlinkInSubdirErrorPropagates", func(t *testing.T) {
		// A hardlink with an unresolvable target inside a subdirectory must
		// surface as an error from OnLayerComplete. Before the fix the
		// recursive call's error was silently dropped.
		ctrl := gomock.NewController(t)
		ctx := context.Background()

		mockCas := mock.NewMockBlobAccess(ctrl)
		df := digest.MustNewFunction("test", remoteexecution.DigestFunction_SHA256)
		trackingCas := newFilesystemTrackingBlobAccess(mockCas, df)

		extractor := NewCASUploadingLayerExtractor(trackingCas, df)

		require.NoError(t, extractor.OnDirectorySeen(ctx, "subdir"))
		require.NoError(t, extractor.OnLinkSeen(ctx, "subdir/dangling", "nonexistent/target"))

		err := extractor.OnLayerComplete(ctx)
		require.Error(t, err)
		require.Contains(t, err.Error(), "couldn't find source dir")
	})
}

func TestSplitPath(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "empty path",
			input:    "",
			expected: []string{},
		},
		{
			name:     "single component",
			input:    "file",
			expected: []string{"file"},
		},
		{
			name:     "multiple components",
			input:    "dir/subdir/file",
			expected: []string{"dir", "subdir", "file"},
		},
		{
			name:     "path with leading slash",
			input:    "/dir/file",
			expected: []string{"dir", "file"},
		},
		{
			name:     "path with trailing slash",
			input:    "dir/subdir/",
			expected: []string{"dir", "subdir"},
		},
		{
			name:     "path with double slashes",
			input:    "dir//subdir///file",
			expected: []string{"dir", "subdir", "file"},
		},
		{
			name:     "path with dot components",
			input:    "dir/./subdir/../file",
			expected: []string{"dir", "file"},
		},
		{
			name:     "current directory",
			input:    ".",
			expected: []string{},
		},
		{
			name:     "parent directory",
			input:    "..",
			expected: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := splitPath(tt.input)
			require.Equal(t, tt.expected, result)
		})
	}
}

// FilesystemTrackingBlobAccess wraps a BlobAccess and tracks filesystem structure.
type filesystemTrackingBlobAccess struct {
	inner          bb_blobstore.BlobAccess
	digestFunction digest.Function
	digests        map[string]any
	entries        []string
}

func newFilesystemTrackingBlobAccess(inner bb_blobstore.BlobAccess, df digest.Function) *filesystemTrackingBlobAccess {
	return &filesystemTrackingBlobAccess{
		inner:          inner,
		digestFunction: df,
		digests:        make(map[string]any),
		entries:        []string{},
	}
}

func (f *filesystemTrackingBlobAccess) Get(ctx context.Context, digest digest.Digest) buffer.Buffer {
	return f.inner.Get(ctx, digest)
}

func (f *filesystemTrackingBlobAccess) Put(ctx context.Context, digest digest.Digest, b buffer.Buffer) error {
	data, err := b.ToByteSlice(100 * 1024 * 1024)
	if err != nil {
		return f.inner.Put(ctx, digest, b)
	}

	var dir remoteexecution.Directory
	if err := proto.Unmarshal(data, &dir); err == nil {
		f.entries = append(f.entries, f.formatDirectoryEntry(&dir))
	} else {
		f.entries = append(f.entries, fmt.Sprintf("file:blob[%d bytes]", len(data)))
	}

	newBuffer := buffer.NewCASBufferFromByteSlice(digest, data, buffer.UserProvided)
	return f.inner.Put(ctx, digest, newBuffer)
}

func (f *filesystemTrackingBlobAccess) formatDirectoryEntry(dir *remoteexecution.Directory) string {
	var parts []string

	if len(dir.Files) > 0 {
		var fileNames []string
		for _, file := range dir.Files {
			name := file.Name
			if file.IsExecutable {
				name += "*"
			}
			fileNames = append(fileNames, name)
		}
		sort.Strings(fileNames)
		parts = append(parts, fmt.Sprintf("%d files: [%s]", len(dir.Files), strings.Join(fileNames, ", ")))
	}

	if len(dir.Directories) > 0 {
		var dirNames []string
		for _, subDir := range dir.Directories {
			dirNames = append(dirNames, subDir.Name)
		}
		sort.Strings(dirNames)
		parts = append(parts, fmt.Sprintf("%d dirs: [%s]", len(dir.Directories), strings.Join(dirNames, ", ")))
	}

	if len(dir.Symlinks) > 0 {
		var symlinks []string
		for _, symlink := range dir.Symlinks {
			symlinks = append(symlinks, fmt.Sprintf("%s->%s", symlink.Name, symlink.Target))
		}
		sort.Strings(symlinks)
		parts = append(parts, fmt.Sprintf("symlinks: [%s]", strings.Join(symlinks, ", ")))
	}

	if len(parts) == 0 {
		return "dir:(empty)"
	}

	return fmt.Sprintf("dir:(%s)", strings.Join(parts, ", "))
}

func (f *filesystemTrackingBlobAccess) GetEntries() []string {
	slices.Sort(f.entries)
	return f.entries
}

func (f *filesystemTrackingBlobAccess) FindMissing(ctx context.Context, digests digest.Set) (digest.Set, error) {
	return f.inner.FindMissing(ctx, digests)
}

func (f *filesystemTrackingBlobAccess) GetCapabilities(ctx context.Context, instanceName digest.InstanceName) (*remoteexecution.ServerCapabilities, error) {
	return f.inner.GetCapabilities(ctx, instanceName)
}

func (f *filesystemTrackingBlobAccess) GetFromComposite(ctx context.Context, parentDigest, childDigest digest.Digest, slicer slicing.BlobSlicer) buffer.Buffer {
	return f.inner.GetFromComposite(ctx, parentDigest, childDigest, slicer)
}
