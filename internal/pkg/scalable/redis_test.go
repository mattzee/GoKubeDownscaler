package scalable

import (
	"testing"

	"github.com/caas-team/gokubedownscaler/internal/pkg/values"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// newTestRedis builds a redisWorkload of the given GVK with the given clusterSize.
// Pass nil to omit spec.clusterSize.
func newTestRedis(gvk schema.GroupVersionKind, clusterSize any) *redisWorkload {
	spec := map[string]any{}
	if clusterSize != nil {
		spec["clusterSize"] = clusterSize
	}

	obj := map[string]any{
		"apiVersion": gvk.Group + "/" + gvk.Version,
		"kind":       gvk.Kind,
		"metadata": map[string]any{
			"name":      "test-redis",
			"namespace": "default",
		},
		"spec": spec,
	}

	u := &unstructured.Unstructured{Object: obj}
	u.SetGroupVersionKind(gvk)

	return &redisWorkload{Unstructured: u, gvk: gvk}
}

func TestRedis_GetSetReplicas_BothKinds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		gvk       schema.GroupVersionKind
		setTo     int32
		wantAfter values.Replicas
		note      string
	}{
		{
			gvk: redisReplicationGVK, setTo: 0, wantAfter: values.AbsoluteReplicas(0),
			note: "RedisReplication parks to clusterSize 0 on the CR directly",
		},
		{
			// RedisSentinel's CRD forbids clusterSize 0, so setReplicas(0) clamps to
			// the minimum of 1; the real park is carried by its child StatefulSet.
			gvk: redisSentinelGVK, setTo: 0, wantAfter: values.AbsoluteReplicas(1),
			note: "RedisSentinel clamps clusterSize to its CRD minimum of 1",
		},
	}

	for _, test := range tests {
		t.Run(test.gvk.Kind, func(t *testing.T) {
			t.Parallel()

			redis := newTestRedis(test.gvk, int64(3))

			got, err := redis.getReplicas()
			require.NoError(t, err)
			assert.Equal(t, values.AbsoluteReplicas(3), got)

			require.NoError(t, redis.setReplicas(test.setTo))

			got, err = redis.getReplicas()
			require.NoError(t, err)
			assert.Equal(t, test.wantAfter, got, test.note)

			// spec.replicas must never be written by this scaler; clusterSize is the field.
			_, found, _ := unstructured.NestedFieldNoCopy(redis.Object, "spec", "replicas")
			assert.False(t, found, "scaler must not touch spec.replicas")
		})
	}
}

func TestRedis_GetReplicas_AbsentClusterSize(t *testing.T) {
	t.Parallel()

	redis := newTestRedis(redisReplicationGVK, nil)
	_, err := redis.getReplicas()
	require.Error(t, err)
}

func TestRedis_GroupVersionKind_BothKinds(t *testing.T) {
	t.Parallel()

	rep := newTestRedis(redisReplicationGVK, int64(3))
	repGVK := rep.GroupVersionKind()
	assert.Equal(t, "redis.redis.opstreelabs.in", repGVK.Group)
	assert.Equal(t, "v1beta2", repGVK.Version)
	assert.Equal(t, "RedisReplication", repGVK.Kind)

	sen := newTestRedis(redisSentinelGVK, int64(3))
	senGVK := sen.GroupVersionKind()
	assert.Equal(t, "RedisSentinel", senGVK.Kind)
}

func TestRedis_Copy_IsDeepCopy(t *testing.T) {
	t.Parallel()

	original := newTestRedis(redisReplicationGVK, int64(3))

	copied, err := original.Copy()
	require.NoError(t, err)

	rsw, ok := copied.(*replicaScaledWorkload)
	require.True(t, ok)

	copiedRedis, ok := rsw.replicaScaledResource.(*redisWorkload)
	require.True(t, ok)
	assert.Equal(t, redisReplicationGVK, copiedRedis.gvk, "copy must carry the same GVK")

	require.NoError(t, copiedRedis.setReplicas(0))

	gotOrig, err := original.getReplicas()
	require.NoError(t, err)
	assert.Equal(t, values.AbsoluteReplicas(3), gotOrig, "mutating the copy must not touch the original")
}

// TestRedis_ParkRepeatWake runs the scan sequence the downscaler actually performs:
// park, park again on the next scan while still in downtime, then wake. A parked
// RedisSentinel sits at its CRD floor of 1, which must count as already scaled down;
// otherwise the second park overwrites the original clusterSize with 1 and the
// wake restores 1 instead of 3.
func TestRedis_ParkRepeatWake(t *testing.T) {
	t.Parallel()

	tests := []struct {
		gvk        schema.GroupVersionKind
		wantParked int64
		wantTo     values.Replicas
		wantCPU    float64
	}{
		{gvk: redisReplicationGVK, wantParked: 0, wantTo: values.AbsoluteReplicas(0), wantCPU: 3 * 0.1},
		// The CR saves 2 pods; the last one is saved (and counted) by the child StatefulSet.
		{gvk: redisSentinelGVK, wantParked: 1, wantTo: values.AbsoluteReplicas(1), wantCPU: 2 * 0.1},
	}

	for _, test := range tests {
		t.Run(test.gvk.Kind, func(t *testing.T) {
			t.Parallel()

			redis := newTestRedis(test.gvk, int64(3))
			require.NoError(t, unstructured.SetNestedField(redis.Object, "100m",
				"spec", "kubernetesConfig", "resources", "requests", "cpu"))

			workload := &replicaScaledWorkload{redis}
			clusterSize := func() int64 {
				val, _, _ := unstructured.NestedInt64(redis.Object, "spec", "clusterSize")
				return val
			}

			parked, err := workload.ScaleDown(values.AbsoluteReplicas(0), nil)
			require.NoError(t, err)
			assert.True(t, parked.IsUpdateNeeded)
			assert.Equal(t, test.wantParked, clusterSize())
			assert.Equal(t, test.wantTo, parked.To)
			assert.InDelta(t, test.wantCPU, parked.SavedResources.TotalCPU(), 1e-9)
			assert.Equal(t, "3", redis.GetAnnotations()[annotationOriginalReplicas])

			again, err := workload.ScaleDown(values.AbsoluteReplicas(0), nil)
			require.NoError(t, err)
			assert.False(t, again.IsUpdateNeeded, "the next scan must not park again")
			assert.Equal(t, "3", redis.GetAnnotations()[annotationOriginalReplicas], "the original must survive the next scan")
			assert.InDelta(t, test.wantCPU, again.SavedResources.TotalCPU(), 1e-9)

			woken, err := workload.ScaleUp(nil)
			require.NoError(t, err)
			assert.True(t, woken.IsUpdateNeeded)
			assert.Equal(t, int64(3), clusterSize(), "wake restores the original clusterSize")
			assert.NotContains(t, redis.GetAnnotations(), annotationOriginalReplicas)
		})
	}
}
