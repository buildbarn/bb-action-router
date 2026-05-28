package actionrouter

import (
	"archive/tar"
	"context"
	"io"
	"sort"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-storage/pkg/blobstore"
	"github.com/buildbarn/bb-storage/pkg/blobstore/buffer"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
	"github.com/buildbarn/bb-storage/pkg/util"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type imageRootChild struct {
	directory *imageRootDirectory
	file      *imageRootFile
	symlink   *imageRootSymlink
}

// imageRootDirectory is an intermediate representation of the flattened image
// filesystem before it is converted to REv2 Directory messages.
type imageRootDirectory struct {
	children map[path.Component]imageRootChild
}

type imageRootFile struct {
	digest       digest.Digest
	isExecutable bool
}

type imageRootSymlink struct {
	target string
}

// imageRootTarPath is an implementation of path.ComponentWalker that is used
// to normalise paths contained in image root tar entries.
type imageRootTarPath struct {
	components []path.Component
}

func (p *imageRootTarPath) OnDirectory(name path.Component) (path.GotDirectoryOrSymlink, error) {
	p.components = append(p.components, name)
	return path.GotDirectory{
		Child:        p,
		IsReversible: true,
	}, nil
}

func (p *imageRootTarPath) OnTerminal(name path.Component) (*path.GotSymlink, error) {
	p.components = append(p.components, name)
	return nil, nil
}

func (p *imageRootTarPath) OnUp() (path.ComponentWalker, error) {
	if len(p.components) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Path resolves to a location outside the image root")
	}
	p.components = p.components[:len(p.components)-1]
	return p, nil
}

func newImageRootDirectory() *imageRootDirectory {
	return &imageRootDirectory{
		children: map[path.Component]imageRootChild{},
	}
}

func readImageRootTar(ctx context.Context, contentAddressableStorage blobstore.BlobAccess, digestFunction digest.Function, r io.Reader) (*imageRootDirectory, error) {
	rootDirectory := newImageRootDirectory()
	tarReader := tar.NewReader(r)
	// mutate.Extract() returns a flattened filesystem tar, so OCI whiteouts
	// have already been applied by go-containerregistry.
	for {
		header, err := tarReader.Next()
		if err != nil {
			if err == io.EOF {
				return rootDirectory, nil
			}
			return nil, util.StatusWrap(err, "Failed to read image root tar")
		}
		components, err := parseImageRootTarPath(header.Name)
		if err != nil {
			return nil, util.StatusWrapf(err, "Could not parse the image root tar path %#v", header.Name)
		}
		// Tar entries are not guaranteed to list parent directories before children.
		switch header.Typeflag {
		case tar.TypeDir:
			if _, err := rootDirectory.getOrCreateDirectory(components); err != nil {
				return nil, util.StatusWrapf(err, "Invalid image root directory %#v", header.Name)
			}
		case tar.TypeReg:
			fileDigest, err := uploadImageRootFile(ctx, contentAddressableStorage, digestFunction, tarReader, header.Size)
			if err != nil {
				return nil, util.StatusWrapf(err, "Failed to upload image root file %#v", header.Name)
			}
			if err := rootDirectory.setChild(components, imageRootChild{
				file: &imageRootFile{
					digest:       fileDigest,
					isExecutable: header.Mode&0o111 != 0,
				},
			}); err != nil {
				return nil, util.StatusWrapf(err, "Invalid image root file %#v", header.Name)
			}
		case tar.TypeLink:
			// A tar hard link has no file contents of its own. Represent it in REv2 as
			// another file node that refers to the already-seen target file digest.
			// TODO: There are edge cases which need solving here.
			targetComponents, err := parseImageRootTarPath(header.Linkname)
			if err != nil {
				return nil, util.StatusWrapf(err, "Could not parse the image root hard link target %#v", header.Linkname)
			}
			targetFile, err := rootDirectory.getFile(targetComponents)
			if err != nil {
				return nil, util.StatusWrapf(err, "Invalid image root hard link %#v to %#v", header.Name, header.Linkname)
			}
			if err := rootDirectory.setChild(components, imageRootChild{
				file: targetFile,
			}); err != nil {
				return nil, util.StatusWrapf(err, "Invalid image root hard link %#v", header.Name)
			}
		case tar.TypeSymlink:
			if err := rootDirectory.setChild(components, imageRootChild{
				symlink: &imageRootSymlink{
					target: header.Linkname,
				},
			}); err != nil {
				return nil, util.StatusWrapf(err, "Invalid image root symlink %#v", header.Name)
			}
		default:
			return nil, status.Errorf(codes.InvalidArgument, "Image root tar entry %#v has unsupported file type %d", header.Name, header.Typeflag)
		}
	}
}

