package scalable

import (
	"context"
	"testing"

	"github.com/caas-team/gokubedownscaler/internal/pkg/values"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	mebibyte = 1024 * 1024
	gibibyte = 1024 * mebibyte

	mongoLastGoodAnnotation = "mongodb.com/v1.lastSuccessfulConfiguration"
	eckClusterLabel         = "elasticsearch.k8s.elastic.co/cluster-name"
)

// operatorCase drives one operator through a full park and wake on the chart's
// default definition. Every fixture's custom resource is named "test".
type operatorCase struct {
	name     string
	resource string
	obj      func() *unstructured.Unstructured
	down     values.Replicas
	// value reads the field or annotation the definition drives.
	value func(*unstructured.Unstructured) any
	// wantRefused expects the park to be refused and the object left untouched.
	wantRefused bool
	// wantParked is value after ScaleDown; wantDownUpdate is false when ScaleDown must be a no-op.
	wantParked     any
	wantDownUpdate bool
	wantFrom       values.Replicas
	wantTo         values.Replicas
	wantCPU        float64
	wantMemory     float64
	// wantOriginal is the downscaler/original-replicas annotation after ScaleDown ("" for none).
	wantOriginal string
	// wantWoken is value after ScaleUp.
	wantWoken    any
	wantChildren []string
}

func specInt(path ...string) func(*unstructured.Unstructured) any {
	return func(obj *unstructured.Unstructured) any {
		val, _, _ := unstructured.NestedInt64(obj.Object, append([]string{"spec"}, path...)...)
		return val
	}
}

func annotation(key string) func(*unstructured.Unstructured) any {
	return func(obj *unstructured.Unstructured) any {
		return obj.GetAnnotations()[key]
	}
}

func rabbitFixture() *unstructured.Unstructured {
	return newFixture("rabbitmq.com/v1beta1", "RabbitmqCluster", nil,
		map[string]any{"replicas": int64(3), "resources": requests("500m", "1Gi")})
}

func cnpgFixture(annotations map[string]any) func() *unstructured.Unstructured {
	return func() *unstructured.Unstructured {
		return newFixture("postgresql.cnpg.io/v1", "Cluster", annotations,
			map[string]any{"instances": int64(3), "resources": requests("250m", "512Mi")})
	}
}

func elasticFixture() *unstructured.Unstructured {
	return newFixture("elasticsearch.k8s.elastic.co/v1", "Elasticsearch", map[string]any{"other": "kept"}, map[string]any{
		"nodeSets": []any{
			map[string]any{"name": "master", "count": int64(3), "podTemplate": map[string]any{"spec": map[string]any{
				"containers": containers(requests("1", "2Gi")),
			}}},
			map[string]any{"name": "data", "count": int64(0), "podTemplate": map[string]any{"spec": map[string]any{
				"containers": containers(requests("2", "4Gi"), requests("100m", "64Mi")),
			}}},
		},
	})
}

func mongoFixture(annotations map[string]any) func() *unstructured.Unstructured {
	return func() *unstructured.Unstructured {
		return newFixture("mongodbcommunity.mongodb.com/v1", "MongoDBCommunity", annotations, map[string]any{
			"members": int64(3),
			"statefulSet": map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
				"containers": containers(requests("500m", "1Gi"), requests("50m", "128Mi")),
			}}}},
		})
	}
}

func redisFixture(kind string) func() *unstructured.Unstructured {
	return func() *unstructured.Unstructured {
		return newFixture("redis.redis.opstreelabs.in/v1beta2", kind, nil, map[string]any{
			"clusterSize":      int64(3),
			"kubernetesConfig": map[string]any{"resources": requests("100m", "256Mi")},
		})
	}
}

// operatorChildrenClient holds child candidates for every operator: each
// operator's real child, plus decoys that must never be picked up.
func operatorChildrenClient() *Clientsets {
	controller := true
	sts := func(name string, labels map[string]string, apiVersion, kind, owner string) *appsv1.StatefulSet {
		obj := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: labels}}
		if kind != "" {
			obj.OwnerReferences = []metav1.OwnerReference{{APIVersion: apiVersion, Kind: kind, Name: owner, Controller: &controller}}
		}

		return obj
	}

	objects := []*appsv1.StatefulSet{
		sts("mongo-owned", nil, "mongodbcommunity.mongodb.com/v1", "MongoDBCommunity", "test"),
		sts("mongo-other", nil, "mongodbcommunity.mongodb.com/v1", "MongoDBCommunity", "other"),
		sts("sentinel-owned", nil, "redis.redis.opstreelabs.in/v1beta2", "RedisSentinel", "test"),
		sts("replication-owned", nil, "redis.redis.opstreelabs.in/v1beta2", "RedisReplication", "test"),
		sts("rabbit-owned", nil, "rabbitmq.com/v1beta1", "RabbitmqCluster", "test"),
		sts("cnpg-owned", nil, "postgresql.cnpg.io/v1", "Cluster", "test"),
		sts("es-master", map[string]string{eckClusterLabel: "test"}, "", "", ""),
		sts("es-other", map[string]string{eckClusterLabel: "other"}, "", "", ""),
	}

	builder := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme)
	for _, obj := range objects {
		builder = builder.WithObjects(obj)
	}

	return &Clientsets{Client: builder.Build()}
}

