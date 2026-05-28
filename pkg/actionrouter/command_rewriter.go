package actionrouter

import (
	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
	"github.com/buildbarn/bb-storage/pkg/util"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// commandWorkingDirectory is an implementation of path.ComponentWalker that is
// used to normalise a REv2 Command working directory before placing it under
// the configured workspace directory.
type commandWorkingDirectory struct {
	components []path.Component
}

func (cwd *commandWorkingDirectory) OnDirectory(name path.Component) (path.GotDirectoryOrSymlink, error) {
	cwd.components = append(cwd.components, name)
	return path.GotDirectory{
		Child:        cwd,
		IsReversible: true,
	}, nil
}

func (cwd *commandWorkingDirectory) OnTerminal(name path.Component) (*path.GotSymlink, error) {
	cwd.components = append(cwd.components, name)
	return nil, nil
}

func (cwd *commandWorkingDirectory) OnUp() (path.ComponentWalker, error) {
	if len(cwd.components) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Path resolves to a location outside the input root directory")
	}
	cwd.components = cwd.components[:len(cwd.components)-1]
	return cwd, nil
}

func rewriteCommandWorkingDirectory(command *remoteexecution.Command, workspaceDirectory path.Component) (*remoteexecution.Command, error) {
	var workingDirectory commandWorkingDirectory
	// Resolve the working directory first, so ".." components are applied
	// before the workspace directory is prepended.
	if err := path.Resolve(path.UNIXFormat.NewParser(command.WorkingDirectory), path.NewRelativeScopeWalker(&workingDirectory)); err != nil {
		return nil, util.StatusWrap(err, "Invalid working directory")
	}

	var trace *path.Trace
	trace = trace.Append(workspaceDirectory)
	for _, component := range workingDirectory.components {
		trace = trace.Append(component)
	}

	rewrittenCommand := remoteexecution.Command{}
	proto.Merge(&rewrittenCommand, command)
	rewrittenCommand.WorkingDirectory = trace.GetUNIXString()
	return &rewrittenCommand, nil
}
