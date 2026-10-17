package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
)

func TestQuotaExhaustionCooldownEscalates(t *testing.T) {
	// The first few strikes must stay short so a genuine 5-hour limit recovers
	// quickly, then grow to hour scale so a spent weekly cap is not retried every
	// 15 minutes for days.
	cases := []struct {
		strikes int
		want    time.Duration
	}{
		{1, 1 * time.Minute},
		{2, 5 * time.Minute},
		{3, 15 * time.Minute},
		{4, 1 * time.Hour},
		{5, 6 * time.Hour},
		{6, 24 * time.Hour},
		{7, 24 * time.Hour},   // clamped at the ceiling
		{100, 24 * time.Hour}, // stays clamped
		{0, 1 * time.Minute},  // defensive
	}
	for _, c := range cases {
		if got := quotaExhaustionCooldown(c.strikes); got != c.want {
			t.Fatalf("strikes=%d: cooldown=%v, want %v", c.strikes, got, c.want)
		}
	}
}

func TestRecordModelExhaustedUsesEscalatingCooldown(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	now := time.Now()
	srv.clock = func() time.Time { return now }

	// Four consecutive observations must reach the hour-scale step, proving the
	// ceiling is no longer 15 minutes.
	for i := 0; i < 4; i++ {
		srv.recordModelExhausted(acct, "gemini-pro")
	}
	acct.mu.RLock()
	quota := append([]gemini.QuotaBucket(nil), acct.Quota...)
	strikes := acct.subStrikes["sub:gemini-pro"]
	acct.mu.RUnlock()

	if strikes != 4 {
		t.Fatalf("strikes = %d, want 4", strikes)
	}
	var bucket *gemini.QuotaBucket
	for i := range quota {
		if quota[i].ID == "sub:gemini-pro" {
			bucket = &quota[i]
			break
		}
	}
	if bucket == nil {
		t.Fatal("no synthetic bucket recorded")
	}
	reset, err := time.Parse(time.RFC3339, bucket.ResetAt)
	if err != nil {
		t.Fatalf("bad resetAt %q: %v", bucket.ResetAt, err)
	}
	if got := reset.Sub(now); got < 59*time.Minute || got > 61*time.Minute {
		t.Fatalf("cooldown = %v, want ~1h after 4 strikes", got)
	}
}

func TestAccountStatusQuarantined(t *testing.T) {
	a := &AccountState{ID: "a", Quarantined: true, QuarantineReason: "account banned"}
	status, reason := AccountStatus(a, time.Now())
	if status != StatusQuarantined {
		t.Fatalf("status = %q, want %q", status, StatusQuarantined)
	}
	if reason != "account banned" {
		t.Fatalf("reason = %q", reason)
	}
}

func TestAccountStatusExpiredBeatsQuota(t *testing.T) {
	// An auth failure means the account cannot be used at all, which is more
	// actionable than a quota message.
	a := &AccountState{
		ID:        "a",
		LastError: "upstream_auth_error: session is unauthenticated or the saved cookies have expired (code 1016)",
		Quota: []gemini.QuotaBucket{
			{ID: "11", Label: "Gemini Flash", Total: 100, Remaining: 0, ResetAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
		},
	}
	status, _ := AccountStatus(a, time.Now())
	if status != StatusExpired {
		t.Fatalf("status = %q, want %q", status, StatusExpired)
	}
}

func TestAccountStatusExhausted(t *testing.T) {
	a := &AccountState{
		ID: "a",
		Quota: []gemini.QuotaBucket{
			{ID: "11", Label: "Gemini Flash", Total: 100, Remaining: 0, ResetAt: time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)},
		},
	}
	status, reason := AccountStatus(a, time.Now())
	if status != StatusExhausted {
		t.Fatalf("status = %q, want %q", status, StatusExhausted)
	}
	if reason == "" {
		t.Fatal("expected a reason naming the exhausted bucket")
	}
}

func TestAccountStatusIgnoresSyntheticAndResetBuckets(t *testing.T) {
	// A synthetic per-model cooldown is not a quota window, and a bucket whose reset
	// time has passed is stale rather than exhausted.
	a := &AccountState{
		ID: "a",
		Quota: []gemini.QuotaBucket{
			{ID: "sub:gemini-pro", Label: "gemini-pro", Total: 1, Remaining: 0, Synthetic: true, ResetAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
			{ID: "11", Label: "Gemini Flash", Total: 100, Remaining: 0, ResetAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)},
		},
	}
	status, _ := AccountStatus(a, time.Now())
	if status != StatusOK {
		t.Fatalf("status = %q, want %q", status, StatusOK)
	}
}

