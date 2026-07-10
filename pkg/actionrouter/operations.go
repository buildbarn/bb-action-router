package actionrouter

import (
	"regexp"
	"text/template"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	pb "github.com/buildbarn/bb-action-router/pkg/proto/configuration/bb_docker_action_router"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// assertPlatformPropertyOp rejects actions whose named property doesn't match a
// regex.
type assertPlatformPropertyOp struct {
	property string
	regex    *regexp.Regexp
}

func newAssertPlatformPropertyOp(config *pb.AssertPlatformProperty) (operation, error) {
	if config.GetProperty() == "" {
		return nil, status.Error(codes.InvalidArgument, "assert_platform_property.property must be set")
	}
	if config.GetRegex() == "" {
		// An empty regex matches everything, including the "" of an absent
		// property, so it would silently assert nothing.
		return nil, status.Error(codes.InvalidArgument, "assert_platform_property.regex must be set")
	}
	regex, err := regexp.Compile(config.GetRegex())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid assert_platform_property.regex %q: %v", config.GetRegex(), err)
	}
	return &assertPlatformPropertyOp{property: config.GetProperty(), regex: regex}, nil
}

func (o *assertPlatformPropertyOp) apply(s *pipelineState) error {
	value := platformView{s.action.GetPlatform()}.Get(o.property)
	if !o.regex.MatchString(value) {
		return status.Errorf(codes.InvalidArgument, "Platform property %q value %q does not match %q", o.property, value, o.regex.String())
	}
	return nil
}

// mapPlatformPropertyOp rewrites a property's value via an exact-match lookup.
type mapPlatformPropertyOp struct {
	property     string
	replacements map[string]string
}

func newMapPlatformPropertyOp(config *pb.MapPlatformProperty) (operation, error) {
	if config.GetProperty() == "" {
		return nil, status.Error(codes.InvalidArgument, "map_platform_property.property must be set")
	}
	return &mapPlatformPropertyOp{property: config.GetProperty(), replacements: config.GetReplacements()}, nil
}

func (o *mapPlatformPropertyOp) apply(s *pipelineState) error {
	for _, property := range s.action.GetPlatform().GetProperties() {
		if property.Name != o.property {
			continue
		}
		if replacement, ok := o.replacements[property.Value]; ok {
			property.Value = replacement
			s.platformChanged = true
		}
	}
	return nil
}

// editPlatformPropertyOp removes and/or adds platform properties.
type editPlatformPropertyOp struct {
	remove map[string]struct{}
	add    []platformPropertyAddition
}

type platformPropertyAddition struct {
	name  string
	value *template.Template
}

func newEditPlatformPropertyOp(config *pb.EditPlatformProperty) (operation, error) {
	op := &editPlatformPropertyOp{remove: map[string]struct{}{}}
	for _, name := range config.GetRemove() {
		op.remove[name] = struct{}{}
	}
	for _, entry := range config.GetAdd() {
		if entry.GetName() == "" {
			return nil, status.Error(codes.InvalidArgument, "edit_platform_property.add.name must be set")
		}
		value, err := parseTemplate("edit_platform_property.add.value", entry.GetValue())
		if err != nil {
			return nil, err
		}
		op.add = append(op.add, platformPropertyAddition{name: entry.GetName(), value: value})
	}
	return op, nil
}

func (o *editPlatformPropertyOp) apply(s *pipelineState) error {
	platform := s.action.GetPlatform()
	if platform == nil {
		platform = &remoteexecution.Platform{}
		s.action.Platform = platform
	}

	if len(o.remove) > 0 {
		kept := platform.Properties[:0]
		for _, property := range platform.Properties {
			if _, removed := o.remove[property.Name]; !removed {
				kept = append(kept, property)
			}
		}
		platform.Properties = kept
	}

	for _, addition := range o.add {
		value, err := render(addition.value, s)
		if err != nil {
			return status.Errorf(codes.Internal, "Failed to render property %q value: %v", addition.name, err)
		}
		platform.Properties = append(platform.Properties, &remoteexecution.Platform_Property{
			Name:  addition.name,
			Value: value,
		})
	}

	s.platformChanged = true
	return nil
}

