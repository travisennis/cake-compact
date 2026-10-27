// Package compact plans and commits context compaction for a Cake session.
//
// The proof-of-concept owner decides whether the last measured context crossed
// a threshold, selects a recent complete suffix, and summarizes the retired
// prefix with an external model call. It then commits the plan in one of two
// ways: in place through `cake sessions checkpoint` (the default), or, only
// when explicitly asked, as a new checkpoint-only session file (the minimal
// fork escape hatch). The in-place path never writes session JSONL itself.
package compact

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/travisennis/cake-compact/internal/cakecli"
	"github.com/travisennis/cake-compact/internal/session"
)

// Mode selects how a compaction plan is committed.
type Mode string

const (
	// ModeInPlace appends a checkpoint to the source session through Cake.
	ModeInPlace Mode = "in-place"
	// ModeFork writes a new checkpoint-only session file. It is an explicit
	// escape hatch, never an implicit fallback.
	ModeFork Mode = "fork"
)

// ErrModeRequired rejects a compactor that was not configured for either
// commit path, so the fork writer can never be reached implicitly.
var ErrModeRequired = fmt.Errorf("compaction mode is required: set ModeInPlace or ModeFork")

// MaxCheckpointRecordBytes is Cake's ceiling for one framed compaction record.
const MaxCheckpointRecordBytes = 8 * 1024 * 1024

// DefaultKeepTokens matches Cake's planned keep_recent_tokens default.
const DefaultKeepTokens = 20000

// Options controls one compaction plan.
type Options struct {
	ThresholdPercent    int
	ThresholdTokens     uint64
	Window              uint64
	KeepTokens          uint64
	MinReductionPercent int
	Force               bool
	DryRun              bool
	Trigger             string
	SummaryFile         string
	Instructions        string
	MaxTranscriptChars  int
}

func (options Options) withDefaults() Options {
	if options.ThresholdPercent == 0 {
		options.ThresholdPercent = 80
	}
	if options.MinReductionPercent == 0 {
		options.MinReductionPercent = 10
	}
	if options.MaxTranscriptChars == 0 {
		options.MaxTranscriptChars = 800000
	}
	if options.Trigger == "" {
		options.Trigger = "threshold"
	}
	return options
}

// Plan is the selected retirement boundary and the items on each side of it.
type Plan struct {
	File      *session.File
	Projected []session.Item
	Prefix    []session.Item
	Retained  []session.Item
	// Start is the index in Projected of the first retained tail item. The
	// owning user request is prepended to Retained and pinned when it sits
	// before Start.
	Start int
	// Anchor is the index in Projected of the real user request that owns the
	// retained tail, or -1 when no real user message exists at or before Start.
	// The synthetic historical summary is never an anchor.
	Anchor          int
	BeforeTokens    uint64
	Window          uint64
	Basis           string
	Trigger         string
	KeepTarget      uint64
	ThresholdTokens uint64
	RetainedTokens  uint64
}

// Decision says whether the plan should be committed and why.
type Decision struct {
	Compact bool
	Reason  string
}

// Result is the machine-readable outcome of a compaction attempt.
type Result struct {
	Compacted       bool   `json:"compacted"`
	Reason          string `json:"reason,omitempty"`
	SourceSessionID string `json:"source_session_id"`
	SessionID       string `json:"session_id,omitempty"`
	SessionFile     string `json:"session_file,omitempty"`
	InPlace         bool   `json:"in_place"`
	CheckpointID    string `json:"checkpoint_id,omitempty"`
	Boundary        int    `json:"boundary,omitempty"`
	Trigger         string `json:"trigger,omitempty"`
	Basis           string `json:"basis,omitempty"`
	BeforeTokens    uint64 `json:"before_tokens"`
	AfterTokens     uint64 `json:"after_tokens,omitempty"`
	Window          uint64 `json:"window,omitempty"`
	KeepTarget      uint64 `json:"keep_target,omitempty"`
	PrefixItems     int    `json:"prefix_items"`
	RetainedItems   int    `json:"retained_items"`
	RetainedCount   int    `json:"retained_count,omitempty"`
	SummaryChars    int    `json:"summary_chars,omitempty"`
	TranscriptChars int    `json:"transcript_chars,omitempty"`
	// SummaryUsage is the summarizer's provider-reported usage; nil when the
	// summarizer is not a model call or reported none. SummaryTimeMs is the
	// wall time spent in the summary call, zero when no call was made.
	SummaryUsage  *cakecli.Usage `json:"summary_usage,omitempty"`
	SummaryTimeMs int64          `json:"summary_time_ms,omitempty"`
}

