package actionrouter

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"text/template"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	pb "github.com/buildbarn/bb-action-router/pkg/proto/configuration/bb_docker_action_router"
	bb_blobstore "github.com/buildbarn/bb-storage/pkg/blobstore"
	"github.com/buildbarn/bb-storage/pkg/blobstore/buffer"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/buildbarn/bb-storage/pkg/util"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// BazelInputRootDirectoryName is the directory inside a merged input root where
// the action's original input root is nested (see MergeDockerRoot).
// Docker images must not contain a top-level entry with this name.
const BazelInputRootDirectoryName = "bazel_exec_root"

// templateFuncs are exposed to every template string in the pipeline. The
// prefix/suffix argument comes first so the value can be piped in, e.g.
// `{{.Platform.Get "ContainerBaseImage" | trimPrefix "docker://"}}`.
var templateFuncs = template.FuncMap{
	"trimPrefix": func(prefix, s string) string { return strings.TrimPrefix(s, prefix) },
	"trimSuffix": func(suffix, s string) string { return strings.TrimSuffix(s, suffix) },
}

func parseTemplate(name, text string) (*template.Template, error) {
	t, err := template.New(name).Funcs(templateFuncs).Parse(text)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid %s template %q: %v", name, text, err)
	}
	return t, nil
}

// operation is a single transformation applied to an action. Operations mutate
// the shared pipelineState in place.
type operation interface {
	apply(s *pipelineState) error
}

// Pipeline applies an ordered list of operations to each action.
type Pipeline struct {
	cas                     bb_blobstore.BlobAccess
	maximumMessageSizeBytes int
	condition               *template.Template // nil => always run
	operations              []operation
}

// pipelineState is the mutable working state for one action. The command is
// loaded from the CAS lazily so that actions filtered out by the condition
// (e.g. those without a ContainerBaseImage property) never incur a fetch.
type pipelineState struct {
	ctx            context.Context
	digestFunction digest.Function
	cas            bb_blobstore.BlobAccess
	maxMessageSize int

	action          *remoteexecution.Action
	command         *remoteexecution.Command
	requestMetadata *remoteexecution.RequestMetadata

	commandLoaded   bool
	commandErr      error
	commandChanged  bool
	platformChanged bool
}

func (s *pipelineState) getCommand() (*remoteexecution.Command, error) {
	if !s.commandLoaded {
		s.command, s.commandErr = loadCommand(s.ctx, s.cas, s.maxMessageSize, s.action, s.digestFunction)
		s.commandLoaded = true
	}
	return s.command, s.commandErr
}

// templateData is the context every template string renders against. Field
// access reflects the live (possibly already-mutated) action, so later
// operations see the effects of earlier ones.
type templateData struct {
	state *pipelineState
}

func (d templateData) Platform() platformView {
	return platformView{d.state.action.GetPlatform()}
}

func (d templateData) WorkingDirectory() (string, error) {
	command, err := d.state.getCommand()
	if err != nil {
		return "", err
	}
	return command.GetWorkingDirectory(), nil
}

// The following expose REv2 RequestMetadata. They are nil-safe and return ""
// when the client/scheduler didn't supply the metadata. Injecting these into
// the command/environment is a stamp-style use: the action router keys the
// action cache on the original (pre-rewrite) action, so this does not cause
// cache misses.
func (d templateData) InvocationID() string {
	return d.state.requestMetadata.GetToolInvocationId()
}

func (d templateData) TargetLabel() string {
	return d.state.requestMetadata.GetTargetId()
}

func (d templateData) ActionMnemonic() string {
	return d.state.requestMetadata.GetActionMnemonic()
}

func (d templateData) ConfigurationID() string {
	return d.state.requestMetadata.GetConfigurationId()
}

func (d templateData) CorrelatedInvocationsID() string {
	return d.state.requestMetadata.GetCorrelatedInvocationsId()
}

type platformView struct {
	platform *remoteexecution.Platform
}

