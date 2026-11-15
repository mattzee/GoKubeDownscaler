//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	originalReplicas = "downscaler/original-replicas"
	forceDowntime    = "downscaler/force-downtime"
	forceUptime      = "downscaler/force-uptime"
	eckPause         = "eck.k8s.elastic.co/pause-orchestration"
	cnpgHibernation  = "cnpg.io/hibernation"

	readyTimeout = 15 * time.Minute
	scaleTimeout = 5 * time.Minute
)

// check reports whether one expected state holds, and what it saw if not.
type check struct {
	name string
	ok   func(ctx context.Context) (bool, string)
}

// TestDownscaler runs in order: the steps share one set of workloads, and each
// starts from the state the previous one left.
func TestDownscaler(t *testing.T) {
	ctx := context.Background()

	// Cleanups run last-registered first, so the report comes after the rest
	// and its wait lets the log follower catch the final scale actions.
	t.Cleanup(func() {
		time.Sleep(2 * time.Second)
		writeEvidenceReport(t, !t.Failed())
	})
	t.Cleanup(func() { _ = clearForceAnnotations(ctx) })
	t.Cleanup(func() { logDownscalerOnFailure(ctx, t) })

	preconditionsMet := t.Run("preconditions", func(t *testing.T) {
		eventually(t, readyTimeout, check{"the downscaler is ready", downscalerReady})
		eventually(t, readyTimeout, check{"all pods in " + envNamespace + " are ready", podsReady})
		expectHealthy(t, "every operator must start healthy")
	})
	if !preconditionsMet {
		// Scaling a broken environment would only produce misleading results.
		t.Fatal("stopping: the environment is not healthy, so nothing after the preconditions would be meaningful")
	}

	t.Run("downscaler health", func(t *testing.T) {
		pod := downscalerPod(ctx, t)

		for _, path := range []string{"/healthz", "/readyz"} {
			body, err := k8s.kube.CoreV1().Pods(downscalerNamespace).ProxyGet("http", pod, "8080", path, nil).DoRaw(ctx)
			verify(t, "GET "+path+" succeeds", err == nil, resultOf(err, strings.TrimSpace(string(body))))
		}

		metrics, err := k8s.kube.CoreV1().Pods(downscalerNamespace).ProxyGet("http", pod, "8080", "/metrics", nil).DoRaw(ctx)
		verify(t, "GET /metrics succeeds", err == nil, resultOf(err, fmt.Sprintf("%d bytes", len(metrics))))
		verify(t, "/metrics has kubedownscaler_cycle_executions_total",
			strings.Contains(string(metrics), "kubedownscaler_cycle_executions_total"), presence(string(metrics), "kubedownscaler_cycle_executions_total"))

		logs := downscalerLogs(ctx, t, pod)
		verify(t, "the downscaler logged \"loaded workload definitions\"",
			strings.Contains(logs, "loaded workload definitions"), presence(logs, "loaded workload definitions"))

		for _, resource := range []string{"rabbitmqclusters", "elasticsearches", "redisreplications", "redissentinels"} {
			verify(t, "the loaded definitions include "+resource, strings.Contains(logs, resource), presence(logs, resource))
		}

		forbidden := strings.Count(strings.ToLower(logs), "forbidden")
		verify(t, "no RBAC \"forbidden\" errors in the downscaler log", forbidden == 0, fmt.Sprintf("%d occurrences", forbidden))
	})

	originals := map[string]int64{
		"search-es-default":       stsReplicas(ctx, t, "search-es-default"),
		"redis-sentinel-sentinel": stsReplicas(ctx, t, "redis-sentinel-sentinel"),
		"mongo":                   stsReplicas(ctx, t, "mongo"),
	}

	t.Run("downscale", func(t *testing.T) {
		annotateNamespace(ctx, t, map[string]any{forceDowntime: "true", forceUptime: nil})

		for _, c := range downscaledChecks(originals) {
			eventually(t, scaleTimeout, c)
		}
	})

	t.Run("excluded workload stays up", func(t *testing.T) {
		replicas := deploymentReplicas(ctx, t, "web-excluded")
		verify(t, "Deployment web-excluded still has 1 replica", replicas == 1, fmt.Sprintf("%d", replicas))

		annotation := deploymentAnnotation(ctx, t, "web-excluded", originalReplicas)
		verify(t, "Deployment web-excluded has no "+originalReplicas+" annotation", annotation == "", fmt.Sprintf("%q", annotation))
	})

	t.Run("operator-owned StatefulSets are left to their operators", func(t *testing.T) {
		// The operators copy their custom resource's annotations onto these
		// StatefulSets, so original-replicas can be there; what matters is that
		// the downscaler did not write it.
		for _, name := range []string{"rabbit-server", "redis-replication"} {
			sts, err := getSts(ctx, name)
			require.NoError(t, err)

			wrote := downscalerWroteAnnotation(sts.ManagedFields, originalReplicas)
			seen := "not written by the downscaler"

			if wrote {
				seen = "written by the downscaler"
			}

			verify(t, "StatefulSet "+name+" (operator-owned) was not downscaled by the statefulsets scaler", !wrote, seen)
		}
	})

	t.Run("repeated scans change nothing", func(t *testing.T) {
		before := snapshot(ctx, t)

		pause(t, 3*scanInterval, "three scans, then compare every workload")

		changed := diffSnapshots(before, snapshot(ctx, t))
		verify(t, fmt.Sprintf("all %d tracked values unchanged over three scans", len(before)),
			len(changed) == 0, orNone(strings.Join(changed, "; ")))
	})

	t.Run("upscale", func(t *testing.T) {
		annotateNamespace(ctx, t, map[string]any{forceDowntime: nil, forceUptime: "true"})

		for _, c := range upscaledChecks(originals) {
			eventually(t, scaleTimeout, c)
		}

		eventually(t, readyTimeout, check{"all pods in " + envNamespace + " are ready again", podsReady})
		expectHealthy(t, "every operator must be healthy again after the upscale")
	})

	t.Run("hand back to schedule", func(t *testing.T) {
		annotateNamespace(ctx, t, map[string]any{forceUptime: nil})

		pause(t, 3*scanInterval, "three scans on the normal schedule")

		for _, c := range upscaledChecks(originals) {
			eventually(t, scanInterval, c)
		}
	})

	t.Run("per operator", func(t *testing.T) {
		untouched := map[string]int64{
			"web":          deploymentGeneration(ctx, t, "web"),
			"web-excluded": deploymentGeneration(ctx, t, "web-excluded"),
		}

		t.Run("operators", func(t *testing.T) {
			for _, op := range operatorScenarios(originals) {
				t.Run(op.name, func(t *testing.T) {
					t.Parallel()
					runOperatorScenario(ctx, t, op)
				})
			}
		})

		for _, name := range []string{"web", "web-excluded"} {
			after := deploymentGeneration(ctx, t, name)
			verify(t, "Deployment "+name+" unchanged while single operators were scaled",
				after == untouched[name], fmt.Sprintf("generation %d → %d", untouched[name], after))
		}
	})

	t.Run("expired windows", func(t *testing.T) {
		t.Run("an expired workload window yields to the environment", func(t *testing.T) {
			t.Cleanup(func() {
				_ = patchDeploymentAnnotations(ctx, "web", map[string]any{forceUptime: nil})
				_ = clearForceAnnotations(ctx)
			})

			t.Logf("action: Deployment/web: %s", describeAnnotations(map[string]any{forceUptime: expiredWindow}))
			require.NoError(t, patchDeploymentAnnotations(ctx, "web", map[string]any{forceUptime: expiredWindow}))
			annotateNamespace(ctx, t, map[string]any{forceDowntime: "true"})

			eventually(t, scaleTimeout, deploymentAt("web", 0))

			annotateNamespace(ctx, t, map[string]any{forceDowntime: nil, forceUptime: "true"})
			eventually(t, scaleTimeout, deploymentAt("web", 2))
		})

		t.Run("an expired environment window hands back to the schedule", func(t *testing.T) {
			t.Cleanup(func() { _ = patchNamespaceAnnotations(ctx, map[string]any{uptime: nil, forceDowntime: nil}) })

			window := windowFromNow(90 * time.Second)
			annotateNamespace(ctx, t, map[string]any{uptime: "always", forceDowntime: window})

			eventually(t, 60*time.Second, deploymentAt("web", 0))
			eventually(t, scaleTimeout, deploymentAt("web", 2))
		})
	})

	t.Run("everything is healthy at the end", func(t *testing.T) {
		expectHealthy(t, "the run must leave every operator healthy")
	})
}

