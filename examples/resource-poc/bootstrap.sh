#!/usr/bin/env bash
set -euo pipefail

# This spike deliberately uses installed CLIs, not the historical runner.
state=${1:?Usage: bash bootstrap.sh NEW_STATE_DIRECTORY}
mkdir "$state"
state=$(cd "$state" && pwd)
cluster="tkn-resource-poc-${RANDOM}-${RANDOM}"
printf '%s\n' "$cluster" > "$state/cluster"
export KIND_EXPERIMENTAL_PROVIDER=podman
kind create cluster --name "$cluster" --image kindest/node:v1.29.2 \
  --kubeconfig "$state/kubeconfig" --wait 180s
chmod 600 "$state/kubeconfig"
k=(kubectl --kubeconfig "$state/kubeconfig" --context "kind-$cluster")
"${k[@]}" apply -f \
  https://infra.tekton.dev/tekton-releases/pipeline/previous/v0.65.2/release.yaml
"${k[@]}" -n tekton-pipelines wait deployment --all --for=condition=Available --timeout=180s
"${k[@]}" -n tekton-pipelines-resolvers wait deployment --all --for=condition=Available --timeout=180s
"${k[@]}" -n tekton-pipelines-resolvers patch configmap resolvers-feature-flags \
  --type merge -p '{"data":{"enable-git-resolver":"true","enable-bundles-resolver":"true"}}'
printf 'Ready. Run: bash run.sh %q\n' "$state"
