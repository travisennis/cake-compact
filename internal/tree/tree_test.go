package tree_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/travisennis/cake-compact/internal/compact"
	"github.com/travisennis/cake-compact/internal/session"
	"github.com/travisennis/cake-compact/internal/tree"
)

const (
	testSessionID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	testTaskID    = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

func rawFixture() string {
	return strings.Join([]string{
		`{"type":"session_meta","format_version":4,"session_id":"` + testSessionID + `","timestamp":"2026-10-29T12:00:00Z","working_directory":"/tmp","tools":[],"model":"test-model"}`,
		`{"type":"task_start","session_id":"` + testSessionID + `","task_id":"` + testTaskID + `","timestamp":"2026-10-29T12:00:01Z"}`,
		`{"type":"message","role":"user","content":"first request"}`,
		`{"type":"reasoning","id":"r1","content":[{"type":"reasoning_text","text":"think about it"}]}`,
		`{"type":"message","role":"assistant","content":"first answer"}`,
		`{"type":"message","role":"user","content":"second request"}`,
		`{"type":"message","role":"assistant","content":"second answer"}`,
		`{"type":"turn_usage","session_id":"` + testSessionID + `","task_id":"` + testTaskID + `","turn":1,"usage":{"input_tokens":90000,"output_tokens":10,"total_tokens":90010},"timestamp":"2026-10-29T12:00:10Z","usage_presence":"complete","context_window":100000}`,
	}, "\n") + "\n"
}

func checkpointFixture() string {
	return strings.Join([]string{
		`{"type":"session_meta","format_version":4,"session_id":"` + testSessionID + `","timestamp":"2026-10-29T12:00:00Z","working_directory":"/tmp","tools":[],"model":"test-model"}`,
		`{"type":"task_start","session_id":"` + testSessionID + `","task_id":"` + testTaskID + `","timestamp":"2026-10-29T12:00:01Z"}`,
		`{"type":"message","role":"user","content":"old request"}`,
		`{"type":"message","role":"assistant","content":"old answer"}`,
		`{"type":"compaction","checkpoint_version":1,"checkpoint_id":"cp-1","session_id":"` + testSessionID + `","task_id":"` + testTaskID + `","timestamp":"2026-10-29T12:00:02Z","trigger":"manual","summary":"prior summary","retained":[{"type":"message","role":"user","content":"recent request"},{"type":"message","role":"assistant","content":"recent answer"}],"model":{"model":"test-model"},"summary_instruction_version":3,"estimated_input":{"before_tokens":80000,"after_tokens":42,"basis":"measured_usage"}}`,
		`{"type":"message","role":"user","content":"next request"}`,
		`{"type":"turn_usage","session_id":"` + testSessionID + `","task_id":"` + testTaskID + `","turn":2,"usage":{"input_tokens":90000,"output_tokens":10,"total_tokens":90010},"timestamp":"2026-10-29T12:00:10Z","usage_presence":"complete","context_window":100000}`,
	}, "\n") + "\n"
}

func render(t *testing.T, data string) (string, *compact.Plan, compact.Decision) {
	t.Helper()
	file, err := session.LoadBytes("fixture.jsonl", []byte(data))
	if err != nil {
		t.Fatal(err)
	}
	plan, decision, err := compact.Analyze(file, compact.Options{
		Window:     100000,
		KeepTokens: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := tree.Render(&out, tree.View{File: file, Plan: plan, Decision: decision}); err != nil {
		t.Fatal(err)
	}
	return out.String(), plan, decision
}

func TestRenderShowsRawRecordsAndTheRetirementSplit(t *testing.T) {
	out, plan, decision := render(t, rawFixture())

	if len(plan.Prefix) != 3 || len(plan.Retained) != 2 || plan.Anchor != 3 {
		t.Fatalf("plan split = %d prefix, %d retained, anchor %d; want 3, 2, 3",
			len(plan.Prefix), len(plan.Retained), plan.Anchor)
	}
	if !decision.Compact {
		t.Fatalf("decision = %+v, want compact", decision)
	}
	for _, want := range []string{
		"RAW FILE\n",
		"8 records: 5 conversation, 3 metadata, 0 checkpoints",
		"line  1  session_meta",
		"line  4  reasoning",
		"think about it",
		"line  8  turn_usage",
		"input 90000 tokens, window 100000",
		"EFFECTIVE CONTEXT (what the next model call sees)",
		"no checkpoint; raw conversation only",
		"--- RETIRED PREFIX (becomes the summary transcript) ---",
		"--- RETAINED TAIL (copied into the checkpoint) ---",
		"first request",
		"second request",
		"PIN user_input",
		"3 retired + 1 summary -> new prefix",
		"2 retained records stay in the checkpoint",
		"estimated input: 90000 tokens (measured_usage)",
		"would compact: yes",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output does not contain %q:\n%s", want, out)
		}
	}
}

func TestRenderUsesProjectedIndicesAcrossThePinnedRequest(t *testing.T) {
	out, _, _ := render(t, rawFixture())

	// The prefix is projected items 0-2. The retained tail is the pinned
	// request at 3 followed by the assistant answer at 4.
	for _, want := range []string{
		"   0  USER",
		"   1  reasoning",
		"   2  ASSISTANT",
		"   3  USER",
		"   4  ASSISTANT",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output does not contain %q:\n%s", want, out)
		}
	}
	retainedAt := strings.Index(out, "--- RETAINED TAIL")
	if retainedAt < 0 {
		t.Fatalf("output has no retained section:\n%s", out)
	}
	if !strings.Contains(out[retainedAt:], "   3  USER") {
		t.Fatalf("pinned request is not in the retained section:\n%s", out)
	}
}

func TestRenderShowsAProjectedCheckpoint(t *testing.T) {
	out, plan, _ := render(t, checkpointFixture())

	if len(plan.Prefix) != 3 || len(plan.Retained) != 1 || plan.Anchor != 3 {
		t.Fatalf("plan split = %d prefix, %d retained, anchor %d; want 3, 1, 3",
			len(plan.Prefix), len(plan.Retained), plan.Anchor)
	}
	for _, want := range []string{
		"line  5  compaction",
		"checkpoint cp-1 boundary 2, 2 retained",
		"projected from checkpoint cp-1: 1 summary + 2 retained + 1 later",
		"synthetic historical summary",
		"recent request",
		"recent answer",
		"next request",
		"PIN user_input",
		"1 item, ",
		"3 retired + 1 summary -> new prefix",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output does not contain %q:\n%s", want, out)
		}
	}
}