func operatorCases() []operatorCase {
	zero := values.AbsoluteReplicas(0)
	lastGood := map[string]any{mongoLastGoodAnnotation: "{}"}

	return []operatorCase{
		{
			name: "rabbit park", resource: "rabbitmqclusters", obj: rabbitFixture, down: zero,
			value: specInt("replicas"), wantParked: int64(0), wantDownUpdate: true,
			wantFrom: values.AbsoluteReplicas(3), wantTo: zero,
			wantCPU: 3 * 0.5, wantMemory: 3 * gibibyte,
			wantOriginal: "3", wantWoken: int64(3),
		},
		{
			name: "rabbit partial downscale", resource: "rabbitmqclusters", obj: rabbitFixture, down: values.AbsoluteReplicas(1),
			value: specInt("replicas"), wantParked: int64(1), wantDownUpdate: true,
			wantFrom: values.AbsoluteReplicas(3), wantTo: values.AbsoluteReplicas(1),
			wantCPU: 2 * 0.5, wantMemory: 2 * gibibyte,
			wantOriginal: "3", wantWoken: int64(3),
		},
		{
			name: "cnpg hibernate and rehydrate", resource: "cnpgclusters", obj: cnpgFixture(map[string]any{"other": "kept"}), down: zero,
			value: annotation("cnpg.io/hibernation"), wantParked: "on", wantDownUpdate: true,
			wantFrom: values.BooleanReplicas(false), wantTo: values.BooleanReplicas(true),
			wantCPU: 3 * 0.25, wantMemory: 3 * 512 * mebibyte,
			wantOriginal: "false", wantWoken: "off",
		},
		{
			name: "cnpg hibernated by someone else is left alone", resource: "cnpgclusters",
			obj: cnpgFixture(map[string]any{"cnpg.io/hibernation": "on"}), down: zero,
			value: annotation("cnpg.io/hibernation"), wantParked: "on", wantDownUpdate: false,
			wantFrom: values.BooleanReplicas(true), wantTo: values.BooleanReplicas(true),
			wantOriginal: "", wantWoken: "on",
		},
		{
			name: "elasticsearch pause and resume", resource: "elasticsearches", obj: elasticFixture, down: zero,
			value: annotation("eck.k8s.elastic.co/pause-orchestration"), wantParked: "true", wantDownUpdate: true,
			wantFrom: values.BooleanReplicas(false), wantTo: values.BooleanReplicas(true),
			// master: 3 pods x 1 CPU; data: count 0 counts as 1 pod x (2 + 0.1) CPU.
			wantCPU: 3*1 + 2.1, wantMemory: 3*2*gibibyte + 4*gibibyte + 64*mebibyte,
			wantOriginal: "false", wantWoken: "false",
			wantChildren: []string{"es-master"},
		},
		{
			name: "mongo park with a known-good config", resource: "mongodbcommunities", obj: mongoFixture(lastGood), down: zero,
			value: specInt("members"), wantParked: int64(0), wantDownUpdate: true,
			wantFrom: values.AbsoluteReplicas(3), wantTo: zero,
			wantCPU: 3 * 0.55, wantMemory: 3 * (gibibyte + 128*mebibyte),
			wantOriginal: "3", wantWoken: int64(3),
			wantChildren: []string{"mongo-owned"},
		},
		{
			name: "mongo park refused without a known-good config", resource: "mongodbcommunities", obj: mongoFixture(nil), down: zero,
			value: specInt("members"), wantRefused: true,
			wantChildren: []string{"mongo-owned"},
		},
		{
			name: "mongo partial downscale is not guarded", resource: "mongodbcommunities", obj: mongoFixture(nil),
			down:  values.AbsoluteReplicas(1),
			value: specInt("members"), wantParked: int64(1), wantDownUpdate: true,
			wantFrom: values.AbsoluteReplicas(3), wantTo: values.AbsoluteReplicas(1),
			wantCPU: 2 * 0.55, wantMemory: 2 * (gibibyte + 128*mebibyte),
			wantOriginal: "3", wantWoken: int64(3),
			wantChildren: []string{"mongo-owned"},
		},
		{
			name: "redis replication park", resource: "redisreplications", obj: redisFixture("RedisReplication"), down: zero,
			value: specInt("clusterSize"), wantParked: int64(0), wantDownUpdate: true,
			wantFrom: values.AbsoluteReplicas(3), wantTo: zero,
			wantCPU: 3 * 0.1, wantMemory: 3 * 256 * mebibyte,
			wantOriginal: "3", wantWoken: int64(3),
		},
		{
			// The CRD forbids clusterSize 0, so the CR stops at 1 and the child StatefulSet carries the park.
			// The CR saves 2 pods; the last pod is saved (and counted) by the child StatefulSet.
			// The repeated park and the wake prove a parked sentinel is not re-parked and wakes to 3, not 1.
			name: "redis sentinel floors at 1 and parks through its child", resource: "redissentinels",
			obj: redisFixture("RedisSentinel"), down: zero,
			value: specInt("clusterSize"), wantParked: int64(1), wantDownUpdate: true,
			wantFrom: values.AbsoluteReplicas(3), wantTo: values.AbsoluteReplicas(1),
			wantCPU: 2 * 0.1, wantMemory: 2 * 256 * mebibyte,
			wantOriginal: "3", wantWoken: int64(3),
			wantChildren: []string{"sentinel-owned"},
		},
	}
}