// Analyze computes the effective context, applies the threshold, and selects a
// complete retained suffix. It always computes the plan when possible so
// `inspect` can show what a compaction would do.
func Analyze(file *session.File, options Options) (*Plan, Decision, error) {
	options = options.withDefaults()

	projected, err := file.Projected()
	if err != nil {
		return nil, Decision{}, err
	}
	before, window, basis := contextEstimate(file, options)
	threshold := thresholdTokens(options, window)
	keep := keepTarget(options, window)

	plan := &Plan{
		File:            file,
		Projected:       projected,
		BeforeTokens:    before,
		Window:          window,
		Basis:           basis,
		Trigger:         options.Trigger,
		KeepTarget:      keep,
		ThresholdTokens: threshold,
	}

	needed := options.Force
	reason := "forced"
	if !needed {
		switch {
		case threshold == 0:
			reason = "no context window or threshold; pass -window or -threshold-tokens, or -force"
		case before < threshold:
			reason = fmt.Sprintf("below threshold: %d < %d estimated input tokens", before, threshold)
		default:
			needed = true
			reason = fmt.Sprintf("threshold crossed: %d >= %d estimated input tokens", before, threshold)
		}
	}

	selection, selectErr := selectTail(projected, keep)
	if selectErr != nil {
		if needed {
			return plan, Decision{Compact: false, Reason: selectErr.Error()}, nil
		}
		return plan, Decision{Compact: false, Reason: reason + "; " + selectErr.Error()}, nil
	}

	plan.Start = selection.Start
	plan.Anchor = selection.Anchor
	plan.Prefix, plan.Retained, err = planSides(projected, selection)
	if err != nil {
		return nil, Decision{}, err
	}
	plan.RetainedTokens = session.EstimateTokens(plan.Retained)

	if len(plan.Prefix) == 0 {
		return plan, Decision{Compact: false, Reason: "nothing earlier than the retained tail to summarize"}, nil
	}
	if !needed {
		return plan, Decision{Compact: false, Reason: reason}, nil
	}
	if before > 0 && plan.RetainedTokens >= before && !options.Force {
		return plan, Decision{
			Compact: false,
			Reason:  fmt.Sprintf("retained tail alone does not reduce context: %d >= %d estimated input tokens", plan.RetainedTokens, before),
		}, nil
	}

	return plan, Decision{Compact: true, Reason: reason}, nil
}

func thresholdTokens(options Options, window uint64) uint64 {
	if options.ThresholdTokens > 0 {
		return options.ThresholdTokens
	}
	if window == 0 {
		return 0
	}
	return window * uint64(options.ThresholdPercent) / 100
}

func keepTarget(options Options, window uint64) uint64 {
	if options.KeepTokens > 0 {
		return options.KeepTokens
	}
	if window > 0 {
		quarter := window / 4
		if quarter == 0 {
			return 1
		}
		if quarter < DefaultKeepTokens {
			return quarter
		}
	}
	return DefaultKeepTokens
}

