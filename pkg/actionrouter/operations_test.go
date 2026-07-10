package actionrouter

import (
	"strings"
	"testing"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	pb "github.com/buildbarn/bb-action-router/pkg/proto/configuration/bb_docker_action_router"
	"github.com/stretchr/testify/require"
)

// newState builds a pipelineState with the command preloaded, so op tests
// exercise the transformation logic without touching the CAS.
func newState(args []string, workingDir string, props []*remoteexecution.Platform_Property, env []*remoteexecution.Command_EnvironmentVariable) *pipelineState {
	return &pipelineState{
		action:  &remoteexecution.Action{Platform: &remoteexecution.Platform{Properties: props}},
		command: &remoteexecution.Command{Arguments: args, WorkingDirectory: workingDir, EnvironmentVariables: env},

		commandLoaded: true,
	}
}

func prop(name, value string) *remoteexecution.Platform_Property {
	return &remoteexecution.Platform_Property{Name: name, Value: value}
}

const testSha = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// The sideloaded EditCommand prepend arguments, kept in sync with the e2e overlay.
var sideloadedPrepend = []string{
	"/bin/bb_chroot_helper",
	`--docker-image-ref={{.Platform.Get "ContainerBaseImage" | trimPrefix "docker://"}}`,
	"--fetcher-socket=/var/fetcher/fetcher.sock",
	"--build-user=0:0",
	`{{if and (eq (.Platform.Get "requires-network") "false") (ne (.Platform.Get "requires-external") "true")}}--network-isolation{{else}}--no-network-isolation{{end}}`,
}

func TestEditCommandSideloadedNetworkIsolated(t *testing.T) {
	op, err := newEditCommandOp(&pb.EditCommand{PrependArguments: sideloadedPrepend})
	require.NoError(t, err)
	s := newState(
		[]string{"echo", "hello world"},
		"",
		[]*remoteexecution.Platform_Property{
			prop("ContainerBaseImage", "docker://busybox@"+testSha),
			prop("requires-network", "false"),
		}, nil,
	)
	require.NoError(t, op.apply(s))
	require.True(t, s.commandChanged)
	require.Equal(t, []string{
		"/bin/bb_chroot_helper",
		"--docker-image-ref=busybox@" + testSha,
		"--fetcher-socket=/var/fetcher/fetcher.sock",
		"--build-user=0:0",
		"--network-isolation",
		"echo", "hello world", // original argv preserved verbatim, spaces intact
	}, s.command.Arguments)
}

func TestEditCommandSideloadedNoNetworkIsolation(t *testing.T) {
	op, err := newEditCommandOp(&pb.EditCommand{PrependArguments: sideloadedPrepend})
	require.NoError(t, err)
	// requires-network absent -> not isolated.
	s := newState([]string{"true"}, "", []*remoteexecution.Platform_Property{
		prop("ContainerBaseImage", "docker://busybox@"+testSha),
	}, nil)
	require.NoError(t, op.apply(s))
	require.Contains(t, s.command.Arguments, "--no-network-isolation")
	require.NotContains(t, s.command.Arguments, "--network-isolation")
}

func TestEditCommandInlinePrependsHelper(t *testing.T) {
	op, err := newEditCommandOp(&pb.EditCommand{PrependArguments: []string{"/bin/bb_chroot_helper"}})
	require.NoError(t, err)
	s := newState([]string{"/usr/bin/make", "-j", "all"}, "", nil, nil)
	require.NoError(t, op.apply(s))
	require.Equal(t, []string{"/bin/bb_chroot_helper", "/usr/bin/make", "-j", "all"}, s.command.Arguments)
}

func TestEditCommandDropsEmptyAndAppends(t *testing.T) {
	op, err := newEditCommandOp(&pb.EditCommand{
		PrependArguments: []string{"/wrap", `{{if .Platform.Get "x"}}--x{{end}}`},
		AppendArguments:  []string{"--trailing"},
	})
	require.NoError(t, err)
	s := newState([]string{"cmd"}, "", nil, nil)
	require.NoError(t, op.apply(s))
	// The conditional --x renders empty and is dropped.
	require.Equal(t, []string{"/wrap", "cmd", "--trailing"}, s.command.Arguments)
}

func TestTruthy(t *testing.T) {
	for _, s := range []string{"true", "1", "yes", " True ", "anything"} {
		require.True(t, truthy(s), "%q", s)
	}
	for _, s := range []string{"", "  ", "false", "0", "FALSE", " 0 "} {
		require.False(t, truthy(s), "%q", s)
	}
}

func TestAssertPlatformProperty(t *testing.T) {
	op, err := newAssertPlatformPropertyOp(&pb.AssertPlatformProperty{
		Property: "ContainerBaseImage",
		Regex:    `^docker://.+@sha256:[0-9a-f]{64}$`,
	})
	require.NoError(t, err)

	ok := newState(nil, "", []*remoteexecution.Platform_Property{prop("ContainerBaseImage", "docker://busybox@"+testSha)}, nil)
	require.NoError(t, op.apply(ok))

	bad := newState(nil, "", []*remoteexecution.Platform_Property{prop("ContainerBaseImage", "docker://busybox:latest")}, nil)
	require.Error(t, op.apply(bad))
}

