# TODO

Phase plan and exit criteria:
`../research/context-compaction-mod-milestones.md`.

## In-place checkpoint mode (done)

`cake-compact` now appends a checkpoint to the existing session through
`cake sessions checkpoint <UUID> --json` instead of writing a new session. The
session id stays stable, raw history remains on disk, and the state file is
unchanged. Implemented:

- `run` and `compact` use in-place by default; the old minimal-fork writer is
  behind the explicit `-fork` flag.
- The payload carries `trigger`, `summary`, `retained`,
  `estimated_input {before_tokens, after_tokens, basis}`,
  `summary_instruction_version`, optional `model`/`task_id`, and optional
  `summary_usage`/`summary_time_ms`. Cake owns the envelope, ids, timestamp,
  boundary, lock, validation, append, and sync.
- `before_tokens` anchors on the newest complete `turn_usage.input_tokens`
  (`measured_usage`) and falls back to the content estimate. A non-reducing
  estimate returns a non-compaction result and never calls the verb; 0/0 is
  never sent.
- A binary without the verb fails with
  `cake sessions checkpoint is unavailable; upgrade cake`; there is no JSONL
  fallback.
- Output JSON keeps the existing names and adds `in_place`, `boundary`, and
  `retained_count`; `run -json` reports the same id as `next_session_id` in
  place.
- `SIGINT`/`SIGTERM` cancel the summarizer and the append; a completed append
  is reported as success and an incomplete one leaves the source byte-stable.
- Tests cover prior-byte preservation, the stable session id, replay with one
  and two checkpoints, newest-checkpoint selection, invalid payloads, locked
  sessions, a missing verb, the fake subprocess payload contract, cancellation,
  and real-Cake replay validation gated on `CAKE_BIN`.

## Qualification instrumentation (done)

`compact -json` and `run -json` report the summarizer's provider usage and wall
time as `summary_usage` and `summary_time_ms`, so Phase 5 can measure cost from
the result document. `summary_usage` reports `input_tokens`, `cached_tokens`,
`output_tokens`, `reasoning_tokens`, `summary_visible_tokens`, and
`total_tokens`. Cached tokens are a subset of input, reasoning is a subset of
output, and visible tokens are output minus reasoning (clamped at zero). A file
or command summarizer reports no usage and omits the field; a provider response
whose usage is missing or malformed leaves it zero rather than failing the
summary. Tests cover nested provider details, flat read-back, missing details,
and reasoning greater than output.

## Checkpoint usage persistence (done 2026-10-28)

`compact -json`/`run -json` reporting is no longer the only place a
measurement survives. Both commit paths copy `summary_usage` and
`summary_time_ms` into the `compaction` record: in place through Cake's
checkpoint payload, and in fork mode through the local checkpoint builder.
Cake accepts the fields as optional additive payload/record fields, so a file
or command summarizer still omits them. The in-place preflight reads the
checkpoint help text and refuses a build that does not advertise both fields,
so an older Cake cannot spend a turn or summary call before failing. Tests
cover the protocol payload, the capability probe, the persisted in-place
checkpoint, and the fork record.

## Bounded single-request tail selection (done)

Compaction no longer requires the retained tail to start at a user message.
`selectTail` walks back from the newest item to `-keep-tokens`, then moves to a
complete provider boundary: no call/output pair may be split and a
reasoning/call batch stays whole. The nearest real user message at or before
that boundary is copied into the retained tail with `pin.kind = user_input`, so
a one-request session can summarize older work inside the same request without
losing the instruction. A synthetic historical summary is never an anchor or a
retained item. No-fit boundaries, non-reducing plans, and oversized summaries
report a reason and leave the source unchanged. `-keep-tokens` is a target, not
a cap.

Tests cover the 493-item single-request regression, exact request and
constraint projection below the 80k target, parallel and split batches, the
synthetic-summary case, repeated checkpoints, and a real-Cake in-place round
trip. The synthetic live recall probe ran on 2026-10-27 as task 003 and returned
**PARTIAL**: the exact request and all constraints survived, but the summary
carried its own temp cwd/environment and the probe repeated it. Follow-up task
004 owns that defect; the real-regression full critical-fact probe was not run.
Procedure and result: `docs/single-request-recall-probe.md`.

## Summarizer runtime metadata (done; superseded by task 005)

Task 004 fixes the false working directory and environment that the task 003
probe found. Cake appends a Developer message with the child process's working
directory, date, platform, architecture, shell, and terminal from
`build_initial_prompt_messages_with_enabled_tools` in Cake's
`src/prompts/mod.rs`; `--system-prompt` replaces only the system message, so the
summarizer cannot suppress it with `--no-session --no-tools --no-skills`.

`BuildSystemPrompt` now declares that message out of scope and forbids facts
absent from the transcript. Before a checkpoint, `cakecli.Summarize` also drops
lines that contain the per-call `cake-compact-summary-` temp path or that begin
with a runtime label and assert a runtime fact absent from the retired
transcript. `summary_instruction_version` was 2. Tests cover the exact task 003
contamination, the child `$PWD`, transcript-backed environment facts, prose that
merely mentions runtime terms, and a summary made only of runtime metadata.
`just ci` and `go test -race ./...` pass; no model call. Task 005 replaced this
containment with Cake's `--no-runtime-context`; see below.

## Summarizer runtime context source fix (done)

