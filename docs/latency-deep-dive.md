# Compaction latency deep dive

Date: 2026-10-28. Free analysis only: no model calls, no model/profile
changes, no session writes. The paid measurements cited here are from the
Phase 5 pass, already authorized and reported in
`../../research/context-compaction-phase5-results.md`.

## TL;DR

The 30-second compaction is not local work. On a 2 MB session the wrapper,
Cake's checkpoint append, and process startup total 40–50 ms. The summary call
is essentially the whole duration.

That call is **output-token-bound**, and a large part of its output is hidden
reasoning:

- Measured wall time fits `ms ≈ 0.023 × input_tokens + 6.06 × output_tokens +
  383` (R² = 0.96, six Phase 5 summaries). That is ~165 output tokens/s and
  ~43k input tokens/s. Every 1k output tokens costs ~6 s; every 100k input
  tokens costs ~2.3 s.
- Visible summaries are only ~1.6k–2.7k tokens, but the provider reported
  5.6k–8.3k output tokens. The gap is hidden reasoning. In the main turns of
  the same sessions, 54–70% of output tokens are reasoning tokens.
- The summarizer inherits the default model's `reasoning_effort = "max"`
  unless `-summarizer-reasoning-effort` overrides it; no A/B has run yet.
- The summarizer input is 75–83% raw tool output. User and assistant messages
  together are under 4% of the transcript.
- Before the 2026-10-28 auto-raise, the default `-keep-tokens 20000` could
  exceed the 800,000-character transcript cap on a 2.4 MB session and fail
  *after* the turn was paid for. The Phase 5 run had to use `-keep-tokens
  140000` on `9f3670d5` for this reason.

Recommended order:

1. **Low reasoning effort first.** Expected 30–60 s → 10–18 s, with no
   visible-content change. Validate with a small paid A/B against the existing
   Phase 5 annotation sheets. Add an output budget or length target only if the
   A/B shows visible summary length, not hidden reasoning, is the remaining
   cost; the current 1.6–2.7k-token summaries have not been shown to be too
   long.
2. **Evidence pack plus a deterministic reference ledger.** Expected 10–18 s →
   6–10 s, 65–80% less summary input, and *better* exact-fact coverage because
   code carries paths, commits, commands, and error text verbatim.
3. Treat speculative summaries and core-native compaction as later options.

## Where the time goes

Existing baseline ([`compaction-stage-baseline.md`](compaction-stage-baseline.md)):

| Stage | 1.2 MB session | 2.0 MB session |
| --- | ---: | ---: |
| load | 4–5 ms | 6 ms |
| plan | 0 ms | 0–1 ms |
| preflight | 4–5 ms | 4 ms |
| commit (Cake append) | 23–28 ms | 30–33 ms |
| **total** | **33–39 ms** | **42–46 ms** |

A larger 8.7 MB session with 11 checkpoints loads in 49 ms. `cake --help`
startup is under 10 ms in this environment. Local work is roughly 0.1% of a
40-second compaction.

Phase 5's measured summary calls:

| Session | Input tokens | Output tokens | `summary_ms` | Summary chars | Visible tokens (chars/4) | Estimated reasoning |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| `9f3670d5` | 204,658 | 5,572 | 35,673 | 7,050 | 1,763 | ~3,809 |
| `26271706` | 196,113 | 7,622 | 58,574 | 10,844 | 2,711 | ~4,911 |
| `ec32da4e` | 126,449 | 1,778 | 14,865 | 6,457 | 1,614 | ~164 |
| `4fc697db` | 178,072 | 8,303 | 50,577 | 7,797 | 1,949 | ~6,354 |
| `7801e444` | 159,530 | 5,617 | 37,050 | 7,884 | 1,971 | ~3,646 |
| injection probe | 620 | 698 | 4,788 | 1,389 | 347 | ~351 |

The visible-token split uses ~4 characters/token and is an estimate until the
provider reports `reasoning_tokens`; `summary_usage` now carries the exact
value (see Instrumentation below). The main-turn data is exact:

