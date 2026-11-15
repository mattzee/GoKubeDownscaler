//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Health checks ask each operator whether its workload is actually serving,
// using the status the operator itself reports, rather than only whether the
// downscaler set the replicas back. A scale-up that leaves an operator stuck
// (e.g. a MongoDB agent that never reaches its goal state) fails here.

// operatorHealth is what "healthy" means for one operator in the test environment.
type operatorHealth struct {
	operator string  // matches operatorScenario.name
	signals  string  // the signals checked, in words, for the log
	checks   []check // each signal as a check
}

func healthChecks() []operatorHealth {
	return []operatorHealth{
		{
			operator: "rabbitmq",
			signals:  "RabbitmqCluster rabbit conditions AllReplicasReady=True and ClusterAvailable=True",
			checks:   []check{crConditionsTrue(rabbitmqClusterGVK, "RabbitMQ", "rabbit", "AllReplicasReady", "ClusterAvailable")},
		},
		{
			operator: "elasticsearch",
			signals:  "Elasticsearch search status.phase=Ready and status.health=green",
			checks: []check{crFieldsAre(elasticsearchGVK, "Elasticsearch", "search",
				fieldIs{path: "status.phase", want: "Ready"}, fieldIs{path: "status.health", want: "green"})},
		},
		{
			operator: "redis",
			signals: "RedisReplication redis-replication has elected a master; " +
				"StatefulSets redis-replication and redis-sentinel-sentinel have every replica ready",
			checks: []check{
				crFieldSet(redisReplicationGVK, "Redis", "redis-replication", "status.masterNode"),
				stsReady("Redis", "redis-replication"),
				stsReady("Redis", "redis-sentinel-sentinel"),
			},
		},
		{
			operator: "postgres",
			signals:  "Cluster pg status.phase=\"Cluster in healthy state\" and status.readyInstances = spec.instances",
			checks: []check{
				crFieldsAre(cnpgClusterGVK, "PostgreSQL", "pg", fieldIs{path: "status.phase", want: "Cluster in healthy state"}),
				cnpgInstancesReady(),
			},
		},
		{
			operator: "mongodb",
			signals:  "MongoDBCommunity mongo status.phase=Running; StatefulSet mongo has every replica ready",
			checks: []check{
				crFieldsAre(mongoDBCommunityGVK, "MongoDB", "mongo", fieldIs{path: "status.phase", want: "Running"}),
				stsReady("MongoDB", "mongo"),
			},
		},
		{
			operator: "deployments",
			signals:  "Deployments web and web-excluded have every replica ready",
			checks:   []check{deploymentReady("web"), deploymentReady("web-excluded")},
		},
	}
}

// expectHealthy logs why and what it checks, then waits for every signal of
// the named operators (all of them when none are named).
func expectHealthy(t *testing.T, why string, operators ...string) {
	t.Helper()

	var selected []operatorHealth

	for _, health := range healthChecks() {
		if len(operators) == 0 || contains(operators, health.operator) {
			selected = append(selected, health)
		}
	}

	names := make([]string, 0, len(selected))
	for _, health := range selected {
		names = append(names, health.operator)
	}

	t.Logf("health: %s: checking %s", why, strings.Join(names, ", "))

	for _, health := range selected {
		t.Logf("health: %s: %s", health.operator, health.signals)

		for _, c := range health.checks {
			eventually(t, readyTimeout, c)
		}
	}
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}

	return false
}

// fieldIs is one expected value at a dotted path in an object.
type fieldIs struct {
	path string
	want string
}

// crFieldsAre checks that every field of a custom resource has its expected value.
func crFieldsAre(gvk schema.GroupVersionKind, label, name string, fields ...fieldIs) check {
	wants := make([]string, 0, len(fields))
	for _, f := range fields {
		wants = append(wants, f.path+"="+f.want)
	}

	return check{
		name: fmt.Sprintf("%s: %s %s is healthy (%s)", label, gvk.Kind, name, strings.Join(wants, ", ")),
		ok: func(ctx context.Context) (bool, string) {
			obj, err := getCR(ctx, gvk, name)
			if err != nil {
				return false, err.Error()
			}

			ok := true
			seen := make([]string, 0, len(fields))

			for _, f := range fields {
				value, _, _ := unstructured.NestedFieldNoCopy(obj.Object, strings.Split(f.path, ".")...)
				got := fmt.Sprint(value)
				seen = append(seen, f.path+"="+got)
				ok = ok && got == f.want
			}

			return ok, strings.Join(seen, ", ")
		},
	}
}

