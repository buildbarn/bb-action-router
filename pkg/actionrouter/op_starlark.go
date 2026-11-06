package actionrouter

import (
	"fmt"
	"os"
	"slices"
	"strings"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	pb "github.com/buildbarn/bb-action-router/pkg/proto/configuration/bb_docker_action_router"

	"go.starlark.net/starlark"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// runStarlarkOp runs a Starlark script against each action. The script is
// parsed and its module body executed once at startup (freezing its globals, so
// the resulting `process` function is safe to call concurrently); `process` is
// then invoked once per action with an `action` value exposing the action's
// platform properties, command line, environment and metadata natively.
type runStarlarkOp struct {
	process starlark.Callable
}

// nameValue is a resolved name/value pair, the common shape of a platform
// property and an environment variable when converting to and from Starlark.
type nameValue struct {
	name  string
	value string
}

func newRunStarlarkOp(config *pb.RunStarlark) (operation, error) {
	src, filename, err := starlarkSource(config)
	if err != nil {
		return nil, err
	}
	thread := &starlark.Thread{Name: "run_starlark-init"}
	globals, err := starlark.ExecFile(thread, filename, src, starlarkPredeclared)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Failed to load %s: %v", filename, err)
	}
	value, ok := globals["process"]
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "run_starlark script must define a function named \"process\"")
	}
	process, ok := value.(starlark.Callable)
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "run_starlark \"process\" must be a function, got %s", value.Type())
	}
	return &runStarlarkOp{process: process}, nil
}

// starlarkSource resolves the script to run from either the inline `script` or
// the `script_file` path (the config's oneof guarantees at most one is set),
// returning its source and a filename used in Starlark error messages and
// tracebacks.
func starlarkSource(config *pb.RunStarlark) (string, string, error) {
	switch source := config.GetSource().(type) {
	case *pb.RunStarlark_Script:
		return source.Script, "run_starlark.star", nil
	case *pb.RunStarlark_ScriptFile:
		src, err := os.ReadFile(source.ScriptFile)
		if err != nil {
			return "", "", status.Errorf(codes.InvalidArgument, "run_starlark: failed to read script_file %q: %v", source.ScriptFile, err)
		}
		return string(src), source.ScriptFile, nil
	default:
		return "", "", status.Error(codes.InvalidArgument, "run_starlark: script or script_file must be set")
	}
}

// threadLocalAction is the key under which the current *starlarkAction is stored
// on the Starlark thread, so the fail builtin can reach it.
const threadLocalAction = "action"

// starlarkPredeclared is the environment available to every script. It replaces
// the standard fail builtin with one that tags the failure as an explicit,
// script-initiated rejection (see failBuiltin), distinguishing it from runtime
// errors (type errors, missing keys, ...) which indicate a buggy script.
var starlarkPredeclared = starlark.StringDict{
	"fail": starlark.NewBuiltin("fail", failBuiltin),
}

// failBuiltin mimics Starlark's fail(*args, sep=" ") but records the message as
// a policy rejection on the current action, so apply can map it to
// InvalidArgument rather than Internal.
func failBuiltin(thread *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	sep := " "
	if err := starlark.UnpackArgs("fail", nil, kwargs, "sep?", &sep); err != nil {
		return nil, err
	}
	var buf strings.Builder
	for i, v := range args {
		if i > 0 {
			buf.WriteString(sep)
		}
		if s, ok := starlark.AsString(v); ok {
			buf.WriteString(s)
		} else {
			buf.WriteString(v.String())
		}
	}
	err := status.Error(codes.InvalidArgument, buf.String())
	if av, ok := thread.Local(threadLocalAction).(*starlarkAction); ok {
		av.rejected = err
	}
	return nil, err
}

func (o *runStarlarkOp) apply(s *pipelineState) error {
	av := &starlarkAction{state: s}
	thread := &starlark.Thread{Name: "run_starlark"}
	thread.SetLocal(threadLocalAction, av)
	_, err := starlark.Call(thread, o.process, starlark.Tuple{av}, nil)
	switch {
	case av.rejected != nil:
		// The script called fail(...): a deliberate policy rejection of this
		// action. InvalidArgument tells the client the action was refused.
		return av.rejected
	case av.err != nil:
		// An accessor/mutator hit a server/script fault (CAS fetch, a bad
		// assignment). It is already coded Internal.
		return av.err
	case err != nil:
		// Any other Starlark error (syntax is caught at startup; this is a
		// runtime bug: type error, missing key, ...). The script, not the
		// action, is at fault, so this is Internal rather than InvalidArgument.
		return status.Errorf(codes.Internal, "run_starlark.script failed: %v", err)
	}
	// Only on success: write the script's edits back into the action.
	return av.syncBack()
}

