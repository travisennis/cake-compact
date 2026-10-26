//go:build !windows

package compact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/travisennis/cake-compact/internal/cakecli"
	"github.com/travisennis/cake-compact/internal/session"
)

func inPlaceCompactor(t *testing.T, dataDir, binary, summary string) *Compactor {
	t.Helper()
	return &Compactor{
		Mode:    ModeInPlace,
		DataDir: dataDir,
		CakeBin: binary,
		Options: Options{Force: true, KeepTokens: 10, SummaryFile: writeSummary(t, summary), Trigger: "manual"},
		Now:     time.Now,
		NewID:   NewUUID,
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestInPlaceAppendKeepsPriorBytesAndSessionID(t *testing.T) {
	dataDir := t.TempDir()
	sourcePath := writeFixture(t, dataDir)
	original := readFile(t, sourcePath)
	originalFile, err := session.Load(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	originalBoundary := originalFile.ConversationItemCount()

	compactor := inPlaceCompactor(t, dataDir, writeFakeCake(t), "The user asked about the retry policy and changed the timeout.")
	result, err := compactor.Compact(context.Background(), sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Compacted || !result.InPlace {
		t.Fatalf("expected an in-place compaction: %+v", result)
	}
	if result.SessionID != sourceID || result.SourceSessionID != sourceID {
		t.Fatalf("session id changed: %+v", result)
	}
	if result.CheckpointID == "" || result.Boundary != originalBoundary {
		t.Fatalf("unexpected checkpoint identity: %+v", result)
	}
	if result.BeforeTokens != 90000 {
		t.Fatalf("before_tokens = %d, want the measured 90000", result.BeforeTokens)
	}
	if result.AfterTokens >= result.BeforeTokens {
		t.Fatalf("after %d is not below before %d", result.AfterTokens, result.BeforeTokens)
	}

	appended := readFile(t, sourcePath)
	if !bytes.HasPrefix(appended, original) {
		t.Fatal("prior session bytes changed")
	}
	if len(appended) <= len(original) {
		t.Fatal("no checkpoint was appended")
	}

	file, err := session.Load(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if file.SessionID() != sourceID {
		t.Fatalf("session id = %s", file.SessionID())
	}
	if file.LatestCheckpointIndex() != len(file.Records)-1 {
		t.Fatalf("newest record is not the checkpoint: %d of %d", file.LatestCheckpointIndex(), len(file.Records))
	}
	checkpoint := file.Records[file.LatestCheckpointIndex()].Checkpoint
	if checkpoint == nil || !strings.Contains(checkpoint.Summary, "retry policy") {
		t.Fatalf("unexpected checkpoint: %+v", checkpoint)
	}
	if len(checkpoint.Retained) != result.RetainedCount || result.RetainedCount == 0 {
		t.Fatalf("retained_count = %d, checkpoint retained %d", result.RetainedCount, len(checkpoint.Retained))
	}
	if checkpoint.EstimatedInput.BeforeTokens != result.BeforeTokens || checkpoint.EstimatedInput.AfterTokens != result.AfterTokens {
		t.Fatalf("estimates disagree: %+v vs result %+v", checkpoint.EstimatedInput, result)
	}
}

func TestInPlaceSecondCheckpointSelectsTheNewest(t *testing.T) {
	dataDir := t.TempDir()
	sourcePath := writeFixture(t, dataDir)
	binary := writeFakeCake(t)

	first := inPlaceCompactor(t, dataDir, binary, "first summary about the retry policy")
	firstResult, err := first.Compact(context.Background(), sourcePath)
	if err != nil || !firstResult.Compacted {
		t.Fatalf("first compaction: %v %+v", err, firstResult)
	}

	// Simulate a later Cake turn: new conversation and a new measurement.
	beforeSecond := appendTurn(t, sourcePath, 60000)

	second := inPlaceCompactor(t, dataDir, binary, "second summary about the timeout")
	secondResult, err := second.Compact(context.Background(), sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !secondResult.Compacted {
		t.Fatalf("second compaction: %s", secondResult.Reason)
	}
	if secondResult.SessionID != sourceID {
		t.Fatalf("session id changed: %+v", secondResult)
	}

	current := readFile(t, sourcePath)
	if !bytes.HasPrefix(current, beforeSecond) {
		t.Fatal("prior bytes changed during the second append")
	}

	file, err := session.Load(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	compactions := 0
	for _, record := range file.Records {
		if record.Checkpoint != nil {
			compactions++
		}
	}
	if compactions != 2 {
		t.Fatalf("compaction records = %d, want 2", compactions)
	}
	latest := file.Records[file.LatestCheckpointIndex()].Checkpoint
	if latest == nil || latest.Summary != "second summary about the timeout" {
		t.Fatalf("newest checkpoint was not selected: %+v", latest)
	}
	projected, err := file.Projected()
	if err != nil {
		t.Fatal(err)
	}
	if len(projected) == 0 || !projected[0].Synthetic || !strings.Contains(projected[0].Message.Content, "second summary") {
		t.Fatalf("projection does not start with the newest summary: %+v", projected)
	}
}

// appendTurn adds one exchange and a measured usage record, returning the file
// bytes as they were before the append.
func appendTurn(t *testing.T, path string, inputTokens uint64) []byte {
	t.Helper()
	before := readFile(t, path)
	lines := strings.Join([]string{
		`{"type":"message","role":"user","content":"Now also update the docs."}`,
		`{"type":"message","role":"assistant","content":"The docs are updated."}`,
		`{"type":"turn_usage","session_id":"` + sourceID + `","task_id":"` + taskID + `","turn":3,"usage":{"input_tokens":` + jsonNumber(inputTokens) + `,"input_tokens_details":{"cached_tokens":0},"output_tokens":100,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":` + jsonNumber(inputTokens+100) + `},"timestamp":"2026-10-23T12:00:20Z","usage_presence":"complete","context_window":100000,"remaining_context_tokens":10000}`,
	}, "\n") + "\n"
	handle, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.WriteString(lines); err != nil {
		_ = handle.Close()
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	return before
}

func jsonNumber(value uint64) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestInPlaceRejectedPayloadLeavesTheFileByteStable(t *testing.T) {
	dataDir := t.TempDir()
	sourcePath := writeFixture(t, dataDir)
	original := readFile(t, sourcePath)
	binary := writeFakeCake(t)

	payload := cakecli.CheckpointPayload{
		Trigger:  "manual",
		Summary:  "a summary that claims a larger result",
		Retained: []json.RawMessage{},
		TaskID:   taskID,
		EstimatedInput: cakecli.CheckpointEstimate{
			BeforeTokens: 100,
			AfterTokens:  200,
			Basis:        "content_class",
		},
		SummaryInstructionVersion: 1,
	}
	_, err := cakecli.Checkpoint(context.Background(), binary, dataDir, sourceID, payload)
	if err == nil {
		t.Fatal("an invalid payload must be rejected")
	}
	var failure *cakecli.CheckpointFailure
	if !errors.As(err, &failure) {
		t.Fatalf("unexpected error type: %T %v", err, err)
	}
	if failure.Class != "input" || failure.ExitCode != 3 {
		t.Fatalf("unexpected failure class: %+v", failure)
	}
	if after := readFile(t, sourcePath); !bytes.Equal(after, original) {
		t.Fatal("a rejected payload modified the session")
	}
}

func TestInPlaceLockedSessionFailsWithoutWriting(t *testing.T) {
	dataDir := t.TempDir()
	sourcePath := writeFixture(t, dataDir)
	original := readFile(t, sourcePath)
	t.Setenv(fakeCakeMode, "locked")

	compactor := inPlaceCompactor(t, dataDir, writeFakeCake(t), "a summary")
	result, err := compactor.Compact(context.Background(), sourcePath)
	if err == nil || result != nil {
		t.Fatalf("a locked session must fail: %+v %v", result, err)
	}
	var failure *cakecli.CheckpointFailure
	if !errors.As(err, &failure) {
		t.Fatalf("unexpected error type: %T %v", err, err)
	}
	if failure.Class != "locked" {
		t.Fatalf("failure class = %q, want locked", failure.Class)
	}
	if after := readFile(t, sourcePath); !bytes.Equal(after, original) {
		t.Fatal("a locked append modified the session")
	}
}

func TestInPlaceNonReductionDoesNotCallTheVerb(t *testing.T) {
	dataDir := t.TempDir()
	sourcePath := writeFixture(t, dataDir)
	original := readFile(t, sourcePath)
	payloadOut := filepath.Join(t.TempDir(), "payload.json")
	t.Setenv(fakePayloadOut, payloadOut)

	compactor := inPlaceCompactor(t, dataDir, writeFakeCake(t), strings.Repeat("verbose summary ", 40000))
	result, err := compactor.Compact(context.Background(), sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if result.Compacted {
		t.Fatal("a non-reducing summary must not commit")
	}
	if !strings.Contains(result.Reason, "did not reduce") {
		t.Fatalf("unexpected reason: %s", result.Reason)
	}
	if _, err := os.Stat(payloadOut); !os.IsNotExist(err) {
		t.Fatal("the verb was called for a non-reducing plan")
	}
	if after := readFile(t, sourcePath); !bytes.Equal(after, original) {
		t.Fatal("a rejected plan modified the session")
	}
}

func TestInPlaceMissingVerbFailsWithoutFallback(t *testing.T) {
	dataDir := t.TempDir()
	sourcePath := writeFixture(t, dataDir)
	original := readFile(t, sourcePath)
	payloadOut := filepath.Join(t.TempDir(), "payload.json")
	t.Setenv(fakePayloadOut, payloadOut)

	compactor := inPlaceCompactor(t, dataDir, writeBrokenCake(t), "a summary")
	_, err := compactor.Compact(context.Background(), sourcePath)
	if err == nil {
		t.Fatal("a binary without the verb must fail")
	}
	if !errors.Is(err, cakecli.ErrCheckpointUnavailable) {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), "cake sessions checkpoint is unavailable; upgrade cake") {
		t.Fatalf("unclear error: %v", err)
	}
	if _, err := os.Stat(payloadOut); !os.IsNotExist(err) {
		t.Fatal("the payload was written to a binary without the verb")
	}
	if after := readFile(t, sourcePath); !bytes.Equal(after, original) {
		t.Fatal("a missing verb modified the session")
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("a missing verb wrote a session: %d files", len(entries))
	}
}

func TestInPlacePayloadMatchesTheVerbContract(t *testing.T) {
	dataDir := t.TempDir()
	sourcePath := writeFixture(t, dataDir)
	payloadOut := filepath.Join(t.TempDir(), "payload.json")
	t.Setenv(fakePayloadOut, payloadOut)

	compactor := inPlaceCompactor(t, dataDir, writeFakeCake(t), "Summary of the retired prefix.")
	result, err := compactor.Compact(context.Background(), sourcePath)
	if err != nil || !result.Compacted {
		t.Fatalf("compaction: %v %+v", err, result)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(readFile(t, payloadOut), &payload); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"trigger": true, "summary": true, "retained": true, "estimated_input": true,
		"summary_instruction_version": true, "model": true, "task_id": true,
	}
	for key := range payload {
		if !allowed[key] {
			t.Fatalf("payload contains an unowned field %q", key)
		}
	}
	for _, coreOwned := range []string{"type", "checkpoint_version", "checkpoint_id", "session_id", "timestamp", "boundary"} {
		if _, ok := payload[coreOwned]; ok {
			t.Fatalf("payload supplies the core-owned field %q", coreOwned)
		}
	}
	for _, required := range []string{"trigger", "summary", "retained", "estimated_input", "summary_instruction_version", "model", "task_id"} {
		if _, ok := payload[required]; !ok {
			t.Fatalf("payload is missing %q", required)
		}
	}

	var trigger, summary, task string
	_ = json.Unmarshal(payload["trigger"], &trigger)
	_ = json.Unmarshal(payload["summary"], &summary)
	_ = json.Unmarshal(payload["task_id"], &task)
	if trigger != "manual" || summary != "Summary of the retired prefix." || task != taskID {
		t.Fatalf("unexpected payload identity: %q %q %q", trigger, summary, task)
	}

	var version int
	_ = json.Unmarshal(payload["summary_instruction_version"], &version)
	if version != SummaryInstructionVersion {
		t.Fatalf("summary_instruction_version = %d", version)
	}

	var model struct {
		Model       string `json:"model"`
		ModelConfig string `json:"model_config"`
	}
	if err := json.Unmarshal(payload["model"], &model); err != nil {
		t.Fatal(err)
	}
	if model.Model != "test-model" || model.ModelConfig != "test" {
		t.Fatalf("unexpected model identity: %+v", model)
	}

	var estimate struct {
		BeforeTokens uint64 `json:"before_tokens"`
		AfterTokens  uint64 `json:"after_tokens"`
		Basis        string `json:"basis"`
	}
	if err := json.Unmarshal(payload["estimated_input"], &estimate); err != nil {
		t.Fatal(err)
	}
	if estimate.BeforeTokens != 90000 || estimate.AfterTokens >= estimate.BeforeTokens || estimate.Basis != "measured_usage" {
		t.Fatalf("unexpected estimate: %+v", estimate)
	}

	var retained []map[string]json.RawMessage
	if err := json.Unmarshal(payload["retained"], &retained); err != nil {
		t.Fatal(err)
	}
	if len(retained) != result.RetainedCount || len(retained) == 0 {
		t.Fatalf("retained = %d, want %d", len(retained), result.RetainedCount)
	}
	for _, item := range retained {
		var kind string
		_ = json.Unmarshal(item["type"], &kind)
		switch kind {
		case "message", "reasoning", "function_call", "function_call_output":
		default:
			t.Fatalf("unexpected retained record kind %q", kind)
		}
	}
}

func TestInPlaceRequiresTheSessionToLiveInTheDataRoot(t *testing.T) {
	dataDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), sourceID+".jsonl")
	if err := os.WriteFile(outside, []byte(fixtureLines()), 0o600); err != nil {
		t.Fatal(err)
	}
	compactor := inPlaceCompactor(t, dataDir, writeFakeCake(t), "a summary")
	if _, err := compactor.Compact(context.Background(), outside); err == nil || !strings.Contains(err.Error(), "-fork") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRecoverCompletedAppendRecognizesAnInterruptedCommit(t *testing.T) {
	dataDir := t.TempDir()
	sourcePath := writeFixture(t, dataDir)
	binary := writeFakeCake(t)

	compactor := inPlaceCompactor(t, dataDir, binary, "the committed summary")
	result, err := compactor.Compact(context.Background(), sourcePath)
	if err != nil || !result.Compacted {
		t.Fatalf("compaction: %v %+v", err, result)
	}

	outcome, ok := recoverCompletedAppend(sourcePath, "the committed summary", 0)
	if !ok || outcome.CheckpointID != result.CheckpointID || outcome.SessionID != sourceID {
		t.Fatalf("a completed append was not recognized: %+v %v", outcome, ok)
	}
	if outcome.RetainedCount != result.RetainedCount || outcome.Boundary != result.Boundary {
		t.Fatalf("unexpected recovered outcome: %+v vs %+v", outcome, result)
	}
	if _, ok := recoverCompletedAppend(sourcePath, "a different summary", 0); ok {
		t.Fatal("a mismatched summary must not count as our append")
	}
	if _, ok := recoverCompletedAppend(sourcePath, "the committed summary", 1); ok {
		t.Fatal("a prior count at the current count must not count as a new append")
	}
}

func TestCanceledSummaryWritesNothing(t *testing.T) {
	dataDir := t.TempDir()
	sourcePath := writeFixture(t, dataDir)
	original := readFile(t, sourcePath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	compactor := &Compactor{
		Mode:       ModeInPlace,
		DataDir:    dataDir,
		CakeBin:    writeFakeCake(t),
		Options:    Options{Force: true, KeepTokens: 10, Trigger: "manual"},
		Summarizer: CommandSummarizer{Command: "sleep 30"},
		Now:        time.Now,
		NewID:      NewUUID,
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if _, err := compactor.Compact(ctx, sourcePath); err == nil {
		t.Fatal("a canceled summary must fail")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("cancellation took %s", elapsed)
	}
	if after := readFile(t, sourcePath); !bytes.Equal(after, original) {
		t.Fatal("a canceled run modified the session")
	}
}
