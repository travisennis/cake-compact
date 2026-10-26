package cakecli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func samplePayload() CheckpointPayload {
	return CheckpointPayload{
		Trigger:                   "manual",
		Summary:                   "The prefix, summarized.",
		Retained:                  []json.RawMessage{json.RawMessage(`{"type":"message","role":"user","content":"the tail"}`)},
		EstimatedInput:            CheckpointEstimate{BeforeTokens: 12000, AfterTokens: 3000, Basis: "measured_usage"},
		SummaryInstructionVersion: 1,
		Model:                     &CheckpointModel{Model: "test-model", ModelConfig: "test"},
		TaskID:                    "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
	}
}

func TestCheckpointAvailable(t *testing.T) {
	script := writeScript(t, "printf 'Append one checkpoint\\n'\n")
	if err := CheckpointAvailable(context.Background(), script); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	old := writeScript(t, "exit 2\n")
	err := CheckpointAvailable(context.Background(), old)
	if err == nil {
		t.Fatal("a binary without the verb must fail")
	}
	if !errors.Is(err, ErrCheckpointUnavailable) {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), "cake sessions checkpoint is unavailable; upgrade cake") {
		t.Fatalf("unclear error: %v", err)
	}
}

func TestCheckpointAvailableDoesNotReportCancellationAsUnavailable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := CheckpointAvailable(ctx, writeScript(t, "exit 0\n"))
	if err == nil || errors.Is(err, ErrCheckpointUnavailable) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckpointSendsTheProtocolPayload(t *testing.T) {
	payloadPath := filepath.Join(t.TempDir(), "payload.json")
	t.Setenv("CAKE_COMPACT_PAYLOAD_OUT", payloadPath)
	script := writeScript(t, `cat > "$CAKE_COMPACT_PAYLOAD_OUT"
printf '%s' '{"schema_version":1,"command":"sessions checkpoint","status":"ok","summary":{"session_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","checkpoint_id":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","boundary":7,"before_tokens":12000,"after_tokens":3000,"retained_count":1},"checks":[],"data":{"session_file":"/tmp/sessions/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa.jsonl","basis":"measured_usage"}}'
`)

	payload := samplePayload()
	outcome, err := Checkpoint(context.Background(), script, t.TempDir(), "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", payload)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.SessionID != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" ||
		outcome.CheckpointID != "cccccccc-cccc-4ccc-8ccc-cccccccccccc" ||
		outcome.Boundary != 7 ||
		outcome.BeforeTokens != 12000 ||
		outcome.AfterTokens != 3000 ||
		outcome.RetainedCount != 1 ||
		outcome.Basis != "measured_usage" ||
		outcome.SessionFile == "" {
		t.Fatalf("unexpected outcome: %+v", outcome)
	}

	data, err := os.ReadFile(payloadPath)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	expected = append(expected, '\n')
	if string(data) != string(expected) {
		t.Fatalf("stdin payload mismatch:\n%s\nwant:\n%s", data, expected)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"type", "checkpoint_version", "checkpoint_id", "session_id", "timestamp", "boundary"} {
		if _, ok := decoded[forbidden]; ok {
			t.Fatalf("payload supplies the core-owned field %q", forbidden)
		}
	}
}

func TestCheckpointSurfacesFailureClass(t *testing.T) {
	inputFailure := writeScript(t, `printf '%s' '{"schema_version":1,"command":"sessions checkpoint","status":"error","summary":{},"checks":[{"id":"sessions.checkpoint_failed","status":"error","message":"checkpoint payload was rejected: no reduction"}],"data":{}}'
printf 'checkpoint payload was rejected: no reduction\n' >&2
exit 3
`)
	_, err := Checkpoint(context.Background(), inputFailure, t.TempDir(), "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", samplePayload())
	var failure *CheckpointFailure
	if !errors.As(err, &failure) {
		t.Fatalf("unexpected error type: %T %v", err, err)
	}
	if failure.Class != "input" || failure.ExitCode != 3 {
		t.Fatalf("unexpected failure: %+v", failure)
	}
	if !strings.Contains(failure.Error(), "exit 3, input") || !strings.Contains(failure.Error(), "no reduction") {
		t.Fatalf("unclear failure: %v", failure)
	}
	if len(failure.Checks) != 1 || failure.Checks[0].ID != "sessions.checkpoint_failed" {
		t.Fatalf("failure document was not surfaced: %+v", failure.Checks)
	}

	locked := writeScript(t, `printf 'Another cake invocation is currently writing to session aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa.\n' >&2
exit 1
`)
	_, err = Checkpoint(context.Background(), locked, t.TempDir(), "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", samplePayload())
	if !errors.As(err, &failure) {
		t.Fatalf("unexpected error type: %T %v", err, err)
	}
	if failure.Class != "locked" || failure.ExitCode != 1 {
		t.Fatalf("unexpected failure: %+v", failure)
	}
}

func TestCheckpointRejectsAMismatchedSession(t *testing.T) {
	script := writeScript(t, `cat >/dev/null
printf '%s' '{"schema_version":1,"command":"sessions checkpoint","status":"ok","summary":{"session_id":"ffffffff-ffff-4fff-8fff-ffffffffffff","checkpoint_id":"c","boundary":1,"before_tokens":10,"after_tokens":5,"retained_count":0},"checks":[],"data":{"session_file":"/tmp/other.jsonl","basis":"content_class"}}'
`)
	_, err := Checkpoint(context.Background(), script, t.TempDir(), "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", samplePayload())
	if err == nil || !strings.Contains(err.Error(), "not aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa") {
		t.Fatalf("unexpected error: %v", err)
	}
}