const (
	uptime = "downscaler/uptime"

	// expiredWindow is an absolute force window that ended long ago.
	expiredWindow = "2020-01-01T00:00:00Z-2020-01-01T01:00:00Z"
)

// windowFromNow is an absolute force window from now until d from now.
func windowFromNow(d time.Duration) string {
	now := time.Now().UTC().Truncate(time.Second)
	return now.Format(time.RFC3339) + "-" + now.Add(d).Format(time.RFC3339)
}

// operatorScenario scales one operator's resources on their own, through
// annotations on the resources themselves rather than on the environment.
type operatorScenario struct {
	name    string
	targets []target
	down    []check
	up      []check
}

type target struct {
	gvk  schema.GroupVersionKind
	name string
}

func operatorScenarios(originals map[string]int64) []operatorScenario {
	return []operatorScenario{
		{
			name:    "rabbitmq",
			targets: []target{{rabbitmqClusterGVK, "rabbit"}},
			down:    []check{crInt(rabbitmqClusterGVK, "rabbit", 0, "spec", "replicas"), stsAt("rabbit-server", 0)},
			up:      []check{crInt(rabbitmqClusterGVK, "rabbit", 1, "spec", "replicas")},
		},
		{
			name:    "elasticsearch",
			targets: []target{{elasticsearchGVK, "search"}},
			down:    []check{crAnnotation(elasticsearchGVK, "search", eckPause, "true"), stsAt("search-es-default", 0)},
			up: []check{
				crAnnotation(elasticsearchGVK, "search", eckPause, "false"),
				stsAt("search-es-default", originals["search-es-default"]),
			},
		},
		{
			name:    "redis",
			targets: []target{{redisReplicationGVK, "redis-replication"}, {redisSentinelGVK, "redis-sentinel"}},
			down: []check{
				crInt(redisReplicationGVK, "redis-replication", 0, "spec", "clusterSize"),
				crInt(redisSentinelGVK, "redis-sentinel", 1, "spec", "clusterSize"),
				stsAt("redis-sentinel-sentinel", 0),
			},
			up: []check{
				crInt(redisReplicationGVK, "redis-replication", 2, "spec", "clusterSize"),
				crInt(redisSentinelGVK, "redis-sentinel", 3, "spec", "clusterSize"),
				stsAt("redis-sentinel-sentinel", originals["redis-sentinel-sentinel"]),
			},
		},
		{
			name:    "postgres",
			targets: []target{{cnpgClusterGVK, "pg"}},
			down:    []check{crAnnotation(cnpgClusterGVK, "pg", cnpgHibernation, "on"), noPods("cnpg.io/cluster=pg")},
			up:      []check{crAnnotation(cnpgClusterGVK, "pg", cnpgHibernation, "off")},
		},
		{
			name:    "mongodb",
			targets: []target{{mongoDBCommunityGVK, "mongo"}},
			down:    []check{crInt(mongoDBCommunityGVK, "mongo", 0, "spec", "members"), stsAt("mongo", 0)},
			up: []check{
				crInt(mongoDBCommunityGVK, "mongo", 1, "spec", "members"),
				stsAt("mongo", originals["mongo"]),
			},
		},
	}
}

