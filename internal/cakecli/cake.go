// Package cakecli runs the Cake binary and resolves its data directory.
package cakecli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ResolveDataDir returns the Cake data root. It matches Cake's own resolution:
// an explicit override, then CAKE_DATA_DIR, then ~/.local/share/cake.
func ResolveDataDir(override string) (string, error) {
	if override != "" {
		return filepath.Abs(override)
	}
	if env := os.Getenv("CAKE_DATA_DIR"); env != "" {
		return filepath.Abs(env)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate the home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "cake"), nil
}

// SessionsDir returns the sessions directory under a Cake data root.
func SessionsDir(dataDir string) string {
	return filepath.Join(dataDir, "sessions")
}

// ResolveBinary returns the Cake executable to use. An explicit path or a
// value from CAKE_BIN wins over PATH.
func ResolveBinary(override string) (string, error) {
	binary := override
	if binary == "" {
		binary = os.Getenv("CAKE_BIN")
	}
	if binary == "" {
		binary = "cake"
	}
	if strings.ContainsRune(binary, filepath.Separator) {
		return filepath.Abs(binary)
	}
	path, err := exec.LookPath(binary)
	if err != nil {
		return "", fmt.Errorf("cannot find the cake executable %q: %w", binary, err)
	}
	return path, nil
}

// Completion is the subset of Cake's --output-format json document this tool
// reads.
type Completion struct {
	SessionID string          `json:"session_id"`
	Result    *string         `json:"result"`
	Error     *string         `json:"error"`
	Turns     uint32          `json:"turns"`
	Usage     json.RawMessage `json:"usage"`
}

// TurnOptions describes one Cake turn.
type TurnOptions struct {
	Binary          string
	DataDir         string
	WorkingDir      string
	Resume          string
	Model           string
	Profile         string
	ReasoningEffort string
	AddDirs         []string
	ExtraArgs       []string
	Prompt          string
	PromptFromStdin bool
	Stderr          io.Writer
}

// RunTurn executes one Cake turn with JSON completion output. It returns the
// process stdout, the process exit code, and a start error.
func RunTurn(ctx context.Context, options TurnOptions) ([]byte, int, error) {
	args := []string{"--output-format", "json"}
	if options.Resume != "" {
		args = append(args, "--resume", options.Resume)
	}
	if options.Model != "" {
		args = append(args, "--model", options.Model)
	}
	if options.Profile != "" {
		args = append(args, "--profile", options.Profile)
	}
	if options.ReasoningEffort != "" {
		args = append(args, "--reasoning-effort", options.ReasoningEffort)
	}
	for _, dir := range options.AddDirs {
		args = append(args, "--add-dir", dir)
	}
	args = append(args, options.ExtraArgs...)
	if options.PromptFromStdin {
		args = append(args, "-")
	} else {
		args = append(args, options.Prompt)
	}

	cmd := exec.CommandContext(ctx, options.Binary, args...)
	if options.WorkingDir != "" {
		cmd.Dir = options.WorkingDir
	}
	if options.PromptFromStdin {
		cmd.Stdin = strings.NewReader(options.Prompt)
	}
	cmd.Env = environmentWith("CAKE_DATA_DIR", options.DataDir)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	if options.Stderr != nil {
		cmd.Stderr = io.MultiWriter(options.Stderr, &stderr)
	} else {
		cmd.Stderr = &stderr
	}

	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), 0, nil
	}

	var exitErr *exec.ExitError
	if ok := asExitError(err, &exitErr); ok {
		return stdout.Bytes(), exitErr.ExitCode(), nil
	}
	return nil, 0, fmt.Errorf("cannot run %s: %w", options.Binary, err)
}

func asExitError(err error, target **exec.ExitError) bool {
	exitErr, ok := err.(*exec.ExitError)
	if ok {
		*target = exitErr
	}
	return ok
}