// Get returns the value of the first property with the given name, or "".
func (v platformView) Get(name string) string {
	for _, property := range v.platform.GetProperties() {
		if property.GetName() == name {
			return property.GetValue()
		}
	}
	return ""
}

func render(t *template.Template, s *pipelineState) (string, error) {
	var b strings.Builder
	if err := t.Execute(&b, templateData{state: s}); err != nil {
		return "", err
	}
	return b.String(), nil
}

// truthy reports whether a rendered condition string counts as "run the
// pipeline": any non-empty value other than "false"/"0" (case-insensitive).
func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "false", "0":
		return false
	}
	return true
}

// HandleAction runs the action through the pipeline, returning it unchanged if
// the condition doesn't match.
func (p *Pipeline) HandleAction(ctx context.Context, action *remoteexecution.Action, requestMetadata *remoteexecution.RequestMetadata, digestFunction digest.Function) (*remoteexecution.Action, error) {
	s := &pipelineState{
		ctx:             ctx,
		digestFunction:  digestFunction,
		cas:             p.cas,
		maxMessageSize:  p.maximumMessageSizeBytes,
		action:          action,
		requestMetadata: requestMetadata,
	}

	if p.condition != nil {
		out, err := render(p.condition, s)
		if err != nil {
			return nil, util.StatusWrap(err, "Failed to evaluate pipeline condition")
		}
		if !truthy(out) {
			return action, nil
		}
	}

	for _, op := range p.operations {
		if err := op.apply(s); err != nil {
			return nil, err
		}
	}

	if s.platformChanged {
		sortPlatform(action.Platform)
	}
	if s.commandChanged {
		command, err := s.getCommand()
		if err != nil {
			return nil, err
		}
		sortEnvironment(command)
		commandDigest, err := putCommand(ctx, p.cas, command, digestFunction)
		if err != nil {
			return nil, err
		}
		action.CommandDigest = commandDigest.GetProto()
	}

	return action, nil
}

// sortPlatform orders properties by (name, value), as REv2 requires.
func sortPlatform(platform *remoteexecution.Platform) {
	if platform == nil {
		return
	}
	slices.SortFunc(platform.Properties, func(a, b *remoteexecution.Platform_Property) int {
		if c := cmp.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return cmp.Compare(a.Value, b.Value)
	})
}

// sortEnvironment orders environment variables by name, as REv2 requires.
func sortEnvironment(command *remoteexecution.Command) {
	slices.SortFunc(command.EnvironmentVariables, func(a, b *remoteexecution.Command_EnvironmentVariable) int {
		return cmp.Compare(a.Name, b.Name)
	})
}

func loadCommand(ctx context.Context, cas bb_blobstore.BlobAccess, maxMessageSize int, action *remoteexecution.Action, digestFunction digest.Function) (*remoteexecution.Command, error) {
	commandDigest, err := digestFunction.NewDigestFromProto(action.CommandDigest)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Failed to load Command digest: %v", err)
	}
	m, err := cas.Get(ctx, commandDigest).ToProto(&remoteexecution.Command{}, maxMessageSize)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to fetch Command: %v", err)
	}
	return m.(*remoteexecution.Command), nil
}

func putCommand(ctx context.Context, cas bb_blobstore.BlobAccess, command *remoteexecution.Command, digestFunction digest.Function) (digest.Digest, error) {
	data, err := proto.Marshal(command)
	if err != nil {
		return digest.Digest{}, status.Errorf(codes.Internal, "Failed to marshal command: %v", err)
	}
	gen := digestFunction.NewGenerator(int64(len(data)))
	if _, err := gen.Write(data); err != nil {
		return digest.Digest{}, status.Errorf(codes.Internal, "Failed to generate digest for command: %v", err)
	}
	commandDigest := gen.Sum()
	if err := cas.Put(ctx, commandDigest, buffer.NewCASBufferFromByteSlice(commandDigest, data, buffer.UserProvided)); err != nil {
		return digest.Digest{}, status.Errorf(codes.Internal, "Failed to store Command to CAS: %v", err)
	}
	return commandDigest, nil
}

