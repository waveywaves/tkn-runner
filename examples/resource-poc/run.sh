#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "$0")" && pwd)
state=${1:?Usage: bash run.sh STATE_DIRECTORY}
cluster=$(< "$state/cluster")
[[ "$cluster" == tkn-resource-poc-* ]] || { echo 'Not a PoC cluster' >&2; exit 2; }
for command in kubectl jq tkn openssl curl; do command -v "$command" >/dev/null; done
k=(kubectl --kubeconfig "$state/kubeconfig" --context "kind-$cluster")
server=$("${k[@]}" config view --minify -o jsonpath='{.clusters[0].cluster.server}')
[[ "$server" == https://127.0.0.1:* ]] || { echo 'Expected local API server' >&2; exit 2; }
"${k[@]}" --request-timeout=20s -n tekton-pipelines wait deployment --all \
  --for=condition=Available --timeout=60s
"${k[@]}" --request-timeout=20s -n tekton-pipelines-resolvers wait deployment --all \
  --for=condition=Available --timeout=60s
namespace=$("${k[@]}" create -f - -o jsonpath='{.metadata.name}' <<<'{"apiVersion":"v1","kind":"Namespace","metadata":{"generateName":"resource-poc-"}}')
out=$(mktemp -d "$state/evidence.XXXXXX")
printf '%s\n' "$namespace" > "$out/namespace"
printf 'Evidence: %s\nNamespace: %s\n' "$out" "$namespace" >&2
n=("${k[@]}" -n "$namespace")
pf=
finish() {
  code=$?
  trap - EXIT
  if [[ -n "$pf" ]]; then kill "$pf" 2>/dev/null || true; wait "$pf" 2>/dev/null || true; fi
  "${n[@]}" get pipelineruns,taskruns,pods,pvc,events,resolutionrequests -o json > "$out/objects.json" || code=1
  if [[ "$code" != 0 ]]; then echo "PoC failed; retained $namespace and $out" >&2; fi
  exit "$code"
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Do not race the namespace controller's asynchronous default-account creation.
"${n[@]}" apply -f - <<<'{"apiVersion":"v1","kind":"ServiceAccount","metadata":{"name":"default"}}'

demo_step() {
  printf '\n%s\n' "$1" >&2
  if [[ "${DEMO_PAUSE:-0}" == 1 ]]; then
    read -r -p 'Press Enter to continue... ' </dev/tty
  fi
}
submit() {
  local label=$1
  tee "$out/$label.submitted.json" | "${n[@]}" create -f - -o jsonpath='{.metadata.name}'
}
terminal() {
  local run=$1 expected=$2
  # kubectl uses list/watch, including the current state: fast completions are not lost.
  "${n[@]}" wait --for="jsonpath={.status.conditions[?(@.type=='Succeeded')].status}=$expected" \
    "pipelinerun/$run" --timeout=360s
  "${n[@]}" get "pipelinerun/$run" -o json > "$out/$run.json"
  jq '{name:.metadata.name,conditions:.status.conditions,results:.status.results}' \
    "$out/$run.json" >&2
}
assert_message() {
  jq -e --arg pattern "$2" \
    '[.status.conditions[] | select(.type=="Succeeded") | .message] | join(" ") | test($pattern;"i")' \
    "$out/$1.json" >/dev/null
}

# 1. Required named Pipeline, then a Secret missing from an otherwise valid run.
demo_step '1. Submit without the required Pipeline: expect a real Tekton failure.'
run=$(submit missing-pipeline < "$root/bindings/run.json")
terminal "$run" False
assert_message "$run" 'local-build.*not found|not found.*local-build'
"${n[@]}" apply -f "$root/bindings/resources.yaml"
"${n[@]}" delete secret local-build-token
demo_step '2. Pipeline, Tasks, PVC and settings exist; the missing Secret must block execution.'
run=$(submit missing-secret < "$root/bindings/run.json")
"${n[@]}" wait --for=jsonpath='{.status.childReferences[?(@.pipelineTaskName=="verify")].name}' \
  "pipelinerun/$run" --timeout=180s
task=$("${n[@]}" get "pipelinerun/$run" -o jsonpath='{.status.childReferences[?(@.pipelineTaskName=="verify")].name}')
"${n[@]}" wait --for=jsonpath='{.status.podName}' "taskrun/$task" --timeout=60s
pod=$("${n[@]}" get "taskrun/$task" -o jsonpath='{.status.podName}')
"${n[@]}" wait --for=jsonpath='{.status.containerStatuses[0].state.waiting.reason}'=CreateContainerConfigError \
  "pod/$pod" --timeout=120s
"${n[@]}" get "pod/$pod" -o json > "$out/missing-secret.pod.json"
jq -e '[.status.containerStatuses[].state.waiting.message // ""] | join(" ") |
  contains("local-build-token")' "$out/missing-secret.pod.json" >/dev/null
jq '.status.containerStatuses[] | select(.state.waiting) | {name,state}' \
  "$out/missing-secret.pod.json" >&2
terminal "$run" False
"${n[@]}" get "taskrun/$task" -o json > "$out/missing-secret.taskrun.json"
jq -e '.status.conditions[] | select(.type=="Succeeded") |
  .status=="False" and .reason=="CreateContainerConfigError"' \
  "$out/missing-secret.taskrun.json" >/dev/null
# The failed TaskRun cannot recover. Preserve its evidence and stop its stranded Pod.
"${n[@]}" delete "pod/$pod"
demo_step '3. Supply the development Secret and submit a NEW run; the failed run stays failed.'
"${n[@]}" apply -f "$root/bindings/resources.yaml"
run=$(submit bindings-configured < "$root/bindings/run.json")
terminal "$run" True
"${n[@]}" logs -l "tekton.dev/pipelineRun=$run" --all-containers=true --prefix > "$out/bindings.log"

# 2. Real Git resolution; missing file must fail for that file, not any infrastructure error.
"${n[@]}" create configmap git-task --from-file=task.yaml="$root/git/task.yaml"
"${n[@]}" apply -f "$root/git/service.yaml"
"${n[@]}" rollout status deployment/git-fixture --timeout=180s
revision=$("${n[@]}" exec deployment/git-fixture -- git --git-dir=/tmp/fixture.git rev-parse HEAD)
jq --arg url "git://git-fixture.$namespace.svc/fixture.git" --arg revision "$revision" \
  '.spec.pipelineSpec.tasks[0].taskRef.params |=
    map(if .name=="url" then .value=$url elif .name=="revision" then .value=$revision else . end)' \
  "$root/git/run.json" > "$out/git.input.json"
demo_step '4. Ask the Git resolver for a nonexistent file at a fixed commit: expect failure.'
run=$(jq '.spec.pipelineSpec.tasks[0].taskRef.params |=
  map(if .name=="pathInRepo" then .value="missing-resource-poc.yaml" else . end)' \
  "$out/git.input.json" | submit missing-git-file)
terminal "$run" False
assert_message "$run" 'missing-resource-poc.yaml'
assert_message "$run" 'file (does not exist|not found)|path.*not found|could not find file|couldn.t find file'
demo_step '5. Correct only the Task path. Resolve the local Git fixture at its actual commit.'
run=$(submit git-configured < "$out/git.input.json")
terminal "$run" True
jq -e '.status.results[] | select(.name=="outcome") |
  .value=="resolved-from-git"' "$out/$run.json" >/dev/null

# 3. Local TLS registry, no external push, real Pipeline AND Task bundle resolution.
demo_step '6. Start a local TLS registry and configure resolver trust.'
host="registry.$namespace.svc"
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -keyout "$out/tls.key" \
  -out "$out/tls.crt" -subj "/CN=$host" -addext "subjectAltName=DNS:$host" >&2
chmod 600 "$out/tls.key"
"${n[@]}" create secret tls registry-tls --cert="$out/tls.crt" --key="$out/tls.key"
"${n[@]}" apply -f "$root/bundle/registry.yaml"
"${n[@]}" rollout status deployment/registry --timeout=180s
# Add this local CA while retaining the resolver image's system CA bundle.
"${k[@]}" -n tekton-pipelines-resolvers create configmap "ca-$namespace" --from-file=ca.crt="$out/tls.crt"
patch=$(jq -n --arg ca "ca-$namespace" '{
  spec:{template:{spec:{
    volumes:[{name:"poc-ca",configMap:{name:$ca}}],
    containers:[{name:"controller",env:[{name:"SSL_CERT_DIR",value:"/poc-ca"}],
      volumeMounts:[{name:"poc-ca",mountPath:"/poc-ca",readOnly:true}]}]
  }}}
}')
"${k[@]}" -n tekton-pipelines-resolvers patch deployment tekton-pipelines-remote-resolvers \
  --type strategic -p "$patch"
"${k[@]}" -n tekton-pipelines-resolvers rollout status deployment/tekton-pipelines-remote-resolvers --timeout=180s

# Subscribe to port-forward stdout before starting it; keep the FIFO open.
mkfifo "$out/forward-ready"
exec 3<>"$out/forward-ready"
"${n[@]}" port-forward service/registry :5000 > "$out/forward-ready" 2>"$out/port-forward.log" &
pf=$!
IFS= read -r -t 30 -u 3 line
[[ "$line" =~ 127.0.0.1:([0-9]+) ]] || { echo "Port forwarding failed: $line" >&2; exit 1; }
port=${BASH_REMATCH[1]}
bundle_run() {
  jq --arg bundle "$1" '.spec.pipelineRef.params |=
    map(if .name=="bundle" then .value=$bundle else . end) |
    .spec.params[0].value=$bundle' "$root/bundle/run.json"
}
bundle_run "$host:5000/poc:missing" > "$out/missing-bundle.input.json"
demo_step '7. Request a bundle absent from the reachable registry: expect missing-manifest failure.'
run=$(submit missing-bundle < "$out/missing-bundle.input.json")
terminal "$run" False
assert_message "$run" 'MANIFEST_UNKNOWN|manifest unknown|404'
demo_step '8. Publish locally, then resolve BOTH Pipeline and Task by digest.'
tkn bundle push "localhost:$port/poc:fixture" --remote-skip-tls -f "$root/bundle/resources.yaml" \
  > "$out/bundle-push.log"
digest=$(curl --fail --silent --show-error --noproxy '*' --cacert "$out/tls.crt" \
  --resolve "$host:$port:127.0.0.1" \
  -H 'Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
  -D "$out/manifest.headers" "https://$host:$port/v2/poc/manifests/fixture" | openssl dgst -sha256)
digest="sha256:${digest##* }"
[[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]
bundle_run "$host:5000/poc@$digest" > "$out/bundle.input.json"
run=$(submit bundle-configured < "$out/bundle.input.json")
terminal "$run" True
jq -e '.status.results[] | select(.name=="outcome") |
  .value=="resolved-from-bundle"' "$out/$run.json" >/dev/null
jq -n --arg namespace "$namespace" --arg evidence "$out" \
  '{outcome:"passed",namespace:$namespace,evidence:$evidence}'