func TestAccountStatusWeeklyCapAfterRepeatedStrikes(t *testing.T) {
	// Three or more strikes means we already retried and the limit was still there,
	// which is the observable signal for the weekly cap that the RPC does not expose.
	a := &AccountState{
		ID:         "a",
		subStrikes: map[string]int{"sub:gemini-pro": 3},
	}
	status, reason := AccountStatus(a, time.Now())
	if status != StatusExhausted {
		t.Fatalf("status = %q, want %q", status, StatusExhausted)
	}
	if reason == "" {
		t.Fatal("expected a reason mentioning repeated retries")
	}
}

func TestAccountStatusCoolingAndDegraded(t *testing.T) {
	now := time.Now()
	cooling := &AccountState{ID: "a", Fails: 1, LastFail: now.Add(-10 * time.Second), LastError: "boom"}
	if status, _ := AccountStatus(cooling, now); status != StatusCooling {
		t.Fatalf("status = %q, want %q", status, StatusCooling)
	}

	degraded := &AccountState{ID: "b", Fails: 2, LastFail: now.Add(-10 * time.Minute), LastError: "boom"}
	if status, _ := AccountStatus(degraded, now); status != StatusDegraded {
		t.Fatalf("status = %q, want %q", status, StatusDegraded)
	}
}

func TestAccountStatusOK(t *testing.T) {
	a := &AccountState{
		ID: "a",
		Quota: []gemini.QuotaBucket{
			{ID: "11", Label: "Gemini Flash", Total: 100, Remaining: 40, ResetAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
		},
	}
	if status, reason := AccountStatus(a, time.Now()); status != StatusOK || reason != "" {
		t.Fatalf("status=%q reason=%q, want ok with empty reason", status, reason)
	}
}

func TestQuotaWindowInfoStatesScopeAndNextReset(t *testing.T) {
	now := time.Now()
	reset := now.Add(90 * time.Minute)
	info := quotaWindowInfo([]gemini.QuotaBucket{
		{ID: "11", Label: "Gemini Flash", Total: 100, Remaining: 40, ResetAt: reset.UTC().Format(time.RFC3339)},
	}, now)

	if info["scope"] != "rolling" || info["label"] != "5h" {
		t.Fatalf("unexpected scope: %v / %v", info["scope"], info["label"])
	}
	if info["nestedUnderWeekly"] != true {
		t.Fatal("the short window must be marked as nested under the weekly cap")
	}
	if info["weeklyExposed"] != false {
		t.Fatal("weekly must be reported as not exposed")
	}
	if got, _ := info["nextResetInSeconds"].(int); got < 5300 || got > 5500 {
		t.Fatalf("nextResetInSeconds = %v, want ~5400", info["nextResetInSeconds"])
	}
}

func TestQuotaWindowInfoIgnoresSyntheticBuckets(t *testing.T) {
	now := time.Now()
	info := quotaWindowInfo([]gemini.QuotaBucket{
		{ID: "sub:gemini-pro", Synthetic: true, ResetAt: now.Add(time.Minute).UTC().Format(time.RFC3339)},
	}, now)
	if _, ok := info["nextResetAt"]; ok {
		t.Fatal("a synthetic cooldown must not be reported as a quota reset")
	}
}

func TestAccountsEndpointExposesStatusAndQuotaWindow(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	acct.mu.Lock()
	acct.Quota = []gemini.QuotaBucket{
		{ID: "11", Label: "Gemini Flash", Total: 100, Remaining: 0, ResetAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
	}
	acct.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/v1/accounts", nil)
	req.Header.Set("Authorization", "Bearer audit")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Accounts []struct {
			ID          string         `json:"id"`
			Status      string         `json:"status"`
			Reason      string         `json:"statusReason"`
			LastError   string         `json:"lastError"`
			LastFailAt  string         `json:"lastFailAt"`
			QuotaWindow map[string]any `json:"quotaWindow"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload.Accounts) != 1 {
		t.Fatalf("got %d accounts, want 1", len(payload.Accounts))
	}
	got := payload.Accounts[0]
	if got.Status != StatusExhausted {
		t.Fatalf("status = %q, want %q", got.Status, StatusExhausted)
	}
	if got.Reason == "" {
		t.Fatal("expected a statusReason")
	}
	if got.QuotaWindow == nil {
		t.Fatal("expected quotaWindow metadata")
	}
	if got.QuotaWindow["scope"] != "rolling" {
		t.Fatalf("quotaWindow.scope = %v", got.QuotaWindow["scope"])
	}
	if got.QuotaWindow["weeklyExposed"] != false {
		t.Fatal("weeklyExposed must be false: the RPC does not expose it")
	}
}