| Session | Output tokens (all turns) | Reasoning tokens | Reasoning share |
| --- | ---: | ---: | ---: |
| `9f3670d5` | 185,706 | 99,960 | 53.8% |
| `26271706` | 288,171 | 197,535 | 68.5% |
| `ec32da4e` | 150,356 | 99,538 | 66.2% |
| `4fc697db` | 147,143 | 97,459 | 66.2% |
| `7801e444` | 208,612 | 145,385 | 69.7% |

`ec32da4e` is the natural experiment: 1.8k output tokens, 14.9 s, and effective
fidelity 7/7 in Phase 5. It shows that a summary near the visible-token length
can still pass the primary bar.

## Budget model

Using the six Phase 5 points:

```text
ms = 0.023079 * input_tokens + 6.057994 * output_tokens + 383
R² = 0.959
```

Implications:

| Change | Approximate saving |
| --- | ---: |
| Remove 1,000 hidden reasoning tokens | 6.1 s |
| Remove 1,000 visible summary tokens | 6.1 s |
| Remove 100,000 input tokens | 2.3 s |
| Remove all local wrapper/append work | 0.04 s |

This is why a faster parser, a cached preflight, or a direct provider call
cannot matter. It is also why input reduction is primarily a cost and
robustness lever, not a latency lever: the provider prefills at ~43k tokens/s.

Illustrative targets, using the same model:

| Configuration | Input | Output | Predicted time |
| --- | ---: | ---: | ---: |
| Current average | 173k | 5.8k | ~39 s |
| Low reasoning, same visible summary | 173k | 2.0k | ~16 s |
| Low reasoning, 1,000-token summary | 173k | 1.0k | ~10.5 s |
| Evidence pack, 1,000-token summary | 40k | 1.0k | ~7.5 s |
| Evidence pack, 700-token summary | 40k | 0.7k | ~6 s |

## What the summarizer actually reads

### Reasoning is currently dropped

`BuildTranscript` currently renders messages, function calls, and function
outputs. It caps each tool call at 4,000 characters, each tool output at 6,000,
and each reasoning item at 4,000. The reasoning branch is effectively dead:
`Reasoning.Text()` reads the `summary` field, but current session records put
the text in `content`. Every analyzed prefix shows `reasoning items > 0,
rendered chars = 0`.

Exact composition of the prefix actually sent to the model:

| Session | Transcript | Tool outputs | Edit/Write args | Bash args | Messages | Other calls | Reasoning |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| `26271706` | 732,049 | 551,183 (75.3%) | 91,504 (12.5%) | 63,849 (8.7%) | 21,787 (3.0%) | 3,174 (0.4%) | 0 |
| `4fc697db` | 709,849 | 581,871 (82.0%) | 55,481 (7.8%) | 52,093 (7.3%) | 3,847 (0.5%) | 12,232 (1.7%) | 0 |
| `7801e444` | 641,384 | 487,162 (76.0%) | 97,071 (15.1%) | 41,915 (6.5%) | 6,391 (1.0%) | 4,574 (0.7%) | 0 |
| `9f3670d5` (keep 140k) | 779,351 | 635,248 (81.5%) | 62,710 (8.0%) | 67,868 (8.7%) | 6,685 (0.9%) | 5,712 (0.7%) | 0 |

Percentages are rendered block sizes over the reported transcript length; block
headers and `---` separators account for the remainder.

The signal-to-noise problem is obvious: the model's own synthesized messages
and the user's instructions are under 4% of its input. Most of the input is raw
tool output that it must read, filter, and then restate.

The 800,000-character cap is also load-bearing. `9f3670d5` at `-keep-tokens
20000` fails with:

```text
summary input would exceed 800000 characters; raise -keep-tokens to retain
more and summarize less
```

At `-keep-tokens 140000` the same prefix fits at 779,351 characters. The Phase
5 run used 140k for that reason. As of 2026-10-28, the wrapper raises the keep
target automatically; on `9f3670d5` it converges at 134,600 tokens (134,733
estimated retained) instead of failing at the default after a paid turn.

If reasoning were included, the Phase 5 prefixes would grow by roughly 20–45%
and most would exceed the cap. The current omission is a reasonable speed and
cost choice, but it should be explicit, not an accident of `Text()`.

## Repeated compaction and summary shape

The local store on 2026-10-28 contains 40 checkpoints across 12 sessions. One
session (`0e8ae979`) has 11 checkpoints and an 8.7 MB file.