func runOperatorScenario(ctx context.Context, t *testing.T, op operatorScenario) {
	t.Helper()

	annotateTargets := func(annotations map[string]any) {
		for _, tg := range op.targets {
			t.Logf("action: %s/%s: %s", tg.gvk.Kind, tg.name, describeAnnotations(annotations))
			require.NoError(t, patchCRAnnotations(ctx, tg.gvk, tg.name, annotations))
		}
	}

	t.Cleanup(func() {
		for _, tg := range op.targets {
			_ = patchCRAnnotations(ctx, tg.gvk, tg.name, map[string]any{forceDowntime: nil, forceUptime: nil})
		}
	})

	annotateTargets(map[string]any{forceDowntime: "true"})

	for _, c := range op.down {
		eventually(t, scaleTimeout, c)
	}

	annotateTargets(map[string]any{forceDowntime: nil, forceUptime: "true"})

	for _, c := range op.up {
		eventually(t, scaleTimeout, c)
	}

	expectHealthy(t, op.name+" must be healthy again after its own scale-up", op.name)

	annotateTargets(map[string]any{forceUptime: nil})
}

func downscaledChecks(originals map[string]int64) []check {
	return []check{
		crInt(rabbitmqClusterGVK, "rabbit", 0, "spec", "replicas"),
		crAnnotation(rabbitmqClusterGVK, "rabbit", originalReplicas, "1"),
		stsAt("rabbit-server", 0),

		crAnnotation(elasticsearchGVK, "search", eckPause, "true"),
		stsAt("search-es-default", 0),
		stsAnnotationIs("search-es-default", originalReplicas, strconv.FormatInt(originals["search-es-default"], 10)),

		crInt(redisReplicationGVK, "redis-replication", 0, "spec", "clusterSize"),
		crAnnotation(redisReplicationGVK, "redis-replication", originalReplicas, "2"),
		stsAt("redis-replication", 0),

		crInt(redisSentinelGVK, "redis-sentinel", 1, "spec", "clusterSize"),
		crAnnotation(redisSentinelGVK, "redis-sentinel", originalReplicas, "3"),
		stsAt("redis-sentinel-sentinel", 0),
		stsAnnotationIs("redis-sentinel-sentinel", originalReplicas, strconv.FormatInt(originals["redis-sentinel-sentinel"], 10)),

		crAnnotation(cnpgClusterGVK, "pg", cnpgHibernation, "on"),
		noPods("cnpg.io/cluster=pg"),

		crInt(mongoDBCommunityGVK, "mongo", 0, "spec", "members"),
		crAnnotation(mongoDBCommunityGVK, "mongo", originalReplicas, "1"),
		stsAt("mongo", 0),
		stsAnnotationIs("mongo", originalReplicas, strconv.FormatInt(originals["mongo"], 10)),

		deploymentAt("web", 0),
	}
}

