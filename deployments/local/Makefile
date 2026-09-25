# Local rig for the downscaler: docker-desktop Kubernetes, Argo CD, Gitea and a
# registry at localhost:5000. Every kubectl call is pinned to docker-desktop.
#
# The Application installs the chart as a published package, with values from
# this branch in Gitea. Two ways to feed it:
#
#   make registry-secrets               once: pull access to CHART_REGISTRY for Argo CD and the kubelet
#   make deploy-published VERSION=<v>   a chart (and its image) already published to CHART_REGISTRY
#   make deploy     build here: image to localhost:5000, chart to Gitea's Helm registry, then deploy
#   make image      build and push the controller image
#   make chart      lint, package and publish the chart (appVersion = image tag)
#   make undeploy   delete the Application; its finalizer removes what it deployed
#   make status     Application, chart version, pods and the image they run
#   make logs       follow the controller logs
#   make probe      hit /healthz, /readyz and /metrics through a port-forward
#
# Machine-specific settings (your registry, secret names) go in local.mk next to
# this file, which git ignores, e.g.
#   CHART_REGISTRY = registry.example.com
#   CHART_REGISTRY_HOSTS = registry.example.com releases.registry.example.com

-include $(dir $(lastword $(MAKEFILE_LIST)))local.mk

KCTX       ?= docker-desktop
KUBECTL    := kubectl --context $(KCTX)
ARGO_NS    ?= argocd
APP        ?= downscaler
NS         ?= downscaler
REGISTRY   ?= localhost:5000
IMAGE      ?= $(REGISTRY)/gokubedownscaler
TAG        ?= $(shell date +%s)
# evaluate once, not on every use
override TAG := $(TAG)
BRANCH     ?= parking-rig
GITEA_URL  ?= http://localhost:13000
GITEA_USER ?= gitea_admin
GITEA_PASS ?= gitea-local-pw
GITEA_OWNER ?= local
GITEA_REPO ?= $(GITEA_OWNER)/GoKubeDownscaler
# OCI registry that published charts and images come from (publish-chart.sh pushes there)
CHART_REGISTRY ?=
CHART_REGISTRY_HOSTS ?= $(CHART_REGISTRY)
PUBLISHED_IMAGE ?= $(CHART_REGISTRY)/gokubedownscaler
PULL_SECRET ?= registry-pull
GITEA_HELM_INCLUSTER ?= http://gitea-http.gitea.svc.cluster.local:3000/api/packages/$(GITEA_OWNER)/helm
REPO_ROOT  := $(shell git rev-parse --show-toplevel)
CHART_DIR  := $(REPO_ROOT)/deployments/chart
HERE       := $(REPO_ROOT)/deployments/local
CHART_NAME := $(shell sed -n 's/^name: *//p' $(CHART_DIR)/Chart.yaml)
CHART_BASE := $(shell sed -n 's/^version: *//p' $(CHART_DIR)/Chart.yaml)
# a unique, valid SemVer per build: 1.3.4-rig.<image tag>
CHART_VERSION ?= $(CHART_BASE)-rig.$(TAG)

# Replace the whole Application rather than apply, so values patched in by hand
# during a test do not survive a redeploy (apply keeps fields it did not set).
define replace-app
$(KUBECTL) replace -f $(HERE)/.application.yaml 2>/dev/null || $(KUBECTL) create -f $(HERE)/.application.yaml
endef

.PHONY: context registry-secrets repo image chart source apply apply-published wait deploy deploy-published undeploy status logs probe

context:
	@$(KUBECTL) version >/dev/null 2>&1 || { echo "kube context $(KCTX) is not reachable"; exit 1; }

image:
	docker build -f $(REPO_ROOT)/build/kubedownscaler/Dockerfile -t $(IMAGE):$(TAG) $(REPO_ROOT)
	docker push $(IMAGE):$(TAG)
	@echo $(TAG) > $(HERE)/.image-tag

