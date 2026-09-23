# RFC: Unified Backup and Restore cancellation

Status: discussion draft.

## Purpose and scope

Let users stop a running Backup or Restore while keeping its API object and available diagnostics for inspection. Deletion cannot serve this purpose because it removes the object and eventually its diagnostics.

This RFC combines the Backup and Restore cancellation proposals in [#9284](https://github.com/velero-io/velero/pull/9284) and [#10509](https://github.com/velero-io/velero/pull/10509). It proposes shared behavior and implementation rules; separate workflow designs cover the details.

## User-facing behavior

### Requesting cancellation

**Proposal:** add an optional, one-way `spec.cancel` field to both resources:

```yaml
spec:
  cancel: true
```

Once true, it cannot be reset. Repeating the request has no additional effect. The CLI would provide:

```text
velero backup cancel NAME
velero restore cancel NAME
```

Both commands accept `--wait` to report the final phase. A successful API write only records the request; it does not confirm cancellation.

A field keeps the API simple, but requires permission to update or patch the Backup or Restore. Kubernetes RBAC cannot limit that permission to `spec.cancel` alone.

**Open question:** do we need cancellation-only permissions or a record of each request? A separate request resource would provide these at the cost of another object to manage.

### Status and completion

**Proposal:** add two phases:

```text
cancellable phase -- request accepted --> Cancelling
Cancelling -- work finishes or deadline expires --> Cancelled
```

- Recording `Cancelling` accepts the request. Velero then stops starting new work and attempts to cancel work already underway.
- If normal completion wins first, its final phase stays unchanged. The request remains visible but is too late to change the outcome; `--wait` reports that normal outcome.
- Once accepted, cancellation cannot be overwritten by normal completion. Child operations that already completed or failed keep their status.

`Cancelled` means Velero has stopped waiting after attempting cancellation through the available interfaces. It does **not** guarantee that all external work stopped or roll back changes already made. Waiting ends no later than the recorded deadline, with a concise report of any known unconfirmed work. This avoids both waiting forever and abandoning useful cancellation attempts immediately.

**Open questions:** agree phase spelling, the time limit and its configuration, and how status and CLI output show late requests or unconfirmed work. See the [decision summary](#decision-summary).

### Data and diagnostics

- **Backup:** retain existing backup data, snapshots, logs, errors, and operation status until normal deletion or expiration. A cancelled Backup cannot be used for restore.
- **Restore:** retain restored resources, destination volumes, and partial data. Cleanup may remove only owned temporary resources, never destination storage, including during in-place restore.

Early cancellation may leave no archive or diagnostics. Downloads and CLI output should distinguish files never created from failed uploads. Saving diagnostics must not delay cancellation indefinitely.

## Shared implementation rules

### Controller responsibility

**Proposal:** a dedicated cancellation controller owns the deadline, coordinates cancellation, resumes it after a restart, and records the final outcome. Every controller handling a cancellable phase stops normal processing once cancellation is accepted; child controllers keep their own cancellation protocols.

Deadline handling must stay responsive even when workflow, plugin, or provider calls are blocked. The controller must act only on children belonging to the correct Backup or Restore, identified by namespace and UID.

Status and metrics must avoid secrets and unbounded identifiers; reporting must not grow without limit.

**Open question:** should a dedicated controller own this responsibility, or can existing Backup and Restore controllers provide the same clear ownership and independent deadline handling?

### Stopping work

Refresh cancellation regularly without reading the API before every item. Check it before starting work, between items/actions, while waiting, and before normal finalization. A call already underway may finish, but must recheck cancellation before starting its next step.

| Work | Cancellation path |
|---|---|
| Not started | Do not start it. |
| PodVolumeBackup/PodVolumeRestore, DataUpload/DataDownload | Set the child `spec.cancel`; its controller handles cancellation. |
| v2 item action with a known operation ID | Call `Cancel(operationID, parent)` and poll progress until it finishes or the deadline expires. Plugin cancellation may be a no-op. |
| No effective cancellation path | Keep available outcome information without waiting past the deadline. Examples include native/CSI snapshot creation, in-flight synchronous calls or writes, and operations whose IDs were lost. |

### Restarts and status updates

- Record acceptance time and a fixed deadline on the Backup or Restore. Repeated requests, retries, configuration changes, and restarts must not extend it.
- On restart, process pending requests and resume accepted cancellations using known children and saved operation IDs. Report missing IDs or unconfirmed work; do not resume normal work for an accepted cancellation.
- Resolve completion versus cancellation with a conditional update based on current status. Retrying an old copy must not overwrite cancellation. Late results may add diagnostics, but cannot reopen the operation or change its cancellation outcome.

## Workflow-specific requirements

Each workflow design must identify its cancellation checks, child discovery, supported plugins/storage modes, and safe cleanup behavior. Two areas need explicit policies:

- **Hooks:** Restore must not run success-dependent hooks or falsely signal successful volume restoration. For Backup, how should a post-hook release an application paused by a successful pre-hook?
- **Deletion:** should deleting a running Backup cancel it first? Define how concurrent deletion, late uploads, and dependent Restores still cancelling are handled.

## Rollout and validation

1. Agree the API, phases, deadline, controller ownership, and workflow policies.
2. Update CRDs, generated clients, permissions, controllers, and CLI together. Review every phase-dependent path, including recovery, synchronization, finalization, deletion/expiration, restore-source validation, downloads, and metrics. Older servers may ignore cancellation; older CRDs may reject the new phases.
3. Test cancellation before work starts, child/plugin limitations, completion races, restarts, hooks, data preservation, deletion races, and late results. Confirm cancelled Backups cannot be restored and blocked calls cannot prevent deadline handling.

## References

- [#9284: Backup Cancellation Design](https://github.com/velero-io/velero/pull/9284).
- [#9284: bounded wait discussion](https://github.com/velero-io/velero/pull/9284#discussion_r2363925189).
- [#9284: cancellation in the existing state machine](https://github.com/velero-io/velero/pull/9284#discussion_r2430934181).
- [#9284: backup payload and diagnostic retention discussion](https://github.com/velero-io/velero/pull/9284#discussion_r2384454776).
- [#10509: Explicit Restore cancellation](https://github.com/velero-io/velero/pull/10509).

## Decision summary

Proposals remain open until recorded as decided with a brief rationale.

### Open questions

| Topic | Decision needed |
|---|---|
| API and permissions | Use `spec.cancel` or a separate request resource? Are cancellation-only permissions and request history needed? How is the one-way field enforced? |
| Controller ownership | Dedicated cancellation controller or existing workflow controllers? How are blocked calls kept from delaying the deadline? |
| Deadline | What is the default time limit, and is configuration global, per server, or per operation? |
| Status and UX | Which phase spelling, status fields, and public warnings/conditions? How do CLI commands and metrics show late requests and unconfirmed work? |
| Diagnostics | What remains available after early cancellation, and how do downloads distinguish absent files from failed uploads? |
| Deletion | Cancel before deleting? How should direct Kubernetes deletion, late uploads, and dependent Restores interact, and how long may deletion wait or retry? |
| Hooks | When and how should Backup run a paired post-hook after cancellation? |
| Supported modes | Which storage modes allow safe temporary-resource cleanup? Which plugin/provider operations may remain running or unconfirmed? |

### Closed / decided

None recorded yet.
