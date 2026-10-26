package session

import (
	"strings"
	"testing"
)

const fixtureSessionID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

func metaLine() string {
	return `{"type":"session_meta","format_version":4,"session_id":"` + fixtureSessionID + `","timestamp":"2026-10-23T12:00:00Z","working_directory":"/tmp","tools":["Read"],"model":"test-model","model_config":"test"}`
}

func TestProjectedWithoutCheckpointIsRawConversation(t *testing.T) {
	lines := []string{
		metaLine(),
		`{"type":"task_start","session_id":"` + fixtureSessionID + `","task_id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","timestamp":"2026-10-23T12:00:01Z"}`,
		`{"type":"message","role":"user","content":"first"}`,
		`{"type":"function_call","id":"fc-1","call_id":"call-1","name":"Read","arguments":"{}"}`,
		`{"type":"function_call_output","call_id":"call-1","output":"contents"}`,
		`{"type":"message","role":"assistant","content":"done"}`,
		`{"type":"turn_usage","session_id":"` + fixtureSessionID + `","task_id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","turn":1,"usage":{"input_tokens":1234,"input_tokens_details":{"cached_tokens":0},"output_tokens":10,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":1244},"timestamp":"2026-10-23T12:00:02Z","usage_presence":"complete","context_window":100000,"remaining_context_tokens":98766}`,
	}
	file, err := LoadBytes("fixture.jsonl", []byte(strings.Join(lines, "\n")+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	items, err := file.Projected()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("expected 4 conversation items, got %d", len(items))
	}
	if items[0].Message == nil || items[0].Message.Content != "first" {
		t.Fatalf("unexpected first item: %+v", items[0])
	}
	index, usage := file.LastUsage()
	if index != 6 || usage == nil || usage.Usage.InputTokens != 1234 {
		t.Fatalf("unexpected usage: index=%d usage=%+v", index, usage)
	}
	if usage.ContextWindow == nil || *usage.ContextWindow != 100000 {
		t.Fatalf("unexpected context window: %+v", usage.ContextWindow)
	}
	if estimated := EstimateTokens(nil); estimated != 0 {
		t.Fatalf("empty estimate = %d, want 0", estimated)
	}
}

func TestProjectedUsesLatestCheckpointAndPreservesProvenance(t *testing.T) {
	lines := []string{
		metaLine(),
		`{"type":"message","role":"user","content":"retired raw"}`,
		`{"type":"compaction","checkpoint_version":1,"checkpoint_id":"c-old","session_id":"` + fixtureSessionID + `","task_id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","timestamp":"2026-10-23T12:00:01Z","trigger":"manual","summary":"old summary","retained":[{"type":"message","role":"user","content":"old retained"}],"model":{"model":"test-model"},"summary_instruction_version":1,"estimated_input":{"before_tokens":100,"after_tokens":20,"basis":"content_class"}}`,
		`{"type":"message","role":"assistant","content":"intervening"}`,
		`{"type":"compaction","checkpoint_version":1,"checkpoint_id":"c-new","session_id":"` + fixtureSessionID + `","task_id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","timestamp":"2026-10-23T12:00:02Z","trigger":"threshold","summary":"new summary","retained":[{"type":"message","role":"user","content":"new retained","pin":{"kind":"user_input"},"origin":"user"},{"type":"function_call","id":"fc-1","call_id":"call-1","name":"Read","arguments":"{}"},{"type":"function_call_output","call_id":"call-1","output":"contents"}],"model":{"model":"test-model"},"summary_instruction_version":1,"estimated_input":{"before_tokens":200,"after_tokens":30,"basis":"measured_usage"}}`,
		`{"type":"message","role":"assistant","content":"after checkpoint"}`,
	}
	file, err := LoadBytes("fixture.jsonl", []byte(strings.Join(lines, "\n")+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if index := file.LatestCheckpointIndex(); index != 4 {
		t.Fatalf("latest checkpoint index = %d, want 4", index)
	}
	items, err := file.Projected()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 5 {
		t.Fatalf("expected summary + 3 retained + 1 later item, got %d", len(items))
	}
	if !items[0].Synthetic || !strings.HasPrefix(items[0].Message.Content, HistoricalSummaryMarker) {
		t.Fatalf("first item is not the summary: %+v", items[0])
	}
	if !strings.Contains(items[0].Message.Content, "new summary") {
		t.Fatalf("summary does not use the latest checkpoint: %q", items[0].Message.Content)
	}
	if !strings.Contains(string(items[1].Raw), `"pin"`) {
		t.Fatalf("retained provenance was not preserved: %s", items[1].Raw)
	}
	if items[4].Message == nil || items[4].Message.Content != "after checkpoint" {
		t.Fatalf("later record missing: %+v", items[4])
	}
}

func TestLoadRejectsForeignCheckpointEnvelope(t *testing.T) {
	lines := []string{
		metaLine(),
		`{"type":"compaction","checkpoint_version":1,"checkpoint_id":"c-1","session_id":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","task_id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","timestamp":"2026-10-23T12:00:01Z","trigger":"manual","summary":"summary","retained":[],"model":{"model":"test-model"},"summary_instruction_version":1,"estimated_input":{"before_tokens":100,"after_tokens":20,"basis":"content_class"}}`,
	}
	_, err := LoadBytes("fixture.jsonl", []byte(strings.Join(lines, "\n")+"\n"))
	if err == nil || !strings.Contains(err.Error(), "belongs to session") {
		t.Fatalf("expected a foreign-envelope error, got %v", err)
	}
}

func TestLastUsageSkipsPartial(t *testing.T) {
	lines := []string{
		metaLine(),
		`{"type":"turn_usage","session_id":"` + fixtureSessionID + `","task_id":"t","turn":1,"usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":0},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":1001},"timestamp":"2026-10-23T12:00:01Z","usage_presence":"complete"}`,
		`{"type":"turn_usage","session_id":"` + fixtureSessionID + `","task_id":"t","turn":2,"usage":{"input_tokens":2000,"input_tokens_details":{"cached_tokens":0},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":2001},"timestamp":"2026-10-23T12:00:02Z","usage_presence":"partial"}`,
	}
	file, err := LoadBytes("fixture.jsonl", []byte(strings.Join(lines, "\n")+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	index, usage := file.LastUsage()
	if index != 1 || usage.Usage.InputTokens != 1000 {
		t.Fatalf("partial usage was not skipped: index=%d usage=%+v", index, usage)
	}
}

func TestLoadToleratesTornFinalRecord(t *testing.T) {
	data := metaLine() + "\n" + `{"type":"message","role":"user","content":"ok"}` + "\n" + `{"type":"message","role":"user","content":"tor`
	file, err := LoadBytes("fixture.jsonl", []byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Warnings) != 1 {
		t.Fatalf("expected one torn-tail warning, got %v", file.Warnings)
	}
	if len(file.Records) != 2 {
		t.Fatalf("expected meta + one message, got %d records", len(file.Records))
	}
}

func TestReasoningTextHandlesTypedAndPlainSummaries(t *testing.T) {
	typed := &Reasoning{Summary: []byte(`[{"type":"summary_text","text":"first"},{"type":"summary_text","text":"second"}]`)}
	if text := typed.Text(); text != "first\nsecond" {
		t.Fatalf("typed reasoning text = %q", text)
	}
	plain := &Reasoning{Summary: []byte(`["one","two"]`)}
	if text := plain.Text(); text != "one\ntwo" {
		t.Fatalf("plain reasoning text = %q", text)
	}
}
