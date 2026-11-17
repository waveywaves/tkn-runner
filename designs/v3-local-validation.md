# V3 proposal: pre-commit validation from Tekton Tasks

> Superseded by [v4: the local Tekton ecosystem](v4-local-tekton-ecosystem.md).
> The confirmed requirement includes PipelineRuns and the full ecosystem.
> This restricted Task-only executor is retained as a historical proposal,
> not the implementation direction. The one-binary/existing-Podman packaging
> choice remains valid.

Date: 2026-11-16. Status: superseded; not implemented.
The current binary still exposes `start` and `delete`.

See the [current PoC assessment](local-validation-assessment.md) for evidence
and the explanation of architectural drift.

## Decision proposed

Make `tkn-runner` a foreground command that executes a strict subset of a
standard Tekton TaskRun against an isolated copy of the current working tree.
Use Podman directly. Return observed outcomes, logs, and files to a human or
agent. Do not start k3s, install Tekton, impersonate a kubelet, or expose a
Kubernetes API.

Retain standard Task syntax and the organization's validation commands. Give
up the promise of executing arbitrary Tasks and PipelineRuns. This is a
deliberately limited second Task executor, not the Tekton controller running
without Kubernetes. Compatibility must be demonstrated for the allowed subset.

| Approach | Decision and reason |
| --- | --- |
| Direct, bounded Task execution | Recommended for the portable pre-commit workflow. Small runtime surface, but owns the declared Task semantics. |
| Existing local cluster plus a convenient wrapper | Prefer this if broad Tekton compatibility becomes a requirement. Use it as the comparison baseline, not another backend in this iteration. |
| Continue the virtual-node provider | Do not extend it for this objective. It keeps the control plane and introduces worker compatibility obligations. |

There is no DAG scheduler, plugin framework, hosted service, IDE integration,
automatic pipeline pruning, or Docker/Wasm backend in the first release.
Those omissions do not prevent the four product requirements below.

## Requirements and observable outcomes

| Requirement | Proposed behavior | Acceptance evidence |
| --- | --- | --- |
| Code and organization-defined checks | Ordered Task steps run project tests, lint, coverage gates, and documentation checks. Thresholds and required outputs are explicit in the definition or invoked check scripts. | Independently breaking each category produces a failed run; missing required reports also fail. |
| Fast agent iteration | One process per invocation; reuse Podman's image store, not a resident control plane. No implicit skip or result cache. | Cold and warm measurements include source transfer and report collection. Proposed budgets appear below. |
| Agent-readable feedback | One versioned JSON report, actual step exits, attributed logs, artifacts, skipped-step records, and distinct failure categories. | An ordinary CLI caller can diagnose and retry without scraping prose or talking to Kubernetes. |
| Minimal setup and one definition | One TaskRun YAML file; one CLI invocation; Podman and Git are prerequisites. | Fresh-machine instructions work on Linux and macOS without `kubectl`, `tkn`, a kubeconfig, or a cluster. |

The validation definition is one input file; application source and check
scripts remain application files. No second runner-specific pipeline language
is introduced. An installed Podman runtime is still required, including its
Linux VM on macOS. This proposal does not claim zero setup.

### Confirmed distribution model

Distribute one `tkn-runner` executable per supported platform, using an existing
Podman installation. Podman must already be reachable; on macOS, the user
manages and starts Podman Machine. The runner does not bundle, install,
upgrade, or start Podman or its VM. If the runtime is unavailable, return an
actionable preparation error.

Container images use Podman's normal pull and cache behavior; they are not
bundled into the executable. Git remains a prerequisite for the source
enumeration described below. No Kubernetes installation or runtime-provisioning
subsystem is part of this distribution model. This packaging choice is
confirmed; the remaining execution-profile details are still a proposal.

## Concrete user workflow

Proposed commands, **not commands supported by the current binary**:

```sh
# Static profile validation; does not execute checks or prove that they pass.
tkn-runner check -f .tekton/local.yaml

# Execute the working-tree snapshot, including uncommitted files.
tkn-runner run -f .tekton/local.yaml --workspace source=. --output json

# Recover resources from an interrupted invocation, using its ownership record.
tkn-runner clean --run-id <run-id>
```

An agent calls `run` after editing, consumes the report, fixes a failing check,
and calls it again. The runner does not contain an agent, choose the checks,
modify source, watch the filesystem, commit, push, or deploy.

Illustrative input for a Go application that owns the three referenced `ci/`
scripts; it is not a runnable example shipped by this repository yet:

```yaml
apiVersion: tekton.dev/v1
kind: TaskRun
metadata:
  name: local-validation
spec:
  timeout: 5m
  params:
    - name: minimum-coverage
      value: "80"
  workspaces:
    - name: source
      emptyDir: {}
  taskSpec:
    params:
      - name: minimum-coverage
        type: string
        default: "80"
    workspaces:
      - name: source
    steps:
      - name: lint
        image: golang:1.25
        workingDir: $(workspaces.source.path)
        command: ["/bin/sh"]
        args: ["ci/lint.sh"]
      - name: unit-tests
        image: golang:1.25
        workingDir: $(workspaces.source.path)
        script: |
          #!/bin/sh
          set -eu
          mkdir -p .validation
          go test -coverprofile=.validation/coverage.out ./...
      - name: coverage-policy
        image: golang:1.25
        workingDir: $(workspaces.source.path)
        env:
          - name: MINIMUM_COVERAGE
            value: $(params.minimum-coverage)
        command: ["/bin/sh"]
        args: ["ci/check-coverage.sh", ".validation/coverage.out"]
      - name: documentation-policy
        image: golang:1.25
        workingDir: $(workspaces.source.path)
        command: ["/bin/sh"]
        args: ["ci/check-docs.sh"]
```

The coverage script reads `MINIMUM_COVERAGE`, fails on a missing report, and
checks the threshold. The documentation script must fail on missing or stale
generated content; merely invoking a generator is not a validation. These
scripts are reviewed policy, not commands inferred by a model. The image tag
keeps the illustration readable; organizations should pin a digest.

The local workspace flag explicitly seeds this run's `emptyDir` from local
files. A cluster run needs equivalent source provisioning; the YAML alone
does not upload source on Kubernetes. Both environments must use the same
check definitions, parameters, source bytes, and image platform for comparison.

## Execution architecture

```text
TaskRun YAML + workspace path
        |
strict decode and supported-profile validation
        |
capture definition, effective inputs, and source snapshot
        |
resolve images and create one run-owned workspace
        |
execute step containers sequentially
        |
collect real exits, logs, and artifacts; clean owned resources
        |
terminal report and CLI exit code
```

One writable copy is shared by the steps in a run. A fresh run starts from a
fresh snapshot. Use separate Podman containers and a shared named volume; a
Podman pod adds no necessary behavior while sidecars are excluded.

Use Podman container IDs and inspect/wait results rather than treating the
Podman client process's exit as proof of a check's outcome. Preserve containers
until status and logs are collected. Override image ENTRYPOINT correctly for
explicit commands; pass argv as an array, not through a host shell. Do not
inject Tekton's entrypoint binary, API credentials, or internal `/tekton` files.

## Supported profile for the first release

Pin the compatibility target to Tekton v1.6.0 initially, matching the existing
PoC. This is a test target, not a claim about downstream product support.