func upscaledChecks(originals map[string]int64) []check {
	return []check{
		crInt(rabbitmqClusterGVK, "rabbit", 1, "spec", "replicas"),
		crAnnotation(rabbitmqClusterGVK, "rabbit", originalReplicas, ""),

		crAnnotation(elasticsearchGVK, "search", eckPause, "false"),
		stsAt("search-es-default", originals["search-es-default"]),

		crInt(redisReplicationGVK, "redis-replication", 2, "spec", "clusterSize"),
		crInt(redisSentinelGVK, "redis-sentinel", 3, "spec", "clusterSize"),
		stsAt("redis-sentinel-sentinel", originals["redis-sentinel-sentinel"]),

		crAnnotation(cnpgClusterGVK, "pg", cnpgHibernation, "off"),

		crInt(mongoDBCommunityGVK, "mongo", 1, "spec", "members"),
		stsAt("mongo", originals["mongo"]),

		deploymentAt("web", 2),
		{"no " + originalReplicas + " annotations are left", noOriginalAnnotations},
	}
}

// heartbeat is how often eventually reports a wait whose value is not changing.
const heartbeat = 30 * time.Second

// eventually polls c until it holds or timeout passes, then fails with the last
// thing it saw. It logs what it waits for, every change in what it sees, a
// heartbeat while nothing changes, and how long the wait took.
func eventually(t *testing.T, timeout time.Duration, c check) {
	t.Helper()

	ctx := context.Background()
	start := time.Now()
	deadline := start.Add(timeout)
	lastReport := start

	var (
		last   string
		waited bool
	)

	for {
		ok, seen := c.ok(ctx)
		if ok {
			recordCheck(t, c.name, seen, since(start), true)

			if waited {
				t.Logf("ok:   %s (after %s), saw: %s", c.name, since(start), seen)
			} else {
				t.Logf("ok:   %s, saw: %s", c.name, seen)
			}

			return
		}

		switch {
		case !waited:
			t.Logf("wait: %s (up to %s), now: %s", c.name, timeout, seen)
			lastReport = time.Now()
		case seen != last:
			t.Logf("      %s, now: %s", c.name, seen)
			lastReport = time.Now()
		case time.Since(lastReport) >= heartbeat:
			t.Logf("      still waiting %s for %s, now: %s", since(start), c.name, seen)
			lastReport = time.Now()
		}

		waited = true
		last = seen

		if time.Now().After(deadline) {
			recordCheck(t, c.name, last, since(start), false)
			t.Fatalf("%s: not true after %s (last seen: %s)", c.name, timeout, last)
		}

		time.Sleep(5 * time.Second)
	}
}

