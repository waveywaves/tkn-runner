# Validation evidence

Workstation: macOS ARM64, 2026-11-17.

## Static checks

`bash -n bootstrap.sh run.sh` passed. `bun test fixtures.test.js` passed all
3 tests with 31 assertions on Bun v1.3.9. The tests parse all fixtures and check
named resource/workspace/Secret bindings and both bundle resource identities.
The pinned v0.65.2 release manifest was independently parsed to confirm that it
includes remote resolvers, the expected feature-flag ConfigMap, and the
`tekton-pipelines-remote-resolvers` Deployment with container name `controller`.
The installed `tkn bundle push --help` confirms the flags used in the harness.
OpenSSL v3.6.3 supports the required certificate extension option.

These checks do not prove admission or execution. Language-server diagnostics
were unavailable: Bash, YAML, JSON and JavaScript language servers are not
installed. No project dependency was added solely for editor diagnostics.

## Runtime blocker

No runtime case passed or ran to completion. An owned Kind v0.22.0 bootstrap
using `kindest/node:v1.29.2`, Podman provider, cluster `tkn-resource-poc`, and
private kubeconfig `/tmp/tkn-resource-poc.kubeconfig` failed during kubeadm:

```text
podman exec --privileged tkn-resource-poc-control-plane kubeadm init ...
exit status 125
unable to connect to Podman socket: Get "http://d/v5.6.2/libpod/_ping": EOF
```

The workstation then reported `ENOSPC`; disk availability subsequently recovered
to about 4.1 GiB. Podman Machine was stopped. Starting the existing
`podman-machine-default` returned success, but the subsequent Kind attempt failed
before creating a cluster:

```text
podman info --format json
exit status 125
dial tcp 127.0.0.1:58927: connect: connection refused
```

Machine inspection again showed it stopped. Its configured resources are 8 CPUs,
8 GiB RAM and a 100 GiB virtual disk. The reason it stops has not been established.
No VM was deleted, resized or recreated, and no unrelated clusters were altered.
An initial failed Kind attempt may have left owned resources; inspect the exact
`tkn-resource-poc` name once Podman is healthy before deleting anything.
The kubeconfig file was absent after the failed bootstrap.

## Remaining runtime gates

### Local demo rehearsal follow-up

The host cleanup removed the stopped failed `tkn-resource-poc` cluster and
partially cleared the measured 62 GiB Go compilation cache. The cache command
hit its deadline; host free space nevertheless rose from 1.7 GiB to 39 GiB.
Unrelated running clusters and volumes were preserved.

Bootstrap subsequently passed on local Podman:
`/tmp/resource-poc-demo-20261117`, cluster `tkn-resource-poc-16353-20316`.
The Kubernetes control plane, storage provisioner, Tekton controller, webhook
and remote resolver initially became ready.

The third rehearsal, in `resource-poc-p45md`, demonstrated:

- Missing named Pipeline: `bindings-mmq72`, failed as expected.
- Missing Secret: `bindings-2wv5s`, failed with `CreateContainerConfigError`.
- Configured inputs: `bindings-xqw7d`, succeeded.
- Evidence: `/tmp/resource-poc-demo-20261117/evidence.rXzuQz`.

Runtime testing corrected two assumptions: Tekton terminally fails the TaskRun
for the missing Secret, requiring a new run, and namespace ServiceAccount
creation must not be left to an asynchronous controller race.

The public Git catalog request timed out at the resolver's one-minute global
limit. A small local Git fixture service now explicitly replaces it; its real
Git commit is recorded in the effective submitted run. That new service and
the OCI bundle case have NOT yet passed runtime validation.

The next rehearsal was interrupted by local infrastructure failure:
`resource-poc-k8hrd/bindings-gqqw9` never reached its verify Task.
The Tekton controller reported `RunContainerError: context deadline exceeded`
and the Kubernetes API intermittently returned `TLS handshake timeout`.
The events controller had seven restarts, including exit code 137.
The host had 34 GiB free but heavy compressed-memory/swap activity. The shared
8 GiB Podman VM also hosts the unrelated running `pharos-poc-20261117` and
`release-trace` clusters. Memory contention is a candidate, not a proven cause.
The demo remains unverified end to end; no OpenShift cluster was contacted.

Run bootstrap and the harness on a healthy Podman machine. Verify:

1. Missing Pipeline and Secret observations, then successful PVC/ConfigMap-backed
   cross-Task handoff and `finally`.
2. Missing remote Git file failure, then successful pinned local-Git Task
   resolution, workload execution and returned Task result.
3. Reachable TLS registry returning a missing-manifest error, then local bundle
   push, digest-based Pipeline and Task resolution and expected Task result.
4. Retained execution evidence and termination of the port-forward on exit.

Do not treat this PoC as runtime-certified until those gates pass. PaC matching,
provider credentials, private Git, cluster resolver RBAC, dirty-tree upload,
full-suite components and embedded-CLI packaging are outside this dependency
spike and are not implied by its static checks.
