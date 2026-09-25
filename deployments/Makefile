# Publish the chart to an OCI registry (see publish-chart.sh for every setting).
#
#   make -C deployments chart-publish REGISTRY=oci://registry.example.com/charts            # dry run
#   make -C deployments chart-publish REGISTRY=oci://registry.example.com/charts PUSH=1     # push
#   make -C deployments chart-publish ... APP_VERSION=<image tag> CHART_VERSION=<version>

export REGISTRY CHART_VERSION APP_VERSION SIGN_KEY KEYRING PLAIN_HTTP

.PHONY: chart-publish
chart-publish:
	./publish-chart.sh $(if $(PUSH),--push)
