package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"io"
	"os"
	"strings"
	"time"

	"github.com/yuin/goldmark"
)

const DefaultTailSize = 200

type jsonlLine struct {
	Type        string          `json:"type"`
	Timestamp   string          `json:"timestamp"`
	UUID        string          `json:"uuid"`
	IsSidechain bool            `json:"isSidechain"`
	Message     json.RawMessage `json:"message"`
}

type messagePayload struct {
	Role    string      `json:"role"`
	Content any `json:"content"`
	Model   string      `json:"model"`
}

// parsedMsg holds raw parsed data before HTML rendering
type parsedMsg struct {
	Type      string
	Timestamp time.Time
	UUID      string
	Role      string
	Content   string
	Model     string
	ToolCalls []ToolCall
}

// tailChunkSize is how many bytes we read from the end of the file when seeking
// the tail. 4MB is enough for hundreds of messages in practice; we double and
// retry if we haven't collected tailSize messages yet.
const tailChunkSize = 4 * 1024 * 1024

// ParseJSONL reads a session JSONL file. Returns all messages with text extracted
// (for search), but only renders HTML for the last `tailSize` messages.
// For large files it seeks from the end to avoid reading the whole file.
func ParseJSONL(path string, tailSize int) ([]Message, int, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, "", err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, 0, "", err
	}
	fileSize := fi.Size()

	// For large files, read a chunk from the end to get the tail messages,
	// then do a fast forward pass for the total count and search text.
	// Threshold: if file > tailChunkSize, use the tail-seek strategy.
	if tailSize > 0 && fileSize > tailChunkSize {
		return parseJSONLLarge(f, fileSize, tailSize)
	}

	// Small file: read everything in one pass (line-by-line to handle malformed lines).
	var all []parsedMsg
	var textBuilder strings.Builder

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		pm, ok := decodeEntryBytes(line)
		if !ok {
			continue
		}
		all = append(all, pm)
		textBuilder.WriteString(pm.Content)
		textBuilder.WriteString("\n")
	}

	total := len(all)
	start := 0
	if tailSize > 0 && total > tailSize {
		start = total - tailSize
	}

	messages := make([]Message, 0, total-start)
	for _, pm := range all[start:] {
		messages = append(messages, toMessage(pm))
	}

	return messages, total, textBuilder.String(), nil
}

// parseJSONLLarge handles files larger than tailChunkSize. It reads backward
// from EOF in increasing chunks until it has enough tail messages, then does
// a forward pass from the start for the total count and search text.
func parseJSONLLarge(f *os.File, fileSize int64, tailSize int) ([]Message, int, string, error) {
	// Grow chunk until we have enough messages from the tail.
	chunkSize := int64(tailChunkSize)
	var tailMsgs []parsedMsg
	for {
		offset := max(fileSize-chunkSize, 0)
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, 0, "", err
		}
		chunk, err := io.ReadAll(f)
		if err != nil {
			return nil, 0, "", err
		}

		// Discard up to the first newline (we may have started mid-line).
		if offset > 0 {
			if nl := bytes.IndexByte(chunk, '\n'); nl >= 0 {
				chunk = chunk[nl+1:]
			}
		}

		tailMsgs = tailMsgs[:0]
		for _, line := range bytes.Split(chunk, []byte("\n")) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			pm, ok := decodeEntryBytes(line)
			if !ok {
				continue
			}
			tailMsgs = append(tailMsgs, pm)
		}

		if len(tailMsgs) >= tailSize || offset == 0 {
			break
		}
		chunkSize *= 2
	}

	// For huge files (>50MB), skip the full forward pass for search text.
	// Just estimate total from tail density and use tail text for search.
	const searchCap = 50 * 1024 * 1024
	var total int
	var searchText string

	if fileSize > searchCap {
		// Estimate total from tail: (tailMsgs / chunkBytes) * fileSize
		chunkBytes := min(fileSize, int64(len(tailMsgs))*50*1024)
		total = max(int(float64(len(tailMsgs))*float64(fileSize)/float64(chunkBytes)), len(tailMsgs))
		// Use tail messages for search
		var tb strings.Builder
		for _, pm := range tailMsgs {
			tb.WriteString(pm.Content)
			tb.WriteString("\n")
		}
		searchText = tb.String()
	} else {
		// Forward pass for total count + search text (line-based to handle malformed lines)
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, 0, "", err
		}
		var textBuilder strings.Builder
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
		for sc.Scan() {
			line := sc.Bytes()
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			pm, ok := decodeEntryBytes(line)
			if !ok {
				continue
			}
			total++
			textBuilder.WriteString(pm.Content)
			textBuilder.WriteString("\n")
		}
		searchText = textBuilder.String()
	}

	// Use the tail messages we already parsed.
	start := 0
	if len(tailMsgs) > tailSize {
		start = len(tailMsgs) - tailSize
	}
	messages := make([]Message, 0, len(tailMsgs)-start)
	for _, pm := range tailMsgs[start:] {
		messages = append(messages, toMessage(pm))
	}

	return messages, total, searchText, nil
}

