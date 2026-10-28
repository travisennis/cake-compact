//go:build !windows

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/travisennis/cake-compact/internal/session"
)

const (
	fakeCakeEnable  = "CAKE_COMPACT_FAKE"
	fakeCakeSession = "CAKE_COMPACT_FAKE_SESSION"
)

// TestFakeCakeHelper stands in for both a cake turn and the checkpoint verb, so
// the CLI tests cross a real process boundary without a model call.
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

	if len(positional) == 0 {
		for _, arg := range args {
			if arg == "--help" || arg == "-h" {
				fmt.Println("Usage: cake [OPTIONS]")
				fmt.Println("      --no-runtime-context  Omit the process-environment Developer message")
				return 0
			}
		}
	}

	if len(positional) >= 2 && positional[0] == "sessions" && positional[1] == "checkpoint" {
		for _, arg := range args {
			if arg == "--help" || arg == "-h" {
				fmt.Println("Append one context-compaction checkpoint; summary_usage and summary_time_ms are accepted")
				return 0
			}
		}
		if len(positional) < 3 {
			return 3
		}
		return fakeCakeCheckpoint(positional[2])
	}

	sessionID := os.Getenv(fakeCakeSession)
	encoded, err := json.Marshal(map[string]any{"session_id": sessionID, "result": "done"})
	if err != nil {
		return 1
	}
	fmt.Println(string(encoded))
	return 0
}

func fakeCakeCheckpoint(sessionID string) int {
	payloadBytes, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake cake: read stdin:", err)
		return 3
	}
	var payload struct {
		Trigger                   string                `json:"trigger"`
		Summary                   string                `json:"summary"`
		Retained                  []json.RawMessage     `json:"retained"`
		EstimatedInput            map[string]any        `json:"estimated_input"`
		SummaryInstructionVersion int                   `json:"summary_instruction_version"`
		SummaryUsage              *session.SummaryUsage `json:"summary_usage,omitempty"`
		SummaryTimeMs             int64                 `json:"summary_time_ms,omitempty"`
		Model                     *struct {
			Model       string `json:"model"`
			ModelConfig string `json:"model_config"`
		} `json:"model"`
		TaskID string `json:"task_id"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(payloadBytes)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		fmt.Fprintln(os.Stderr, "fake cake: invalid payload:", err)
		return 3
	}

	path := filepath.Join(os.Getenv("CAKE_DATA_DIR"), "sessions", sessionID+".jsonl")
	file, err := session.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake cake: load session:", err)
		return 1
	}
	boundary := file.ConversationItemCount()

	before, _ := payload.EstimatedInput["before_tokens"].(float64)
	after, _ := payload.EstimatedInput["after_tokens"].(float64)
	basis, _ := payload.EstimatedInput["basis"].(string)
	model := session.ModelIdentity{Model: file.MetaString("model"), ModelConfig: file.MetaString("model_config")}
	if payload.Model != nil {
		model = session.ModelIdentity{Model: payload.Model.Model, ModelConfig: payload.Model.ModelConfig}
	}
	taskID := payload.TaskID
	if taskID == "" {
		taskID = file.LatestTaskID()
	}

	checkpointID := fmt.Sprintf("cccccccc-cccc-4ccc-8ccc-%012d", time.Now().UnixNano()%1_000_000_000_000)
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
			BeforeTokens: uint64(before),
			AfterTokens:  uint64(after),
			Basis:        basis,
		},
		SummaryUsage:  payload.SummaryUsage,
		SummaryTimeMs: payload.SummaryTimeMs,
	}
	line, err := json.Marshal(checkpoint)
	if err != nil {
		return 1
	}
	handle, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake cake: open session:", err)
		return 1
	}
	if _, err := handle.Write(append(line, '\n')); err != nil {
		_ = handle.Close()
		return 1
	}
	if err := handle.Close(); err != nil {
		return 1
	}

	document, err := json.Marshal(map[string]any{
		"schema_version": 1,
		"command":        "sessions checkpoint",
		"status":         "ok",
		"summary": map[string]any{
			"session_id":     sessionID,
			"checkpoint_id":  checkpointID,
			"boundary":       boundary,
			"before_tokens":  uint64(before),
			"after_tokens":   uint64(after),
			"retained_count": len(payload.Retained),
		},
		"checks": []any{},
		"data": map[string]any{
			"session_file": path,
			"basis":        basis,
		},
	})
	if err != nil {
		return 1
	}
	fmt.Println(string(document))
	return 0
}

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
	t.Setenv(fakeCakeSession, testSourceID)
	return path
}