| Input | Allowed behavior |
| --- | --- |
| Document | Exactly one `tekton.dev/v1` TaskRun with inline `spec.taskSpec`. Reject multiple documents, duplicate keys, and unknown fields. |
| Metadata | `name` or `generateName` for human identity only; run ownership uses a separate unique ID. Reject other metadata initially, including annotations that might imply behavior. |
| Task description | `taskSpec.description` is informational. |
| Parameters | Declared string parameters with optional string defaults and TaskRun string values. Reject extra, duplicate, missing-required, array, and object parameters. |
| Steps | Unique names, fixed image references, explicit `workingDir`, literal/string-substituted `env`, and either `command`/`args` or `script`. Execute in order and stop after the first failed step. |
| Commands | Nonempty command array with optional args. No image-command inference or host shell interpretation. |
| Scripts | No shebang, or `#!/bin/sh`. The no-shebang case uses the pinned Tekton default preamble; an explicit shebang does not get an extra `set -e`. Reject other interpreters and `command`/`args` combined with script initially. |
| Substitutions | `$(params.NAME)` in args and env values; `$(workspaces.source.path)` in args, env values, and workingDir. Reject other Tekton substitutions and substitution in scripts, commands, or images. Shell `$(date)` inside a script is not a Tekton expression. |
| Workspace | Exactly one required workspace named `source`, mounted at `/workspace/source`, bound to plain `emptyDir: {}` and seeded by `--workspace source=PATH`. No custom mount paths, optional workspaces, subpaths, or additional volumes. |
| Deadline | Honor positive `spec.timeout`; use a documented five-minute default if absent. No unlimited `0` deadline in this profile. |

