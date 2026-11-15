#!/usr/bin/env bash
# Package the downscaler chart and push it to an OCI registry.
#
# For publishing builds of this fork to your own registry until the changes are
# released upstream (upstream publishes its releases from .github/workflows/helm_build.yaml).
# Nothing here is specific to one organisation: the registry and versions come
# from the environment, and you log in to the registry yourself beforehand
# (`helm registry login <host>`; helm also reuses `docker login`).
#
#   REGISTRY=oci://registry.example.com/charts deployments/publish-chart.sh           # dry run (default)
#   REGISTRY=oci://registry.example.com/charts deployments/publish-chart.sh --push    # package and push
#
# Environment:
#   REGISTRY       OCI location to push to, oci://host[/path]. Required with --push.
#   CHART_VERSION  Chart version. Default: <Chart.yaml version>-<UTC timestamp>, unique per run
#                  and a valid SemVer pre-release, e.g. 1.3.4-20260925014055.
#   APP_VERSION    appVersion, which is also the image tag the chart defaults to.
#                  Default: Chart.yaml appVersion. Set it to the image tag you built.
#   SIGN_KEY       Optional GPG key name: sign the package (.prov) the way upstream does.
#   KEYRING        Keyring holding SIGN_KEY. Default: ~/.gnupg/secring.gpg.
#   PLAIN_HTTP     Set to 1 for a registry without TLS (a local test registry).
set -euo pipefail

usage() { sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'; }

push=false
case "${1:-}" in
  "") ;;
  --push) push=true ;;
  -h|--help) usage; exit 0 ;;
  *) echo "unknown argument: $1" >&2; usage >&2; exit 64 ;;
esac

if [[ -t 1 && -z "${NO_COLOR:-}" ]]; then
  bold=$'\033[1m'; green=$'\033[32m'; yellow=$'\033[33m'; red=$'\033[31m'; off=$'\033[0m'
else
  bold=; green=; yellow=; red=; off=
fi
step() { echo; echo "${bold}==> $*${off}"; }
ok()   { echo "${green}    $*${off}"; }
warn() { echo "${yellow}    $*${off}"; }
die()  { echo "${red}error: $*${off}" >&2; exit 1; }

for tool in helm yq; do
  command -v "$tool" >/dev/null || die "$tool is not installed"
done

chart_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/chart" && pwd)"
out_dir="$(cd "$chart_dir/../.." && pwd)/dist/chart"
name="$(yq -r .name "$chart_dir/Chart.yaml")"
base_version="$(yq -r .version "$chart_dir/Chart.yaml")"
chart_version="${CHART_VERSION:-${base_version}-$(date -u +%Y%m%d%H%M%S)}"
app_version="${APP_VERSION:-$(yq -r .appVersion "$chart_dir/Chart.yaml")}"
registry="${REGISTRY:-}"

if $push; then
  [[ -n "$registry" ]] || die "REGISTRY is required with --push, e.g. REGISTRY=oci://registry.example.com/charts"
  [[ "$registry" == oci://* ]] || die "REGISTRY must start with oci:// (got $registry)"
fi

step "$name $chart_version (appVersion $app_version)"

step "README parameters table"
readme_before="$(cat "$chart_dir/README.md")"
npx --yes @bitnami/readme-generator-for-helm@2.7.2 \
  --values "$chart_dir/values.yaml" --readme "$chart_dir/README.md" >/dev/null
if [[ "$(cat "$chart_dir/README.md")" == "$readme_before" ]]; then
  ok "up to date"
else
  $push && die "README.md was out of date with values.yaml and has been regenerated; commit it, then publish"
  warn "README.md was out of date with values.yaml and has been regenerated; commit it"
fi

step "helm lint --strict"
helm lint --strict "$chart_dir"
for values in "$chart_dir"/ci/*-values.yaml; do
  [[ -e "$values" ]] && helm lint --strict "$chart_dir" --values "$values"
done

step "helm unittest"
if helm plugin list 2>/dev/null | grep -q '^unittest'; then
  helm unittest "$chart_dir"
else
  $push && die "helm-unittest is not installed: helm plugin install https://github.com/helm-unittest/helm-unittest"
  warn "skipped: helm-unittest is not installed"
fi

step "helm package"
rm -rf "$out_dir" && mkdir -p "$out_dir"
sign_args=()
if [[ -n "${SIGN_KEY:-}" ]]; then
  sign_args=(--sign --key "$SIGN_KEY" --keyring "${KEYRING:-$HOME/.gnupg/secring.gpg}")
fi
# --version/--app-version set the packaged Chart.yaml; the source Chart.yaml is not edited.
helm package "$chart_dir" --version "$chart_version" --app-version "$app_version" \
  --destination "$out_dir" ${sign_args[@]+"${sign_args[@]}"}
package="$out_dir/$name-$chart_version.tgz"
[[ -s "$package" ]] || die "expected $package"
ok "$package"

if ! $push; then
  step "dry run: not pushing"
  echo "    push with: REGISTRY=${registry:-oci://<host>[/path]} $0 --push"
  exit 0
fi

step "helm push $registry"
push_args=()
[[ "${PLAIN_HTTP:-}" == 1 ]] && push_args=(--plain-http)
helm push "$package" "$registry" ${push_args[@]+"${push_args[@]}"}
ok "pushed $registry/$name:$chart_version"
echo "    Argo CD: repoURL ${registry#oci://}, chart $name, targetRevision $chart_version"