// pause waits d, saying why.
func pause(t *testing.T, d time.Duration, why string) {
	t.Helper()
	t.Logf("pause %s: %s", d, why)
	time.Sleep(d)
}

// describeAnnotations reads an annotation patch as "set k=v, remove k".
func describeAnnotations(annotations map[string]any) string {
	keys := make([]string, 0, len(annotations))
	for key := range annotations {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	parts := make([]string, 0, len(keys))

	for _, key := range keys {
		if annotations[key] == nil {
			parts = append(parts, "remove "+key)
		} else {
			parts = append(parts, fmt.Sprintf("set %s=%v", key, annotations[key]))
		}
	}

	return strings.Join(parts, ", ")
}

// logDownscalerOnFailure logs the end of the downscaler's logs when t failed,
// to show what it did last.
func logDownscalerOnFailure(ctx context.Context, t *testing.T) {
	t.Helper()

	if !t.Failed() {
		return
	}

	pods, err := k8s.kube.CoreV1().Pods(downscalerNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/instance=" + release,
	})
	if err != nil {
		t.Logf("downscaler logs: %v", err)
		return
	}

	tail := int64(50)

	for _, pod := range pods.Items {
		if !strings.HasPrefix(pod.Name, deploymentName) {
			continue
		}

		logs, err := k8s.kube.CoreV1().Pods(downscalerNamespace).
			GetLogs(pod.Name, &corev1.PodLogOptions{TailLines: &tail}).DoRaw(ctx)
		if err != nil {
			t.Logf("downscaler logs of %s: %v", pod.Name, err)
			continue
		}

		t.Logf("last %d log lines of %s:\n%s", tail, pod.Name, logs)
	}
}

func getCR(ctx context.Context, gvk schema.GroupVersionKind, name string) (*unstructured.Unstructured, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)

	err := k8s.client.Get(ctx, types.NamespacedName{Namespace: envNamespace, Name: name}, obj)

	return obj, err
}

func crString(ctx context.Context, gvk schema.GroupVersionKind, name string, path ...string) string {
	obj, err := getCR(ctx, gvk, name)
	if err != nil {
		return "error: " + err.Error()
	}

	value, _, _ := unstructured.NestedFieldNoCopy(obj.Object, path...)

	return fmt.Sprint(value)
}

func crInt(gvk schema.GroupVersionKind, name string, want int64, path ...string) check {
	return check{
		name: fmt.Sprintf("%s %s %s is %d", gvk.Kind, name, strings.Join(path, "."), want),
		ok: func(ctx context.Context) (bool, string) {
			seen := crString(ctx, gvk, name, path...)
			return seen == strconv.FormatInt(want, 10), seen
		},
	}
}

