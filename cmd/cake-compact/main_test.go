//go:build !windows

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/travisennis/cake-compact/internal/cakecli"
	"github.com/travisennis/cake-compact/internal/compact"
	"github.com/travisennis/cake-compact/internal/session"
	"github.com/travisennis/cake-compact/internal/state"
)

const (
	testSourceID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	testTaskID   = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

func testFixture() string {
	return strings.Join([]string{
		`{"type":"session_meta","format_version":4,"session_id":"` + testSourceID + `","timestamp":"2026-10-23T12:00:00Z","working_directory":"/tmp","tools":[],"model":"test-model"}`,
		`{"type":"task_start","session_id":"` + testSourceID + `","task_id":"` + testTaskID + `","timestamp":"2026-10-23T12:00:01Z"}`,
		`{"type":"message","role":"user","content":"first request"}`,
		`{"type":"message","role":"assistant","content":"first answer"}`,
		`{"type":"message","role":"user","content":"second request"}`,
		`{"type":"message","role":"assistant","content":"second answer"}`,
		`{"type":"turn_usage","session_id":"` + testSourceID + `","task_id":"` + testTaskID + `","turn":2,"usage":{"input_tokens":90000,"input_tokens_details":{"cached_tokens":0},"output_tokens":10,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":90010},"timestamp":"2026-10-23T12:00:10Z","usage_presence":"complete","context_window":100000,"remaining_context_tokens":10000}`,
	}, "\n") + "\n"
}

func captureOutput(t *testing.T, run func() int) (int, string, string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errRead, errWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = outWrite, errWrite

	type captured struct{ stdout, stderr string }
	done := make(chan captured, 1)
	go func() {
		out, _ := io.ReadAll(outRead)
		errOut, _ := io.ReadAll(errRead)
		done <- captured{string(out), string(errOut)}
	}()

	code := run()

	_ = outWrite.Close()
	_ = errWrite.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	result := <-done
	_ = outRead.Close()
	_ = errRead.Close()
	return code, result.stdout, result.stderr
}

func writeSessionFixture(t *testing.T) (string, string) {
	t.Helper()
	dataDir := t.TempDir()
	sessions := filepath.Join(dataDir, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(sessions, testSourceID+".jsonl")
	if err := os.WriteFile(sourcePath, []byte(testFixture()), 0o600); err != nil {
		t.Fatal(err)
	}
	return dataDir, sourcePath
}

func writeSummaryFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "summary.txt")
	if err := os.WriteFile(path, []byte("The user made two requests; both were answered."), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunCompactsInPlaceAndKeepsTheSession(t *testing.T) {
	dataDir, sourcePath := writeSessionFixture(t)
	original, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	summaryPath := writeSummaryFile(t)
	statePath := filepath.Join(t.TempDir(), "state.json")

	code, stdout, stderr := captureOutput(t, func() int {
		return runCLI([]string{
			"run",
			"-cake-bin", writeFakeCake(t),
			"-data-dir", dataDir,
			"-state", statePath,
			"-summary-file", summaryPath,
			"-keep-tokens", "1",
			"-force-compact",
			"-json",
			"hello",
		})
	})
	if code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, stderr)
	}

	var output runOutput
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("invalid output JSON: %v\n%s", err, stdout)
	}
	if output.SessionID != testSourceID || output.NextSessionID != testSourceID {
		t.Fatalf("in-place run changed the session id: %+v", output)
	}
	if output.Result == nil || *output.Result != "done" {
		t.Fatalf("unexpected run result: %+v", output)
	}
	compaction := output.Compaction
	if compaction == nil || !compaction.Compacted || !compaction.InPlace {
		t.Fatalf("expected an in-place compaction: %+v", output.Compaction)
	}
	if compaction.SessionID != testSourceID || compaction.SourceSessionID != testSourceID {
		t.Fatalf("compaction changed the session id: %+v", compaction)
	}
	if compaction.CheckpointID == "" || compaction.Boundary != 4 || compaction.RetainedCount != 2 {
		t.Fatalf("unexpected checkpoint identity: %+v", compaction)
	}
	if compaction.BeforeTokens != 90000 || compaction.AfterTokens >= compaction.BeforeTokens {
		t.Fatalf("unexpected estimates: %+v", compaction)
	}
	if compaction.SessionFile != sourcePath {
		t.Fatalf("session file = %s, want %s", compaction.SessionFile, sourcePath)
	}

	saved, err := state.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if saved == nil || saved.SessionID != testSourceID {
		t.Fatalf("state moved for an in-place compaction: %+v", saved)
	}

	appended, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(appended, original) {
		t.Fatal("the source session lost its prior bytes")
	}
	if len(appended) <= len(original) {
		t.Fatal("no checkpoint was appended")
	}

	entries, err := os.ReadDir(filepath.Join(dataDir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("in-place compaction wrote %d session files, want 1", len(entries))
	}
}

