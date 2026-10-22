package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/prompt"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/toolbridge"
)

const (
	sessionCacheTTL        = 30 * time.Minute
	sessionCacheMaxEntries = 256
)

type persistedSessionEntry struct {
	Key  string    `json:"key"`
	Meta []any     `json:"meta"`
	At   time.Time `json:"at"`
}

func (s *Server) sessionCacheDiskPath() string {
	if s.Cfg.HomeDir == "" {
		return ""
	}
	st, err := os.Stat(s.Cfg.HomeDir)
	if err != nil || !st.IsDir() {
		return ""
	}
	return filepath.Join(s.Cfg.HomeDir, "session-cache.json")
}

func (s *Server) loadSessionCacheFromDisk() {
	p := s.sessionCacheDiskPath()
	if p == "" {
		return
	}
	raw, err := os.ReadFile(p)
	if err != nil || len(raw) == 0 {
		return
	}
	var entries []persistedSessionEntry
	if err := json.Unmarshal(raw, &entries); err != nil || len(entries) == 0 {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range entries {
		if e.Key == "" || len(e.Meta) == 0 || now.Sub(e.At) >= sessionCacheTTL {
			continue
		}
		if _, exists := s.sessionCache[e.Key]; !exists {
			if len(s.sessionOrder) >= sessionCacheMaxEntries {
				oldest := s.sessionOrder[0]
				s.sessionOrder = s.sessionOrder[1:]
				delete(s.sessionCache, oldest)
				delete(s.sessionCacheAt, oldest)
			}
			s.sessionOrder = append(s.sessionOrder, e.Key)
		}
		s.sessionCache[e.Key] = e.Meta
		s.sessionCacheAt[e.Key] = e.At
	}
}

// FlushSessionCacheNow writes the cache synchronously and waits for any
// asynchronous write already in flight. Callers that flush at shutdown, or a test
// flushing during cleanup, would otherwise return while a background writer is
// still creating its temp file in a directory that may already be removed.
func (s *Server) FlushSessionCacheNow() {
	s.cacheFlushWG.Wait()
	s.flushSessionCacheLocked()
}

// flushSessionCacheLocked performs the write without waiting on the wait group, so
// the background writer can call it without deadlocking on itself.
func (s *Server) flushSessionCacheLocked() {
	p := s.sessionCacheDiskPath()
	if p == "" {
		return
	}
	s.cacheFlushMu.Lock()
	defer s.cacheFlushMu.Unlock()
	s.cacheFlushPending.Store(false)

	now := time.Now()
	s.mu.RLock()
	entries := make([]persistedSessionEntry, 0, len(s.sessionOrder))
	for _, k := range s.sessionOrder {
		meta := s.sessionCache[k]
		at := s.sessionCacheAt[k]
		if len(meta) == 0 || (!at.IsZero() && now.Sub(at) >= sessionCacheTTL) {
			continue
		}
		cp := make([]any, len(meta))
		copy(cp, meta)
		entries = append(entries, persistedSessionEntry{Key: k, Meta: cp, At: at})
	}
	s.mu.RUnlock()

	b, err := json.Marshal(entries)
	if err != nil {
		return
	}
	tmp := fmt.Sprintf("%s.tmp-%d", p, time.Now().UnixNano())
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		if err := os.Rename(tmp, p); err != nil {
			_ = os.Remove(tmp)
		}
	}
}

func (s *Server) flushSessionCacheToDisk() {
	if s.sessionCacheDiskPath() == "" {
		return
	}
	if !s.cacheFlushPending.CompareAndSwap(false, true) {
		return
	}
	s.cacheFlushWG.Add(1)
	go func() {
		defer s.cacheFlushWG.Done()
		s.flushSessionCacheLocked()
	}()
}

func (s *Server) sessionCacheLen() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessionCache)
}

// promoteSessionCacheLocked moves key to the newest position in sessionOrder.
// Callers must hold s.mu. A key missing from sessionOrder is appended so a live
// cache entry is never invisible to eviction.
func (s *Server) promoteSessionCacheLocked(key string) {
	for i, k := range s.sessionOrder {
		if k != key {
			continue
		}
		if i == len(s.sessionOrder)-1 {
			return
		}
		copy(s.sessionOrder[i:], s.sessionOrder[i+1:])
		s.sessionOrder[len(s.sessionOrder)-1] = key
		return
	}
	s.sessionOrder = append(s.sessionOrder, key)
}