| Measure | Range | Median |
| --- | ---: | ---: |
| Summary chars | 2,667–10,602 | 6,137 |
| `before_tokens` | 37,523–336,415 | 136,879 |

There is a fixed-ish summary floor:

```text
summary_chars ≈ 0.0145 * before_tokens + 4,039   (correlation 0.66)
```

Even a 37k-token compaction produces 2.7k characters. The model writes a
full narrative every time; it does not just append a delta. Consecutive
summaries in the 11-checkpoint session share only 0–5% exact lines, so a
rolling-summary cache would not save much output. The lever is making each
summary shorter, not reusing the previous one.

The summaries are also reference-dense. In the 11-checkpoint session, lines
containing an exact reference (backticked path/commit/command/URL, PR number,
or task number) are 53–80% of lines and account for 66–94% of the characters.
That metric counts the whole line, so it overstates the exact-reference share,
but the direction is clear: a large portion of the generated text is facts that
code could copy verbatim.

That is the core design opportunity: **let code write the exact facts and let
the model write the short narrative.** It cuts generation time and improves
fidelity at the same time.

## Other findings

### The summary call has no warm provider cache

Main-turn input is 95–99% cached (`input_tokens_details.cached_tokens`;
97.6% across the five Phase 5 sessions). The
summary child starts a fresh tool-less session with a different system prompt
and a rendered transcript, so it is almost certainly a cold prompt. The
recorded data cannot prove that: `cakecli.Usage` dropped `cached_tokens` when
Phase 5 ran. The field is now captured, so the next paid run will settle it.
Phase 5's 865k summary input tokens are at full price as far as the evidence
shows. This is why the summary is 61% of the compacted path's input even
though the follow-up turn reuses most of its prefix.

A core-native compactor could reuse the session prefix cache; an external
wrapper cannot do it cleanly without reconstructing Cake's exact system prompt,
tools, and message order.

### `run` and the REPL configure the summarizer differently

In `cake-repl`, the compactor receives the user's `-model` and `-profile` when
set. In `cake-compact run`, `-model`/`-profile` apply only to the turn; the
summarizer uses `-summarizer-model`/`-summarizer-profile` or Cake's default.
Neither path passes a reasoning effort. If a user chooses a fast/low-reasoning
turn model, the summary still runs at the default model's effort. This is a
consistency and quota issue, not just a latency issue.

### Duplicate preflights are real but irrelevant

`run` checks the checkpoint verb and `--no-runtime-context` before the turn;
`Compact` checks the verb again; `Summarize` checks the flag again. Each
`cake --help` spawn is a few milliseconds. Caching them is clean but saves
under 0.1% of a 30-second compaction. Do not spend a task on it.

### Prompt-injection boundary must survive any transcript redesign

The Phase 5 synthetic injection probe passed. A distilled evidence pack that
copies raw error/tool-output lines into a ledger must keep treating those lines
as untrusted data, and should redact common secret patterns (`sk-`, `ghp_`,
`AKIA`, bearer tokens) before storing them in a checkpoint summary.

## Options

| # | Option | Expected latency | Input cost | Quality risk | Effort |
| --- | --- | --- | --- | --- | --- |
| A | Summarizer reasoning effort (output cap only if measured) | 30–60 s → 10–18 s | none | Low; A/B required | S |
| B | Evidence pack + deterministic reference ledger | 10–18 s → 6–10 s (aggressive) | −65–80%; −40–65% with a raw excerpt | Medium; A/B required | M |
| C | Prompt tightening alone | −15–25% | none | Medium; exact facts may drop | S |
| D | Adaptive transcript budget / higher keep | −1–3 s; removes 800k failure | −10–25% | Low | S–M |
| E | Speculative summary during the turn | Hides most/all remaining latency | Extra call when trigger misses | Medium; merge semantics | L |
| F | Chunked parallel map-reduce | 40 s → ~12–16 s if chunks run truly parallel | N+1 requests; similar total input | Medium; cross-chunk facts | M–L |
| G | Core-native compaction | Removes wrapper, process, and cold prefill | Large | Long-term qualification | XL |
| H | Micro-optimizations (preflight cache, direct HTTP) | <0.2 s | none | none | S |

### A. Summarizer reasoning effort; output cap only if measured

