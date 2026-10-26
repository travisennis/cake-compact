// Checkpoint append: the wire contract between this tool and
// `cake sessions checkpoint <UUID> [--json]`.
//
// The caller owns the selection, the summary, the retained tail, the trigger,
// and the token estimates. Cake owns the record envelope, the checkpoint and
// session identities, the timestamp, the boundary, the session lock,
// validation, the append, and its sync. This file only speaks that protocol: it
// never writes a session file itself.

package cakecli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ErrCheckpointUnavailable marks a Cake binary that does not implement the
// checkpoint verb. Callers must not fall back to writing session JSONL.
var ErrCheckpointUnavailable = errors.New("cake sessions checkpoint is unavailable; upgrade cake")

// CheckpointPayload is the caller-owned part of one checkpoint append.
//
// Field names and the set of fields are the protocol: Cake rejects an unknown
// field and any field the record envelope owns (type, checkpoint_version,
// checkpoint_id, session_id, timestamp, boundary).
type CheckpointPayload struct {
	Trigger                   string             `json:"trigger"`
	Summary                   string             `json:"summary"`
	Retained                  []json.RawMessage  `json:"retained"`
	EstimatedInput            CheckpointEstimate `json:"estimated_input"`
	SummaryInstructionVersion int                `json:"summary_instruction_version"`
	Model                     *CheckpointModel   `json:"model,omitempty"`
	TaskID                    string             `json:"task_id,omitempty"`
}

// CheckpointEstimate is the caller's before/after estimate for one append.
type CheckpointEstimate struct {
	BeforeTokens uint64 `json:"before_tokens"`
	AfterTokens  uint64 `json:"after_tokens"`
	Basis        string `json:"basis"`
}

// CheckpointModel is the optional model identity. When it is absent Cake uses
// the session's recorded identity.
type CheckpointModel struct {
	Model       string `json:"model"`
	ModelConfig string `json:"model_config,omitempty"`
}

// CheckpointOutcome is the subset of the verb's diagnostic document this tool
// reports. Summary carries the identity and estimate fields; Data carries the
// file and basis.
type CheckpointOutcome struct {
	SessionID     string
	CheckpointID  string
	Boundary      int
	BeforeTokens  uint64
	AfterTokens   uint64
	RetainedCount int
	Basis         string
	SessionFile   string
}

// CheckpointFailure is one refused or failed append. Class follows the verb's
// documented exit classes: input, locked, operational, cancelled, or start.
type CheckpointFailure struct {
	Class    string
	ExitCode int
	Message  string
	Checks   []DiagnosticCheck
}

// Error renders the exit class and the verb's own diagnostic.
func (f *CheckpointFailure) Error() string {
	detail := strings.TrimSpace(f.Message)
	if detail == "" {
		detail = "no diagnostic"
	}
	if f.ExitCode < 0 {
		return fmt.Sprintf("cake sessions checkpoint failed (%s): %s", f.Class, detail)
	}
	return fmt.Sprintf("cake sessions checkpoint failed (exit %d, %s): %s", f.ExitCode, f.Class, detail)
}

// VerbUnavailableError reports a binary that cannot append checkpoints.
type VerbUnavailableError struct {
	Binary string
	Detail string
}

// Error includes the upgrade instruction so the operator sees the fix.
func (e *VerbUnavailableError) Error() string {
	detail := strings.TrimSpace(e.Detail)
	if detail == "" {
		detail = "the command is not implemented"
	}
	return fmt.Sprintf("%s (%s: %s)", ErrCheckpointUnavailable.Error(), e.Binary, detail)
}

// Unwrap lets callers test with errors.Is(err, ErrCheckpointUnavailable).
func (e *VerbUnavailableError) Unwrap() error { return ErrCheckpointUnavailable }

// DiagnosticCheck is one finding in the diagnostic document.
type DiagnosticCheck struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

type checkpointSummary struct {
	SessionID     string `json:"session_id"`
	CheckpointID  string `json:"checkpoint_id"`
	Boundary      int    `json:"boundary"`
	BeforeTokens  uint64 `json:"before_tokens"`
	AfterTokens   uint64 `json:"after_tokens"`
	RetainedCount int    `json:"retained_count"`
}

type checkpointData struct {
	SessionFile string `json:"session_file"`
	Basis       string `json:"basis"`
}

type checkpointDocument struct {
	SchemaVersion int               `json:"schema_version"`
	Command       string            `json:"command"`
	Status        string            `json:"status"`
	Summary       checkpointSummary `json:"summary"`
	Checks        []DiagnosticCheck `json:"checks"`
	Data          checkpointData    `json:"data"`
}