The pinned [script implementation](https://github.com/tektoncd/pipeline/blob/v1.6.0/pkg/pod/script.go)
uses `#!/bin/sh` followed by `set -e` when supplying a preamble. Test that
behavior rather than relying on the latest documentation's defaults.

The allowed paths are a whitelist, not a list of fields to interpret while
silently discarding everything else. Reject unsupported fields even if their
values look empty or default. Validate all steps before starting any workload.
Definition errors include field paths and reasons.

Explicitly reject PipelineRuns, Task/StepAction references, remote resolvers,
sidecars, retries, `when`, `onError`, step templates, results, per-step timeouts,
resource/security overrides, Pod templates, service accounts, Secrets,
ConfigMaps, downward API, additional workspace bindings, and status input.
Tasks needing those features remain cluster workloads. Do not automatically
drop dependency-producing Tasks or invent missing result values.

Kubernetes resource/security fields are rejected rather than weakened.
Initially require rootless Podman, preserve image USER, and never force root
to repair a workspace permission failure. Prove workspace writes for both
root and nonroot image users on the supported platforms. Image USER does not
reproduce OpenShift SCC enforcement, which is outside this profile.

## Source, environment, and ownership contracts

- Use Git to enumerate tracked paths at their current working-tree contents,
  excluding deletions, plus untracked nonignored paths. Exclude Git internals.
  Reject submodules initially. This validates the working tree, not the index
  that would become a commit.
- Reserve `.validation/` for generated outputs and exclude it from input.
  Reject tracked files under that reserved path rather than silently omitting
  source. Every run starts without old reports, so a stale coverage file cannot
  satisfy a missing-output check.
- Copy bytes and executable modes, preserving internal symlinks without
  following them. Reject escaping/absolute symlinks and special files. A
  project requiring `.git` metadata is outside the initial profile.
- Capture the definition once. Hash the actual copied files, relative paths,
  modes, and symlink targets; record the definition hash and effective params
  separately. Do not identify uncommitted input solely by HEAD SHA.
- Ask callers not to edit during capture; compare source manifests around
  capture and reject detected changes as `input_changed`. The report identifies
  the exact snapshot executed, not an atomic filesystem snapshot. Edits after
  capture cannot alter the running inputs.
- Seed a runtime-owned volume through a tar stream into a container attached
  to that volume. Do not rely on a macOS client path also existing inside
  Podman Machine. Use a pinned copy-helper image that preserves modes/symlinks;
  include its digest and transfer cost in the evidence.
- A step may modify its run's source copy, never the developer checkout.
  Copy `.validation/` back as artifacts after success or failure, rejecting
  symlinks and escaping paths during export. Check scripts are responsible for
  declaring required outputs by failing if they are missing.
- Resolve each distinct image once per invocation and execute the resolved
  identity for every matching step. Default to Podman's existing cached image
  when present; record digest/image ID and OS/architecture. Registry tags do not
  imply reproducibility, and there is no automatic cross-architecture emulation.
- Use Podman's ordinary isolated networking with outbound access. Do not mount
  host credentials, sockets, the whole home directory, or a kubeconfig into
  steps. Image-pull authentication can use the operator's Podman configuration;
  it is not passed into workload containers. No cluster network-policy claim.
- Give the run one deadline, starting after definition admission, covering
  source capture, runtime preparation, image resolution, source transfer, and
  steps. On timeout or cancellation, stop actual owned
  containers, observe their terminal states, collect available evidence, then
  remove resources. Use a separate bounded 30-second cleanup budget.
- Store an ownership manifest containing exact container and volume IDs under
  the user's cache directory with private permissions. Cleanup uses those IDs
  and matching run labels, never global prune or broad name prefixes.
  `clean --run-id` handles leftovers after SIGKILL or a crash; do not claim
  SIGKILL can be handled synchronously.

## Output contract

`--output json` writes exactly one versioned terminal JSON object to stdout.
Progress goes to stderr. Store raw stdout/stderr per step outside the source
snapshot under the run directory; stream to files rather than unbounded memory.
Set a documented 64 MiB limit per stream: exceeding it stops the run with an
evidence-storage error, not a silent successful truncation.

The report contains:

- Schema version, run ID, outcome, reason, and observed start/end times.
- Source snapshot and exact definition digests, effective params, runner
  version, and resolved image identities/platform.
- Each step's outcome (`passed`, `failed`, `not_run`, or `interrupted`), actual
  exit code when observed, timings, and stdout/stderr paths. Never invent an
  exit code for a step that did not start.
- Artifact paths and hashes; unsupported field diagnostics when applicable.
- Primary failure, cleanup errors, and remaining owned resource IDs separately.

| CLI exit | Meaning |
| --- | --- |
| 0 | All declared checks executed and passed; evidence was stored and cleanup succeeded. |
| 1 | A check failed. |
| 2 | Invalid/unsupported definition, invalid workspace, or input changed during capture. |
| 3 | Runtime, preparation, evidence-storage, or cleanup error. |
| 124 | Run deadline exceeded. |
| 130 | User cancellation. |

Cleanup errors force exit 3 while preserving the primary outcome and its
reason in the report. No failure, skipped requirement, unsupported input, or
incomplete cleanup may produce exit 0. If the runtime connection is lost,
report unknown container state and recovery IDs rather than claiming success.

Logs and `.validation/` files are evidence, not Tekton Results. Coverage and
documentation gates consume files within the shared workspace; dynamic
cross-Task result substitution is not needed for the initial use case.

Local checks are advisory feedback, not non-falsifiable organizational approval.
An agent able to modify both code and policy can weaken a check. Record policy
and input identities, review policy changes, and rerun authoritative checks in
CI from protected definitions. Do not claim the local report establishes trust
in an untrusted workstation.

## Exact keep, remove, and replace plan

These are proposed implementation changes. None of these code removals occur
in the documentation change.

| Existing path or behavior | Action | Replacement or reason |
| --- | --- | --- |
| `cmd/tkn-runner/main.go` | Rewrite CLI wiring. Remove `startCommand` and `deleteCommand` when the new path is ready. | `check`, `run`, and run-scoped `clean`; no daemon/bootstrap workflow. |
| `pkg/controlplane/k3s/` | Remove all three Go files. | Nothing. This product does not require a control plane. |
| `pkg/embedded/k3s.go`, `pkg/embedded/k3s-binary` | Remove. | Nothing; do not embed another Kubernetes distribution. |
| `pkg/manifests/` including embedded CRDs and `controller/release.yaml` | Remove. | No CRD installation, webhook, controller readiness, or manifest apply path. |
| `pkg/config/config.go` | Remove cluster configuration. | Ordinary CLI options and the TaskRun definition; no replacement configuration framework. |
| `pkg/virtualkubelet/node.go`, `provider.go` | Remove. | Direct observed run/step outcomes, not Node/Pod resources or synthetic Kubernetes status. |
| `pkg/virtualkubelet/executor.go` | Replace, not move wholesale. | Small Podman executor consuming admitted steps. Reuse the subprocess/streaming approach, not Pod translation, root overrides, credentials, fake platform commands, sidecar heuristics, or volume naming. |
| `go.mod`, `go.sum` | Recalculate after replacing imports. | Use pinned Tekton v1 API types and an existing strict YAML library where suitable; remove unused direct client-go/apiextensions dependencies. API types can retain transitive Kubernetes libraries; do not promise a dependency-free binary. |
| `pipeline` gitlink | Remove from the distribution once any local work is preserved explicitly. | A pinned module and versioned test fixtures, not a checked-out upstream runtime. Never delete a dirty checkout as part of this migration. |
| `examples/simple-taskrun.yaml`, `test-taskrun.yaml`, `tekton-runner/test-final.yaml` | Replace hello-world claims with executable validation examples in `examples/`. | A small project covering lint, tests, coverage, and documentation failures; keep one canonical invocation. |
| `README.md` | Retain and rewrite for the selected contract. | Setup, supported subset, output/exit behavior, real measurements, and explicit limitations. |
| `designs/v1-managed-by-controller.md`, `v2-virtual-kubelet.md` | Retain as historical records with notices. | No systemd, kcp, Flannel, hosted service, or multi-runtime roadmap in v3. |
| Global volume prune and broad container deletion | Remove entirely. | Exact run-owned resource cleanup and targeted crash recovery. |

Proposed additions use one private `internal/runner` package, not a backend
interface hierarchy:

| Proposed path | Responsibility |
| --- | --- |
| `internal/runner/definition.go` | Strict decode, supported-field admission, params, and limited substitution. |
| `internal/runner/workspace.go` | Source capture, digest, volume transfer, and safe artifact export. |
| `internal/runner/podman.go` | Container creation/start/wait/inspect/log collection and owned cleanup. |
| `internal/runner/runner.go` | Sequential execution, deadline, cancellation, and failure classification. |
| `internal/runner/report.go` | Versioned report serialization and private run/ownership records. |
| Neighboring `*_test.go`, `testdata/`, `test/integration/` | Deterministic unit fixtures, real Podman lifecycle tests, and pinned-Tekton comparisons. |
| `examples/local-validation/` | Runnable sample application, one TaskRun file, and real organization-check scripts. |
| `.github/workflows/test.yaml`, `.github/workflows/release.yaml` | Automated checks and downloadable platform binaries/checksums once the pilot gates pass. |

Use exported API types from the pinned Tekton module, not copied controller
internals. Check the actual binary/dependency cost before making packaging
claims. Do not introduce internal helpers as public APIs.

## PaC boundary

The first release accepts an explicitly selected inline TaskRun, not a PaC
PipelineRun. Its value is applying agreed checks to dirty source, not predicting
a future provider event.

Reuse project check scripts in both the local Task and cluster CI. If a project
needs a selected Pipeline task later, add an explicit resolution/export step
only after specifying how references, params, dependencies, and policy identity
are preserved. `tkn pac resolve` alone outputs a PipelineRun; piping it directly
into this TaskRun-only runner is not a supported integration.

Keep event/CEL testing and full PipelineRun execution on their existing paths.
No automatic fallback to a remote cluster: that changes where code executes
and where credentials are used.

## Migration sequence and release gates

Each stage ends with a runnable, checked increment. The stages are work to do,
not claims that the current repository already implements them.

1. **Agree the contract and fixtures.** Approve this profile, inventory
   representative project validations, and turn supported/rejected YAML and
   exit/report behavior into fixtures. Reject unsupported input before Podman
   workload creation. If essential checks require excluded features, decide
   whether to use the cluster path rather than silently expanding scope.
2. **Build the first complete run.** Add `run` alongside the historical CLI
   temporarily, with one real sample project. Implement source capture,
   shared workspace, sequential steps, actual exits, logs, and JSON. No fake
   success or stub paths. Match command, argv, script, and param fixtures
   against pinned Tekton v1.6.0.
3. **Make failure and recovery dependable.** Add timeout/cancellation,
   nonroot workspace tests, failed-run artifacts, stream limits, concurrent-run
   isolation, ownership records, and targeted crash cleanup. Prove unrelated
   containers/volumes and the developer checkout remain untouched.
4. **Exercise all four product requirements.** Run the sample's lint, unit,
   coverage, and docs checks on Linux and macOS, deliberately fail each, and
   demonstrate an external caller fixes source and reruns without a commit.
   Publish baseline and candidate measurements with methodology.
5. **Cut over and remove the old runtime.** Once those gates pass, remove the
   old packages/commands listed above, tidy dependencies, update examples and
   release docs, and produce verified binaries. Preserve local submodule work
   before any gitlink/checkout change. Do not retain two permanent backends.

### Correctness acceptance

- Dirty files, untracked files, deletions, ignored paths, executable bits, and
  internal symlinks produce the expected snapshot. Escaping links, submodules,
  and detected capture changes fail explicitly.
- An image with ENTRYPOINT, arguments containing spaces/metacharacters,
  parameter values with shell syntax, both allowed script forms, and a nonroot
  image behave according to the profile. Later steps are `not_run` after failure.
- Shared files reach later steps, but no files from a previous run or unrelated
  run leak in. Old `.validation/` output is not imported. Exported artifacts
  remain available after a failed check.
- JSON is parseable with logs on both streams; exits distinguish check failure,
  preparation/runtime failure, unsupported input, timeout, and cancellation.
- Timeout/cancellation tests subscribe to an exact container/step-start signal
  before triggering the event. Use bounded waits, not fixed sleeps or
  timing-dependent polling. Recovery tests prove actual container termination.
- The same admitted TaskSpec, input bytes, params, and platform-specific image
  identities run against a warm pinned Tekton installation. Compare exits,
  execution order, fixture outputs, and file effects. Provision the cluster
  workspace externally; do not add a Kubernetes backend to the shipping binary.
- Automated tests and an actual Podman integration run must pass before
  cutover. Cross-compilation alone is not Linux/macOS runtime validation.

### Performance acceptance

These are proposed pilot budgets to review, not measurements or promises:

- Warm no-op fixture: three steps, cached images, a 10 MiB source tree with at
  most 1,000 files; p95 end-to-end at most five seconds.
- Added p95 overhead relative to equivalent direct Podman execution and source
  transfer: at most two seconds on the same host.
- Measure at least 20 warm invocations, report p50/p95 and raw timings, and
  separately record cold image pulls, machine startup, source capture/transfer,
  execution, evidence collection, cleanup, and binary size.
- Compare with a warm local Kubernetes/Tekton baseline, not only a freshly
  created cluster. Include total host/VM memory and disk; do not compare a Go
  process's RSS with a whole cluster's footprint.
- No runner-owned control-plane process remains idle after completion. Podman
  Machine may remain running; account for it rather than claiming zero overhead.
- Include a real project workload and a larger source tree alongside the
  no-op fixture. No validation-result cache in this iteration; cached images
  must not cause checks to be skipped.

If these budgets or correctness gates fail, publish the result and revise the
scope before adding features. Retain the direct runner only if measured setup,
latency, or footprint benefits justify its compatibility maintenance.

## Effort and approval boundary

Planning estimate: approximately three to five engineer-weeks for a usable
Linux/macOS pilot including the sample, real-runtime tests, comparison evidence,
and documentation. This is an estimate, not a delivery commitment. Workspace
permissions across Podman Machine, image command behavior, cancellation, and
source-transfer costs are the main uncertainties. A supported product needs
additional release, support, upgrade, and security work.

The decision to approve is the contract: a constrained Task executor rather
than arbitrary PipelineRun compatibility, with Podman as the sole initial
runtime. Review the sample against actual project needs before implementing
stage 2. This documentation change approves no runtime removal, publication,
push, or release.
