package compact

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/travisennis/cake-compact/internal/cakecli"
	"github.com/travisennis/cake-compact/internal/session"
)

const (
	sourceID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	newID    = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	taskID   = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

func fixtureLines() string {
	return strings.Join([]string{
		`{"type":"session_meta","format_version":4,"session_id":"` + sourceID + `","timestamp":"2026-10-23T12:00:00Z","working_directory":"/tmp","tools":["Read"],"model":"test-model","model_config":"test"}`,
		`{"type":"task_start","session_id":"` + sourceID + `","task_id":"` + taskID + `","timestamp":"2026-10-23T12:00:01Z","model":"test-model","model_config":"test"}`,
		`{"type":"message","role":"user","content":"Please read the config and explain the retry policy."}`,
		`{"type":"message","role":"assistant","content":"I will read the config."}`,
		`{"type":"function_call","id":"fc-1","call_id":"call-1","name":"Read","arguments":"{\"file_path\":\"config.toml\"}"}`,
		`{"type":"function_call_output","call_id":"call-1","output":"retries = 3\ntimeout = 30s"}`,
		`{"type":"message","role":"assistant","content":"The retry policy allows three attempts with a 30 second timeout."}`,
		`{"type":"message","role":"user","content":"Now change the timeout to 60 seconds."}`,
		`{"type":"message","role":"assistant","content":"I updated the timeout."}`,
		`{"type":"turn_usage","session_id":"` + sourceID + `","task_id":"` + taskID + `","turn":2,"usage":{"input_tokens":90000,"input_tokens_details":{"cached_tokens":0},"output_tokens":200,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":90200},"timestamp":"2026-10-23T12:00:10Z","usage_presence":"complete","context_window":100000,"remaining_context_tokens":10000}`,
		`{"type":"task_complete","subtype":"success","is_error":false,"result":"done","duration_ms":1000,"turn_count":2,"tool_call_count":1,"usage":{"input_tokens":90000,"input_tokens_details":{"cached_tokens":0},"output_tokens":200,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":90200},"session_id":"` + sourceID + `","task_id":"` + taskID + `"}`,
	}, "\n") + "\n"
}

func writeFixture(t *testing.T, dataDir string) string {
	t.Helper()
	dir := cakecli.SessionsDir(dataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sourceID+".jsonl")
	if err := os.WriteFile(path, []byte(fixtureLines()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeSummary(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "summary.txt")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func loadFixture(t *testing.T) *session.File {
	t.Helper()
	file, err := session.LoadBytes("fixture.jsonl", []byte(fixtureLines()))
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func messageItem(role, content string) session.Item {
	return session.Item{Type: session.TypeMessage, Message: &session.Message{Role: role, Content: content}}
}

func callItem(callID string) session.Item {
	return session.Item{Type: session.TypeFunctionCall, Call: &session.FunctionCall{CallID: callID, Name: "Read", Arguments: "{}"}}
}

func outputItem(callID string) session.Item {
	return session.Item{Type: session.TypeFunctionOutput, Output: &session.FunctionCallOutput{CallID: callID, Output: "result"}}
}

func TestSelectStartKeepsCompleteExchanges(t *testing.T) {
	items := []session.Item{
		messageItem("user", "first"),
		messageItem("assistant", "one"),
		messageItem("user", "second"),
		messageItem("assistant", "two"),
	}
	start, err := selectStart(items, 1)
	if err != nil {
		t.Fatal(err)
	}
	if start != 2 {
		t.Fatalf("start = %d, want 2 (second user message)", start)
	}

	parallel := []session.Item{
		messageItem("user", "run both"),
		callItem("call-1"),
		callItem("call-2"),
		outputItem("call-2"),
		outputItem("call-1"),
		messageItem("assistant", "both done"),
	}
	start, err = selectStart(parallel, 1)
	if err != nil {
		t.Fatal(err)
	}
	if start != 0 {
		t.Fatalf("parallel start = %d, want 0", start)
	}
}

func TestSelectStartRejectsCrossBoundaryAndUnfinishedCalls(t *testing.T) {
	crossing := []session.Item{
		messageItem("user", "first"),
		callItem("call-1"),
		messageItem("user", "second"),
		outputItem("call-1"),
	}
	if _, err := selectStart(crossing, 1); err == nil {
		t.Fatal("a call split by a user message must not be retained")
	}

	unfinished := []session.Item{
		messageItem("user", "first"),
		callItem("call-1"),
	}
	if _, err := selectStart(unfinished, 1); err == nil {
		t.Fatal("an unfinished call must not be retained")
	}
}

func TestAnalyzeThresholdAndPlan(t *testing.T) {
	file := loadFixture(t)
	plan, decision, err := Analyze(file, Options{Force: true, KeepTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Compact {
		t.Fatalf("expected a compact decision: %s", decision.Reason)
	}
	if plan.BeforeTokens != 90000 {
		t.Fatalf("before = %d, want 90000", plan.BeforeTokens)
	}
	if plan.Window != 100000 || plan.ThresholdTokens != 80000 {
		t.Fatalf("window/threshold = %d/%d", plan.Window, plan.ThresholdTokens)
	}
	if len(plan.Retained) != 2 || len(plan.Prefix) != 5 {
		t.Fatalf("prefix/retained = %d/%d, want 5/2", len(plan.Prefix), len(plan.Retained))
	}
}

func TestAnalyzeBelowThresholdAndMissingWindow(t *testing.T) {
	file := loadFixture(t)
	_, decision, err := Analyze(file, Options{ThresholdTokens: 200000, KeepTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Compact {
		t.Fatalf("expected no compaction below the threshold: %s", decision.Reason)
	}
	if !strings.Contains(decision.Reason, "below threshold") {
		t.Fatalf("unexpected reason: %s", decision.Reason)
	}

	noWindow, err := session.LoadBytes("fixture.jsonl", []byte(strings.Replace(fixtureLines(), `,"context_window":100000,"remaining_context_tokens":10000`, "", 1)))
	if err != nil {
		t.Fatal(err)
	}
	_, decision, err = Analyze(noWindow, Options{KeepTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Compact || !strings.Contains(decision.Reason, "no context window") {
		t.Fatalf("unexpected missing-window decision: %+v", decision)
	}
}

func TestBuildCheckpointShape(t *testing.T) {
	file := loadFixture(t)
	plan, decision, err := Analyze(file, Options{Force: true, KeepTokens: 10})
	if err != nil || !decision.Compact {
		t.Fatalf("analyze: %v %+v", err, decision)
	}
	checkpoint, line, err := BuildCheckpoint(plan, "A compact summary.", newID, taskID, time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(line) == 0 || line[len(line)-1] != '\n' {
		t.Fatal("checkpoint line must end with a newline")
	}
	if len(line) > MaxCheckpointRecordBytes {
		t.Fatal("checkpoint exceeds the record ceiling")
	}
	if checkpoint.SessionID != newID || checkpoint.TaskID != taskID {
		t.Fatalf("unexpected envelope: %+v", checkpoint)
	}
	if checkpoint.Trigger != "threshold" || checkpoint.CheckpointVersion != 1 {
		t.Fatalf("unexpected trigger/version: %+v", checkpoint)
	}
	if len(checkpoint.Retained) != 2 || checkpoint.EstimatedInput.AfterTokens >= checkpoint.EstimatedInput.BeforeTokens {
		t.Fatalf("unexpected estimates: %+v", checkpoint.EstimatedInput)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(line, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"type", "checkpoint_version", "checkpoint_id", "session_id", "task_id", "timestamp", "trigger", "summary", "retained", "model", "summary_instruction_version", "estimated_input"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("checkpoint is missing %q", key)
		}
	}
}

func TestBuildCheckpointRejectsOversizedRecord(t *testing.T) {
	file := loadFixture(t)
	plan := &Plan{
		File:         file,
		Retained:     []session.Item{{Type: session.TypeMessage, Raw: json.RawMessage(`{"type":"message","role":"user","content":"` + strings.Repeat("x", MaxCheckpointRecordBytes) + `"}`)}},
		BeforeTokens: 100000,
		Basis:        "content_class",
		Trigger:      "manual",
	}
	if _, _, err := BuildCheckpoint(plan, "summary", newID, taskID, time.Now()); err == nil {
		t.Fatal("an oversized checkpoint must be rejected")
	}
}

func TestCompactWritesAValidatedNewSession(t *testing.T) {
	dataDir := t.TempDir()
	sourcePath := writeFixture(t, dataDir)
	original, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}

	compactor := &Compactor{
		Mode:    ModeFork,
		DataDir: dataDir,
		Options: Options{
			Force:               true,
			KeepTokens:          10,
			SummaryFile:         writeSummary(t, "The user asked about the retry policy and then changed the timeout to 60 seconds."),
			Trigger:             "manual",
			MinReductionPercent: 10,
		},
		Now:   func() time.Time { return time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC) },
		NewID: func() (string, error) { return newID, nil },
	}

	result, err := compactor.Compact(context.Background(), sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Compacted {
		t.Fatalf("expected compaction: %s", result.Reason)
	}
	if result.SessionID != newID || result.CheckpointID == "" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.AfterTokens >= result.BeforeTokens {
		t.Fatalf("after %d is not smaller than before %d", result.AfterTokens, result.BeforeTokens)
	}

	current, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != string(original) {
		t.Fatal("the source session was modified")
	}

	newPath := filepath.Join(cakecli.SessionsDir(dataDir), newID+".jsonl")
	compacted, err := session.Load(newPath)
	if err != nil {
		t.Fatal(err)
	}
	if compacted.SessionID() != newID {
		t.Fatalf("new session id = %s", compacted.SessionID())
	}
	if len(compacted.Records) != 2 {
		t.Fatalf("expected meta + checkpoint, got %d records", len(compacted.Records))
	}
	checkpoint := compacted.Records[1].Checkpoint
	if checkpoint == nil || checkpoint.SessionID != newID {
		t.Fatalf("missing re-enveloped checkpoint: %+v", checkpoint)
	}
	if !strings.Contains(checkpoint.Summary, "retry policy") {
		t.Fatalf("unexpected summary: %q", checkpoint.Summary)
	}
	if compacted.LatestCheckpointIndex() != 1 {
		t.Fatalf("checkpoint index = %d", compacted.LatestCheckpointIndex())
	}
}

func TestCompactDryRunWritesNothing(t *testing.T) {
	dataDir := t.TempDir()
	sourcePath := writeFixture(t, dataDir)
	compactor := &Compactor{
		Mode:    ModeFork,
		DataDir: dataDir,
		Options: Options{
			Force:       true,
			KeepTokens:  10,
			DryRun:      true,
			SummaryFile: writeSummary(t, "short summary"),
		},
		Now:   time.Now,
		NewID: func() (string, error) { return newID, nil },
	}
	result, err := compactor.Compact(context.Background(), sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if result.Compacted || !strings.Contains(result.Reason, "dry-run") {
		t.Fatalf("unexpected dry-run result: %+v", result)
	}
	if result.AfterTokens == 0 {
		t.Fatal("dry-run with a summary file should report the estimated after size")
	}
	if _, err := os.Stat(filepath.Join(cakecli.SessionsDir(dataDir), newID+".jsonl")); !os.IsNotExist(err) {
		t.Fatal("dry-run wrote a session")
	}
}

func TestCompactRejectsInsufficientReduction(t *testing.T) {
	dataDir := t.TempDir()
	sourcePath := writeFixture(t, dataDir)
	compactor := &Compactor{
		Mode:    ModeFork,
		DataDir: dataDir,
		Options: Options{
			Force:       true,
			KeepTokens:  10,
			SummaryFile: writeSummary(t, strings.Repeat("verbose summary ", 40000)),
		},
		Now:   time.Now,
		NewID: func() (string, error) { return newID, nil },
	}
	result, err := compactor.Compact(context.Background(), sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if result.Compacted {
		t.Fatal("a summary larger than the retired context must not commit")
	}
	if _, err := os.Stat(filepath.Join(cakecli.SessionsDir(dataDir), newID+".jsonl")); !os.IsNotExist(err) {
		t.Fatal("a rejected compaction wrote a session")
	}
}

func TestCompactingACompactedSessionUsesTheProjection(t *testing.T) {
	dataDir := t.TempDir()
	sourcePath := writeFixture(t, dataDir)
	first := &Compactor{
		Mode:    ModeFork,
		DataDir: dataDir,
		Options: Options{Force: true, KeepTokens: 10, SummaryFile: writeSummary(t, "first summary about the retry policy")},
		Now:     time.Now,
		NewID:   func() (string, error) { return newID, nil },
	}
	firstResult, err := first.Compact(context.Background(), sourcePath)
	if err != nil || !firstResult.Compacted {
		t.Fatalf("first compaction: %v %+v", err, firstResult)
	}

	secondID := "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	firstPath := filepath.Join(cakecli.SessionsDir(dataDir), newID+".jsonl")
	second := &Compactor{
		Mode:    ModeFork,
		DataDir: dataDir,
		Options: Options{Force: true, KeepTokens: 1, SummaryFile: writeSummary(t, "second summary")},
		Now:     time.Now,
		NewID:   func() (string, error) { return secondID, nil },
	}
	secondResult, err := second.Compact(context.Background(), firstPath)
	if err != nil {
		t.Fatal(err)
	}
	if !secondResult.Compacted {
		t.Fatalf("second compaction: %s", secondResult.Reason)
	}
	secondFile, err := session.Load(filepath.Join(cakecli.SessionsDir(dataDir), secondID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := secondFile.Records[1].Checkpoint
	if checkpoint == nil || checkpoint.Summary != "second summary" {
		t.Fatalf("unexpected second checkpoint: %+v", checkpoint)
	}
	if len(checkpoint.Retained) != 2 || checkpoint.Retained[0] == nil {
		t.Fatalf("expected the latest exchange to be retained: %+v", checkpoint.Retained)
	}
	var retained struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(checkpoint.Retained[0], &retained); err != nil {
		t.Fatal(err)
	}
	if retained.Role != "user" || retained.Content != "Now change the timeout to 60 seconds." {
		t.Fatalf("unexpected retained head: %+v", retained)
	}
}

func TestRealCakeForkSessionPassesReplay(t *testing.T) {
	binary := os.Getenv("CAKE_BIN")
	if binary == "" {
		t.Skip("set CAKE_BIN to a cake build with checkpoint support to run replay validation")
	}
	dataDir := t.TempDir()
	sourcePath := writeFixture(t, dataDir)
	compactor := &Compactor{
		Mode:    ModeFork,
		DataDir: dataDir,
		CakeBin: binary,
		Options: Options{Force: true, KeepTokens: 10, SummaryFile: writeSummary(t, "summary for replay validation")},
		Now:     time.Now,
		NewID:   func() (string, error) { return newID, nil },
	}
	result, err := compactor.Compact(context.Background(), sourcePath)
	if err != nil || !result.Compacted {
		t.Fatalf("compaction: %v %+v", err, result)
	}
	output, err := cakecli.Replay(context.Background(), binary, dataDir, result.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), `"type":"compaction"`) {
		t.Fatalf("replay did not emit the checkpoint: %s", output)
	}
}

// usageSummarizer stands in for the Cake summarizer: it returns fixed text and
// the provider usage the result document must carry.
type usageSummarizer struct {
	text  string
	usage cakecli.Usage
}

func (s usageSummarizer) Summarize(context.Context, string) (string, cakecli.Usage, error) {
	return s.text, s.usage, nil
}

func TestCompactReportsSummaryUsageAndWallTime(t *testing.T) {
	dataDir := t.TempDir()
	sourcePath := writeFixture(t, dataDir)

	// Every clock read advances 5ms so the measured window is deterministic.
	var ticks int64
	clock := func() time.Time {
		ticks++
		return time.Unix(0, 0).Add(time.Duration(ticks) * 5 * time.Millisecond)
	}
	wantUsage := cakecli.Usage{InputTokens: 1200, OutputTokens: 300, TotalTokens: 1500}
	compactor := &Compactor{
		Mode:    ModeInPlace,
		DataDir: dataDir,
		CakeBin: writeFakeCake(t),
		Options: Options{Force: true, KeepTokens: 10, Trigger: "manual"},
		Summarizer: usageSummarizer{
			text:  "the user asked about the retry policy and changed the timeout to 60 seconds",
			usage: wantUsage,
		},
		Now:   clock,
		NewID: NewUUID,
	}

	result, err := compactor.Compact(context.Background(), sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Compacted || !result.InPlace {
		t.Fatalf("result = %+v", result)
	}
	if result.SummaryUsage == nil || *result.SummaryUsage != wantUsage {
		t.Fatalf("summary usage = %+v, want %+v", result.SummaryUsage, wantUsage)
	}
	if result.SummaryTimeMs <= 0 {
		t.Fatalf("summary time = %d, want a positive wall time", result.SummaryTimeMs)
	}

	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"summary_usage", "summary_time_ms"} {
		if _, ok := document[key]; !ok {
			t.Fatalf("document has no %s: %s", key, raw)
		}
	}
}

// A summarizer with no provider behind it reports no usage, so the document
// omits the field rather than claiming zero tokens.
func TestCompactOmitsUsageWithoutAProvider(t *testing.T) {
	dataDir := t.TempDir()
	sourcePath := writeFixture(t, dataDir)
	compactor := &Compactor{
		Mode:       ModeInPlace,
		DataDir:    dataDir,
		CakeBin:    writeFakeCake(t),
		Options:    Options{Force: true, KeepTokens: 10, Trigger: "manual"},
		Summarizer: FileSummarizer{Path: writeSummary(t, "the user asked about the retry policy")},
		Now:        time.Now,
		NewID:      NewUUID,
	}
	result, err := compactor.Compact(context.Background(), sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if result.SummaryUsage != nil {
		t.Fatalf("summary usage = %+v, want nil", result.SummaryUsage)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "summary_usage") {
		t.Fatalf("document carries summary_usage without a provider: %s", raw)
	}
}