The output cap and length target are conditional, not committed. The visible
summaries are already 1.6–2.7k tokens; nothing yet shows that is too long. Test
reasoning effort first, then add a cap only if the A/B shows visible output
length is the remaining cost.

Add:

- `-summarizer-reasoning-effort` (pass `--reasoning-effort` to the summary
  child). Start with `low`; test `none` as a second arm. `--reasoning-budget`
  is an alternative if the provider honors it.
- If, and only if, the A/B shows visible output length is the remaining cost:
  - `-summary-max-tokens` (pass `--max-tokens`). Use ~4096 as a safety net, not
    as the primary length control. Detect a token-limit termination instead of
    silently accepting a truncated summary.
  - `-summary-target-tokens` (default ~1000) and a prompt instruction with a
    structural budget, e.g. "At most 12 bullets, one line each, 25 words max;
    target 800–1000 tokens; do not restate the historical summary."
- A dedicated summary profile via `-summarizer-profile`: same fast model,
  `reasoning_effort = "low"` or `"none"`, and a lower temperature such as 0.2.
  Keep it separate from the turn model. Do not switch to a different model
  without an A/B and explicit approval. An output cap belongs here only if the
  A/B justifies one.
- `summary_usage` now reports `reasoning_tokens`, `cached_tokens`, and
  `summary_visible_tokens = output_tokens - reasoning_tokens` (implemented
  2026-10-28; clamped at zero).

Why it works: hidden reasoning is not part of the visible summary, so removing
it does not directly remove content. The visible summary is already 1.6–2.7k
tokens; a 1k target is a real cut, so this needs the A/B and preferably the
ledger from option B.

Expected: output drops from 5.6–8.3k to ~1.5–2.5k tokens. At 6.06 ms/token,
that is 25–40 s saved per compaction. Input unchanged.

### B. Evidence pack + deterministic reference ledger

Instead of sending the raw transcript, build a compact evidence pack:

1. **Messages**: every real user and assistant message, verbatim (they are
   under 4% of current input and carry the goals, decisions, and state).
2. **Edits and writes**: full call arguments (diffs and file content matter).
3. **Commands**: Bash/Edit/Write/BashSession calls, deduplicated, with
   read-only noise dropped.
4. **Deterministic ledger**: extract from the *full* retired items:
   - paths mentioned in user/assistant messages, touched by edits, or present
     in error lines;
   - commit hashes, PR/issue numbers, task/ADR numbers, URLs;
   - error/failure/panic/denied lines;
   - test-result and build-result lines;
   - latest state-changing commands (`git`, `just`, `cargo`, `go`, package
     managers).
   Dedupe, cap, and redact secrets.
5. **Optional capped raw excerpt**: keep, say, 100–200k characters of raw tool
   output for the model to consult. This is the conservative variant; the
   aggressive variant omits raw output entirely.

The checkpoint summary becomes:

```text
## Narrative
<model-written goals, decisions, state, dead ends, next steps; ~500–800 tokens>

## Exact references
<code-generated ledger: paths, commits, PRs, commands, errors, test results>
```

The prompt tells the model not to repeat exact references from the ledger and
not to restate the prior historical summary. Code guarantees the facts; the
model does the synthesis.

Expected:

- Input: 640–780k chars → ~150–250k chars, or ~35–60k tokens. Prefill drops
  from ~4 s to ~1 s. This is the aggressive pack with no raw excerpt.
- With a 100–200k-character raw safety excerpt, input is ~250–450k chars
  (~40–65% less) and the expected total is ~8–12 s. Start there if fidelity
  matters more than the last few seconds.
- Output: the narrative is ~500–800 tokens instead of 1.6–2.7k visible
  tokens. Generation drops from ~10–16 s to ~3–5 s.
- Total: ~6–10 s, including the ledger build (milliseconds).
- Exact-fact coverage should be equal or better; narrative quality is the A/B
  question.

Risks: extractor misses a prose-only fact, ledger noise, injection text in
tool output, secrets. Mitigate with the optional raw excerpt, a cap on ledger
size, and the Phase 5 annotation sheets as the test set.

This is the best "without losing effectiveness" option because it removes the
part of the output the model is worst at (copying exact strings) and keeps the
part it is good at (synthesis).

