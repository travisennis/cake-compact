//go:build !windows

package compact

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/travisennis/cake-compact/internal/cakecli"
)

// delayedSummarizer gives the stage accounting a controlled provider delay
// without a model call.
type delayedSummarizer struct {
	delay time.Duration
	text  string
	err   error
}

func (s delayedSummarizer) Summarize(ctx context.Context, _ string) (string, cakecli.Usage, error) {
	if s.delay > 0 {
		timer := time.NewTimer(s.delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return "", cakecli.Usage{}, ctx.Err()
		}
	}
	if s.err != nil {
		return "", cakecli.Usage{}, s.err
	}
	return s.text, cakecli.Usage{}, nil
}

func TestCompactTimingsAccountForStages(t *testing.T) {
	dataDir := t.TempDir()
	sourcePath := writeFixture(t, dataDir)
	t.Setenv(fakeCakeDelay, "30")

	compactor := &Compactor{
		Mode:    ModeInPlace,
		DataDir: dataDir,
		CakeBin: writeFakeCake(t),
		Options: Options{Force: true, KeepTokens: 10, Trigger: "manual"},
		Summarizer: delayedSummarizer{
			delay: 40 * time.Millisecond,
			text:  "The user asked about the retry policy and changed the timeout.",
		},
		Now:   time.Now,
		NewID: NewUUID,
	}
	result, err := compactor.Compact(context.Background(), sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Compacted {
		t.Fatalf("expected a compaction: %+v", result)
	}

	timings := result.Timings
	if timings == nil {
		t.Fatal("result has no timings")
	}
	for name, value := range map[string]*int64{
		"load":      timings.LoadMs,
		"plan":      timings.PlanMs,
		"preflight": timings.PreflightMs,
		"summary":   timings.SummaryMs,
		"commit":    timings.CommitMs,
	} {
		if value == nil {
			t.Fatalf("timings.%s is missing: %+v", name, timings)
		}
	}
	if timings.VerifyMs != nil {
		t.Fatalf("in-place compaction reported verify_ms: %+v", timings)
	}
	if *timings.SummaryMs < 40 {
		t.Fatalf("summary_ms = %d, want at least the 40ms delay", *timings.SummaryMs)
	}
	if *timings.PreflightMs < 30 || *timings.CommitMs < 30 {
		t.Fatalf("preflight/commit = %d/%dms, want at least the 30ms fake-cake delay", *timings.PreflightMs, *timings.CommitMs)
	}
	if result.SummaryTimeMs != *timings.SummaryMs {
		t.Fatalf("summary_time_ms = %d, timings.summary_ms = %d", result.SummaryTimeMs, *timings.SummaryMs)
	}
	stageSum := *timings.LoadMs + *timings.PlanMs + *timings.PreflightMs + *timings.SummaryMs + *timings.CommitMs
	if timings.TotalMs < stageSum {
		t.Fatalf("total_ms = %d, below the stage sum %d: %+v", timings.TotalMs, stageSum, timings)
	}

	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	encoded, ok := document["timings"].(map[string]any)
	if !ok {
		t.Fatalf("document has no timings object: %s", raw)
	}
	for _, key := range []string{"total_ms", "load_ms", "plan_ms", "preflight_ms", "summary_ms", "commit_ms"} {
		if _, ok := encoded[key]; !ok {
			t.Fatalf("timings has no %s: %s", key, raw)
		}
	}
	if _, ok := encoded["verify_ms"]; ok {
		t.Fatalf("in-place timings has verify_ms: %s", raw)
	}
}

func TestCompactTimingsOnNoOp(t *testing.T) {
	dataDir := t.TempDir()
	sourcePath := writeFixture(t, dataDir)

	compactor := &Compactor{
		Mode:    ModeInPlace,
		DataDir: dataDir,
		CakeBin: writeFakeCake(t),
		Options: Options{ThresholdTokens: 1_000_000, KeepTokens: 10, Trigger: "threshold"},
		Now:     time.Now,
		NewID:   NewUUID,
	}
	result, err := compactor.Compact(context.Background(), sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if result.Compacted {
		t.Fatalf("expected a no-op: %+v", result)
	}
	timings := result.Timings
	if timings == nil {
		t.Fatal("result has no timings")
	}
	if timings.LoadMs == nil || timings.PlanMs == nil {
		t.Fatalf("no-op timings omit load or plan: %+v", timings)
	}
	if timings.PreflightMs != nil || timings.SummaryMs != nil || timings.CommitMs != nil || timings.VerifyMs != nil {
		t.Fatalf("no-op timings report a stage that did not run: %+v", timings)
	}
	if timings.TotalMs < *timings.LoadMs+*timings.PlanMs {
		t.Fatalf("total_ms = %d, below load+plan: %+v", timings.TotalMs, timings)
	}
}

func TestCompactTimingFailureHasNoResult(t *testing.T) {
	dataDir := t.TempDir()
	sourcePath := writeFixture(t, dataDir)
	t.Setenv(fakeCakeDelay, "20")
	t.Setenv(fakeCakeMode, "reject")

	compactor := &Compactor{
		Mode:    ModeInPlace,
		DataDir: dataDir,
		CakeBin: writeFakeCake(t),
		Options: Options{Force: true, KeepTokens: 10, SummaryFile: writeSummary(t, "a summary"), Trigger: "manual"},
		Now:     time.Now,
		NewID:   NewUUID,
	}
	started := time.Now()
	result, err := compactor.Compact(context.Background(), sourcePath)
	if err == nil {
		t.Fatal("expected the rejected checkpoint to fail")
	}
	if result != nil {
		t.Fatalf("failed compaction returned a result document: %+v", result)
	}
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond {
		t.Fatalf("failure returned after %s, before the controlled subprocess delay", elapsed)
	}
}

func TestForkTimingsIncludeVerification(t *testing.T) {
	dataDir := t.TempDir()
	sourcePath := writeFixture(t, dataDir)
	t.Setenv(fakeCakeDelay, "25")

	compactor := &Compactor{
		Mode:    ModeFork,
		DataDir: dataDir,
		CakeBin: writeFakeCake(t),
		Verify:  true,
		Options: Options{Force: true, KeepTokens: 10, SummaryFile: writeSummary(t, "fork summary"), Trigger: "manual"},
		Now:     time.Now,
		NewID:   func() (string, error) { return newID, nil },
	}
	result, err := compactor.Compact(context.Background(), sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Compacted || result.InPlace {
		t.Fatalf("expected a fork compaction: %+v", result)
	}
	timings := result.Timings
	if timings == nil {
		t.Fatal("result has no timings")
	}
	if timings.CommitMs == nil || timings.VerifyMs == nil {
		t.Fatalf("fork timings omit commit or verify: %+v", timings)
	}
	for name, value := range map[string]*int64{
		"load":    timings.LoadMs,
		"plan":    timings.PlanMs,
		"summary": timings.SummaryMs,
	} {
		if value == nil {
			t.Fatalf("fork timings omit %s: %+v", name, timings)
		}
	}
	if *timings.VerifyMs < 25 {
		t.Fatalf("verify_ms = %d, want at least the 25ms fake-cake delay", *timings.VerifyMs)
	}
	stageSum := *timings.LoadMs + *timings.PlanMs + *timings.SummaryMs + *timings.CommitMs + *timings.VerifyMs
	if timings.TotalMs < stageSum {
		t.Fatalf("total_ms = %d, below the stage sum %d: %+v", timings.TotalMs, stageSum, timings)
	}
}
