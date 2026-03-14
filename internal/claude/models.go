package claude

import (
	"html/template"
	"sync"
	"time"
)

type Session struct {
	Mu           sync.RWMutex `json:"-"`
	ID           string    `json:"sessionId"`
	Project      string    `json:"projectPath"`
	ProjectDir   string    `json:"-"`
	Summary      string    `json:"summary"`
	FirstPrompt  string    `json:"firstPrompt"`
	GitBranch    string    `json:"gitBranch"`
	MessageCount int       `json:"messageCount"`
	Created      time.Time `json:"created"`
	Modified     time.Time `json:"modified"`
	FilePath     string    `json:"-"`
	IsSidechain  bool      `json:"isSidechain"`
	Active       bool      `json:"-"`

	LastMessage  string    `json:"-"`

	Messages      []Message `json:"-"`
	TotalMessages int       `json:"-"`
	MessagesText  string    `json:"-"`
	Loaded        bool      `json:"-"`
	FullyLoaded   bool      `json:"-"`
}

type Message struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	UUID      string    `json:"uuid"`

	// For user/assistant messages
	Role    string `json:"-"`
	Content string `json:"-"` // extracted text content

	// Pre-rendered HTML (markdown for assistant, escaped for user)
	ContentHTML template.HTML `json:"-"`
	ToolCalls   []ToolCall    `json:"-"`

	// Raw for flexibility
	RawMessage rawMessage `json:"message"`
}

type ToolCall struct {
	Name  string `json:"name"`
	ID    string `json:"id"`
	Input string `json:"input"` // summarized input
}

type rawMessage struct {
	Role    string      `json:"role"`
	Content any `json:"content"`
	Model   string      `json:"model"`
}

type sessionsIndex struct {
	Version int          `json:"version"`
	Entries []indexEntry `json:"entries"`
}

type indexEntry struct {
	SessionID   string `json:"sessionId"`
	FullPath    string `json:"fullPath"`
	FileMtime   int64  `json:"fileMtime"`
	FirstPrompt string `json:"firstPrompt"`
	Summary     string `json:"summary"`
	MessageCount int   `json:"messageCount"`
	Created     string `json:"created"`
	Modified    string `json:"modified"`
	GitBranch   string `json:"gitBranch"`
	ProjectPath string `json:"projectPath"`
	IsSidechain bool   `json:"isSidechain"`
}