// CAS uploads require the digest before Put() can be called, so buffer one file
// while computing its digest.
func uploadImageRootFile(ctx context.Context, contentAddressableStorage blobstore.BlobAccess, digestFunction digest.Function, r io.Reader, sizeBytes int64) (digest.Digest, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return digest.BadDigest, err
	}
	digestGenerator := digestFunction.NewGenerator(sizeBytes)
	if _, err := digestGenerator.Write(data); err != nil {
		return digest.BadDigest, err
	}
	fileDigest := digestGenerator.Sum()
	if err := contentAddressableStorage.Put(
		ctx,
		fileDigest,
		buffer.NewCASBufferFromByteSlice(fileDigest, data, buffer.UserProvided)); err != nil {
		return digest.BadDigest, err
	}
	return fileDigest, nil
}

func (d *imageRootDirectory) uploadImageRootDirectory(ctx context.Context, contentAddressableStorage blobstore.BlobAccess, digestFunction digest.Function) (digest.Digest, error) {
	// Upload child directories first, so this directory can refer to them by
	// digest.
	childNames := make(path.ComponentsList, 0, len(d.children))
	for name := range d.children {
		childNames = append(childNames, name)
	}
	sort.Sort(childNames)

	directory := remoteexecution.Directory{}
	for _, name := range childNames {
		child := d.children[name]
		nameString := name.String()
		switch {
		case child.directory != nil:
			childDigest, err := child.directory.uploadImageRootDirectory(ctx, contentAddressableStorage, digestFunction)
			if err != nil {
				return digest.BadDigest, util.StatusWrapf(err, "Failed to upload image root directory %#v", nameString)
			}
			directory.Directories = append(directory.Directories, &remoteexecution.DirectoryNode{
				Name:   nameString,
				Digest: childDigest.GetProto(),
			})
		case child.file != nil:
			directory.Files = append(directory.Files, &remoteexecution.FileNode{
				Name:         nameString,
				Digest:       child.file.digest.GetProto(),
				IsExecutable: child.file.isExecutable,
			})
		case child.symlink != nil:
			directory.Symlinks = append(directory.Symlinks, &remoteexecution.SymlinkNode{
				Name:   nameString,
				Target: child.symlink.target,
			})
		default:
			return digest.BadDigest, status.Errorf(codes.Internal, "Image root child %#v has no type", nameString)
		}
	}
	return blobstore.CASPutProto(ctx, contentAddressableStorage, &directory, digestFunction)
}

func parseImageRootTarPath(name string) ([]path.Component, error) {
	var p imageRootTarPath
	if err := path.Resolve(path.UNIXFormat.NewParser(name), path.NewRelativeScopeWalker(&p)); err != nil {
		return nil, err
	}
	return p.components, nil
}

func (d *imageRootDirectory) setChild(components []path.Component, child imageRootChild) error {
	if len(components) == 0 {
		return status.Error(codes.InvalidArgument, "Path resolves to the image root")
	}
	parentDirectory, err := d.getOrCreateDirectory(components[:len(components)-1])
	if err != nil {
		return err
	}
	name := components[len(components)-1]
	if _, ok := parentDirectory.children[name]; ok {
		return status.Errorf(codes.InvalidArgument, "Directory contains multiple children named %#v", name.String())
	}
	parentDirectory.children[name] = child
	return nil
}

func (d *imageRootDirectory) getFile(components []path.Component) (*imageRootFile, error) {
	if len(components) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Path resolves to the image root")
	}
	parentDirectory, err := d.getDirectory(components[:len(components)-1])
	if err != nil {
		return nil, err
	}
	name := components[len(components)-1]
	child, ok := parentDirectory.children[name]
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "Directory does not contain child named %#v", name.String())
	}
	if child.file == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Child %#v is not a file", name.String())
	}
	return child.file, nil
}

func (d *imageRootDirectory) getDirectory(components []path.Component) (*imageRootDirectory, error) {
	directory := d
	for _, component := range components {
		child, ok := directory.children[component]
		if !ok {
			return nil, status.Errorf(codes.InvalidArgument, "Directory does not contain child named %#v", component.String())
		}
		if child.directory == nil {
			return nil, status.Errorf(codes.InvalidArgument, "Path component %#v is not a directory", component.String())
		}
		directory = child.directory
	}
	return directory, nil
}

func (d *imageRootDirectory) getOrCreateDirectory(components []path.Component) (*imageRootDirectory, error) {
	directory := d
	for _, component := range components {
		child, ok := directory.children[component]
		if !ok {
			childDirectory := newImageRootDirectory()
			directory.children[component] = imageRootChild{
				directory: childDirectory,
			}
			directory = childDirectory
			continue
		}
		if child.directory == nil {
			return nil, status.Errorf(codes.InvalidArgument, "Path component %#v is not a directory", component.String())
		}
		directory = child.directory
	}
	return directory, nil
}
