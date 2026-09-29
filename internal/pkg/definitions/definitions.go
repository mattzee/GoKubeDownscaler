// Package definitions loads and validates workload definitions: operator custom
// resources described in configuration instead of a hand-written scaler. One
// generic scaler (see scalable.definedResource) drives every definition.
package definitions

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

const (
	// MatchOwnerReference matches children that carry an ownerReference (kind and name) back to the parent.
	MatchOwnerReference = "ownerReference"
	// MatchLabelEqualsName matches children whose Label equals the parent's name.
	MatchLabelEqualsName = "labelEqualsName"

	childKindStatefulSet = "StatefulSet"
	childKindDeployment  = "Deployment"
)

// Definition describes one operator custom resource the downscaler can scale.
type Definition struct {
	// Resource is the name used in --include-resources.
	Resource string `json:"resource"`
	// Plural is the CRD's plural resource name, used for RBAC and admission rules. Defaults to Resource.
	Plural  string `json:"plural,omitempty"`
	Group   string `json:"group"`
	Version string `json:"version"`
	Kind    string `json:"kind"`
	// Replicas makes the resource replica-shaped. Exactly one of Replicas and Suspend is set.
	Replicas *Replicas `json:"replicas,omitempty"`
	// Suspend makes the resource suspend-shaped. Exactly one of Replicas and Suspend is set.
	Suspend *Suspend `json:"suspend,omitempty"`
	// ParkGuard refuses to park (scale to 0) unless the guard's conditions hold. Replica-shaped only.
	ParkGuard *ParkGuard `json:"parkGuard,omitempty"`
	// Children are workloads the parent owns that are scaled with it when scale-children is enabled.
	Children []Child `json:"children,omitempty"`
	// Savings describes where the pod resource requests live, for the saved-resources metric.
	Savings *Savings `json:"savings,omitempty"`
}

// Replicas describes a replica-shaped resource.
type Replicas struct {
	// Path is the dot-separated path to the integer replica field.
	Path string `json:"path"`
	// Minimum clamps any downscale target below it. Default 0.
	Minimum int32 `json:"minimum,omitempty"`
}

// Suspend describes a suspend-shaped resource. Exactly one of Path and Annotation is set.
type Suspend struct {
	Path           string `json:"path,omitempty"`
	Annotation     string `json:"annotation,omitempty"`
	SuspendedValue string `json:"suspendedValue"`
	ResumedValue   string `json:"resumedValue"`
}

// ParkGuard refuses a park when any of RequireAnnotations is missing.
type ParkGuard struct {
	RequireAnnotations []string `json:"requireAnnotations"`
}

// Child describes how to find workloads owned by the parent.
type Child struct {
	Kind  string `json:"kind"`
	Match string `json:"match"`
	Label string `json:"label,omitempty"`
	// Always scales the child with the parent even when scale-children is off. Set it
	// for children that actually stop the pods, where parking the parent alone does not.
	Always bool `json:"always,omitempty"`
}

// Savings describes where pod resource requests live. All paths are dot-separated.
//
// Per-pod requests come from RequestsPath (a requests map) or ContainersPath (a
// container list whose resources.requests are summed). Replica-shaped resources
// multiply the per-pod requests by the change in replicas. Suspend-shaped
// resources multiply them by CountPath (default 1); with GroupsPath, the other
// paths are relative to each item of that list and the results are summed.
type Savings struct {
	GroupsPath     string `json:"groupsPath,omitempty"`
	CountPath      string `json:"countPath,omitempty"`
	RequestsPath   string `json:"requestsPath,omitempty"`
	ContainersPath string `json:"containersPath,omitempty"`
}

// GetPlural returns the CRD plural, defaulting to Resource.
func (d *Definition) GetPlural() string {
	if d.Plural != "" {
		return d.Plural
	}

	return d.Resource
}

// ErrInvalidDefinition wraps every validation failure.
var ErrInvalidDefinition = errors.New("invalid workload definition")

