# Stable redesign v1

## Problem

Stable is becoming a local EDA platform where one engineer gives a goal to an autonomous agent team, reviews its report and candidate, then explicitly accepts the result. The first release must install and start on Linux, Windows, and macOS. EDA capability may differ by host and must be reported accurately. At least one supported host must complete a real LCEDA schematic repair and validation flow before that adapter is called usable.

Current goal workflows use Temporal, but team child turns use a process-local scheduler. Workspaces, candidate review, and journaled acceptance already provide valuable isolation and human control. Actions run `38832715324` completed the official LCEDA CLI probe against a client-generated offline `.eprj3`: candidate modification, save, fresh-session reopen, schematic check, and BOM export all completed. The schematic-check API returned two violations (`ATTRMATCHCODE`, `CHECKFLOATPIN`). An earlier attempt failed while waiting for the new schematic page to initialize, before candidate modification. These runs do not establish compatibility with user projects or guarantee repeatability. This design connects the existing durable goal path to team work and adds a real project-tree adapter boundary without bypassing candidate acceptance.

## Engineer flow

```sh
stable doctor
stable project add ./controller.eprj3 --adapter lceda
stable goal create --project controller --request 'Repair the reset net and verify the schematic'
stable goal status --goal <goal-id>
stable report show --goal <goal-id>
stable candidate review <candidate-id>
stable candidate accept <candidate-id>
```

The runtime resumes durable work after restart. The engineer can inspect one final report containing the task summary, project-tree diff, client identity, checks and their evidence, and any incomplete work. Missing activation, unsupported isolation, failed checks, or uncertain effects produce a blocked or incomplete report. Agent completion never accepts a candidate or proves the goal succeeded.

## Synthesis decision

The arena selected candidate C as the base. All candidates scored 5/5 on a single lifecycle and 4/5 on install/start boundaries. C scored highest on the LCEDA tree-level adapter boundary; A was strongest on effect recovery and B on compact state transitions. C keeps the existing Temporal supervisor, SQLite store, workspace/candidate services, and acceptance journal in their current ownership roles while adding the missing durable team lifecycle.

The synthesis keeps C's `ApplyAndValidate` operation over a complete project tree. From A it adopts validated request/ID types, immutable per-attempt workspaces, generation fencing for late results, durable pause/cancel behavior, engineer-only review/accept authority, and startup diagnostics that work when EDA execution is unavailable. From B it adopts transactional compare-and-swap revisions, idempotency keys, lease epochs, and an `outcome_unknown` state for ambiguous external effects.

The design rejects B's replacement of Temporal with a new SQLite scheduler because that would make Stable own timer, lease, replay, history, and recovery machinery while duplicating an existing durable engine. It rejects running LCEDA validation outside the declared isolation boundary until a real-client confinement model is proven. A separate `mission` facade and new `project` owner from A are not introduced initially; existing goal, workspace, candidate, and acceptance owners remain the integration points.

## Ownership

- `internal/orchestrator` owns durable control flow: task admission, bounded parallelism, retry scheduling, pause, resume, cancellation, and workflow recovery. All durable agent tasks use this path; the process-local team scheduler is not a second long-task runner.
- `internal/store` owns canonical goal/task/project/report facts and an operation journal. Activities carry IDs and expected revisions, then use transactional compare-and-swap and idempotency keys. Temporal history owns scheduling control, not a competing copy of mutable business facts. On replay, activities read the store before repeating an external effect.
- `internal/sessionlog` owns conversation and evidence history. It is not an independent source of task completion or candidate acceptance. Task/report views derive status from canonical goal/task facts and refer to immutable evidence.
- `internal/workspace`, `internal/candidate`, and `internal/store` acceptance transactions own private workspaces, complete project-tree manifests, merge/freeze, revalidation, explicit acceptance, and crash recovery. Only the engineer-facing review path can invoke acceptance.
- `internal/eda` owns adapter registration and typed EDA contracts. `internal/eda/lceda` owns `.eprj3` semantics, client compatibility, schematic edits, native reopen/check operations, and evidence interpretation. Generic orchestration never handles LCEDA wire records.
- `internal/platform` owns process trees, IPC, path and file identity rules, confinement, and host capability reporting. Build-tagged implementations report capabilities through one registry used by `stable doctor` and task admission.
- `internal/runtime` supervises the local worker and workflow service. Install/start and diagnostics remain available when an optional EDA adapter or execution capability is unavailable.

## Contracts

IDs and revisions are validated at CLI, storage, adapter, and OS boundaries. Business state changes are transactional and idempotent. Activities receive stable operation IDs and expected revisions; duplicate delivery returns the stored outcome. A lease epoch fences stale processes. Each attempt writes to a distinct private workspace. A late result from an old attempt is retained for diagnosis but cannot publish over the active generation.

An ambiguous external result becomes `outcome_unknown`. Recovery inspects the operation journal, owned process identity, workspace identity, tree digest, and adapter evidence before it chooses to commit, block, or start a fresh attempt. It never blindly repeats an edit against a workspace whose effect is uncertain. Pause stops new admission and checkpoints cooperative work; cancel stops admission, terminates owned processes, and reconciles effects before becoming terminal.

The LCEDA adapter receives a private whole-project tree, not a single file or formal project path:

