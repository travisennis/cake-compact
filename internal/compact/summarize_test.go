package compact

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/travisennis/cake-compact/internal/cakecli"
	"github.com/travisennis/cake-compact/internal/session"
)

func TestBuildTranscriptFormatsAndBoundsContent(t *testing.T) {
	items := []session.Item{
		messageItem("user", "read the file"),
		{Type: session.TypeFunctionCall, Call: &session.FunctionCall{CallID: "call-1", Name: "Read", Arguments: `{"file_path":"big.txt"}`}},
		{Type: session.TypeFunctionOutput, Output: &session.FunctionCallOutput{
			CallID: "call-1",
			Output: strings.Repeat("y", 12000),
			Images: []session.Image{{MediaType: "image/png", DataBase64: "abc"}},
		}},
		{Type: session.TypeReasoning, Reasoning: &session.Reasoning{Summary: []byte(`[{"type":"summary_text","text":"looked at the file"}]`), EncryptedContent: "opaque"}},
		{Type: session.TypeMessage, Message: &session.Message{Role: "assistant", Content: "done"}},
	}

	transcript, err := BuildTranscript(items, 100000)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"USER:\nread the file", "TOOL CALL Read (call-1)", "TOOL RESULT (call-1)", "characters omitted", "ASSISTANT REASONING SUMMARY:\nlooked at the file", "[image omitted: image/png", "ASSISTANT:\ndone"} {
		if !strings.Contains(transcript, want) {
			t.Fatalf("transcript is missing %q:\n%s", want, transcript)
		}
	}
	if strings.Contains(transcript, strings.Repeat("y", 12000)) {
		t.Fatal("tool output was not truncated")
	}
	if strings.Contains(transcript, "opaque") {
		t.Fatal("opaque reasoning bytes must not enter the summary prompt")
	}
}

func TestBuildTranscriptRejectsContentAboveTheCap(t *testing.T) {
	items := []session.Item{messageItem("user", strings.Repeat("x", 5000))}
	if _, err := BuildTranscript(items, 100); err == nil {
		t.Fatal("an over-cap transcript must fail rather than drop content")
	}
}

func TestBuildSystemPromptIncludesExtraInstructions(t *testing.T) {
	prompt := BuildSystemPrompt("focus on the failing command")
	for _, want := range []string{
		"context-compaction summarizer",
		"focus on the failing command",
		"Do not invent facts",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt is missing %q: %s", want, prompt)
		}
	}
	for _, unwanted := range []string{
		"Runtime boundary:",
		"Developer message",
	} {
		if strings.Contains(prompt, unwanted) {
			t.Fatalf("prompt still contains %q: %s", unwanted, prompt)
		}
	}
}

func TestFileSummarizerTrimsContent(t *testing.T) {
	path := writeSummary(t, "  summary text\n")
	summary, usage, err := FileSummarizer{Path: path}.Summarize(context.Background(), "ignored")
	if err != nil {
		t.Fatal(err)
	}
	if summary != "summary text" {
		t.Fatalf("summary = %q", summary)
	}
	if usage != (cakecli.Usage{}) {
		t.Fatalf("a file summarizer reported usage: %+v", usage)
	}
}

func TestCommandSummarizer(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("sh is unavailable")
	}
	summary, usage, err := CommandSummarizer{Command: "cat >/dev/null; printf 'from command'"}.Summarize(context.Background(), "transcript")
	if err != nil {
		t.Fatal(err)
	}
	if summary != "from command" {
		t.Fatalf("summary = %q", summary)
	}
	if usage != (cakecli.Usage{}) {
		t.Fatalf("a command summarizer reported usage: %+v", usage)
	}
}
