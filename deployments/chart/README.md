# Go Kubernetes Downscaler - Helm Chart

This is the official Helm Chart for the `GoKubeDownscaler`, a controller that automatically scales Kubernetes
workloads down to zero during inactive periods to optimize cloud costs.

In order to install the GoKubeDownscaler using our Helm Chart
you only need to have Helm installed on a system and have access to a Kubernetes cluster in some kind of way.

## Installation

The installation is quite simple.

All you have to is install our chart from the GitHub registry by running:

```bash
helm upgrade -i gokubedownscaler oci://ghcr.io/caas-team/charts/go-kube-downscaler
```

You can also alternatively add our helm chart repo in order to install the chart.

```bash
helm repo add caas-team https://caas-team.github.io/helm-charts/
```

This will add all of our published Helm Charts to a local Helm repo named caas-team.

After that you just need to run the following command:

```bash
helm install go-kube-downscaler caas-team/go-kube-downscaler
```

## Customizing Your Installation

Our Helm Chart offers a lot of customizable values for your installation of the GoKubeDownscaler.

You can find information on how to adjust the chart to your needs on our [helm documentation page](https://kube-downscaler.io/docs/helm-chart).

## Parameters

### Common Parameters

| Name               | Description                                      | Value |
| ------------------ | ------------------------------------------------ | ----- |
| `nameOverride`     | Override the chart name used in resource names   | `""`  |
| `fullnameOverride` | Override the full resource name                  | `""`  |
| `commonLabels`     | Labels added to every resource the chart creates | `{}`  |

### Downscaler Controller

| Name                                          | Description                                                                                 | Value                                |
| --------------------------------------------- | ------------------------------------------------------------------------------------------- | ------------------------------------ |
| `replicaCount`                                | Number of controller replicas. Leader election is turned on when this is greater than 1     | `1`                                  |
| `revisionHistoryLimit`                        | Number of old ReplicaSets kept for rollback                                                 | `3`                                  |
| `updateStrategy.type`                         | Deployment update strategy                                                                  | `RollingUpdate`                      |
| `updateStrategy.rollingUpdate.maxSurge`       | Extra pods during a rollout                                                                 | `0`                                  |
| `updateStrategy.rollingUpdate.maxUnavailable` | Pods that may be down during a rollout                                                      | `1`                                  |
| `image.repository`                            | Controller image repository                                                                 | `ghcr.io/caas-team/gokubedownscaler` |
| `image.tag`                                   | Controller image tag. Defaults to the chart appVersion                                      | `""`                                 |
| `image.digest`                                | Controller image digest (sha256:...). Takes precedence over the tag when set                | `""`                                 |
| `image.pullPolicy`                            | Controller image pull policy                                                                | `IfNotPresent`                       |
| `imagePullSecrets`                            | Secrets for pulling the controller image, e.g. `[{name: regcred}]`                          | `[]`                                 |
| `arguments`                                   | Additional command-line arguments, kept for compatibility with extraArguments               | `nil`                                |
| `extraArguments`                              | Additional command-line arguments, e.g. `["--interval=60s"]`                                | `nil`                                |
| `includedResources`                           | Kinds the downscaler manages. See the comments in values.yaml for every supported kind      | `["deployments"]`                    |
| `constrainedNamespaces`                       | Restrict the downscaler to these namespaces, with namespaced Roles instead of a ClusterRole | `[]`                                 |
| `excludedNamespaces`                          | Namespaces the downscaler never touches. Empty means the release namespace and kube-system  | `["kube-downscaler","kube-system"]`  |
| `configMap.name`                              | Name of the configuration ConfigMap. Defaults to the full name                              | `""`                                 |
| `configMap.extraConfig`                       | Extra lines for the ConfigMap, e.g. `DOWNSCALE_PERIOD: "Mon-Sun 19:00-20:00 Europe/Berlin"` | `""`                                 |
| `forceRestartOnConfigChange`                  | Roll the pods when the ConfigMap changes                                                    | `true`                               |
| `logging.json`                                | Write logs as JSON, one object per line, for Loki and other log pipelines                   | `true`                               |
| `logging.debug`                               | Log at debug level                                                                          | `false`                              |
| `extraEnv`                                    | Extra environment variables for the controller container                                    | `[]`                                 |

### Service Account and RBAC

| Name                                          | Description                                                                        | Value   |
| --------------------------------------------- | ---------------------------------------------------------------------------------- | ------- |
| `serviceAccount.create`                       | Create a ServiceAccount for the controller                                         | `true`  |
| `serviceAccount.name`                         | ServiceAccount name. Defaults to the full name when created, otherwise `default`   | `""`    |
| `serviceAccount.annotations`                  | Annotations for the ServiceAccount, e.g. an IRSA role                              | `{}`    |
| `serviceAccount.automountServiceAccountToken` | Mount the API token on the ServiceAccount. The pod mounts it explicitly either way | `false` |

### Pod Scheduling and Security

| Name                                             | Description                                                                      | Value            |
| ------------------------------------------------ | -------------------------------------------------------------------------------- | ---------------- |
| `priorityClassName`                              | Priority class for the controller pods                                           | `""`             |
| `terminationGracePeriodSeconds`                  | Time the controller gets to finish its cycle and flush traces on shutdown        | `30`             |
| `podAnnotations`                                 | Annotations for the controller pods                                              | `{}`             |
| `podLabels`                                      | Labels for the controller pods                                                   | `{}`             |
| `podSecurityContext.runAsNonRoot`                | Refuse to start as root                                                          | `true`           |
| `podSecurityContext.runAsUser`                   | User ID the container runs as                                                    | `1000`           |
| `podSecurityContext.runAsGroup`                  | Group ID the container runs as                                                   | `1000`           |
| `podSecurityContext.fsGroup`                     | Group ID for mounted volumes                                                     | `1000`           |
| `podSecurityContext.supplementalGroups[0]`       | Supplemental group ID                                                            | `1000`           |
| `podSecurityContext.seccompProfile.type`         | Seccomp profile, required by the restricted Pod Security Standard                | `RuntimeDefault` |
| `securityContext.readOnlyRootFilesystem`         | Mount the root filesystem read-only                                              | `true`           |
| `securityContext.allowPrivilegeEscalation`       | Allow privilege escalation                                                       | `false`          |
| `securityContext.privileged`                     | Run privileged                                                                   | `false`          |
| `securityContext.capabilities.drop`              | Linux capabilities to drop                                                       | `["ALL"]`        |
| `resources.requests.cpu`                         | CPU request                                                                      | `25m`            |
| `resources.requests.memory`                      | Memory request                                                                   | `64Mi`           |
| `resources.limits.memory`                        | Memory limit                                                                     | `128Mi`          |
| `nodeSelector`                                   | Node labels for scheduling                                                       | `{}`             |
| `tolerations`                                    | Tolerations for scheduling                                                       | `[]`             |
| `affinity`                                       | Affinity rules. When empty and replicaCount > 1, replicas prefer different nodes | `{}`             |
| `topologySpreadConstraints`                      | Topology spread constraints for the controller pods                              | `[]`             |
| `podDisruptionBudget.enabled`                    | Create a PodDisruptionBudget for the controller                                  | `true`           |
| `podDisruptionBudget.minAvailable`               | Minimum available pods. Leave empty to use maxUnavailable                        | `""`             |
| `podDisruptionBudget.maxUnavailable`             | Maximum unavailable pods. 1 lets a single replica be drained                     | `1`              |
| `podDisruptionBudget.unhealthyPodEvictionPolicy` | Evict pods that are not ready even when the budget is exhausted                  | `AlwaysAllow`    |

### Port and Health Probes

| Name                                              | Description                                                                                  | Value  |
| ------------------------------------------------- | -------------------------------------------------------------------------------------------- | ------ |
| `port`                                            | Container port for /healthz, /readyz and /metrics                                            | `8080` |
| `healthProbes.livenessStaleAfter`                 | Time without a completed cycle before liveness fails. Empty means ten intervals, at least 5m | `""`   |
| `healthProbes.readinessProbe.enabled`             | Enable the readiness probe on /readyz                                                        | `true` |
| `healthProbes.readinessProbe.initialDelaySeconds` | Readiness probe initial delay                                                                | `0`    |
| `healthProbes.readinessProbe.periodSeconds`       | Readiness probe period                                                                       | `10`   |
| `healthProbes.readinessProbe.timeoutSeconds`      | Readiness probe timeout                                                                      | `2`    |
| `healthProbes.readinessProbe.failureThreshold`    | Readiness probe failure threshold                                                            | `3`    |
| `healthProbes.readinessProbe.successThreshold`    | Readiness probe success threshold                                                            | `1`    |
| `healthProbes.livenessProbe.enabled`              | Enable the liveness probe on /healthz                                                        | `true` |
| `healthProbes.livenessProbe.initialDelaySeconds`  | Liveness probe initial delay                                                                 | `0`    |
| `healthProbes.livenessProbe.periodSeconds`        | Liveness probe period                                                                        | `20`   |
| `healthProbes.livenessProbe.timeoutSeconds`       | Liveness probe timeout                                                                       | `2`    |
| `healthProbes.livenessProbe.failureThreshold`     | Liveness probe failure threshold                                                             | `3`    |
| `healthProbes.livenessProbe.successThreshold`     | Liveness probe success threshold                                                             | `1`    |
| `healthProbes.startupProbe.enabled`               | Enable the startup probe on /readyz                                                          | `true` |
| `healthProbes.startupProbe.initialDelaySeconds`   | Startup probe initial delay                                                                  | `0`    |
| `healthProbes.startupProbe.periodSeconds`         | Startup probe period                                                                         | `5`    |
| `healthProbes.startupProbe.timeoutSeconds`        | Startup probe timeout                                                                        | `2`    |
| `healthProbes.startupProbe.failureThreshold`      | Startup probe failure threshold                                                              | `60`   |
| `healthProbes.startupProbe.successThreshold`      | Startup probe success threshold                                                              | `1`    |

### Network Policy

| Name                                                              | Description                                                                         | Value           |
| ----------------------------------------------------------------- | ----------------------------------------------------------------------------------- | --------------- |
| `networkPolicy.enabled`                                           | Create a NetworkPolicy for the controller pods                                      | `true`          |
| `networkPolicy.apiServer.ports[0]`                                | API server port behind the kubernetes Service (managed control planes)              | `443`           |
| `networkPolicy.apiServer.ports[1]`                                | API server port on self-managed control planes                                      | `6443`          |
| `networkPolicy.apiServer.cidrs`                                   | CIDRs the API server is reached at. Narrow this to your control plane where you can | `["0.0.0.0/0"]` |
| `networkPolicy.dns.namespaceSelector.kubernetes.io/metadata.name` | Namespace label of the cluster DNS                                                  | `kube-system`   |
| `networkPolicy.dns.podSelector.k8s-app`                           | Pod label of the cluster DNS                                                        | `kube-dns`      |
| `networkPolicy.metricsFrom`                                       | Peers allowed to reach the port. Empty allows any source, on that port only         | `[]`            |
| `networkPolicy.extraEgress`                                       | Extra egress rules, e.g. to a tracing collector outside tracing.collector           | `[]`            |

### Metrics

| Name                               | Description                                                                              | Value      |
| ---------------------------------- | ---------------------------------------------------------------------------------------- | ---------- |
| `metrics.enabled`                  | Serve Prometheus metrics on /metrics                                                     | `true`     |
| `metrics.service.enabled`          | Create a Service in front of the metrics port, for the ServiceMonitor                    | `true`     |
| `metrics.service.annotations`      | Annotations for the metrics Service                                                      | `{}`       |
| `serviceMonitor.enabled`           | Create a Prometheus Operator ServiceMonitor. Needs metrics.service.enabled               | `false`    |
| `serviceMonitor.namespace`         | Namespace for the ServiceMonitor. Defaults to the release namespace                      | `""`       |
| `serviceMonitor.interval`          | Scrape interval                                                                          | `1m`       |
| `serviceMonitor.scrapeTimeout`     | Scrape timeout                                                                           | `10s`      |
| `serviceMonitor.additionalLabels`  | Labels your Prometheus selects ServiceMonitors by, e.g. `release: kube-prometheus-stack` | `{}`       |
| `serviceMonitor.relabelings`       | Relabelings applied before scraping                                                      | `[]`       |
| `serviceMonitor.metricRelabelings` | Relabelings applied to scraped samples                                                   | `[]`       |
| `podMonitor.enabled`               | Create a Prometheus Operator PodMonitor instead of a ServiceMonitor                      | `false`    |
| `podMonitor.namespace`             | Namespace for the PodMonitor. Defaults to the release namespace                          | `""`       |
| `podMonitor.interval`              | Scrape interval                                                                          | `1m`       |
| `podMonitor.scrapeTimeout`         | Scrape timeout                                                                           | `10s`      |
| `podMonitor.additionalLabels`      | Labels your Prometheus selects PodMonitors by                                            | `{}`       |
| `podMonitor.relabelings`           | Relabelings applied before scraping                                                      | `[]`       |
| `podMonitor.metricRelabelings`     | Relabelings applied to scraped samples                                                   | `[]`       |
| `prometheusRule.enabled`           | Create a PrometheusRule with the downscaler alerts                                       | `false`    |
| `prometheusRule.namespace`         | Namespace for the PrometheusRule. Defaults to the release namespace                      | `""`       |
| `prometheusRule.additionalLabels`  | Labels your Prometheus selects rules by                                                  | `{}`       |
| `prometheusRule.cycleStaleFor`     | Alert when no cycle completed for this long                                              | `10m`      |
| `prometheusRule.severity.critical` | Severity label for the down and stale alerts                                             | `critical` |
| `prometheusRule.severity.warning`  | Severity label for the error and restart alerts                                          | `warning`  |
| `prometheusRule.additionalRules`   | Extra rules appended to the group                                                        | `[]`       |

### Tracing

| Name                                  | Description                                                                  | Value                      |
| ------------------------------------- | ---------------------------------------------------------------------------- | -------------------------- |
| `tracing.enabled`                     | Export OpenTelemetry traces                                                  | `false`                    |
| `tracing.endpoint`                    | OTLP gRPC endpoint, e.g. `http://alloy.monitoring.svc:4317`                  | `""`                       |
| `tracing.insecure`                    | Send spans without TLS                                                       | `true`                     |
| `tracing.serviceName`                 | service.name on the spans. Defaults to the full name                         | `""`                       |
| `tracing.sampler`                     | Sampler (OTEL_TRACES_SAMPLER)                                                | `parentbased_traceidratio` |
| `tracing.samplerArg`                  | Sampler argument (OTEL_TRACES_SAMPLER_ARG), the ratio for the ratio samplers | `1.0`                      |
| `tracing.resourceAttributes`          | Extra resource attributes, e.g. `{deployment.environment: dev}`              | `{}`                       |
| `tracing.collector.namespaceSelector` | Namespace of the collector, opened in the NetworkPolicy                      | `{}`                       |
| `tracing.collector.podSelector`       | Pods of the collector, opened in the NetworkPolicy                           | `{}`                       |
| `tracing.collector.port`              | Collector port, opened in the NetworkPolicy                                  | `4317`                     |

### Extra Resources

| Name                                | Description                                                              | Value   |
| ----------------------------------- | ------------------------------------------------------------------------ | ------- |
| `deployExtraResources.gatewayClass` | Create the downscaler GatewayClass (with gateways in includedResources)  | `false` |
| `deployExtraResources.ingressClass` | Create the downscaler IngressClass (with ingresses in includedResources) | `false` |

### Annotation Compliance

| Name                                                               | Description                                                                 | Value      |
| ------------------------------------------------------------------ | --------------------------------------------------------------------------- | ---------- |
| `annotationsCompliance.mutateUnauthorizedAnnotationsAddition`      | Strip downscaler annotations added by unauthorised users (Kubernetes 1.34+) | `false`    |
| `annotationsCompliance.failurePolicy`                              | Admission policy failure policy                                             | `Ignore`   |
| `annotationsCompliance.validationActions`                          | Admission policy validation actions                                         | `["Deny"]` |
| `annotationsCompliance.authorizedNamespacesToServiceAccountsRegex` | Map of namespace to service-account regexes allowed to set annotations      | `{}`       |
| `annotationsCompliance.authorizedUsersRegex`                       | Users allowed to set downscaler annotations                                 | `[]`       |
| `annotationsCompliance.authorizedGroupsRegex`                      | Groups allowed to set downscaler annotations                                | `[]`       |
| `annotationsCompliance.onWorkloads.enabled`                        | Enforce on workloads                                                        | `false`    |
| `annotationsCompliance.onWorkloads.preventRemoval`                 | Also block removal of the annotations on workloads                          | `false`    |
| `annotationsCompliance.onNamespace.enabled`                        | Enforce on namespaces                                                       | `false`    |
| `annotationsCompliance.onNamespace.preventRemoval`                 | Also block removal of the annotations on namespaces                         | `false`    |

### Webhook Controller

| Name                                                                | Description                                                                                                 | Value                                        |
| ------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------- | -------------------------------------------- |
| `webhookController.enabled`                                         | Deploy the admission webhook controller                                                                     | `false`                                      |
| `webhookController.image.repository`                                | Webhook image repository                                                                                    | `ghcr.io/caas-team/gokubedownscaler-webhook` |
| `webhookController.image.pullPolicy`                                | Webhook image pull policy                                                                                   | `IfNotPresent`                               |
| `webhookController.image.tag`                                       | Webhook image tag. Defaults to the chart appVersion                                                         | `""`                                         |
| `webhookController.replicaCount`                                    | Webhook replicas                                                                                            | `1`                                          |
| `webhookController.priorityClassName`                               | Priority class for the webhook pods                                                                         | `""`                                         |
| `webhookController.serviceAccount.create`                           | Create a ServiceAccount for the webhook                                                                     | `true`                                       |
| `webhookController.serviceAccount.annotations`                      | Annotations for the webhook ServiceAccount                                                                  | `{}`                                         |
| `webhookController.serviceAccount.name`                             | Webhook ServiceAccount name                                                                                 | `""`                                         |
| `webhookController.extraArguments`                                  | Additional webhook arguments                                                                                | `[]`                                         |
| `webhookController.imagePullSecrets`                                | Secrets for pulling the webhook image                                                                       | `[]`                                         |
| `webhookController.podAnnotations`                                  | Annotations for the webhook pods                                                                            | `{}`                                         |
| `webhookController.podLabels`                                       | Labels for the webhook pods                                                                                 | `{}`                                         |
| `webhookController.extraEnv`                                        | Extra environment variables for the webhook                                                                 | `[]`                                         |
| `webhookController.securityContext.readOnlyRootFilesystem`          | Mount the root filesystem read-only                                                                         | `true`                                       |
| `webhookController.securityContext.allowPrivilegeEscalation`        | Allow privilege escalation                                                                                  | `false`                                      |
| `webhookController.securityContext.privileged`                      | Run privileged                                                                                              | `false`                                      |
| `webhookController.securityContext.capabilities.drop`               | Linux capabilities to drop                                                                                  | `["ALL"]`                                    |
| `webhookController.healthProbes.readinessProbe.enabled`             | Enable the readiness probe                                                                                  | `true`                                       |
| `webhookController.healthProbes.readinessProbe.failureThreshold`    | Readiness probe failure threshold                                                                           | `3`                                          |
| `webhookController.healthProbes.readinessProbe.initialDelaySeconds` | Readiness probe initial delay                                                                               | `5`                                          |
| `webhookController.healthProbes.readinessProbe.periodSeconds`       | Readiness probe period                                                                                      | `3`                                          |
| `webhookController.healthProbes.readinessProbe.timeoutSeconds`      | Readiness probe timeout                                                                                     | `2`                                          |
| `webhookController.healthProbes.readinessProbe.successThreshold`    | Readiness probe success threshold                                                                           | `1`                                          |
| `webhookController.healthProbes.livenessProbe.enabled`              | Enable the liveness probe                                                                                   | `true`                                       |
| `webhookController.healthProbes.livenessProbe.failureThreshold`     | Liveness probe failure threshold                                                                            | `3`                                          |
| `webhookController.healthProbes.livenessProbe.initialDelaySeconds`  | Liveness probe initial delay                                                                                | `5`                                          |
| `webhookController.healthProbes.livenessProbe.periodSeconds`        | Liveness probe period                                                                                       | `3`                                          |
| `webhookController.healthProbes.livenessProbe.timeoutSeconds`       | Liveness probe timeout                                                                                      | `2`                                          |
| `webhookController.healthProbes.livenessProbe.successThreshold`     | Liveness probe success threshold                                                                            | `1`                                          |
| `webhookController.healthProbes.startupProbe.enabled`               | Enable the startup probe                                                                                    | `true`                                       |
| `webhookController.healthProbes.startupProbe.failureThreshold`      | Startup probe failure threshold                                                                             | `3`                                          |
| `webhookController.healthProbes.startupProbe.initialDelaySeconds`   | Startup probe initial delay                                                                                 | `5`                                          |
| `webhookController.healthProbes.startupProbe.periodSeconds`         | Startup probe period                                                                                        | `3`                                          |
| `webhookController.healthProbes.startupProbe.timeoutSeconds`        | Startup probe timeout                                                                                       | `2`                                          |
| `webhookController.healthProbes.startupProbe.successThreshold`      | Startup probe success threshold                                                                             | `1`                                          |
| `webhookController.resources.limits.cpu`                            | Webhook CPU limit                                                                                           | `150m`                                       |
| `webhookController.resources.limits.memory`                         | Webhook memory limit                                                                                        | `128Mi`                                      |
| `webhookController.resources.requests.cpu`                          | Webhook CPU request                                                                                         | `50m`                                        |
| `webhookController.resources.requests.memory`                       | Webhook memory request                                                                                      | `64Mi`                                       |
| `webhookController.nodeSelector`                                    | Node labels for scheduling the webhook                                                                      | `{}`                                         |
| `webhookController.tolerations`                                     | Tolerations for the webhook                                                                                 | `[]`                                         |
| `webhookController.affinity`                                        | Affinity rules for the webhook                                                                              | `{}`                                         |
| `webhookController.mutatingWebhookConfiguration.timeoutSeconds`     | Webhook call timeout                                                                                        | `10`                                         |
| `webhookController.mutatingWebhookConfiguration.failurePolicy`      | Webhook failure policy                                                                                      | `Ignore`                                     |
| `webhookController.podDisruptionBudget.minAvailable`                | Webhook PDB minimum available                                                                               | `""`                                         |
| `webhookController.podDisruptionBudget.maxUnavailable`              | Webhook PDB maximum unavailable                                                                             | `""`                                         |
| `webhookController.clusterDomain`                                   | Cluster DNS domain for the webhook certificate                                                              | `cluster.local`                              |
| `webhookController.certManager.enabled`                             | Issue the webhook certificate with cert-manager                                                             | `false`                                      |
| `webhookController.certManager.duration`                            | Certificate lifetime                                                                                        | `8760h0m0s`                                  |
| `webhookController.certManager.renewBefore`                         | Renew this long before expiry                                                                               | `5840h0m0s`                                  |
| `webhookController.certManager.secretTemplate`                      | Template for the certificate Secret                                                                         | `{}`                                         |
| `webhookController.certManager.ca.generate`                         | Generate a CA. When false, the Secret needs the `cert-manager.io/allow-direct-injection: "true"` annotation | `true`                                       |
| `webhookController.certManager.ca.secretName`                       | Secret holding the CA                                                                                       | `kubedownscaler-ca`                          |
| `webhookController.certManager.issuer.generate`                     | Create the Issuer. When false, name an existing Issuer or ClusterIssuer below                               | `true`                                       |
| `webhookController.certManager.issuer.name`                         | Existing issuer name                                                                                        | `foo-org-ca`                                 |
| `webhookController.certManager.issuer.kind`                         | Existing issuer kind                                                                                        | `ClusterIssuer`                              |
| `webhookController.certManager.issuer.group`                        | Existing issuer API group                                                                                   | `cert-manager.io`                            |
| `webhookController.podMonitor.enabled`                              | Create a PodMonitor for the webhook                                                                         | `false`                                      |
| `webhookController.podMonitor.interval`                             | Scrape interval                                                                                             | `1m`                                         |
| `webhookController.podMonitor.scrapeTimeout`                        | Scrape timeout                                                                                              | `10s`                                        |
| `webhookController.podMonitor.namespace`                            | Namespace for the PodMonitor. Defaults to the release namespace                                             | `""`                                         |
| `webhookController.podMonitor.additionalLabels`                     | Labels your Prometheus selects PodMonitors by                                                               | `{}`                                         |
| `webhookController.podMonitor.relabelings`                          | Relabelings applied before scraping                                                                         | `[]`                                         |
| `webhookController.podMonitor.metricRelabelings`                    | Relabelings applied to scraped samples                                                                      | `[]`                                         |

The table is generated from the `## @param` comments in `values.yaml` with
[readme-generator-for-helm](https://github.com/bitnami/readme-generator-for-helm).
Edit the comments, not the table, then regenerate:

```bash
npx @bitnami/readme-generator-for-helm@2.7.2 --values deployments/chart/values.yaml --readme deployments/chart/README.md
```