func TestRunForkEscapeHatchWritesANewSession(t *testing.T) {
	dataDir, sourcePath := writeSessionFixture(t)
	original, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	summaryPath := writeSummaryFile(t)
	statePath := filepath.Join(t.TempDir(), "state.json")

	code, stdout, stderr := captureOutput(t, func() int {
		return runCLI([]string{
			"run",
			"-cake-bin", writeFakeCake(t),
			"-data-dir", dataDir,
			"-state", statePath,
			"-summary-file", summaryPath,
			"-keep-tokens", "1",
			"-force-compact",
			"-fork",
			"-no-verify",
			"-json",
			"hello",
		})
	})
	if code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, stderr)
	}

	var output runOutput
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("invalid output JSON: %v\n%s", err, stdout)
	}
	if output.Compaction == nil || !output.Compaction.Compacted || output.Compaction.InPlace {
		t.Fatalf("expected a fork compaction: %+v", output.Compaction)
	}
	if output.Compaction.SessionID == testSourceID || output.NextSessionID != output.Compaction.SessionID {
		t.Fatalf("fork did not change the session id: %+v", output)
	}

	saved, err := state.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if saved == nil || saved.SessionID != output.Compaction.SessionID {
		t.Fatalf("state did not move to the forked session: %+v", saved)
	}

	current, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current, original) {
		t.Fatal("the fork path modified the source session")
	}

	newPath := filepath.Join(dataDir, "sessions", output.Compaction.SessionID+".jsonl")
	compacted, err := session.Load(newPath)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := compacted.Records[1].Checkpoint
	if checkpoint == nil || checkpoint.SessionID != output.Compaction.SessionID {
		t.Fatalf("unexpected checkpoint: %+v", checkpoint)
	}
}

func TestRunNoCompactKeepsTheSourceSession(t *testing.T) {
	dataDir, _ := writeSessionFixture(t)

	code, _, stderr := captureOutput(t, func() int {
		return runCLI([]string{
			"run",
			"-cake-bin", writeFakeCake(t),
			"-data-dir", dataDir,
			"-no-state",
			"-no-compact",
			"-json",
			"hello",
		})
	})
	if code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, stderr)
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected only the source session, got %d files", len(entries))
	}
}

func TestCompactInPlaceJSONShape(t *testing.T) {
	dataDir, sourcePath := writeSessionFixture(t)

	code, stdout, stderr := captureOutput(t, func() int {
		return runCLI([]string{
			"compact",
			"-cake-bin", writeFakeCake(t),
			"-data-dir", dataDir,
			"-summary-file", writeSummaryFile(t),
			"-keep-tokens", "1",
			"-json",
			testSourceID,
		})
	})
	if code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, stderr)
	}

	var document map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &document); err != nil {
		t.Fatalf("invalid output JSON: %v\n%s", err, stdout)
	}
	required := []string{
		"compacted", "source_session_id", "session_id", "in_place", "checkpoint_id",
		"boundary", "trigger", "basis", "before_tokens", "after_tokens", "window",
		"retained_items", "retained_count", "session_file",
	}
	for _, key := range required {
		if _, ok := document[key]; !ok {
			t.Fatalf("result is missing %q:\n%s", key, stdout)
		}
	}

	var inPlace bool
	if err := json.Unmarshal(document["in_place"], &inPlace); err != nil || !inPlace {
		t.Fatalf("in_place = %s", document["in_place"])
	}
	var sessionID string
	if err := json.Unmarshal(document["session_id"], &sessionID); err != nil || sessionID != testSourceID {
		t.Fatalf("session_id = %s", document["session_id"])
	}
	var sessionFile string
	if err := json.Unmarshal(document["session_file"], &sessionFile); err != nil || sessionFile != sourcePath {
		t.Fatalf("session_file = %s", document["session_file"])
	}
}

