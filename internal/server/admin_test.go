package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/config"
)

func TestWebUIDisabledByDefaultReturns404(t *testing.T) {
	cfg := config.Load()
	cfg.HomeDir = t.TempDir()
	cfg.AccountsDir = filepath.Join(cfg.HomeDir, "accounts")
	cfg.WebUI = false

	srv := New(cfg)

	for _, path := range []string{"/ui", "/ui/", "/v1/admin/config"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "127.0.0.1:43210"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("path %s with WebUI=false: expected 404, got %d (%s)", path, rec.Code, rec.Body.String())
		}
	}
}

func TestWebUIRemoteAccessGateAndRateLimit(t *testing.T) {
	cfg := config.Load()
	cfg.HomeDir = t.TempDir()
	cfg.AccountsDir = filepath.Join(cfg.HomeDir, "accounts")
	cfg.EnvFile = filepath.Join(cfg.HomeDir, "config.env")
	cfg.WebUI = true
	cfg.APIKey = "public"
	cfg.AdminKey = ""

	srv := New(cfg)

	// 1. Direct loopback with public key -> allowed
	reqLoop := httptest.NewRequest(http.MethodGet, "/ui", nil)
	reqLoop.RemoteAddr = "127.0.0.1:50001"
	recLoop := httptest.NewRecorder()
	srv.ServeHTTP(recLoop, reqLoop)
	if recLoop.Code != http.StatusOK || !strings.Contains(recLoop.Body.String(), "Gemini Stealth Bridge Control Plane") {
		t.Fatalf("loopback /ui expected 200 HTML, got %d", recLoop.Code)
	}

	// 2. Remote peer with public key and no AdminKey -> 403 remote_web_ui_locked
	reqRemote := httptest.NewRequest(http.MethodGet, "/ui", nil)
	reqRemote.RemoteAddr = "198.51.100.42:50002"
	recRemote := httptest.NewRecorder()
	srv.ServeHTTP(recRemote, reqRemote)
	if recRemote.Code != http.StatusForbidden || !strings.Contains(recRemote.Body.String(), "remote_web_ui_locked") {
		t.Fatalf("remote /ui with public key expected 403 remote_web_ui_locked, got %d: %s", recRemote.Code, recRemote.Body.String())
	}

	// 3. Loopback peer carrying X-Forwarded-For from a public IP -> treated as remote, 403
	reqProxy := httptest.NewRequest(http.MethodGet, "/v1/admin/config", nil)
	reqProxy.RemoteAddr = "127.0.0.1:50003"
	reqProxy.Header.Set("X-Forwarded-For", "203.0.113.9")
	recProxy := httptest.NewRecorder()
	srv.ServeHTTP(recProxy, reqProxy)
	if recProxy.Code != http.StatusForbidden {
		t.Fatalf("proxied /v1/admin/config with public key expected 403, got %d", recProxy.Code)
	}

	// 4. Configure AdminKey -> remote /ui serves HTML shell, /v1/admin/config requires token and rate-limits brute force
	srv.mu.Lock()
	srv.Cfg.AdminKey = "top-secret-admin-key"
	srv.mu.Unlock()

	for i := range 5 {
		reqBad := httptest.NewRequest(http.MethodGet, "/v1/admin/config", nil)
		reqBad.RemoteAddr = "198.51.100.77:51000"
		reqBad.Header.Set("Authorization", "Bearer wrong-key")
		recBad := httptest.NewRecorder()
		srv.ServeHTTP(recBad, reqBad)
		if recBad.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d expected 401, got %d", i+1, recBad.Code)
		}
	}

	// 6th attempt from same IP -> 429 rate limited even with valid key
	reqRL := httptest.NewRequest(http.MethodGet, "/v1/admin/config", nil)
	reqRL.RemoteAddr = "198.51.100.77:51000"
	reqRL.Header.Set("Authorization", "Bearer top-secret-admin-key")
	recRL := httptest.NewRecorder()
	srv.ServeHTTP(recRL, reqRL)
	if recRL.Code != http.StatusTooManyRequests {
		t.Fatalf("6th attempt expected 429, got %d: %s", recRL.Code, recRL.Body.String())
	}

	// Valid key from clean IP -> 200 OK
	reqOK := httptest.NewRequest(http.MethodGet, "/v1/admin/config", nil)
	reqOK.RemoteAddr = "198.51.100.88:51000"
	reqOK.Header.Set("X-GSB-Admin-Key", "top-secret-admin-key")
	recOK := httptest.NewRecorder()
	srv.ServeHTTP(recOK, reqOK)
	if recOK.Code != http.StatusOK {
		t.Fatalf("valid admin key expected 200, got %d: %s", recOK.Code, recOK.Body.String())
	}
}