### C. Prompt tightening alone

The current prompt asks for six categories and says "short headings and bullet
points" but sets no budget. The model writes 4–10k characters. A strict bullet
budget and "only new/changed facts, do not restate the historical summary"
will help. But a blunt cap without the ledger risks dropping exact facts that
the Phase 5 annotations expect. Use C as part of A/B, not as the whole answer.

### D. Adaptive transcript budget / higher keep

**Status (2026-10-28):** auto-raise implemented in cake-compact. When the
rendered prefix exceeds 800,000 characters, `Analyze` raises the keep target to
the smallest larger value whose prefix fits, then re-selects at a complete
boundary. A higher default keep (50k–80k) and `-transcript-out` remain open.

Two changes:

- Auto-raise `-keep-tokens` (or switch to evidence mode) when the rendered
  transcript would exceed the cap. Never spend the turn and then fail.
- For long sessions, prefer 50k–80k retained tokens over 20k. It cuts summary
  input, preserves more exact tail context, and reduces the chance of the cap
  failure. Phase 5 used 140k on `9f3670d5`; that is a large tail but still left
  `after_tokens` at 140k for a 497k source.
- Add `-transcript-out <path>` to `compact -dry-run` so the exact summary input
  can be inspected and sized without a model call. The existing TODO already
  wants a `-payload-out`; this is the same idea for the summary transcript.

### E. Speculative summary during the turn

The session is append-only and the tail selection moves forward as the turn
adds items. A prefix that is definitely retired before the turn remains retired
after it. So `run` can start summarizing a conservative prefix while the turn
is running, then after the turn summarize only the newly retired delta and
concatenate or merge the two summaries.

Benefits: hides most or all of the summary latency behind the turn. Costs: a
summary call is spent even when the trigger does not fire; it contends with the
turn for provider capacity; and the merge/concatenation needs quality work.
Gate it on a pre-turn measurement near the trigger, or on `-force-compact`.
This is the right phase after A/B, not before.

### F. Chunked parallel map-reduce

Split the retired prefix into 3–5 chronological chunks, summarize each in
parallel with a strict 300–500-token output (facts and decisions only), then
merge the chunk summaries into a final 700–1000-token summary. Wall time is
roughly `max(chunk time) + merge time`, so a 200k-token prefix that takes ~40 s
as one call can land near 12–16 s if the provider runs the chunks concurrently.

Costs and risks: N+1 requests, possible rate limiting, cross-chunk facts that
only make sense together, and a merge call that can lose detail. The total
input is similar to a single call; output may be somewhat higher. Use it only
if A and B still leave more than ~15 s, or as a fallback for prefixes too large
to fit one transcript. It does not fix the hidden-reasoning problem by itself;
combine it with A.

### G. Core-native compaction

The long-term fix for both latency and the in-turn overshoot. It can:

- run at provider-request boundaries (Cake task 034f/034g);
- reuse the session's warm provider cache instead of a cold 200k-token prefill;
- avoid a second process, a rendered transcript, and the external state file;
- compact during a long tool loop, which the between-turn wrapper cannot do.

Out of scope for this repo, but it is the reason not to over-invest in
increasingly complex external speculation.

## Recommendation

**Do A first, then B.**

A is the highest-leverage, lowest-risk change: it attacks the dominant output
term and does not change the visible summary. B is the quality-preserving path
that makes a shorter summary safe, cuts input cost 65–80%, and fixes the
long-session cap cliff. D is a safety fix that should land regardless.

Do not start with E or G. E is complex and spends quota speculatively; G is a
core change. F is a fallback if A and B still leave more than ~15 s.

## Paid A/B plan (spend-gated)

No calls are authorized by this document. If Trav approves, use the existing
Phase 5 sessions and annotation sheets:

1. **Arm A1**: `--reasoning-effort low`, `--max-tokens 4096`, target 800–1000
   visible tokens, same `-keep-tokens` and boundary as Phase 5.
   Sessions: `ec32da4e` (126k input), `4fc697db` (178k), `26271706` (196k).
   Cost: ~500k input, ~6k output, three summaries.
2. **Arm A2** (only if A1 passes): `--reasoning-effort none`, same sessions or
   a subset. Another ~500k input if all three.
