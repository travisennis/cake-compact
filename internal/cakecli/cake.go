// Package cakecli runs the Cake binary and resolves its data directory.
package cakecli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// ErrNoRuntimeContextUnavailable marks a Cake binary without the root
// --no-runtime-context flag. Callers must not fall back to prompting around
// the process-environment Developer message.
var ErrNoRuntimeContextUnavailable = errors.New("cake --no-runtime-context is unavailable; upgrade cake")

// RuntimeContextUnavailableError reports a binary that cannot suppress the
// process-environment Developer message.
type RuntimeContextUnavailableError struct {
	Binary string
	Detail string
}

// Error includes the upgrade instruction so the operator sees the fix.
func (e *RuntimeContextUnavailableError) Error() string {
	detail := strings.TrimSpace(e.Detail)
	if detail == "" {
		detail = "the flag is not implemented"
	}
	return fmt.Sprintf("%s (%s: %s)", ErrNoRuntimeContextUnavailable.Error(), e.Binary, detail)
}

// Unwrap lets callers test with errors.Is(err, ErrNoRuntimeContextUnavailable).
func (e *RuntimeContextUnavailableError) Unwrap() error { return ErrNoRuntimeContextUnavailable }

// NoRuntimeContextAvailable reports whether `cake --help` lists the root
// --no-runtime-context flag. It needs no session, no payload, and no model.
func NoRuntimeContextAvailable(ctx context.Context, binary string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("cake --no-runtime-context check cancelled: %w", err)
	}
	cmd := exec.CommandContext(ctx, binary, "--help")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("cake --no-runtime-context check cancelled: %w", ctxErr)
		}
		detail := firstNonEmpty(stderr.String(), stdout.String(), err.Error())
		return &RuntimeContextUnavailableError{Binary: binary, Detail: detail}
	}
	if !strings.Contains(stdout.String(), "--no-runtime-context") {
		return &RuntimeContextUnavailableError{Binary: binary, Detail: "the flag is not listed in cake --help"}
	}
	return nil
}

// Summarize asks a tool-less, session-less Cake invocation to summarize the
// transcript. The invocation omits Cake's process-environment Developer
// message, so the summary cannot treat the summarizer's cwd, date, platform,
// shell, or terminal as session content. The child runs in a temporary
// directory with a temporary data root so the summary call cannot pollute the
// user's sessions or telemetry.
// The returned usage is that invocation's own provider usage; a response whose
// usage is absent or unparseable leaves it zero rather than failing a summary
// that otherwise succeeded.
func Summarize(ctx context.Context, options SummaryOptions) (string, Usage, error) {
	var usage Usage
	if err := NoRuntimeContextAvailable(ctx, options.Binary); err != nil {
		return "", usage, err
	}
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
		"--no-runtime-context",
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
	summary := strings.TrimSpace(*completion.Result)
	return summary, usage, nil
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
