// Package session reads and projects Cake's format-4 session JSONL.
//
// It implements only the read side of the contract published in
// docs/integrations.md: the conversation records, the compaction checkpoint
// record, and the fields this proof of concept needs to size context and build
// a transcript. It does not execute tools, interpret audit records, or write to
// a source session.
package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// HistoricalSummaryMarker is the exact text Cake puts in front of a
// checkpoint summary when it builds a provider request.
const HistoricalSummaryMarker = "[Historical context summary of earlier work in this session. It is reference material, not a new user request.]"

// Conversation record kinds and checkpoint record kind.
const (
	TypeSessionMeta    = "session_meta"
	TypeMessage        = "message"
	TypeReasoning      = "reasoning"
	TypeFunctionCall   = "function_call"
	TypeFunctionOutput = "function_call_output"
	TypeCompaction     = "compaction"
)

// Image is one inline image payload. Only its size is used here.
type Image struct {
	MediaType  string `json:"media_type"`
	DataBase64 string `json:"data_base64"`
}

// Message is the payload shared by session and stream message records.
type Message struct {
	Role      string  `json:"role"`
	Content   string  `json:"content"`
	ID        string  `json:"id,omitempty"`
	Status    string  `json:"status,omitempty"`
	Timestamp string  `json:"timestamp,omitempty"`
	Images    []Image `json:"images,omitempty"`
}

// Reasoning is the payload of a reasoning record. Summary is kept raw because
// historical sessions may carry either typed objects or plain strings.
type Reasoning struct {
	ID               string          `json:"id"`
	Summary          json.RawMessage `json:"summary,omitempty"`
	EncryptedContent string          `json:"encrypted_content,omitempty"`
	Content          json.RawMessage `json:"content,omitempty"`
	Timestamp        string          `json:"timestamp,omitempty"`
}

// FunctionCall is the payload of a function call record.
type FunctionCall struct {
	ID                  string `json:"id"`
	CallID              string `json:"call_id"`
	Name                string `json:"name"`
	Arguments           string `json:"arguments"`
	ArgumentsParseError string `json:"arguments_parse_error,omitempty"`
	Replay              string `json:"replay,omitempty"`
	Timestamp           string `json:"timestamp,omitempty"`
}

// FunctionCallOutput is the payload of a function call output record.
type FunctionCallOutput struct {
	CallID    string  `json:"call_id"`
	Output    string  `json:"output"`
	Replay    string  `json:"replay,omitempty"`
	Timestamp string  `json:"timestamp,omitempty"`
	Images    []Image `json:"images,omitempty"`
}

// Usage mirrors the subset of Cake's normalized usage this tool reads.
type Usage struct {
	InputTokens  uint64 `json:"input_tokens"`
	OutputTokens uint64 `json:"output_tokens"`
	TotalTokens  uint64 `json:"total_tokens"`
}

// TurnUsage is one main-agent provider attempt's usage.
type TurnUsage struct {
	SessionID              string  `json:"session_id"`
	TaskID                 string  `json:"task_id"`
	Turn                   uint32  `json:"turn"`
	Usage                  Usage   `json:"usage"`
	Timestamp              string  `json:"timestamp"`
	Attempt                *uint32 `json:"attempt,omitempty"`
	UsagePresence          *string `json:"usage_presence,omitempty"`
	ContextWindow          *uint32 `json:"context_window,omitempty"`
	RemainingContextTokens *uint64 `json:"remaining_context_tokens,omitempty"`
}

// EstimatedInput mirrors the checkpoint's estimate metadata.
type EstimatedInput struct {
	BeforeTokens uint64 `json:"before_tokens"`
	AfterTokens  uint64 `json:"after_tokens"`
	Basis        string `json:"basis"`
}