func TestAdminConfigPatchAndEnvFilePersistence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BRIDGE_HOME", home)
	t.Setenv("BRIDGE_DEFAULT_THINKING", "")
	t.Setenv("BRIDGE_ACCOUNT_STRATEGY", "")
	t.Setenv("BRIDGE_MAX_RPM_PER_ACCOUNT", "")

	cfg := config.Load()
	cfg.WebUI = true
	srv := New(cfg)

	patchBody := map[string]any{
		"defaultThinking":  "medium",
		"accountStrategy":  "lru",
		"maxRpmPerAccount": 36,
		"liveToolCalls":    true,
		"persist":          true,
	}
	raw, _ := json.Marshal(patchBody)
	req := httptest.NewRequest(http.MethodPatch, "/v1/admin/config", bytes.NewReader(raw))
	req.RemoteAddr = "127.0.0.1:41000"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH /v1/admin/config failed: %d %s", rec.Code, rec.Body.String())
	}

	if srv.Cfg.DefaultThinking != 4 || srv.Cfg.AccountStrategy != "lru" || srv.Cfg.MaxRPMPerAccount != 36 || !srv.Cfg.LiveToolCalls {
		t.Fatalf("in-memory config not updated: %+v", srv.Cfg)
	}

	// Reload from disk via config.Load() and verify config.env was persisted
	reloaded := config.Load()
	if reloaded.DefaultThinking != 4 || reloaded.AccountStrategy != "lru" || reloaded.MaxRPMPerAccount != 36 || !reloaded.LiveToolCalls {
		t.Fatalf("reloaded config from config.env did not match persisted values: %+v", reloaded)
	}
	if reloaded.FieldSources["BRIDGE_DEFAULT_THINKING"] != "file" {
		t.Fatalf("expected source 'file' for BRIDGE_DEFAULT_THINKING, got %q", reloaded.FieldSources["BRIDGE_DEFAULT_THINKING"])
	}
}

func TestAdminAccountLifecycleAndWriteOnlyCookies(t *testing.T) {
	home := t.TempDir()
	cfg := config.Load()
	cfg.HomeDir = home
	cfg.AccountsDir = filepath.Join(home, "accounts")
	cfg.WebUI = true

	srv := New(cfg)
	srv.clientFactory = func(a *AccountState) UpstreamClient {
		return &mockUpstream{}
	}

	secretCookie := "__Secure-1PSID=super-secret-psid-value; SAPISID=fake-sapisid-12345"
	importPayload, _ := json.Marshal(map[string]any{
		"id":           "ops-acct-1",
		"accountGroup": "tier1",
		"cookie":       secretCookie,
	})
	reqImp := httptest.NewRequest(http.MethodPost, "/v1/admin/accounts/import", bytes.NewReader(importPayload))
	reqImp.RemoteAddr = "127.0.0.1:42000"
	recImp := httptest.NewRecorder()
	srv.ServeHTTP(recImp, reqImp)
	if recImp.Code != http.StatusOK {
		t.Fatalf("import failed: %d %s", recImp.Code, recImp.Body.String())
	}
	if strings.Contains(recImp.Body.String(), "super-secret-psid-value") {
		t.Fatalf("import response leaked raw cookie value: %s", recImp.Body.String())
	}

	// Quarantine the account, then clear quarantine via POST /v1/admin/accounts/ops-acct-1/unquarantine
	acct := srv.findAccountByID("ops-acct-1")
	if acct == nil {
		t.Fatalf("expected ops-acct-1 in server pool")
	}
	acct.mu.Lock()
	acct.Quarantined = true
	acct.QuarantineReason = "TOS_VIOLATION"
	acct.mu.Unlock()

	reqUnq := httptest.NewRequest(http.MethodPost, "/v1/admin/accounts/ops-acct-1/unquarantine", nil)
	reqUnq.RemoteAddr = "127.0.0.1:42000"
	recUnq := httptest.NewRecorder()
	srv.ServeHTTP(recUnq, reqUnq)
	if recUnq.Code != http.StatusOK {
		t.Fatalf("unquarantine failed: %d %s", recUnq.Code, recUnq.Body.String())
	}
	if acct.Quarantined {
		t.Fatalf("expected account quarantine cleared")
	}

	// Delete the account via DELETE /v1/admin/accounts/ops-acct-1
	reqDel := httptest.NewRequest(http.MethodDelete, "/v1/admin/accounts/ops-acct-1", nil)
	reqDel.RemoteAddr = "127.0.0.1:42000"
	recDel := httptest.NewRecorder()
	srv.ServeHTTP(recDel, reqDel)
	if recDel.Code != http.StatusOK {
		t.Fatalf("delete failed: %d %s", recDel.Code, recDel.Body.String())
	}
	if srv.findAccountByID("ops-acct-1") != nil {
		t.Fatalf("expected ops-acct-1 evicted from pool")
	}
	if _, err := os.Stat(filepath.Join(cfg.AccountsDir, "ops-acct-1.json")); !os.IsNotExist(err) {
		t.Fatalf("expected ops-acct-1.json removed from disk")
	}
}