// crAnnotation expects an annotation value; "" means the annotation is absent.
func crAnnotation(gvk schema.GroupVersionKind, name, key, want string) check {
	return check{
		name: fmt.Sprintf("%s %s annotation %s is %q", gvk.Kind, name, key, want),
		ok: func(ctx context.Context) (bool, string) {
			obj, err := getCR(ctx, gvk, name)
			if err != nil {
				return false, err.Error()
			}

			seen := obj.GetAnnotations()[key]

			return seen == want, fmt.Sprintf("%q", seen)
		},
	}
}

func getSts(ctx context.Context, name string) (*appsv1.StatefulSet, error) {
	return k8s.kube.AppsV1().StatefulSets(envNamespace).Get(ctx, name, metav1.GetOptions{})
}

func stsReplicas(ctx context.Context, t *testing.T, name string) int64 {
	t.Helper()

	sts, err := getSts(ctx, name)
	require.NoError(t, err)

	return int64(*sts.Spec.Replicas)
}

func stsAt(name string, want int64) check {
	return check{
		name: fmt.Sprintf("StatefulSet %s has %d replicas", name, want),
		ok: func(ctx context.Context) (bool, string) {
			sts, err := getSts(ctx, name)
			if err != nil {
				return false, err.Error()
			}

			return int64(*sts.Spec.Replicas) == want, fmt.Sprintf("%d", *sts.Spec.Replicas)
		},
	}
}

func stsAnnotationIs(name, key, want string) check {
	return check{
		name: fmt.Sprintf("StatefulSet %s annotation %s is %q", name, key, want),
		ok: func(ctx context.Context) (bool, string) {
			sts, err := getSts(ctx, name)
			if err != nil {
				return false, err.Error()
			}

			return sts.Annotations[key] == want, fmt.Sprintf("%q", sts.Annotations[key])
		},
	}
}

func deploymentReplicas(ctx context.Context, t *testing.T, name string) int64 {
	t.Helper()

	deploy, err := k8s.kube.AppsV1().Deployments(envNamespace).Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)

	return int64(*deploy.Spec.Replicas)
}

func deploymentAnnotation(ctx context.Context, t *testing.T, name, key string) string {
	t.Helper()

	deploy, err := k8s.kube.AppsV1().Deployments(envNamespace).Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)

	return deploy.Annotations[key]
}

func deploymentAt(name string, want int64) check {
	return check{
		name: fmt.Sprintf("Deployment %s has %d replicas", name, want),
		ok: func(ctx context.Context) (bool, string) {
			deploy, err := k8s.kube.AppsV1().Deployments(envNamespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, err.Error()
			}

			return int64(*deploy.Spec.Replicas) == want, fmt.Sprintf("%d", *deploy.Spec.Replicas)
		},
	}
}

func noPods(selector string) check {
	return check{
		name: "no pods match " + selector,
		ok: func(ctx context.Context) (bool, string) {
			pods, err := k8s.kube.CoreV1().Pods(envNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
			if err != nil {
				return false, err.Error()
			}

			return len(pods.Items) == 0, fmt.Sprintf("%d pods", len(pods.Items))
		},
	}
}

// podsReady holds when every pod in the environment namespace is ready or has completed.
func podsReady(ctx context.Context) (bool, string) {
	pods, err := k8s.kube.CoreV1().Pods(envNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, err.Error()
	}

	if len(pods.Items) == 0 {
		return false, "no pods yet"
	}

	var waiting []string

	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodSucceeded || isReady(&pod) {
			continue
		}

		waiting = append(waiting, pod.Name+" ("+string(pod.Status.Phase)+")")
	}

	return len(waiting) == 0, "not ready: " + strings.Join(waiting, ", ")
}

func isReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}

func downscalerReady(ctx context.Context) (bool, string) {
	deploy, err := k8s.kube.AppsV1().Deployments(downscalerNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		return false, err.Error()
	}

	return deploy.Status.ReadyReplicas >= 1, fmt.Sprintf("%d ready", deploy.Status.ReadyReplicas)
}