# Package with appVersion = the image tag, so the chart version pins the image.
chart:
	@tag=$$(cat $(HERE)/.image-tag 2>/dev/null) || { echo "run make image first"; exit 1; }; \
	version=$(CHART_BASE)-rig.$$tag; \
	helm lint --strict $(CHART_DIR) --values $(HERE)/values-local.yaml && \
	rm -rf $(HERE)/.charts && mkdir -p $(HERE)/.charts && \
	helm package $(CHART_DIR) --version $$version --app-version $$tag -d $(HERE)/.charts && \
	curl -fsS --user $(GITEA_USER):$(GITEA_PASS) -X POST \
	  --upload-file $(HERE)/.charts/$(CHART_NAME)-$$version.tgz \
	  $(GITEA_URL)/api/packages/$(GITEA_OWNER)/helm/api/charts && \
	echo $$version > $(HERE)/.chart-version && \
	echo "published $(CHART_NAME) $$version"

# The values file is read from Git, so push what is checked out.
source:
	@git -C $(REPO_ROOT) diff --quiet HEAD -- deployments/local || echo "warning: uncommitted changes in deployments/local are not deployed"
	git -C $(REPO_ROOT) push --force \
	  "$(subst ://,://$(GITEA_USER):$(GITEA_PASS)@,$(GITEA_URL))/$(GITEA_REPO).git" HEAD:refs/heads/$(BRANCH)

