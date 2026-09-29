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
	// included holds the resource names of definitions that are in includedResources.
	included map[string]struct{}
}

const (
	appsGroup       = "apps"
	kruiseAppsGroup = "apps.kruise.io"
)

// builtinGroupKinds are the group/kinds the built-in scalers and webhook parsers
// handle. A definition may not claim one, since definitions are checked first and
// would silently take the kind over.
//
//nolint:gochecknoglobals // read-only lookup table
var builtinGroupKinds = map[schema.GroupKind]struct{}{
	{Group: appsGroup, Kind: deploymentKind}:                      {},
	{Group: appsGroup, Kind: statefulSetKind}:                     {},
	{Group: appsGroup, Kind: daemonSetKind}:                       {},
	{Group: "batch", Kind: jobKind}:                               {},
	{Group: "batch", Kind: cronJobKind}:                           {},
	{Group: "autoscaling", Kind: horizontalPodAutoscalerKind}:     {},
	{Group: "policy", Kind: podDisruptionBudgetKind}:              {},
	{Group: "", Kind: serviceKind}:                                {},
	{Group: "networking.k8s.io", Kind: ingressKind}:               {},
	{Group: "gateway.networking.k8s.io", Kind: gatewayKind}:       {},
	{Group: "keda.sh", Kind: scaledObjectKind}:                    {},
	{Group: "argoproj.io", Kind: rolloutKind}:                     {},
	{Group: "zalando.org", Kind: stackKind}:                       {},
	{Group: "monitoring.coreos.com", Kind: prometheusKind}:        {},
	{Group: "actions.github.com", Kind: autoscalingRunnerSetKind}: {},
	{Group: "acid.zalan.do", Kind: postgresqlKind}:                {},
	{Group: kafkaStrimziGroup, Kind: kafkaConnectKind}:            {},
	{Group: kafkaStrimziGroup, Kind: kafkaMirrorMaker2Kind}:       {},
	{Group: kafkaStrimziGroup, Kind: kafkaBridgeKind}:             {},
	{Group: kruiseAppsGroup, Kind: statefulSetKind}:               {},
	{Group: kruiseAppsGroup, Kind: daemonSetKind}:                 {},
	{Group: kruiseAppsGroup, Kind: cloneSetKind}:                  {},
	{Group: kruiseAppsGroup, Kind: advancedCronJobKind}:           {},
	{Group: kruiseAppsGroup, Kind: broadcastJobKind}:              {},
	{Group: kruiseAppsGroup, Kind: "ImagePullJob"}:                {},
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
// definition's resource name or group/kind shadows a built-in resource.
// includeResources marks which definitions are in use.
func RegisterDefinitions(defs []definitions.Definition, includeResources []string) error {
	builtins := builtinResourceFuncs()
	registry := &definitionRegistry{
		byResource:  make(map[string]*definitions.Definition, len(defs)),
		byGroupKind: make(map[schema.GroupKind]*definitions.Definition, len(defs)),
		included:    make(map[string]struct{}, len(includeResources)),
	}

	for i := range defs {
		def := &defs[i]
		groupKind := schema.GroupKind{Group: def.Group, Kind: def.Kind}

		if _, builtin := builtins[def.Resource]; builtin {
			return fmt.Errorf("%w: resource %q is a built-in resource", definitions.ErrInvalidDefinition, def.Resource)
		}

		if _, builtin := builtinGroupKinds[groupKind]; builtin {
			return fmt.Errorf("%w: resource %q uses %s, which a built-in resource already handles",
				definitions.ErrInvalidDefinition, def.Resource, groupKind)
		}

		registry.byResource[def.Resource] = def
		registry.byGroupKind[groupKind] = def
	}

	for _, resource := range includeResources {
		resource = strings.ToLower(resource)
		if _, defined := registry.byResource[resource]; defined {
			registry.included[resource] = struct{}{}
		}
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

	if err = RegisterDefinitions(defs, includeResources); err != nil {
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

// IncludedDefinitionsWithOptionalChildren returns the sorted included definitions that
// have children not marked always. Those children are only scaled with scale-children on.
func IncludedDefinitionsWithOptionalChildren() []string {
	registry := registeredDefinitions.Load()
	if registry == nil {
		return nil
	}

	var names []string

	for resource := range registry.included {
		for _, child := range registry.byResource[resource].Children {
			if !child.Always {
				names = append(names, resource)

				break
			}
		}
	}

	sort.Strings(names)

	return names
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

// isIncludedDefinition reports whether the definition's resource is in includedResources.
func isIncludedDefinition(def *definitions.Definition) bool {
	registry := registeredDefinitions.Load()
	if registry == nil {
		return false
	}

	_, included := registry.included[def.Resource]

	return included
}

func definitionByGroupKind(group, kind string) *definitions.Definition {
	registry := registeredDefinitions.Load()
	if registry == nil {
		return nil
	}

	return registry.byGroupKind[schema.GroupKind{Group: group, Kind: kind}]
}