3. **Injection probe**: rerun the 620-input synthetic probe with the new prompt;
   confirm the summary still treats tool output as data.
4. **Grading**: summary-only facts from each sheet; effective context is the
   summary plus the same deterministic retained tail, so it can be graded
   offline with no follow-up turns. Also record `summary_ms`, output tokens,
   `reasoning_tokens`, visible tokens, and summary chars.
5. **Pass criteria**: all retired facts present (same as Phase 5 or better), no
   contradictions or invented work, injection still declined, `summary_ms`
   under ~20 s per real session, visible summary 800–1200 tokens, output tokens
   under 2.5k.

This is much cheaper than Phase 5: one arm is ~0.5M input and no follow-up
turns. If A1 passes, consider a second arm on one session to test `none` before
choosing the default.

For B, the A/B can use the same three sessions with the evidence pack; its
input is expected to be 60–100k per session, so a three-session arm is roughly
200–300k input. Test the conservative pack (with a 100–200k-char raw excerpt)
first; only remove the raw excerpt if fidelity holds.

## Instrumentation to add before the A/B

Implemented on 2026-10-28 and free:

- `summary_usage.reasoning_tokens`, `summary_usage.cached_tokens`, and
  `summary_visible_tokens = output_tokens - reasoning_tokens` from the child's
  `usage` object. `cakecli.Usage` now reads both detail objects and flattens
  them into the result document.

Still additive and free:

- `summary_target_tokens`, `summary_max_tokens`, and the summarizer model /
  reasoning effort in the result document.
- `transcript_mode`, `transcript_items`, `transcript_chars`, and
  `transcript_dropped_items` / `ledger_chars` for the evidence pack.
- `-transcript-out` on `compact -dry-run`.
- Optional: `summary_first_token_ms` by switching the child to
  `--output-format stream-json`. This would separate process/prefill from
  generation and prove the reasoning split without inference.

## What not to optimize

- Parsing, projection, selection, and checkpoint append: 33–50 ms.
- Duplicate `cake --help` preflights: 4–5 ms each.
- Cake startup: under 10 ms.
- A direct provider HTTP call: saves ~10 ms of process startup and adds auth,
  retry, usage, and settings drift.
- A blunt hard cap on summary length without a ledger: likely loses the exact
  facts the Phase 5 sheets require.

## Open decisions for Trav

1. After Trav reviews the new `summary_usage` numbers, authorize the A1 paid
   A/B (~0.5M input, three summaries) against the Phase 5 annotations?
2. If A1 passes, should `low` be the summarizer default, or stay opt-in?
3. Approve the narrative + deterministic-ledger summary shape for B? It changes
   what a checkpoint summary looks like, but it should improve exact-fact
   fidelity.
4. Should the reasoning transcript stay dropped? Recommend yes for now, stated
   explicitly, because including it would add 20–45% input and break the 800k
   cap on the Phase 5 prefixes.
5. Should `run` default the summarizer to the turn's model/profile, or keep the
   separate `-summarizer-*` controls? Recommend keeping them separate for cost
   control.

## Reproduction notes

- Stage baseline: `docs/compaction-stage-baseline.md`.
- Phase 5 timings and usage: `../../research/context-compaction-phase5-results.md`.
- Checkpoint audit:

  ```sh
  for f in ~/.local/share/cake/sessions/*.jsonl; do
    jq -c 'select(.type=="compaction") |
      {summary_chars: (.summary|length),
       before: .estimated_input.before_tokens,
       retained: (.retained|length),
       version: .summary_instruction_version}' "$f"
  done
  ```

- 800k-cap check (updated 2026-10-28):

  ```sh
  ./bin/cake-compact compact -dry-run -force -keep-tokens 20000 -json \
    9f3670d5-84a9-4361-9910-4c220db643ec
  ```

  It raises `keep_target` to 134,600 and plans a 134,733-token tail without a
  model call. Before the auto-raise, the same command failed with the
  800,000-character message; an explicit `-keep-tokens 140000` was needed.

- Transcript composition was measured by loading the session, calling
  `compact.Analyze` with `Force: true` and the stated keep target, then
  grouping `plan.Prefix` by item kind and rendered size. The temporary analyzer
  was removed; the numbers above are the retained evidence.