// takeSessionCache consumes one continuation entry, promoting it only when a
// generation actually uses it. Hit/miss counters are updated once per consume
// attempt; affinity scans use peekSessionCache and never reach this method.
func (s *Server) takeSessionCache(key string) ([]any, bool) {
	s.mu.Lock()
	now := time.Now()
	src := s.sessionCache[key]
	at := s.sessionCacheAt[key]
	if len(src) == 0 || (!at.IsZero() && now.Sub(at) >= sessionCacheTTL) {
		s.mu.Unlock()
		s.cacheMisses.Add(1)
		return nil, false
	}
	cp := make([]any, len(src))
	copy(cp, src)
	s.promoteSessionCacheLocked(key)
	s.mu.Unlock()
	s.cacheHits.Add(1)
	return cp, true
}

// getSessionCache returns a copy of a live entry and records the lookup. It is
// retained for direct callers and tests; generation lookup uses a single
// selected key through takeSessionCache.
func (s *Server) getSessionCache(key string) []any {
	cp, _ := s.takeSessionCache(key)
	return cp
}

// peekSessionCache returns a copy of a live entry without recording hit/miss
// telemetry or changing LRU order. Account-affinity scans use this so routing
// probes do not distort /v1/sessions counters or eviction order.
func (s *Server) peekSessionCache(key string) []any {
	if key == "" {
		return nil
	}
	s.mu.RLock()
	now := time.Now()
	src := s.sessionCache[key]
	at := s.sessionCacheAt[key]
	if len(src) == 0 || (!at.IsZero() && now.Sub(at) >= sessionCacheTTL) {
		s.mu.RUnlock()
		return nil
	}
	cp := make([]any, len(src))
	copy(cp, src)
	s.mu.RUnlock()
	return cp
}

// deleteSessionCache removes a continuation entry and schedules a disk flush so
// the invalidation survives a restart. Concurrent callers and an empty key are
// safe; the flush is coalesced by the existing atomic writer.
func (s *Server) deleteSessionCache(key string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	delete(s.sessionCache, key)
	delete(s.sessionCacheAt, key)
	if len(s.sessionOrder) > 0 {
		kept := s.sessionOrder[:0]
		for _, k := range s.sessionOrder {
			if k != key {
				kept = append(kept, k)
			}
		}
		s.sessionOrder = kept
	}
	s.mu.Unlock()
	s.flushSessionCacheToDisk()
}

func (s *Server) putSessionCache(key string, meta []any) {
	if len(meta) == 0 {
		return
	}
	cp := make([]any, len(meta))
	copy(cp, meta)
	now := time.Now()
	s.mu.Lock()
	if _, exists := s.sessionCache[key]; !exists {
		if len(s.sessionOrder) >= sessionCacheMaxEntries {
			oldest := s.sessionOrder[0]
			s.sessionOrder = s.sessionOrder[1:]
			delete(s.sessionCache, oldest)
			delete(s.sessionCacheAt, oldest)
		}
	}
	s.sessionCache[key] = cp
	s.sessionCacheAt[key] = now
	s.promoteSessionCacheLocked(key)
	s.mu.Unlock()
	s.flushSessionCacheToDisk()
}

func (s *Server) saveSessionContinuation(
	accountID string,
	modelID string,
	messages []map[string]any,
	toolsRaw any,
	toolChoiceRaw any,
	content any,
	oaCalls []toolbridge.OpenAIToolCall,
	cid, rid, rcid, contextToken string,
	sessionKey string,
) {
	if !s.Cfg.SessionCache || cid == "" || rid == "" || rcid == "" {
		return
	}
	var replyMsg map[string]any
	if len(oaCalls) > 0 {
		tcMaps := make([]any, 0, len(oaCalls))
		for _, tc := range oaCalls {
			tcMaps = append(tcMaps, map[string]any{
				"id":   tc.ID,
				"type": tc.Type,
				"function": map[string]any{
					"name":      tc.Function.Name,
					"arguments": tc.Function.Arguments,
				},
			})
		}
		replyMsg = map[string]any{
			"role":       "assistant",
			"content":    content,
			"tool_calls": tcMaps,
		}
	} else {
		replyMsg = map[string]any{
			"role":    "assistant",
			"content": content,
		}
	}
	nextHistory := make([]map[string]any, 0, len(messages)+1)
	nextHistory = append(nextHistory, messages...)
	nextHistory = append(nextHistory, replyMsg)
	nextKey := prompt.HashHistoryWithTools(accountID, modelID, nextHistory, toolsRaw, toolChoiceRaw)
	meta := gemini.BuildContinuationMetadata(cid, rid, rcid, contextToken)
	s.putSessionCache(nextKey, meta)
	if sessionKey != "" && len(meta) > 0 {
		s.putSessionBinding(sessionBinding{
			Key:         sessionKey,
			AccountID:   accountID,
			Model:       modelID,
			Meta:        meta,
			NextLen:     len(nextHistory),
			HistoryHash: nextKey,
		})
	}
}

