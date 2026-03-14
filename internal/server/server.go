package server

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/teal-bauer/prauject/internal/claude"
)

//go:embed static
var staticFS embed.FS

//go:embed templates
var templateFS embed.FS

type Server struct {
	router     *chi.Mux
	dev        bool
	claudeArgs []string
	hub        *Hub
	sessions   []*claude.Session
	mu         sync.RWMutex

	// track running tmux sessions: sessionID -> tmux session name
	running   map[string]string
	runningMu sync.RWMutex

	loadingDone    int32
	loadingCurrent int32
	loadingTotal   int32
	loadingRunning int32

	subs   map[chan string]bool
	subsMu sync.RWMutex

	cachedTmpl *template.Template
}

func New(dev bool, claudeArgs []string) (*Server, error) {
	if len(claudeArgs) > 0 {
		log.Printf("claude args: %v", claudeArgs)
	} else {
		log.Printf("claude args: (none)")
	}

	s := &Server{
		dev:        dev,
		claudeArgs: claudeArgs,
		hub:        newHub(),
		running:    make(map[string]string),
		subs:       make(map[chan string]bool),
	}

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Static files
	staticSub, _ := fs.Sub(staticFS, "static")
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

	// Routes
	r.Get("/", s.handleSessions)
	r.Get("/search", s.handleSearch)
	r.Get("/sessions/{id}", s.handleSessionDetail)
	r.Post("/sessions/{id}/resume", s.handleResume)
	r.Get("/sessions/{id}/ws", s.handleWebSocket)
	r.Get("/sessions/{id}/status", s.handleSessionStatus)
	r.Get("/sessions/{id}/earlier", s.handleEarlierMessages)
	r.Get("/sessions/{id}/tail", s.handleTailMessages)
	r.Get("/events", s.handleEvents)
	r.Get("/loading", s.handleLoading)

	s.router = r

	if !dev {
		tmpl, err := s.parseTemplates()
		if err != nil {
			return nil, fmt.Errorf("parse templates: %w", err)
		}
		s.cachedTmpl = tmpl
	}

	go s.scanSessions()

	// Background loading of message content
	go s.backgroundLoad()

	// Watch for file changes
	go s.startWatcher()

	return s, nil
}

func (s *Server) ListenAndServe(addr string) error {
	return http.ListenAndServe(addr, s.router)
}

func (s *Server) detectRunningTmux(sessions []*claude.Session) {
	out, err := exec.Command("tmux", "list-sessions", "-F", "#{session_name}").Output()
	if err != nil {
		return
	}
	tmuxSessions := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.HasPrefix(line, "cc-") {
			tmuxSessions[line] = true
		}
	}
	if len(tmuxSessions) == 0 {
		return
	}
	s.runningMu.Lock()
	defer s.runningMu.Unlock()
	for _, sess := range sessions {
		if len(sess.ID) < 8 {
			continue
		}
		tmuxName := "cc-" + sess.ID[:8]
		if tmuxSessions[tmuxName] {
			s.running[sess.ID] = tmuxName
			go s.monitorTmux(sess.ID, tmuxName)
		}
	}
	if len(s.running) > 0 {
		log.Printf("found %d existing tmux sessions", len(s.running))
	}
}

func (s *Server) scanSessions() {
	sessions, err := claude.ScanSessions()
	if err != nil {
		log.Printf("scan sessions: %v", err)
		return
	}

	// Preserve loaded data from existing sessions
	s.mu.Lock()
	existing := make(map[string]*claude.Session, len(s.sessions))
	for _, sess := range s.sessions {
		existing[sess.ID] = sess
	}
	for i, newSess := range sessions {
		if old, ok := existing[newSess.ID]; ok && old.Loaded {
			old.Mu.RLock()
			newSess.Messages = old.Messages
			newSess.TotalMessages = old.TotalMessages
			newSess.MessagesText = old.MessagesText
			newSess.Loaded = old.Loaded
			newSess.FullyLoaded = old.FullyLoaded
			newSess.LastMessage = old.LastMessage
			if newSess.FirstPrompt == "" {
				newSess.FirstPrompt = old.FirstPrompt
			}
			old.Mu.RUnlock()
			sessions[i] = newSess
		}
	}
	s.sessions = sessions
	s.mu.Unlock()

	s.detectRunningTmux(sessions)
	log.Printf("found %d sessions", len(sessions))
}

