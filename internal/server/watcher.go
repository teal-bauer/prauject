package server

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/teal-bauer/prauject/internal/claude"
)

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (s *Server) startWatcher() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("watcher: %v", err)
		return
	}

	projectsDir := claude.ProjectsDir()

	// Recursively walk and watch all directories under projects/
	var dirCount int
	filepath.WalkDir(projectsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if watchErr := watcher.Add(path); watchErr != nil {
				log.Printf("watch %s: %v", path, watchErr)
			} else {
				dirCount++
			}
		}
		return nil
	})

	log.Printf("watching %d directories under %s", dirCount, projectsDir)

	// Also watch ~/.claude/sessions/ for PID file changes (active session detection)
	sessionsDir := filepath.Join(filepath.Dir(projectsDir), "sessions")
	if err := watcher.Add(sessionsDir); err == nil {
		dirCount++
	}

	var debounceTimer *time.Timer
	var pendingMu sync.Mutex
	var pendingPath string

	go func() {
		defer watcher.Close()
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}

				if event.Has(fsnotify.Create) {
					if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
						watcher.Add(event.Name)
					}
				}

				if !isRelevantChange(event) {
					continue
				}

				pendingMu.Lock()
				pendingPath = event.Name
				if debounceTimer != nil {
					debounceTimer.Stop()
				}
				debounceTimer = time.AfterFunc(500*time.Millisecond, func() {
					pendingMu.Lock()
					p := pendingPath
					pendingMu.Unlock()
					s.handleFileChange(p)
				})
				pendingMu.Unlock()

			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("watcher error: %v", err)
			}
		}
	}()
}

func isRelevantChange(event fsnotify.Event) bool {
	if !event.Has(fsnotify.Write) && !event.Has(fsnotify.Create) {
		return false
	}
	name := filepath.Base(event.Name)
	return strings.HasSuffix(name, ".jsonl") || name == "sessions-index.json" || strings.HasSuffix(name, ".json")
}

func (s *Server) handleFileChange(path string) {
	name := filepath.Base(path)

	// PID file change in sessions/ dir — just refresh active flags
	if strings.HasSuffix(name, ".json") && strings.Contains(path, "/sessions/") {
		active := claude.ScanActiveSessions()
		s.mu.RLock()
		for _, sess := range s.sessions {
			sess.Active = active[sess.ID]
		}
		s.mu.RUnlock()
		s.detectRunningTmux(s.getSessions())
		s.notify("sessions-updated")
		return
	}

	if name == "sessions-index.json" {
		log.Printf("index changed: %s, re-scanning", path)
		s.scanSessions()
		go s.backgroundLoad()
		s.notify("sessions-updated")
		return
	}

	if strings.HasSuffix(name, ".jsonl") {
		sessionID := strings.TrimSuffix(name, ".jsonl")

		s.mu.RLock()
		var sess *claude.Session
		for _, existing := range s.sessions {
			if existing.ID == sessionID {
				sess = existing
				break
			}
		}
		s.mu.RUnlock()

		if sess != nil {
			claude.LoadMessages(sess)
			s.notify("session-updated:" + sessionID)
		} else {
			// New session file — add it without re-scanning everything
			info, err := os.Stat(path)
			if err != nil {
				return
			}
			projDir := filepath.Dir(path)
			projDirName := filepath.Base(projDir)
			project := claude.CWDFromJSONL(path)
			if project == "" {
				project = claude.DecodeProjectPath(projDirName)
			}
			newSess := &claude.Session{
				ID:         sessionID,
				Project:    project,
				ProjectDir: projDirName,
				FilePath:   path,
				Modified:   info.ModTime(),
				Created:    info.ModTime(),
			}
			claude.LoadMessages(newSess)

			s.mu.Lock()
			s.sessions = append(s.sessions, newSess)
			s.mu.Unlock()

			log.Printf("new session: %s", shortID(sessionID))
			s.notify("sessions-updated")
		}
	}
}
