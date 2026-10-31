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

// sessionBinding is the persisted continuation for an explicit X-GSB-Session.
// The session cache is content-addressed; this type is session-id-addressed and
// records enough history state to validate that the caller is continuing the
// conversation the binding was created for.
type sessionBinding struct {
	Key         string
	AccountID   string
	Model       string
	Meta        []any
	NextLen     int
	HistoryHash string
	At          time.Time
}

type persistedSessionBinding struct {
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
	st, err := os.Stat(s.Cfg.HomeDir)
	if err != nil || !st.IsDir() {
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
	seed := "gsb-session\x00" + scope.Token + "\x00" + scope.AccountID + "\x00" + scope.Group + "\x00" + sessionID
	sum := sha256.Sum256([]byte(seed))
	return "sid:" + hex.EncodeToString(sum[:16])
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
	var entries []persistedSessionBinding
	if err := json.Unmarshal(raw, &entries); err != nil {
		return
	}
	// Persisted order is not guaranteed to be LRU order. Keep the newest
	// bindings when a legacy file contains more entries than the configured cap.
	sort.Slice(entries, func(i, j int) bool { return entries[i].At.After(entries[j].At) })
	now := time.Now()
	ttl := s.sessionBindingTTL()
	max := s.sessionBindingMax()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range entries {
		if e.Key == "" || e.AccountID == "" || len(e.Meta) == 0 || now.Sub(e.At) >= ttl {
			continue
		}
		if len(s.sessionBindings) >= max {
			break
		}
		s.sessionBindings[e.Key] = sessionBinding{
			Key: e.Key, AccountID: e.AccountID, Model: e.Model, Meta: e.Meta,
			NextLen: e.NextLen, HistoryHash: e.HistoryHash, At: e.At,
		}
	}
}

func (s *Server) getSessionBinding(key string) *sessionBinding {
	if key == "" {
		return nil
	}
	now := time.Now()
	s.mu.RLock()
	b, ok := s.sessionBindings[key]
	s.mu.RUnlock()
	if !ok || now.Sub(b.At) >= s.sessionBindingTTL() {
		// Expired entries are ignored here; the next put/load pass removes them.
		// Reading a binding must not trigger disk I/O from prepareGeneration.
		return nil
	}
	cp := b
	cp.Meta = append([]any(nil), b.Meta...)
	return &cp
}

func (s *Server) putSessionBinding(b sessionBinding) {
	if b.Key == "" || b.AccountID == "" || len(b.Meta) == 0 {
		return
	}
	b.Meta = append([]any(nil), b.Meta...)
	b.At = time.Now()
	s.mu.Lock()
	if s.sessionBindings == nil {
		s.sessionBindings = make(map[string]sessionBinding)
	}
	ttl := s.sessionBindingTTL()
	now := b.At
	for k, existing := range s.sessionBindings {
		if now.Sub(existing.At) >= ttl {
			delete(s.sessionBindings, k)
		}
	}
	max := s.sessionBindingMax()
	for len(s.sessionBindings) >= max {
		oldestKey := ""
		var oldest time.Time
		for k, existing := range s.sessionBindings {
			if oldestKey == "" || existing.At.Before(oldest) {
				oldestKey, oldest = k, existing.At
			}
		}
		if oldestKey == "" {
			break
		}
		delete(s.sessionBindings, oldestKey)
	}
	s.sessionBindings[b.Key] = b
	s.mu.Unlock()
	s.flushSessionBindings()
}

func (s *Server) deleteSessionBinding(key string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	if _, ok := s.sessionBindings[key]; !ok {
		s.mu.Unlock()
		return
	}
	delete(s.sessionBindings, key)
	s.mu.Unlock()
	s.flushSessionBindings()
}

func (s *Server) flushSessionBindings() {
	if s.sessionBindingsDiskPath() == "" {
		return
	}
	if !s.bindingFlushPending.CompareAndSwap(false, true) {
		return
	}
	s.bindingFlushWG.Add(1)
	go func() {
		defer s.bindingFlushWG.Done()
		s.flushSessionBindingsLocked()
	}()
}

// flushSessionBindingsLocked writes the current binding snapshot. cacheFlushMu
// serializes it with the continuation-cache writer so two concurrent turns
// cannot install an older snapshot over a newer one.
func (s *Server) flushSessionBindingsLocked() {
	p := s.sessionBindingsDiskPath()
	if p == "" {
		return
	}
	s.cacheFlushMu.Lock()
	defer s.cacheFlushMu.Unlock()
	s.bindingFlushPending.Store(false)

	s.mu.RLock()
	entries := make([]persistedSessionBinding, 0, len(s.sessionBindings))
	for _, b := range s.sessionBindings {
		entries = append(entries, persistedSessionBinding{
			Key: b.Key, AccountID: b.AccountID, Model: b.Model, Meta: b.Meta,
			NextLen: b.NextLen, HistoryHash: b.HistoryHash, At: b.At,
		})
	}
	s.mu.RUnlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].At.After(entries[j].At) })
	_ = writeJSONAtomically(p, entries)
}