func downscalerPod(ctx context.Context, t *testing.T) string {
	t.Helper()

	pods, err := k8s.kube.CoreV1().Pods(downscalerNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/instance=" + release,
	})
	require.NoError(t, err)

	for _, pod := range pods.Items {
		if isReady(&pod) && strings.HasPrefix(pod.Name, deploymentName) {
			return pod.Name
		}
	}

	t.Fatalf("no ready %s pod in %s", deploymentName, downscalerNamespace)

	return ""
}

func downscalerLogs(ctx context.Context, t *testing.T, pod string) string {
	t.Helper()

	logs, err := k8s.kube.CoreV1().Pods(downscalerNamespace).GetLogs(pod, &corev1.PodLogOptions{}).DoRaw(ctx)
	require.NoError(t, err)

	return string(logs)
}

func annotateNamespace(ctx context.Context, t *testing.T, annotations map[string]any) {
	t.Helper()
	t.Logf("action: namespace %s: %s", envNamespace, describeAnnotations(annotations))

	require.NoError(t, patchNamespaceAnnotations(ctx, annotations))
}

// patchNamespaceAnnotations sets each annotation, or removes it when its value is nil.
func patchNamespaceAnnotations(ctx context.Context, annotations map[string]any) error {
	patch, err := mergeAnnotations(annotations)
	if err != nil {
		return err
	}

	return k8s.client.Patch(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: envNamespace}}, patch)
}

// mergeAnnotations builds a merge patch that sets each annotation, or removes it when its value is nil.
func mergeAnnotations(annotations map[string]any) (ctrlclient.Patch, error) {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": annotations}})
	if err != nil {
		return nil, fmt.Errorf("marshal patch: %w", err)
	}

	return ctrlclient.RawPatch(types.MergePatchType, patch), nil
}

func patchCRAnnotations(ctx context.Context, gvk schema.GroupVersionKind, name string, annotations map[string]any) error {
	patch, err := mergeAnnotations(annotations)
	if err != nil {
		return err
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(envNamespace)
	obj.SetName(name)

	return k8s.client.Patch(ctx, obj, patch)
}

func patchDeploymentAnnotations(ctx context.Context, name string, annotations map[string]any) error {
	patch, err := mergeAnnotations(annotations)
	if err != nil {
		return err
	}

	return k8s.client.Patch(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: envNamespace, Name: name}}, patch)
}

func deploymentGeneration(ctx context.Context, t *testing.T, name string) int64 {
	t.Helper()

	deploy, err := k8s.kube.AppsV1().Deployments(envNamespace).Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)

	return deploy.Generation
}

func clearForceAnnotations(ctx context.Context) error {
	return patchNamespaceAnnotations(ctx, map[string]any{forceDowntime: nil, forceUptime: nil})
}

// snapshot captures every value a scan could change, to compare between scans.
func snapshot(ctx context.Context, t *testing.T) map[string]string {
	t.Helper()

	state := map[string]string{}

	for _, cr := range []struct {
		gvk  schema.GroupVersionKind
		name string
		path []string
	}{
		{rabbitmqClusterGVK, "rabbit", []string{"spec", "replicas"}},
		{redisReplicationGVK, "redis-replication", []string{"spec", "clusterSize"}},
		{redisSentinelGVK, "redis-sentinel", []string{"spec", "clusterSize"}},
		{mongoDBCommunityGVK, "mongo", []string{"spec", "members"}},
		{elasticsearchGVK, "search", nil},
		{cnpgClusterGVK, "pg", nil},
	} {
		obj, err := getCR(ctx, cr.gvk, cr.name)
		require.NoError(t, err)

		key := cr.gvk.Kind + "/" + cr.name
		if cr.path != nil {
			state[key+" "+strings.Join(cr.path, ".")] = crString(ctx, cr.gvk, cr.name, cr.path...)
		}

		for _, annotation := range []string{originalReplicas, eckPause, cnpgHibernation} {
			state[key+" "+annotation] = obj.GetAnnotations()[annotation]
		}
	}

	statefulSets, err := k8s.kube.AppsV1().StatefulSets(envNamespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)

	for _, sts := range statefulSets.Items {
		state["StatefulSet/"+sts.Name] = fmt.Sprintf("%d %s", *sts.Spec.Replicas, sts.Annotations[originalReplicas])
	}

	deployments, err := k8s.kube.AppsV1().Deployments(envNamespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)

	for _, deploy := range deployments.Items {
		state["Deployment/"+deploy.Name] = fmt.Sprintf("%d %s", *deploy.Spec.Replicas, deploy.Annotations[originalReplicas])
	}

	return state
}

