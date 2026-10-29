// Package tree renders a session's raw records and its effective provider
// context as a read-only tree. The effective side mirrors Cake's projector:
// the newest checkpoint supplies a synthetic historical summary and retained
// tail, followed by any conversation records appended after it. When the
// caller has a compaction plan, the same view also shows the retired prefix
// and the retained tail on either side of the selected boundary.
package tree

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/travisennis/cake-compact/internal/compact"
	"github.com/travisennis/cake-compact/internal/session"
)

const (
	detailWidth     = 58
	minKindWidth    = 18
	minRawTypeWidth = 20
	callIDWidth     = 12
)

// View is one read-only tree rendering. Plan and Decision are optional; without
// them Render still shows the raw records and the effective conversation, but
// not the retirement split.
type View struct {
	File     *session.File
	Plan     *compact.Plan
	Decision compact.Decision
}

// Render writes the raw-record list followed by the effective-context view and
// the compaction plan. It never writes to the session or calls a model.
func Render(w io.Writer, view View) error {
	if view.File == nil {
		return fmt.Errorf("tree: no session file")
	}
	p := &printer{w: w}
	p.printf("session: %s\n", view.File.SessionID())
	p.printf("file: %s\n\n", view.File.Path)
	renderRaw(p, view.File)
	p.printf("\n")
	renderEffective(p, view)
	return p.err
}

type printer struct {
	w   io.Writer
	err error
}

func (p *printer) printf(format string, args ...any) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprintf(p.w, format, args...)
}

type row struct {
	index  int
	kind   string
	detail string
	tokens uint64
	marker string
}

func renderRaw(p *printer, file *session.File) {
	conversation, checkpoints := 0, 0
	typeWidth := minRawTypeWidth
	for _, record := range file.Records {
		if _, ok := session.ItemFromRecord(record); ok {
			conversation++
		}
		if record.Checkpoint != nil {
			checkpoints++
		}
		if len(record.Type) > typeWidth {
			typeWidth = len(record.Type)
		}
	}
	metadata := len(file.Records) - conversation - checkpoints

	recordWord := "records"
	if len(file.Records) == 1 {
		recordWord = "record"
	}
	checkpointWord := "checkpoints"
	if checkpoints == 1 {
		checkpointWord = "checkpoint"
	}

	p.printf("RAW FILE\n")
	p.printf("  %d %s: %d conversation, %d metadata, %d %s\n",
		len(file.Records), recordWord, conversation, metadata, checkpoints, checkpointWord)
	for _, warning := range file.Warnings {
		p.printf("  warning: %s\n", warning)
	}
	for _, record := range file.Records {
		kind, detail := rawLabel(file, record)
		p.printf("  line %2d  %-*s %s\n", record.Index+1, typeWidth, kind, truncate(detail, detailWidth))
	}
}

func rawLabel(file *session.File, record session.Record) (string, string) {
	switch {
	case record.Type == session.TypeSessionMeta:
		return record.Type, "session " + file.SessionID()
	case record.TaskID != "":
		return record.Type, "task " + record.TaskID
	case record.Usage != nil:
		detail := fmt.Sprintf("input %d tokens", record.Usage.Usage.InputTokens)
		if record.Usage.ContextWindow != nil && *record.Usage.ContextWindow > 0 {
			detail += fmt.Sprintf(", window %d", *record.Usage.ContextWindow)
		}
		return record.Type, detail
	case record.Checkpoint != nil:
		checkpoint := record.Checkpoint
		detail := fmt.Sprintf("checkpoint %s boundary %d",
			shortCheckpointID(checkpoint.CheckpointID), checkpointBoundary(file, record.Index))
		if len(checkpoint.Retained) > 0 {
			detail += fmt.Sprintf(", %d retained", len(checkpoint.Retained))
		}
		return record.Type, detail
	}
	if item, ok := session.ItemFromRecord(record); ok {
		return record.Type, rawItemDetail(item)
	}
	return record.Type, metadataDetail(record)
}

func rawItemDetail(item session.Item) string {
	_, detail := itemLabel(item)
	switch {
	case item.Message != nil:
		return strings.TrimSpace(strings.ToUpper(item.Message.Role) + " " + detail)
	case item.Call != nil:
		return strings.TrimSpace(item.Call.Name + " " + detail)
	case item.Output != nil:
		return strings.TrimSpace(shortID(item.Output.CallID) + " " + detail)
	default:
		return detail
	}
}

func shortCheckpointID(checkpointID string) string {
	if len(checkpointID) <= 12 {
		return checkpointID
	}
	if dash := strings.IndexByte(checkpointID, '-'); dash > 0 {
		return checkpointID[:dash]
	}
	return checkpointID[:8]
}

