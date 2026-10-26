# cake-compact

An external context-compaction wrapper for [cake](https://github.com/travisennis/cake).
This is a proof of concept, not a proposed core feature: it exercises the idea
that compaction can live outside Cake, as a mod that can be deleted without
touching the agent loop.

It compacts **between invocations**. Each `cake-compact run` is one ordinary
Cake turn; after the turn, the wrapper reads the context measurement Cake
already writes, summarizes the older part of the conversation, and appends one
`compaction` checkpoint to the existing session through
`cake sessions checkpoint <UUID> --json`. The session id does not change, raw
history stays in the file, and the next turn resumes the same session.

`-fork` keeps the older behavior as an explicit escape hatch: write a new
session file containing only `session_meta` and one checkpoint. It is never
used implicitly.

## Requirements

- A Cake build with the checkpoint record and the append verb. That means both
  the M1 `compaction` record (Cake commit `2d9e4c0`, PR #747) and
  `cake sessions checkpoint` (task 127). `cake --version` reports `0.1.0` for
  both old and new builds, so the wrapper probes the verb itself.
- Go 1.25 or newer to build.
- macOS or Linux. The wrapper takes a shared `flock` on the source session to
  read it and uses `sh` for `-summarizer-command`.

If the binary cannot answer `cake sessions checkpoint --help`, compaction fails
with `cake sessions checkpoint is unavailable; upgrade cake`. `run` checks this
before starting the turn, so an unsupported binary cannot spend a model call on
a turn whose compaction must then fail; pass `-no-compact` (turn only) or
`-fork` (new-session writer) to use an older build. There is no fallback to
writing session JSONL.

A Cake build without checkpoint support cannot read a session that has received
one. The raw records are unchanged, but the checkpoint record is unknown to it;
keep a checkpoint-aware binary for compacted sessions.

## How it works

1. `run` resumes the session saved for the current directory, or starts one,
   and executes one Cake turn with `--output-format json`.
2. It reads the newest complete main-agent `turn_usage` record from the
   session. `context_window` comes from the model config (or `-window`), and
   `-threshold-percent` (default 80) turns it into a trigger.
   `-threshold-tokens` overrides the percentage.
3. It builds the **effective conversation**: the raw conversation, or the
   latest checkpoint's summary plus retained tail plus later records when one
   exists.
4. It selects a suffix of at least `-keep-tokens` (default 20,000 or a quarter
   of the window), moved back to a real user message so the retained tail starts
   at a complete request. A suffix that would split a call/output pair is moved
   back until it is complete.
5. It estimates the retired and retained sizes. `before_tokens` anchors on the
   newest complete `turn_usage.input_tokens` when that measurement is newer than
   the latest checkpoint (`basis: measured_usage`); when a checkpoint is newer,
   it uses the checkpoint's persisted `after_tokens` plus the records added
   since. With neither, it falls back to a content estimate
   (`basis: content_class`). `after_tokens` is the retained-tail estimate plus
   the summary estimate. Nothing is appended when `after_tokens >=
   before_tokens`, and a 0/0 estimate is never sent.
6. It summarizes the retired prefix. The default summarizer is a Cake
   invocation with `--no-session --no-tools --no-skills`, run in a temporary
   directory with a temporary data root; its provider usage and wall time are
   reported in the result document for cost measurement. `-summary-file` and
   `-summarizer-command` replace the model call and report no usage.
7. **In place (default):** it pipes one JSON payload to
   `cake sessions checkpoint <UUID> --json` on stdin. Cake owns the envelope,
   checkpoint id, session id, timestamp, boundary, exclusive append lock,
   validation, append, and sync. Prior bytes are never rewritten; a rejected
   payload leaves the file byte-stable. The wrapper parses the returned
   diagnostic document for `session_id`, `checkpoint_id`, `boundary`,
   `before_tokens`, `after_tokens`, `retained_count`, `session_file`, and
   `basis`.
8. **Fork (`-fork`):** it writes a new session file containing `session_meta`
   and exactly one `compaction` record, then runs
   `cake --output-format stream-json replay <new>` to validate it. A failed
   validation removes the new file. The state file moves to the new id.

Nothing in Cake changes. The in-place path never writes session JSONL itself;
the fork path is reached only through `-fork`.

### In place vs fork

| | In place (default) | `-fork` |
| --- | --- | --- |
| Session id | unchanged | new |
| Source file | one record appended | untouched |
| Raw history on disk | preserved in the same file | only in the source file |
| `cake replay` | raw transcript plus checkpoint(s) | metadata plus one checkpoint |
| Resume | same session id | new session id |
| State file | unchanged | moves to the new id |
| Validation | Cake's append validation | wrapper runs `cake replay` |

Raw history is preserved on disk but is not sent to the provider: resume and
fork project the newest checkpoint as the historical summary plus retained
tail, exactly as Cake's projector does. `cake replay` still shows the raw
transcript because it re-emits stored records verbatim.

## Build

```sh
just build          # bin/cake-compact
just install        # go install ./cmd/cake-compact
just test
just test-race
just test-cake      # real-binary tests; needs CAKE_BIN
```

`test-cake` defaults `CAKE_BIN` to `../cake/target/debug/cake`. Point it at any
Cake build with the checkpoint verb:

```sh
CAKE_BIN=/path/to/cake just test-cake
```

## Use

```sh
# First turn in this directory.
cake-compact run "read the retry code and explain the policy"

# Later turns resume the saved session automatically and compact in place.
cake-compact run "now change the timeout to 60 seconds"

# See the measurement and the planned retirement boundary.
cake-compact inspect <session-uuid>

# Compact now, without running a turn.
cake-compact compact <session-uuid>

# Show what a compaction would do without calling a model or writing.
cake-compact compact -dry-run <session-uuid>

# Use a fixed summary, or a custom summarizer command.
cake-compact compact -summary-file notes.txt <session-uuid>
cake-compact compact -summarizer-command './summarize.sh' <session-uuid>

# Escape hatch: write a new minimal session instead of appending in place.
cake-compact compact -fork -summary-file notes.txt <session-uuid>
```

The session argument is a UUID resolved under the Cake sessions directory or a
path to a `.jsonl` file. In-place compaction requires the file to be the one
Cake resolves for that UUID; a copy elsewhere fails with a message that names
`-fork`.

## Output JSON

Every command accepts `-json`.

`compact -json` prints the compaction document directly. On success it is:

```json
{
  "compacted": true,
  "reason": "forced",
  "source_session_id": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
  "session_id": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
  "session_file": "/Users/me/.local/share/cake/sessions/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa.jsonl",
  "in_place": true,
  "checkpoint_id": "892d5ccb-2b82-4579-b4a9-598059125935",
  "boundary": 7,
  "trigger": "manual",
  "basis": "measured_usage",
  "before_tokens": 90000,
  "after_tokens": 42,
  "window": 100000,
  "keep_target": 10,
  "prefix_items": 5,
  "retained_items": 2,
  "retained_count": 2,
  "summary_chars": 77,
  "summary_usage": {
    "input_tokens": 1200,
    "output_tokens": 300,
    "total_tokens": 1500
  },
  "summary_time_ms": 8420
}
```

| Field | Meaning |
| --- | --- |
| `compacted` | Whether a checkpoint was committed. |
| `reason` | Why it compacted or why it did not. |
| `source_session_id` | The session that was read. Always present. |
| `session_id` | The session to resume. In place this equals the source id. Absent when nothing was committed. |
| `session_file` | Absolute path of the session file. Absent when nothing was committed. |
| `in_place` | `true` for an append, `false` for `-fork`. |
| `checkpoint_id` | Cake's id for the appended checkpoint (or the fork's record). |
| `boundary` | Provider-visible conversation-item count at the append position. |
| `trigger` | `manual`, `threshold`, or `overflow`. |
| `basis` | `measured_usage` or `content_class`, as reported by Cake. |
| `before_tokens` / `after_tokens` | The estimates Cake persisted. |
| `window` | The context window used for the threshold, when known. |
| `keep_target` | The retained-tail token target. |
| `prefix_items` / `retained_items` | Planned item counts on each side of the boundary. |
| `retained_count` | Retained records Cake persisted (same as `retained_items` on success). |
| `summary_chars` / `transcript_chars` | Sizes used for the summary call. |
| `summary_usage` | The summarizer's provider-reported tokens. Absent when the summarizer is a file or command, or the provider reported none. |
| `summary_time_ms` | Wall time of the summary call. Absent when no summary call was made. |

On a non-compaction result, `compacted` is `false` and `reason` says why;
`session_id`, `session_file`, `checkpoint_id`, and `boundary` are absent. Use
`source_session_id` as the session to resume.

`run -json` prints:

```json
{
  "session_id": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
  "next_session_id": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
  "result": "the model's final answer",
  "compaction": { "...": "the document above" },
  "compaction_error": "only present when compaction failed"
}
```

In place, `next_session_id` is the same id as `session_id`, so a consumer that
keeps `--resume` unchanged can keep using `session_id` and ignore
`next_session_id` entirely. With `-fork`, `next_session_id` is the new session
and must be used for the following turn. `compaction` is absent when the turn
was below threshold or compaction was skipped. A summary or append failure
after the turn still reports the turn result, sets `compaction_error`, and
exits `1`. A missing verb is caught before the turn instead (see
[Requirements](#requirements)), so there is no turn result to report.

## State and paths

- Cake data root: `-data-dir`, then `CAKE_DATA_DIR`, then
  `~/.local/share/cake`. Sessions live in `<data-root>/sessions`.
- Wrapper state: `~/.local/state/cake-compact/<hash-of-cwd>.json`, using
  `XDG_STATE_HOME` when set. `-state` overrides it; `-no-state` disables it.
- `run` passes `CAKE_DATA_DIR` to the Cake child, so the wrapper and Cake agree
  on the session directory.
- The state file remembers one session per working directory. It is updated
  only when the session id changes, so an in-place compaction leaves it alone.
  A missing session starts a new one with a warning.

## Flags

Run `cake-compact help` for the full list. The important ones:

| Flag | Meaning |
| --- | --- |
| `-fork` | Write a new minimal session instead of appending in place. |
| `-threshold-percent` | Trigger at this percent of the context window (default 80). |
| `-threshold-tokens` | Explicit trigger instead of a percentage. |
| `-window` | Override the window read from the session. |
| `-keep-tokens` | Retained-tail target (default 20,000 or window/4). |
| `-min-reduction-percent` | Minimum estimated reduction (default 10). |
| `-force` | Ignore the trigger and minimum reduction. |
| `-summary-file` | Use a fixed summary. |
| `-summarizer-command` | Shell command: transcript on stdin, summary on stdout. |
| `-summarizer-model` / `-summarizer-profile` | Model/profile for the default Cake summarizer. |
| `-instructions` | Extra focus for the summary prompt. |
| `-dry-run` | Plan without a model call or write. Never calls the verb. |
| `-no-verify` | Skip the final `cake replay` validation (fork only). |
| `-cwd` | Directory for the Cake turn (`run`). |
| `-resume` | Resume a specific session instead of the state file (`run`). |
| `-no-compact` | Run the turn without considering compaction (`run`). |
| `-force-compact` | Compact after the turn regardless of the trigger (`run`). |

## Cancellation

`compact` and the post-turn compaction in `run` trap `SIGINT`/`SIGTERM` and
cancel the summarizer and the checkpoint subprocess. Temp files are removed.
A checkpoint that Cake fully appended before an interrupt is a valid record and
is reported as success; otherwise the source file is left byte-stable.

## What this prototype does not do

- No compaction inside a single invocation. A long tool loop that fills the
  window in one `cake` process cannot be compacted by an external wrapper,
  because Cake holds the session lock and only restores/projects at invocation
  start. That is what core M3/M4 would add.
- No exact pins beyond what this prototype chooses: current user input,
  observed skill instruction reads, and unconsumed images get no special
  treatment beyond starting the retained tail at a user message. The verb
  preserves pin and provenance metadata when the caller supplies it; this
  caller does not. If those guarantees matter, they need core support.
- Token counts are estimates: 4 characters per token, plus a 25% safety margin,
  plus 1,000 tokens per inline image. `before_tokens` anchors on the
  provider's last reported input when that measurement is newer than the latest
  checkpoint; otherwise it is derived from the checkpoint or a content
  estimate. `after_tokens` is an estimate. Cake enforces only that the append
  reduces the estimate.
- The summary is one model call over the retired prefix. There is no batching,
  no bounded multi-request summarization, and no real-model quality
  qualification. A prefix whose transcript exceeds 800,000 characters fails
  rather than silently dropping content.
- The summarizer is an LLM reading untrusted transcript content. Prompt
  injection, factual fidelity, and summary quality are not evaluated; this is
  exactly the gap Cake's separate qualification milestone exists to close.
- Summary usage is invisible to Cake's task accounting. Use a cheap
  `-summarizer-model` while experimenting.
- The default summarizer uses user-level settings, not the project's
  `.cake/settings.toml`: it runs in a temporary directory so project hooks,
  AGENTS.md, skills, and tools cannot affect the summary. Pass
  `-summarizer-model` or `-summarizer-profile` to choose the model.
- The fork escape hatch writes a new session containing only metadata and the
  checkpoint; the source remains the raw audit record. It is mainly useful for
  exporting or handing off a compacted view.

## Verified

- `go test ./...`: session parsing/projection, checkpoint construction, suffix
  selection (including parallel and split calls), dry-run, reduction guard,
  repeated compaction, fake-cake run orchestration, state updates, and the
  in-place contract: prior bytes preserved, stable session id, a second append
  selecting the newest checkpoint, rejected payloads and locked sessions
  leaving the file byte-stable, missing-verb detection with no fallback, the
  subprocess payload shape, summarizer cancellation, and recognition of an
  append that completed before an interrupt.
- `CAKE_BIN=/path/to/cake just test-cake`: the generated checkpoint is accepted
  by real Cake; replay shows the raw transcript plus exactly one checkpoint
  (two after a second append), the source session id is unchanged, and real
  Cake rejects an invalid payload and a locked session without writing.
- Manual end-to-end check against a local mock provider: three `run` turns, a
  compaction after the second, and a third provider request containing the
  historical summary and the recent exchange but not the retired first prompt.

## Removing it

Delete this directory. Nothing in Cake changed. Sessions that received an
in-place checkpoint keep their raw history and the appended record; they need a
checkpoint-aware Cake binary to resume. Sessions compacted with `-fork` can be
deleted as ordinary session files, and the source sessions remain untouched.
