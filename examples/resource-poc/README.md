# Resource-dependent PipelineRun PoC

This isolated spike exercises real upstream Tekton resource lookup and workload
execution, without using the historical k3s/virtual-kubelet runner. It is not the
single-binary product or a PaC provider simulation. Nothing publishes to a forge
or external registry.

## Cases

| Case | Missing dependency | Explicit configuration | Required observation |
| --- | --- | --- | --- |
| Build inputs | Named Pipeline, then required Secret | Local Tasks/Pipeline, PVC, ConfigMap and dummy development Secret | Missing Pipeline terminates unsuccessfully; missing Secret fails the TaskRun; a new run succeeds after the Secret is supplied |
| Git resolver | Task file absent at a pinned Git revision | Correct path at the same revision in a local Git service | Missing path fails; resolved Task executes and returns its real result |
| Bundle resolver | Manifest absent from reachable local registry | Push a Pipeline and Task bundle to a namespace-local TLS registry | Missing manifest fails; both resolution hops succeed using the uploaded digest and return the expected Task result |

The build-input case is **not a Maven build**. It replaces compilation and source
fetching with deterministic file checks, while retaining a shared PVC, settings
ConfigMap, named Task references, a Task result edge, and `finally`. Its Secret
contains only a public dummy value; it tests Secret injection, not authentication.
The harness deliberately removes only that Secret, not the ConfigMap or PVC.
Those remain hard prerequisites and their absence is not independently tested.

The Git case serves `git/task.yaml` in a tiny, ephemeral Git repository inside
the namespace. The actual generated commit is recorded in the submitted run.
This explicitly replaces GitHub and the original OpenShift-only Task image;
it still uses the upstream Git resolver, not a locally installed Task. It
exercises one Git resolution hop, not two, and does not test provider
authentication or dirty source. This substitution followed an observed
one-minute timeout resolving the larger public catalog from this workstation.
The fixture service has no credentials and is only intended for this local demo.

The bundle case preserves both remote Pipeline and remote Task resolution.
It replaces Quay with a local registry and replaces the OpenShift internal image
with BusyBox. The host publishes through loopback port forwarding; the resolver
uses Service DNS and a locally generated CA. It does not rely on node image
mirrors to rewrite addresses inside Pods. The registry is temporary, unauthenticated,
and has no durable artifact contract. The CA is added only to this owned cluster.

## Run

For a presentation with pauses and explanations, follow [DEMO.md](DEMO.md).

Prerequisites: working Podman (on macOS, a running Podman machine), Kind, kubectl,
`tkn bundle`, jq, curl, and OpenSSL supporting `req -addext`. These CLIs are
deliberate spike dependencies, not the final product's distribution contract.
Do not use `tkn-runner delete`; its historical cleanup is unrelated to this PoC.

```sh
cd examples/resource-poc
bash bootstrap.sh /tmp/resource-poc-state
bash run.sh /tmp/resource-poc-state
```

Bootstrap requires a new state directory, creates a uniquely named Kind cluster
through Podman, and writes a private kubeconfig without changing the ambient one.
It pins Kind's node image to Kubernetes v1.29.2 and Tekton to v0.65.2, including
the upstream remote resolvers. This is a compatibility spike, not a recommendation
to deploy these older releases. The initial workstation has Kind v0.22.0.

Each invocation creates a new namespace and evidence directory. It retains
submitted runs, final conditions, missing-Secret Pod state, execution objects,
ResolutionRequests, and build logs. Timeouts or unrelated infrastructure failures
fail the suite; they do not count as expected missing-resource results. Positive
runs must report `Succeeded=True` and the Git/bundle runs must expose actual
expected results. A pass prints one JSON summary on stdout (tool progress also
appears on stdout in this spike). No automatic task skipping occurs.
Run invocations sequentially: the registry CA mount is shared by this cluster's
resolver Deployment and is replaced by each invocation.
The harness explicitly supplies the default ServiceAccount instead of racing
the namespace controller. After recording the missing-Secret failure, it removes
that failed TaskRun's stranded Pod and submits a new run with the Secret present.

On interruption, the script stops its port-forward but **retains workloads**;
it does not claim that stopping observation cancels Tekton. Retention is
intentional for debugging. Inspect/cancel runs before removing the namespace.
The TLS private key is generated locally and kept under the evidence directory,
which `mktemp` makes private. Treat that directory as local state, not source.

```sh
# Read the exact owned name saved by bootstrap, then remove only that cluster.
state=/tmp/resource-poc-state
cluster=$(< "$state/cluster")
KIND_EXPERIMENTAL_PROVIDER=podman kind delete cluster --name "$cluster"
# Remove the local state directory separately when the evidence is no longer needed.
```

A failed bootstrap leaves its state directory for diagnosis. Do not reuse it
blindly: check its saved cluster name and clean up that owned cluster first.
No global volume prune or broad container deletion is performed.

## Inspiration

Hack revision: `de7cbbdf088e7a65e27c4caa7b68bdfbc04a1ec8`.
Its [acceptance-test template](https://github.com/openshift-pipelines/hack/blob/de7cbbdf088e7a65e27c4caa7b68bdfbc04a1ec8/internal/konflux/templates/konflux/acceptance-tests.yaml)
leads through the Operator acceptance pipeline to `openshift-pipelines/release-tests`.

- [Hack MultiKueue PipelineRun](https://github.com/openshift-pipelines/hack/blob/de7cbbdf088e7a65e27c4caa7b68bdfbc04a1ec8/release-tests/multikueue/pipelinerun/pr-multikueue.yaml):
  named Tasks, kubeconfig Secrets and shared storage. This spike does not install
  MultiKueue or substitute dummy credentials for real cluster access.
- [Maven Pipeline](https://github.com/openshift-pipelines/release-tests/blob/5bed519b85821919a3912473a9c09dc31bdc9e2f/testdata/ecosystem/pipelines/maven.yaml)
  and [run](https://github.com/openshift-pipelines/release-tests/blob/5bed519b85821919a3912473a9c09dc31bdc9e2f/testdata/ecosystem/pipelineruns/maven.yaml):
  settings, shared PVC and catalog resolution. Local named Tasks replace the
  original cluster resolver; that resolver is not covered by this case.
- [Git-resolved run](https://github.com/openshift-pipelines/release-tests/blob/5bed519b85821919a3912473a9c09dc31bdc9e2f/testdata/resolvers/pipelineruns/git-resolver-pipelinerun.yaml)
  and [bundle-resolved run](https://github.com/openshift-pipelines/release-tests/blob/5bed519b85821919a3912473a9c09dc31bdc9e2f/testdata/resolvers/pipelineruns/bundles-resolver-pipelinerun.yaml):
  real remote resolution rather than installing fetched resources as local objects.

## Verification

```sh
bash -n bootstrap.sh run.sh
bun test fixtures.test.js
```

These static checks parse the manifests and check resource bindings and bundle
contents. They do not validate Kubernetes admission, resolver behavior, TLS,
image availability or workload execution; only `run.sh` can establish those.
See `VALIDATION.md` for measured workstation evidence.
