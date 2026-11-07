package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	defaultSessionBindingTTL = 24 * time.Hour
	defaultSessionBindingMax = 1024
)

type sessionBinding struct {
	Key         string    `json:"key"`
	AccountID   string    `json:"account"`
	Model       string    `json:"model"`
	Meta        []any     `json:"meta"`
	NextLen     int       `json:"nextLen"`
	HistoryHash string    `json:"historyHash"`
	At          time.Time `json:"at"`
}

func (s *Server) sessionBindingsDiskPath() string {
	if s.Cfg.HomeDir == "" {
		return ""
	}
	if st, err := os.Stat(s.Cfg.HomeDir); err != nil || !st.IsDir() {
		return ""
	}
	return filepath.Join(s.Cfg.HomeDir, "session-bindings.json")
}

func (s *Server) sessionBindingTTL() time.Duration {
	if s.Cfg.SessionBindingTTL > 0 {
		return s.Cfg.SessionBindingTTL
	}
	return defaultSessionBindingTTL
}

func (s *Server) sessionBindingMax() int {
	if s.Cfg.SessionBindingMax > 0 {
		return s.Cfg.SessionBindingMax
	}
	return defaultSessionBindingMax
}

func sessionBindingKey(scope AuthScope, sessionID string) string {
	tok, acct := scope.Token, scope.AccountID
	if scope.Pool {
		tok, acct = "pool", ""
	}
	sum := sha256.Sum256([]byte("gsb-session\x00" + tok + "\x00" + acct + "\x00" + scope.Group + "\x00" + sessionID))
	return "sid:" + hex.EncodeToString(sum[:16])
}

func (s *Server) sessionBindingLen() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.bindings)
}

func (s *Server) loadSessionBindingsFromDisk() {
	p := s.sessionBindingsDiskPath()
	if p == "" {
		return
	}
	raw, err := os.ReadFile(p)
	if err != nil || len(raw) == 0 {
		return
	}
	var entries []sessionBinding
	if err := json.Unmarshal(raw, &entries); err != nil {
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].At.After(entries[j].At) })
	now, ttl, max := s.now(), s.sessionBindingTTL(), s.sessionBindingMax()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range entries {
		if e.Key == "" || e.AccountID == "" || len(e.Meta) == 0 || now.Sub(e.At) >= ttl {
			continue
		}
		if len(s.bindings) >= max {
			break
		}
		s.bindings[e.Key] = e
	}
}

func (s *Server) getSessionBinding(key string) *sessionBinding {
	if key == "" {
		return nil
	}
	now := s.now()
	s.mu.RLock()
	b, ok := s.bindings[key]
	s.mu.RUnlock()
	if !ok || now.Sub(b.At) >= s.sessionBindingTTL() {
		return nil
	}
	b.Meta = append([]any(nil), b.Meta...)
	return &b
}

func (s *Server) putSessionBinding(b sessionBinding) {
	if b.Key == "" || b.AccountID == "" || len(b.Meta) == 0 {
		return
	}
	b.Meta = append([]any(nil), b.Meta...)
	b.At = s.now()
	ttl, max := s.sessionBindingTTL(), s.sessionBindingMax()
	s.mu.Lock()
	if s.bindings == nil {
		s.bindings = make(map[string]sessionBinding)
	}
	for k, existing := range s.bindings {
		if b.At.Sub(existing.At) >= ttl {
			delete(s.bindings, k)
		}
	}
	for len(s.bindings) >= max {
		oldestKey := ""
		var oldest time.Time
		for k, existing := range s.bindings {
			if oldestKey == "" || existing.At.Before(oldest) {
				oldestKey, oldest = k, existing.At
			}
		}
		if oldestKey == "" {
			break
		}
		delete(s.bindings, oldestKey)
	}
	s.bindings[b.Key] = b
	s.mu.Unlock()
	s.flushSessionBindings()
}

func (s *Server) deleteSessionBinding(key string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	_, ok := s.bindings[key]
	delete(s.bindings, key)
	s.mu.Unlock()
	if ok {
		s.flushSessionBindings()
	}
}

func (s *Server) purgeAccountSessionBindings(accountID string) {
	if accountID == "" {
		return
	}
	removed := false
	s.mu.Lock()
	for k, b := range s.bindings {
		if b.AccountID == accountID {
			delete(s.bindings, k)
			removed = true
		}
	}
	s.mu.Unlock()
	if removed {
		s.flushSessionBindings()
	}
}

func (s *Server) flushSessionBindings() {
	if s.sessionBindingsDiskPath() == "" || !s.bindFlushPending.CompareAndSwap(false, true) {
		return
	}
	s.cacheFlushWG.Add(1)
	go func() {
		defer s.cacheFlushWG.Done()
		s.flushSessionBindingsLocked()
	}()
}

func (s *Server) flushSessionBindingsLocked() {
	p := s.sessionBindingsDiskPath()
	if p == "" {
		return
	}
	s.cacheFlushMu.Lock()
	defer s.cacheFlushMu.Unlock()
	s.bindFlushPending.Store(false)

	now, ttl := s.now(), s.sessionBindingTTL()
	s.mu.RLock()
	entries := make([]sessionBinding, 0, len(s.bindings))
	for _, b := range s.bindings {
		if now.Sub(b.At) < ttl {
			b.Meta = append([]any(nil), b.Meta...)
			entries = append(entries, b)
		}
	}
	s.mu.RUnlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].At.After(entries[j].At) })
	writeJSONAtomically(p, entries)
}
