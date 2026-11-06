package actionrouter

import (
	"os"
	"path/filepath"
	"testing"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	pb "github.com/buildbarn/bb-action-router/pkg/proto/configuration/bb_docker_action_router"
	blobstore_configuration "github.com/buildbarn/bb-storage/pkg/blobstore/configuration"
	"github.com/stretchr/testify/require"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// runScript builds a run_starlark op from inline src and applies it to s.
func runScript(t *testing.T, src string, s *pipelineState) error {
	t.Helper()
	op, err := newRunStarlarkOp(&pb.RunStarlark{Source: &pb.RunStarlark_Script{Script: src}})
	require.NoError(t, err)
	return op.apply(s)
}

func envNames(command *remoteexecution.Command) []string {
	var names []string
	for _, v := range command.EnvironmentVariables {
		names = append(names, v.Name)
	}
	return names
}

func TestRunStarlarkRequiresScript(t *testing.T) {
	_, err := newRunStarlarkOp(&pb.RunStarlark{})
	require.Error(t, err)
}

func TestRunStarlarkFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "process.star")
	require.NoError(t, os.WriteFile(path, []byte(`
def process(action):
    action.arguments = ["/wrap"] + action.arguments
`), 0o644))
	s := newState([]string{"cmd"}, "", nil, nil)
	op, err := newRunStarlarkOp(&pb.RunStarlark{Source: &pb.RunStarlark_ScriptFile{ScriptFile: path}})
	require.NoError(t, err)
	require.NoError(t, op.apply(s))
	require.Equal(t, []string{"/wrap", "cmd"}, s.command.Arguments)
}

func TestRunStarlarkRejectsMissingFile(t *testing.T) {
	_, err := newRunStarlarkOp(&pb.RunStarlark{
		Source: &pb.RunStarlark_ScriptFile{ScriptFile: filepath.Join(t.TempDir(), "nope.star")},
	})
	require.Error(t, err)
}

