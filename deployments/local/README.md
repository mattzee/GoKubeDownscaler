# Local build, deploy and test

Use this to check a change on your own machine before you push it: first the
checks CI runs, then a run against real operators on Docker Desktop's
Kubernetes.
Run the commands from the repository root, in order.

- [0.
  Prerequisites](#0-prerequisites)
- [1.
  CI checks, locally](#1-ci-checks-locally)
- [2.
  Operators and the test environment](#2-operators-and-the-test-environment)
- [3.
  Run the downscaler](#3-run-the-downscaler)
- [4.
  Tests](#4-tests)
- [5.
  Tear down](#5-tear-down)

The [`Makefile`](Makefile) here is a different path: it deploys through Argo CD
and expects Argo CD, Gitea and a registry to be installed already.
This README
needs none of those.

## 0. Prerequisites

Install these however you prefer:

| Tool                                                                        | Used for                                         |
| --------------------------------------------------------------------------- | ------------------------------------------------ |
| Docker Desktop, with Kubernetes enabled (`docker`, `kubectl`)               | building images and the local cluster            |
| Go, at least the `go` version in `go.mod`                                   | building and testing, running from source (3a)   |
| Helm                                                                        | chart checks, installing operators and the chart |
| [helm-unittest](https://github.com/helm-unittest/helm-unittest) Helm plugin | chart unit tests (1.2)                           |
| Node.js with npm (`npx`)                                                    | the chart README check (1.2)                     |
| cosign                                                                      | image signing keys                               |
| GnuPG (`gpg`)                                                               | the chart signing check (1.4)                    |
| actionlint                                                                  | linting the workflow files (1.5)                 |

Point `kubectl` and `helm` at Docker Desktop's cluster.
Both use the current
context, so check it before running anything else:

```bash
kubectl config use-context docker-desktop
kubectl get nodes
```

## 1. CI checks, locally

These match what the workflows in `.github/workflows` run.

### 1.1 Go (pre-commit)

```bash
go build ./... && go vet ./... && go test ./...
```

### 1.2 Helm chart (Lint helm chart)

```bash
CHART=deployments/chart
helm lint --strict "$CHART"
helm lint --strict "$CHART" --values "$CHART/ci/all-features-values.yaml"
helm lint --strict "$CHART" --values "$CHART/examples/operator-workload-definitions.yaml" \
  --set 'includedResources={deployments,rabbitmqclusters,elasticsearches,redisreplications,redissentinels}'
helm unittest "$CHART"
```

The chart README is generated from `values.yaml`.
Regenerate it and check that
nothing changed:

```bash
npx --yes @bitnami/readme-generator-for-helm@2.7.2 --values "$CHART/values.yaml" --readme "$CHART/README.md"
git diff --exit-code -- "$CHART/README.md"
```

### 1.3 Images (Build and push Image, without the push)

Both images, for both architectures the workflow publishes:

```bash
docker buildx build --platform linux/amd64,linux/arm64 -f build/kubedownscaler/Dockerfile .
docker buildx build --platform linux/amd64,linux/arm64 -f build/admissioncontroller/Dockerfile .
```

### 1.4 Chart package and signature (Build and push helm chart, without the push)

This signs with a throwaway key.
It checks the packaging and signing steps;
your real key is only used by the workflow.

```bash
export GNUPGHOME=$(mktemp -d)
gpg --batch --passphrase '' --quick-generate-key "local test <test@example.invalid>" rsa2048 sign never
KEY=$(gpg --list-secret-keys --with-colons | awk -F: '/^sec/{print $5; exit}')
gpg --batch --export-secret-keys "$KEY" > "$GNUPGHOME/secring.gpg"
gpg --batch --export "$KEY" > "$GNUPGHOME/pubring.gpg"
OUT=$(mktemp -d)
helm package deployments/chart -d "$OUT" --sign --key "$KEY" --keyring "$GNUPGHOME/secring.gpg"
helm verify "$OUT"/*.tgz --keyring "$GNUPGHOME/pubring.gpg"
unset GNUPGHOME
```

### 1.5 Workflow files

```bash
actionlint
```

## 2. Operators and the test environment

This is laid out as it would be in a real cluster: each operator runs in its
own namespace, and the sample workloads run in a namespace of their own,
`downscaler-e2e`, with no operators in it.
The downscaler is limited to
`downscaler-e2e`, so it can never scale an operator.

| Namespace          | Contains                                                                            |
| ------------------ | ----------------------------------------------------------------------------------- |
| `cert-manager`     | cert-manager, which issues the RabbitMQ operator's webhook certificate              |
| `rabbitmq-system`  | RabbitMQ cluster operator                                                           |
| `elastic-system`   | ECK (Elasticsearch) operator                                                        |
| `redis-operator`   | OT Redis operator                                                                   |
| `cnpg-system`      | CloudNativePG (Postgres) operator                                                   |
| `mongodb-operator` | MongoDB Community operator                                                          |
| `downscaler-e2e`   | the sample workloads: one of each kind the operators manage, plus plain Deployments |
| `downscaler`       | the downscaler itself, when run in the cluster (3b)                                 |

RabbitMQ, Elasticsearch and Redis are scaled through the example definitions in
[`../chart/examples/operator-workload-definitions.yaml`](../chart/examples/operator-workload-definitions.yaml).
Postgres and MongoDB are built into the downscaler, so they check that built-in
scaling still works next to the definitions.

### 2.1 The test environment namespace

Create it first.
The MongoDB Community operator is configured to watch it, and
it puts the service account and role that MongoDB's own pods run with into that
namespace, which is how the operator works:

```bash
kubectl create namespace downscaler-e2e --dry-run=client -o yaml | kubectl apply -f -
```

### 2.2 cert-manager

```bash
helm repo add jetstack https://charts.jetstack.io
helm repo add elastic https://helm.elastic.co
helm repo add ot-helm https://ot-container-kit.github.io/helm-charts
helm repo add cnpg https://cloudnative-pg.github.io/charts
helm repo add mongodb https://mongodb.github.io/helm-charts
helm repo update
helm upgrade --install cert-manager jetstack/cert-manager --version v1.21.3 -n cert-manager --create-namespace --set crds.enabled=true
kubectl -n cert-manager rollout status deploy/cert-manager-webhook
```

### 2.3 Operators

Each into its own namespace.
Every command here is safe to run again:

```bash
kubectl apply -f https://github.com/rabbitmq/cluster-operator/releases/download/v2.23.3/cluster-operator.yml
helm upgrade --install elastic-operator elastic/eck-operator --version 3.5.0 -n elastic-system --create-namespace
helm upgrade --install redis-operator ot-helm/redis-operator --version 0.27.0 -n redis-operator --create-namespace
helm upgrade --install cnpg cnpg/cloudnative-pg --version 0.29.1 -n cnpg-system --create-namespace
helm upgrade --install community-operator mongodb/community-operator --version 0.13.0 -n mongodb-operator --create-namespace \
  --set operator.watchNamespace=downscaler-e2e --set database.namespace=downscaler-e2e
```

Wait for the five operators to start:

```bash
kubectl -n rabbitmq-system rollout status deploy/rabbitmq-cluster-operator
kubectl -n elastic-system rollout status statefulset/elastic-operator
kubectl -n redis-operator rollout status deploy/redis-operator
kubectl -n cnpg-system rollout status deploy/cnpg-cloudnative-pg
kubectl -n mongodb-operator rollout status deploy/mongodb-kubernetes-operator
```

### 2.4 Sample workloads

Create the sample workloads, sized to fit on a laptop.
They are in
[`test/e2e/testdata/environment.yaml`](../../test/e2e/testdata/environment.yaml),
which the end-to-end tests use too:

```bash
kubectl apply -n downscaler-e2e -f test/e2e/testdata/environment.yaml
```

Wait until everything is running (Elasticsearch takes a few minutes).
MongoDB
must also reach `Running`: the downscaler refuses to scale it to 0 until the
operator has recorded a working configuration
(`mongodb.com/v1.lastSuccessfulConfiguration`), and logs `scaling refused`
instead:

```bash
kubectl -n downscaler-e2e get rabbitmqcluster,elasticsearch,redisreplication,redissentinel,cluster.postgresql.cnpg.io,mongodbcommunity,statefulset,deployment
kubectl -n downscaler-e2e wait --for=condition=Ready pod --all --timeout=10m
kubectl -n downscaler-e2e wait mongodbcommunity/mongo --for=jsonpath='{.status.phase}'=Running --timeout=10m
```

## 3. Run the downscaler

Choose one. 3a is the quick loop for code changes. 3b also tests the chart:
RBAC, the definitions ConfigMap and the Deployment.

Both use the same resources: the four definitions, the built-in `cnpgclusters`
and `mongodbcommunities`, plus `deployments` and `statefulsets`.
Including `statefulsets` checks that the StatefulSets the
operators own are left to their operators and are not scaled a second time.

```bash
RESOURCES=deployments,statefulsets,cnpgclusters,mongodbcommunities,rabbitmqclusters,elasticsearches,redisreplications,redissentinels
```

### 3a. From source, against the cluster

As in the upstream [developing guide](../../website/content/contributing/0%20-%20developing/0%20-%20Downscaler.mdx).
The chart renders the definitions file, so it matches what an install loads:

```bash
DEFS=$(mktemp)
helm template gkd deployments/chart \
  -f deployments/chart/examples/operator-workload-definitions.yaml \
  --set "includedResources={${RESOURCES}}" \
  --show-only templates/workloaddefinitionsconfigmap.yaml |
  kubectl create --dry-run=client -f - -o jsonpath='{.data.workload-definitions\.yaml}' > "$DEFS"
cat "$DEFS"

go run ./cmd/kubedownscaler -k ~/.kube/config --debug --interval=10s \
  --namespace=downscaler-e2e --include-resources="$RESOURCES" --workload-definitions-file="$DEFS"
```

Leave it running and use a second shell for section 4.

### 3b. In the cluster, from the chart

Build the image, then install the chart from this checkout.
Docker Desktop's
cluster pulls images straight from the local Docker image store, so no registry
is needed: the chart only has to use the exact name the image was built with.

```bash
TAG=dev-$(git rev-parse --short HEAD)-$(date +%s)
docker build -f build/kubedownscaler/Dockerfile -t gokubedownscaler:$TAG .

helm upgrade --install gkd deployments/chart -n downscaler --create-namespace \
  -f deployments/chart/examples/operator-workload-definitions.yaml \
  --set "includedResources={${RESOURCES}}" \
  --set 'constrainedNamespaces={downscaler-e2e}' \
  --set image.repository=gokubedownscaler --set image.tag=$TAG \
  --set 'extraArguments={--interval=10s,--debug}'

kubectl -n downscaler rollout status deploy -l app.kubernetes.io/instance=gkd
kubectl -n downscaler logs -f deploy/gkd-go-kube-downscaler
```

The deployment name follows the chart's fullname; `kubectl -n downscaler get deploy` shows it.

## 4. Tests

These are the steps `TestDownscaler` in
[`test/e2e/downscaler_test.go`](../../test/e2e/downscaler_test.go) runs, in the
same order.
Run them by hand here, or run them all with Go:

```bash
go test -tags e2e -v -timeout 60m ./test/e2e/...
```

The Go suite does sections 2 and 3b itself first.
On a cluster already set up
from them, skip that with `E2E_SKIP_SETUP=1`; `E2E_TEARDOWN=1` removes everything
afterwards.
The Go suite runs the downscaler as in 3b, so the health checks in
4.2 need 3b; the rest also works with 3a.

A snapshot of everything the tests look at:

```bash
snap() {
  kubectl -n downscaler-e2e get rabbitmqcluster rabbit -o jsonpath='{"rabbit replicas="}{.spec.replicas}{" original="}{.metadata.annotations.downscaler/original-replicas}{"\n"}'
  kubectl -n downscaler-e2e get elasticsearch search -o jsonpath='{"search paused="}{.metadata.annotations.eck\.k8s\.elastic\.co/pause-orchestration}{"\n"}'
  kubectl -n downscaler-e2e get redisreplication redis-replication -o jsonpath='{"replication clusterSize="}{.spec.clusterSize}{" original="}{.metadata.annotations.downscaler/original-replicas}{"\n"}'
  kubectl -n downscaler-e2e get redissentinel redis-sentinel -o jsonpath='{"sentinel clusterSize="}{.spec.clusterSize}{" original="}{.metadata.annotations.downscaler/original-replicas}{"\n"}'
  kubectl -n downscaler-e2e get cluster.postgresql.cnpg.io pg -o jsonpath='{"pg hibernation="}{.metadata.annotations.cnpg\.io/hibernation}{"\n"}'
  kubectl -n downscaler-e2e get mongodbcommunity mongo -o jsonpath='{"mongo members="}{.spec.members}{" original="}{.metadata.annotations.downscaler/original-replicas}{" phase="}{.status.phase}{"\n"}'
  kubectl -n downscaler-e2e get statefulset,deployment -o custom-columns=NAME:.metadata.name,REPLICAS:.spec.replicas,ORIGINAL:.metadata.annotations.downscaler/original-replicas,GENERATION:.metadata.generation
}
snap
```

The downscaler scans every 10 seconds, so after each change allow about 30
seconds before checking (`sleep 30; snap`).
Elasticsearch, MongoDB and Postgres
take longer to come back up.

### 4.1 Preconditions

Everything in `downscaler-e2e` is ready, MongoDB is `Running` (see 2.4), and the
downscaler is ready:

```bash
kubectl -n downscaler-e2e wait --for=condition=Ready pod --all --timeout=15m
kubectl -n downscaler-e2e wait mongodbcommunity/mongo --for=jsonpath='{.status.phase}'=Running --timeout=15m
kubectl -n downscaler rollout status deploy/gkd-go-kube-downscaler
```

### 4.2 Downscaler health

The probes and metrics answer on port 8080, the log shows the four definitions
were loaded, and there are no RBAC errors:

```bash
POD=$(kubectl -n downscaler get pod -l app.kubernetes.io/instance=gkd -o jsonpath='{.items[0].metadata.name}')
kubectl get --raw "/api/v1/namespaces/downscaler/pods/$POD:8080/proxy/healthz"
kubectl get --raw "/api/v1/namespaces/downscaler/pods/$POD:8080/proxy/readyz"
kubectl get --raw "/api/v1/namespaces/downscaler/pods/$POD:8080/proxy/metrics" | grep kubedownscaler_cycle_executions_total
kubectl -n downscaler logs "$POD" | grep 'loaded workload definitions'
kubectl -n downscaler logs "$POD" | grep -ic forbidden
```

The `loaded workload definitions` line lists `rabbitmqclusters`,
`elasticsearches`, `redisreplications` and `redissentinels`.
The last command
prints `0`.

### 4.3 The whole environment

The environment's Namespace carries the window, so everything in it is scaled together.

Downscale:

```bash
kubectl annotate namespace downscaler-e2e downscaler/force-downtime=true downscaler/force-uptime- --overwrite
sleep 30; snap
```

| Workload            | Expected                                                                                                                                                                      |
| ------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `rabbit`            | `spec.replicas` 0, original 1; StatefulSet `rabbit-server` 0 through the operator                                                                                             |
| `search`            | `eck.k8s.elastic.co/pause-orchestration` `true`; StatefulSet `search-es-default` 0, scaled as a child since pausing leaves the pods running, with its original replicas saved |
| `redis-replication` | `spec.clusterSize` 0, original 2; StatefulSet `redis-replication` 0 through the operator                                                                                      |
| `redis-sentinel`    | `spec.clusterSize` 1 (the CRD minimum), original 3; StatefulSet `redis-sentinel-sentinel` 0 as a child, original saved                                                        |
| `pg` (built in)     | `cnpg.io/hibernation` `on`; the `pg-1` pod is gone, its volume kept                                                                                                           |
| `mongo` (built in)  | `spec.members` 0, original 1; StatefulSet `mongo` 0 as a child, original saved                                                                                                |
| `web`               | replicas 0, original 2                                                                                                                                                        |

Then, still downscaled:

- Excluded workload: `web-excluded` (annotated `downscaler/exclude: "true"`)
  is still at 1, with no original-replicas annotation.
- Operator-owned StatefulSets: `rabbit-server` and `redis-replication` have no
  original-replicas annotation.
  Their operators scale them, so the
  `statefulsets` scaler must leave them alone.
- Repeated scans change nothing: wait three scan intervals and compare.
  A
  sentinel whose original changed from 3 to 1 would mean it was downscaled twice.

  ```bash
  snap > /tmp/before.txt; sleep 30; snap | diff /tmp/before.txt - && echo unchanged
  ```

Upscale:

```bash
kubectl annotate namespace downscaler-e2e downscaler/force-downtime- downscaler/force-uptime=true --overwrite
sleep 30; snap
kubectl -n downscaler-e2e wait --for=condition=Ready pod --all --timeout=15m
```

Everything is back to its original value: rabbit 1, search unpaused
(`false`), replication 2, sentinel 3, pg `off` with its pod back, mongo 1,
web 2, and no original-replicas annotations are left anywhere.

Hand back to the schedule: remove the override.
Nothing changes, because no
schedule is set:

```bash
kubectl annotate namespace downscaler-e2e downscaler/force-uptime-
sleep 30; snap
```

### 4.4 Each operator on its own

The window goes on the operator's own resource instead of the Namespace, and
overrides the Namespace for that resource only.
The Go suite runs all five at
the same time; by hand, run them together or one at a time.

Note the `GENERATION` of `web` and `web-excluded` in `snap` first: neither may
change during this section, which shows nothing reached the environment level.

| Operator      | Resources                                                            | Expected down                                                | Expected up                               |
| ------------- | -------------------------------------------------------------------- | ------------------------------------------------------------ | ----------------------------------------- |
| RabbitMQ      | `rabbitmqcluster/rabbit`                                             | replicas 0, StatefulSet `rabbit-server` 0                    | replicas 1                                |
| Elasticsearch | `elasticsearch/search`                                               | paused `true`, StatefulSet `search-es-default` 0             | paused `false`, StatefulSet restored      |
| Redis         | `redisreplication/redis-replication`, `redissentinel/redis-sentinel` | clusterSize 0 and 1, StatefulSet `redis-sentinel-sentinel` 0 | clusterSize 2 and 3, StatefulSet restored |
| Postgres      | `cluster.postgresql.cnpg.io/pg`                                      | hibernation `on`, no `pg` pods                               | hibernation `off`                         |
| MongoDB       | `mongodbcommunity/mongo`                                             | members 0, StatefulSet `mongo` 0                             | members 1, StatefulSet restored           |

For each, downscale, check, upscale, check, then remove the override.
For
RabbitMQ:

```bash
kubectl -n downscaler-e2e annotate rabbitmqcluster/rabbit downscaler/force-downtime=true --overwrite
sleep 30; snap
kubectl -n downscaler-e2e annotate rabbitmqcluster/rabbit downscaler/force-downtime- downscaler/force-uptime=true --overwrite
sleep 30; snap
kubectl -n downscaler-e2e annotate rabbitmqcluster/rabbit downscaler/force-uptime-
```

The same three commands work for every resource in the table; Redis needs both of its resources annotated.

### 4.5 Expired windows

A force window can be an absolute time span.
Once it has ended it counts as
unset, so whatever is below it applies again.

An expired workload window yields to the environment. `web` carries a
`force-uptime` window that ended in 2020; the environment is forced down, so
`web` scales down:

```bash
kubectl -n downscaler-e2e annotate deployment/web downscaler/force-uptime=2020-01-01T00:00:00Z-2020-01-01T01:00:00Z --overwrite
kubectl annotate namespace downscaler-e2e downscaler/force-downtime=true --overwrite
sleep 30; kubectl -n downscaler-e2e get deployment web
kubectl annotate namespace downscaler-e2e downscaler/force-downtime- downscaler/force-uptime=true --overwrite
sleep 30; kubectl -n downscaler-e2e get deployment web
kubectl annotate namespace downscaler-e2e downscaler/force-uptime-
kubectl -n downscaler-e2e annotate deployment/web downscaler/force-uptime-
```

`web` goes to 0, then back to 2.

An expired environment window hands back to the schedule.
The environment's
schedule is `always` up, with a forced downtime for the next 90 seconds: `web`
goes down during the window and comes back on its own after it ends.
Build the
window with `date` (macOS shown; on Linux use `date -u -d '+90 seconds' +%FT%TZ`):

```bash
WINDOW="$(date -u +%FT%TZ)-$(date -u -v+90S +%FT%TZ)"
kubectl annotate namespace downscaler-e2e downscaler/uptime=always "downscaler/force-downtime=$WINDOW" --overwrite
sleep 30; kubectl -n downscaler-e2e get deployment web
sleep 90; kubectl -n downscaler-e2e get deployment web
kubectl annotate namespace downscaler-e2e downscaler/uptime- downscaler/force-downtime-
```

`web` is at 0 after the first check and back at 2 after the second.

## 5. Tear down

```bash
kubectl delete namespace downscaler-e2e
helm uninstall gkd -n downscaler; kubectl delete namespace downscaler
helm uninstall redis-operator -n redis-operator; kubectl delete namespace redis-operator
helm uninstall elastic-operator -n elastic-system; kubectl delete namespace elastic-system
helm uninstall cnpg -n cnpg-system; kubectl delete namespace cnpg-system
helm uninstall community-operator -n mongodb-operator; kubectl delete namespace mongodb-operator
kubectl delete -f https://github.com/rabbitmq/cluster-operator/releases/download/v2.23.3/cluster-operator.yml
helm uninstall cert-manager -n cert-manager; kubectl delete namespace cert-manager
```
