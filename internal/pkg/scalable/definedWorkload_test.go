package scalable

import (
	"context"
	"testing"

	"github.com/caas-team/gokubedownscaler/internal/pkg/definitions"
	"github.com/caas-team/gokubedownscaler/internal/pkg/values"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func replicaResource(t *testing.T, def *definitions.Definition, obj *unstructured.Unstructured) *definedReplicaResource {
	t.Helper()

	workload, ok := newDefinedWorkload(obj, def).(*replicaScaledWorkload)
	require.True(t, ok)

	resource, ok := workload.replicaScaledResource.(*definedReplicaResource)
	require.True(t, ok)

	return resource
}

func suspendResource(t *testing.T, def *definitions.Definition, obj *unstructured.Unstructured) *definedSuspendResource {
	t.Helper()

	workload, ok := newDefinedWorkload(obj, def).(*suspendScaledWorkload)
	require.True(t, ok)

	resource, ok := workload.suspendScaledResource.(*definedSuspendResource)
	require.True(t, ok)

	return resource
}

func TestDefinedReplicaResource_GetReplicas(t *testing.T) {
	t.Parallel()

	def := exampleDefinitions(t)["rabbitmqclusters"]

	tests := []struct {
		name    string
		members any
		want    values.Replicas
		wantErr bool
	}{
		{name: "int64", members: int64(3), want: values.AbsoluteReplicas(3)},
		{name: "float64 from API JSON", members: float64(5), want: values.AbsoluteReplicas(5)},
		{name: "absent", wantErr: true},
		{name: "wrong type", members: "3", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			spec := map[string]any{}
			if test.members != nil {
				spec["replicas"] = test.members
			}

			got, err := replicaResource(t, def, newFixture("rabbitmq.com/v1beta1", "RabbitmqCluster", nil, spec)).getReplicas()
			if test.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestDefinedReplicaResource_SetReplicas(t *testing.T) {
	t.Parallel()

	defs := exampleDefinitions(t)

	tests := []struct {
		name     string
		resource string
		kind     string
		path     []string
		target   int32
		want     int64
	}{
		{
			name:     "rabbit to 0",
			resource: "rabbitmqclusters",
			kind:     "RabbitmqCluster",
			path:     []string{"spec", "replicas"},
			target:   0,
			want:     0,
		},
		{
			name:     "sentinel clamped to minimum",
			resource: "redissentinels",
			kind:     "RedisSentinel",
			path:     []string{"spec", "clusterSize"},
			target:   0,
			want:     1,
		},
		{
			name:     "sentinel above minimum",
			resource: "redissentinels",
			kind:     "RedisSentinel",
			path:     []string{"spec", "clusterSize"},
			target:   2,
			want:     2,
		},
		{
			name:     "replication to 0",
			resource: "redisreplications",
			kind:     "RedisReplication",
			path:     []string{"spec", "clusterSize"},
			target:   0,
			want:     0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			def := defs[test.resource]
			obj := newFixture(def.Group+"/"+def.Version, test.kind, nil, map[string]any{})
			require.NoError(t, unstructured.SetNestedField(obj.Object, int64(3), test.path...))

			require.NoError(t, replicaResource(t, def, obj).setReplicas(test.target))

			got, _, _ := unstructured.NestedInt64(obj.Object, test.path...)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestDefinedSuspendResource_Annotation(t *testing.T) {
	t.Parallel()

	const pause = "eck.k8s.elastic.co/pause-orchestration"

	def := exampleDefinitions(t)["elasticsearches"]
	obj := newFixture("elasticsearch.k8s.elastic.co/v1", "Elasticsearch", map[string]any{"other": "kept"}, map[string]any{})
	resource := suspendResource(t, def, obj)

	current, target := resource.getSuspend()
	assert.Equal(t, values.BooleanReplicas(false), current, "a missing annotation reads as resumed")
	assert.Equal(t, values.BooleanReplicas(true), target)

	resource.setSuspend(true)
	assert.Equal(t, map[string]string{pause: "true", "other": "kept"}, obj.GetAnnotations())

	current, _ = resource.getSuspend()
	assert.Equal(t, values.BooleanReplicas(true), current)

	resource.setSuspend(false)
	assert.Equal(t, map[string]string{pause: "false", "other": "kept"}, obj.GetAnnotations())
}

func TestDefinedWorkload_SavedResources(t *testing.T) {
	t.Parallel()

	defs := exampleDefinitions(t)

	rabbit := replicaResource(t, defs["rabbitmqclusters"], newFixture("rabbitmq.com/v1beta1", "RabbitmqCluster", nil,
		map[string]any{"replicas": int64(3), "resources": requests("500m", "1Gi")}))
	saved := rabbit.getSavedResourcesRequests(3)
	assert.InDelta(t, 1.5, saved.TotalCPU(), 1e-9)
	assert.InDelta(t, 3*1024*1024*1024, saved.TotalMemory(), 1)

	elastic := suspendResource(t, defs["elasticsearches"], newFixture("elasticsearch.k8s.elastic.co/v1", "Elasticsearch", nil, map[string]any{
		"nodeSets": []any{
			map[string]any{"count": int64(3), "podTemplate": map[string]any{"spec": map[string]any{"containers": containers(requests("1", "1Gi"))}}},
			map[string]any{"count": int64(0), "podTemplate": map[string]any{"spec": map[string]any{
				"containers": containers(requests("2", "1Gi"), requests("100m", "1Gi")),
			}}},
		},
	}))
	assert.InDelta(t, 3*1+1*2.1, elastic.getSavedResourcesRequests().TotalCPU(), 1e-9, "a count of 0 counts as 1")

	none := replicaResource(t, &definitions.Definition{
		Resource: "things", Group: "example.com", Version: "v1", Kind: "Thing", Replicas: &definitions.Replicas{Path: "spec.replicas"},
	}, newFixture("example.com/v1", "Thing", nil, map[string]any{"replicas": int64(2)}))
	assert.Zero(t, none.getSavedResourcesRequests(2).TotalCPU())
}

func TestDefinedWorkload_CopyAndCompare(t *testing.T) {
	t.Parallel()

	defs := exampleDefinitions(t)
	obj := newFixture("rabbitmq.com/v1beta1", "RabbitmqCluster", nil, map[string]any{"replicas": int64(3)})
	workload := newDefinedWorkload(obj, defs["rabbitmqclusters"])

	copied, err := workload.Copy()
	require.NoError(t, err)

	_, err = copied.ScaleDown(values.AbsoluteReplicas(0), nil)
	require.NoError(t, err)

	patch, err := workload.Compare(copied)
	require.NoError(t, err)
	assert.NotEmpty(t, patch, "scaling the copy must not change the original")

	original, err := workload.(*replicaScaledWorkload).getReplicas()
	require.NoError(t, err)
	assert.Equal(t, values.AbsoluteReplicas(3), original)

	suspendWorkload := newDefinedWorkload(newFixture("elasticsearch.k8s.elastic.co/v1", "Elasticsearch", nil, map[string]any{}),
		defs["elasticsearches"])
	_, err = workload.Compare(suspendWorkload)
	require.Error(t, err, "comparing different shapes is an error")

	assert.Equal(t, "rabbitmq.com", workload.GroupVersionKind().Group)
	assert.Equal(t, "RabbitmqCluster", workload.GroupVersionKind().Kind)
}

func TestDefinedResource_GetChildren(t *testing.T) {
	t.Parallel()

	defs := exampleDefinitions(t)

	controller := true
	ownedBy := func(apiVersion, kind, name string) []metav1.OwnerReference {
		return []metav1.OwnerReference{{APIVersion: apiVersion, Kind: kind, Name: name, Controller: &controller}}
	}
	sts := func(name string, labels map[string]string, owners []metav1.OwnerReference) *appsv1.StatefulSet {
		return &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", Labels: labels, OwnerReferences: owners,
		}}
	}

	objects := []*appsv1.StatefulSet{
		sts("sentinel-owned", nil, ownedBy("redis.redis.opstreelabs.in/v1beta2", "RedisSentinel", "test")),
		sts("sentinel-other-name", nil, ownedBy("redis.redis.opstreelabs.in/v1beta2", "RedisSentinel", "other")),
		sts("sentinel-other-group", nil, ownedBy("example.com/v1", "RedisSentinel", "test")),
		sts("unowned", nil, nil),
		sts("es-labeled", map[string]string{"elasticsearch.k8s.elastic.co/cluster-name": "test"}, nil),
		sts("es-other-cluster", map[string]string{"elasticsearch.k8s.elastic.co/cluster-name": "other"}, nil),
	}

	builder := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme)
	for _, obj := range objects {
		builder = builder.WithObjects(obj)
	}

	builder = builder.WithObjects(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "thing-owned", Namespace: "default", OwnerReferences: ownedBy("example.com/v1", "Thing", "test"),
	}})

	clientsets := &Clientsets{Client: builder.Build()}

	thingDef := &definitions.Definition{
		Resource: "things", Group: "example.com", Version: "v1", Kind: "Thing",
		Replicas: &definitions.Replicas{Path: "spec.replicas"},
		Children: []definitions.Child{{Kind: "Deployment", Match: definitions.MatchOwnerReference}},
	}

	tests := []struct {
		name string
		def  *definitions.Definition
		obj  *unstructured.Unstructured
		want []string
	}{
		{
			name: "owner reference matches group, kind and name",
			def:  defs["redissentinels"],
			obj:  newFixture("redis.redis.opstreelabs.in/v1beta2", "RedisSentinel", nil, map[string]any{}),
			want: []string{"sentinel-owned"},
		},
		{
			name: "label equals name",
			def:  defs["elasticsearches"],
			obj:  newFixture("elasticsearch.k8s.elastic.co/v1", "Elasticsearch", nil, map[string]any{}),
			want: []string{"es-labeled"},
		},
		{
			name: "deployment children",
			def:  thingDef,
			obj:  newFixture("example.com/v1", "Thing", nil, map[string]any{}),
			want: []string{"thing-owned"},
		},
		{
			name: "no children defined",
			def:  defs["rabbitmqclusters"],
			obj:  newFixture("rabbitmq.com/v1beta1", "RabbitmqCluster", nil, map[string]any{}),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			workload := newDefinedWorkload(test.obj, test.def)

			parent, isParent := workload.(ParentWorkload)
			require.True(t, isParent)

			children, err := parent.GetChildren(context.Background(), clientsets)
			require.NoError(t, err)

			names := make([]string, 0, len(children))
			for _, child := range children {
				names = append(names, child.GetName())
				assert.Equal(t, "apps", child.GroupVersionKind().Group)
			}

			assert.ElementsMatch(t, test.want, names)
			assert.Equal(t, len(test.def.Children) > 0, MustScaleChildren(workload),
				"listed children are always scaled, through both the replica and the suspend wrapper")
		})
	}
}

func TestDefinedWorkload_ParseFromBytes(t *testing.T) {
	t.Parallel()

	def := exampleDefinitions(t)["redissentinels"]

	workload, err := parseDefinedWorkloadFromBytes(def,
		[]byte(`{"apiVersion":"redis.redis.opstreelabs.in/v1beta2","kind":"RedisSentinel",`+
			`"metadata":{"name":"s","namespace":"default"},"spec":{"clusterSize":3}}`))
	require.NoError(t, err)
	assert.Equal(t, "s", workload.GetName())

	_, err = parseDefinedWorkloadFromBytes(def, []byte("not json"))
	require.Error(t, err)
}

// TestDefinitionRegistry is not parallel: it swaps the process-wide registry.
//
//nolint:paralleltest // mutates the package-level registry
func TestDefinitionRegistry(t *testing.T) {
	previous := registeredDefinitions.Load()

	t.Cleanup(func() { registeredDefinitions.Store(previous) })

	defs := exampleDefinitions(t)
	list := make([]definitions.Definition, 0, len(defs))
	included := []string{"statefulsets"}

	for _, def := range defs {
		list = append(list, *def)

		if def.Resource != "redissentinels" {
			included = append(included, def.Resource)
		}
	}

	require.Error(t, RegisterDefinitions([]definitions.Definition{{Resource: "deployments"}}, nil), "built-in names cannot be shadowed")
	require.ErrorIs(t, RegisterDefinitions([]definitions.Definition{
		{Resource: "mydeployments", Group: "apps", Version: "v1", Kind: "Deployment"},
	}, nil), definitions.ErrInvalidDefinition, "built-in group/kinds cannot be taken over")
	require.ErrorIs(t, RegisterDefinitions([]definitions.Definition{
		{Resource: "mykruisesets", Group: "apps.kruise.io", Version: "v1beta1", Kind: "StatefulSet"},
	}, nil), definitions.ErrInvalidDefinition, "the kind name alone is not enough, the group must match too")
	require.ErrorIs(t, RegisterDefinitions([]definitions.Definition{
		{Resource: "mypostgres", Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster",
			Suspend: &definitions.Suspend{Annotation: "cnpg.io/hibernation", SuspendedValue: "on", ResumedValue: "off"}},
	}, nil), definitions.ErrInvalidDefinition, "built-in operator scalers such as CNPG cannot be redefined")
	require.NoError(t, RegisterDefinitions(list, included))

	assert.True(t, IsSupportedResource("deployments"))
	assert.True(t, IsSupportedResource(ScaledObjectsResource), "GetScaledObjects looks ScaledObjects up by this name")
	assert.True(t, IsSupportedResource("cnpgclusters"), "built in")
	assert.True(t, IsSupportedResource("rabbitmqclusters"), "defined")
	assert.False(t, IsSupportedResource("nothings"))
	assert.Len(t, RegisteredResources(), len(defs))

	controller := true
	owned := func(apiVersion, kind string) Workload {
		return &replicaScaledWorkload{&statefulSet{&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{{APIVersion: apiVersion, Kind: kind, Name: "x", Controller: &controller}},
		}}}}
	}

	sentinel := "redis.redis.opstreelabs.in/v1beta2"
	assert.False(t, isManagedByOwnerReference(owned(sentinel, "RedisSentinel")),
		"a defined owner that isn't included leaves its StatefulSets to the statefulsets scaler")

	require.NoError(t, RegisterDefinitions(list, append(included, "RedisSentinels")))
	assert.True(t, isManagedByOwnerReference(owned(sentinel, "RedisSentinel")),
		"an included defined owner scales its StatefulSets itself")
	assert.False(t, isManagedByOwnerReference(owned("example.com/v1", "RedisSentinel")),
		"a defined owner is matched by group as well as kind")

	workload, err := ParseWorkloadFromRawObject("redis.redis.opstreelabs.in", "RedisSentinel", "redissentinel",
		[]byte(`{"apiVersion":"redis.redis.opstreelabs.in/v1beta2","kind":"RedisSentinel","metadata":{"name":"s","namespace":"default"}}`))
	require.NoError(t, err, "the webhook parses defined kinds by group and kind")
	assert.Equal(t, "s", workload.GetName())

	_, err = ParseWorkloadFromRawObject("example.com", "Nothing", "nothing", []byte(`{}`))
	require.Error(t, err)
}