func contextEstimate(file *session.File, options Options) (before, window uint64, basis string) {
	usageIndex, usage := file.LastUsage()
	checkpointIndex := file.LatestCheckpointIndex()

	if usage != nil && usage.ContextWindow != nil && *usage.ContextWindow > 0 {
		window = uint64(*usage.ContextWindow)
	}
	if options.Window > 0 {
		window = options.Window
	}

	switch {
	case checkpointIndex >= 0 && checkpointIndex > usageIndex:
		checkpoint := file.Records[checkpointIndex].Checkpoint
		before = checkpoint.EstimatedInput.AfterTokens
		basis = "content_class"
		if before == 0 {
			if projected, err := file.Projected(); err == nil {
				before = session.EstimateTokens(projected)
			}
		}
		if later, err := file.ConversationItemsAfter(checkpointIndex); err == nil {
			before += session.EstimateTokens(later)
		}
	case usageIndex >= 0 && usage != nil:
		before = usage.Usage.InputTokens
		basis = "measured_usage"
		if later, err := file.ConversationItemsAfter(usageIndex); err == nil {
			before += session.EstimateTokens(later)
		}
	default:
		basis = "content_class"
		if projected, err := file.Projected(); err == nil {
			before = session.EstimateTokens(projected)
		}
	}
	return before, window, basis
}

// tailSelection is a bounded retained suffix plus the request that owns it.
type tailSelection struct {
	Start  int
	Anchor int
}

// selectTail chooses a suffix of at least keepTokens and then moves its start
// back to a complete provider boundary: no call/output pair may straddle the
// boundary, and a reasoning/call batch may not be cut. The owning real user
// message is reported separately so the caller can pin it exactly instead of
// forcing the whole exchange into the retained tail. A synthetic historical
// summary is never an anchor: it carries no user instruction to preserve.
//
// keepTokens is a target, not a ceiling. An indivisible item or the pinned
// request can push the retained estimate above it.
func selectTail(items []session.Item, keepTokens uint64) (tailSelection, error) {
	if len(items) == 0 {
		return tailSelection{}, fmt.Errorf("session has no conversation items")
	}
	if keepTokens == 0 {
		keepTokens = 1
	}

	start := 0
	var total uint64
	for i := len(items) - 1; i >= 0; i-- {
		total += session.EstimateItemTokens(items[i])
		start = i
		if total >= keepTokens {
			break
		}
	}

	for start > 0 && validateRetained(items[start:]) != nil {
		start--
	}
	if err := validateRetained(items[start:]); err != nil {
		return tailSelection{}, fmt.Errorf("cannot retain a complete provider tail: %w", err)
	}

	// Keep a reasoning/call batch whole. If the budget lands on a call or in
	// the middle of a contiguous reasoning run, move to the batch's first item.
batch:
	for start > 0 {
		switch {
		case items[start].Call != nil && items[start-1].Reasoning != nil:
			start--
		case items[start].Reasoning != nil && items[start-1].Reasoning != nil:
			start--
		default:
			break batch
		}
	}

	anchor := -1
	for i := start; i >= 0; i-- {
		if items[i].IsUserMessage() {
			anchor = i
			break
		}
	}
	return tailSelection{Start: start, Anchor: anchor}, nil
}

// planSides splits the projected context into the retired prefix and the
// retained tail. The owning user request between them is copied into the tail
// with a user_input pin, so the request stays exact while the work after it is
// summarized. The prefix keeps source order without duplicating that request.
func planSides(projected []session.Item, selection tailSelection) (prefix, retained []session.Item, err error) {
	start := selection.Start
	switch {
	case selection.Anchor == start:
		pinned, err := pinUserRequest(projected[start])
		if err != nil {
			return nil, nil, err
		}
		retained = make([]session.Item, 0, len(projected)-start)
		retained = append(retained, pinned)
		retained = append(retained, projected[start+1:]...)
		prefix = projected[:start]
	case selection.Anchor >= 0:
		pinned, err := pinUserRequest(projected[selection.Anchor])
		if err != nil {
			return nil, nil, err
		}
		prefix = make([]session.Item, 0, start-1)
		prefix = append(prefix, projected[:selection.Anchor]...)
		prefix = append(prefix, projected[selection.Anchor+1:start]...)
		retained = make([]session.Item, 0, len(projected)-start+1)
		retained = append(retained, pinned)
		retained = append(retained, projected[start:]...)
	default:
		prefix = projected[:start]
		retained = projected[start:]
	}
	return prefix, retained, nil
}

