package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/config"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/cookies"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
)

var reSafeID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// UpstreamClient abstracts an account's Gemini Web operations so unit tests can
// inject deterministic frame streams while production uses *gemini.AccountClient.
type UpstreamClient interface {
	Bootstrap(ctx context.Context) error
	AvailableModels(ctx context.Context, force bool) (*gemini.AvailableModelsResult, error)
	GetQuota(ctx context.Context) []gemini.QuotaBucket
	UploadFile(ctx context.Context, data []byte, filename, contentType string) (gemini.UploadedFile, error)
	StreamGenerateLive(ctx context.Context, promptText string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error)
	RotateCookies(ctx context.Context) (*cookies.RotateResult, error)
	ReloadFromDiskIfNewer() bool
	InvalidateAuth()
}

// AccountState holds the server-side state and cached metadata for one Google account.
type AccountState struct {
	mu              sync.RWMutex
	ID              string
	Email           string
	FilePath        string
	CapturedAt      time.Time
	Client          UpstreamClient
	Models          []gemini.ModelInfo
	Capacity        int
	Quota           []gemini.QuotaBucket
	QuotaAt         time.Time
	quotaRefreshing bool
	ModelsAt        time.Time
	ModelsError     error
	Fails           int
	LastFail        time.Time
	LastError       string
	LastUsed        time.Time
}

// AuthScope represents the caller's resolved token scope.
type AuthScope struct {
	OK        bool
	AccountID string
	Admin     bool
}

type cachedUpload struct {
	File gemini.UploadedFile
	At   time.Time
}

// Server is the browserless Gemini Stealth Bridge HTTP daemon.
type Server struct {
	Cfg     config.Config
	Pool    *transport.Pool
	Rotator *cookies.Rotator

	mu             sync.RWMutex
	accounts       []*AccountState
	tokenToAcct    map[string]string
	sessionCache   map[string][]any
	sessionCacheAt map[string]time.Time
	sessionOrder   []string
	uploadCache    map[string]cachedUpload
	uploadOrder    []string
	lastHotLoadAt  time.Time
	clientFactory  func(a *AccountState) UpstreamClient
}

// New constructs a new Server from cfg.
func New(cfg config.Config) *Server {
	maxSessions := cfg.MaxSessions
	if maxSessions <= 0 {
		maxSessions = 16
	}
	pool := transport.NewPool(transport.PoolOptions{
		ClientConfig: transport.ClientConfig{
			ProfileName:      cfg.ImpersonateProfile,
			ProxyURL:         cfg.ProxyURL,
			UserAgent:        cfg.UserAgent,
			SecCHUA:          cfg.SecCHUA,
			SecCHUAPlatform:  cfg.SecCHUAPlatform,
			Timeout:          cfg.StreamTimeout,
			FirstByteTimeout: cfg.FirstByteTimeout,
			StallTimeout:     cfg.StallTimeout,
		},
		MaxSessions: maxSessions,
		MaxQueue:    cfg.MaxQueueDepth,
		WaitTimeout: cfg.SessionWaitTimeout,
	})
	rotator := cookies.NewRotator()

	tokMap := make(map[string]string, len(cfg.AccountTokens))
	for k, v := range cfg.AccountTokens {
		tokMap[k] = v
	}

	s := &Server{
		Cfg:            cfg,
		Pool:           pool,
		Rotator:        rotator,
		tokenToAcct:    tokMap,
		sessionCache:   make(map[string][]any),
		sessionCacheAt: make(map[string]time.Time),
		uploadCache:    make(map[string]cachedUpload),
	}
	s.loadSessionCacheFromDisk()
	return s
}

// extractRequestToken extracts the caller's API key from Authorization Bearer,
// x-api-key, x-goog-api-key, or ?key= query parameter.
func extractRequestToken(r *http.Request) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(auth) > 7 && strings.EqualFold(auth[:7], "Bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	for _, h := range []string{"x-api-key", "x-goog-api-key"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return v
		}
	}
	if r.URL != nil {
		if v := strings.TrimSpace(r.URL.Query().Get("key")); v != "" {
			return v
		}
	}
	return ""
}