// decodeEntryBytes parses a single JSONL line into a parsedMsg.
func decodeEntryBytes(line []byte) (parsedMsg, bool) {
	var entry jsonlLine
	if err := json.Unmarshal(line, &entry); err != nil {
		return parsedMsg{}, false
	}
	return decodeEntryCommon(entry)
}

func decodeEntryCommon(entry jsonlLine) (parsedMsg, bool) {
	if entry.IsSidechain || (entry.Type != "user" && entry.Type != "assistant") {
		return parsedMsg{}, false
	}
	var payload messagePayload
	if entry.Message != nil {
		if err := json.Unmarshal(entry.Message, &payload); err != nil {
			return parsedMsg{}, false
		}
	}
	content, toolCalls := extractContent(payload.Content)
	if content == "" && len(toolCalls) == 0 {
		return parsedMsg{}, false
	}
	ts, _ := time.Parse(time.RFC3339Nano, entry.Timestamp)
	return parsedMsg{
		Type:      entry.Type,
		Timestamp: ts,
		UUID:      entry.UUID,
		Role:      payload.Role,
		Content:   content,
		Model:     payload.Model,
		ToolCalls: toolCalls,
	}, true
}

// ParseJSONLRange renders a specific range of messages from a JSONL file.
// Returns messages from index `from` to `to` (exclusive).
func ParseJSONLRange(path string, from, to int) ([]Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var idx int
	var messages []Message

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		pm, ok := decodeEntryBytes(line)
		if !ok {
			continue
		}

		if idx >= from && idx < to {
			messages = append(messages, toMessage(pm))
		}

		idx++
		if idx >= to {
			break
		}
	}

	return messages, sc.Err()
}

func toMessage(pm parsedMsg) Message {
	msg := Message{
		Type:        pm.Type,
		Timestamp:   pm.Timestamp,
		UUID:        pm.UUID,
		Role:        pm.Role,
		Content:     pm.Content,
		ContentHTML: renderHTML(pm.Role, pm.Content),
		ToolCalls:   pm.ToolCalls,
	}
	msg.RawMessage.Role = pm.Role
	msg.RawMessage.Model = pm.Model
	return msg
}

func extractContent(c any) (string, []ToolCall) {
	if c == nil {
		return "", nil
	}

	if s, ok := c.(string); ok {
		return s, nil
	}

	if arr, ok := c.([]any); ok {
		var parts []string
		var tools []ToolCall
		for _, item := range arr {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch m["type"] {
			case "text":
				if text, ok := m["text"].(string); ok {
					parts = append(parts, text)
				}
			case "tool_use":
				name, _ := m["name"].(string)
				id, _ := m["id"].(string)
				inputSummary := summarizeInput(m["input"])
				tools = append(tools, ToolCall{Name: name, ID: id, Input: inputSummary})
			}
		}
		return strings.Join(parts, "\n"), tools
	}

	return "", nil
}

func summarizeInput(input any) string {
	if input == nil {
		return ""
	}
	m, ok := input.(map[string]any)
	if !ok {
		return ""
	}
	var parts []string
	for k, v := range m {
		s := fmt.Sprintf("%v", v)
		if len(s) > 60 {
			s = s[:60] + "…"
		}
		parts = append(parts, k+"="+s)
	}
	return strings.Join(parts, ", ")
}

var md = goldmark.New()

func renderHTML(role, content string) template.HTML {
	if role == "assistant" {
		var buf bytes.Buffer
		if err := md.Convert([]byte(content), &buf); err == nil {
			return template.HTML(buf.String())
		}
	}
	escaped := html.EscapeString(content)
	escaped = strings.ReplaceAll(escaped, "\n", "<br>")
	return template.HTML(escaped)
}

// IsSystemNoise returns true if the content looks like a system/command message
func IsSystemNoise(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return true
	}
	prefixes := []string{"<local-command-caveat>", "<command-name>", "<system-reminder>", "<task-notification>", "<available-deferred-tools>", "<bash-input>", "<local-command-stdout>", "[Request interrupted"}
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