func TestRenderMapsPrefixIndicesAcrossThePinnedRequest(t *testing.T) {
	file, err := session.LoadBytes("fixture.jsonl", []byte(fmt.Sprintf(
		`{"type":"session_meta","format_version":4,"session_id":%q,"tools":[]}`+"\n", testSessionID)))
	if err != nil {
		t.Fatal(err)
	}
	message := func(role, content string) session.Item {
		return session.Item{Message: &session.Message{Role: role, Content: content}}
	}
	plan := &compact.Plan{
		Projected: []session.Item{
			message("user", "owning request"),
			message("assistant", "first answer"),
			message("assistant", "second answer"),
			message("assistant", "last answer"),
		},
		Prefix: []session.Item{
			message("assistant", "first answer"),
			message("assistant", "second answer"),
		},
		Retained: []session.Item{
			{Raw: []byte(`{"type":"message","role":"user","content":"owning request","pin":{"kind":"user_input"}}`), Message: &session.Message{Role: "user", Content: "owning request"}},
			message("assistant", "last answer"),
		},
		Start:  3,
		Anchor: 0,
	}

	var out bytes.Buffer
	if err := tree.Render(&out, tree.View{File: file, Plan: plan, Decision: compact.Decision{Compact: true}}); err != nil {
		t.Fatal(err)
	}
	rendered := out.String()
	for _, want := range []string{
		"   1  ASSISTANT",
		"   2  ASSISTANT",
		"   0  USER",
		"   3  ASSISTANT",
		"PIN user_input",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("output does not contain %q:\n%s", want, rendered)
		}
	}
	prefixAt := strings.Index(rendered, "--- RETIRED PREFIX")
	retainedAt := strings.Index(rendered, "--- RETAINED TAIL")
	if prefixAt < 0 || retainedAt < 0 {
		t.Fatalf("output has no split sections:\n%s", rendered)
	}
	if !strings.Contains(rendered[prefixAt:retainedAt], "   1  ASSISTANT") ||
		!strings.Contains(rendered[prefixAt:retainedAt], "   2  ASSISTANT") {
		t.Fatalf("shifted prefix indices are not in the prefix section:\n%s", rendered)
	}
}

func TestRenderWithoutAPlanStillShowsTheProjection(t *testing.T) {
	file, err := session.LoadBytes("fixture.jsonl", []byte(rawFixture()))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := tree.Render(&out, tree.View{File: file}); err != nil {
		t.Fatal(err)
	}
	rendered := out.String()
	if !strings.Contains(rendered, "no checkpoint; raw conversation only") {
		t.Fatalf("output does not contain the projection header:\n%s", rendered)
	}
	if !strings.Contains(rendered, "first request") || strings.Contains(rendered, "RETIRED PREFIX") {
		t.Fatalf("output should show the projection without a split:\n%s", rendered)
	}
}

func TestRenderRejectsANilFile(t *testing.T) {
	var out bytes.Buffer
	if err := tree.Render(&out, tree.View{}); err == nil {
		t.Fatal("Render() error = nil, want an error")
	}
}

func TestRenderHandlesAnEmptyProjection(t *testing.T) {
	data := fmt.Sprintf(`{"type":"session_meta","format_version":4,"session_id":%q,"tools":[]}`+"\n", testSessionID)
	file, err := session.LoadBytes("fixture.jsonl", []byte(data))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := tree.Render(&out, tree.View{File: file, Plan: &compact.Plan{}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no conversation items") {
		t.Fatalf("output does not contain an empty-projection note:\n%s", out.String())
	}
}