// starlarkAction is the `action` value passed to the script's process(action).
// The platform, environment and arguments are exposed as ordinary Starlark
// dict/list values, built from the action on first access and cached here; after
// process() returns successfully, syncBack writes any that changed back into the
// action. Scalars (working_directory, metadata) are read/written directly.
//
//	action.platform              dict of platform properties (str->str)
//	action.environment           dict of environment variables (str->str)
//	action.arguments             list of command arguments
//	action.working_directory     command working directory (read + assign)
//	action.invocation_id etc.    read-only RequestMetadata accessors
//
// A script that calls fail(...) rejects the action with InvalidArgument, which
// is how validation (e.g. requiring a pinned image digest) is expressed. Any
// other runtime error is a bug in the script and surfaces as Internal.
type starlarkAction struct {
	state *pipelineState
	// rejected holds the error from an explicit fail(...) call, mapped to
	// InvalidArgument: a deliberate rejection of the action.
	rejected error
	// err holds the first server/script fault from an accessor or mutator (CAS
	// fetch, a bad assignment), coded Internal.
	err error

	// Cached Starlark views, created on first access and synced back by
	// syncBack. A nil pointer means the script never touched that field, so it
	// is left untouched on the action.
	platform    *starlark.Dict
	environment *starlark.Dict
	arguments   *starlark.List
}

var (
	_ starlark.HasAttrs    = (*starlarkAction)(nil)
	_ starlark.HasSetField = (*starlarkAction)(nil)
)

func (*starlarkAction) String() string        { return "action" }
func (*starlarkAction) Type() string          { return "action" }
func (*starlarkAction) Freeze()               {}
func (*starlarkAction) Truth() starlark.Bool  { return starlark.True }
func (*starlarkAction) Hash() (uint32, error) { return 0, fmt.Errorf("action is not hashable") }

var actionAttrNames = []string{
	"action_mnemonic",
	"arguments",
	"configuration_id",
	"correlated_invocations_id",
	"environment",
	"invocation_id",
	"platform",
	"target_label",
	"working_directory",
}

func (*starlarkAction) AttrNames() []string { return actionAttrNames }

func (a *starlarkAction) Attr(name string) (starlark.Value, error) {
	switch name {
	case "platform":
		if a.platform == nil {
			a.platform = dictFromNameValues(platformItems(a.state.action.GetPlatform()))
		}
		return a.platform, nil
	case "environment":
		command, err := a.state.getCommand()
		if err != nil {
			return nil, a.record(err)
		}
		if a.environment == nil {
			a.environment = dictFromNameValues(environmentItems(command))
		}
		return a.environment, nil
	case "arguments":
		command, err := a.state.getCommand()
		if err != nil {
			return nil, a.record(err)
		}
		if a.arguments == nil {
			a.arguments = listFromStrings(command.Arguments)
		}
		return a.arguments, nil
	case "working_directory":
		command, err := a.state.getCommand()
		if err != nil {
			return nil, a.record(err)
		}
		return starlark.String(command.GetWorkingDirectory()), nil
	case "invocation_id":
		return starlark.String(a.state.requestMetadata.GetToolInvocationId()), nil
	case "target_label":
		return starlark.String(a.state.requestMetadata.GetTargetId()), nil
	case "action_mnemonic":
		return starlark.String(a.state.requestMetadata.GetActionMnemonic()), nil
	case "configuration_id":
		return starlark.String(a.state.requestMetadata.GetConfigurationId()), nil
	case "correlated_invocations_id":
		return starlark.String(a.state.requestMetadata.GetCorrelatedInvocationsId()), nil
	default:
		// (nil, nil) signals "no such attribute" to Starlark.
		return nil, nil
	}
}

func (a *starlarkAction) SetField(name string, val starlark.Value) error {
	switch name {
	case "platform":
		d, ok := val.(*starlark.Dict)
		if !ok {
			return a.record(status.Errorf(codes.Internal, "action.platform must be assigned a dict, got %s", val.Type()))
		}
		a.platform = d
		return nil
	case "environment":
		d, ok := val.(*starlark.Dict)
		if !ok {
			return a.record(status.Errorf(codes.Internal, "action.environment must be assigned a dict, got %s", val.Type()))
		}
		if _, err := a.state.getCommand(); err != nil {
			return a.record(err)
		}
		a.environment = d
		return nil
	case "arguments":
		l, ok := val.(*starlark.List)
		if !ok {
			return a.record(status.Errorf(codes.Internal, "action.arguments must be assigned a list, got %s", val.Type()))
		}
		if _, err := a.state.getCommand(); err != nil {
			return a.record(err)
		}
		a.arguments = l
		return nil
	case "working_directory":
		s, ok := starlark.AsString(val)
		if !ok {
			return a.record(status.Errorf(codes.Internal, "action.working_directory must be a string, got %s", val.Type()))
		}
		command, err := a.state.getCommand()
		if err != nil {
			return a.record(err)
		}
		command.WorkingDirectory = s
		a.state.commandChanged = true
		return nil
	default:
		return a.record(status.Errorf(codes.Internal, "can't assign to action.%s", name))
	}
}

// record remembers the first failure so apply can return its grpc status code.
func (a *starlarkAction) record(err error) error {
	if err != nil && a.err == nil {
		a.err = err
	}
	return err
}

