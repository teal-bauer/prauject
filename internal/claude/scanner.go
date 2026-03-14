package claude

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

func claudeDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// ProjectsDir returns the path to ~/.claude/projects/
func ProjectsDir() string {
	return filepath.Join(claudeDir(), "projects")
}

type pidEntry struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
}

// scanActiveSessions returns a set of session IDs that have a running claude process.
// It matches by both the PID file's sessionId AND by finding the most recently
// modified JSONL in the same cwd (since resumed sessions may have a different ID).
func ScanActiveSessions() map[string]bool {
	active := map[string]bool{}
	sessDir := filepath.Join(claudeDir(), "sessions")
	entries, err := os.ReadDir(sessDir)
	if err != nil {
		return active
	}

	var activeCWDs []string

	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(sessDir, e.Name()))
		if err != nil {
			continue
		}
		var pe pidEntry
		if json.Unmarshal(data, &pe) != nil {
			continue
		}
		if pe.PID <= 0 {
			continue
		}
		proc, err := os.FindProcess(pe.PID)
		if err != nil {
			continue
		}
		if proc.Signal(syscall.Signal(0)) != nil {
			continue
		}
		// Process is alive
		active[pe.SessionID] = true
		if pe.CWD != "" {
			activeCWDs = append(activeCWDs, pe.CWD)
		}
	}

	// Also mark sessions whose JSONL was recently modified in an active cwd
	projectsDir := filepath.Join(claudeDir(), "projects")
	for _, cwd := range activeCWDs {
		// Encode cwd to project dir name
		encoded := "-" + strings.ReplaceAll(cwd[1:], "/", "-")
		projDir := filepath.Join(projectsDir, encoded)
		jsonls, _ := filepath.Glob(filepath.Join(projDir, "*.jsonl"))
		for _, jp := range jsonls {
			info, err := os.Stat(jp)
			if err != nil {
				continue
			}
			// If modified in the last 5 minutes, it's likely the active session
			if time.Since(info.ModTime()) < 5*time.Minute {
				sid := strings.TrimSuffix(filepath.Base(jp), ".jsonl")
				active[sid] = true
			}
		}
	}

	return active
}

// ScanSessions discovers all sessions from ~/.claude/projects/
func ScanSessions() ([]*Session, error) {
	projectsDir := filepath.Join(claudeDir(), "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil, err
	}

	active := ScanActiveSessions()

	var sessions []*Session
	seen := map[string]bool{}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		projDir := filepath.Join(projectsDir, entry.Name())

		// Try sessions-index.json first
		indexPath := filepath.Join(projDir, "sessions-index.json")
		if data, err := os.ReadFile(indexPath); err == nil {
			var idx sessionsIndex
			if err := json.Unmarshal(data, &idx); err == nil && len(idx.Entries) > 0 {
				for _, e := range idx.Entries {
					if seen[e.SessionID] || e.IsSidechain {
						continue
					}
					seen[e.SessionID] = true
					s := sessionFromIndex(e, entry.Name())
					sessions = append(sessions, s)
				}
				continue
			}
		}

		// Fallback: scan for *.jsonl files
		jsonls, _ := filepath.Glob(filepath.Join(projDir, "*.jsonl"))
		for _, jp := range jsonls {
			base := strings.TrimSuffix(filepath.Base(jp), ".jsonl")
			if seen[base] || len(base) < 30 { // UUID is 36 chars
				continue
			}
			seen[base] = true

			info, _ := os.Stat(jp)
			project := CWDFromJSONL(jp)
			if project == "" {
				project = DecodeProjectPath(entry.Name())
			}

			sessions = append(sessions, &Session{
				ID:         base,
				Project:    project,
				ProjectDir: entry.Name(),
				FilePath:   jp,
				Modified:   info.ModTime(),
				Created:    info.ModTime(),
			})
		}
	}

	// Mark active sessions
	for _, s := range sessions {
		s.Active = active[s.ID]
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].Modified.After(sessions[j].Modified)
	})

	return sessions, nil
}

func sessionFromIndex(e indexEntry, projDirName string) *Session {
	created, _ := time.Parse(time.RFC3339, e.Created)
	modified, _ := time.Parse(time.RFC3339, e.Modified)

	project := e.ProjectPath
	if project == "" {
		project = DecodeProjectPath(projDirName)
	}

	return &Session{
		ID:           e.SessionID,
		Project:      project,
		ProjectDir:   projDirName,
		Summary:      e.Summary,
		FirstPrompt:  e.FirstPrompt,
		GitBranch:    e.GitBranch,
		MessageCount: e.MessageCount,
		Created:      created,
		Modified:     modified,
		FilePath:     e.FullPath,
		IsSidechain:  e.IsSidechain,
	}
}

// DecodeProjectPath converts an encoded project dir name back to a path.
func DecodeProjectPath(encoded string) string {
	// -home-teal-src-foo -> /home/teal/src/foo
	// This is lossy (hyphens in dir names), but used only as fallback
	if !strings.HasPrefix(encoded, "-") {
		return encoded
	}
	return "/" + strings.ReplaceAll(encoded[1:], "-", "/")
}

// firstPromptFromJSONL reads the first real user prompt from the start of the file
func firstPromptFromJSONL(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	for dec.More() {
		var entry struct {
			Type    string `json:"type"`
			Message struct {
				Role    string      `json:"role"`
				Content any `json:"content"`
			} `json:"message"`
		}
		if dec.Decode(&entry) != nil {
			continue
		}
		if entry.Type != "user" || entry.Message.Role != "user" {
			continue
		}
		content, _ := extractContent(entry.Message.Content)
		if content != "" && !IsSystemNoise(content) {
			if len(content) > 200 {
				content = content[:200] + "…"
			}
			return content
		}
	}
	return ""
}

// CWDFromJSONL reads the first user/assistant message to extract cwd.
func CWDFromJSONL(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	for i := 0; i < 20 && dec.More(); i++ {
		var entry struct {
			CWD string `json:"cwd"`
		}
		if dec.Decode(&entry) == nil && entry.CWD != "" {
			return entry.CWD
		}
	}
	return ""
}

// LoadMessages reads and parses the JSONL file for a session.
// Only renders HTML for the last DefaultTailSize messages.
func LoadMessages(s *Session) error {
	if s.FilePath == "" {
		return nil
	}
	if _, err := os.Stat(s.FilePath); os.IsNotExist(err) {
		return nil
	}
	messages, total, fullText, err := ParseJSONL(s.FilePath, DefaultTailSize)
	if err != nil {
		log.Printf("parse %s: %v", s.FilePath, err)
		return err
	}
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.Messages = messages
	s.TotalMessages = total
	s.MessagesText = fullText
	s.Loaded = true
	s.FullyLoaded = total <= DefaultTailSize

	if len(messages) > 0 {
		last := messages[len(messages)-1]
		preview := last.Content
		if len(preview) > 500 {
			preview = preview[:500]
		}
		s.LastMessage = preview
	}

	if s.FirstPrompt == "" && s.FilePath != "" {
		s.FirstPrompt = firstPromptFromJSONL(s.FilePath)
	}
	if s.MessageCount == 0 {
		s.MessageCount = total
	}
	return nil
}

// LoadMessagesRange loads earlier messages for a session.
func LoadMessagesRange(s *Session, from, to int) ([]Message, error) {
	if s.FilePath == "" {
		return nil, nil
	}
	return ParseJSONLRange(s.FilePath, from, to)
}