Task 005 removes the prompt-plus-scrub containment now that Cake has the source
fix. `cakecli.Summarize` probes `cake --help` for `--no-runtime-context`, passes
it to the summarizer, and fails with
`cake --no-runtime-context is unavailable; upgrade cake` when absent. The
runtime-boundary prompt and the summary scanner are removed;
`summary_instruction_version` is 3. Tests cover the flag being passed, the
missing-flag failure, the `run` preflight, and the prompt. `just ci`,
`go test -race ./...`, and the real-Cake `test-cake` build pass without a model
call.

## Stage timing (done)

`compact -json` and `inspect -json` now report `timings`: `load_ms`, `plan_ms`,
`preflight_ms`, `summary_ms`, `commit_ms`, `verify_ms`, and `total_ms`. Existing
fields are unchanged; `summary_time_ms` remains the compatibility alias for the
summary stage. `-verbose` prints the same durations to stderr. Only numbers are
recorded, never transcript or secret content.

The baseline in `docs/compaction-stage-baseline.md` uses real Cake against temp
copies and a fixed summary file. On the 1.2 MB single-request session, in-place
total time was 33–39 ms, with 23–28 ms in the checkpoint subprocess; on the 2.0
MB `f8d30612` session it was 42–46 ms total, with 30–33 ms in the checkpoint
subprocess. The observed 31.7-second live gap is therefore not local wrapper
payload, planning, or append work. The unmeasured stage is the summary call
(Cake startup plus provider latency); reducing it requires a model, prompt, or
quality tradeoff, not a local optimization. Do not change the default
model/profile without separate authorization. The duplicate in-place preflight
in `run` would save only about 4–5 ms, so it was left alone.

Controlled-delay tests cover summary, preflight, commit, and fork verification;
no-op results report only load, plan, and total, and failed attempts return no
timing document. `just ci`, `go test -race ./...`, and the real-Cake `test-cake`
run pass without a model call.

## Adaptive transcript budget (done)

When the rendered retired prefix exceeds the 800,000-character transcript cap,
`Analyze` raises `-keep-tokens` to the smallest larger value whose prefix fits,
then re-selects at a complete provider boundary. `-keep-tokens` is the starting
target; `keep_target` reports the effective value. No transcript content is
truncated, and no summary call starts until a nonempty prefix fits. If retaining
the whole session still leaves no nonempty prefix, compaction is refused with a
reason. `TestAnalyzeRaisesKeepToFitTheTranscriptCap` covers a 100,000-character
cap on the 493-item single-request fixture.

## Latency (open; spend-gated)

Deep dive: `docs/latency-deep-dive.md` (2026-10-28, free analysis). The
summary call is output-bound: Phase 5 fits `ms ≈ 0.023 × input + 6.06 ×
output + 383` (R² = 0.96), ~165 output tokens/s. Visible summaries are 1.6–2.7k
tokens, but the provider reports 5.6–8.3k output tokens; 54–70% of main-turn
output is hidden reasoning, and the summarizer inherits the default model's
`reasoning_effort = "max"`. Local load/plan/commit work is 33–50 ms. The
transcript is 75–83% raw tool output; messages are under 4%. Before the
adaptive budget landed, the default `-keep-tokens 20000` could exceed the
800k-character cap and fail after a turn was paid for; `Analyze` now raises the
target before the summary call.

Recommended order:

1. Summarizer controls: `-summarizer-model` and `-summarizer-profile` exist;
   `-summarizer-reasoning-effort` is implemented. Do not add
   `-summary-max-tokens` or a fixed summary length target yet: visible summary
   length has not been shown to be the problem, and a target would be arbitrary.
   `summary_usage` now captures `reasoning_tokens`, `cached_tokens`, and
   `summary_visible_tokens`; A1 is held until Trav reviews those numbers. Then
   run the paid A/B on Phase 5 sessions (three summaries, ~0.5M input; no
   follow-up turns) and grade against the annotation sheets. If A1 shows visible
   output length is the remaining bottleneck, add a bounded output safety cap
   then. Expected 30–60 s -> 10–18 s.
2. Build an evidence pack (all messages, Edit/Write args, commands) plus a
   deterministic exact-reference ledger; the model writes only the narrative.
   Expected 10–18 s -> 6–10 s, 65–80% less input, equal or better exact-fact
   coverage. Add `-transcript-out` on dry-run so it can be inspected offline.
3. Adaptive transcript budget: implemented; see "Adaptive transcript budget
   (done)". A higher default keep (50k–80k) and `-transcript-out` remain.
4. Consider speculative summary during the turn only if the above still leaves
   more than ~15 s; core-native compaction is the long-term fix (cache reuse
   and in-turn boundaries).

Do not optimize the local stages, duplicate preflights, or cake startup; each
is milliseconds against a 30–60 s summary call.

## Later

- Add a small `-payload-out` or dry-run payload printer so other tools can
  produce the protocol payload without linking the Go code.
- Decide whether the `-fork` escape hatch still earns its place now that
  in-place works (export/handoff is the remaining use).
- Add a summary-quality/prompt-injection note to any qualification work; the
  current POC does not evaluate it.
- Decide whether to include `content`-shaped reasoning (`reasoning_text`) in the
  summary transcript. It is currently dropped because `Reasoning.Text()` reads
  only `summary`; including it would raise input and the chance of the 800k cap,
  and may expose sensitive reasoning.
- Consider exposing exact pins for observed skill reads and unconsumed images.
  The caller now pins the owning user request; the other pin kinds are not
  produced yet.
