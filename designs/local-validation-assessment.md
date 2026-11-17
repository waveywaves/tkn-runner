# Local validation: assessment of the current PoC

Date: 2026-11-16.
Assessed revision: [`f53b12d3a457744ba5c396e4cf4adcccf3324fe3`](https://github.com/waveywaves/tkn-runner/commit/f53b12d3a457744ba5c396e4cf4adcccf3324fe3).

This describes existing code, not a completed runtime validation. The current
proposal is [v4: the local Tekton ecosystem](v4-local-tekton-ecosystem.md).
The later requirement for PipelineRuns and the full ecosystem supersedes the
restricted Task-only recommendation; it does not invalidate the PoC findings.

## Product objective

Catch application and organization-defined validation failures against a
developer's current files before a commit or pull request. A coding agent should
be able to run the checks, identify a failure, change the code, and run them
again. This requires:

1. Code checks such as lint and unit tests, alongside coverage, documentation,
   and other organization-defined checks.
2. Low enough setup and execution overhead for repeated local iteration.
3. Dependable machine-readable outcomes and actionable logs.
4. One version-controlled validation definition, independent of a particular
   editor or agent.

Those initial validation goals alone did not require every PipelineRun feature.
The subsequently confirmed requirement does: PipelineRuns and the full Tekton
ecosystem must run locally, as specified in v4. Executing checks is also
different from an API/schema dry-run: an accepted manifest is not evidence
that tests passed.

## What the PoC implements

```text
TaskRun / PipelineRun
        |
k3s API and standard Tekton controller
        |
Kubernetes Pod
        |
Custom virtual-node provider
        |
Podman containers
```

[main.go](../cmd/tkn-runner/main.go) starts k3s, installs CRDs, starts the
provider, and then applies the embedded Tekton release. The embedded controller
manifest identifies Tekton v1.6.0.

This preserves Tekton reconciliation and pipeline orchestration. It does not
replace the DAG engine with a new implementation. However,
[node.go](../pkg/virtualkubelet/node.go) and
[provider.go](../pkg/virtualkubelet/provider.go) implement a custom Pod watcher
and lifecycle adapter, taking responsibility for behavior normally supplied by
the kubelet. An unchanged controller does not make that adapter equivalent.

The macOS path uses an agentless k3s container. The Linux native startup does not
currently pass `--disable-agent`. These are not equivalent runtime paths.
Podman on macOS still requires a Linux VM.

## Gaps that affect local validation

| Area | Source evidence | Consequence |
| --- | --- | --- |
| Actual working tree | The CLI has only `start` and `delete`; no source ingestion. | Running a Task does not automatically validate dirty or untracked application files. |
| Workspace identity | `getVolumeName` in [executor.go](../pkg/virtualkubelet/executor.go) includes the Pod name; ordinary mounts become named volumes. | A shared PVC is not translated into shared storage across TaskRun Pods. Host source paths are not honored as such. |
| Container commands | `executeContainer` appends `container.Command` after the image without overriding image ENTRYPOINT. | An image with ENTRYPOINT can execute different argv than Kubernetes would. |
| Environment and identity | ConfigMap/Secret env references return empty strings after warnings; containers are forced to run as root. | Missing inputs and different permissions can change validation outcomes. |
| Lifecycle evidence | `updatePodStatus` fabricates aggregate per-container exits and timestamps, marks readiness without probing, and appends status entries. | Status is not a reliable account of which steps actually ran or failed. |
| Cancellation | `CreatePod` executes with `context.Background`; `UpdatePod` replaces a cached object. | Lifecycle updates are not a reliable cancellation or live downward-API update mechanism. |
| Logs and results | No kubelet logs endpoint or termination-message extraction; the embedded feature flags select `results-from: sidecar-logs`. | The configured results path lacks the log-serving implementation it needs. |
| Sidecars | Separate containers share a named network; sidecars start asynchronously without readiness gating. | Shared networking is not a shared Pod network namespace or a readiness guarantee. |
| Cleanup | `deleteCommand` invokes `podman volume prune -f` without an ownership filter. | Deletion can affect unused volumes unrelated to this runner. |

The [v2 logging and results sections](v2-virtual-kubelet.md#8-log-collection)
already acknowledge important gaps. Earlier README claims of full support,
automatic feature inheritance, working results, and VM-free macOS execution
should not be treated as verified capabilities.

## How the design drifted

There is one commit in the inspected, non-shallow repository history:
`f53b12d` (`feat: k8s less tekton`, 2025-11-03). Both design documents and the
implementation are in that snapshot. Consequently, the repository does not
establish a chronological sequence of implementation decisions or their
authors' motivations. The following is an architectural reading of the
documents, not a reconstructed commit history.

1. **The desired outcome was replaced by a compatibility objective.**
   Local feedback needs checks, files, execution, and outcomes. The
   [v1 design](v1-managed-by-controller.md) instead makes full API compatibility
   and unchanged `kubectl`/`tkn` usage key benefits. That brings an API server,
   stored resources, authentication, and controller lifecycle into the product.
2. **The scope expanded beyond the developer loop.**
   V1 includes edge deployments, legacy integrations, multiple execution
   backends, system services, and multi-tenant kcp/SaaS deployments. Those
   requirements do not follow from running pre-commit validations.
3. **V2 reduced one implementation burden by accepting another.**
   Reusing the standard controller avoids rebuilding TaskRun/PipelineRun
   reconciliation. Moving the integration point down to Pods requires
   implementing worker semantics: storage, credentials, networking, entrypoint
   behavior, readiness, logs, termination messages, and cancellation.
4. **Compatibility work displaced the useful end-to-end slice.**
   The code addresses service-account files, downward-API files, fake nodes,
   and API connectivity, but still has no command that takes the current source
   tree and returns trustworthy validation results.
5. **The compatibility promise lacked a test boundary.**
   There are simple TaskRun examples but no Go tests or measured compatibility
   suite. Infrastructure starting and a container printing a message are not
   acceptance criteria for an agent validation loop.

The initial recommendation was a narrower product with a bounded direct Task
executor. The confirmed full-ecosystem requirement rules out that restriction.
The revised solution retains real Kubernetes worker semantics locally instead
of adding exceptions to the virtual-node provider. See v4 for the updated
compatibility contract and the distinction between one distributed binary and
the containerized services it manages.

## PaC, Podman pods, and Wasm

- **PaC:** [`tkn pac resolve`](https://pipelinesascode.com/docs/cli/resolve/)
  already resolves PipelineRun templates locally. Its documentation notes that
  a `git-clone` Task still needs a remotely accessible source revision. Template
  resolution does not ingest uncommitted application files.
- **Event evaluation:** [`tkn pac cel`](https://pipelinesascode.com/docs/cli/cel/)
  uses payloads and headers and documents changed-file limitations. Keep event
  selection separate from running an explicitly selected local check.
- **Source transfer:** [CLI PR #1334](https://github.com/tektoncd/cli/pull/1334)
  experimented with copying local files into a PVC through a temporary Pod.
  It is prior art, not evidence of a current supported CLI workflow.
- **Podman pods:** [Shared namespaces](https://docs.podman.io/en/latest/markdown/podman-pod-create.1.html)
  could address some sidecar networking gaps, but do not supply Tekton
  reconciliation, result collection, or faithful Kubernetes lifecycle status.
  The current executor does not use Podman pods.
- **Wasm:** [WASI](https://wasi.dev/) serves software compiled to WebAssembly.
  Supporting selected Wasm tools is a separate toolchain contract, not a
  drop-in replacement for existing Linux container images and shell Tasks.

## Verification and remaining evidence

On 2026-11-16, `go test ./...` passed at the assessed revision using
Go 1.25.0 on darwin/arm64. All six packages reported `[no test files]`.
This establishes compilation and the command's successful completion, not
runtime correctness.

No live TaskRun, cross-platform execution, or performance benchmark was run
for this assessment. V2's resource tables include estimates, not a reproducible
benchmark record. No claim that this PoC is faster or smaller than a warm local
Kubernetes installation is established here.

The [Podman machine documentation](https://docs.podman.io/en/latest/markdown/podman-machine.1.html)
confirms the VM prerequisite on macOS. The
[Podman run documentation](https://docs.podman.io/en/latest/markdown/podman-run.1.html)
documents the explicit ENTRYPOINT override needed by the command translation.