// TestDefinedOperators runs a park, a repeated park and a wake for every operator
// shipped in the chart's default workloadDefinitions.
func TestDefinedOperators(t *testing.T) {
	t.Parallel()

	defs := chartDefaultDefinitions(t)
	clientsets := operatorChildrenClient()

	for _, test := range operatorCases() {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			def := defs[test.resource]
			require.NotNil(t, def, "the chart must ship a %s definition", test.resource)

			obj := test.obj()
			pristine := obj.DeepCopy()
			workload := newDefinedWorkload(obj, def)

			assert.Equal(t, def.Group, workload.GroupVersionKind().Group)
			assert.Equal(t, def.Kind, workload.GroupVersionKind().Kind)

			parent, ok := workload.(ParentWorkload)
			require.True(t, ok)

			children, err := parent.GetChildren(context.Background(), clientsets)
			require.NoError(t, err)

			childNames := make([]string, 0, len(children))
			for _, child := range children {
				childNames = append(childNames, child.GetName())
			}

			assert.ElementsMatch(t, test.wantChildren, childNames, "children")

			down, err := workload.ScaleDown(test.down, nil)
			if test.wantRefused {
				var refused *ParkRefusedError
				require.ErrorAs(t, err, &refused)
				assert.Equal(t, pristine.Object, obj.Object, "a refused park must not change the object")

				return
			}

			require.NoError(t, err)
			assert.Equal(t, test.wantParked, test.value(obj), "value after park")
			assert.Equal(t, test.wantDownUpdate, down.IsUpdateNeeded, "park needs an update")
			assert.Equal(t, test.wantFrom, down.From)
			assert.Equal(t, test.wantTo, down.To)
			assert.InDelta(t, test.wantCPU, down.SavedResources.TotalCPU(), 1e-9, "saved CPU")
			assert.InDelta(t, test.wantMemory, down.SavedResources.TotalMemory(), 1, "saved memory")
			assert.Equal(t, test.wantOriginal, obj.GetAnnotations()[annotationOriginalReplicas], "original replicas annotation")
			assert.Equal(t, pristine.GetAnnotations()["other"], obj.GetAnnotations()["other"], "unrelated annotations are kept")

			again, err := workload.ScaleDown(test.down, nil)
			require.NoError(t, err)
			assert.False(t, again.IsUpdateNeeded, "a repeated park is a no-op")
			assert.Equal(t, test.wantParked, test.value(obj), "value after repeated park")

			if test.wantOriginal != "" {
				assert.InDelta(t, test.wantCPU, again.SavedResources.TotalCPU(), 1e-9, "a repeated park still reports the saving")
			}

			woken, err := workload.ScaleUp(nil)
			require.NoError(t, err)
			assert.Equal(t, test.wantWoken, test.value(obj), "value after wake")
			assert.Equal(t, test.wantDownUpdate, woken.IsUpdateNeeded, "wake needs an update only after a park")
			assert.NotContains(t, obj.GetAnnotations(), annotationOriginalReplicas, "wake clears the original replicas annotation")
			assert.Equal(t, pristine.GetAnnotations()["other"], obj.GetAnnotations()["other"], "unrelated annotations are kept")

			if test.wantDownUpdate {
				assert.Equal(t, test.wantOriginal, woken.To.String(), "wake restores the original")
			}
		})
	}
}