# One-off: pull access to the registry for Argo CD (charts) and the kubelet
# (images), from the login stored by `docker login $(CHART_REGISTRY)`. One Argo CD
# repository Secret per host (named after the host's first label) and one
# dockerconfigjson Secret covering all hosts. Nothing is printed.
registry-secrets: context need-registry
	@auth=$$(jq -r '.auths["$(CHART_REGISTRY)"].auth // empty' $$HOME/.docker/config.json); \
	test -n "$$auth" || { echo "no stored login for $(CHART_REGISTRY): docker login $(CHART_REGISTRY)"; exit 1; }; \
	userpass=$$(echo "$$auth" | base64 -d); user=$${userpass%%:*}; pass=$${userpass#*:}; \
	$(KUBECTL) create namespace $(NS) --dry-run=client -o yaml | $(KUBECTL) apply --server-side -f - >/dev/null; \
	for host in $(CHART_REGISTRY_HOSTS); do \
	  name=$${host%%.*}; \
	  $(KUBECTL) -n $(ARGO_NS) create secret generic $$name --from-literal=type=helm --from-literal=name=$$name \
	    --from-literal=url=$$host --from-literal=enableOCI=true --from-literal=username="$$user" --from-literal=password="$$pass" \
	    --dry-run=client -o yaml | $(KUBECTL) label -f - --local -o yaml argocd.argoproj.io/secret-type=repository \
	    | $(KUBECTL) apply --server-side --force-conflicts -f - >/dev/null || exit 1; \
	done; \
	jq -n --arg a "$$auth" '{auths: ([$$ARGS.positional[] | {(.): {auth: $$a}}] | add)}' --args $(CHART_REGISTRY_HOSTS) \
	  | $(KUBECTL) -n $(NS) create secret generic $(PULL_SECRET) --type=kubernetes.io/dockerconfigjson \
	    --from-file=.dockerconfigjson=/dev/stdin --dry-run=client -o yaml \
	  | $(KUBECTL) apply --server-side --force-conflicts -f - >/dev/null && \
	echo "Argo CD: $(foreach h,$(CHART_REGISTRY_HOSTS),$(ARGO_NS)/$(firstword $(subst ., ,$(h))))  images: $(NS)/$(PULL_SECRET)"

need-registry:
	@test -n "$(CHART_REGISTRY)" || { echo "set CHART_REGISTRY (in local.mk or on the command line)"; exit 1; }

repo: context need-registry
	@$(KUBECTL) -n $(ARGO_NS) get secret $(firstword $(subst ., ,$(CHART_REGISTRY))) >/dev/null 2>&1 || { echo "Argo CD has no credentials for $(CHART_REGISTRY) yet: run make registry-secrets"; exit 1; }

# Point the Application at the chart `make chart` put in Gitea and the image
# `make image` put in the local registry.
apply: context
	@version=$$(cat $(HERE)/.chart-version 2>/dev/null) || { echo "run make chart first"; exit 1; }; \
	yq ".spec.source.repoURL = \"$(GITEA_HELM_INCLUSTER)\" | .spec.source.targetRevision = \"$$version\" \
	  | .spec.source.helm.valuesObject = load(\"$(HERE)/values-local.yaml\") \
	  | .spec.source.helm.valuesObject.image.repository = \"$(IMAGE)\"" \
	  $(HERE)/argocd-application.yaml > $(HERE)/.application.yaml && \
	$(call replace-app) && \
	$(KUBECTL) -n $(ARGO_NS) annotate application $(APP) argocd.argoproj.io/refresh=hard --overwrite

# Install a chart version straight from CHART_REGISTRY (VERSION = chart version = image
# tag), with values-local.yaml inlined into the Application. No Git involved.
apply-published: context repo
	@test -n "$(VERSION)" || { echo "usage: make deploy-published VERSION=<chart version>"; exit 1; }
	yq '.spec.source.repoURL = "$(CHART_REGISTRY)" | .spec.source.targetRevision = "$(VERSION)" \
	  | .spec.source.helm.valuesObject = load("$(HERE)/values-local.yaml") \
	  | .spec.source.helm.valuesObject.image.repository = "$(PUBLISHED_IMAGE)" \
	  | .spec.source.helm.valuesObject.imagePullSecrets = [{"name": "$(PULL_SECRET)"}]' \
	  $(HERE)/argocd-application.yaml > $(HERE)/.application.yaml
	$(call replace-app)
	$(KUBECTL) -n $(ARGO_NS) annotate application $(APP) argocd.argoproj.io/refresh=hard --overwrite
	@echo $(VERSION) > $(HERE)/.image-tag; echo $(VERSION) > $(HERE)/.chart-version

# Wait until Argo reports Synced/Healthy AND the pods run the tag just built,
# so a stale Healthy from the previous sync does not end the wait early.
wait: context
	@tag=$$(cat $(HERE)/.image-tag); \
	for i in $$(seq 1 60); do \
	  s=$$($(KUBECTL) -n $(ARGO_NS) get application $(APP) -o jsonpath='{.status.sync.status}/{.status.health.status}' 2>/dev/null); \
	  img=$$($(KUBECTL) -n $(NS) get deploy -l app.kubernetes.io/instance=downscaler -o jsonpath='{.items[0].spec.template.spec.containers[0].image}' 2>/dev/null); \
	  echo "  $$s  $$img"; \
	  if [ "$$s" = "Synced/Healthy" ] && [ "$${img##*:}" = "$$tag" ]; then echo "deployed $(CHART_NAME) $$(cat $(HERE)/.chart-version), image $$tag"; exit 0; fi; \
	  sleep 5; \
	done; echo "timed out waiting for $(APP)"; exit 1

deploy: context
	$(MAKE) image TAG=$(TAG)
	$(MAKE) chart
	$(MAKE) source
	$(MAKE) apply
	$(MAKE) wait

deploy-published: context
	$(MAKE) apply-published VERSION=$(VERSION)
	$(MAKE) wait

undeploy: context
	$(KUBECTL) -n $(ARGO_NS) delete application $(APP) --ignore-not-found --wait=true

status: context
	@$(KUBECTL) -n $(ARGO_NS) get application $(APP) -o custom-columns=APP:.metadata.name,CHART:.spec.sources[0].targetRevision,SYNC:.status.sync.status,HEALTH:.status.health.status
	@$(KUBECTL) -n $(NS) get pods -o custom-columns=POD:.metadata.name,READY:.status.containerStatuses[0].ready,RESTARTS:.status.containerStatuses[0].restartCount,IMAGE:.spec.containers[0].image

logs: context
	$(KUBECTL) -n $(NS) logs -f deploy/downscaler-go-kube-downscaler

probe: context
	@$(KUBECTL) -n $(NS) port-forward deploy/downscaler-go-kube-downscaler 18080:8080 >/dev/null 2>&1 & pf=$$!; sleep 2; \
	for p in healthz readyz; do printf '/%s: ' $$p; curl -s -w ' (%{http_code})\n' localhost:18080/$$p; done; \
	curl -s localhost:18080/metrics | grep -E '^kubedownscaler_(scanning|last_successful|cycle_executions|workload_scan_errors)'; \
	kill $$pf
