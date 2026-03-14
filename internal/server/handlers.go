package server

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/go-chi/chi/v5"
	"github.com/teal-bauer/prauject/internal/claude"
)

func activeSessions(sessions []*claude.Session) []*claude.Session {
	var active []*claude.Session
	for _, s := range sessions {
		if s.Active {
			active = append(active, s)
		}
	}
	return active
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	sessions := s.getSessions()
	sortBy := r.URL.Query().Get("sort")
	if sortBy == "" {
		sortBy = "recent"
	}

	sorted := make([]*claude.Session, len(sessions))
	copy(sorted, sessions)

	if sortBy == "recent" {
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].Modified.After(sorted[j].Modified)
		})
	} else {
		sort.Slice(sorted, func(i, j int) bool {
			if sorted[i].Project != sorted[j].Project {
				return sorted[i].Project < sorted[j].Project
			}
			return sorted[i].Modified.After(sorted[j].Modified)
		})
	}

	data := map[string]any{
		"Sessions":       sorted,
		"ActiveSessions": activeSessions(sorted),
		"Sort":           sortBy,
		"Total":          len(sorted),
		"LoadingDone":    atomic.LoadInt32(&s.loadingDone) == 1,
		"LoadingCurrent": atomic.LoadInt32(&s.loadingCurrent),
		"LoadingTotal":   atomic.LoadInt32(&s.loadingTotal),
	}

	if r.Header.Get("HX-Request") == "true" {
		s.render(w, "session_list", data)
		return
	}
	s.render(w, "layout", data)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if q == "" {
		s.handleSessions(w, r)
		return
	}

	sessions := s.getSessions()
	var matches []*claude.Session

	for _, sess := range sessions {
		if matchesSession(sess, q) {
			matches = append(matches, sess)
		}
	}

	sort.Slice(matches, func(i, j int) bool {
		return matches[i].Modified.After(matches[j].Modified)
	})

	data := map[string]any{
		"Sessions":       matches,
		"ActiveSessions": activeSessions(matches),
		"Sort":           "recent",
		"Total":          len(matches),
		"Query":          r.URL.Query().Get("q"),
		"LoadingDone":    atomic.LoadInt32(&s.loadingDone) == 1,
		"LoadingCurrent": atomic.LoadInt32(&s.loadingCurrent),
		"LoadingTotal":   atomic.LoadInt32(&s.loadingTotal),
	}

	if r.Header.Get("HX-Request") == "true" {
		s.render(w, "session_list", data)
		return
	}
	s.render(w, "layout", data)
}

func matchesSession(sess *claude.Session, q string) bool {
	if strings.Contains(strings.ToLower(sess.Summary), q) ||
		strings.Contains(strings.ToLower(sess.FirstPrompt), q) ||
		strings.Contains(strings.ToLower(sess.Project), q) ||
		strings.Contains(strings.ToLower(sess.GitBranch), q) {
		return true
	}
	if sess.Loaded && strings.Contains(strings.ToLower(sess.MessagesText), q) {
		return true
	}
	return false
}

func (s *Server) handleSessionDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sess := s.getSession(id)
	if sess == nil {
		http.NotFound(w, r)
		return
	}

	sess.Mu.RLock()
	loaded := sess.Loaded
	sess.Mu.RUnlock()
	if !loaded {
		claude.LoadMessages(sess)
	}

	tmuxName, running := s.isRunning(id)

	data := map[string]any{
		"Session":   sess,
		"Running":   running,
		"TmuxName":  tmuxName,
		"AttachCmd": fmt.Sprintf("tmux attach -t %s", tmuxName),
	}

	// Fragment mode: return just the detail content for HTMX swap
	if r.URL.Query().Get("fragment") == "1" || r.Header.Get("HX-Request") == "true" {
		s.render(w, "session_detail", data)
		return
	}

	// Full page load: render the split layout with this session selected
	sessions := s.getSessions()
	sorted := make([]*claude.Session, len(sessions))
	copy(sorted, sessions)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Modified.After(sorted[j].Modified)
	})

	fullData := map[string]any{
		"Sessions":       sorted,
		"ActiveSessions": activeSessions(sorted),
		"Sort":           "recent",
		"Total":          len(sorted),
		"Detail":         data,
		"LoadingDone":    atomic.LoadInt32(&s.loadingDone) == 1,
		"LoadingCurrent": atomic.LoadInt32(&s.loadingCurrent),
		"LoadingTotal":   atomic.LoadInt32(&s.loadingTotal),
	}
	s.render(w, "layout", fullData)
}

func (s *Server) handleSessionStatus(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sess := s.getSession(id)
	if sess == nil {
		http.NotFound(w, r)
		return
	}

	tmuxName, running := s.isRunning(id)

	data := map[string]any{
		"Session":   sess,
		"Running":   running,
		"TmuxName":  tmuxName,
		"AttachCmd": fmt.Sprintf("tmux attach -t %s", tmuxName),
	}
	s.render(w, "terminal_section", data)
}

func (s *Server) handleEarlierMessages(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sess := s.getSession(id)
	if sess == nil {
		http.NotFound(w, r)
		return
	}

	sess.Mu.RLock()
	shownCount := len(sess.Messages)
	totalMessages := sess.TotalMessages
	sess.Mu.RUnlock()

	earlierCount := totalMessages - shownCount
	if earlierCount <= 0 {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<!-- no earlier messages -->"))
		return
	}

	to := earlierCount
	from := max(to-200, 0)

	earlier, err := claude.LoadMessagesRange(sess, from, to)
	if err != nil {
		http.Error(w, "failed to load messages", 500)
		return
	}

	data := map[string]any{
		"Messages":      earlier,
		"HasMore":       from > 0,
		"SessionID":     id,
		"RemainingCount": from,
	}
	s.render(w, "earlier_messages", data)
}

func (s *Server) handleTailMessages(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sess := s.getSession(id)
	if sess == nil {
		http.NotFound(w, r)
		return
	}

	afterStr := r.URL.Query().Get("after")
	after, _ := strconv.Atoi(afterStr)
	after = max(after, 0)

	claude.LoadMessages(sess)

	sess.Mu.RLock()
	defer sess.Mu.RUnlock()

	if after >= len(sess.Messages) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<!-- no new messages -->"))
		return
	}

	newMsgs := sess.Messages[after:]

	prevRole := ""
	if after > 0 {
		prevRole = sess.Messages[after-1].Role
	}

	data := map[string]any{
		"Messages": newMsgs,
		"PrevRole": prevRole,
		"Total":    len(sess.Messages),
	}
	s.render(w, "new_messages", data)
}

func (s *Server) handleLoading(w http.ResponseWriter, r *http.Request) {
	done := atomic.LoadInt32(&s.loadingDone) == 1
	if done {
		// Return empty div — no more polling
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<!-- loading complete -->"))
		return
	}
	data := map[string]any{
		"LoadingDone":    false,
		"LoadingCurrent": atomic.LoadInt32(&s.loadingCurrent),
		"LoadingTotal":   atomic.LoadInt32(&s.loadingTotal),
	}
	s.render(w, "loading_bar", data)
}

// SSE endpoint for live updates
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Subscribe to update notifications
	ch := s.subscribe()
	defer s.unsubscribe(ch)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			fmt.Fprintf(w, "event: update\ndata: %s\n\n", msg)
			flusher.Flush()
		}
	}
}