func TestCompactResolvesCakeBinFromTheEnvironment(t *testing.T) {
	dataDir, sourcePath := writeSessionFixture(t)
	t.Setenv("CAKE_BIN", writeFakeCake(t))

	code, stdout, stderr := captureOutput(t, func() int {
		return runCLI([]string{
			"compact",
			"-data-dir", dataDir,
			"-summary-file", writeSummaryFile(t),
			"-keep-tokens", "1",
			"-json",
			testSourceID,
		})
	})
	if code != 0 {
		t.Fatalf("exit code = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &document); err != nil {
		t.Fatalf("invalid output JSON: %v\n%s", err, stdout)
	}
	var sessionFile string
	if err := json.Unmarshal(document["session_file"], &sessionFile); err != nil || sessionFile != sourcePath {
		t.Fatalf("session_file = %s", document["session_file"])
	}
}

func TestCompactMissingVerbFailsWithoutWriting(t *testing.T) {
	dataDir, sourcePath := writeSessionFixture(t)
	original, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	oldCake := filepath.Join(t.TempDir(), "old-cake")
	if err := os.WriteFile(oldCake, []byte("#!/bin/sh\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := captureOutput(t, func() int {
		return runCLI([]string{
			"compact",
			"-cake-bin", oldCake,
			"-data-dir", dataDir,
			"-summary-file", writeSummaryFile(t),
			"-keep-tokens", "1",
			"-json",
			testSourceID,
		})
	})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "cake sessions checkpoint is unavailable; upgrade cake") {
		t.Fatalf("unclear failure: %s", stderr)
	}
	current, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current, original) {
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

func TestRunAppendFailureReportsCompactionError(t *testing.T) {
	dataDir, sourcePath := writeSessionFixture(t)
	original, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "fake-cake")
	body := fmt.Sprintf(`#!/bin/sh
case "$*" in
  *"sessions checkpoint --help"*) echo "Append a checkpoint"; exit 0 ;;
  *sessions*) echo "Another cake invocation is currently writing to session %s." >&2; exit 1 ;;
esac
printf '{"session_id":"%s","result":"done"}'
`, testSourceID, testSourceID)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := captureOutput(t, func() int {
		return runCLI([]string{
			"run",
			"-cake-bin", script,
			"-data-dir", dataDir,
			"-no-state",
			"-summary-file", writeSummaryFile(t),
			"-keep-tokens", "1",
			"-force-compact",
			"-json",
			"hello",
		})
	})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr: %s", code, stderr)
	}
	var output runOutput
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("invalid output JSON: %v\n%s", err, stdout)
	}
	if output.Result == nil || *output.Result != "done" || output.SessionID != testSourceID {
		t.Fatalf("the turn result was not preserved: %+v", output)
	}
	if output.NextSessionID != testSourceID || output.Compaction != nil {
		t.Fatalf("unexpected output: %+v", output)
	}
	if !strings.Contains(output.CompactionError, "locked") {
		t.Fatalf("compaction_error = %q", output.CompactionError)
	}

	current, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current, original) {
		t.Fatal("a locked append modified the session")
	}
}

func TestRunMissingVerbFailsWithoutFallback(t *testing.T) {
	dataDir, sourcePath := writeSessionFixture(t)
	original, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "turn-ran")
	t.Setenv("CAKE_COMPACT_TURN_MARKER", marker)
	script := filepath.Join(t.TempDir(), "fake-cake")
	body := fmt.Sprintf(`#!/bin/sh
case "$*" in
  *sessions*) exit 2 ;;
esac
: > "$CAKE_COMPACT_TURN_MARKER"
printf '{"session_id":"%s","result":"done"}'
`, testSourceID)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := captureOutput(t, func() int {
		return runCLI([]string{
			"run",
			"-cake-bin", script,
			"-data-dir", dataDir,
			"-no-state",
			"-summary-file", writeSummaryFile(t),
			"-keep-tokens", "1",
			"-force-compact",
			"-json",
			"hello",
		})
	})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "cake sessions checkpoint is unavailable; upgrade cake") {
		t.Fatalf("unclear failure: %s", stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("unexpected stdout: %s", stdout)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("the turn ran before the missing verb was reported")
	}

	current, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current, original) {
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

// The human surface names the summary's cost when the summarizer reported one;
// the JSON document carries the same numbers for Phase 5.
func TestPrintCompactionResultShowsSummaryUsage(t *testing.T) {
	result := &compact.Result{
		Compacted:     true,
		InPlace:       true,
		SessionID:     testSourceID,
		BeforeTokens:  90000,
		AfterTokens:   42,
		Basis:         "measured_usage",
		RetainedItems: 2,
		KeepTarget:    10,
		CheckpointID:  "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
		Boundary:      7,
		SessionFile:   "/tmp/session.jsonl",
		SummaryUsage:  &cakecli.Usage{InputTokens: 1200, OutputTokens: 300, TotalTokens: 1500},
		SummaryTimeMs: 8420,
	}
	code, stdout, _ := captureOutput(t, func() int { return printCompactionResult(result, false) })
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	for _, want := range []string{
		"input: 90000 -> 42 estimated tokens (measured_usage)",
		"summary: 1200 in / 300 out tokens in 8.42s",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

// A summary-file run has no provider usage, so the human output omits the cost
// line rather than printing a zero.
func TestPrintCompactionResultOmitsUsageWithoutAProvider(t *testing.T) {
	result := &compact.Result{
		Compacted:    true,
		InPlace:      true,
		SessionID:    testSourceID,
		BeforeTokens: 90000,
		AfterTokens:  42,
		Basis:        "measured_usage",
		SessionFile:  "/tmp/session.jsonl",
	}
	code, stdout, _ := captureOutput(t, func() int { return printCompactionResult(result, false) })
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if strings.Contains(stdout, "summary:") {
		t.Fatalf("stdout names a summary cost without usage:\n%s", stdout)
	}
}