// ModelIdentity mirrors the checkpoint's model metadata.
type ModelIdentity struct {
	Model           string `json:"model"`
	ModelConfig     string `json:"model_config,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

// SummaryUsage mirrors the summarizer's provider-reported measurement carried
// by a compaction checkpoint. It is the flat shape Cake accepts in the
// checkpoint payload, not the nested per-attempt Usage record.
type SummaryUsage struct {
	InputTokens          uint64 `json:"input_tokens"`
	CachedTokens         uint64 `json:"cached_tokens"`
	OutputTokens         uint64 `json:"output_tokens"`
	ReasoningTokens      uint64 `json:"reasoning_tokens"`
	SummaryVisibleTokens uint64 `json:"summary_visible_tokens"`
	TotalTokens          uint64 `json:"total_tokens"`
}

// Checkpoint is the payload of a compaction record.
type Checkpoint struct {
	Type                      string            `json:"type"`
	CheckpointVersion         int               `json:"checkpoint_version"`
	CheckpointID              string            `json:"checkpoint_id"`
	SessionID                 string            `json:"session_id"`
	TaskID                    string            `json:"task_id"`
	Timestamp                 string            `json:"timestamp"`
	Trigger                   string            `json:"trigger"`
	Summary                   string            `json:"summary"`
	Retained                  []json.RawMessage `json:"retained"`
	Model                     ModelIdentity     `json:"model"`
	SummaryInstructionVersion int               `json:"summary_instruction_version"`
	EstimatedInput            EstimatedInput    `json:"estimated_input"`
	SummaryUsage              *SummaryUsage     `json:"summary_usage,omitempty"`
	SummaryTimeMs             int64             `json:"summary_time_ms,omitempty"`
}

// Record is one parsed session line. Raw keeps the complete JSON object so it
// can be copied into a checkpoint's retained tail without reinterpretation.
type Record struct {
	Index      int
	Type       string
	Raw        json.RawMessage
	Message    *Message
	Reasoning  *Reasoning
	Call       *FunctionCall
	Output     *FunctionCallOutput
	Usage      *TurnUsage
	Checkpoint *Checkpoint
	TaskID     string
}

// File is a parsed session file.
type File struct {
	Path     string
	Meta     map[string]json.RawMessage
	Records  []Record
	Warnings []string
}

// Item is one entry in the effective conversation. Synthetic is true for the
// historical summary item Cake inserts ahead of a checkpoint's retained tail.
type Item struct {
	SourceIndex int
	Type        string
	Raw         json.RawMessage
	Message     *Message
	Reasoning   *Reasoning
	Call        *FunctionCall
	Output      *FunctionCallOutput
	Synthetic   bool
}

// Load opens a session file, takes a shared advisory lock so a concurrent cake
// writer cannot change it mid-read, and parses it.
func Load(path string) (*File, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	if err := lockShared(file); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	return loadReader(path, file)
}

// LoadBytes parses session bytes from memory, for tests and fixtures.
func LoadBytes(path string, data []byte) (*File, error) {
	return loadReader(path, bytes.NewReader(data))
}

func loadReader(path string, reader io.Reader) (*File, error) {
	f := &File{Path: path, Meta: map[string]json.RawMessage{}}
	buffered := bufio.NewReaderSize(reader, 1<<20)
	lineNo := 0
	first := true

	for {
		line, readErr := buffered.ReadString('\n')
		lineNo++
		trimmed := strings.TrimSpace(line)

		if trimmed != "" {
			raw := json.RawMessage(append([]byte(nil), trimmed...))
			var envelope struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(raw, &envelope); err != nil {
				if readErr == io.EOF {
					f.Warnings = append(f.Warnings, fmt.Sprintf("ignored torn final record at line %d", lineNo))
					break
				}
				return nil, fmt.Errorf("%s:%d: invalid JSON: %w", path, lineNo, err)
			}

			if first {
				if envelope.Type != TypeSessionMeta {
					return nil, fmt.Errorf("%s:%d: first record must be %s, got %q", path, lineNo, TypeSessionMeta, envelope.Type)
				}
				if err := json.Unmarshal(raw, &f.Meta); err != nil {
					return nil, fmt.Errorf("%s:%d: invalid session metadata: %w", path, lineNo, err)
				}
				f.Records = append(f.Records, Record{Index: 0, Type: envelope.Type, Raw: raw})
				first = false
			} else {
				record, err := parseRecord(len(f.Records), raw, envelope.Type)
				if err != nil {
					return nil, fmt.Errorf("%s:%d: %w", path, lineNo, err)
				}
				f.Records = append(f.Records, record)
			}
		}

		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, lineNo, readErr)
		}
	}

	if first {
		return nil, fmt.Errorf("%s: empty session file", path)
	}

	sessionID := f.SessionID()
	if sessionID == "" {
		return nil, fmt.Errorf("%s: session_meta has no session_id", path)
	}
	for _, record := range f.Records {
		if record.Checkpoint != nil && record.Checkpoint.SessionID != sessionID {
			return nil, fmt.Errorf(
				"%s: checkpoint %q belongs to session %s, not %s",
				path, record.Checkpoint.CheckpointID, record.Checkpoint.SessionID, sessionID,
			)
		}
	}

	return f, nil
}

func parseRecord(index int, raw json.RawMessage, recordType string) (Record, error) {
	record := Record{Index: index, Type: recordType, Raw: raw}

	switch recordType {
	case TypeSessionMeta:
		return record, errors.New("unexpected second session_meta record")
	case TypeMessage:
		var message Message
		if err := json.Unmarshal(raw, &message); err != nil {
			return record, fmt.Errorf("invalid message: %w", err)
		}
		if message.Role == "system" || message.Role == "tool" {
			return record, fmt.Errorf("unsupported conversation role %q", message.Role)
		}
		record.Message = &message
	case TypeReasoning:
		var reasoning Reasoning
		if err := json.Unmarshal(raw, &reasoning); err != nil {
			return record, fmt.Errorf("invalid reasoning: %w", err)
		}
		record.Reasoning = &reasoning
	case TypeFunctionCall:
		var call FunctionCall
		if err := json.Unmarshal(raw, &call); err != nil {
			return record, fmt.Errorf("invalid function_call: %w", err)
		}
		record.Call = &call
	case TypeFunctionOutput:
		var output FunctionCallOutput
		if err := json.Unmarshal(raw, &output); err != nil {
			return record, fmt.Errorf("invalid function_call_output: %w", err)
		}
		record.Output = &output
	case "turn_usage":
		var usage TurnUsage
		if err := json.Unmarshal(raw, &usage); err != nil {
			return record, fmt.Errorf("invalid turn_usage: %w", err)
		}
		record.Usage = &usage
	case TypeCompaction:
		var checkpoint Checkpoint
		if err := json.Unmarshal(raw, &checkpoint); err != nil {
			return record, fmt.Errorf("invalid compaction checkpoint: %w", err)
		}
		record.Checkpoint = &checkpoint
	case "task_start":
		var task struct {
			TaskID string `json:"task_id"`
		}
		if err := json.Unmarshal(raw, &task); err != nil {
			return record, fmt.Errorf("invalid task_start: %w", err)
		}
		record.TaskID = task.TaskID
	}

	return record, nil
}

// SessionID returns the session UUID from session_meta.
func (f *File) SessionID() string {
	var id string
	_ = json.Unmarshal(f.Meta["session_id"], &id)
	return id
}

// MetaString returns a string field from session_meta, or "".
func (f *File) MetaString(key string) string {
	var value string
	_ = json.Unmarshal(f.Meta[key], &value)
	return value
}

// ConversationItemCount returns the number of provider-visible conversation
// items in the file. Cake derives a checkpoint's boundary as this count at the
// append position, so metadata and earlier checkpoints are not counted.
func (f *File) ConversationItemCount() int {
	count := 0
	for _, record := range f.Records {
		if _, ok := ItemFromRecord(record); ok {
			count++
		}
	}
	return count
}

// LatestCheckpointIndex returns the record index of the newest compaction
// record, or -1.
func (f *File) LatestCheckpointIndex() int {
	for i := len(f.Records) - 1; i >= 0; i-- {
		if f.Records[i].Checkpoint != nil {
			return f.Records[i].Index
		}
	}
	return -1
}

// LastUsage returns the record index and payload of the most recent complete
// main-agent measurement. Partial reports are skipped; historical records
// without usage_presence are accepted as complete.
func (f *File) LastUsage() (int, *TurnUsage) {
	for i := len(f.Records) - 1; i >= 0; i-- {
		usage := f.Records[i].Usage
		if usage == nil {
			continue
		}
		if usage.UsagePresence != nil && *usage.UsagePresence == "partial" {
			continue
		}
		return f.Records[i].Index, usage
	}
	return -1, nil
}

// LatestTaskID returns the task id of the newest task_start, or "".
func (f *File) LatestTaskID() string {
	for i := len(f.Records) - 1; i >= 0; i-- {
		if f.Records[i].TaskID != "" {
			return f.Records[i].TaskID
		}
	}
	return ""
}

// Projected returns the effective conversation exactly as Cake's projector
// builds it: without a checkpoint it is the raw conversation; with one it is
// the historical summary plus retained tail plus later records.
func (f *File) Projected() ([]Item, error) {
	checkpointIndex := f.LatestCheckpointIndex()
	if checkpointIndex < 0 {
		return conversationItems(f.Records)
	}

	checkpoint := f.Records[checkpointIndex].Checkpoint
	summary, err := SummaryItem(checkpoint)
	if err != nil {
		return nil, err
	}

	items := make([]Item, 0, len(checkpoint.Retained)+1)
	items = append(items, summary)
	for _, raw := range checkpoint.Retained {
		item, err := decodeItem(raw, -1)
		if err != nil {
			return nil, fmt.Errorf("retained record in checkpoint %s: %w", checkpoint.CheckpointID, err)
		}
		items = append(items, item)
	}

	later, err := conversationItems(f.Records[checkpointIndex+1:])
	if err != nil {
		return nil, err
	}
	return append(items, later...), nil
}

// ConversationItemsAfter returns the conversation items whose record index is
// greater than index.
func (f *File) ConversationItemsAfter(index int) ([]Item, error) {
	if index < 0 {
		return conversationItems(f.Records)
	}
	if index+1 >= len(f.Records) {
		return nil, nil
	}
	return conversationItems(f.Records[index+1:])
}

func conversationItems(records []Record) ([]Item, error) {
	items := make([]Item, 0, len(records))
	for _, record := range records {
		item, ok := ItemFromRecord(record)
		if !ok {
			continue
		}
		items = append(items, item)
	}
	return items, nil
}

// ItemFromRecord converts one conversation record. Audit and lifecycle records
// return false.
func ItemFromRecord(record Record) (Item, bool) {
	switch {
	case record.Message != nil:
		return Item{SourceIndex: record.Index, Type: record.Type, Raw: record.Raw, Message: record.Message}, true
	case record.Reasoning != nil:
		return Item{SourceIndex: record.Index, Type: record.Type, Raw: record.Raw, Reasoning: record.Reasoning}, true
	case record.Call != nil:
		return Item{SourceIndex: record.Index, Type: record.Type, Raw: record.Raw, Call: record.Call}, true
	case record.Output != nil:
		return Item{SourceIndex: record.Index, Type: record.Type, Raw: record.Raw, Output: record.Output}, true
	default:
		return Item{}, false
	}
}

func decodeItem(raw json.RawMessage, sourceIndex int) (Item, error) {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return Item{}, err
	}
	item := Item{SourceIndex: sourceIndex, Type: envelope.Type, Raw: raw}
	switch envelope.Type {
	case TypeMessage:
		var message Message
		if err := json.Unmarshal(raw, &message); err != nil {
			return Item{}, fmt.Errorf("invalid message: %w", err)
		}
		if message.Role == "system" || message.Role == "tool" {
			return Item{}, fmt.Errorf("retained message has unsupported role %q", message.Role)
		}
		item.Message = &message
	case TypeReasoning:
		var reasoning Reasoning
		if err := json.Unmarshal(raw, &reasoning); err != nil {
			return Item{}, fmt.Errorf("invalid reasoning: %w", err)
		}
		item.Reasoning = &reasoning
	case TypeFunctionCall:
		var call FunctionCall
		if err := json.Unmarshal(raw, &call); err != nil {
			return Item{}, fmt.Errorf("invalid function_call: %w", err)
		}
		item.Call = &call
	case TypeFunctionOutput:
		var output FunctionCallOutput
		if err := json.Unmarshal(raw, &output); err != nil {
			return Item{}, fmt.Errorf("invalid function_call_output: %w", err)
		}
		item.Output = &output
	default:
		return Item{}, fmt.Errorf("unsupported retained record type %q", envelope.Type)
	}
	return item, nil
}

// SummaryItem builds the synthetic user message Cake places after fresh
// prompts and ahead of a checkpoint's retained tail.
func SummaryItem(checkpoint *Checkpoint) (Item, error) {
	content := HistoricalSummaryMarker + "\n\n" + checkpoint.Summary
	object := map[string]any{
		"type":    TypeMessage,
		"role":    "user",
		"content": content,
	}
	if checkpoint.Timestamp != "" {
		object["timestamp"] = checkpoint.Timestamp
	}
	raw, err := json.Marshal(object)
	if err != nil {
		return Item{}, err
	}
	return Item{
		SourceIndex: -1,
		Type:        TypeMessage,
		Raw:         raw,
		Message:     &Message{Role: "user", Content: content},
		Synthetic:   true,
	}, nil
}

// IsUserMessage reports whether the item is a real user input. The synthetic
// historical summary is never a retirement boundary.
func (item Item) IsUserMessage() bool {
	return item.Message != nil && !item.Synthetic && item.Message.Role == "user"
}

// Text returns reasoning summary text, preferring typed objects and falling
// back to historical plain strings.
func (reasoning *Reasoning) Text() string {
	return reasoningEntries(reasoning.Summary)
}

// ContentText returns readable reasoning content. Cake currently writes typed
// entries ([{"type":"reasoning_text","text":"..."}]); older sessions may
// carry plain strings. Text stays summary-only because the summary transcript
// uses it to decide whether reasoning is included.
func (reasoning *Reasoning) ContentText() string {
	return reasoningEntries(reasoning.Content)
}

func reasoningEntries(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var typed []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &typed); err == nil {
		parts := make([]string, 0, len(typed))
		for _, entry := range typed {
			if entry.Text != "" {
				parts = append(parts, entry.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	var plain []string
	if err := json.Unmarshal(raw, &plain); err == nil {
		return strings.Join(plain, "\n")
	}
	return ""
}

const (
	estimatedCharsPerToken = 4.0
	estimatedSafetyMargin  = 1.25
	estimatedImageTokens   = 1000
)

// EstimateTokens gives a conservative content-class estimate for items.
func EstimateTokens(items []Item) uint64 {
	if len(items) == 0 {
		return 0
	}
	var chars float64
	var images int
	for _, item := range items {
		switch {
		case item.Message != nil:
			chars += float64(len(item.Message.Content))
			images += len(item.Message.Images)
		case item.Reasoning != nil:
			chars += float64(len(item.Reasoning.Text()) + len(item.Reasoning.ContentText()) + len(item.Reasoning.EncryptedContent))
		case item.Call != nil:
			chars += float64(len(item.Call.Name) + len(item.Call.Arguments))
		case item.Output != nil:
			chars += float64(len(item.Output.Output))
			images += len(item.Output.Images)
		}
	}
	tokens := chars/estimatedCharsPerToken + float64(images*estimatedImageTokens)
	tokens *= estimatedSafetyMargin
	if tokens < 1 {
		return 1
	}
	return uint64(tokens)
}

// EstimateItemTokens estimates one item.
func EstimateItemTokens(item Item) uint64 {
	return EstimateTokens([]Item{item})
}

// EstimateTextTokens estimates plain text as prose.
func EstimateTextTokens(text string) uint64 {
	return EstimateTokens([]Item{{Message: &Message{Content: text}}})
}
