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

func writeScript(t *testing.T, body string) string {
	t.Helper()
	help := `if [ "${1:-}" = "--help" ]; then
  printf '%s\n' 'Usage: cake [OPTIONS]' '      --no-runtime-context  Omit the process-environment Developer message'
  exit 0
fi
`
	return writeCakeScript(t, help+body)
}

func writeCakeScript(t *testing.T, body string) string {
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

func TestSummarizePassesNoRuntimeContext(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("sh is unavailable")
	}
	argsPath := filepath.Join(t.TempDir(), "args")
	script := writeScript(t, `printf '%s\n' "$@" > "`+argsPath+`"
cat >/dev/null
printf '{"result":"the summary"}'
`)
	summary, _, err := Summarize(context.Background(), SummaryOptions{
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
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--no-runtime-context") {
		t.Fatalf("summarizer did not pass --no-runtime-context: %s", args)
	}
}

func TestSummarizePassesSelectionFlags(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("sh is unavailable")
	}
	argsPath := filepath.Join(t.TempDir(), "args")
	script := writeScript(t, `printf '%s\n' "$@" > "`+argsPath+`"
cat >/dev/null
printf '{"result":"the summary"}'
`)
	summary, _, err := Summarize(context.Background(), SummaryOptions{
		Binary:          script,
		Model:           "summary-model",
		Profile:         "summary-profile",
		ReasoningEffort: "low",
		SystemPrompt:    "summarize",
		Transcript:      "USER:\nhello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary != "the summary" {
		t.Fatalf("summary = %q", summary)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(args)
	for _, want := range []string{
		"--model\nsummary-model\n",
		"--profile\nsummary-profile\n",
		"--reasoning-effort\nlow\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summarizer args missing %q: %s", want, got)
		}
	}
}

func TestSummarizeRequiresNoRuntimeContext(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("sh is unavailable")
	}
	argsPath := filepath.Join(t.TempDir(), "args")
	script := writeCakeScript(t, `printf '%s\n' "$*" >> "`+argsPath+`"
cat >/dev/null
printf '{"result":"the summary"}'
`)
	_, _, err := Summarize(context.Background(), SummaryOptions{
		Binary:       script,
		SystemPrompt: "summarize",
		Transcript:   "USER:\nhello",
	})
	if !errors.Is(err, ErrNoRuntimeContextUnavailable) {
		t.Fatalf("error = %v, want ErrNoRuntimeContextUnavailable", err)
	}
	if !strings.Contains(err.Error(), "upgrade cake") {
		t.Fatalf("error does not name the upgrade: %v", err)
	}
	args, readErr := os.ReadFile(argsPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got := strings.TrimSpace(string(args)); got != "--help" {
		t.Fatalf("summarizer ran beyond the capability probe: %q", got)
	}
}

func TestNoRuntimeContextAvailableReportsFailedHelp(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("sh is unavailable")
	}
	script := writeCakeScript(t, "echo 'cake: unknown flag' >&2\nexit 2\n")
	err := NoRuntimeContextAvailable(context.Background(), script)
	if !errors.Is(err, ErrNoRuntimeContextUnavailable) {
		t.Fatalf("error = %v, want ErrNoRuntimeContextUnavailable", err)
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("error lost the help detail: %v", err)
	}
}
