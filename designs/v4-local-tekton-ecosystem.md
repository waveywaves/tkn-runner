# V4 proposal: the Tekton ecosystem running locally

Date: 2026-11-16. Status: design proposal; not implemented.

This supersedes [v3's restricted Task executor](v3-local-validation.md).
The confirmed requirements are:

- One distributed `tkn-runner` binary, using an existing Podman installation.
- Execution on the user's machine, not a remote Kubernetes cluster.
- PipelineRuns and the full Tekton ecosystem, not a whitelist of simple Tasks.
- Fast pre-commit validation of local source, including organization-defined
  checks, with dependable feedback for coding agents.

The existing PoC still implements only its historical `start`/`delete`
workflow. Commands and architecture below describe the proposed replacement.

## Architectural decision

Run the real upstream Tekton components on a real local Kubernetes environment
inside Podman. The binary manages that environment using Kind's Go library,
explicitly selecting its Podman provider. It does not translate Task steps or
Kubernetes Pods into a second execution model.

```text
One tkn-runner executable on the workstation
  |
  +-- definition/source preparation and machine-readable reporting
  +-- Kind Go library, explicitly configured for existing Podman
  +-- Kubernetes, Tekton and service API clients
  |
  v
Local Podman
  |
  +-- local Kubernetes node container(s)
        |
        +-- API server, real kubelet, containerd, networking and storage
        +-- Tekton Operator
        +-- Pipelines and resolvers
        +-- Triggers and interceptors
        +-- Chains
        +-- Results API, watcher and retention services
        +-- Dashboard
        +-- Pipelines-as-Code
        +-- local database, log storage and OCI registry
        +-- maintained self-hosted Hub API/UI and catalog data
        +-- workload Pods created by Tekton
```

On Linux the environment runs through local Podman. On macOS it runs inside
the user's existing Podman Machine Linux VM. The binary does not install or
upgrade Podman, create that VM, or choose a remote Podman connection.

This is one distributed executable, not one operating-system process.
Kubernetes and Tekton processes live in containers. `kind`, `kubectl`, Helm,
`tkn`, `openssl`, and database clients are not required as separately installed
commands for the managed workflow. Git remains required for source enumeration.

The earlier statement "no Kubernetes" is withdrawn. The user-facing
requirement is no separately managed cluster, not removal of Kubernetes from
Tekton's implementation.

## Why this is different from the original PoC

The original PoC retained Kubernetes's API and Tekton's controller but replaced
worker behavior with a custom virtual node. This design retains real worker
behavior too: Pod lifecycle, volumes, readiness, credentials, logs, security
contexts, and termination messages remain Kubernetes responsibilities.

The runner owns installation, source delivery, observation, and cleanup. It
does not own Tekton scheduling, substitution, step ordering, sidecar semantics,
results propagation, retries, or pipeline DAG evaluation.

Kind's public Go API exposes `ProviderWithPodman()`. Call it explicitly;
do not rely on automatic Docker/Podman selection or an ambient kubeconfig.
Rootless Podman has documented cgroup, networking and filesystem restrictions.
Preflight must check the chosen host/VM and return an actionable failure rather
than silently changing privileges or falling back to another runtime.

## Full ecosystem scope

The product target includes every row below. Delivering only Pipelines is an
intermediate milestone, not completion of this design.

| Component | Local implementation | Completion evidence |
| --- | --- | --- |
| Pipelines | Upstream controller, webhook, CRDs, resolvers and entrypoint images. | TaskRuns and PipelineRuns execute with references, DAGs, parallel tasks, `finally`, retries, conditions, matrices, typed params/results, StepActions, sidecars, workspaces, secrets and service accounts as supported by the pinned release. |
| Triggers | Upstream controller, webhook, core interceptors and EventListeners. | A locally submitted event passes through bindings/templates/interceptors and creates a real run. |
| Chains | Upstream controller with a persistent local development signing key, local storage and local OCI registry. | A completed workload produces verifiable signatures/attestations; missing keys or storage are reported rather than signing being silently disabled. |
| Results | Upstream API, watcher and retention components; local Postgres and persistent log storage. | Runs and logs are queryable after execution-resource cleanup, and survive ordinary environment reuse. |
| Dashboard | Upstream Dashboard exposed through a loopback-only connection. | The user can inspect live runs, logs and historical data supported by the selected component versions. |
| Operator | Upstream Kubernetes Operator installation and explicit TektonConfig/component configuration. | Install, readiness, restart and upgrade behavior are exercised against the selected component set. |
| CLI | Embed compatible upstream `tkn` command code under a runner subcommand, with clients bound to this local environment. | Core upstream CLI operations work without installing a second binary. External third-party plugins are not silently discovered or launched. |
| Catalog and Hub | Use upstream catalog resources and resolvers; deploy the maintained self-hosted Hub fork and its local database for Hub API/UI functionality. | Local resource discovery, retrieval and execution work against configured catalog content; no dependency on the retired public Tekton Hub endpoint. |
| Pipelines-as-Code | Upstream matching/resolution for local simulation and the real PaC controller for configured provider integrations. | Local simulation is explicitly labeled, while native-controller tests exercise its real event/provider path with supplied credentials or local test services. |

The upstream `tektoncd/hub` repository is archived and its public service was
shut down. Its README points self-hosted users to `openshift-pipelines/hub`.
The version inventory must distinguish that maintained fork from the archived
project; "full ecosystem" must not silently mean deploying abandoned artifacts.

## What full compatibility means

Use upstream APIs and execution semantics for the selected, tested releases.
There is no Task-only whitelist and no rejection of PipelineRuns, sidecars,
remote resolvers, results, or security contexts merely because the runner
cannot interpret them. Upstream admission and controllers interpret them.

The release records every component version, image digest, API version and
feature flag. Stable and enabled beta features must work. Alpha/experimental
features remain explicitly configurable and require their upstream dependencies
and dedicated tests; do not turn all feature flags on and call that parity.

CustomRuns work when their corresponding custom controllers are installed.
Users can install their additional Kubernetes resources into the owned local
environment; missing controllers must remain visible as unmet prerequisites.

Local Kubernetes is not OpenShift. OpenShift-only Routes, SCCs, operators,
cloud infrastructure, hardware devices and external services do not appear
merely because Tekton runs. Workloads relying on them require those resources
or an explicitly configured local equivalent. Report that distinction instead
of approximating behavior or claiming every possible Task is portable.

All baseline execution, state, signing, logs, database and UI services run
locally. Image/catalog downloads and explicitly configured Git providers, KMS,
registries or deployment targets can use the network. Local execution does not
mean offline operation or that an arbitrary pipeline has no external effects.
An offline mode requires preloaded images/resources; it is not assumed.

## Packaging, versions and persistent state

Ship one native executable per supported platform. Embed the installer
manifests and tested default configuration, not the complete container images.
Normal OCI pulls populate Podman's node-image store and the Kubernetes nodes'
separate workload image stores. A cached image in Podman is not automatically
present in node containerd.

Use one pinned release inventory for Kind, Kubernetes node images, Operator,
Tekton components, PaC, Hub and helper services. Prefer Operator-managed
components over separate competing installers. Do not assume Operator's
profile named `all` enables every row above: reconcile and verify the explicit
component inventory for the pinned Operator version.

Exact versions are a release-selection gate, not invented in this design.
Current upstream documentation demonstrates capabilities but is not evidence
that arbitrary component releases work together. A release cannot ship until
its concrete inventory and compatibility tests are recorded.

Store environment identity, private kubeconfig, signing material, configuration
and ownership records in a runner-owned state directory. Persistent workload
history, Results storage, registry data and catalog data belong to the local
environment, not to individual validation runs.

Never merge the managed kubeconfig into the user's default configuration.
Verify recorded environment identity and local Podman endpoint before writes.
Do not adopt or mutate an unrelated cluster with a matching name.
Embedded CLI commands must use the same locked local clients; flags that would
redirect them to a different cluster must fail rather than bypass this boundary.

## Proposed user workflow

These commands are proposed, not available in the current binary:

```sh
# Bootstrap the full local ecosystem once; also invoked by run if absent.
tkn-runner up

# Verify component readiness and show local environment/version information.
tkn-runner status --output json

# Execute an ordinary PipelineRun or TaskRun with its declared inputs.
tkn-runner run -f .tekton/pipeline.yaml --output json

# Explicitly seed a declared workspace from uncommitted local source.
tkn-runner run -f .tekton/local.yaml --workspace source=. --output json

# Use upstream CLI commands against this owned local environment.
tkn-runner tkn pipelinerun list

# Expose Dashboard on loopback; print its URL without opening a browser.
tkn-runner dashboard

# Recover and clean only one run's owned execution resources.
tkn-runner clean --run-id <run-id>

# Destroy this owned environment and its persistent data explicitly.
tkn-runner down --delete-data
```

`up` is idempotent and reuses a healthy environment. `run` never rebuilds the
cluster between agent iterations, never silently upgrades components, and never
falls back to the current kubeconfig context or a remote cluster.

The cluster remains warm after `run` exits. `down --delete-data` is distinct
from per-run cleanup and must identify the environment and data being deleted.
An upgrade is an explicit lifecycle operation with version checks and database
backup/migration handling, not an incidental effect of starting a run.

## Definitions, local source and PaC

Accept standard TaskRun/PipelineRun YAML with inline specs or references.
Supporting Tasks, Pipelines, StepActions and namespaced Kubernetes resources
can be included as multiple documents in the same version-controlled file.
An invocation must select one entry run explicitly if the input contains
several; zero selected runs is not successful validation.

Apply supporting resources in dependency order to a run-owned namespace and
use upstream admission, not a replacement YAML interpretation engine. Preserve
the original definition and the exact submitted resource for the report.
Namespace/name allocation and explicit workspace overrides must be recorded.
Never remove tasks based on names such as `clone` or `fetch`.

Two source modes are deliberately distinct:

1. **Declared inputs:** submit the pipeline with its normal source acquisition,
   bindings and resolver behavior. This tests that pipeline, not necessarily
   the developer's dirty working tree.
2. **Explicit local source:** capture tracked current files and nonignored
   untracked files, upload an isolated snapshot into a local PVC, and bind the
   named workspace. The pipeline must be authored to consume a supplied
   workspace or explicitly condition its source-fetching stage.

Prepopulating a workspace cannot prevent an arbitrary later clone task from
overwriting it. Full pipeline compatibility is not automatic conversion of any
pipeline into a dirty-tree test. Do not invent clone results or commit SHAs to
hide this distinction. The report records source mode, snapshot identity and
all explicit input changes.

Use Git-aware file enumeration and a streamed archive transfer through a
temporary Kubernetes upload Pod. Preserve executable modes and supported
internal symlinks; reject escaping links and unsupported filesystem entries.
Do not mount the developer checkout writable into the cluster. Handle submodules
by recursively capturing initialized submodule working trees at their mounted
paths, including their local modifications. Fail on uninitialized or missing
submodules rather than silently omitting them or fetching remote content.
Detect changes during capture; record hashes of the actual uploaded content.

For returned filesystem artifacts, export explicitly selected paths from owned
PVCs before deleting them, with bounded size and path/symlink checks. Tekton
Results are collected through their real API/status paths, not confused with
arbitrary filesystem artifacts.

PaC matching and resolution are a separate front end. Reuse upstream code
instead of implementing CEL or provider semantics. Pact provides a useful
reference for local changed-file information and source upload, but its
heuristic clone removal and no-match-success behavior are not adopted.

PaC simulation does not require a real PR or forge webhook, and must be labeled
as simulation. Native provider integration still requires its actual payload,
configuration and credentials. Local Triggers events can exercise EventListeners
without a public webhook tunnel; exposing listeners externally is opt-in.

## Local service defaults

- **Storage:** a verified local dynamic provisioner supplies PVCs; per-run PVCs
  and persistent service PVCs have separate ownership and cleanup policies.
- **Results:** local Postgres, private generated credentials, TLS and local
  filesystem/PVC log storage. No external database or cloud bucket is required
  for the baseline. Development sizing is explicit, not a production promise.
- **Chains:** persistent local development signing key and local registry/
  Kubernetes-backed attestation storage supported by the pinned release.
  Record the signing identity; it is not an organization's trusted production
  identity or evidence of a trustworthy workstation.
- **Registry:** a local OCI registry for generated images, bundles and signing
  tests, reachable by both the host-side tooling and workload Pods with tested
  routing and certificate configuration.
- **Hub/catalog:** maintained API/UI with its own database credentials and
  pinned catalog content. OAuth or remote catalog synchronization is explicitly
  configured, not required to execute an already available Task.
- **Endpoints:** API server, Dashboard, Hub, Results and local event forwarding
  are bound to loopback by default. Use Go API clients/port-forwarding rather
  than requiring external command-line tools.

## Outcomes, cancellation and cleanup

Return a versioned JSON terminal report on stdout; live progress goes to
stderr. Include environment/component identities, original/submitted
definitions, source mode/digests, upstream run UIDs and conditions, actual
TaskRun/step states, logs, Tekton Results, artifact references, signing and
Results-persistence outcomes, and cleanup state.

Do not treat a completed PipelineRun as proof that asynchronous Chains signing
or Results persistence has completed. Observe the corresponding component
evidence with bounded waits before claiming those parts succeeded or pruning
the execution objects. Required evidence failures remain failures.

Use upstream cancellation APIs for PipelineRuns/TaskRuns and observe terminal
workloads. Stopping a log reader is not cancellation. Keep the primary outcome,
whether cleanup finished, and any remaining resource identifiers distinct.
An independent bounded cleanup context may outlive the caller's cancellation.

Run-owned resources may be pruned only after required evidence is retained.
Persistent database, signing keys, registry, catalog and cluster resources are
never removed by per-run cleanup. No global Podman volume prune or broad
container-name deletion is permitted.

Exit categories remain distinct: checks failed, invalid definition, unmet
environment prerequisite, infrastructure/evidence failure, timeout and
cancellation. Exit zero requires a selected run and completion of its required
execution/evidence contract; a no-match or missing component is not green.

## Changes to this repository

| Existing or proposed item | Revised action |
| --- | --- |
| `pkg/virtualkubelet/` | Remove after native execution is verified. Do not repair or expand the Pod translator. |
| `pkg/controlplane/k3s/`, embedded k3s binary | Replace with a local Kind/Podman lifecycle manager using real workers. |
| `pkg/manifests/` | Replace ad hoc controller/CRD installation with a pinned upstream Operator/component inventory and tested local service configuration. Keep embedding manifests as a distribution technique. |
| V3 direct step executor and Task whitelist | Do not implement. Tekton and Kubernetes execute and validate their own definitions. |
| `cmd/tkn-runner/main.go` | Replace historical CLI wiring with local environment lifecycle, native run/observe operations, and embedded upstream CLI integration. |
| Cluster configuration | Retain only local environment/component configuration needed for the confirmed full scope; no remote-provider abstraction. |
| Source snapshot, JSON reports and run ownership | Retain these v3 product requirements, adapting transfer and ownership to real namespaces/PVCs/Pods. |
| `pipeline` gitlink | Replace with pinned module/test inputs only after preserving any local work. Do not touch the currently dirty checkout. |
| Tests/examples | Add real PipelineRun and ecosystem integration scenarios, not only simple Task demonstrations. |
| V1/V2/V3 documents | Preserve as historical proposals. V4 is the current direction. |

Suggested implementation boundaries are local-environment lifecycle, suite
configuration, source delivery, native execution/observation, and reporting.
Reuse client-go and upstream component APIs. Do not add a generic execution
backend framework or a second scheduler.

Pact is the closest reference for the cluster-backed workflow. Its architecture
is more relevant now than it was to v3. Direct code reuse still requires
clarifying its missing project license and selecting compatible dependency
versions. Importing the whole project is not assumed.

## Implementation sequence

1. **Prove the local substrate.** Embed Kind with explicit Podman selection;
   verify a real node, storage, networking, API access and restart on Linux
   and macOS. No remote cluster or separately installed Kind CLI.
2. **Prove native PipelineRuns.** Run the official controller with DAGs,
   results, `finally`, sidecars, retries, references and multi-Task workspaces;
   exercise dirty-source upload, actual logs and cancellation.
3. **Complete the ecosystem.** Install and test Triggers, Chains, Results,
   Dashboard, Operator lifecycle, PaC, embedded CLI and maintained local Hub/
   catalog functionality with the local service defaults.
4. **Complete the agent workflow.** One-file invocation, explicit source
   selection, strict no-work outcomes, JSON reports, durable evidence and
   reliable targeted cleanup; deliberately fail lint, tests, coverage and docs.
5. **Cut over and package.** Remove the obsolete shim, publish the tested
   component inventory and prerequisite matrix, verify native binaries, and
   document upgrades and data deletion.

These are implementation milestones, not a reduced final scope. The full
ecosystem remains incomplete until stage 3 and the cross-component gates pass.
The v3 three-to-five-week estimate does not cover this expanded requirement;
re-estimate after the substrate spike and component-version selection.

## Acceptance criteria

- All workload execution and baseline backing services are demonstrably local
  through the selected Podman connection. No ambient kubeconfig is read for
  cluster targeting, and no remote execution fallback exists.
- One binary plus existing Podman and Git suffices; no separately installed
  cluster CLI, Tekton CLI, database client or certificate generator is needed.
- Real upstream integration scenarios cover the Pipelines features in the
  component table. Unknown/admission-invalid definitions fail upstream rather
  than being silently simplified.
- A local event creates a run through Triggers. Chains evidence verifies.
  Results and logs remain queryable after run cleanup. Dashboard and Hub work
  through local endpoints. PaC simulation and native provider tests are
  reported separately.
- Native cancellation stops execution; cleanup preserves unrelated resources
  and persistent service data. Restart, failed bootstrap, crashed client,
  concurrent invocation, certificate expiry and interrupted upgrades have
  explicit recovery evidence.
- End-to-end fixtures use real Podman/Kubernetes components, with deterministic
  event-based synchronization and bounded waits, not sleeps that happen to
  pass. Cross-compilation is not runtime validation.
- Record cold bootstrap separately from warm run latency. Measure source
  transfer, scheduling, actual checks, signing/persistence, log collection and
  cleanup, with total host/VM memory, disk and idle cost.
- Compare the wrapper with equivalent direct submission to the same warm
  cluster. V3's small direct-container timing budgets are withdrawn; select
  full-suite budgets from measured representative workloads before claiming
  fast iteration.

No component combination or host configuration is certified by writing this
document. The concrete release inventory, local backing-service manifests and
runtime results are required implementation evidence.

## Sources checked for this revision

- [Kind rootless Podman requirements](https://kind.sigs.k8s.io/docs/user/rootless/)
- [Kind provider Go API](https://github.com/kubernetes-sigs/kind/blob/v0.32.0/pkg/cluster/provider.go)
- [Tekton ecosystem overview](https://tekton.dev/docs/concepts/overview/)
- [Operator TektonConfig](https://github.com/tektoncd/operator/blob/main/docs/TektonConfig.md)
- [Operator Results configuration](https://github.com/tektoncd/operator/blob/main/docs/TektonResult.md)
- [Results local-database installation](https://github.com/tektoncd/results/blob/main/docs/install.md)
- [Chains configuration](https://github.com/tektoncd/chains/blob/main/docs/config.md)
- [Dashboard installation and exposure](https://tekton.dev/docs/dashboard/install/)
- [Archived Hub and maintained-fork direction](https://github.com/tektoncd/hub/blob/main/README.md)
- [Upstream CLI command construction](https://github.com/tektoncd/cli/blob/main/pkg/cmd/root.go)
- [Pact comparison source revision](https://github.com/theakshaypant/pact/tree/b0378a2c39f1a54aaeb39388d4a93cfe18bf221d)