// pinUserRequest marks the retained copy of the owning user request. A pin is
// checkpoint-local metadata: Cake validates its placement and never emits it as
// a provider item, so the request stays exact without becoming a new
// instruction. Retaining the raw message also preserves images and ids.
func pinUserRequest(item session.Item) (session.Item, error) {
	if !item.IsUserMessage() {
		return session.Item{}, fmt.Errorf("cannot pin a non-user item as the owning request")
	}
	if len(item.Raw) == 0 {
		return session.Item{}, fmt.Errorf("user request has no raw payload")
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(item.Raw, &object); err != nil {
		return session.Item{}, fmt.Errorf("decode user request for pinning: %w", err)
	}
	object["pin"] = json.RawMessage(`{"kind":"user_input"}`)
	raw, err := json.Marshal(object)
	if err != nil {
		return session.Item{}, fmt.Errorf("encode pinned user request: %w", err)
	}
	item.Raw = raw
	return item, nil
}

// validateRetained mirrors Cake's checkpoint pairing rule: parallel calls may
// open together and close in any order, but a call and its output may not be
// separated by a message or reasoning item, and no call may remain open.
func validateRetained(items []session.Item) error {
	var open []string
	for _, item := range items {
		switch {
		case item.Call != nil:
			for _, callID := range open {
				if callID == item.Call.CallID {
					return fmt.Errorf("two unfinished function calls share call_id %q", item.Call.CallID)
				}
			}
			open = append(open, item.Call.CallID)
		case item.Output != nil:
			position := -1
			for i, callID := range open {
				if callID == item.Output.CallID {
					position = i
					break
				}
			}
			if position < 0 {
				return fmt.Errorf("function call output for call_id %q has no open call", item.Output.CallID)
			}
			open = append(open[:position], open[position+1:]...)
		case item.Message != nil:
			if len(open) > 0 {
				return fmt.Errorf("message appears while function call %q still awaits its output", open[0])
			}
			switch item.Message.Role {
			case "user", "assistant", "developer":
			default:
				return fmt.Errorf("unsupported retained message role %q", item.Message.Role)
			}
		case item.Reasoning != nil:
			if len(open) > 0 {
				return fmt.Errorf("reasoning appears while function call %q still awaits its output", open[0])
			}
		}
	}
	if len(open) > 0 {
		return fmt.Errorf("function call %q has no output", open[0])
	}
	return nil
}

// Compactor owns one compaction attempt.
type Compactor struct {
	Mode       Mode
	DataDir    string
	CakeBin    string
	Options    Options
	Summarizer Summarizer
	Verify     bool
	Now        func() time.Time
	NewID      func() (string, error)
	Logf       func(format string, args ...any)
}

func (c *Compactor) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// Compact plans, summarizes, and commits one compaction.
//
// The in-place path appends through `cake sessions checkpoint` and leaves the
// source file otherwise byte-identical. The fork path writes a new
// checkpoint-only session and validates it with `cake replay`.
func (c *Compactor) Compact(ctx context.Context, sourcePath string) (*Result, error) {
	if c.Mode != ModeInPlace && c.Mode != ModeFork {
		return nil, ErrModeRequired
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.Options = c.Options.withDefaults()
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.NewID == nil {
		c.NewID = NewUUID
	}

	file, err := session.Load(sourcePath)
	if err != nil {
		return nil, err
	}

	plan, decision, err := Analyze(file, c.Options)
	if err != nil {
		return nil, err
	}

	result := &Result{
		SourceSessionID: file.SessionID(),
		InPlace:         c.Mode == ModeInPlace,
		Trigger:         plan.Trigger,
		Basis:           plan.Basis,
		BeforeTokens:    plan.BeforeTokens,
		Window:          plan.Window,
		KeepTarget:      plan.KeepTarget,
		PrefixItems:     len(plan.Prefix),
		RetainedItems:   len(plan.Retained),
		AfterTokens:     plan.RetainedTokens,
	}
	if !decision.Compact {
		result.Reason = decision.Reason
		return result, nil
	}

	if c.Options.DryRun {
		result.Reason = "dry-run: no summary call made"
		if c.Options.SummaryFile != "" {
			summary, err := c.summarizeForResult(ctx, plan, result)
			if err != nil {
				return nil, err
			}
			result.AfterTokens = plan.RetainedTokens + session.EstimateTextTokens(summary)
			result.Reason = "dry-run: summary file applied, nothing written"
		}
		return result, nil
	}

	if c.Mode == ModeInPlace {
		// Fail before spending a model call on a binary that cannot commit.
		if err := c.requireCheckpointVerb(ctx); err != nil {
			return nil, err
		}
	}

	summary, err := c.summarizeForResult(ctx, plan, result)
	if err != nil {
		return nil, err
	}
	after := plan.RetainedTokens + session.EstimateTextTokens(summary)
	result.AfterTokens = after

	if plan.BeforeTokens == 0 {
		result.Reason = "cannot estimate the current input size; refusing to append a 0/0 estimate"
		return result, nil
	}
	if after >= plan.BeforeTokens {
		result.Reason = fmt.Sprintf("summary did not reduce context: %d >= %d estimated input tokens", after, plan.BeforeTokens)
		return result, nil
	}
	if !c.Options.Force && plan.BeforeTokens > 0 {
		minimum := plan.BeforeTokens * uint64(100-c.Options.MinReductionPercent) / 100
		if after > minimum {
			result.Reason = fmt.Sprintf(
				"reduction below the %d%% minimum: %d > %d estimated input tokens",
				c.Options.MinReductionPercent, after, minimum,
			)
			return result, nil
		}
	}

	switch c.Mode {
	case ModeInPlace:
		return c.appendInPlace(ctx, file, plan, decision, result, summary)
	default:
		return c.writeFork(ctx, file, plan, decision, result, summary, after)
	}
}

// requireCheckpointVerb turns a Cake build without the checkpoint verb into a
// clear failure. It never falls back to writing session JSONL.
func (c *Compactor) requireCheckpointVerb(ctx context.Context) error {
	if c.CakeBin == "" {
		return fmt.Errorf("in-place compaction requires a cake binary")
	}
	return cakecli.CheckpointAvailable(ctx, c.CakeBin)
}

// appendInPlace commits the plan through `cake sessions checkpoint` and
// reports the verb's own identity and estimates.
func (c *Compactor) appendInPlace(
	ctx context.Context,
	file *session.File,
	plan *Plan,
	decision Decision,
	result *Result,
	summary string,
) (*Result, error) {
	sessionID, err := c.inPlaceSessionID(file)
	if err != nil {
		return nil, err
	}

	payload, err := BuildCheckpointPayload(plan, summary, file)
	if err != nil {
		return nil, err
	}

	priorCheckpoints := 0
	for _, record := range file.Records {
		if record.Checkpoint != nil {
			priorCheckpoints++
		}
	}

	outcome, err := cakecli.Checkpoint(ctx, c.CakeBin, c.DataDir, sessionID, payload)
	if err != nil {
		// A cancelled child can still have completed its append: Cake writes
		// the record under the session lock and syncs it before exiting. Only
		// an interrupted invocation can land here after a valid append, so
		// check the file before reporting failure.
		if ctx.Err() != nil {
			if recovered, ok := recoverCompletedAppend(file.Path, summary, priorCheckpoints); ok {
				outcome = recovered
			} else {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	result.Compacted = true
	result.SessionID = outcome.SessionID
	result.SessionFile = outcome.SessionFile
	result.CheckpointID = outcome.CheckpointID
	result.Boundary = outcome.Boundary
	result.Trigger = plan.Trigger
	result.Basis = firstNonEmpty(outcome.Basis, plan.Basis)
	result.BeforeTokens = outcome.BeforeTokens
	result.AfterTokens = outcome.AfterTokens
	result.RetainedCount = outcome.RetainedCount
	result.Reason = decision.Reason
	if expected := file.ConversationItemCount(); outcome.Boundary != expected {
		// The plan was read before the append lock was taken. Cake still
		// appended at the real boundary, but another writer may have added
		// records after our snapshot.
		c.logf("warning: session grew between read and append (boundary %d, read %d)", outcome.Boundary, expected)
	}
	c.logf("compacted %s in place at boundary %d (%d -> %d estimated input tokens)", sessionID, outcome.Boundary, result.BeforeTokens, result.AfterTokens)
	return result, nil
}

// inPlaceSessionID verifies that the source file is the session file Cake will
// resolve for its UUID. An arbitrary path cannot be appended in place.
func (c *Compactor) inPlaceSessionID(file *session.File) (string, error) {
	sessionID := file.SessionID()
	expected := filepath.Join(cakecli.SessionsDir(c.DataDir), sessionID+".jsonl")
	actual, err := filepath.Abs(file.Path)
	if err != nil {
		return "", err
	}
	expected, err = filepath.Abs(expected)
	if err != nil {
		return "", err
	}
	if filepath.Clean(actual) != filepath.Clean(expected) {
		return "", fmt.Errorf(
			"in-place compaction requires %s; %s is not a Cake session file in the data root (use -fork to write a new session)",
			expected, actual,
		)
	}
	return sessionID, nil
}

// recoverCompletedAppend reports whether an interrupted append nevertheless
// committed the checkpoint identified by its summary.
func recoverCompletedAppend(path, summary string, priorCheckpoints int) (*cakecli.CheckpointOutcome, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	file, err := session.LoadBytes(path, data)
	if err != nil {
		return nil, false
	}

	count := 0
	var latest *session.Checkpoint
	for _, record := range file.Records {
		if record.Checkpoint != nil {
			count++
			latest = record.Checkpoint
		}
	}
	if count <= priorCheckpoints || latest == nil {
		return nil, false
	}
	// Cake appends the checkpoint last. If another record followed it, the
	// boundary is no longer the position of this append.
	if file.LatestCheckpointIndex() != len(file.Records)-1 {
		return nil, false
	}
	if strings.TrimSpace(latest.Summary) != strings.TrimSpace(summary) {
		return nil, false
	}

	return &cakecli.CheckpointOutcome{
		SessionID:     file.SessionID(),
		CheckpointID:  latest.CheckpointID,
		Boundary:      file.ConversationItemCount(),
		BeforeTokens:  latest.EstimatedInput.BeforeTokens,
		AfterTokens:   latest.EstimatedInput.AfterTokens,
		RetainedCount: len(latest.Retained),
		Basis:         latest.EstimatedInput.Basis,
		SessionFile:   path,
	}, true
}

// writeFork writes the plan as a new checkpoint-only session and validates it
// with `cake replay`. This is the explicit escape hatch, not a fallback.
func (c *Compactor) writeFork(
	ctx context.Context,
	file *session.File,
	plan *Plan,
	decision Decision,
	result *Result,
	summary string,
	after uint64,
) (*Result, error) {
	newID, err := c.resolveNewID()
	if err != nil {
		return nil, err
	}
	checkpoint, line, err := BuildCheckpoint(plan, summary, newID, file.LatestTaskID(), c.Now())
	if err != nil {
		return nil, err
	}
	path := filepath.Join(cakecli.SessionsDir(c.DataDir), newID+".jsonl")
	if err := writeSession(path, file.Meta, newID, line, c.Now()); err != nil {
		return nil, err
	}

	if c.Verify {
		if _, err := cakecli.Replay(ctx, c.CakeBin, c.DataDir, newID); err != nil {
			_ = os.Remove(path)
			return nil, fmt.Errorf("compacted session failed validation and was removed: %w", err)
		}
	}

	result.Compacted = true
	result.SessionID = newID
	result.SessionFile = path
	result.CheckpointID = checkpoint.CheckpointID
	result.RetainedCount = len(checkpoint.Retained)
	result.Reason = decision.Reason
	c.logf("compacted %s -> %s (%d -> %d estimated input tokens)", file.SessionID(), newID, plan.BeforeTokens, after)
	return result, nil
}

// BuildCheckpointPayload renders the caller-owned half of the checkpoint
// protocol. Cake owns the envelope, ids, timestamp, and boundary.
func BuildCheckpointPayload(plan *Plan, summary string, file *session.File) (cakecli.CheckpointPayload, error) {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return cakecli.CheckpointPayload{}, fmt.Errorf("checkpoint summary is empty")
	}
	trigger, err := normalizeTrigger(plan.Trigger)
	if err != nil {
		return cakecli.CheckpointPayload{}, err
	}
	if plan.BeforeTokens == 0 {
		return cakecli.CheckpointPayload{}, fmt.Errorf("checkpoint needs a nonzero before_tokens estimate")
	}
	after := plan.RetainedTokens + session.EstimateTextTokens(summary)
	if after >= plan.BeforeTokens {
		return cakecli.CheckpointPayload{}, fmt.Errorf(
			"checkpoint must shrink the context: after_tokens (%d) must be below before_tokens (%d)",
			after, plan.BeforeTokens,
		)
	}

	retained, err := retainedPayload(plan)
	if err != nil {
		return cakecli.CheckpointPayload{}, err
	}

	payload := cakecli.CheckpointPayload{
		Trigger:  trigger,
		Summary:  summary,
		Retained: retained,
		TaskID:   file.LatestTaskID(),
		EstimatedInput: cakecli.CheckpointEstimate{
			BeforeTokens: plan.BeforeTokens,
			AfterTokens:  after,
			Basis:        plan.Basis,
		},
		SummaryInstructionVersion: SummaryInstructionVersion,
	}
	if model := file.MetaString("model"); model != "" {
		payload.Model = &cakecli.CheckpointModel{
			Model:       model,
			ModelConfig: file.MetaString("model_config"),
		}
	}
	return payload, nil
}

func retainedPayload(plan *Plan) ([]json.RawMessage, error) {
	retained := make([]json.RawMessage, 0, len(plan.Retained))
	for _, item := range plan.Retained {
		if item.Synthetic {
			return nil, fmt.Errorf("the historical summary item cannot be retained")
		}
		if len(item.Raw) == 0 {
			return nil, fmt.Errorf("retained item has no raw payload")
		}
		retained = append(retained, item.Raw)
	}
	return retained, nil
}

func normalizeTrigger(trigger string) (string, error) {
	switch trigger {
	case "manual", "threshold", "overflow":
		return trigger, nil
	case "":
		return "manual", nil
	default:
		return "", fmt.Errorf("unsupported compaction trigger %q", trigger)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (c *Compactor) resolveNewID() (string, error) {
	sessionsDir := cakecli.SessionsDir(c.DataDir)
	for range 5 {
		id, err := c.NewID()
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(filepath.Join(sessionsDir, id+".jsonl")); os.IsNotExist(err) {
			return id, nil
		}
	}
	return "", fmt.Errorf("cannot allocate an unused session id")
}

// summarizeForResult runs the summary call and records its provider usage and
// wall time on the result. Cost is evidence for the qualification phase, so it
// belongs in the machine-readable document rather than only in a log line.
func (c *Compactor) summarizeForResult(ctx context.Context, plan *Plan, result *Result) (string, error) {
	started := c.Now()
	summary, usage, transcriptChars, err := c.summarize(ctx, plan)
	result.SummaryTimeMs = c.Now().Sub(started).Milliseconds()
	if usage != (cakecli.Usage{}) {
		result.SummaryUsage = &usage
	}
	if err != nil {
		return "", err
	}
	result.SummaryChars = len(summary)
	result.TranscriptChars = transcriptChars
	return summary, nil
}

func (c *Compactor) summarize(ctx context.Context, plan *Plan) (summary string, usage cakecli.Usage, transcriptChars int, err error) {
	if c.Options.SummaryFile != "" {
		data, err := os.ReadFile(c.Options.SummaryFile)
		if err != nil {
			return "", cakecli.Usage{}, 0, fmt.Errorf("read summary file: %w", err)
		}
		summary = strings.TrimSpace(string(data))
		if summary == "" {
			return "", cakecli.Usage{}, 0, fmt.Errorf("summary file %s is empty", c.Options.SummaryFile)
		}
		return summary, cakecli.Usage{}, 0, nil
	}
	if c.Summarizer == nil {
		return "", cakecli.Usage{}, 0, fmt.Errorf("no summarizer configured")
	}
	transcript, err := BuildTranscript(plan.Prefix, c.Options.MaxTranscriptChars)
	if err != nil {
		return "", cakecli.Usage{}, 0, err
	}
	summary, usage, err = c.Summarizer.Summarize(ctx, transcript)
	if err != nil {
		return "", cakecli.Usage{}, 0, err
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return "", cakecli.Usage{}, 0, fmt.Errorf("summarizer returned an empty summary")
	}
	return summary, usage, len(transcript), nil
}

// BuildCheckpoint renders the M1 compaction record. The returned bytes are the
// exact framed JSONL record, including its trailing newline.
func BuildCheckpoint(plan *Plan, summary, sessionID, taskID string, now time.Time) (*session.Checkpoint, []byte, error) {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return nil, nil, fmt.Errorf("checkpoint summary is empty")
	}
	if sessionID == "" {
		return nil, nil, fmt.Errorf("checkpoint session id is empty")
	}
	if taskID == "" {
		id, err := NewUUID()
		if err != nil {
			return nil, nil, err
		}
		taskID = id
	}

	retained, err := retainedPayload(plan)
	if err != nil {
		return nil, nil, err
	}

	model := session.ModelIdentity{
		Model:       plan.File.MetaString("model"),
		ModelConfig: plan.File.MetaString("model_config"),
	}
	if model.Model == "" {
		model.Model = "unknown"
	}

	checkpointID, err := NewUUID()
	if err != nil {
		return nil, nil, err
	}

	checkpoint := &session.Checkpoint{
		Type:                      session.TypeCompaction,
		CheckpointVersion:         1,
		CheckpointID:              checkpointID,
		SessionID:                 sessionID,
		TaskID:                    taskID,
		Timestamp:                 now.UTC().Format(time.RFC3339Nano),
		Trigger:                   plan.Trigger,
		Summary:                   summary,
		Retained:                  retained,
		Model:                     model,
		SummaryInstructionVersion: SummaryInstructionVersion,
		EstimatedInput: session.EstimatedInput{
			BeforeTokens: plan.BeforeTokens,
			AfterTokens:  plan.RetainedTokens + session.EstimateTextTokens(summary),
			Basis:        plan.Basis,
		},
	}

	line, err := json.Marshal(checkpoint)
	if err != nil {
		return nil, nil, fmt.Errorf("serialize checkpoint: %w", err)
	}
	line = append(line, '\n')
	if len(line) > MaxCheckpointRecordBytes {
		return nil, nil, fmt.Errorf(
			"checkpoint record is %d bytes; Cake's limit is %d bytes",
			len(line), MaxCheckpointRecordBytes,
		)
	}
	return checkpoint, line, nil
}

func writeSession(path string, meta map[string]json.RawMessage, sessionID string, checkpointLine []byte, now time.Time) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create sessions directory: %w", err)
	}

	copied := make(map[string]json.RawMessage, len(meta))
	for key, value := range meta {
		copied[key] = value
	}
	copied["session_id"] = mustJSON(sessionID)
	copied["timestamp"] = mustJSON(now.UTC().Format(time.RFC3339Nano))
	metaLine, err := json.Marshal(copied)
	if err != nil {
		return fmt.Errorf("serialize session metadata: %w", err)
	}

	temp, err := os.CreateTemp(dir, ".cake-compact-*")
	if err != nil {
		return fmt.Errorf("create compacted session: %w", err)
	}
	tempPath := temp.Name()
	cleanup := func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}

	if _, err := temp.Write(append(metaLine, '\n')); err != nil {
		cleanup()
		return fmt.Errorf("write compacted session metadata: %w", err)
	}
	if _, err := temp.Write(checkpointLine); err != nil {
		cleanup()
		return fmt.Errorf("write compacted checkpoint: %w", err)
	}
	if err := temp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync compacted session: %w", err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("close compacted session: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("install compacted session: %w", err)
	}
	syncDir(dir)
	return nil
}

func syncDir(dir string) {
	handle, err := os.Open(dir)
	if err != nil {
		return
	}
	defer handle.Close()
	_ = handle.Sync()
}

func mustJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}
