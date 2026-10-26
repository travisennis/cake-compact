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
  `summary_instruction_version`, and optional `model`/`task_id`. Cake owns the
  envelope, ids, timestamp, boundary, lock, validation, append, and sync.
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
time as `summary_usage {input_tokens, output_tokens, total_tokens}` and
`summary_time_ms`, so Phase 5 can measure cost from the result document. A file
or command summarizer reports no usage and omits the field; a provider response
whose usage is missing or malformed leaves it zero rather than failing the
summary.

## Later

- Add a small `-payload-out` or dry-run payload printer so other tools can
  produce the protocol payload without linking the Go code.
- Decide whether the `-fork` escape hatch still earns its place now that
  in-place works (export/handoff is the remaining use).
- Add a summary-quality/prompt-injection note to any qualification work; the
  current POC does not evaluate it.
- Consider exposing exact pins (current user input, observed skill reads,
  unconsumed images) in the payload. The verb accepts the metadata; this caller
  does not produce it yet.
