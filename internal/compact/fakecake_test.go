//go:build !windows

package compact

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/travisennis/cake-compact/internal/cakecli"
	"github.com/travisennis/cake-compact/internal/session"
)

const (
	fakeCakeEnable = "CAKE_COMPACT_FAKE"
	fakeCakeMode   = "CAKE_COMPACT_FAKE_MODE"
	fakePayloadOut = "CAKE_COMPACT_PAYLOAD_OUT"
	fakeCakeDelay  = "CAKE_COMPACT_FAKE_DELAY_MS"
)

// TestFakeCakeHelper is the offline stand-in for a Cake binary. The tests run
// it through a small shell wrapper so the payload and exit path cross a real
// process boundary, without a model call or a real Cake install.
func TestFakeCakeHelper(t *testing.T) {
	if os.Getenv(fakeCakeEnable) != "1" {
		return
	}
	os.Exit(fakeCakeMain(os.Args[1:]))
}

func fakeCakeMain(args []string) int {
	positional := make([]string, 0, len(args))
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
		}
	}
	if raw := os.Getenv(fakeCakeDelay); raw != "" {
		if milliseconds, err := strconv.Atoi(raw); err == nil && milliseconds > 0 {
			time.Sleep(time.Duration(milliseconds) * time.Millisecond)
		}
	}
	for _, arg := range positional {
		if arg == "replay" {
			fmt.Println(`{"type":"compaction"}`)
			return 0
		}
	}
	if len(positional) == 0 {
		for _, arg := range args {
			if arg == "--help" || arg == "-h" {
				fmt.Println("Usage: cake [OPTIONS]")
				fmt.Println("      --no-runtime-context  Omit the process-environment Developer message")
				return 0
			}
		}
	}
	if len(positional) < 2 || positional[0] != "sessions" || positional[1] != "checkpoint" {
		fmt.Fprintln(os.Stderr, "fake cake: unsupported invocation")
		return 2
	}
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			fmt.Println("Append one context-compaction checkpoint to an existing session")
			return 0
		}
	}
	if len(positional) < 3 {
		fmt.Fprintln(os.Stderr, "fake cake: missing session id")
		return 3
	}
	sessionID := positional[2]

	payloadBytes, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake cake: read stdin:", err)
		return 3
	}
	if out := os.Getenv(fakePayloadOut); out != "" {
		if err := os.WriteFile(out, payloadBytes, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "fake cake: write payload:", err)
			return 1
		}
	}

	switch os.Getenv(fakeCakeMode) {
	case "locked":
		message := fmt.Sprintf("Another cake invocation is currently writing to session %s. Wait for it to finish or run in a different directory.", sessionID)
		printFailure(message)
		fmt.Fprintln(os.Stderr, message)
		return 1
	case "reject":
		message := "checkpoint payload was rejected: checkpoint payload must shrink the context"
		printFailure(message)
		fmt.Fprintln(os.Stderr, message)
		return 3
	}

	return appendFakeCheckpoint(sessionID, payloadBytes)
}

func printFailure(message string) {
	document := map[string]any{
		"schema_version": 1,
		"command":        "sessions checkpoint",
		"status":         "error",
		"summary":        map[string]any{},
		"checks": []map[string]any{{
			"id":      "sessions.checkpoint_failed",
			"status":  "error",
			"message": message,
		}},
		"data": map[string]any{},
	}
	encoded, _ := json.Marshal(document)
	fmt.Println(string(encoded))
}

func appendFakeCheckpoint(sessionID string, payloadBytes []byte) int {
	decoder := json.NewDecoder(strings.NewReader(string(payloadBytes)))
	decoder.DisallowUnknownFields()
	var payload cakecli.CheckpointPayload
	if err := decoder.Decode(&payload); err != nil {
		message := "checkpoint payload is invalid: " + err.Error()
		printFailure(message)
		fmt.Fprintln(os.Stderr, message)
		return 3
	}
	if strings.TrimSpace(payload.Summary) == "" {
		printFailure("checkpoint payload summary must not be empty")
		return 3
	}
	if payload.EstimatedInput.AfterTokens >= payload.EstimatedInput.BeforeTokens {
		message := fmt.Sprintf(
			"checkpoint payload must shrink the context: after_tokens (%d) must be below before_tokens (%d)",
			payload.EstimatedInput.AfterTokens, payload.EstimatedInput.BeforeTokens,
		)
		printFailure(message)
		fmt.Fprintln(os.Stderr, message)
		return 3
	}

	dataDir := os.Getenv("CAKE_DATA_DIR")
	path := filepath.Join(dataDir, "sessions", sessionID+".jsonl")
	file, err := session.Load(path)
	if err != nil {
		printFailure("cannot read session: " + err.Error())
		return 1
	}
	boundary := file.ConversationItemCount()

	checkpointID, err := NewUUID()
	if err != nil {
		return 1
	}
	model := session.ModelIdentity{Model: file.MetaString("model"), ModelConfig: file.MetaString("model_config")}
	if payload.Model != nil {
		model = session.ModelIdentity{Model: payload.Model.Model, ModelConfig: payload.Model.ModelConfig}
	}
	taskID := payload.TaskID
	if taskID == "" {
		taskID = file.LatestTaskID()
	}

	checkpoint := session.Checkpoint{
		Type:                      session.TypeCompaction,
		CheckpointVersion:         1,
		CheckpointID:              checkpointID,
		SessionID:                 sessionID,
		TaskID:                    taskID,
		Timestamp:                 time.Now().UTC().Format(time.RFC3339Nano),
		Trigger:                   payload.Trigger,
		Summary:                   payload.Summary,
		Retained:                  payload.Retained,
		Model:                     model,
		SummaryInstructionVersion: payload.SummaryInstructionVersion,
		EstimatedInput: session.EstimatedInput{
			BeforeTokens: payload.EstimatedInput.BeforeTokens,
			AfterTokens:  payload.EstimatedInput.AfterTokens,
			Basis:        payload.EstimatedInput.Basis,
		},
	}
	line, err := json.Marshal(checkpoint)
	if err != nil {
		return 1
	}
	handle, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 1
	}
	if _, err := handle.Write(append(line, '\n')); err != nil {
		_ = handle.Close()
		return 1
	}
	if err := handle.Sync(); err != nil {
		_ = handle.Close()
		return 1
	}
	if err := handle.Close(); err != nil {
		return 1
	}

	document := map[string]any{
		"schema_version": 1,
		"command":        "sessions checkpoint",
		"status":         "ok",
		"summary": map[string]any{
			"session_id":     sessionID,
			"checkpoint_id":  checkpointID,
			"boundary":       boundary,
			"before_tokens":  payload.EstimatedInput.BeforeTokens,
			"after_tokens":   payload.EstimatedInput.AfterTokens,
			"retained_count": len(payload.Retained),
		},
		"checks": []any{},
		"data": map[string]any{
			"session_file": path,
			"basis":        payload.EstimatedInput.Basis,
		},
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return 1
	}
	fmt.Println(string(encoded))
	return 0
}

// writeFakeCake installs the wrapper script and enables the helper process.
func writeFakeCake(t *testing.T) string {
	t.Helper()
	binary, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fake-cake")
	script := fmt.Sprintf("#!/bin/sh\nexec '%s' -test.run='^TestFakeCakeHelper$' -- \"$@\"\n", binary)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeCakeEnable, "1")
	return path
}

// writeBrokenCake installs a binary that rejects every invocation, standing in
// for a Cake build without the checkpoint verb.
func writeBrokenCake(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "old-cake")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