func TestRunStarlarkRequiresProcessFunction(t *testing.T) {
	_, err := newRunStarlarkOp(&pb.RunStarlark{Source: &pb.RunStarlark_Script{Script: "x = 1"}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "process")
}

func TestRunStarlarkRejectsNonFunctionProcess(t *testing.T) {
	_, err := newRunStarlarkOp(&pb.RunStarlark{Source: &pb.RunStarlark_Script{Script: "process = 1"}})
	require.Error(t, err)
}

func TestRunStarlarkRejectsSyntaxError(t *testing.T) {
	_, err := newRunStarlarkOp(&pb.RunStarlark{Source: &pb.RunStarlark_Script{Script: "def process(action)\n  pass"}})
	require.Error(t, err)
}

func TestRunStarlarkArgumentsAssign(t *testing.T) {
	s := newState([]string{"echo", "hi"}, "", nil, nil)
	err := runScript(t, `
def process(action):
    action.arguments = ["/wrap", "--"] + action.arguments + ["--trailing"]
`, s)
	require.NoError(t, err)
	require.True(t, s.commandChanged)
	require.Equal(t, []string{"/wrap", "--", "echo", "hi", "--trailing"}, s.command.Arguments)
}

func TestRunStarlarkArgumentsReadable(t *testing.T) {
	s := newState([]string{"a", "b", "c"}, "", nil, nil)
	err := runScript(t, `
def process(action):
    if len(action.arguments) == 3 and action.arguments[0] == "a":
        action.arguments = action.arguments[1:]
`, s)
	require.NoError(t, err)
	require.Equal(t, []string{"b", "c"}, s.command.Arguments)
}

// In-place list mutation works now that arguments is a real cached list that is
// read back after process().
func TestRunStarlarkArgumentsAppendInPlace(t *testing.T) {
	s := newState([]string{"cmd"}, "", nil, nil)
	err := runScript(t, `
def process(action):
    action.arguments.append("--flag")
`, s)
	require.NoError(t, err)
	require.True(t, s.commandChanged)
	require.Equal(t, []string{"cmd", "--flag"}, s.command.Arguments)
}

// Reading a field without changing it must not mark the action changed, so no
// needless Command re-upload / platform re-sort happens.
func TestRunStarlarkReadOnlyAccessDoesNotMarkChanged(t *testing.T) {
	s := newState([]string{"cmd"}, "", []*remoteexecution.Platform_Property{prop("OSFamily", "linux")},
		[]*remoteexecution.Command_EnvironmentVariable{{Name: "PATH", Value: "/bin"}})
	err := runScript(t, `
def process(action):
    _ = action.arguments[0]
    _ = action.platform["OSFamily"]
    _ = action.environment["PATH"]
`, s)
	require.NoError(t, err)
	require.False(t, s.commandChanged)
	require.False(t, s.platformChanged)
}

func TestRunStarlarkPlatformMapReadWrite(t *testing.T) {
	s := newState(nil, "", []*remoteexecution.Platform_Property{
		prop("ContainerBaseImage", "docker://busybox@"+testSha),
		prop("requires-network", "false"),
	}, nil)
	err := runScript(t, `
def process(action):
    if action.platform["requires-network"] == "false":
        action.platform["isolate"] = "true"
    action.platform.pop("ContainerBaseImage")
`, s)
	require.NoError(t, err)
	require.True(t, s.platformChanged)
	sortPlatform(s.action.Platform)
	require.Equal(t, []*remoteexecution.Platform_Property{
		prop("isolate", "true"),
		prop("requires-network", "false"),
	}, s.action.Platform.Properties)
}

func TestRunStarlarkPlatformMembershipAndGet(t *testing.T) {
	s := newState(nil, "", []*remoteexecution.Platform_Property{prop("OSFamily", "linux")}, nil)
	err := runScript(t, `
def process(action):
    if "OSFamily" in action.platform and "Missing" not in action.platform:
        action.platform["verified"] = action.platform.get("OSFamily")
    action.platform["fallback"] = action.platform.get("absent", "default")
`, s)
	require.NoError(t, err)
	require.Equal(t, "linux", platformView{s.action.Platform}.Get("verified"))
	require.Equal(t, "default", platformView{s.action.Platform}.Get("fallback"))
}

func TestRunStarlarkPlatformSetCollapsesDuplicates(t *testing.T) {
	s := newState(nil, "", []*remoteexecution.Platform_Property{
		prop("dns", "1.1.1.1"),
		prop("dns", "8.8.8.8"),
	}, nil)
	err := runScript(t, `
def process(action):
    action.platform["dns"] = "9.9.9.9"
`, s)
	require.NoError(t, err)
	sortPlatform(s.action.Platform)
	require.Equal(t, []*remoteexecution.Platform_Property{prop("dns", "9.9.9.9")}, s.action.Platform.Properties)
}

func TestRunStarlarkPlatformIterate(t *testing.T) {
	s := newState(nil, "", []*remoteexecution.Platform_Property{
		prop("a", "1"),
		prop("b", "2"),
	}, nil)
	err := runScript(t, `
def process(action):
    keys = []
    for k in action.platform:
        keys.append(k)
    action.platform["keys"] = ",".join(sorted(keys))
    action.platform["count"] = str(len(action.platform) - 1)
`, s)
	require.NoError(t, err)
	require.Equal(t, "a,b", platformView{s.action.Platform}.Get("keys"))
	require.Equal(t, "2", platformView{s.action.Platform}.Get("count"))
}

func TestRunStarlarkPlatformWholeAssign(t *testing.T) {
	s := newState(nil, "", []*remoteexecution.Platform_Property{prop("old", "x")}, nil)
	err := runScript(t, `
def process(action):
    action.platform = {k: v for k, v in action.platform.items() if k != "old"}
    action.platform["new"] = "y"
`, s)
	require.NoError(t, err)
	sortPlatform(s.action.Platform)
	require.Equal(t, []*remoteexecution.Platform_Property{prop("new", "y")}, s.action.Platform.Properties)
}

func TestRunStarlarkEnvironmentMap(t *testing.T) {
	s := newState(nil, "", nil, []*remoteexecution.Command_EnvironmentVariable{
		{Name: "DROP", Value: "x"},
		{Name: "PATH", Value: "old"},
		{Name: "KEEP", Value: "y"},
	})
	err := runScript(t, `
def process(action):
    action.environment.pop("DROP")
    action.environment["PATH"] = "/usr/bin:/bin"
`, s)
	require.NoError(t, err)
	require.True(t, s.commandChanged)
	sortEnvironment(s.command)
	require.Equal(t, []*remoteexecution.Command_EnvironmentVariable{
		{Name: "KEEP", Value: "y"},
		{Name: "PATH", Value: "/usr/bin:/bin"},
	}, s.command.EnvironmentVariables)
}

func TestRunStarlarkEnvironmentUpdate(t *testing.T) {
	s := newState(nil, "", nil, nil)
	err := runScript(t, `
def process(action):
    action.environment.update({"A": "1", "B": "2"})
`, s)
	require.NoError(t, err)
	sortEnvironment(s.command)
	require.Equal(t, []string{"A", "B"}, envNames(s.command))
}

func TestRunStarlarkWorkingDirectory(t *testing.T) {
	s := newState(nil, "sub/dir", nil, nil)
	err := runScript(t, `
def process(action):
    action.working_directory = "prefix/" + action.working_directory
`, s)
	require.NoError(t, err)
	require.True(t, s.commandChanged)
	require.Equal(t, "prefix/sub/dir", s.command.WorkingDirectory)
}

func TestRunStarlarkAssertPlatformPropertyPasses(t *testing.T) {
	s := newState(nil, "", []*remoteexecution.Platform_Property{prop("ContainerBaseImage", "docker://busybox@"+testSha)}, nil)
	err := runScript(t, `
def process(action):
    image = action.platform["ContainerBaseImage"]
    if not image.startswith("docker://") or "@sha256:" not in image:
        fail("ContainerBaseImage must be a pinned docker:// digest: " + image)
`, s)
	require.NoError(t, err)
}

// A validation failure via fail() must surface as InvalidArgument, so the
// scheduler rejects the action with the right code.
func TestRunStarlarkValidationFailsWithInvalidArgument(t *testing.T) {
	s := newState(nil, "", []*remoteexecution.Platform_Property{prop("ContainerBaseImage", "docker://busybox:latest")}, nil)
	err := runScript(t, `
def process(action):
    image = action.platform["ContainerBaseImage"]
    if "@sha256:" not in image:
        fail("ContainerBaseImage must be a pinned docker:// digest: " + image)
`, s)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// An explicit fail(...) is a deliberate rejection -> InvalidArgument (4xx).
func TestRunStarlarkFailIsInvalidArgument(t *testing.T) {
	s := newState(nil, "", nil, nil)
	err := runScript(t, `
def process(action):
    fail("boom")
`, s)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// A runtime error (not fail) is a bug in the script -> Internal (5xx), because
// the action is fine; the server's configured script is wrong.
func TestRunStarlarkRuntimeErrorIsInternal(t *testing.T) {
	s := newState(nil, "", nil, nil)
	err := runScript(t, `
def process(action):
    x = 1 + "a"
`, s)
	require.Error(t, err)
	require.Equal(t, codes.Internal, status.Code(err))
}

func TestRunStarlarkRejectsNonStringArgument(t *testing.T) {
	s := newState([]string{"cmd"}, "", nil, nil)
	err := runScript(t, `
def process(action):
    action.arguments = [1, 2]
`, s)
	require.Error(t, err)
	require.Equal(t, codes.Internal, status.Code(err))
}

func TestRunStarlarkRejectsAssignToReadOnlyField(t *testing.T) {
	s := newState(nil, "", nil, nil)
	err := runScript(t, `
def process(action):
    action.invocation_id = "x"
`, s)
	require.Error(t, err)
	require.Equal(t, codes.Internal, status.Code(err))
}

func TestRunStarlarkMissingKeyIsInternal(t *testing.T) {
	s := newState(nil, "", nil, nil)
	err := runScript(t, `
def process(action):
    x = action.platform["absent"]
`, s)
	require.Error(t, err)
	require.Equal(t, codes.Internal, status.Code(err))
}

func TestRunStarlarkRequestMetadata(t *testing.T) {
	s := newState(nil, "", nil, nil)
	s.requestMetadata = &remoteexecution.RequestMetadata{ToolInvocationId: "inv-123", TargetId: "//foo:bar"}
	err := runScript(t, `
def process(action):
    action.environment["BUILD_INVOCATION_ID"] = action.invocation_id
    action.environment["BUILD_TARGET"] = action.target_label
`, s)
	require.NoError(t, err)
	sortEnvironment(s.command)
	require.Equal(t, []*remoteexecution.Command_EnvironmentVariable{
		{Name: "BUILD_INVOCATION_ID", Value: "inv-123"},
		{Name: "BUILD_TARGET", Value: "//foo:bar"},
	}, s.command.EnvironmentVariables)
}

func TestRunStarlarkRequestMetadataNilSafe(t *testing.T) {
	s := newState(nil, "", nil, nil) // requestMetadata is nil
	err := runScript(t, `
def process(action):
    action.environment["X"] = action.invocation_id
`, s)
	require.NoError(t, err)
	require.Equal(t, "", s.command.EnvironmentVariables[0].Value)
}

func TestRunStarlarkNoOpLeavesActionUnchanged(t *testing.T) {
	s := newState([]string{"cmd"}, "", nil, nil)
	err := runScript(t, `
def process(action):
    pass
`, s)
	require.NoError(t, err)
	require.False(t, s.commandChanged)
	require.False(t, s.platformChanged)
}

func TestRunStarlarkForLoop(t *testing.T) {
	s := newState(nil, "", nil, nil)
	err := runScript(t, `
def process(action):
    for name in ["A", "B", "C"]:
        action.environment[name] = "1"
`, s)
	require.NoError(t, err)
	sortEnvironment(s.command)
	require.Equal(t, []string{"A", "B", "C"}, envNames(s.command))
}

func TestNewPipelineBuildsRunStarlark(t *testing.T) {
	op, err := buildOperation(&pb.Operation{
		Kind: &pb.Operation_RunStarlark{RunStarlark: &pb.RunStarlark{Source: &pb.RunStarlark_Script{Script: "def process(action):\n    pass\n"}}},
	}, blobstore_configuration.ContentAddressableStorageInfo{}, nil, 0)
	require.NoError(t, err)
	require.IsType(t, &runStarlarkOp{}, op)
}