const (
	defaultSessionBindingTTL = 24 * time.Hour
	defaultSessionBindingMax = 1024
)

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
		if ok {
			s.deleteSessionBinding(key)
		}
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
	p := s.sessionBindingsDiskPath()
	if p == "" {
		return
	}
	s.mu.RLock()
	entries := make([]persistedSessionBinding, 0, len(s.sessionBindings))
	for _, b := range s.sessionBindings {
		entries = append(entries, persistedSessionBinding{
			Key: b.Key, AccountID: b.AccountID, Model: b.Model, Meta: b.Meta,
			NextLen: b.NextLen, HistoryHash: b.HistoryHash, At: b.At,
		})
	}
	s.mu.RUnlock()
	raw, err := json.Marshal(entries)
	if err != nil {
		return
	}
	tmp := fmt.Sprintf("%s.tmp-%d", p, time.Now().UnixNano())
	if err := os.WriteFile(tmp, raw, 0o600); err == nil {
		if err := os.Rename(tmp, p); err != nil {
			_ = os.Remove(tmp)
		}
	}
}

func (s *Server) getUploadCache(key string) (gemini.UploadedFile, bool) {
	if key == "" {
		return gemini.UploadedFile{}, false
	}
	now := time.Now()
	s.mu.Lock()
	cached, ok := s.uploadCache[key]
	if !ok || now.Sub(cached.At) >= 25*time.Minute {
		s.mu.Unlock()
		return gemini.UploadedFile{}, false
	}
	for i, k := range s.uploadOrder {
		if k != key {
			continue
		}
		if i != len(s.uploadOrder)-1 {
			copy(s.uploadOrder[i:], s.uploadOrder[i+1:])
			s.uploadOrder[len(s.uploadOrder)-1] = key
		}
		break
	}
	s.mu.Unlock()
	return cached.File, true
}

func (s *Server) uploadAttachments(
	ctx context.Context,
	acctID string,
	c UpstreamClient,
	parts []prompt.AttachmentPart,
) ([]gemini.UploadedFile, error) {
	if len(parts) == 0 {
		return nil, nil
	}
	files := make([]gemini.UploadedFile, 0, len(parts))
	for _, att := range parts {
		uf, err := s.uploadCached(ctx, acctID, c, att)
		if err != nil {
			return nil, err
		}
		files = append(files, uf)
	}
	return files, nil
}

func (s *Server) uploadCached(ctx context.Context, acctID string, c UpstreamClient, att prompt.AttachmentPart) (gemini.UploadedFile, error) {
	if len(att.Data) == 0 && att.RemoteURL != "" {
		urlSum := sha256.Sum256([]byte(att.RemoteURL))
		urlKey := acctID + ":url:" + hex.EncodeToString(urlSum[:])
		if cachedURL, okURL := s.getUploadCache(urlKey); okURL {
			return cachedURL, nil
		}
		fetched, err := prompt.FetchRemoteImage(ctx, att.RemoteURL, 0)
		if err != nil {
			return gemini.UploadedFile{}, err
		}
		att = *fetched
		uf, err := s.uploadCachedBytes(ctx, acctID, c, att)
		if err != nil {
			return gemini.UploadedFile{}, err
		}
		s.putUploadCache(urlKey, uf)
		return uf, nil
	}
	return s.uploadCachedBytes(ctx, acctID, c, att)
}

func (s *Server) uploadCachedBytes(ctx context.Context, acctID string, c UpstreamClient, att prompt.AttachmentPart) (gemini.UploadedFile, error) {
	sum := sha256.Sum256(att.Data)
	cacheKey := acctID + ":" + hex.EncodeToString(sum[:])

	if cached, ok := s.getUploadCache(cacheKey); ok {
		return cached, nil
	}

	uf, err := c.UploadFile(ctx, att.Data, att.Filename, att.ContentType)
	if err != nil {
		return gemini.UploadedFile{}, err
	}

	s.putUploadCache(cacheKey, uf)
	return uf, nil
}

func (s *Server) putUploadCache(cacheKey string, uf gemini.UploadedFile) {
	s.mu.Lock()
	if _, exists := s.uploadCache[cacheKey]; !exists {
		if len(s.uploadOrder) >= 128 {
			oldest := s.uploadOrder[0]
			s.uploadOrder = s.uploadOrder[1:]
			delete(s.uploadCache, oldest)
		}
		s.uploadOrder = append(s.uploadOrder, cacheKey)
	}
	s.uploadCache[cacheKey] = cachedUpload{File: uf, At: time.Now()}
	s.mu.Unlock()
}