// syncBack writes any touched dict/list views back into the action, marking the
// platform/command changed only when the contents actually differ (so a script
// that merely reads a field doesn't force a re-sort or Command re-upload).
func (a *starlarkAction) syncBack() error {
	if a.platform != nil {
		items, err := nameValuesFromDict("platform", a.platform)
		if err != nil {
			return err
		}
		if !sameNameValues(platformItems(a.state.action.GetPlatform()), items) {
			setPlatform(a.state.action, items)
			a.state.platformChanged = true
		}
	}
	if a.environment != nil {
		items, err := nameValuesFromDict("environment", a.environment)
		if err != nil {
			return err
		}
		command, err := a.state.getCommand()
		if err != nil {
			return err
		}
		if !sameNameValues(environmentItems(command), items) {
			setEnvironment(command, items)
			a.state.commandChanged = true
		}
	}
	if a.arguments != nil {
		args, err := stringsFromList("arguments", a.arguments)
		if err != nil {
			return err
		}
		command, err := a.state.getCommand()
		if err != nil {
			return err
		}
		if !slices.Equal(command.Arguments, args) {
			command.Arguments = args
			a.state.commandChanged = true
		}
	}
	return nil
}

// --- proto <-> Starlark conversions ---

func platformItems(platform *remoteexecution.Platform) []nameValue {
	properties := platform.GetProperties()
	out := make([]nameValue, 0, len(properties))
	for _, p := range properties {
		out = append(out, nameValue{name: p.Name, value: p.Value})
	}
	return out
}

func environmentItems(command *remoteexecution.Command) []nameValue {
	out := make([]nameValue, 0, len(command.EnvironmentVariables))
	for _, v := range command.EnvironmentVariables {
		out = append(out, nameValue{name: v.Name, value: v.Value})
	}
	return out
}

func setPlatform(action *remoteexecution.Action, items []nameValue) {
	platform := action.GetPlatform()
	if platform == nil {
		platform = &remoteexecution.Platform{}
		action.Platform = platform
	}
	properties := make([]*remoteexecution.Platform_Property, 0, len(items))
	for _, it := range items {
		properties = append(properties, &remoteexecution.Platform_Property{Name: it.name, Value: it.value})
	}
	platform.Properties = properties
}

func setEnvironment(command *remoteexecution.Command, items []nameValue) {
	variables := make([]*remoteexecution.Command_EnvironmentVariable, 0, len(items))
	for _, it := range items {
		variables = append(variables, &remoteexecution.Command_EnvironmentVariable{Name: it.name, Value: it.value})
	}
	command.EnvironmentVariables = variables
}

func dictFromNameValues(items []nameValue) *starlark.Dict {
	d := starlark.NewDict(len(items))
	for _, it := range items {
		// SetKey can only fail on a frozen dict or unhashable key; neither
		// applies to a fresh dict with string keys.
		_ = d.SetKey(starlark.String(it.name), starlark.String(it.value))
	}
	return d
}

func listFromStrings(values []string) *starlark.List {
	elems := make([]starlark.Value, len(values))
	for i, v := range values {
		elems[i] = starlark.String(v)
	}
	return starlark.NewList(elems)
}

// nameValuesFromDict converts a str->str dict to []nameValue, in iteration
// order. Non-string keys/values are script bugs, reported as Internal.
func nameValuesFromDict(name string, d *starlark.Dict) ([]nameValue, error) {
	items := d.Items()
	out := make([]nameValue, 0, len(items))
	for _, item := range items {
		key, ok := starlark.AsString(item[0])
		if !ok {
			return nil, status.Errorf(codes.Internal, "action.%s keys must be strings, got %s", name, item[0].Type())
		}
		if key == "" {
			return nil, status.Errorf(codes.Internal, "action.%s keys must not be empty", name)
		}
		value, ok := starlark.AsString(item[1])
		if !ok {
			return nil, status.Errorf(codes.Internal, "action.%s value for key %q must be a string, got %s", name, key, item[1].Type())
		}
		out = append(out, nameValue{name: key, value: value})
	}
	return out, nil
}

func stringsFromList(name string, l *starlark.List) ([]string, error) {
	out := make([]string, 0, l.Len())
	for i := 0; i < l.Len(); i++ {
		s, ok := starlark.AsString(l.Index(i))
		if !ok {
			return nil, status.Errorf(codes.Internal, "action.%s entries must be strings, got %s", name, l.Index(i).Type())
		}
		out = append(out, s)
	}
	return out, nil
}

// sameNameValues reports whether two name/value sets are equal as maps (order
// is irrelevant; both platform and environment are re-sorted downstream).
func sameNameValues(a, b []nameValue) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]string, len(a))
	for _, nv := range a {
		m[nv.name] = nv.value
	}
	for _, nv := range b {
		if v, ok := m[nv.name]; !ok || v != nv.value {
			return false
		}
	}
	return true
}