// checkpointBoundary counts provider-visible conversation items before the
// checkpoint record. Cake derives the same boundary when it appends the
// checkpoint; it is not stored as a field.
func checkpointBoundary(file *session.File, recordIndex int) int {
	count := 0
	for _, record := range file.Records {
		if record.Index >= recordIndex {
			break
		}
		if _, ok := session.ItemFromRecord(record); ok {
			count++
		}
	}
	return count
}

func metadataDetail(record session.Record) string {
	var fields struct {
		SessionID     string `json:"session_id"`
		Role          string `json:"role"`
		Content       string `json:"content"`
		Event         string `json:"event"`
		Name          string `json:"name"`
		Subtype       string `json:"subtype"`
		IsError       bool   `json:"is_error"`
		FormatVersion int    `json:"format_version"`
	}
	if err := json.Unmarshal(record.Raw, &fields); err != nil {
		return ""
	}
	switch record.Type {
	case "prompt_context":
		if fields.Role != "" {
			return fmt.Sprintf("%s prompt, %d chars", fields.Role, len(fields.Content))
		}
	case "skill_activated":
		if fields.Name != "" {
			return "skill " + fields.Name
		}
	case "hook_event":
		if fields.Event != "" {
			return "event " + fields.Event
		}
	case "task_complete", "result":
		detail := fields.Subtype
		if detail == "" {
			detail = "complete"
		}
		if fields.IsError {
			detail += ", error"
		}
		return detail
	case "session_start", "init":
		detail := fmt.Sprintf("format %d", fields.FormatVersion)
		if fields.SessionID != "" {
			detail += ", session " + fields.SessionID
		}
		return detail
	}
	if fields.SessionID != "" {
		return "session " + fields.SessionID
	}
	return ""
}

func renderEffective(p *printer, view View) {
	file := view.File
	plan := view.Plan

	p.printf("EFFECTIVE CONTEXT (what the next model call sees)\n")
	checkpointIndex := file.LatestCheckpointIndex()
	if checkpointIndex < 0 {
		p.printf("  no checkpoint; raw conversation only\n")
	} else {
		checkpoint := file.Records[checkpointIndex].Checkpoint
		later, err := file.ConversationItemsAfter(checkpointIndex)
		if err != nil {
			p.printf("  projected from checkpoint %s\n", checkpoint.CheckpointID)
		} else {
			p.printf("  projected from checkpoint %s: 1 summary + %d retained + %d later\n",
				checkpoint.CheckpointID, len(checkpoint.Retained), len(later))
		}
	}

	if plan == nil {
		projected, err := file.Projected()
		if err != nil {
			p.printf("  cannot project the session: %v\n", err)
			return
		}
		if len(projected) == 0 {
			p.printf("  no conversation items\n")
			return
		}
		p.printf("\n")
		rows := itemRows(projected, func(index int) int { return index }, "")
		renderRows(p, rows)
		writeItemTotal(p, len(projected), session.EstimateTokens(projected))
		return
	}
	if len(plan.Projected) == 0 {
		p.printf("  no conversation items\n")
		return
	}
	if len(plan.Prefix) == 0 && len(plan.Retained) == 0 {
		if view.Decision.Reason != "" {
			p.printf("  no retained-tail split: %s\n", view.Decision.Reason)
		}
		p.printf("\n")
		rows := itemRows(plan.Projected, func(index int) int { return index }, "")
		renderRows(p, rows)
		writeItemTotal(p, len(plan.Projected), session.EstimateTokens(plan.Projected))
		renderFooter(p, plan, view.Decision)
		return
	}

	p.printf("\n  --- RETIRED PREFIX (becomes the summary transcript) ---\n")
	prefix := make([]row, 0, len(plan.Prefix))
	for index, item := range plan.Prefix {
		prefix = append(prefix, itemRow(item, prefixIndex(index, plan), "retire"))
	}
	renderRows(p, prefix)
	writeItemTotal(p, len(plan.Prefix), session.EstimateTokens(plan.Prefix))

	p.printf("\n  --- RETAINED TAIL (copied into the checkpoint) ---\n")
	retained := make([]row, 0, len(plan.Retained))
	for index, item := range plan.Retained {
		marker := "keep"
		if kind := pinKind(item); kind != "" {
			marker = "PIN " + kind
		}
		retained = append(retained, itemRow(item, retainedIndex(index, plan), marker))
	}
	renderRows(p, retained)
	writeItemTotal(p, len(plan.Retained), session.EstimateTokens(plan.Retained))

	if len(plan.Prefix) > 0 {
		p.printf("\n  %d retired + 1 summary -> new prefix\n", len(plan.Prefix))
		word := "records"
		verb := "stay"
		if len(plan.Retained) == 1 {
			word = "record"
			verb = "stays"
		}
		p.printf("  %d retained %s %s in the checkpoint\n", len(plan.Retained), word, verb)
	}

	renderFooter(p, plan, view.Decision)
}

func tokenWord(tokens uint64) string {
	if tokens == 1 {
		return "token"
	}
	return "tokens"
}