// noOriginalAnnotations holds when no workload in the environment namespace still
// carries the original-replicas annotation.
func noOriginalAnnotations(ctx context.Context) (bool, string) {
	var left []string

	for _, gvk := range []schema.GroupVersionKind{
		rabbitmqClusterGVK, elasticsearchGVK, redisReplicationGVK, redisSentinelGVK, cnpgClusterGVK, mongoDBCommunityGVK,
	} {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))

		if err := k8s.client.List(ctx, list, ctrlclient.InNamespace(envNamespace)); err != nil {
			return false, err.Error()
		}

		for _, item := range list.Items {
			if _, ok := item.GetAnnotations()[originalReplicas]; ok {
				left = append(left, gvk.Kind+"/"+item.GetName())
			}
		}
	}

	statefulSets, err := k8s.kube.AppsV1().StatefulSets(envNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, err.Error()
	}

	for _, sts := range statefulSets.Items {
		// Operators copy annotations from their custom resources and never remove
		// them, so only the downscaler's own writes count here.
		if _, ok := sts.Annotations[originalReplicas]; ok && downscalerWroteAnnotation(sts.ManagedFields, originalReplicas) {
			left = append(left, "StatefulSet/"+sts.Name)
		}
	}

	deployments, err := k8s.kube.AppsV1().Deployments(envNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, err.Error()
	}

	for _, deploy := range deployments.Items {
		if _, ok := deploy.Annotations[originalReplicas]; ok {
			left = append(left, "Deployment/"+deploy.Name)
		}
	}

	return len(left) == 0, "still annotated: " + strings.Join(left, ", ")
}

// downscalerFieldManager is the field manager the downscaler's updates are recorded under.
const downscalerFieldManager = "gokubedownscaler"

// downscalerWroteAnnotation reports whether the downscaler's field manager owns
// the annotation key, as opposed to an operator that copied it from its
// custom resource.
func downscalerWroteAnnotation(fields []metav1.ManagedFieldsEntry, key string) bool {
	for _, entry := range fields {
		if entry.Manager != downscalerFieldManager || entry.FieldsV1 == nil {
			continue
		}

		var owned struct {
			Metadata struct {
				Annotations map[string]any `json:"f:annotations"`
			} `json:"f:metadata"`
		}

		if err := json.Unmarshal(entry.FieldsV1.Raw, &owned); err != nil {
			continue
		}

		if _, ok := owned.Metadata.Annotations["f:"+key]; ok {
			return true
		}
	}

	return false
}

// resultOf describes a request result for the evidence report.
func resultOf(err error, body string) string {
	if err != nil {
		return "error: " + err.Error()
	}

	if body == "" {
		return "ok"
	}

	return body
}

// presence says whether want occurs in s, for the evidence report.
func presence(s, want string) string {
	if strings.Contains(s, want) {
		return "present"
	}

	return "missing"
}

func orNone(s string) string {
	if s == "" {
		return "no changes"
	}

	return s
}

// diffSnapshots lists every value that differs between two snapshots.
func diffSnapshots(before, after map[string]string) []string {
	var changed []string

	for key, was := range before {
		if now, ok := after[key]; !ok || now != was {
			changed = append(changed, fmt.Sprintf("%s: %q → %q", key, was, now))
		}
	}

	for key, now := range after {
		if _, ok := before[key]; !ok {
			changed = append(changed, fmt.Sprintf("%s: new %q", key, now))
		}
	}

	sort.Strings(changed)

	return changed
}