```go
type Adapter interface {
    ID() AdapterID
    Readiness(context.Context, ProjectRef, HostCapabilities) (Readiness, error)
    ApplyAndValidate(context.Context, AdapterRun) (AdapterResult, error)
}

type AdapterRun struct {
    Operation OperationID
    Baseline TreeRef
    Candidate CandidateWorkspace
    Objective string
    Limits RunLimits
}

type AdapterResult struct {
    CandidateTree TreeDigest
    Client ClientEvidence
    Checks []CheckEvidence
    Summary string
}
```

The contract requires successful open/edit/save/reopen and every required electrical check to bind to the exact candidate tree digest and client version. If the real client normalizes project files, the resulting tree is a new candidate and must be checked again. Fixture and substitute results are contract evidence only; they cannot report a usable adapter. If isolation or a required client operation is unavailable, readiness is blocked and execution has no unrestricted host fallback.

The existing candidate tree manifest is extended for project directories; no parallel tree identity system is introduced. It must define normalized relative paths, link handling, case-collision behavior, entry and byte limits, protected metadata, and external-reference policy. Review binds the baseline digest, candidate digest, report digest, and check evidence. Acceptance revalidates the current formal tree and records the existing journal receipt. A changed formal tree invalidates review.

## Platform and release contract

All three operating systems must install and start the product. EDA support is advertised per OS and client version. `stable doctor` distinguishes orchestration startup from adapter readiness and reports actionable states such as `client_unavailable`, `activation_required`, `isolation_unavailable`, and `unverified_client`. Unsupported operations fail closed. At least one OS/client combination must pass the complete real-client LCEDA flow before release claims a usable LCEDA adapter; other OSes may ship with narrower documented capabilities.

Install/start evidence is separate from EDA evidence. The first covers packaged binaries, user data paths, SQLite startup, local IPC, supervisor lifecycle, and recovery of an interrupted service start on each OS. The EDA qualification covers a genuine existing `.eprj3`, bounded schematic change in an isolated candidate, real-client reopen and electrical checks, report binding, stale-review rejection, explicit acceptance, and acceptance recovery. No current J00 fixture or compatibility report proves that qualification.

## Open risks

- The official LCEDA client has completed the disposable generated-project probe, but the latest schematic check returned two violations and an earlier attempt failed during page initialization. Repeatability and compatibility with a genuine existing `.eprj3` remain unverified. A genuine project and an authorized activated client are required to qualify the adapter.
- The LCEDA client must be shown to operate within a write boundary compatible with candidate isolation; checking a candidate outside that boundary is not assumed safe.
- SQLite build/runtime support, including native CGO compiler availability, process recovery, filesystem identity behavior, local workflow service packaging, and installation still need platform-specific implementation and evidence.
- `.eprj3` tree limits, symlink and external-library policies, client network needs, and client normalization behavior must be decided from a real project.

## Implementation order

1. Complete install/start prerequisites on Linux, Windows, and macOS; publish truthful host capabilities.
2. Add canonical task/operation records and CAS/idempotency boundaries to the existing store; connect them to durable workflow activities.
3. Route one bounded team task through an isolated candidate workspace and produce a report that remains separate from acceptance.
4. Add the tree-level adapter contract and LCEDA readiness probe; keep execution blocked until the real client path is qualified.
5. Qualify the genuine LCEDA edit/reopen/check/accept path on at least one host before marking the adapter usable.

## Implementation reconciliation

- The first portability slice enables the existing CGO SQLite driver for Linux, macOS, and Windows native builds, and applies the `.exe` helper suffix only on Windows. macOS and Windows package/install scripts now follow the existing `bin`/`libexec`/`share` layout and verify the pinned Temporal archive. This is a prerequisite for the clarified install/start scope; it does not establish packaged or running support on any new platform.
- GitHub Actions run `38832715324` completed the Linux disposable LCEDA project round trip; the schematic-check API returned two violations (`ATTRMATCHCODE`, `CHECKFLOATPIN`). The same run passed the J00 contract tests and Windows/macOS process and file smoke jobs serially. Those two jobs do not establish Stable install/start support or LCEDA support on Windows/macOS. Earlier run `38773265254` failed before candidate editing with D-Bus address parsing errors and a schematic-page readiness timeout; the later run validates the private D-Bus setup on Linux.
- The Linux artifacts from runs `38294157281` and `38773265254` were deleted after review found LCEDA Cookie headers in copied client logs. Local client-log copies were removed. Run `38832715324` uploads only an explicit evidence allowlist; its downloaded Linux artifact contains the redacted probe report, BOM, and environment summary, and a credential-marker scan found no Cookie, Authorization, or token values. The successful Actions run validates artifact filtering and process-group supervision.
- Local validation before the Actions run: 27 J00 unit tests passed, all 11 embedded Python workflow steps compiled after YAML parsing, `bash -n` passed, and `git diff --check` passed. The Linux CLI/API disposable-project probe then ran in Actions. No packaged install/start acceptance or genuine existing-project qualification was run.
- No workflow task ledger, LCEDA project-tree adapter, or user-facing EDA command has been implemented by this slice. Package builders were not run.

This design record does not mark the redesign complete. Run `38832715324` provides current disposable-client and sanitized-artifact evidence, with two schematic-check violations; platform installation and a genuine existing project remain unqualified.
