# Single-request recall and continuation probe

This probe is the live quality check for cake-compact task 002 (bounded
recent-tail selection with an exact request pin). It is **not** part of
`just ci`. Run it only after Trav explicitly authorizes the spend; it costs
summarizer and probe model calls.

The offline suite proves structure, ordering, reduction, and that real Cake
accepts and projects the checkpoint. It cannot prove that the summary carries
the facts the assistant needs to continue. This probe measures that.

## Result (2026-10-27)

Task 003 ran the synthetic fixture once with the default model:
checkpoint `270,233 -> 10,628`; summary `83,936 in / 605 out`; probe
`3,694 in / 3,707 out`. The probe quoted the exact request and every stated
constraint and did not invent work or live verification. It is **PARTIAL**
because the summary carried the summarizer's own temporary working directory
and environment as session facts; the probe repeated the false working
directory. Follow-up task 004 owns that defect. The real-regression full
critical-fact probe was not run.

Task 004 contained the defect on 2026-10-27 without a model call: the summary
prompt declared Cake's runtime Developer message out of scope, and the returned
summary dropped lines that only restated the per-call temp path or an environment
fact absent from the retired transcript. `summary_instruction_version` was 2.
Task 005 removed the containment on 2026-10-27 by passing Cake's
`--no-runtime-context`, which omits the runtime Developer message at the source;
the prompt and scrub are gone and `summary_instruction_version` is 3. No live
re-probe was run; the synthetic probe result above remains the only live recall
evidence.

## Fixtures

Use one of these, never both in one run:

- **Synthetic (preferred for repeatability):** the fixture built by
  `singleRequestFixtureLines` in
  `internal/compact/compact_test.go`. It has one request, 123 completed
  reasoning/call/output batches, and a measured 270,233-token input.
- **Real regression:** a copy of session
  `98de4cb4-d81f-43e7-80ac-55ff4e88b1db`. The raw transcript stays private:
  copy it into a temporary `CAKE_DATA_DIR`, and do not paste it into a task,
  commit, or PR.

## Setup

```sh
# Work on a copy; the source session must stay byte-identical.
tmp=$(mktemp -d)
mkdir -p "$tmp/sessions"
cp ~/.local/share/cake/sessions/98de4cb4-d81f-43e7-80ac-55ff4e88b1db.jsonl "$tmp/sessions/"

export CAKE_DATA_DIR="$tmp"
cake-compact inspect -json -threshold-tokens 60000 -keep-tokens 10000 \
  <session-uuid>
```

Require `compact: true`, a nonempty `prefix_items`, and a `retained_items`
count far below `effective_items`. Then compact with the real summarizer and
capture its cost:

```sh
cake-compact compact -json -threshold-tokens 60000 -keep-tokens 10000 \
  <session-uuid> | tee "$tmp/compact.json"
```

Do not pass `--model`, `--profile`, or `--reasoning-effort`. The configured
default is the model under test.

## Probe prompt

Resume the checkpointed copy with no tools and no skills, and ask for a
structured recall before any continuation:

```sh
cake --resume <session-uuid> --no-tools --no-skills --sandbox read-only \
  --output-format json \
  "Do not use tools and do not change any files. From the compacted context,
   answer in five sections: (1) quote the original user request verbatim;
   (2) list every constraint it states; (3) list the decisions made and the
   work completed, with file paths and commands; (4) state what was verified
   and what was not; (5) give the single next action. If a fact is not
   present, say 'not in context' instead of guessing."
```

A second, optional continuation turn asks for the next step in one sentence;
it must agree with section 5.

## Critical facts

The checkpointed context must preserve all of these without tools:

- The exact request: bounded recent-tail selection for the single-request
  session, with the complete original instruction, not a paraphrase.
- Constraints: preserve the request exactly; keep complete
  reasoning/call/output batches; do not change the model or profile; run
  `just ci`; never touch `/etc`; report every failure with the exact command.
- Decisions: pin the owning user request as `user_input`; summarize older work
  inside the same request; move the tail to complete provider boundaries.
- Verification: offline structural tests and the real-Cake in-place round trip
  passed; the synthetic live probe ran on 2026-10-27 as PARTIAL. The
  real-regression full critical-fact comparison is still not collected.
- Unresolved work: the real-regression full critical-fact comparison. The
  summary runtime-metadata cleanup is fixed by cake-compact 004; no live
  re-probe was run.

## Scoring

- **Pass:** every critical fact is present, the request is quoted exactly, and
  no fact is contradicted or invented.
- **Partial:** constraints and decisions are present but paraphrased or one
  verification/unresolved item is missing.
- **Fail:** the original request or a constraint is missing, the model invents
  work, or it claims live verification happened.

Record the result, the summarizer's `summary_usage`, the probe's provider
usage, and the session ids in the task comment. Do not record the raw
transcript.