func loadDirectory(ctx context.Context, cas bb_blobstore.BlobAccess, maxMessageSize int, digestProto *remoteexecution.Digest, digestFunction digest.Function) (*remoteexecution.Directory, error) {
	parsed, err := digestFunction.NewDigestFromProto(digestProto)
	if err != nil {
		return nil, util.StatusWrap(err, "Failed to parse directory digest")
	}
	data, err := cas.Get(ctx, parsed).ToByteSlice(maxMessageSize)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to fetch directory %s from CAS: %v", digestProto, err)
	}
	directory := &remoteexecution.Directory{}
	if err := proto.Unmarshal(data, directory); err != nil {
		return nil, util.StatusWrap(err, "Failed to unmarshal directory")
	}
	return directory, nil
}

func putDirectory(ctx context.Context, cas bb_blobstore.BlobAccess, directory *remoteexecution.Directory, digestFunction digest.Function) (digest.Digest, error) {
	data, err := proto.Marshal(directory)
	if err != nil {
		return digest.Digest{}, status.Errorf(codes.Internal, "Failed to marshal directory: %v", err)
	}
	gen := digestFunction.NewGenerator(int64(len(data)))
	if _, err := gen.Write(data); err != nil {
		return digest.Digest{}, status.Errorf(codes.Internal, "Failed to generate digest for directory: %v", err)
	}
	directoryDigest := gen.Sum()
	if err := cas.Put(ctx, directoryDigest, buffer.NewCASBufferFromByteSlice(directoryDigest, data, buffer.UserProvided)); err != nil {
		return digest.Digest{}, status.Errorf(codes.Internal, "Failed to store directory %s to CAS: %v", directoryDigest, err)
	}
	return directoryDigest, nil
}

// NewPipeline builds a Pipeline from configuration. Templates and regexes are
// compiled up front, so a malformed config fails at startup rather than
// per-action.
func NewPipeline(config *pb.ApplicationConfiguration, cas, actionCache bb_blobstore.BlobAccess) (*Pipeline, error) {
	pipelineConfig := config.GetPipeline()
	if pipelineConfig == nil {
		return nil, status.Error(codes.InvalidArgument, "pipeline must be set")
	}
	p := &Pipeline{
		cas:                     cas,
		maximumMessageSizeBytes: int(config.GetMaximumMessageSizeBytes()),
	}
	if c := pipelineConfig.GetCondition(); c != "" {
		condition, err := parseTemplate("condition", c)
		if err != nil {
			return nil, err
		}
		p.condition = condition
	}
	for i, opConfig := range pipelineConfig.GetOperations() {
		op, err := buildOperation(opConfig, cas, actionCache, p.maximumMessageSizeBytes)
		if err != nil {
			return nil, util.StatusWrapf(err, "Operation %d", i)
		}
		p.operations = append(p.operations, op)
	}
	return p, nil
}

func buildOperation(config *pb.Operation, cas, actionCache bb_blobstore.BlobAccess, maxMessageSize int) (operation, error) {
	switch kind := config.Kind.(type) {
	case *pb.Operation_AssertPlatformProperty:
		return newAssertPlatformPropertyOp(kind.AssertPlatformProperty)
	case *pb.Operation_MapPlatformProperty:
		return newMapPlatformPropertyOp(kind.MapPlatformProperty)
	case *pb.Operation_EditPlatformProperty:
		return newEditPlatformPropertyOp(kind.EditPlatformProperty)
	case *pb.Operation_EditCommand:
		return newEditCommandOp(kind.EditCommand)
	case *pb.Operation_EditEnvironment:
		return newEditEnvironmentOp(kind.EditEnvironment)
	case *pb.Operation_MergeDockerRoot:
		return newMergeDockerRootOp(kind.MergeDockerRoot, cas, actionCache, maxMessageSize)
	case nil:
		return nil, status.Error(codes.InvalidArgument, "operation kind must be set")
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unknown operation kind %T", kind)
	}
}
