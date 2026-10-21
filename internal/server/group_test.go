package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/config"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
)

// TestGroupScopedKeyOnlyUsesItsOwnAccounts verifies that a group key routes
// requests to the accounts of its own group and never to another group's
// accounts, even when the pool contains both.
func TestGroupScopedKeyOnlyUsesItsOwnAccounts(t *testing.T) {
	cfg := config.Load()
	cfg.HomeDir = t.TempDir()
	cfg.GroupTokens = map[string]string{"team-a-key": "team-a"}
	srv := New(cfg)
	t.Cleanup(func() { srv.FlushSessionCacheNow() })

	servedBy := make(chan string, 4)
	newAcct := func(id, group string) *AccountState {
		return &AccountState{
			ID:           id,
			Email:        id + "@example.com",
			AccountGroup: group,
			Models:       []gemini.ModelInfo{testModel},
			Capacity:     2,
			ModelsAt:     time.Now(),
			Client: &mockUpstream{
				streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
					servedBy <- id
					_ = onFrame(&gemini.LineParseResult{
						CID:             "c1",
						RID:             "r1",
						AnsweredModelID: testModel.ModelID,
						Frames:          []gemini.Frame{makeFrame("ok", "", true, testModel.ModelID)},
					})
					return &gemini.StreamResponse{Status: 200, CID: "c1", RID: "r1"}, nil
				},
			},
		}
	}
	srv.SetAccounts([]*AccountState{
		newAcct("acct-a1", "team-a"),
		newAcct("acct-b1", "team-b"),
	})

	body := `{"model":"gemini-pro","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer team-a-key")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200 for the group key, got %d: %s", rec.Code, rec.Body.String())
	}
	select {
	case id := <-servedBy:
		if id != "acct-a1" {
			t.Fatalf("expected the group key to use acct-a1, got %q", id)
		}
	default:
		t.Fatal("expected the request to reach an account")
	}
}

// TestGroupScopedKeyCannotSeeOtherGroupAccounts verifies that the accounts
// endpoint only lists the caller's own group.
func TestGroupScopedKeyCannotSeeOtherGroupAccounts(t *testing.T) {
	cfg := config.Load()
	cfg.HomeDir = t.TempDir()
	cfg.GroupTokens = map[string]string{"team-a-key": "team-a"}
	srv := New(cfg)
	t.Cleanup(func() { srv.FlushSessionCacheNow() })

	srv.SetAccounts([]*AccountState{
		{ID: "acct-a1", Email: "a1@example.com", AccountGroup: "team-a", Models: []gemini.ModelInfo{testModel}, ModelsAt: time.Now()},
		{ID: "acct-b1", Email: "b1@example.com", AccountGroup: "team-b", Models: []gemini.ModelInfo{testModel}, ModelsAt: time.Now()},
	})

	req := httptest.NewRequest("GET", "/v1/accounts", nil)
	req.Header.Set("Authorization", "Bearer team-a-key")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Accounts []struct {
			ID string `json:"id"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode accounts: %v (%s)", err, rec.Body.String())
	}
	if len(payload.Accounts) != 1 || payload.Accounts[0].ID != "acct-a1" {
		t.Fatalf("expected only acct-a1 to be visible, got %+v", payload.Accounts)
	}
}

// TestGroupScopedKeyFailsOverInsideItsGroup verifies that failover for a group
// key stays inside the group instead of borrowing another group's account.
func TestGroupScopedKeyFailsOverInsideItsGroup(t *testing.T) {
	cfg := config.Load()
	cfg.HomeDir = t.TempDir()
	cfg.GroupTokens = map[string]string{"team-a-key": "team-a"}
	cfg.MaxAttempts = 3
	srv := New(cfg)
	t.Cleanup(func() { srv.FlushSessionCacheNow() })

	var mu = make(chan string, 8)
	newAcct := func(id, group string, fail bool) *AccountState {
		return &AccountState{
			ID:           id,
			Email:        id + "@example.com",
			AccountGroup: group,
			Models:       []gemini.ModelInfo{testModel},
			Capacity:     2,
			ModelsAt:     time.Now(),
			Client: &mockUpstream{
				streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
					mu <- id
					if fail {
						// A rate-limit response is what triggers a pool failover.
						return nil, transport.NewUpstreamError("rate limited", 429, "rate_limit_error", map[string]string{"retry-after": "1"}, nil)
					}
					_ = onFrame(&gemini.LineParseResult{
						CID:             "c1",
						RID:             "r1",
						AnsweredModelID: testModel.ModelID,
						Frames:          []gemini.Frame{makeFrame("ok", "", true, testModel.ModelID)},
					})
					return &gemini.StreamResponse{Status: 200, CID: "c1", RID: "r1"}, nil
				},
			},
		}
	}
	srv.SetAccounts([]*AccountState{
		newAcct("acct-a1", "team-a", true),
		newAcct("acct-a2", "team-a", false),
		newAcct("acct-b1", "team-b", false),
	})

	body := `{"model":"gemini-pro","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer team-a-key")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200 after in-group failover, got %d: %s", rec.Code, rec.Body.String())
	}

	close(mu)
	seen := map[string]bool{}
	for id := range mu {
		seen[id] = true
	}
	if seen["acct-b1"] {
		t.Fatalf("group key must not fail over into another group, saw %v", seen)
	}
	if !seen["acct-a1"] || !seen["acct-a2"] {
		t.Fatalf("expected failover from acct-a1 to acct-a2, saw %v", seen)
	}
}
