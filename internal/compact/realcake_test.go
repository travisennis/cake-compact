//go:build !windows

package compact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/travisennis/cake-compact/internal/cakecli"
	"github.com/travisennis/cake-compact/internal/session"
)

// realCakeBinary returns the Cake build under test, or skips.
func realCakeBinary(t *testing.T) string {
	t.Helper()
	binary := os.Getenv("CAKE_BIN")
	if binary == "" {
		t.Skip("set CAKE_BIN to a cake build with 'sessions checkpoint' to run real-cake tests")
	}
	if _, err := os.Stat(binary); err != nil {
		t.Fatalf("CAKE_BIN=%s: %v", binary, err)
	}
	return binary
}

func replayRecords(t *testing.T, binary, dataDir, sessionID string) []string {
	t.Helper()
	cmd := exec.Command(binary, "--output-format", "stream-json", "replay", sessionID)
	cmd.Env = append(os.Environ(), "CAKE_DATA_DIR="+dataDir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("cake replay: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func countType(lines []string, recordType string) int {
	count := 0
	for _, line := range lines {
		var envelope struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(line), &envelope) == nil && envelope.Type == recordType {
			count++
		}
	}
	return count
}

func TestRealCakeInPlaceRoundTrip(t *testing.T) {
	binary := realCakeBinary(t)
	dataDir := t.TempDir()
	sourcePath := writeFixture(t, dataDir)
	original := readFile(t, sourcePath)

	before, err := session.Load(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	boundary := before.ConversationItemCount()

	compactor := inPlaceCompactor(t, dataDir, binary, "The user asked about the retry policy and then changed the timeout to 60 seconds.")
	result, err := compactor.Compact(context.Background(), sourcePath)
	if err != nil {
		t.Fatalf("in-place compaction: %v", err)
	}
	if !result.Compacted || !result.InPlace {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.SessionID != sourceID || result.SourceSessionID != sourceID {
		t.Fatalf("session id changed: %+v", result)
	}
	if result.CheckpointID == "" || result.Boundary != boundary {
		t.Fatalf("unexpected checkpoint identity: %+v (boundary %d)", result, boundary)
	}
	if result.BeforeTokens != 90000 || result.AfterTokens >= result.BeforeTokens {
		t.Fatalf("unexpected estimates: %+v", result)
	}

	appended := readFile(t, sourcePath)
	if !bytes.HasPrefix(appended, original) {
		t.Fatal("prior session bytes changed")
	}

	lines := replayRecords(t, binary, dataDir, sourceID)
	if got := countType(lines, "compaction"); got != 1 {
		t.Fatalf("replay emitted %d compaction records, want 1", got)
	}
	if got := countType(lines, "message"); got != 5 {
		t.Fatalf("replay emitted %d raw messages, want the 5 stored messages", got)
	}
	if got := countType(lines, "function_call"); got != 1 {
		t.Fatalf("replay emitted %d function calls, want 1", got)
	}
	if got := countType(lines, "function_call_output"); got != 1 {
		t.Fatalf("replay emitted %d function outputs, want 1", got)
	}
	replayed := strings.Join(lines, "\n")
	if !strings.Contains(replayed, "Please read the config and explain the retry policy.") {
		t.Fatal("replay did not include the retired raw prompt")
	}

	// A later turn appends new history and a new measurement; a second
	// checkpoint must append again and become the active one.
	beforeSecond := appendTurn(t, sourcePath, 60000)
	second := inPlaceCompactor(t, dataDir, binary, "Second summary: the timeout changed and the docs were updated.")
	secondResult, err := second.Compact(context.Background(), sourcePath)
	if err != nil {
		t.Fatalf("second in-place compaction: %v", err)
	}
	if !secondResult.Compacted || secondResult.SessionID != sourceID {
		t.Fatalf("unexpected second result: %+v", secondResult)
	}
	if !bytes.HasPrefix(readFile(t, sourcePath), beforeSecond) {
		t.Fatal("prior bytes changed during the second append")
	}

	file, err := session.Load(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if file.SessionID() != sourceID {
		t.Fatalf("session id = %s", file.SessionID())
	}
	latest := file.Records[file.LatestCheckpointIndex()].Checkpoint
	if latest == nil || !strings.Contains(latest.Summary, "Second summary") {
		t.Fatalf("newest checkpoint was not selected: %+v", latest)
	}
	lines = replayRecords(t, binary, dataDir, sourceID)
	if got := countType(lines, "compaction"); got != 2 {
		t.Fatalf("replay emitted %d compaction records, want 2", got)
	}
	if got := countType(lines, "message"); got != 7 {
		t.Fatalf("replay emitted %d raw messages, want the 7 stored messages", got)
	}
}

func TestRealCakeInPlacePinsTheOwningRequest(t *testing.T) {
	binary := realCakeBinary(t)
	dataDir := t.TempDir()
	sourcePath := writeSingleRequestFixture(t, dataDir)
	original := readFile(t, sourcePath)

	compactor := &Compactor{
		Mode:    ModeInPlace,
		DataDir: dataDir,
		CakeBin: binary,
		Options: Options{
			ThresholdTokens: 60000,
			KeepTokens:      10000,
			SummaryFile:     writeSummary(t, singleRequestSummary),
			Trigger:         "threshold",
		},
		Now:   time.Now,
		NewID: NewUUID,
	}
	result, err := compactor.Compact(context.Background(), sourcePath)
	if err != nil {
		t.Fatalf("in-place compaction: %v", err)
	}
	if !result.Compacted || result.SessionID != sourceID {
		t.Fatalf("unexpected result: %+v", result)
	}
	if !bytes.HasPrefix(readFile(t, sourcePath), original) {
		t.Fatal("prior session bytes changed")
	}

	file, err := session.Load(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := file.Records[file.LatestCheckpointIndex()].Checkpoint
	if checkpoint == nil || len(checkpoint.Retained) == 0 {
		t.Fatalf("missing checkpoint: %+v", checkpoint)
	}

	var head struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content string `json:"content"`
		Pin     *struct {
			Kind string `json:"kind"`
		} `json:"pin"`
	}
	if err := json.Unmarshal(checkpoint.Retained[0], &head); err != nil {
		t.Fatal(err)
	}
	if head.Type != "message" || head.Role != "user" || head.Content != singleRequestText {
		t.Fatalf("retained head changed the request: %+v", head)
	}
	if head.Pin == nil || head.Pin.Kind != "user_input" {
		t.Fatalf("real cake did not retain the user_input pin: %+v", head.Pin)
	}

	projected, err := file.Projected()
	if err != nil {
		t.Fatal(err)
	}
	if len(projected) < 2 || !projected[0].Synthetic || projected[1].Message == nil || projected[1].Message.Content != singleRequestText {
		t.Fatalf("projection does not preserve the request: %+v", projected)
	}
	if got := session.EstimateTokens(projected); got >= 80000 {
		t.Fatalf("projected context estimate = %d, want below the 80000 quality target", got)
	}

	lines := replayRecords(t, binary, dataDir, sourceID)
	if got := countType(lines, "compaction"); got != 1 {
		t.Fatalf("replay emitted %d compaction records, want 1", got)
	}
}

func TestRealCakeRejectsInvalidPayloadWithoutWriting(t *testing.T) {
	binary := realCakeBinary(t)
	dataDir := t.TempDir()
	sourcePath := writeFixture(t, dataDir)
	original := readFile(t, sourcePath)

	payload := cakecli.CheckpointPayload{
		Trigger:  "manual",
		Summary:  "does not shrink",
		Retained: []json.RawMessage{},
		TaskID:   taskID,
		EstimatedInput: cakecli.CheckpointEstimate{
			BeforeTokens: 100,
			AfterTokens:  100,
			Basis:        "content_class",
		},
		SummaryInstructionVersion: 1,
	}
	_, err := cakecli.Checkpoint(context.Background(), binary, dataDir, sourceID, payload)
	if err == nil {
		t.Fatal("real cake must reject a non-reducing payload")
	}
	var failure *cakecli.CheckpointFailure
	if !errors.As(err, &failure) {
		t.Fatalf("unexpected error type: %T %v", err, err)
	}
	if failure.Class != "input" || failure.ExitCode != 3 {
		t.Fatalf("unexpected failure: %+v", failure)
	}
	if after := readFile(t, sourcePath); !bytes.Equal(after, original) {
		t.Fatal("a rejected payload modified the session")
	}
}

func TestRealCakeLockedSessionFailsWithoutWriting(t *testing.T) {
	binary := realCakeBinary(t)
	dataDir := t.TempDir()
	sourcePath := writeFixture(t, dataDir)
	original := readFile(t, sourcePath)

	file, err := session.Load(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	plan, decision, err := Analyze(file, Options{Force: true, KeepTokens: 10, Trigger: "manual"})
	if err != nil || !decision.Compact {
		t.Fatalf("analyze: %v %+v", err, decision)
	}
	payload, err := BuildCheckpointPayload(plan, "a summary", file)
	if err != nil {
		t.Fatal(err)
	}

	handle, err := os.OpenFile(sourcePath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if err := syscall.Flock(int(handle.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Skipf("cannot take the exclusive session lock: %v", err)
	}
	defer syscall.Flock(int(handle.Fd()), syscall.LOCK_UN)

	_, err = cakecli.Checkpoint(context.Background(), binary, dataDir, sourceID, payload)
	if err == nil {
		t.Fatal("a locked session must fail")
	}
	var failure *cakecli.CheckpointFailure
	if !errors.As(err, &failure) {
		t.Fatalf("unexpected error type: %T %v", err, err)
	}
	if failure.Class != "locked" {
		t.Fatalf("failure class = %q (%+v), want locked", failure.Class, failure)
	}
	if after := readFile(t, sourcePath); !bytes.Equal(after, original) {
		t.Fatal("a locked append modified the session")
	}
}
