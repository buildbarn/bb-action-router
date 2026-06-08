package blobstore

import (
	"context"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	bb_blobstore "github.com/buildbarn/bb-storage/pkg/blobstore"
	"github.com/buildbarn/bb-storage/pkg/blobstore/buffer"
	bb_digest "github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/buildbarn/bb-storage/pkg/util"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// CASUploadingLayerExtractor allows us to upload a directory structure to CAS at the
// same time as we're traversing a Docker layer by implementing the LayerContentsVisitor
// interface.
type CASUploadingLayerExtractor struct {
	cas            bb_blobstore.BlobAccess
	digestFunction bb_digest.Function
	root           *uploadDirState
}

// NewCASUploadingLayerExtractor creates a new extractor that uploads Docker layer contents to CAS as it traverses them.
func NewCASUploadingLayerExtractor(cas bb_blobstore.BlobAccess, df bb_digest.Function) *CASUploadingLayerExtractor {
	return &CASUploadingLayerExtractor{
		cas:            cas,
		digestFunction: df,
		root:           newUploadDirState(),
	}
}

// OnDirectorySeen is called when a directory is encountered in the Docker layer.
func (e *CASUploadingLayerExtractor) OnDirectorySeen(ctx context.Context, path string) error {
	// Create the directory dirNode
	_ = e.root.getOrCreateChildDirState(path)

	return nil
}

// OnFileSeen is called when a file is encountered in the Docker layer. It uploads the file to CAS and handles whiteout files.
func (e *CASUploadingLayerExtractor) OnFileSeen(ctx context.Context, path string, data io.Reader, mode int64) error {
	dirname, filename := filepath.Split(path)
	dirNode := e.root.getOrCreateChildDirState(dirname)

	// Handle Docker whiteout files
	// See https://github.com/opencontainers/image-spec/blob/main/layer.md#whiteouts
	if strings.HasPrefix(filename, ".wh.") {
		if filename == ".wh..wh..opq" {
			// Opaque whiteout - clear all files and directories in the current directory
			dirNode.files = make(map[string]*remoteexecution.FileNode)
			dirNode.subDirs = make(map[string]*uploadDirState)
			dirNode.symlinks = make(map[string]*remoteexecution.SymlinkNode)
			dirNode.hardLinks = make(map[string]string)
		} else {
			// Regular whiteout - delete the specific file/directory
			targetName := strings.TrimPrefix(filename, ".wh.")
			dirNode.delete(targetName)
		}
		return nil
	}

	// Writing a file will overwrite past entries with the same name.
	dirNode.delete(filename)

	// The CAS APIs require io.ReaderAt (a seekable reader), but that's not what
	// we get when working with tar files. Since we need to copy the contents
	// once to compute the digest, dumping it into a tempfile seems an
	// acceptable workaround.
	f, err := os.CreateTemp("", "casUpload")
	if err != nil {
		return util.StatusWrap(err, "Failed to create temp file")
	}
	// We don't close/remove the temp file here, instead it's done by
	// the object returned from newSectionReadCloser after the upload
	// is completed.

	digestGenerator := e.digestFunction.NewGenerator(math.MaxInt64)
	sizeBytes, err := io.Copy(io.MultiWriter(digestGenerator, f), data)
	if err != nil {
		f.Close()
		if rmErr := os.Remove(f.Name()); rmErr != nil {
			log.Printf("failed deleting temporary file %s: %v", f.Name(), rmErr)
		}
		return util.StatusWrap(err, "Failed to compute file digest")
	}
	blobDigest := digestGenerator.Sum()

	if err := e.cas.Put(
		ctx,
		blobDigest,
		buffer.NewCASBufferFromReader(
			blobDigest,
			newSectionReadCloser(f, 0, sizeBytes),
			buffer.UserProvided,
		),
	); err != nil {
		return util.StatusWrap(err, "Failed to upload file")
	}

	dirNode.files[filename] = &remoteexecution.FileNode{
		Name:         filename,
		Digest:       blobDigest.GetProto(),
		IsExecutable: (mode & 0o111) != 0,
	}

	return nil
}

// OnLinkSeen is called when a hard link is encountered in the Docker layer.
func (e *CASUploadingLayerExtractor) OnLinkSeen(ctx context.Context, path, target string) error {
	dirname, linkname := filepath.Split(path)
	dirNode := e.root.getOrCreateChildDirState(dirname)
	dirNode.delete(linkname)
	dirNode.hardLinks[linkname] = target
	return nil
}

// OnSymlinkSeen is called when a symbolic link is encountered in the Docker layer.
func (e *CASUploadingLayerExtractor) OnSymlinkSeen(ctx context.Context, path, target string) error {
	return e.addSymlink(path, target)
}

func (e *CASUploadingLayerExtractor) addSymlink(path, target string) error {
	dirname, linkname := filepath.Split(path)
	dirNode := e.root.getOrCreateChildDirState(dirname)
	dirNode.delete(linkname)
	dirNode.symlinks[linkname] = &remoteexecution.SymlinkNode{
		Name:   linkname,
		Target: target,
	}
	return nil
}

// OnLayerComplete is called after all other OnSeen* calls corresponding to a single Docker layer.
func (e *CASUploadingLayerExtractor) OnLayerComplete(ctx context.Context) error {
	// Resolve hard links separately for each layer (otherwise overwrites in later layers
	// could incorrectly cause contents of the hard links to change).
	return e.root.resolveHardlinks(e.root)
}

// UploadDirectories uploads the accumulated filesystem state to the CAS
// returning the digest of the root directory.
func (e *CASUploadingLayerExtractor) UploadDirectories(ctx context.Context) (*remoteexecution.Digest, error) {
	digest, err := e.root.upload(ctx, e.cas, e.digestFunction)
	if err != nil {
		return nil, err
	}

	e.root = newUploadDirState()
	return digest, nil
}

// uploadDirState tracks the state of a directory. We can't use REv2.Directory
// directly as that expects a set of digests as children. Instead we accumulate
// child information here and write it to CAS in the upload method
type uploadDirState struct {
	subDirs   map[string]*uploadDirState
	files     map[string]*remoteexecution.FileNode
	symlinks  map[string]*remoteexecution.SymlinkNode
	hardLinks map[string]string
}

func newUploadDirState() *uploadDirState {
	return &uploadDirState{
		subDirs:   make(map[string]*uploadDirState),
		files:     make(map[string]*remoteexecution.FileNode),
		symlinks:  make(map[string]*remoteexecution.SymlinkNode),
		hardLinks: make(map[string]string),
	}
}

func splitPath(path string) []string {
	path = filepath.Clean(path)
	if filepath.IsAbs(path) {
		path = path[1:]
	}
	if path == "" || path == "." || path == ".." {
		return []string{}
	}
	return strings.Split(path, "/")
}

// getChildDirState finds the uploadDirState node associated with path.
// path is assumed to be a directory path. returns nil if no node can be
// found.
func (u *uploadDirState) getChildDirState(path string) *uploadDirState {
	lookupPath := splitPath(path)

	node := u
	for _, childName := range lookupPath {
		if child, ok := node.subDirs[childName]; ok {
			node = child
		} else {
			return nil
		}
	}
	return node
}

// getChildDirState finds the uploadDirState node associated with path
// path is assumed to be a directory path. This function creates any nodes
// along the way and so never returns nil.
func (u *uploadDirState) getOrCreateChildDirState(path string) *uploadDirState {
	lookupPath := splitPath(path)

	node := u
	for _, childName := range lookupPath {
		if child, ok := node.subDirs[childName]; ok {
			node = child
			continue
		}
		child := newUploadDirState()
		node.delete(childName)
		node.subDirs[childName] = child
		node = child
	}
	return node
}

func (u *uploadDirState) delete(filename string) {
	// Docker seems to implement "most recent one wins" semantics.
	delete(u.files, filename)
	delete(u.subDirs, filename)
	delete(u.symlinks, filename)
	delete(u.hardLinks, filename)
}

func (u *uploadDirState) resolveHardlinks(root *uploadDirState) error {
	for hlinkName, target := range u.hardLinks {
		targetDir, targetName := filepath.Split(target)

		targetDirDs := root.getChildDirState(targetDir)
		if targetDirDs == nil {
			return status.Errorf(codes.Internal, "couldn't find source dir for %s -> %s hardlink", hlinkName, target)
		}

		// If the source is a file then simply create a copy of the
		// REv2.FileNode. A copy is not exactly like a hard link, but
		// actions aren't expected to write to their inputs, so it should
		// work fine for our purposes.
		if targetFile := targetDirDs.files[targetName]; targetFile != nil {
			u.delete(hlinkName)
			u.files[hlinkName] = &remoteexecution.FileNode{
				Name:         hlinkName,
				Digest:       targetFile.Digest,
				IsExecutable: targetFile.IsExecutable,
			}
		} else if targetDir := targetDirDs.subDirs[targetName]; targetDir != nil {
			// The source is a directory. This is very unusual, but some
			// filesystems allow hard links with directories. Ideally we'd clone
			// the entire dir hierarchy here but that requires detecting cycles,
			// so we (somewhat incorrectly) "downgrade" the hardlink to a
			// symlink.
			u.delete(hlinkName)
			u.symlinks[hlinkName] = &remoteexecution.SymlinkNode{
				Name:   hlinkName,
				Target: target,
			}
		} else if targetLink := targetDirDs.symlinks[targetName]; targetLink != nil {
			// A hardlink to a symlink is very unlikely, but we support it.
			u.delete(hlinkName)
			u.symlinks[hlinkName] = &remoteexecution.SymlinkNode{
				Name:   hlinkName,
				Target: targetLink.Target,
			}
		} else {
			return status.Errorf(codes.Internal, "Couldn't find source file for %s -> %s hardlink", hlinkName, target)
		}
	}

	for _, child := range u.subDirs {
		if err := child.resolveHardlinks(root); err != nil {
			return err
		}
	}

	u.hardLinks = make(map[string]string)
	return nil
}

func assertNoDuplicateNames(reDir *remoteexecution.Directory) error {
	seen := make(map[string]any)
	for _, subDir := range reDir.Directories {
		if _, ok := seen[subDir.Name]; !ok {
			seen[subDir.Name] = subDir
		} else {
			return status.Errorf(codes.Internal, "duplicate entires for %s in directory", subDir.Name)
		}
	}
	for _, symlink := range reDir.Symlinks {
		if _, ok := seen[symlink.Name]; !ok {
			seen[symlink.Name] = symlink
		} else {
			return status.Errorf(codes.Internal, "duplicate entires for %s in directory", symlink.Name)
		}
	}
	for _, file := range reDir.Files {
		if _, ok := seen[file.Name]; !ok {
			seen[file.Name] = file
		} else {
			return status.Errorf(codes.Internal, "duplicate entires for %s in directory", file.Name)
		}
	}
	return nil
}

func (u *uploadDirState) upload(ctx context.Context, cas bb_blobstore.BlobAccess, df bb_digest.Function) (*remoteexecution.Digest, error) {
	dirNodes := []*remoteexecution.DirectoryNode{}
	for name, chUplSt := range u.subDirs {
		chDig, err := chUplSt.upload(ctx, cas, df)
		if err != nil {
			return nil, err
		}
		dirNodes = append(dirNodes, &remoteexecution.DirectoryNode{
			Name:   name,
			Digest: chDig,
		})
	}

	// TODO: The protocol requires these to be sorted but the current implementation doesn't
	// upload these to a "real" CAS, so the only downside is that the digests aren't stable.
	directory := &remoteexecution.Directory{
		Directories: dirNodes,
		Symlinks:    toSlice(u.symlinks),
		Files:       toSlice(u.files),
	}

	err := assertNoDuplicateNames(directory)
	if err != nil {
		return nil, err
	}

	data, err := proto.Marshal(directory)
	if err != nil {
		return nil, util.StatusWrapWithCode(err, codes.Internal, "failed to marshall dir: %v")
	}

	digestGenerator := df.NewGenerator(int64(len(data)))
	if _, err := digestGenerator.Write(data); err != nil {
		panic(err)
	}
	dirDigest := digestGenerator.Sum()

	// Upload the directory to CAS
	if err := cas.Put(
		ctx,
		dirDigest,
		buffer.NewCASBufferFromByteSlice(dirDigest, data, buffer.UserProvided),
	); err != nil {
		return nil, util.StatusWrap(err, "Failed to upload directory")
	}
	return dirDigest.GetProto(), nil
}

// toSlice converts a map of values to a slice
func toSlice[V any](m map[string]V) []V {
	result := make([]V, 0, len(m))
	for _, v := range m {
		result = append(result, v)
	}
	return result
}

type tmpFileCloser struct {
	f *os.File
}

func (c *tmpFileCloser) Close() error {
	e1 := c.f.Close()
	e2 := os.Remove(c.f.Name())

	if e1 != nil {
		return e1
	}
	return e2
}

// newSectionReadCloser adapts the temporary file to the API expected by BlobAccess.
func newSectionReadCloser(f *os.File, off, n int64) io.ReadCloser {
	return &struct {
		io.SectionReader
		io.Closer
	}{
		SectionReader: *io.NewSectionReader(f, off, n),
		Closer:        &tmpFileCloser{f: f},
	}
}