// Replay runs cake replay and rejects a compacted session Cake cannot load.
func Replay(ctx context.Context, binary, dataDir, sessionID string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, binary, "--output-format", "stream-json", "replay", sessionID)
	cmd.Env = environmentWith("CAKE_DATA_DIR", dataDir)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		return nil, fmt.Errorf("cake replay rejected session %s: %v: %s", sessionID, err, detail)
	}

	found := false
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var envelope struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(line), &envelope) != nil {
			continue
		}
		if envelope.Type == "compaction" {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("cake replay did not emit the compaction record for %s", sessionID)
	}
	return stdout.Bytes(), nil
}

// SummaryOptions describes one summarizer run.
type SummaryOptions struct {
	Binary       string
	Model        string
	Profile      string
	SystemPrompt string
	Transcript   string
}

// Usage is the provider-reported token usage for one Cake invocation. A zero
// value means the response carried no usage, not zero tokens.
type Usage struct {
	InputTokens  uint64 `json:"input_tokens"`
	OutputTokens uint64 `json:"output_tokens"`
	TotalTokens  uint64 `json:"total_tokens"`
}

// Summarize asks a tool-less, session-less Cake invocation to summarize the
// transcript. The child runs in a temporary directory with a temporary data
// root so the summary call cannot pollute the user's sessions or telemetry.
// The returned usage is that invocation's own provider usage; a response whose
// usage is absent or unparseable leaves it zero rather than failing a summary
// that otherwise succeeded.
func Summarize(ctx context.Context, options SummaryOptions) (string, Usage, error) {
	var usage Usage
	tempDir, err := os.MkdirTemp("", "cake-compact-summary-*")
	if err != nil {
		return "", usage, err
	}
	defer os.RemoveAll(tempDir)

	promptPath := filepath.Join(tempDir, "system-prompt.txt")
	if err := os.WriteFile(promptPath, []byte(options.SystemPrompt), 0o600); err != nil {
		return "", usage, err
	}
	dataDir := filepath.Join(tempDir, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", usage, err
	}

	args := []string{
		"--no-session",
		"--no-tools",
		"--no-skills",
		"--output-format", "json",
		"--system-prompt", promptPath,
	}
	if options.Model != "" {
		args = append(args, "--model", options.Model)
	}
	if options.Profile != "" {
		args = append(args, "--profile", options.Profile)
	}
	args = append(args, "-")

	cmd := exec.CommandContext(ctx, options.Binary, args...)
	cmd.Dir = tempDir
	cmd.Env = environmentWith("CAKE_DATA_DIR", dataDir)
	cmd.Stdin = strings.NewReader(options.Transcript)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		return "", usage, fmt.Errorf("summarizer cake invocation failed: %v: %s", err, detail)
	}

	var completion Completion
	if err := json.Unmarshal(stdout.Bytes(), &completion); err != nil {
		return "", usage, fmt.Errorf("summarizer returned invalid completion JSON: %w: %s", err, strings.TrimSpace(stdout.String()))
	}
	// Usage is diagnostic: never let an unfamiliar shape fail the summary.
	if len(completion.Usage) > 0 {
		_ = json.Unmarshal(completion.Usage, &usage)
	}
	if completion.Error != nil && strings.TrimSpace(*completion.Error) != "" {
		return "", usage, fmt.Errorf("summarizer failed: %s", strings.TrimSpace(*completion.Error))
	}
	if completion.Result == nil || strings.TrimSpace(*completion.Result) == "" {
		return "", usage, fmt.Errorf("summarizer returned an empty summary")
	}
	summary := sanitizeSummary(strings.TrimSpace(*completion.Result), options.Transcript, tempDir, time.Now())
	if summary == "" {
		return "", usage, fmt.Errorf("summarizer returned only its own runtime metadata")
	}
	return summary, usage, nil
}

// summaryRuntimeFact is one piece of Cake's process environment that the
// summarizer must not present as session content.
type summaryRuntimeFact struct {
	values []string
}

func (fact summaryRuntimeFact) matches(text string) bool {
	lower := strings.ToLower(text)
	for _, value := range fact.values {
		if value != "" && strings.Contains(lower, strings.ToLower(value)) {
			return true
		}
	}
	return false
}

