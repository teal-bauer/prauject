package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
)

func isValidSessionID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for _, c := range id {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') || c == '-') {
			return false
		}
	}
	return true
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		return strings.Contains(origin, "://localhost:") || strings.Contains(origin, "://127.0.0.1:")
	},
}

type wsMessage struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !isValidSessionID(id) {
		http.Error(w, "invalid session ID", 400)
		return
	}
	sess := s.getSession(id)
	if sess == nil {
		http.Error(w, "session not found", 404)
		return
	}

	if _, running := s.isRunning(id); running {
		w.Header().Set("HX-Redirect", fmt.Sprintf("/sessions/%s", id))
		w.WriteHeader(200)
		return
	}

	tmuxName := "cc-" + id[:8]
	dir := sess.Project
	if dir == "" {
		dir = "/tmp"
	}

	// Create tmux session running claude --resume
	claudeCmd := append([]string{"claude"}, s.claudeArgs...)
	claudeCmd = append(claudeCmd, "--resume", id)
	tmuxArgs := append([]string{"new-session", "-d", "-s", tmuxName, "-c", dir}, claudeCmd...)
	cmd := exec.Command("tmux", tmuxArgs...)
	if err := cmd.Run(); err != nil {
		log.Printf("tmux create: %v", err)
		http.Error(w, fmt.Sprintf("failed to start tmux: %v", err), 500)
		return
	}

	s.runningMu.Lock()
	s.running[id] = tmuxName
	s.runningMu.Unlock()

	// Monitor tmux session in background
	go s.monitorTmux(id, tmuxName)

	w.Header().Set("HX-Redirect", fmt.Sprintf("/sessions/%s", id))
	w.WriteHeader(200)
}

func (s *Server) monitorTmux(sessionID, tmuxName string) {
	for {
		time.Sleep(2 * time.Second)
		err := exec.Command("tmux", "has-session", "-t", tmuxName).Run()
		if err != nil {
			s.runningMu.Lock()
			delete(s.running, sessionID)
			s.runningMu.Unlock()
			log.Printf("tmux session %s ended", tmuxName)
			return
		}
	}
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !isValidSessionID(id) {
		http.Error(w, "invalid session ID", 400)
		return
	}

	tmuxName, running := s.isRunning(id)
	if !running {
		http.Error(w, "session not running", 400)
		return
	}

	rawConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws upgrade: %v", err)
		return
	}
	defer rawConn.Close()
	conn := &wsConn{conn: rawConn}

	s.hub.register(id, conn)
	defer s.hub.unregister(id, conn)

	// Send current tmux pane dimensions so client knows the starting size
	if dims, err := exec.Command("tmux", "display-message", "-t", tmuxName, "-p", "#{pane_width}x#{pane_height}").Output(); err == nil {
		msg, _ := json.Marshal(wsMessage{Type: "size", Data: strings.TrimSpace(string(dims))})
		conn.writeMessage(websocket.TextMessage, msg)
	}

	stopCh := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		var lastContent string
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
			}

			out, err := exec.Command("tmux", "capture-pane", "-p", "-e", "-S", "-500", "-t", tmuxName).Output()
			if err != nil {
				msg, _ := json.Marshal(wsMessage{Type: "terminal", Data: "\r\n[session ended]\r\n"})
				conn.writeMessage(websocket.TextMessage, msg)
				return
			}
			content := string(out)
			if content == lastContent {
				continue
			}
			lastContent = content
			msg, _ := json.Marshal(wsMessage{Type: "terminal", Data: content})
			if err := conn.writeMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		}
	}()

	for {
		_, raw, err := rawConn.ReadMessage()
		if err != nil {
			close(stopCh)
			break
		}
		var payload struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			continue
		}
		switch payload.Type {
		case "resize-restore":
			exec.Command("tmux", "set-option", "-t", tmuxName, "window-size", "latest").Run()
			// Force tmux to recalculate by toggling aggressive-resize
			exec.Command("tmux", "set-option", "-t", tmuxName, "aggressive-resize", "on").Run()
			exec.Command("tmux", "set-option", "-t", tmuxName, "aggressive-resize", "off").Run()
		case "input":
			if payload.Data != "" {
				exec.Command("tmux", "send-keys", "-t", tmuxName, "-l", payload.Data).Run()
			}
		case "resize":
			// Browser sends its available cols x rows.
			// Use min(browser, current tmux) for each dimension
			// so output fits in both the real terminal and the browser.
			parts := strings.SplitN(payload.Data, "x", 2)
			if len(parts) != 2 {
				break
			}
			browserCols, _ := strconv.Atoi(parts[0])
			browserRows, _ := strconv.Atoi(parts[1])
			if browserCols <= 0 || browserRows <= 0 {
				break
			}
			// Switch to manual sizing so resize-window works
			exec.Command("tmux", "set-option", "-t", tmuxName, "window-size", "manual").Run()
			// Get current tmux size, use min of each dimension
			if out, err := exec.Command("tmux", "display-message", "-t", tmuxName, "-p", "#{window_width}x#{window_height}").Output(); err == nil {
				tp := strings.SplitN(strings.TrimSpace(string(out)), "x", 2)
				if len(tp) == 2 {
					tmuxCols, _ := strconv.Atoi(tp[0])
					tmuxRows, _ := strconv.Atoi(tp[1])
					newCols := min(browserCols, tmuxCols)
					newRows := min(browserRows, tmuxRows)
					exec.Command("tmux", "resize-window", "-t", tmuxName, "-x", strconv.Itoa(newCols), "-y", strconv.Itoa(newRows)).Run()
					msg, _ := json.Marshal(wsMessage{Type: "size", Data: fmt.Sprintf("%dx%d", newCols, newRows)})
					conn.writeMessage(websocket.TextMessage, msg)
				}
			}
		}
	}
	<-done
}