// crFieldSet checks that a field of a custom resource is set to anything.
func crFieldSet(gvk schema.GroupVersionKind, label, name, path string) check {
	return check{
		name: fmt.Sprintf("%s: %s %s has %s set", label, gvk.Kind, name, path),
		ok: func(ctx context.Context) (bool, string) {
			obj, err := getCR(ctx, gvk, name)
			if err != nil {
				return false, err.Error()
			}

			value, found, _ := unstructured.NestedString(obj.Object, strings.Split(path, ".")...)
			if !found || value == "" {
				return false, path + " not set"
			}

			return true, path + "=" + value
		},
	}
}

// crConditionsTrue checks that each named status condition of a custom resource is True.
func crConditionsTrue(gvk schema.GroupVersionKind, label, name string, conditions ...string) check {
	return check{
		name: fmt.Sprintf("%s: %s %s conditions %s are True", label, gvk.Kind, name, strings.Join(conditions, ", ")),
		ok: func(ctx context.Context) (bool, string) {
			obj, err := getCR(ctx, gvk, name)
			if err != nil {
				return false, err.Error()
			}

			list, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
			status := map[string]string{}

			for _, item := range list {
				if condition, ok := item.(map[string]any); ok {
					status[fmt.Sprint(condition["type"])] = fmt.Sprint(condition["status"])
				}
			}

			ok := true
			seen := make([]string, 0, len(conditions))

			for _, condition := range conditions {
				got, present := status[condition]
				if !present {
					got = "missing"
				}

				seen = append(seen, condition+"="+got)
				ok = ok && got == "True"
			}

			return ok, strings.Join(seen, " ")
		},
	}
}

// cnpgInstancesReady checks that every PostgreSQL instance the Cluster asks for is ready.
func cnpgInstancesReady() check {
	return check{
		name: "PostgreSQL: Cluster pg has every instance ready",
		ok: func(ctx context.Context) (bool, string) {
			obj, err := getCR(ctx, cnpgClusterGVK, "pg")
			if err != nil {
				return false, err.Error()
			}

			want, _, _ := unstructured.NestedInt64(obj.Object, "spec", "instances")
			ready, _, _ := unstructured.NestedInt64(obj.Object, "status", "readyInstances")

			return want > 0 && ready == want, fmt.Sprintf("%d/%d instances ready", ready, want)
		},
	}
}

// stsReady checks that a StatefulSet wants replicas and has all of them ready.
func stsReady(label, name string) check {
	return check{
		name: fmt.Sprintf("%s: StatefulSet %s has every replica ready", label, name),
		ok: func(ctx context.Context) (bool, string) {
			sts, err := getSts(ctx, name)
			if err != nil {
				return false, err.Error()
			}

			want := int32(1)
			if sts.Spec.Replicas != nil {
				want = *sts.Spec.Replicas
			}

			return want > 0 && sts.Status.ReadyReplicas == want, fmt.Sprintf("%d/%d ready", sts.Status.ReadyReplicas, want)
		},
	}
}

// deploymentReady checks that a Deployment wants replicas and has all of them ready.
func deploymentReady(name string) check {
	return check{
		name: fmt.Sprintf("Deployment %s has every replica ready", name),
		ok: func(ctx context.Context) (bool, string) {
			deploy, err := k8s.kube.AppsV1().Deployments(envNamespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, err.Error()
			}

			want := int32(1)
			if deploy.Spec.Replicas != nil {
				want = *deploy.Spec.Replicas
			}

			return want > 0 && deploy.Status.ReadyReplicas == want, fmt.Sprintf("%d/%d ready", deploy.Status.ReadyReplicas, want)
		},
	}
}