// ResolveAuthScope resolves the caller's API key into an AuthScope.
func (s *Server) ResolveAuthScope(r *http.Request) AuthScope {
	token := extractRequestToken(r)

	s.mu.RLock()
	if acctID, ok := s.tokenToAcct[token]; ok && token != "" {
		s.mu.RUnlock()
		return AuthScope{OK: true, AccountID: acctID}
	}
	explicitTokens := len(s.Cfg.AccountTokens) > 0
	s.mu.RUnlock()

	// Hot-load newly added account files on the fly when not locked by explicit BRIDGE_ACCOUNT_TOKENS,
	// rate-limited to avoid disk/SQLite scans on repeated invalid tokens.
	if token != "" && !explicitTokens && (strings.HasPrefix(token, "gsb-") || reSafeID.MatchString(token)) {
		s.mu.RLock()
		canReload := time.Since(s.lastHotLoadAt) >= 2*time.Second
		s.mu.RUnlock()
		if canReload {
			s.mu.Lock()
			s.lastHotLoadAt = time.Now()
			s.mu.Unlock()
			s.LoadCredentials()
			s.mu.RLock()
			acctID, ok := s.tokenToAcct[token]
			s.mu.RUnlock()
			if ok {
				return AuthScope{OK: true, AccountID: acctID}
			}
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.tokenToAcct) > 0 || explicitTokens {
		if s.Cfg.APIKey != "public" && token == s.Cfg.APIKey {
			return AuthScope{OK: true, Admin: true}
		}
		if s.Cfg.APIKey == "public" && (token == "" || token == "public") {
			if len(s.accounts) == 1 {
				return AuthScope{OK: true, AccountID: s.accounts[0].ID}
			}
			return AuthScope{OK: true, Admin: true}
		}
		return AuthScope{OK: false}
	}

	if s.Cfg.APIKey == "public" {
		if len(s.accounts) == 1 {
			return AuthScope{OK: true, AccountID: s.accounts[0].ID}
		}
		return AuthScope{OK: true, Admin: true}
	}
	if token == s.Cfg.APIKey {
		if len(s.accounts) == 1 {
			return AuthScope{OK: true, AccountID: s.accounts[0].ID}
		}
		return AuthScope{OK: true, Admin: true}
	}
	return AuthScope{OK: false}
}

// ServeHTTP implements http.Handler for all bridge endpoints.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	ctx := r.Context()

	switch {
	case r.Method == http.MethodGet && (p == "/health" || p == "/"):
		accts := s.Accounts()
		models := len(s.allModels())
		sendJSON(w, 200, map[string]any{
			"status":    "ok",
			"service":   "gemini-stealth-bridge",
			"transport": "stealth-utls-h2",
			"profile":   s.Cfg.ImpersonateProfile,
			"accounts":  len(accts),
			"models":    models,
			"upstream":  "gemini.google.com",
		}, nil)
		return

	case r.Method == http.MethodGet && (p == "/v1/sessions" || p == "/sessions"):
		scope := s.ResolveAuthScope(r)
		if !scope.OK {
			sendJSON(w, 401, map[string]any{"error": map[string]any{"message": "invalid api key", "type": "authentication_error"}}, nil)
			return
		}
		sendJSON(w, 200, map[string]any{
			"transport": "stealth-utls-h2",
			"profile":   s.Cfg.ImpersonateProfile,
			"sessions":  s.Pool.Status(),
		}, nil)
		return

	case r.Method == http.MethodGet && (p == "/v1/accounts" || p == "/accounts"):
		scope := s.ResolveAuthScope(r)
		if !scope.OK {
			sendJSON(w, 401, map[string]any{"error": map[string]any{"message": "invalid api key", "type": "authentication_error"}}, nil)
			return
		}
		s.EnsureModels(ctx, false)
		pool := s.scopedAccounts(scope)
		list := make([]map[string]any, 0, len(pool))
		for _, a := range pool {
			a.mu.RLock()
			mNames := make([]string, 0, len(a.Models))
			for _, m := range a.Models {
				mNames = append(mNames, m.ModelName)
			}
			list = append(list, map[string]any{
				"id":       a.ID,
				"email":    a.Email,
				"capacity": a.Capacity,
				"fails":    a.Fails,
				"models":   mNames,
				"quota":    a.Quota,
			})
			a.mu.RUnlock()
		}
		sendJSON(w, 200, map[string]any{"accounts": list}, nil)
		return

	case r.Method == http.MethodGet && (p == "/v1/quota" || p == "/quota"):
		scope := s.ResolveAuthScope(r)
		if !scope.OK {
			sendJSON(w, 401, map[string]any{"error": map[string]any{"message": "invalid api key", "type": "authentication_error"}}, nil)
			return
		}
		s.EnsureModels(ctx, false)
		pool := s.scopedAccounts(scope)
		var wg sync.WaitGroup
		for _, a := range pool {
			wg.Add(1)
			go func(acct *AccountState) {
				defer wg.Done()
				q := s.clientFor(acct).GetQuota(ctx)
				if len(q) > 0 {
					s.mergeAccountQuota(acct, q)
				}
			}(a)
		}
		wg.Wait()
		list := make([]map[string]any, 0, len(pool))
		for _, a := range pool {
			a.mu.RLock()
			list = append(list, map[string]any{
				"id":    a.ID,
				"email": a.Email,
				"quota": a.Quota,
			})
			a.mu.RUnlock()
		}
		sendJSON(w, 200, map[string]any{"accounts": list}, nil)
		return

	case r.Method == http.MethodGet && (p == "/v1/models" || p == "/models"):
		scope := s.ResolveAuthScope(r)
		if !scope.OK {
			sendJSON(w, 401, map[string]any{"error": map[string]any{"message": "invalid api key", "type": "authentication_error"}}, nil)
			return
		}
		accts := s.EnsureModels(ctx, false)
		if len(accts) == 0 {
			sendJSON(w, 503, map[string]any{"error": map[string]any{"message": "no accounts configured", "type": "auth_unavailable"}}, nil)
			return
		}
		pool := s.scopedAccounts(scope)
		allEmpty := len(pool) > 0
		var firstErr error
		for _, a := range pool {
			a.mu.RLock()
			if len(a.Models) > 0 {
				allEmpty = false
			}
			if firstErr == nil && a.ModelsError != nil {
				firstErr = a.ModelsError
			}
			a.mu.RUnlock()
		}
		if allEmpty && firstErr != nil {
			sendChatError(w, false, firstErr)
			return
		}

		seen := make(map[string]bool)
		var models []gemini.ModelInfo
		for _, a := range pool {
			a.mu.RLock()
			for _, m := range a.Models {
				if !seen[m.ModelName] {
					seen[m.ModelName] = true
					models = append(models, m)
				}
			}
			a.mu.RUnlock()
		}
		nowSec := time.Now().Unix()
		data := make([]map[string]any, 0, len(models))
		for _, x := range models {
			isReasoning := gemini.IsReasoningModel(x.ModelName)
			defThinkStr := "low"
			if isReasoning {
				defThinkStr = "high"
			}
			entry := map[string]any{
				"id":                    x.ModelName,
				"object":                "model",
				"created":               nowSec,
				"owned_by":              "google",
				"context_length":        1048576,
				"context_window":        1048576,
				"max_completion_tokens": 65536,
				"reasoning":             isReasoning,
				"thinking": map[string]any{
					"mode":           "effort",
					"default":        defThinkStr,
					"defaultLevel":   defThinkStr,
					"efforts":        gemini.SupportedThinkingEfforts,
					"requiresEffort": true,
				},
				"compat": map[string]any{
					"supportsReasoningEffort": isReasoning,
				},
				"capabilities": map[string]any{
					"vision":                  true,
					"pdf":                     false,
					"audioInput":              false,
					"videoInput":              false,
					"imageOutput":             false,
					"audioOutput":             false,
					"search":                  true,
					"tools":                   true,
					"reasoning":               isReasoning,
					"thinkingFormat":          "gemini-level",
					"thinkingCanDisable":      false,
					"thinkingEffortSupported": isReasoning,
					"thinkingLevels":          gemini.SupportedThinkingEfforts,
					"defaultThinkingLevel":    defThinkStr,
					"contextWindow":           1048576,
					"maxOutput":               65536,
				},
			}
			data = append(data, entry)
		}
		sendJSON(w, 200, map[string]any{"object": "list", "data": data}, nil)
		return

	case r.Method == http.MethodPost && (p == "/v1/chat/completions" || p == "/chat/completions"):
		scope := s.ResolveAuthScope(r)
		if !scope.OK {
			sendJSON(w, 401, map[string]any{"error": map[string]any{"message": "invalid api key", "type": "authentication_error"}}, nil)
			return
		}
		body, ok := readJSONBody(w, r)
		if !ok {
			return
		}
		s.HandleChat(ctx, w, body, scope)
		return

	case r.Method == http.MethodPost && (p == "/v1/responses" || p == "/responses"):
		scope := s.ResolveAuthScope(r)
		if !scope.OK {
			sendJSON(w, 401, map[string]any{"error": map[string]any{"message": "invalid api key", "type": "authentication_error"}}, nil)
			return
		}
		body, ok := readJSONBody(w, r)
		if !ok {
			return
		}
		s.HandleResponses(ctx, w, body, scope)
		return
	case r.Method == http.MethodPost && (p == "/v1/refresh" || p == "/refresh"):
		if r.Header.Get("Origin") != "" {
			sendJSON(w, 403, map[string]any{"error": map[string]any{"message": "origin not allowed", "type": "authentication_error"}}, nil)
			return
		}
		scope := s.ResolveAuthScope(r)
		if !scope.OK {
			sendJSON(w, 401, map[string]any{"error": map[string]any{"message": "invalid api key", "type": "authentication_error"}}, nil)
			return
		}
		s.LoadCredentials()
		accts := s.EnsureModels(ctx, true)
		sendJSON(w, 200, map[string]any{
			"status":   "ok",
			"accounts": len(accts),
			"models":   len(s.allModels()),
		}, nil)
		return

	default:
		sendJSON(w, 404, map[string]any{
			"error": map[string]any{
				"message": "not found: " + p,
				"type":    "invalid_request_error",
			},
		}, nil)
	}
}

func readJSONBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	const maxRequestBodyBytes = 32 * 1024 * 1024
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes+1))
	if err != nil {
		sendJSON(w, 400, map[string]any{"error": map[string]any{"message": "invalid request body", "type": "invalid_request_error"}}, nil)
		return nil, false
	}
	if len(raw) > maxRequestBodyBytes {
		sendJSON(w, 413, map[string]any{"error": map[string]any{"message": "request body exceeds 32MB limit", "type": "invalid_request_error"}}, nil)
		return nil, false
	}
	var body map[string]any
	if len(raw) == 0 {
		body = map[string]any{}
	} else if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		sendJSON(w, 400, map[string]any{"error": map[string]any{"message": "invalid JSON", "type": "invalid_request_error"}}, nil)
		return nil, false
	}
	return body, true
}

// StartBackgroundWorkers starts periodic model warming, dual-path cookie rotation,
// and idle session reaping until ctx is cancelled.
func (s *Server) StartBackgroundWorkers(ctx context.Context) {
	// Warm model registry periodically
	refreshEvery := s.Cfg.ModelRefreshEvery
	if refreshEvery <= 0 {
		refreshEvery = 10 * time.Minute
	}
	go func() {
		wCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		s.EnsureModels(wCtx, false)
		cancel()
		ticker := time.NewTicker(refreshEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				rCtx, rCancel := context.WithTimeout(ctx, 45*time.Second)
				s.EnsureModels(rCtx, true)
				rCancel()
			}
		}
	}()

	// Reap idle transport sessions every 60 seconds
	if s.Cfg.IdleSessionTimeout > 0 {
		go func() {
			ticker := time.NewTicker(60 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					s.Pool.ReapIdle(s.Cfg.IdleSessionTimeout)
				}
			}
		}()
	}

	// Proactive dual-path cookie rotation (__Secure-1PSIDTS + SIDCC)
	if s.Cfg.RotateCookiesEvery > 0 {
		go func() {
			ticker := time.NewTicker(s.Cfg.RotateCookiesEvery)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					for _, a := range s.Accounts() {
						if s.Pool != nil {
							if sess := s.Pool.Existing(a.ID); sess != nil && sess.Busy() {
								continue
							}
						}
						c := s.clientFor(a)
						rCtx, rCancel := context.WithTimeout(ctx, 15*time.Second)
						res, err := c.RotateCookies(rCtx)
						rCancel()
						if err != nil {
							log.Printf("[cookies] %s rotate error: %v", a.ID, err)
						} else if res != nil && res.Rotated {
							log.Printf("[cookies] %s rotated session cookies (HTTP %d)", a.ID, res.Status)
						}
					}
				}
			}
		}()
	}
}

