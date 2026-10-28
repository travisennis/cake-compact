package compact

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"unicode/utf8"

	"github.com/travisennis/cake-compact/internal/cakecli"
	"github.com/travisennis/cake-compact/internal/session"
)

// SummaryInstructionVersion identifies the summary prompt below. Bump it when
// the prompt changes in a way that affects the stored summary.
const SummaryInstructionVersion = 3

// Summarizer turns an earlier-session transcript into a checkpoint summary. It
// also returns the provider usage the summary call reported; a summarizer with
// no provider behind it returns a zero usage.
type Summarizer interface {
	Summarize(ctx context.Context, transcript string) (string, cakecli.Usage, error)
}

// FileSummarizer returns a fixed summary, for tests and manual experiments.
type FileSummarizer struct {
	Path string
}

// Summarize reads the summary file.
func (s FileSummarizer) Summarize(context.Context, string) (string, cakecli.Usage, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return "", cakecli.Usage{}, err
	}
	return strings.TrimSpace(string(data)), cakecli.Usage{}, nil
}

// CommandSummarizer pipes the transcript to a shell command and reads the
// summary from its stdout.
type CommandSummarizer struct {
	Command string
}

// Summarize runs the command.
func (s CommandSummarizer) Summarize(ctx context.Context, transcript string) (string, cakecli.Usage, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", s.Command)
	cmd.Stdin = strings.NewReader(transcript)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", cakecli.Usage{}, fmt.Errorf("summarizer command failed: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	summary := strings.TrimSpace(stdout.String())
	if summary == "" {
		return "", cakecli.Usage{}, fmt.Errorf("summarizer command returned an empty summary")
	}
	return summary, cakecli.Usage{}, nil
}

// CakeSummarizer asks a tool-less, session-less Cake invocation for a summary.
type CakeSummarizer struct {
	Binary          string
	Model           string
	Profile         string
	ReasoningEffort string
	Instructions    string
}

// Summarize runs Cake as the summarizer.
func (s CakeSummarizer) Summarize(ctx context.Context, transcript string) (string, cakecli.Usage, error) {
	return cakecli.Summarize(ctx, cakecli.SummaryOptions{
		Binary:          s.Binary,
		Model:           s.Model,
		Profile:         s.Profile,
		ReasoningEffort: s.ReasoningEffort,
		SystemPrompt:    BuildSystemPrompt(s.Instructions),
		Transcript:      transcript,
	})
}

// BuildSystemPrompt returns the summarizer instructions.
func BuildSystemPrompt(instructions string) string {
	var builder strings.Builder
	builder.WriteString(`You are the context-compaction summarizer for the Cake coding assistant.

The text supplied on stdin is an earlier portion of one coding session, ordered oldest first. Write a compact, factual summary that lets the assistant continue the work with less context.

Preserve:
- the user's goals, constraints, and corrections;
- decisions and the reasons behind them;
- exact file paths, identifiers, commands, and error text that matter;
- what was implemented or changed, and what was verified;
- failures, dead ends, and unresolved questions;
- the current state of the work and concrete next steps.

Do not invent facts, do not claim work was verified unless the transcript says so, and do not add advice or commentary. Use short headings and bullet points. Output only the summary.
`)
	if strings.TrimSpace(instructions) != "" {
		builder.WriteString("\nAdditional focus requested by the user:\n")
		builder.WriteString(strings.TrimSpace(instructions))
		builder.WriteString("\n")
	}
	return builder.String()
}

// BuildTranscript renders the retired prefix as a readable transcript. It
// omits image bytes and opaque reasoning blobs, bounds each tool result, and
// fails rather than silently dropping content above maxChars.
func BuildTranscript(items []session.Item, maxChars int) (string, error) {
	if maxChars <= 0 {
		maxChars = 800000
	}
	var builder strings.Builder
	builder.WriteString("EARLIER SESSION TRANSCRIPT (oldest first)\n\n")

	for _, item := range items {
		var block strings.Builder
		switch {
		case item.Synthetic && item.Message != nil:
			summary := strings.TrimPrefix(item.Message.Content, session.HistoricalSummaryMarker+"\n\n")
			block.WriteString("HISTORICAL SUMMARY:\n")
			block.WriteString(summary)
			block.WriteString("\n")
		case item.Message != nil:
			block.WriteString(strings.ToUpper(item.Message.Role))
			block.WriteString(":\n")
			block.WriteString(item.Message.Content)
			block.WriteString("\n")
			for _, image := range item.Message.Images {
				fmt.Fprintf(&block, "[image omitted: %s, %d base64 characters]\n", image.MediaType, len(image.DataBase64))
			}
		case item.Reasoning != nil:
			text := strings.TrimSpace(item.Reasoning.Text())
			if text == "" {
				continue
			}
			block.WriteString("ASSISTANT REASONING SUMMARY:\n")
			block.WriteString(truncate(text, 4000))
			block.WriteString("\n")
		case item.Call != nil:
			block.WriteString("TOOL CALL ")
			block.WriteString(item.Call.Name)
			block.WriteString(" (")
			block.WriteString(item.Call.CallID)
			block.WriteString("):\n")
			block.WriteString(truncate(item.Call.Arguments, 4000))
			block.WriteString("\n")
		case item.Output != nil:
			block.WriteString("TOOL RESULT (")
			block.WriteString(item.Output.CallID)
			block.WriteString("):\n")
			block.WriteString(truncate(item.Output.Output, 6000))
			block.WriteString("\n")
			for _, image := range item.Output.Images {
				fmt.Fprintf(&block, "[image omitted: %s, %d base64 characters]\n", image.MediaType, len(image.DataBase64))
			}
		default:
			continue
		}

		if builder.Len()+block.Len() > maxChars {
			return "", fmt.Errorf(
				"summary input would exceed %d characters; raise -keep-tokens to retain more and summarize less",
				maxChars,
			)
		}
		builder.WriteString(block.String())
		builder.WriteString("\n---\n\n")
	}

	return builder.String(), nil
}

func truncate(text string, max int) string {
	if len(text) <= max {
		return text
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + fmt.Sprintf("\n[... %d characters omitted ...]", len(text)-cut)
}
