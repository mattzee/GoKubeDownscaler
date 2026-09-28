package scalable

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/caas-team/gokubedownscaler/internal/pkg/definitions"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// chartDefaultDefinitions loads the workloadDefinitions shipped as chart defaults,
// so tests exercise exactly what a default install runs.
func chartDefaultDefinitions(t *testing.T) map[string]*definitions.Definition {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("..", "..", "..", "deployments", "chart", "values.yaml"))
	require.NoError(t, err)

	var chartValues struct {
		WorkloadDefinitions map[string]map[string]any `json:"workloadDefinitions"`
	}
	require.NoError(t, yaml.Unmarshal(data, &chartValues))

	names := make([]string, 0, len(chartValues.WorkloadDefinitions))
	for name := range chartValues.WorkloadDefinitions {
		names = append(names, name)
	}

	sort.Strings(names)

	list := make([]map[string]any, 0, len(names))
	for _, name := range names {
		entry := chartValues.WorkloadDefinitions[name]
		entry["resource"] = name
		list = append(list, entry)
	}

	rendered, err := yaml.Marshal(list)
	require.NoError(t, err)

	defs, err := definitions.Parse(rendered)
	require.NoError(t, err)

	byResource := make(map[string]*definitions.Definition, len(defs))
	for i := range defs {
		byResource[defs[i].Resource] = &defs[i]
	}

	return byResource
}

// newFixture builds an unstructured custom resource for tests.
func newFixture(apiVersion, kind string, annotations, spec map[string]any) *unstructured.Unstructured {
	metadata := map[string]any{"name": "test", "namespace": "default"}
	if annotations != nil {
		metadata["annotations"] = annotations
	}

	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   metadata,
		"spec":       spec,
	}}
}

func requests(cpu, memory string) map[string]any {
	return map[string]any{"requests": map[string]any{"cpu": cpu, "memory": memory}}
}

func containers(reqs ...map[string]any) []any {
	result := make([]any, 0, len(reqs))
	for _, req := range reqs {
		result = append(result, map[string]any{"name": "main", "resources": req})
	}

	return result
}
