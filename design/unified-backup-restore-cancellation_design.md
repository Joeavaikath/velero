# RFC: Unified Backup and Restore cancellation

Status: discussion draft.

## Contents

- [Purpose and scope](#purpose-and-scope)
- [Terminology](#terminology)
- [User-facing behavior](#user-facing-behavior)
  - [Requesting cancellation](#requesting-cancellation)
  - [Status and completion](#status-and-completion)
  - [Deadline](#deadline)
  - [Data and diagnostics](#data-and-diagnostics)
- [Shared implementation rules](#shared-implementation-rules)
  - [Controller responsibility](#controller-responsibility)
  - [Stopping work](#stopping-work)
  - [Restarts and status updates](#restarts-and-status-updates)
- [Workflow-specific requirements](#workflow-specific-requirements)
- [Rollout and validation](#rollout-and-validation)
- [References](#references)
- [Decision summary](#decision-summary)
  - [Open questions](#open-questions)
  - [Closed / decided](#closed--decided)

## Purpose and scope

Let users stop a running Backup or Restore while keeping its API object and available diagnostics for inspection. Deletion cannot serve this purpose because it removes the object and eventually its diagnostics.

This RFC combines the Backup and Restore cancellation proposals in [#9284](https://github.com/velero-io/velero/pull/9284) and [#10509](https://github.com/velero-io/velero/pull/10509). It proposes shared behavior and implementation rules; separate workflow designs cover the details.

## Terminology

| Term | Meaning |
|---|---|
| **Cancellation request** | The user's intent, expressed through `spec.cancel: true`. A request does not mean cancellation has been accepted. |
| **Cancellation acceptance** | The controller commits to cancellation by recording `Cancelling` and the cancellation timestamps. Normal processing must not resume afterward. |
| **Cancellation handling** | Work performed after acceptance: stopping normal processing, attempting to cancel running work, cancellation cleanup, and diagnostic collection. |
| **Cancellation timeout** | The duration allowed for cancellation handling, measured from acceptance. |
| **Cancellation deadline** | The fixed time when Velero must stop waiting and transition to `Cancelled`, subject to controller and API availability. |
| **Terminal phase** | A phase that marks a Backup or Restore as finished, including `Cancelled`. External work may still be running. |
| **Finalization** | Workflow steps performed at the end of normal processing. Finalization is not a synonym for reaching a terminal phase. |
| **Unconfirmed work** | Work whose outcome Velero cannot determine. It may still be running or may already have finished. |
| **Cancellation cleanup** | Best-effort removal of eligible payloads or temporary resources while preserving the resources and diagnostics required by this design. |

Use **reaches `Cancelled`** for the terminal transition, rather than “cancellation completed,” which could imply that all work has stopped.

## User-facing behavior

### Requesting cancellation

**Decided:** use an optional `spec.cancel` field on both resources. An optional `spec.cancellationTimeout` overrides the server's cancellation timeout for this request:

```yaml
spec:
  cancel: true
  cancellationTimeout: 1m
```

Setting `spec.cancel: true` requests cancellation. The request is accepted when the Backup or Restore enters `Cancelling`.

- Cancellation can be requested in any non-terminal phase, including before work starts.
- If the Backup or Restore reaches a terminal phase before acceptance, the cancellation request has no effect.
- After acceptance, changing either cancellation field cannot resume normal processing or change the cancellation deadline.
- Repeating the request has no additional effect.

Once status records cancellation acceptance, controllers must not resume normal processing, even if `spec.cancel` is later cleared. This does not require API validation rules that prevent clearing the field, keeping the feature available on all Kubernetes versions Velero supports.

As with existing Backups and Restores, permission to patch the resource also allows changes to its status. This RFC does not add a status subresource or separate status protection.

`Schedule.spec.template.cancel` is invalid and must be rejected. Users pause a Schedule with `spec.paused` rather than creating immediately cancelled Backups.

The CLI would provide:

```text
velero backup cancel NAME
velero restore cancel NAME
```

Both commands accept `--wait` to report the terminal phase. A successful API write only records the cancellation request; it does not confirm acceptance or that the Backup or Restore has reached `Cancelled`.

A field keeps the API simple, but requires permission to update or patch the Backup or Restore. Kubernetes RBAC cannot limit that permission to `spec.cancel` alone.

### Status and completion

**Decided:** use `Cancelling` and `Cancelled` for the two new phases:

```text
non-terminal phase -- request accepted --> Cancelling
Cancelling -- cancellation handling ends or deadline expires --> Cancelled
```

- After acceptance, Velero starts no new normal workflow work and attempts to cancel work already underway. It may start work required for cancellation handling.
- If normal completion wins first, its terminal phase stays unchanged. The request remains visible but is too late to change the outcome; `--wait` reports that normal outcome.
- Once the request is accepted, normal completion cannot replace `Cancelling` or `Cancelled` with another terminal phase. Child operations that already completed or failed keep their status.

The Backup or Restore may reach `Cancelled` before the cancellation deadline. What must happen first depends on where the workflow observes the cancellation request and what work has already started.

`Cancelled` means Velero has finished its cancellation handling or stopped waiting because the deadline expired. It does **not** guarantee that all external work has stopped or roll back changes already made. Velero reports any known unfinished or unconfirmed work.

**Decided:** add `status.cancellation` when the request is accepted, alongside the existing status metadata:

| Field | Meaning |
|---|---|
| `acceptedAt` | Time of cancellation acceptance. |
| `deadline` | Cancellation deadline. |
| `reason` | Why cancellation handling ended. |
| `conditions` | Progress of cancellation handling, known unfinished or unconfirmed work, and cancellation cleanup or diagnostic outcomes. |

Update conditions during cancellation handling. Set the reason and the existing `status.completionTimestamp` when recording `Cancelled`.

### Deadline

**Decided:** configure a server-wide cancellation timeout with a server argument. `spec.cancellationTimeout` can override it for an individual Backup or Restore.

The deadline guarantee begins at acceptance, not when the request is written. A blocked workflow controller may delay acceptance; resolving that blockage is outside this RFC's cancellation guarantee.

At acceptance, the workflow controller resolves the timeout and records a fixed deadline. Later spec changes, server configuration changes, retries, and restarts do not change that deadline.

The cancellation deadline bounds how long Velero waits for cancellation handling. At the deadline, the cancellation controller must stop waiting and transition the Backup or Restore to `Cancelled`, reporting any known unfinished or unconfirmed work. Reaching a terminal phase allows lifecycle operations such as deletion to proceed.

If controller downtime or API unavailability prevents the status update, the controller must record `Cancelled` as soon as reconciliation can persist it after the deadline. It must not start a new waiting period.

**Open questions:** the default duration, condition types/reasons, and CLI presentation remain to be agreed. See the [decision summary](#decision-summary).

### Data and diagnostics

- **Backup:** attempt to remove unusable backup payloads, snapshots, and temporary resources. Retain the Backup object and available logs, errors, results, and operation metadata for inspection until normal deletion or expiration. Restore source selection must reject Backups in `Cancelled` phase in every explicit and automatic selection path.
- **Restore:** retain restored resources, destination volumes, and partial data. Cancellation cleanup may remove only owned temporary resources, never destination storage, including during in-place restore.

Cancellation cleanup is best effort and reuses only the deletion steps that preserve retained resources and diagnostics. Cleanup failures and unfinished work must be reported, but cleanup must not delay `Cancelled` beyond the deadline.

Cancelled Backups are later deleted through the existing `spec.ttl` lifecycle, including their retained diagnostics. Restores require explicit deletion.

Early cancellation may leave no archive or diagnostics. Downloads and CLI output should distinguish files never created from failed uploads. Saving diagnostics must not delay `Cancelled` beyond the deadline.

## Shared implementation rules

### Controller responsibility

**Decided:** cancellation proceeds in two stages:

1. The workflow controller accepts the request in a non-terminal phase by recording `Cancelling`, `status.cancellation.acceptedAt`, and `status.cancellation.deadline` together, then stops normal processing.
2. The dedicated Backup or Restore cancellation controller begins after acceptance. It coordinates cancellation handling, resumes it after restarts, enforces the cancellation deadline, and makes the single transition to `Cancelled`.

Child controllers continue to handle cancellation of their own resources. Controllers must act only on children belonging to the correct Backup or Restore, identified by namespace and UID.

The cancellation controller must enforce the deadline independently of blocked workflow, plugin, or provider calls.

Status and metrics must not expose secrets. Keep status reports bounded in size and avoid metric labels with unbounded sets of values.

### Stopping work

Check for cancellation regularly without reading the API before every item. Check before starting normal workflow work, between items or actions, while waiting, and before normal finalization. A call already underway may finish, but the workflow must recheck cancellation before starting its next normal processing step.

| Work | Cancellation path |
|---|---|
| Normal workflow work not yet started | Do not start it. |
| PodVolumeBackup/PodVolumeRestore, DataUpload/DataDownload | Set the child `spec.cancel`; its controller handles cancellation. |
| v2 item action with a known operation ID | Call `Cancel(operationID, parent)` and poll progress until it finishes or the deadline expires. Plugin cancellation may be a no-op. |
| No effective cancellation path | Keep available outcome information without waiting past the deadline. Examples include native/CSI snapshot creation, in-flight synchronous calls or writes, and operations whose IDs were lost. |

### Restarts and status updates

- On restart, workflow controllers process cancellation requests that have not yet been accepted. Cancellation controllers resume cancellation handling using the recorded deadline, known child resources, and saved operation IDs.
- Report missing IDs or unconfirmed work. Do not resume normal processing after the cancellation request is accepted.
- Use a conditional status update so normal completion and cancellation acceptance cannot overwrite each other. A retry using stale status must not undo an accepted cancellation request.
- Late results may add diagnostics, but must not move a Backup or Restore out of `Cancelled`, change its recorded cancellation reason, or restart normal processing.

## Workflow-specific requirements

Each workflow design must identify its cancellation checkpoints and the conditions for ending cancellation handling at each checkpoint. It must also define child resource discovery, supported plugins and storage modes, and safe cancellation cleanup. The following areas need explicit policies:

- **Finalization — further investigation required:** define how cancellation is handled during finalization, including which finalization steps must still complete and how they interact with the deadline.
- **Hooks — further investigation required:** examine partial pre-hook execution, application pause/unfreeze behavior, interrupted exec calls, and duplicate side effects after retries or restarts before choosing a cancellation policy. Distinguish cleanup hooks from success-only hooks. Restore must not falsely signal successful volume restoration.
- **Cancellation cleanup and deletion:** identify which payloads and temporary resources can be removed while retaining diagnostics. Decide whether full Backup deletion requests cancellation first, and how concurrent deletion, late uploads, and dependent Restores are handled.

## Rollout and validation

1. Resolve the remaining condition definitions, timeout default, and workflow-policy questions.
2. Update CRDs, generated clients, permissions, controllers, and CLI together. Review every phase-dependent path, including recovery, synchronization, finalization, deletion/expiration, restore-source validation, downloads, and metrics. Older servers may ignore cancellation; older CRDs may reject the new phases.
3. Validate:
   - Cancellation before work starts and during each non-terminal phase, including finalization.
   - Child and plugin cancellation limitations.
   - Completion races, restarts, and late results.
   - Hook behavior, data preservation, and deletion races.
   - Rejection of cancelled Backups as restore sources.
   - Deadline enforcement despite blocked calls after acceptance.
   - Transition to `Cancelled` after recovery from an expired deadline, without another waiting period.

## References

- [#9284: Backup Cancellation Design](https://github.com/velero-io/velero/pull/9284).
- [#9284: bounded wait discussion](https://github.com/velero-io/velero/pull/9284#discussion_r2363925189).
- [#9284: cancellation in the existing state machine](https://github.com/velero-io/velero/pull/9284#discussion_r2430934181).
- [#9284: backup payload and diagnostic retention discussion](https://github.com/velero-io/velero/pull/9284#discussion_r2384454776).
- [#10509: Explicit Restore cancellation](https://github.com/velero-io/velero/pull/10509).

## Decision summary

Proposals remain open until recorded as decided with a brief rationale.
See the [research companion](unified-backup-restore-cancellation_research.md) for recommendations, alternatives, and supporting evidence.

### Open questions

| Topic | Decision needed |
|---|---|
| Cancellation timeout | What is the default cancellation timeout? |
| Status and UX | Which condition types and reasons for ending cancellation handling are public API? How do CLI commands and metrics show late cancellation requests and unconfirmed work? |
| Diagnostics | What remains available after early cancellation, and how do downloads distinguish absent files from failed uploads? |
| Cancellation cleanup and deletion | Which payloads and temporary resources can be removed, and which deletion steps can be reused? How are failures, late writers, and dependent Restores reported? Should full deletion cancel first, and how does direct Kubernetes deletion interact? |
| Finalization | Which finalization steps must still complete after cancellation, and how do they interact with the deadline? |
| Hooks | Investigate partial execution, cleanup versus success-only behavior, timeouts, and retry/restart side effects before selecting a policy. |
| Supported modes | Which storage modes allow safe temporary-resource cleanup? Which plugin/provider operations may remain running or unconfirmed? |

### Closed / decided

| Topic | Decision and rationale |
|---|---|
| Request API | Use `spec.cancel` on Backup and Restore, keeping the request on the existing object without a separate request resource. |
| Cancellation acceptance | Clearing `spec.cancel` cannot undo cancellation acceptance; controllers follow status without requiring one-way API validation. Reject `Schedule.spec.template.cancel`; use `spec.paused` for schedules. |
| Phase eligibility | Allow cancellation in any non-terminal phase, including before work starts. Finalization handling needs further investigation. |
| Controller architecture | Workflow controllers accept cancellation requests and record acceptance times and deadlines. Dedicated Backup and Restore cancellation controllers then coordinate cancellation handling, enforce cancellation deadlines, recover after restarts, and record `Cancelled`. |
| Phase spelling | Use `Cancelling` and `Cancelled` consistently for both Backup and Restore. |
| Cancellation deadline | Configure a server-wide cancellation timeout with an optional `spec.cancellationTimeout` override. The guarantee begins at acceptance; time waiting for acceptance is not bounded by it. At expiry, record `Cancelled` so lifecycle operations can proceed, or persist the transition as soon as reconciliation can do so. |
| Cancellation status | Add `acceptedAt`, `deadline`, `reason`, and `conditions` under `status.cancellation` after acceptance, alongside existing status metadata. |
| Cancellation cleanup | Attempt to remove unusable backup payloads, snapshots, and temporary resources while retaining Backup objects and available diagnostics. Preserve Restore destination resources and data. |
| Post-cancellation deletion | Use the existing Backup TTL for full deletion after cancellation cleanup, removing retained diagnostics and the Backup object. Restores require explicit deletion. |
