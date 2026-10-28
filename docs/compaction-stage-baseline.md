# Compaction stage timing baseline

Date: 2026-10-27. No paid calls and no model/profile changes. All summaries in
this baseline are fixed files, and every in-place compaction ran against a
temporary copy under a temporary Cake data root.

## Purpose

The live `f8d30612-55ca-47f4-820a-b890f457583d` session completed its task at
approximately `2026-10-26T14:43:31.36Z` (the last `turn_usage` is
`14:43:31.345Z`) and appended a checkpoint at `2026-10-26T14:44:03.068Z`: a
31.7-second observed gap. That gap was not a measured provider duration. This
baseline separates the local wrapper and Cake append stages from the summary
stage so the next latency decision is based on evidence.

## Instrumentation

`compact -json` and `inspect -json` now include `timings` with whole-millisecond
values:

- `load_ms` — open, shared-lock, and parse the session file.
- `plan_ms` — projection, measurement, threshold, selection, and pin.
- `preflight_ms` — in-place `cake sessions checkpoint --help` probe.
- `summary_ms` — transcript build plus summary file, command, or model call.
- `commit_ms` — in-place checkpoint subprocess, or fork file write.
- `verify_ms` — fork `cake replay` validation.
- `total_ms` — wall time from after mode/config validation through result
  assembly. It excludes process startup, flag parsing, and the `run` turn, and
  may exceed the stage sum because of unmeasured gaps.

A stage that did not run is omitted. `-verbose` prints the same numbers to
stderr. Only durations are recorded, never transcript content or secrets. The
existing `summary_time_ms` field remains for compatibility and equals
`timings.summary_ms` when both are present.

## Controlled-delay verification

The timing tests use a delayed summarizer and a fake Cake process with a
configurable delay:

```sh
go test -run 'Timings' -v ./internal/compact
```

- `TestCompactTimingsAccountForStages` uses a 40 ms summarizer and 30 ms fake
  Cake preflight/commit delays, checks each in-place stage, total duration, the
  JSON shape, and the `summary_time_ms` alias.
- `TestForkTimingsIncludeVerification` uses a 25 ms fake replay delay and checks
  `commit_ms` and `verify_ms`.
- `TestCompactTimingsOnNoOp` checks that a below-threshold result reports only
  `load_ms`, `plan_ms`, and `total_ms`.
- A failed attempt returns an error rather than a result document, so no partial
  timing object is reported.

These tests make no model calls.

## Reproducible commands

Build once, then inspect a real session (read-only):

```sh
just build
./bin/cake-compact inspect -json \
  -threshold-tokens 60000 -keep-tokens 10000 \
  <session-uuid>
```

Measure the dry-run path with a fixed summary (read-only):

```sh
cp ~/.local/share/cake/sessions/<session-uuid>.jsonl /tmp/session-copy.jsonl
printf 'A synthetic summary.\n' > /tmp/summary.txt
./bin/cake-compact compact -dry-run -force -keep-tokens 10000 \
  -summary-file /tmp/summary.txt -json /tmp/session-copy.jsonl
```

Measure the real in-place commit on a copy with real Cake. The source session is
never touched:

```sh
tmp=$(mktemp -d)
mkdir -p "$tmp/sessions"
cp ~/.local/share/cake/sessions/<session-uuid>.jsonl \
  "$tmp/sessions/<session-uuid>.jsonl"
./bin/cake-compact compact \
  -data-dir "$tmp" \
  -cake-bin ../cake/target/debug/cake \
  -force -keep-tokens 10000 \
  -summary-file /tmp/summary.txt \
  -json <session-uuid>
rm -rf "$tmp"
```

Use a fresh copy for each in-place run; a second run would read the appended
checkpoint and measure a different plan.

## Results

Warm-cache runs on 2026-10-27:

| Session | File size | Path | Runs | `total_ms` | `load_ms` | `plan_ms` | `preflight_ms` | `summary_ms` | `commit_ms` |
| --- | --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| `98de4cb4-d81f-43e7-80ac-55ff4e88b1db` | 1.2 MB | `inspect` | 5 | 4–5 | 4–5 | 0 | — | — | — |
| `98de4cb4-d81f-43e7-80ac-55ff4e88b1db` | 1.2 MB | `compact -dry-run -summary-file` | 5 | 4–6 | 4–6 | 0 | — | 0 | — |
| `98de4cb4-d81f-43e7-80ac-55ff4e88b1db` | 1.2 MB | real Cake in place, fixed summary | 5 | 33–39 | 4–5 | 0 | 4–5 | 0 | 23–28 |
| `f8d30612-55ca-47f4-820a-b890f457583d` | 2.0 MB | `inspect` | 5 | 6–7 | 5–6 | 0–1 | — | — | — |
| `f8d30612-55ca-47f4-820a-b890f457583d` | 2.0 MB | real Cake in place, fixed summary | 3 | 42–46 | 6 | 0–1 | 4 | 0 | 30–33 |

The first read after a cold page cache was slower (the initial `inspect` took
about 0.27 seconds including process startup), but steady-state local work is
tens of milliseconds.

The measured in-place plan for `98de4cb4` was 483 retired items and 71 retained
items at 10,986 retained tokens, matching the task 002 qualification. The
`f8d30612` copy compacted at 55 retired / 63 retained items because it already
contained a checkpoint; it was used only to measure file size and commit cost.

## Comparison and decision

The observed 31.7-second live gap is roughly 700× the measured local in-place
path on a 2.0 MB session (42–46 ms). Local parsing, projection, selection, and
Cake's checkpoint validation/append cannot account for it.

The remaining unmeasured stage is the summary call: the default summarizer is a
Cake process launch plus a provider request. This baseline cannot put a number
on that stage without a paid call; the fixed summary file makes `summary_ms` 0
by design. The evidence is enough to rule out a local wrapper optimization:

- The duplicate in-place preflight in `run` (the pre-turn check and the check
  inside `Compact`) is about 4–5 ms here. Removing it would save less than 0.02%
  of the observed gap and would weaken the guarantee if the binary changed
  between the turn and the append.
- Parsing and planning are already 6 ms or less on 2 MB.
- Cake's checkpoint subprocess is 30–33 ms on 2 MB.

Further reduction would require a provider, prompt, or quality tradeoff: a
faster/cheaper summarizer model or profile, a shorter or more selective
transcript, or accepting a lower-quality summary. Those choices need explicit
authorization; this task did not change the default model/profile. A future
`run` will report `timings.summary_ms` directly and make the provider share
visible without guessing.

The follow-up deep dive ([`latency-deep-dive.md`](latency-deep-dive.md)) fits
the summary stage on the Phase 5 data: wall time is output-token-bound, and
hidden reasoning is the largest term. It records the options, expected impact,
and the spend-gated A/B.
