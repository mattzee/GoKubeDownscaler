package scalable

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/caas-team/gokubedownscaler/internal/pkg/definitions"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// definitionRegistry indexes the loaded workload definitions. It is built once at
// startup and never mutated afterwards; RegisterDefinitions swaps in a new one.
type definitionRegistry struct {
	byResource  map[string]*definitions.Definition
	byGroupKind map[schema.GroupKind]*definitions.Definition
}

// registeredDefinitions is the process-wide definition registry. It is a global
// because the lookups it serves (GetWorkloads, ParseWorkloadFromRawObject and the
// owner-reference exclusion in FilterExcluded) are package functions with no
// shared receiver, and threading it through all of them would change every
// caller in both binaries. It is written once at startup, before the first scan.
//
//nolint:gochecknoglobals // written once at startup, read-only afterwards
var registeredDefinitions atomic.Pointer[definitionRegistry]

// RegisterDefinitions replaces the registered workload definitions. It fails if a
// definition's resource name shadows a built-in resource.
func RegisterDefinitions(defs []definitions.Definition) error {
	builtins := builtinResourceFuncs()
	registry := &definitionRegistry{
		byResource:  make(map[string]*definitions.Definition, len(defs)),
		byGroupKind: make(map[schema.GroupKind]*definitions.Definition, len(defs)),
	}

	for i := range defs {
		def := &defs[i]

		if _, builtin := builtins[def.Resource]; builtin {
			return fmt.Errorf("%w: resource %q is a built-in resource", definitions.ErrInvalidDefinition, def.Resource)
		}

		registry.byResource[def.Resource] = def
		registry.byGroupKind[schema.GroupKind{Group: def.Group, Kind: def.Kind}] = def
	}

	registeredDefinitions.Store(registry)

	return nil
}

// ErrUnsupportedResource is returned when an included resource is neither built in nor defined.
var ErrUnsupportedResource = errors.New("unsupported resource")

// InitDefinitions loads and registers the workload definitions from path, then
// checks that every included resource is built in or defined. It runs once at
// startup; an error should stop the process so a bad configuration fails the
// rollout instead of silently dropping resources.
func InitDefinitions(path string, includeResources []string) error {
	defs, err := definitions.Load(path)
	if err != nil {
		return fmt.Errorf("failed to load workload definitions: %w", err)
	}

	if err = RegisterDefinitions(defs); err != nil {
		return fmt.Errorf("failed to register workload definitions: %w", err)
	}

	slog.Info("loaded workload definitions", "file", path, "resources", RegisteredResources())

	var unsupported []string

	for _, resource := range includeResources {
		if !IsSupportedResource(strings.ToLower(resource)) {
			unsupported = append(unsupported, resource)
		}
	}

	if len(unsupported) > 0 {
		return fmt.Errorf("%w: %s is neither built in nor defined in %s",
			ErrUnsupportedResource, strings.Join(unsupported, ", "), path)
	}

	return nil
}

// IsSupportedResource reports whether resource is a built-in or registered resource name.
func IsSupportedResource(resource string) bool {
	if _, builtin := builtinResourceFuncs()[resource]; builtin {
		return true
	}

	return definitionByResource(resource) != nil
}

// RegisteredResources returns the sorted resource names of all registered definitions.
func RegisteredResources() []string {
	registry := registeredDefinitions.Load()
	if registry == nil {
		return nil
	}

	names := make([]string, 0, len(registry.byResource))
	for name := range registry.byResource {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

func definitionByResource(resource string) *definitions.Definition {
	registry := registeredDefinitions.Load()
	if registry == nil {
		return nil
	}

	return registry.byResource[resource]
}

func definitionByGroupKind(group, kind string) *definitions.Definition {
	registry := registeredDefinitions.Load()
	if registry == nil {
		return nil
	}

	return registry.byGroupKind[schema.GroupKind{Group: group, Kind: kind}]
}