func writeItemTotal(p *printer, items int, tokens uint64) {
	word := "items"
	if items == 1 {
		word = "item"
	}
	p.printf("      %d %s, %d estimated tokens\n", items, word, tokens)
}

func renderFooter(p *printer, plan *compact.Plan, decision compact.Decision) {
	p.printf("\n  estimated input: %d tokens (%s)\n", plan.BeforeTokens, plan.Basis)
	switch {
	case plan.Window > 0:
		p.printf("  window: %d tokens; threshold: %d tokens; keep target: %d %s\n",
			plan.Window, plan.ThresholdTokens, plan.KeepTarget, tokenWord(plan.KeepTarget))
	case plan.ThresholdTokens > 0:
		p.printf("  threshold: %d tokens; keep target: %d %s\n",
			plan.ThresholdTokens, plan.KeepTarget, tokenWord(plan.KeepTarget))
	default:
		p.printf("  keep target: %d %s\n", plan.KeepTarget, tokenWord(plan.KeepTarget))
	}
	if decision.Compact {
		p.printf("  would compact: yes\n")
	} else if decision.Reason != "" {
		p.printf("  would compact: no (%s)\n", decision.Reason)
	} else {
		p.printf("  would compact: no\n")
	}
}

func itemRows(items []session.Item, index func(int) int, marker string) []row {
	rows := make([]row, 0, len(items))
	for i, item := range items {
		rows = append(rows, itemRow(item, index(i), marker))
	}
	return rows
}

func itemRow(item session.Item, index int, marker string) row {
	kind, detail := itemLabel(item)
	return row{
		index:  index,
		kind:   kind,
		detail: detail,
		tokens: session.EstimateItemTokens(item),
		marker: marker,
	}
}

func renderRows(p *printer, rows []row) {
	width := minKindWidth
	for _, row := range rows {
		if len(row.kind) > width {
			width = len(row.kind)
		}
	}
	for _, row := range rows {
		line := fmt.Sprintf("  %2d  %-*s %-*s %6d tok",
			row.index, width, row.kind, detailWidth, truncate(row.detail, detailWidth), row.tokens)
		if row.marker != "" {
			line += "  " + row.marker
		}
		p.printf("%s\n", line)
	}
}

// prefixIndex maps a row in Plan.Prefix back to its index in Plan.Projected.
// The prefix omits the owning request when that request sits before Start.
func prefixIndex(index int, plan *compact.Plan) int {
	if plan.Anchor >= 0 && index >= plan.Anchor {
		return index + 1
	}
	return index
}

// retainedIndex maps a row in Plan.Retained back to its index in
// Plan.Projected. Retained[0] is the pinned owning request when one exists.
func retainedIndex(index int, plan *compact.Plan) int {
	switch {
	case plan.Anchor < 0:
		return plan.Start + index
	case index == 0:
		return plan.Anchor
	case plan.Anchor == plan.Start:
		return plan.Start + index
	default:
		return plan.Start + index - 1
	}
}

func itemLabel(item session.Item) (string, string) {
	switch {
	case item.Synthetic:
		return "USER", "synthetic historical summary"
	case item.Message != nil:
		role := strings.ToUpper(item.Message.Role)
		if role == "" {
			role = "?"
		}
		return role, firstLine(item.Message.Content)
	case item.Reasoning != nil:
		return "reasoning", firstLine(reasoningText(item.Reasoning))
	case item.Call != nil:
		name := item.Call.Name
		if name == "" {
			name = "?"
		}
		return "call " + name, firstLine(item.Call.Arguments)
	case item.Output != nil:
		return "result " + shortID(item.Output.CallID), firstLine(item.Output.Output)
	default:
		return item.Type, ""
	}
}

func reasoningText(reasoning *session.Reasoning) string {
	if text := reasoning.Text(); text != "" {
		return text
	}
	return reasoning.ContentText()
}

func pinKind(item session.Item) string {
	var pinned struct {
		Pin *struct {
			Kind string `json:"kind"`
		} `json:"pin"`
	}
	if err := json.Unmarshal(item.Raw, &pinned); err != nil || pinned.Pin == nil {
		return ""
	}
	if pinned.Pin.Kind == "" {
		return "pinned"
	}
	return pinned.Pin.Kind
}

func shortID(callID string) string {
	if callID == "" {
		return "?"
	}
	if len(callID) <= callIDWidth {
		return callID
	}
	return callID[:callIDWidth]
}

func firstLine(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if newline := strings.IndexByte(text, '\n'); newline >= 0 {
		text = text[:newline]
	}
	text = strings.ReplaceAll(text, "\t", " ")
	return strings.TrimRight(text, " \r")
}

func truncate(text string, width int) string {
	if width <= 0 || utf8.RuneCountInString(text) <= width {
		return text
	}
	runes := []rune(text)
	return string(runes[:width-1]) + "…"
}