// editCommandOp wraps the command's argument vector with rendered templates.
type editCommandOp struct {
	prepend []*template.Template
	append  []*template.Template
}

func newEditCommandOp(config *pb.EditCommand) (operation, error) {
	if len(config.GetPrependArguments()) == 0 && len(config.GetAppendArguments()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "edit_command must set prepend_arguments and/or append_arguments")
	}
	op := &editCommandOp{}
	var err error
	if op.prepend, err = parseArgumentTemplates("edit_command.prepend_arguments", config.GetPrependArguments()); err != nil {
		return nil, err
	}
	if op.append, err = parseArgumentTemplates("edit_command.append_arguments", config.GetAppendArguments()); err != nil {
		return nil, err
	}
	return op, nil
}

func parseArgumentTemplates(name string, texts []string) ([]*template.Template, error) {
	templates := make([]*template.Template, 0, len(texts))
	for _, text := range texts {
		t, err := parseTemplate(name, text)
		if err != nil {
			return nil, err
		}
		templates = append(templates, t)
	}
	return templates, nil
}

// renderArguments renders each template to one argv element, dropping those
// that render to the empty string (so `{{if ...}}--flag{{end}}` is optional).
func renderArguments(templates []*template.Template, s *pipelineState) ([]string, error) {
	var args []string
	for _, t := range templates {
		value, err := render(t, s)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "Failed to render command argument: %v", err)
		}
		if value != "" {
			args = append(args, value)
		}
	}
	return args, nil
}

func (o *editCommandOp) apply(s *pipelineState) error {
	command, err := s.getCommand()
	if err != nil {
		return err
	}
	prepend, err := renderArguments(o.prepend, s)
	if err != nil {
		return err
	}
	appendArgs, err := renderArguments(o.append, s)
	if err != nil {
		return err
	}
	arguments := make([]string, 0, len(prepend)+len(command.Arguments)+len(appendArgs))
	arguments = append(arguments, prepend...)
	arguments = append(arguments, command.Arguments...)
	arguments = append(arguments, appendArgs...)
	command.Arguments = arguments
	s.commandChanged = true
	return nil
}

// editEnvironmentOp removes and/or sets environment variables.
type editEnvironmentOp struct {
	remove map[string]struct{}
	set    map[string]*template.Template
}

func newEditEnvironmentOp(config *pb.EditEnvironment) (operation, error) {
	op := &editEnvironmentOp{remove: map[string]struct{}{}, set: map[string]*template.Template{}}
	for _, name := range config.GetRemove() {
		op.remove[name] = struct{}{}
	}
	for name, value := range config.GetSet() {
		if name == "" {
			return nil, status.Error(codes.InvalidArgument, "edit_environment.set keys must not be empty")
		}
		tmpl, err := parseTemplate("edit_environment.set["+name+"]", value)
		if err != nil {
			return nil, err
		}
		op.set[name] = tmpl
	}
	return op, nil
}

func (o *editEnvironmentOp) apply(s *pipelineState) error {
	command, err := s.getCommand()
	if err != nil {
		return err
	}

	// A variable is dropped if it's in remove or being overwritten by set.
	kept := command.EnvironmentVariables[:0]
	for _, variable := range command.EnvironmentVariables {
		_, removed := o.remove[variable.Name]
		_, overwritten := o.set[variable.Name]
		if !removed && !overwritten {
			kept = append(kept, variable)
		}
	}
	command.EnvironmentVariables = kept

	for name, tmpl := range o.set {
		value, err := render(tmpl, s)
		if err != nil {
			return status.Errorf(codes.Internal, "Failed to render environment variable %q: %v", name, err)
		}
		command.EnvironmentVariables = append(command.EnvironmentVariables, &remoteexecution.Command_EnvironmentVariable{
			Name:  name,
			Value: value,
		})
	}

	s.commandChanged = true
	return nil
}
