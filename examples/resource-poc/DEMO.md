# Local Podman demo

## Message to demonstrate

"Valid PipelineRun YAML is not enough. Execution also depends on resources,
remote definitions and registry contents. We make those dependencies explicit
and show the actual failures before supplying them."

This runs Kubernetes and Tekton inside Kind on **local Podman**. It does not
target OpenShift or change your ambient kubeconfig. Image pulls still require
internet access; Git and OCI fixture services run inside the local cluster.

## Prepare before presenting

Run from a Bash terminal:

```sh
cd /Users/vibhavbobade/go/src/github.com/waveywaves/tkn-runner/examples/resource-poc
state=/tmp/resource-poc-demo
podman info
bash bootstrap.sh "$state"
bash run.sh "$state"
```

Bootstrap needs a new state directory. Retain the successful warm environment
for the presentation instead of spending demo time downloading node/controller
images. The rehearsal must finish with `"outcome": "passed"`; missing-dependency
failures alone do not establish that the environment works.

## Present

```sh
DEMO_PAUSE=1 bash run.sh "$state"
```

The script pauses before each step. Press Enter to continue. It prints actual
Tekton conditions and results rather than simulated success messages.

| Step | Show | Explain |
| --- | --- | --- |
| 1 | Missing named Pipeline fails | A run references definitions that must exist |
| 2 | Verify Pod reports the missing Secret | Valid definitions still cannot supply absent configuration |
| 3 | Supply the Secret; a new run succeeds | Tekton keeps the failed run failed; the new run uses complete inputs |
| 4 | Git resolution fails for a nonexistent file | Resolvers need the right remote revision and path |
| 5 | Correct path succeeds at the same recorded commit | The real resolver retrieves the fixture Task from a local Git server; no Task is installed to bypass resolution |
| 6–7 | Local TLS registry is reachable, but bundle absent | Connectivity and trust do not imply the requested artifact exists |
| 8 | Publish the bundle locally; both resolution hops succeed | The Pipeline and Task are resolved by their actual uploaded digest |

Optional second terminal, using the same state path:

```sh
kubectl --kubeconfig /tmp/resource-poc-demo/kubeconfig get pipelineruns -A --watch
```

The harness prints the namespace and evidence directory. Inspect that namespace's
TaskRuns, Pods and PVCs while paused. The saved `missing-secret.pod.json` proves
the blocked state even after recovery; submitted JSON and final run JSON show
exactly what was tested.

Close with: "These examples identify configuration the runner must manage.
They are not a full PaC simulation, a production signing setup, or proof that
every pipeline works offline."

## After presenting

Runs and evidence remain available. Stop the optional watch with Ctrl+C.
To remove only this demo's cluster:

```sh
cluster=$(< "$state/cluster")
KIND_EXPERIMENTAL_PROVIDER=podman kind delete cluster --name "$cluster"
```

Do not prune unrelated Podman containers, images or volumes. Remove the local
state directory separately only when you no longer need its evidence.
