# 0006 — Two Sandbox lifecycles: ephemeral and session (L2 survival)

## Status

Accepted (2026-08-11)

## Context

setec today launches a fresh **ephemeral** microVM per call ("single ephemeral
microVM execution"), and pause does not persist to disk (evicting the Pod loses
state, `docs/snapshots.md`). But a long-running agent — e.g. an opencode-style
coding/fuzzing agent (zerocool Platform mode, setec#150) — needs a **session**
that holds a worktree across many turns, does not die mid-run, and reattaches by
handle. That workload is a different lifecycle, unbuilt today.

## Decision

A Sandbox declares one of two lifecycles:

- **Ephemeral** — run-to-completion: fast start (snapshot warm-start, ADR-0004),
  auto-destroy on exit, stateless. One tool/agent action.
- **Session** — long-lived: lives across many calls, **reattach by handle**
  (the Sandbox handle / session id), explicit teardown, and is **not idle-reaped
  during active use**.

**Session survival is L2:** every session sandbox has a **durable workspace
volume** (continuous persistence — corpus, findings, worktree survive the VM,
never lost even to an untimely node death) **plus periodic/on-event memory
checkpoints** (via the snapshot machinery). Checkpoints let setec **suspend idle
sessions** to free the node (cost) and **resume/restore on node drain or spot
reclaim so the process continues** uninterrupted. Worst case (node dies between
checkpoints) → the process restarts from the durable workspace; good case → it
resumes seamlessly. (L1 durable-only and L3 live-migration are the fallback and
the deferred over-reach, respectively.)

Isolation holds per ADR-0005: a session VM + its workspace + checkpoints serve
**one session**, and are wiped at session end; intra-session suspend/resume is
permitted, cross-session/tenant reuse is not.

## Consequences

- New: a session lifecycle mode, a durable per-session workspace volume
  (portable CSI, encrypted), reattach-by-handle, a not-during-active-session
  eviction/timeout policy, and checkpoint-on-suspend / checkpoint-on-drain.
- The snapshot machinery serves *both* lifecycles: fast-start (ephemeral) and
  suspend/resume/migrate (session).
- Cost control: idle sessions suspend to disk instead of holding a live microVM.