func (s *Server) backgroundLoad() {
	if !atomic.CompareAndSwapInt32(&s.loadingRunning, 0, 1) {
		return
	}
	defer atomic.StoreInt32(&s.loadingRunning, 0)

	for {
		s.mu.RLock()
		n := len(s.sessions)
		s.mu.RUnlock()
		if n > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	s.mu.RLock()
	var unloaded []*claude.Session
	for _, sess := range s.sessions {
		if !sess.Loaded {
			unloaded = append(unloaded, sess)
		}
	}
	s.mu.RUnlock()

	if len(unloaded) == 0 {
		atomic.StoreInt32(&s.loadingDone, 1)
		return
	}

	total := len(unloaded)
	atomic.StoreInt32(&s.loadingTotal, int32(total))
	atomic.StoreInt32(&s.loadingCurrent, 0)
	atomic.StoreInt32(&s.loadingDone, 0)

	const workers = 16
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, sess := range unloaded {
		wg.Add(1)
		sem <- struct{}{}
		go func(sess *claude.Session) {
			defer wg.Done()
			defer func() { <-sem }()
			start := time.Now()
			claude.LoadMessages(sess)
			if elapsed := time.Since(start); elapsed > time.Second {
				sid := sess.ID
				if len(sid) > 8 {
					sid = sid[:8]
				}
				log.Printf("slow load: %s (%s, %s)", sid, sess.Project, elapsed.Round(time.Millisecond))
			}
			atomic.AddInt32(&s.loadingCurrent, 1)
		}(sess)
	}
	wg.Wait()

	atomic.StoreInt32(&s.loadingDone, 1)
	s.notify("loading-complete")
	log.Printf("background loading complete (%d sessions)", total)
}

func (s *Server) subscribe() chan string {
	ch := make(chan string, 16)
	s.subsMu.Lock()
	s.subs[ch] = true
	s.subsMu.Unlock()
	return ch
}

func (s *Server) unsubscribe(ch chan string) {
	s.subsMu.Lock()
	delete(s.subs, ch)
	s.subsMu.Unlock()
}

func (s *Server) notify(msg string) {
	s.subsMu.RLock()
	defer s.subsMu.RUnlock()
	for ch := range s.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

func (s *Server) getSessions() []*claude.Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions
}

func (s *Server) getSession(id string) *claude.Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sess := range s.sessions {
		if sess.ID == id {
			return sess
		}
	}
	return nil
}

func (s *Server) isRunning(id string) (string, bool) {
	s.runningMu.RLock()
	defer s.runningMu.RUnlock()
	name, ok := s.running[id]
	return name, ok
}

func (s *Server) parseTemplates() (*template.Template, error) {
	funcMap := template.FuncMap{
		"humanTime": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return humanize.Time(t)
		},
		"shortTime": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.Format("Jan 2 15:04")
		},
		"sub": func(a, b int) int {
			return a - b
		},
		"divf": func(a, b int32) float64 {
			if b == 0 {
				return 0
			}
			return float64(a) / float64(b) * 100
		},
		"isNoise": func(s string) bool {
			return claude.IsSystemNoise(s)
		},
		"truncate": func(s string, n int) string {
			if len(s) <= n {
				return s
			}
			return s[:n] + "…"
		},
		"shortProject": func(p string) string {
			parts := strings.Split(p, "/")
			if len(parts) > 2 {
				return strings.Join(parts[len(parts)-2:], "/")
			}
			return p
		},
		"isRunning": func(id string) bool {
			_, ok := s.isRunning(id)
			return ok
		},
		"groupByProject": func(sessions []*claude.Session) []projectGroup {
			groups := map[string]*projectGroup{}
			var order []string
			for _, sess := range sessions {
				key := sess.Project
				if g, ok := groups[key]; ok {
					g.Sessions = append(g.Sessions, sess)
				} else {
					groups[key] = &projectGroup{
						Project:  key,
						Sessions: []*claude.Session{sess},
					}
					order = append(order, key)
				}
			}
			var result []projectGroup
			for _, k := range order {
				g := groups[k]
				sort.Slice(g.Sessions, func(i, j int) bool {
					return g.Sessions[i].Modified.After(g.Sessions[j].Modified)
				})
				result = append(result, *g)
			}
			return result
		},
	}

	if s.dev {
		return template.New("").Funcs(funcMap).ParseGlob("internal/server/templates/*.html")
	}
	return template.New("").Funcs(funcMap).ParseFS(templateFS, "templates/*.html")
}

type projectGroup struct {
	Project  string
	Sessions []*claude.Session
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	var tmpl *template.Template
	var err error
	if s.cachedTmpl != nil {
		tmpl = s.cachedTmpl
	} else {
		tmpl, err = s.parseTemplates()
		if err != nil {
			log.Printf("parse templates: %v", err)
			http.Error(w, "template error", 500)
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}