func TestAssertPlatformPropertyRejectsEmptyRegex(t *testing.T) {
	// An empty regex matches every value, including the "" of an absent
	// property, so it would assert nothing at all.
	_, err := newAssertPlatformPropertyOp(&pb.AssertPlatformProperty{Property: "ContainerBaseImage"})
	require.Error(t, err)
}

func TestMapPlatformProperty(t *testing.T) {
	op, err := newMapPlatformPropertyOp(&pb.MapPlatformProperty{
		Property:     "ContainerBaseImage",
		Replacements: map[string]string{"docker://alias": "docker://real@" + testSha},
	})
	require.NoError(t, err)
	s := newState(nil, "", []*remoteexecution.Platform_Property{prop("ContainerBaseImage", "docker://alias")}, nil)
	require.NoError(t, op.apply(s))
	require.True(t, s.platformChanged)
	require.Equal(t, "docker://real@"+testSha, platformView{s.action.Platform}.Get("ContainerBaseImage"))
}

func TestEditPlatformProperty(t *testing.T) {
	op, err := newEditPlatformPropertyOp(&pb.EditPlatformProperty{
		Remove: []string{"ContainerBaseImage", "requires-network", "Flavor", "Version"},
		Add: []*pb.PlatformPropertyEntry{
			{Name: "Flavor", Value: "chroot"},
			{Name: "Version", Value: "generic"},
		},
	})
	require.NoError(t, err)
	s := newState(nil, "", []*remoteexecution.Platform_Property{
		prop("ContainerBaseImage", "docker://busybox@"+testSha),
		prop("requires-network", "false"),
		prop("OSFamily", "linux"),
		prop("Flavor", "old"),
	}, nil)
	require.NoError(t, op.apply(s))
	sortPlatform(s.action.Platform)
	require.Equal(t, []*remoteexecution.Platform_Property{
		prop("Flavor", "chroot"),
		prop("OSFamily", "linux"),
		prop("Version", "generic"),
	}, s.action.Platform.Properties)
}

func TestEditEnvironment(t *testing.T) {
	op, err := newEditEnvironmentOp(&pb.EditEnvironment{
		Remove: []string{"DROP"},
		Set:    map[string]string{"PATH": "/usr/bin:/bin"},
	})
	require.NoError(t, err)
	s := newState(nil, "", nil, []*remoteexecution.Command_EnvironmentVariable{
		{Name: "DROP", Value: "x"},
		{Name: "PATH", Value: "old"},
		{Name: "KEEP", Value: "y"},
	})
	require.NoError(t, op.apply(s))
	require.True(t, s.commandChanged)
	sortEnvironment(s.command)
	require.Equal(t, []*remoteexecution.Command_EnvironmentVariable{
		{Name: "KEEP", Value: "y"},
		{Name: "PATH", Value: "/usr/bin:/bin"},
	}, s.command.EnvironmentVariables)
}

func TestEditEnvironmentRejectsEmptyName(t *testing.T) {
	_, err := newEditEnvironmentOp(&pb.EditEnvironment{Set: map[string]string{"": "x"}})
	require.Error(t, err)
}

func TestNewPipelineRejectsBadTemplate(t *testing.T) {
	_, err := NewPipeline(&pb.ApplicationConfiguration{
		Pipeline: &pb.ActionPipeline{
			Operations: []*pb.Operation{
				{Kind: &pb.Operation_EditCommand{EditCommand: &pb.EditCommand{PrependArguments: []string{"{{.Nope"}}}},
			},
		},
	}, nil, nil)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "template"))
}

func TestNewPipelineRequiresPipeline(t *testing.T) {
	_, err := NewPipeline(&pb.ApplicationConfiguration{}, nil, nil)
	require.Error(t, err)
}

func TestTemplateRequestMetadata(t *testing.T) {
	op, err := newEditEnvironmentOp(&pb.EditEnvironment{Set: map[string]string{
		"BUILD_INVOCATION_ID": "{{.InvocationID}}",
		"BUILD_TARGET":        "{{.TargetLabel}}",
	}})
	require.NoError(t, err)
	s := newState(nil, "", nil, nil)
	s.requestMetadata = &remoteexecution.RequestMetadata{
		ToolInvocationId: "inv-123",
		TargetId:         "//foo:bar",
	}
	require.NoError(t, op.apply(s))
	sortEnvironment(s.command)
	require.Equal(t, []*remoteexecution.Command_EnvironmentVariable{
		{Name: "BUILD_INVOCATION_ID", Value: "inv-123"},
		{Name: "BUILD_TARGET", Value: "//foo:bar"},
	}, s.command.EnvironmentVariables)
}

func TestTemplateRequestMetadataNilSafe(t *testing.T) {
	op, err := newEditEnvironmentOp(&pb.EditEnvironment{Set: map[string]string{"X": "{{.InvocationID}}"}})
	require.NoError(t, err)
	s := newState(nil, "", nil, nil) // requestMetadata is nil
	require.NoError(t, op.apply(s))
	require.Equal(t, "", s.command.EnvironmentVariables[0].Value)
}