var (
	pathPattern     = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`)
	resourcePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
)

// Load reads definitions from path. A missing file is not an error and yields no
// definitions. Unknown fields and invalid definitions are errors.
func Load(path string) ([]Definition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("failed to read workload definitions file %q: %w", path, err)
	}

	return Parse(data)
}

// Parse strictly unmarshals and validates definitions.
func Parse(data []byte) ([]Definition, error) {
	var defs []Definition
	if err := yaml.UnmarshalStrict(data, &defs); err != nil {
		return nil, fmt.Errorf("failed to parse workload definitions: %w", err)
	}

	if err := Validate(defs); err != nil {
		return nil, err
	}

	return defs, nil
}

// Validate checks every definition and that resource names and group+kinds are unique.
func Validate(defs []Definition) error {
	resources := map[string]struct{}{}
	groupKinds := map[string]struct{}{}

	var errs []error

	for i := range defs {
		def := &defs[i]

		if err := def.validate(); err != nil {
			errs = append(errs, fmt.Errorf("definition %d (%q): %w", i, def.Resource, err))
			continue
		}

		if _, dup := resources[def.Resource]; dup {
			errs = append(errs, fmt.Errorf("%w: duplicate resource %q", ErrInvalidDefinition, def.Resource))
		}

		resources[def.Resource] = struct{}{}

		groupKind := def.Group + "/" + def.Kind
		if _, dup := groupKinds[groupKind]; dup {
			errs = append(errs, fmt.Errorf("%w: duplicate group and kind %q", ErrInvalidDefinition, groupKind))
		}

		groupKinds[groupKind] = struct{}{}
	}

	return errors.Join(errs...)
}

func (d *Definition) validate() error {
	var errs []error

	invalid := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%w: %s", ErrInvalidDefinition, fmt.Sprintf(format, args...)))
	}

	if !resourcePattern.MatchString(d.Resource) {
		invalid("resource %q must be a lowercase name", d.Resource)
	}

	if d.Plural != "" && !resourcePattern.MatchString(d.Plural) {
		invalid("plural %q must be a lowercase name", d.Plural)
	}

	if d.Group == "" || d.Version == "" || d.Kind == "" {
		invalid("group, version and kind are required")
	}

	d.validateShape(invalid)

	if d.ParkGuard != nil {
		d.ParkGuard.validate(invalid, d.Replicas != nil)
	}

	for i, child := range d.Children {
		child.validate(invalid, i)
	}

	if d.Savings != nil {
		d.Savings.validate(invalid, d.Suspend != nil)
	}

	return errors.Join(errs...)
}

func (d *Definition) validateShape(invalid func(string, ...any)) {
	switch {
	case d.Replicas == nil && d.Suspend == nil:
		invalid("one of replicas or suspend is required")
	case d.Replicas != nil && d.Suspend != nil:
		invalid("only one of replicas or suspend may be set")
	case d.Replicas != nil:
		checkPath(invalid, "replicas.path", d.Replicas.Path, true)

		if d.Replicas.Minimum < 0 {
			invalid("replicas.minimum must not be negative")
		}
	case d.Suspend != nil:
		d.Suspend.validate(invalid)
	}
}

func (p *ParkGuard) validate(invalid func(string, ...any), replicaShaped bool) {
	if !replicaShaped {
		invalid("parkGuard is only supported with replicas")
	}

	if len(p.RequireAnnotations) == 0 {
		invalid("parkGuard.requireAnnotations must not be empty")
	}
}

func (s *Suspend) validate(invalid func(string, ...any)) {
	if (s.Path == "") == (s.Annotation == "") {
		invalid("exactly one of suspend.path or suspend.annotation is required")
	}

	checkPath(invalid, "suspend.path", s.Path, false)

	if s.SuspendedValue == "" || s.ResumedValue == "" {
		invalid("suspend.suspendedValue and suspend.resumedValue are required")
	}

	if s.SuspendedValue == s.ResumedValue {
		invalid("suspend.suspendedValue and suspend.resumedValue must differ")
	}
}

func (c *Child) validate(invalid func(string, ...any), index int) {
	if c.Kind != childKindStatefulSet && c.Kind != childKindDeployment {
		invalid("children[%d].kind must be %s or %s", index, childKindStatefulSet, childKindDeployment)
	}

	switch c.Match {
	case MatchOwnerReference:
		if c.Label != "" {
			invalid("children[%d].label is only used with match %s", index, MatchLabelEqualsName)
		}
	case MatchLabelEqualsName:
		if c.Label == "" {
			invalid("children[%d].label is required with match %s", index, MatchLabelEqualsName)
		}
	default:
		invalid("children[%d].match must be %s or %s", index, MatchOwnerReference, MatchLabelEqualsName)
	}
}

func (s *Savings) validate(invalid func(string, ...any), suspendShaped bool) {
	if (s.RequestsPath == "") == (s.ContainersPath == "") {
		invalid("exactly one of savings.requestsPath or savings.containersPath is required")
	}

	if !suspendShaped && (s.GroupsPath != "" || s.CountPath != "") {
		invalid("savings.groupsPath and savings.countPath are only used with suspend")
	}

	checkPath(invalid, "savings.groupsPath", s.GroupsPath, false)
	checkPath(invalid, "savings.countPath", s.CountPath, false)
	checkPath(invalid, "savings.requestsPath", s.RequestsPath, false)
	checkPath(invalid, "savings.containersPath", s.ContainersPath, false)
}

func checkPath(invalid func(string, ...any), field, path string, required bool) {
	if path == "" {
		if required {
			invalid("%s is required", field)
		}

		return
	}

	if !pathPattern.MatchString(path) {
		invalid("%s %q must be a dot-separated field path", field, path)
	}
}

// SplitPath splits a dot-separated field path.
func SplitPath(path string) []string {
	if path == "" {
		return nil
	}

	return strings.Split(path, ".")
}