// CheckpointAvailable reports whether the Cake binary implements
// `sessions checkpoint`. It asks for the subcommand help, which needs no
// session, no payload, and no model.
func CheckpointAvailable(ctx context.Context, binary string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("cake sessions checkpoint check cancelled: %w", err)
	}
	cmd := exec.CommandContext(ctx, binary, "sessions", "checkpoint", "--help")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("cake sessions checkpoint check cancelled: %w", ctxErr)
		}
		detail := firstNonEmpty(stderr.String(), stdout.String(), err.Error())
		return &VerbUnavailableError{Binary: binary, Detail: detail}
	}
	return nil
}

// Checkpoint pipes one payload to `cake sessions checkpoint <UUID> --json` and
// parses the diagnostic document. It returns an outcome only for a successful
// append; every failure leaves the session untouched and is reported through
// *CheckpointFailure (or *VerbUnavailableError for a binary without the verb).
func Checkpoint(ctx context.Context, binary, dataDir, sessionID string, payload CheckpointPayload) (*CheckpointOutcome, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode checkpoint payload: %w", err)
	}
	body = append(body, '\n')

	cmd := exec.CommandContext(ctx, binary, "sessions", "checkpoint", sessionID, "--json")
	cmd.Env = environmentWith("CAKE_DATA_DIR", dataDir)
	cmd.Stdin = bytes.NewReader(body)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if runErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, &CheckpointFailure{
				Class:    "cancelled",
				ExitCode: -1,
				Message:  firstNonEmpty(stderr.String(), stdout.String(), ctxErr.Error()),
			}
		}
		document, _ := parseCheckpointDocument(stdout.Bytes())
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		detail := firstNonEmpty(
			stderr.String(),
			document.failureMessage(),
			stdout.String(),
			runErr.Error(),
		)
		return nil, &CheckpointFailure{
			Class:    failureClass(exitCode, detail),
			ExitCode: exitCode,
			Message:  detail,
			Checks:   document.Checks,
		}
	}

	document, err := parseCheckpointDocument(stdout.Bytes())
	if err != nil {
		return nil, &CheckpointFailure{
			Class:    "protocol",
			ExitCode: -1,
			Message:  fmt.Sprintf("could not parse the checkpoint diagnostic document: %v: %s", err, firstNonEmpty(stdout.String(), stderr.String())),
		}
	}
	if document.Status == "error" {
		return nil, &CheckpointFailure{
			Class:    "operational",
			ExitCode: -1,
			Message:  document.failureMessage(),
			Checks:   document.Checks,
		}
	}
	if document.Summary.SessionID == "" || document.Summary.CheckpointID == "" {
		return nil, &CheckpointFailure{
			Class:    "protocol",
			ExitCode: -1,
			Message:  "the checkpoint diagnostic document is missing the session or checkpoint id",
		}
	}
	if document.Summary.SessionID != sessionID {
		return nil, &CheckpointFailure{
			Class:    "protocol",
			ExitCode: -1,
			Message:  fmt.Sprintf("the checkpoint was appended to session %s, not %s", document.Summary.SessionID, sessionID),
		}
	}

	return &CheckpointOutcome{
		SessionID:     document.Summary.SessionID,
		CheckpointID:  document.Summary.CheckpointID,
		Boundary:      document.Summary.Boundary,
		BeforeTokens:  document.Summary.BeforeTokens,
		AfterTokens:   document.Summary.AfterTokens,
		RetainedCount: document.Summary.RetainedCount,
		Basis:         document.Data.Basis,
		SessionFile:   document.Data.SessionFile,
	}, nil
}

func parseCheckpointDocument(stdout []byte) (checkpointDocument, error) {
	var document checkpointDocument
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 {
		return document, errors.New("no document on stdout")
	}
	if err := json.Unmarshal(trimmed, &document); err != nil {
		return checkpointDocument{}, err
	}
	return document, nil
}

// failureMessage returns the first error finding, which is the same text Cake
// writes to stderr.
func (d checkpointDocument) failureMessage() string {
	for _, check := range d.Checks {
		if check.Status == "error" && strings.TrimSpace(check.Message) != "" {
			return check.Message
		}
	}
	return ""
}

// failureClass maps the verb's documented exit classes and the lock diagnostic
// to a bounded class this tool can report.
func failureClass(exitCode int, detail string) string {
	switch exitCode {
	case 3:
		return "input"
	case 1:
		if strings.Contains(detail, "currently writing to session") {
			return "locked"
		}
		return "operational"
	case -1:
		return "start"
	default:
		return "operational"
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