func retryHeaders(hdrs map[string]string) map[string]string {
	if len(hdrs) == 0 {
		return nil
	}
	out := make(map[string]string)
	for _, k := range []string{"retry-after", "x-resets-at"} {
		val := hdrs[k]
		if val == "" {
			if k == "retry-after" {
				val = hdrs["Retry-After"]
			} else {
				val = hdrs["X-Resets-At"]
			}
		}
		if val != "" && !strings.ContainsAny(val, "\r\n") {
			out[k] = val
		}
	}
	return out
}

func sendJSON(w http.ResponseWriter, status int, obj any, extraHeaders map[string]string) {
	b, _ := json.Marshal(obj)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	for k, v := range extraHeaders {
		w.Header().Set(k, v)
	}
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func sendChatError(w http.ResponseWriter, headersSent bool, err error) {
	status := 502
	code := ""
	var hdrs map[string]string
	var ue *transport.UpstreamError
	if errors.As(err, &ue) && ue != nil {
		if ue.StatusCode >= 400 && ue.StatusCode <= 599 {
			status = ue.StatusCode
		}
		code = ue.Code
		hdrs = ue.Headers
	}
	errType := "upstream_error"
	switch status {
	case 429:
		errType = "rate_limit_error"
	case 401, 403:
		errType = "authentication_error"
	case 400:
		errType = "invalid_request_error"
	}
	if code == "" {
		code = errType
	}
	payload := map[string]any{
		"error": map[string]any{
			"message": err.Error(),
			"type":    errType,
			"code":    code,
		},
	}
	if headersSent {
		b, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", string(b))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	} else {
		sendJSON(w, status, payload, retryHeaders(hdrs))
	}
}