// summaryRuntimeFacts describes the environment Cake adds as a separate
// Developer message. Cake's build_initial_prompt_messages_with_enabled_tools
// appends that message even when --system-prompt replaces the system prompt, so
// the summarizer cannot suppress it from this process.
func summaryRuntimeFacts(workingDir string, now time.Time) ([]summaryRuntimeFact, []string) {
	dirs := []string{workingDir}
	if resolved, err := filepath.EvalSymlinks(workingDir); err == nil && resolved != workingDir {
		dirs = append(dirs, resolved)
	}

	platform := []string{runtime.GOOS}
	switch runtime.GOOS {
	case "darwin":
		platform = append(platform, "macos", "mac os")
	case "windows":
		platform = append(platform, "windows")
	}

	architecture := []string{runtime.GOARCH}
	switch runtime.GOARCH {
	case "arm64":
		architecture = append(architecture, "aarch64")
	case "amd64":
		architecture = append(architecture, "x86_64", "x86-64")
	case "386":
		architecture = append(architecture, "i386", "x86")
	}

	shell := []string{}
	if value := os.Getenv("SHELL"); value != "" {
		shell = append(shell, value)
		if base := filepath.Base(value); base != value {
			shell = append(shell, base)
		}
	}

	terminal := []string{}
	if value := os.Getenv("TERM_PROGRAM"); value != "" {
		terminal = append(terminal, value)
	}
	if value := os.Getenv("TERM"); value != "" {
		terminal = append(terminal, value)
	}

	return []summaryRuntimeFact{
		{values: platform},
		{values: architecture},
		{values: shell},
		{values: terminal},
		{values: []string{now.Format("2006-01-02")}},
	}, dirs
}

// sanitizeSummary removes lines that can only have come from the summarizer's
// own process environment. A line with an environment label is dropped when it
// asserts a runtime fact that the retired transcript does not contain; a path
// from the per-call summary temp directory is always dropped.
func sanitizeSummary(summary, transcript, workingDir string, now time.Time) string {
	facts, dirs := summaryRuntimeFacts(workingDir, now)
	lines := strings.Split(summary, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if runtimeOnlyLine(line, transcript, facts, dirs) {
			continue
		}
		kept = append(kept, line)
	}

	cleaned := strings.TrimSpace(strings.Join(kept, "\n"))
	for strings.Contains(cleaned, "\n\n\n") {
		cleaned = strings.ReplaceAll(cleaned, "\n\n\n", "\n\n")
	}
	return cleaned
}

func runtimeOnlyLine(line, transcript string, facts []summaryRuntimeFact, dirs []string) bool {
	lower := strings.ToLower(line)
	if strings.Contains(lower, "cake-compact-summary-") {
		return true
	}
	for _, dir := range dirs {
		if dir != "" && strings.Contains(line, dir) {
			return true
		}
	}
	if !runtimeLabelAtStart(line) {
		return false
	}
	present, missing := 0, 0
	for _, fact := range facts {
		if fact.matches(line) {
			present++
			if !fact.matches(transcript) {
				missing++
			}
		}
	}
	return present > 0 && missing > 0
}

func runtimeLabelAtStart(line string) bool {
	trimmed := strings.ToLower(strings.TrimLeft(strings.TrimSpace(line), "-*#>_ \t"))
	for _, label := range []string{
		"working directory", "current working directory", "environment",
		"platform", "architecture", "shell", "terminal", "today's date",
		"current date", "date",
	} {
		if strings.HasPrefix(trimmed, label) {
			return true
		}
	}
	return false
}

// environmentWith returns the process environment with key set to value,
// removing duplicate assignments so the child sees the intended value first.
func environmentWith(key, value string) []string {
	env := os.Environ()
	filtered := make([]string, 0, len(env)+1)
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			continue
		}
		filtered = append(filtered, entry)
	}
	return append(filtered, prefix+value)
}
