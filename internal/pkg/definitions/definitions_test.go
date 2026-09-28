package definitions

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// head is a definition's identity; tests append the fields under test.
const head = "- resource: a\n  group: g\n  version: v\n  kind: K\n"

const validReplicas = `
- resource: rabbitmqclusters
  group: rabbitmq.com
  version: v1beta1
  kind: RabbitmqCluster
  replicas:
    path: spec.replicas
`

func TestParse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{name: "valid replicas", input: validReplicas},
		{name: "empty", input: ""},
		{
			name: "valid suspend with everything",
			input: `
- resource: cnpgclusters
  plural: clusters
  group: postgresql.cnpg.io
  version: v1
  kind: Cluster
  suspend:
    annotation: cnpg.io/hibernation
    suspendedValue: "on"
    resumedValue: "off"
  children:
    - kind: StatefulSet
      match: labelEqualsName
      label: app
    - kind: Deployment
      match: ownerReference
  savings:
    groupsPath: spec.nodeSets
    countPath: count
    containersPath: podTemplate.spec.containers
`,
		},
		{name: "unknown field", input: validReplicas + "  replica: 1\n", wantErr: "unknown field"},
		{
			name:    "typo in nested field",
			input:   head + "  replicas:\n    pathh: spec.x\n",
			wantErr: "unknown field",
		},
		{name: "missing gvk", input: "- resource: a\n  replicas:\n    path: spec.x\n", wantErr: "group, version and kind are required"},
		{
			name:    "uppercase resource",
			input:   "- resource: Foo\n  group: g\n  version: v\n  kind: K\n  replicas:\n    path: spec.x\n",
			wantErr: "lowercase",
		},
		{
			name:    "no shape",
			input:   head,
			wantErr: "one of replicas or suspend is required",
		},
		{
			name: "both shapes",
			input: head + "  replicas:\n    path: spec.x\n" +
				"  suspend:\n    path: spec.s\n    suspendedValue: \"true\"\n    resumedValue: \"false\"\n",
			wantErr: "only one of replicas or suspend",
		},
		{
			name:    "bad path",
			input:   head + "  replicas:\n    path: spec..x\n",
			wantErr: "dot-separated",
		},
		{
			name:    "negative minimum",
			input:   head + "  replicas:\n    path: spec.x\n    minimum: -1\n",
			wantErr: "must not be negative",
		},
		{
			name:    "suspend path and annotation",
			input:   head + "  suspend:\n    path: spec.s\n    annotation: a/b\n    suspendedValue: \"true\"\n    resumedValue: \"false\"\n",
			wantErr: "exactly one of suspend.path or suspend.annotation",
		},
		{
			name:    "suspend same values",
			input:   head + "  suspend:\n    path: spec.s\n    suspendedValue: \"x\"\n    resumedValue: \"x\"\n",
			wantErr: "must differ",
		},
		{
			name:    "suspend missing values",
			input:   head + "  suspend:\n    path: spec.s\n",
			wantErr: "are required",
		},
		{
			name: "park guard on suspend",
			input: head + "  suspend:\n    path: spec.s\n    suspendedValue: \"true\"\n    resumedValue: \"false\"\n" +
				"  parkGuard:\n    requireAnnotations: [x]\n",
			wantErr: "parkGuard is only supported with replicas",
		},
		{name: "empty park guard", input: validReplicas + "  parkGuard:\n    requireAnnotations: []\n", wantErr: "must not be empty"},
		{
			name:    "bad child kind",
			input:   validReplicas + "  children:\n    - kind: DaemonSet\n      match: ownerReference\n",
			wantErr: "children[0].kind",
		},
		{
			name:    "bad child match",
			input:   validReplicas + "  children:\n    - kind: StatefulSet\n      match: name\n",
			wantErr: "children[0].match",
		},
		{
			name:    "label matcher without label",
			input:   validReplicas + "  children:\n    - kind: StatefulSet\n      match: labelEqualsName\n",
			wantErr: "label is required",
		},
		{
			name:    "owner matcher with label",
			input:   validReplicas + "  children:\n    - kind: StatefulSet\n      match: ownerReference\n      label: x\n",
			wantErr: "label is only used",
		},
		{
			name:    "savings without source",
			input:   validReplicas + "  savings:\n    countPath: x\n",
			wantErr: "exactly one of savings.requestsPath or savings.containersPath",
		},
		{
			name:    "savings groups on replicas",
			input:   validReplicas + "  savings:\n    requestsPath: spec.r\n    groupsPath: spec.g\n",
			wantErr: "only used with suspend",
		},
		{name: "duplicate resource", input: validReplicas + validReplicas, wantErr: "duplicate resource"},
		{
			name: "duplicate group and kind",
			input: validReplicas +
				"- resource: other\n  group: rabbitmq.com\n  version: v1\n  kind: RabbitmqCluster\n  replicas:\n    path: spec.replicas\n",
			wantErr: "duplicate group and kind",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := Parse([]byte(test.input))
			if test.wantErr == "" {
				assert.NoError(t, err)
				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), test.wantErr)
		})
	}
}

func TestLoad(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	defs, err := Load(filepath.Join(dir, "missing.yaml"))
	require.NoError(t, err)
	assert.Empty(t, defs)

	valid := filepath.Join(dir, "valid.yaml")
	require.NoError(t, os.WriteFile(valid, []byte(validReplicas), 0o600))

	defs, err = Load(valid)
	require.NoError(t, err)
	require.Len(t, defs, 1)
	assert.Equal(t, "rabbitmqclusters", defs[0].GetPlural())
	assert.Equal(t, "spec.replicas", defs[0].Replicas.Path)

	invalid := filepath.Join(dir, "invalid.yaml")
	require.NoError(t, os.WriteFile(invalid, []byte("- resource: a\n"), 0o600))

	_, err = Load(invalid)
	require.ErrorIs(t, err, ErrInvalidDefinition)
}

func TestGetPlural(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "clusters", (&Definition{Resource: "cnpgclusters", Plural: "clusters"}).GetPlural())
	assert.Equal(t, "cnpgclusters", (&Definition{Resource: "cnpgclusters"}).GetPlural())
}
