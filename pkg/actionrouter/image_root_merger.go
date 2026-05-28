package actionrouter

import (
	"sort"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func mergeImageRootDirectory(
	imageRootDirectory *remoteexecution.Directory,
	workspaceDirectory path.Component,
	inputRootDigest digest.Digest,
) (*remoteexecution.Directory, error) {
	workspaceDirectoryName := workspaceDirectory.String()
	mergedRootDirectory := remoteexecution.Directory{}
	proto.Merge(&mergedRootDirectory, imageRootDirectory)

	children := make(map[path.Component]string, len(mergedRootDirectory.Directories)+len(mergedRootDirectory.Files)+len(mergedRootDirectory.Symlinks)+1)
	directoriesByName := make(map[path.Component]*remoteexecution.DirectoryNode, len(mergedRootDirectory.Directories)+1)
	directoryNames := make(path.ComponentsList, 0, len(mergedRootDirectory.Directories)+1)
	for _, directory := range mergedRootDirectory.Directories {
		component, ok := path.NewComponent(directory.Name)
		if !ok {
			return nil, status.Errorf(codes.InvalidArgument, "Directory %#v has an invalid name", directory.Name)
		}
		if name, ok := children[component]; ok {
			return nil, status.Errorf(codes.InvalidArgument, "Directory contains multiple children named %#v", name)
		}
		children[component] = directory.Name
		directoriesByName[component] = directory
		directoryNames = append(directoryNames, component)
	}

	for _, file := range mergedRootDirectory.Files {
		component, ok := path.NewComponent(file.Name)
		if !ok {
			return nil, status.Errorf(codes.InvalidArgument, "File %#v has an invalid name", file.Name)
		}
		if name, ok := children[component]; ok {
			return nil, status.Errorf(codes.InvalidArgument, "Directory contains multiple children named %#v", name)
		}
		children[component] = file.Name
	}

	for _, symlink := range mergedRootDirectory.Symlinks {
		component, ok := path.NewComponent(symlink.Name)
		if !ok {
			return nil, status.Errorf(codes.InvalidArgument, "Symlink %#v has an invalid name", symlink.Name)
		}
		if name, ok := children[component]; ok {
			return nil, status.Errorf(codes.InvalidArgument, "Directory contains multiple children named %#v", name)
		}
		children[component] = symlink.Name
	}

	if name, ok := children[workspaceDirectory]; ok {
		return nil, status.Errorf(codes.InvalidArgument, "Image root already contains child named %#v", name)
	}

	directoriesByName[workspaceDirectory] = &remoteexecution.DirectoryNode{
		Name:   workspaceDirectoryName,
		Digest: inputRootDigest.GetProto(),
	}
	directoryNames = append(directoryNames, workspaceDirectory)
	sort.Sort(directoryNames)

	mergedRootDirectory.Directories = mergedRootDirectory.Directories[:0]
	for _, name := range directoryNames {
		mergedRootDirectory.Directories = append(mergedRootDirectory.Directories, directoriesByName[name])
	}
	return &mergedRootDirectory, nil
}
