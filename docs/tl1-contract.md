# TL1 archive/release contract

> **Mothballed (2026-09-24):** Pharos reclamation is parked, and this contract is kept as the spec for resuming it. See [`reclamation/README.md`](reclamation/README.md).

For the implementation-ready team request—including ownership boundaries, locks,
worker fencing, durable operation state, export-before-mutation, fixtures, and the
acceptance matrix—see [TL1 team handoff](tl1-team-handoff.md).

The base URL is configured as `tl1_url`. Requests are authenticated with `Authorization: Bearer …`, use JSON, and are idempotent by `operation_id`. Endpoint names are implementation defaults; transport can change if the guarantees do not.

## `POST inspect-reclaim-state`

Input: `source_id` (the stable TL1 work/task/candidate ID).

Required response:

```json
{
  "source_revision": "immutable export revision",
  "custody_holder": null,
  "custody_version": "compare-and-swap version",
  "terminal": true,
  "meaningful_activity_at": "2026-08-01T12:00:00Z",
  "running_processes": [],
  "release_blockers": [],
  "base_ref": "commit",
  "head_ref": "commit",
  "resources": [
    {"id": "owner-resource-id", "kind": "worktree", "path": "/exact/owned/path"},
    {"id": "owner-transcript-id", "kind": "transcript"}
  ]
}
```

TL1 derives this from native task/attempt/candidate events and ownership records—not recursive directory mtimes. Every resource needs an owner ID and kind. Paths alone are not identity.

## `POST preservation-acknowledgement`

The archive sends `operation_id`, exact `source_revision`/`custody_version`, manifest hash, verified SSD path and bytes, plus full retained/omitted inventories. TL1 durably records it before returning:

```json
{"recorded": true, "operation_id": "…", "acknowledgement_id": "stable-id"}
```

Repeated identical acknowledgements return the same ID. A hash/revision conflict rejects the request.

## `POST custody-aware-release`

Input binds the acknowledgement, expected revision/custody version, and exact resources and requires both the running-worker handshake and a release lease.

Before and through removal, TL1 must:

1. perform the repository’s documented running-worker handshake;
2. acquire the exact per-effort lock and a lease/state transition that prevents reacquisition;
3. atomically recheck terminal state, custody, protection/activity, revision, process use, and exact ownership;
4. use owner-coordinated `git worktree remove` for worktrees;
5. never delete shared objects or invoke `git gc`;
6. reclaim candidate/transcript-store items only by mapped owner ID;
7. record partial progress durably.

A generic task delete is not a valid implementation. Reset/purge/automatic completion paths must create an export operation before evidence-removing mutation and wait for acknowledgement.

Response state is `released`, `completed`, `blocked`, `partially_reclaimed`, or `failed`, with exact resources and errors.

## `POST reconcile-status`

Input is `operation_id`. Repeated calls never repeat completed removals and never broaden the resource list. The response state is `pending`, `blocked`, `released`, `partially_reclaimed`, `completed`, or `failed`.

## `POST scheduler-activity`

```json
{"known": true, "active_runs": 0, "active_agent_sessions": 0, "observed_at": "…"}
```

Unknown or unavailable owner state prevents heavy jobs. This endpoint reports actual TL1 runs and linked agent sessions; wakefulness is irrelevant.

## Proof gate

`release_hook_proven` may be set only after tests demonstrate: protected candidate rejection; running-worker and missing-lock rejection; stale revision/custody rejection; lease prevents reacquisition; exact resource scope; reset/purge export-before-mutate; SSD/hash failure rejection; idempotent retry; crash at every durable transition; worktree registration consistency; shared object survival; no `git gc`; and measured allocated bytes before/after.
