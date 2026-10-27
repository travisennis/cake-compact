package cakecli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-cake")
	script := "#!/bin/sh\nset -eu\n" + body
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEnvironmentWithReplacesExistingValue(t *testing.T) {
	t.Setenv("CAKE_DATA_DIR", "/old")
	env := environmentWith("CAKE_DATA_DIR", "/new")
	count := 0
	for _, entry := range env {
		if strings.HasPrefix(entry, "CAKE_DATA_DIR=") {
			count++
			if entry != "CAKE_DATA_DIR=/new" {
				t.Fatalf("unexpected entry %q", entry)
			}
		}
	}
	if count != 1 {
		t.Fatalf("expected one CAKE_DATA_DIR entry, got %d", count)
	}
}

func TestResolveDataDirAndBinary(t *testing.T) {
	t.Setenv("CAKE_DATA_DIR", "/tmp/cake-data")
	dir, err := ResolveDataDir("")
	if err != nil || dir != "/tmp/cake-data" {
		t.Fatalf("data dir = %q, %v", dir, err)
	}
	override, err := ResolveDataDir("/tmp/other")
	if err != nil || override != "/tmp/other" {
		t.Fatalf("override = %q, %v", override, err)
	}

	script := writeScript(t, "exit 0\n")
	binary, err := ResolveBinary(script)
	if err != nil || binary != script {
		t.Fatalf("binary = %q, %v", binary, err)
	}
	if SessionsDir("/tmp/data") != filepath.Join("/tmp/data", "sessions") {
		t.Fatalf("unexpected sessions dir: %s", SessionsDir("/tmp/data"))
	}
}

func TestRunTurnParsesCompletion(t *testing.T) {
	script := writeScript(t, `printf '{"session_id":"11111111-1111-4111-8111-111111111111","result":"ok","turns":1}'`+"\n")
	stdout, code, err := RunTurn(context.Background(), TurnOptions{
		Binary:  script,
		DataDir: t.TempDir(),
		Prompt:  "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	var completion Completion
	if err := json.Unmarshal(stdout, &completion); err != nil {
		t.Fatal(err)
	}
	if completion.SessionID == "" || completion.Result == nil || *completion.Result != "ok" {
		t.Fatalf("unexpected completion: %+v", completion)
	}
}

func TestRunTurnReportsExitCode(t *testing.T) {
	script := writeScript(t, "printf '{\"error\":\"bad\"}'\nexit 3\n")
	_, code, err := RunTurn(context.Background(), TurnOptions{
		Binary:  script,
		DataDir: t.TempDir(),
		Prompt:  "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
}

func TestSummarizeUsesStdinAndParsesResult(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("sh is unavailable")
	}
	script := writeScript(t, `cat >/dev/null; printf '{"session_id":"11111111-1111-4111-8111-111111111111","result":"the summary","usage":{"input_tokens":120,"output_tokens":30,"total_tokens":150}}'`+"\n")
	summary, usage, err := Summarize(context.Background(), SummaryOptions{
		Binary:       script,
		SystemPrompt: "summarize",
		Transcript:   "USER:\nhello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary != "the summary" {
		t.Fatalf("summary = %q", summary)
	}
	want := Usage{InputTokens: 120, OutputTokens: 30, TotalTokens: 150}
	if usage != want {
		t.Fatalf("usage = %+v, want %+v", usage, want)
	}
}

// A response whose usage is missing or malformed still produces the summary;
// usage is evidence, not a completion requirement.
func TestSummarizeToleratesUnusableUsage(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("sh is unavailable")
	}
	script := writeScript(t, `cat >/dev/null; printf '{"result":"the summary","usage":{"input_tokens":"oops"}}'`+"\n")
	summary, usage, err := Summarize(context.Background(), SummaryOptions{
		Binary:       script,
		SystemPrompt: "summarize",
		Transcript:   "USER:\nhello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary != "the summary" {
		t.Fatalf("summary = %q", summary)
	}
	if usage != (Usage{}) {
		t.Fatalf("usage = %+v, want zero", usage)
	}
}

func TestSanitizeSummaryDropsProcessEnvironmentFacts(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("TERM_PROGRAM", "ghostty")
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.Local)
	dir := filepath.Join(t.TempDir(), "cake-compact-summary-123")
	summary := "## Session Overview\n" +
		"- Working directory: `" + dir + "`\n" +
		"- Environment: macOS, aarch64, shell `/bin/zsh`, terminal ghostty, date " + now.Format("2006-01-02") + ".\n" +
		"- The project is /tmp/project."

	got := sanitizeSummary(summary, "USER:\nThe project is /tmp/project.", dir, now)
	if strings.Contains(got, dir) || strings.Contains(got, "cake-compact-summary-") {
		t.Fatalf("summary retained its process directory: %s", got)
	}
	if strings.Contains(got, "Environment:") {
		t.Fatalf("summary retained process environment facts: %s", got)
	}
	if !strings.Contains(got, "The project is /tmp/project.") {
		t.Fatalf("summary lost transcript content: %s", got)
	}
}

func TestSanitizeSummaryKeepsEnvironmentFactsPresentInTranscript(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("TERM_PROGRAM", "ghostty")
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.Local)
	summary := "Environment: " + runtime.GOOS + ", shell `/bin/zsh`."
	transcript := "USER:\nThe host is " + runtime.GOOS + " and the shell is /bin/zsh."

	got := sanitizeSummary(summary, transcript, t.TempDir(), now)
	if got != summary {
		t.Fatalf("summary with transcript-backed facts changed: %q", got)
	}
}

func TestSanitizeSummaryKeepsProseThatMentionsRuntimeTerms(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("TERM_PROGRAM", "ghostty")
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.Local)
	summary := "- The terminal command failed on date " + now.Format("2006-01-02") + "."
	transcript := "USER:\nThe terminal command failed."

	got := sanitizeSummary(summary, transcript, t.TempDir(), now)
	if got != summary {
		t.Fatalf("prose containing runtime terms changed: %q", got)
	}
}

func TestSummarizeDropsSummarizerWorkingDirectory(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("sh is unavailable")
	}
	script := writeScript(t, `cat >/dev/null; printf '{"result":"Working directory: %s\\nProject: /tmp/project"}' "$PWD"`+"\n")
	summary, _, err := Summarize(context.Background(), SummaryOptions{
		Binary:       script,
		SystemPrompt: "summarize",
		Transcript:   "USER:\nThe project is /tmp/project.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(summary, "Working directory:") || strings.Contains(summary, "cake-compact-summary-") {
		t.Fatalf("summary retained the child working directory: %q", summary)
	}
	if !strings.Contains(summary, "Project: /tmp/project") {
		t.Fatalf("summary lost transcript content: %q", summary)
	}
}

func TestSummarizeRejectsOnlyRuntimeMetadata(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("sh is unavailable")
	}
	script := writeScript(t, `cat >/dev/null; printf '{"result":"Working directory: %s"}' "$PWD"`+"\n")
	_, _, err := Summarize(context.Background(), SummaryOptions{
		Binary:       script,
		SystemPrompt: "summarize",
		Transcript:   "USER:\nhello",
	})
	if err == nil || !strings.Contains(err.Error(), "only its own runtime metadata") {
		t.Fatalf("error = %v, want a runtime-metadata rejection", err)
	}
}
