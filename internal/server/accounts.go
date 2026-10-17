package server

import (
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/cookies"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/prompt"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/toolbridge"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
)

// SetAccounts replaces the server's loaded account list (used by tests).
func (s *Server) SetAccounts(accts []*AccountState) {
	s.mu.Lock()
	s.accounts = accts
	if len(s.Cfg.AccountTokens) == 0 {
		for _, a := range accts {
			if a != nil && a.ID != "" {
				s.registerAccountTokensLocked(a.ID, a.ID)
			}
		}
	}
	s.mu.Unlock()
}

func (s *Server) registerAccountTokensLocked(rawID, targetID string) {
	if rawID == "" || targetID == "" {
		return
	}
	for _, tok := range []string{"gsb-" + rawID, rawID} {
		if _, explicit := s.Cfg.AccountTokens[tok]; !explicit {
			s.tokenToAcct[tok] = targetID
		}
	}
}

// Accounts returns a snapshot of the loaded accounts.
func (s *Server) Accounts() []*AccountState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*AccountState, len(s.accounts))
	copy(out, s.accounts)
	return out
}

func (s *Server) scopedAccounts(scope AuthScope) []*AccountState {
	accts := s.Accounts()
	if scope.AccountID == "" {
		return accts
	}
	var out []*AccountState
	for _, a := range accts {
		if a.ID == scope.AccountID {
			out = append(out, a)
		}
	}
	return out
}

// LoadCredentials loads and deduplicates account JSON files from AccountsDir and CredFile,
// falling back to live Chrome SQLite decryption if no saved files exist.
func (s *Server) LoadCredentials() int {
	var rawList []*AccountState

	credFile := filepath.Join(s.Cfg.HomeDir, "web-credentials.json")
	if entries, err := os.ReadDir(s.Cfg.AccountsDir); err == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, fn := range names {
			id := strings.TrimSuffix(fn, ".json")
			if !reSafeID.MatchString(id) {
				continue
			}
			fullPath := filepath.Join(s.Cfg.AccountsDir, fn)
			if st, err := s.loadAccountStateFromFile(id, fullPath); err == nil {
				rawList = append(rawList, st)
			}
		}
	}

	if st, err := s.loadAccountStateFromFile("default", credFile); err == nil {
		rawList = append(rawList, st)
	}

	if len(rawList) == 0 && s.Cfg.CookieDB != "" {
		if dbPath := cookies.FindChromeCookieDB(s.Cfg.CookieDB); dbPath != "" {
			if hdr, err := cookies.ReadChromeCookies(dbPath); err == nil && cookies.ExtractSAPISID(hdr) != "" {
				ac := gemini.NewAccountClient("chrome-live", hdr, "", s.Pool, s.Rotator)
				rawList = append(rawList, &AccountState{
					ID:     "chrome-live",
					Email:  "chrome-profile",
					Client: ac,
				})
			}
		}
	}

	deduped := dedupeAccounts(rawList, credFile)

	s.mu.Lock()
	s.accounts = deduped
	// Auto-populate gsb-<id> (and raw <id>) so 9router and direct harnesses work out of the box.
	byEmail := make(map[string]string, len(deduped))
	for _, a := range deduped {
		s.registerAccountTokensLocked(a.ID, a.ID)
		if a.Email != "" {
			byEmail[strings.ToLower(strings.TrimSpace(a.Email))] = a.ID
		}
	}
	for _, raw := range rawList {
		if targetID, ok := byEmail[strings.ToLower(strings.TrimSpace(raw.Email))]; ok {
			s.registerAccountTokensLocked(raw.ID, targetID)
		}
	}
	s.mu.Unlock()
	return len(deduped)
}

func (s *Server) loadAccountStateFromFile(id, path string) (*AccountState, error) {
	ac, err := gemini.LoadAccountFromFile(path, s.Pool, s.Rotator)
	if err != nil {
		return nil, err
	}
	ac.ID = id
	rec, _, _ := cookies.LoadAccountFile(path)
	var capAt, rotAt time.Time
	var importedFrom, profileID, acctGroup, qReason string
	var quarantined bool
	surfaces := make(map[string]string)
	if rec != nil {
		if rec.CapturedAt != "" {
			capAt, _ = time.Parse(time.RFC3339, rec.CapturedAt)
		}
		if rec.RotatedAt != "" {
			rotAt, _ = time.Parse(time.RFC3339, rec.RotatedAt)
		}
		importedFrom = rec.ImportedFrom
		profileID = rec.ProfileID
		acctGroup = rec.AccountGroup
		quarantined = rec.Quarantined
		qReason = rec.QuarantineReason
		for k, v := range rec.Surfaces {
			surfaces[k] = v
		}
	}
	if g, ok := s.Cfg.AccountGroups[id]; ok && g != "" {
		acctGroup = g
	}
	proxyURL := ""
	if p, ok := s.Cfg.EgressPerAccount[id]; ok && p != "" {
		proxyURL = p
	} else if rec != nil && rec.ProxyURL != "" {
		proxyURL = rec.ProxyURL
	}
	ac.SetRuntimeOptions(s.Cfg.Locale, s.Cfg.BLOverride, s.Cfg.ProtocolMode, acctGroup, proxyURL)

	return &AccountState{
		ID:               id,
		Email:            ac.Email(),
		FilePath:         path,
		CapturedAt:       capAt,
		RotatedAt:        rotAt,
		ImportedFrom:     importedFrom,
		ProfileID:        profileID,
		AccountGroup:     acctGroup,
		Quarantined:      quarantined,
		QuarantineReason: qReason,
		Surfaces:         surfaces,
		subStrikes:       make(map[string]int),
		Client:           ac,
	}, nil
}

func effectiveFreshnessTime(a *AccountState) time.Time {
	if !a.RotatedAt.IsZero() {
		return a.RotatedAt
	}
	return a.CapturedAt
}

func dedupeAccounts(list []*AccountState, credFile string) []*AccountState {
	byEmail := make(map[string]*AccountState)
	var out []*AccountState
	for _, a := range list {
		key := strings.ToLower(strings.TrimSpace(a.Email))
		if key == "" {
			out = append(out, a)
			continue
		}
		prev, exists := byEmail[key]
		if !exists {
			byEmail[key] = a
			out = append(out, a)
			continue
		}
		aNamed := a.FilePath != credFile
		prevNamed := prev.FilePath != credFile
		aTime := effectiveFreshnessTime(a)
		prevTime := effectiveFreshnessTime(prev)
		newer := aTime.After(prevTime)
		tieNamed := aTime.Equal(prevTime) && aNamed && !prevNamed
		if newer || tieNamed {
			byEmail[key] = a
			for i := range out {
				if out[i] == prev {
					out[i] = a
					break
				}
			}
		}
	}
	return out
}

func (s *Server) clientFor(a *AccountState) UpstreamClient {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s.clientFactory != nil {
		c := s.clientFactory(a)
		if c != nil {
			a.Client = c
			return c
		}
	}
	if a.Client != nil {
		a.Client.ReloadFromDiskIfNewer()
		return a.Client
	}
	if a.FilePath != "" {
		if ac, err := gemini.LoadAccountFromFile(a.FilePath, s.Pool, s.Rotator); err == nil {
			ac.ID = a.ID
			a.Client = ac
			return ac
		}
	}
	ac := gemini.NewAccountClient(a.ID, "", a.FilePath, s.Pool, s.Rotator)
	a.Client = ac
	return ac
}

// RefreshAccount refreshes the live model list and quota for a single account,
// preserving the last known good state on failure.
func (s *Server) RefreshAccount(ctx context.Context, a *AccountState, force bool) *AccountState {
	a.mu.RLock()
	hasModels := len(a.Models) > 0
	fresh := !a.ModelsAt.IsZero() && time.Since(a.ModelsAt) < 10*time.Minute
	a.mu.RUnlock()
	if !force && hasModels && fresh {
		return a
	}

	c := s.clientFor(a)
	am, err := c.AvailableModels(ctx, force)
	if err != nil || am == nil || am.Status != 200 || len(am.Models) == 0 {
		if err == nil {
			err = transport.NewUpstreamError("invalid upstream model registry", 502, "model_discovery_error", nil, nil)
		}
		a.mu.Lock()
		a.ModelsError = err
		a.mu.Unlock()
		return a
	}

	quota := c.GetQuota(ctx)

	a.mu.Lock()
	a.Models = am.Models
	a.Capacity = am.Capacity
	a.ModelsAt = time.Now()
	a.ModelsError = nil
	a.mu.Unlock()
	if len(quota) > 0 {
		s.mergeAccountQuota(a, quota)
	}
	return a
}

// EnsureModels refreshes all accounts concurrently.
func (s *Server) EnsureModels(ctx context.Context, force bool) []*AccountState {
	accts := s.Accounts()
	var wg sync.WaitGroup
	for _, a := range accts {
		wg.Add(1)
		go func(acct *AccountState) {
			defer wg.Done()
			s.RefreshAccount(ctx, acct, force)
		}(a)
	}
	wg.Wait()
	return accts
}

func (s *Server) allModels() []gemini.ModelInfo {
	accts := s.Accounts()
	seen := make(map[string]bool)
	var out []gemini.ModelInfo
	for _, a := range accts {
		a.mu.RLock()
		for _, m := range a.Models {
			if !seen[m.ModelName] {
				seen[m.ModelName] = true
				out = append(out, m)
			}
		}
		a.mu.RUnlock()
	}
	return out
}

func resolveModelOnAccount(a *AccountState, wanted string) *gemini.ModelInfo {
	a.mu.RLock()
	defer a.mu.RUnlock()
	w := strings.TrimSpace(wanted)
	if w == "" {
		if len(a.Models) > 0 {
			m := a.Models[0]
			return &m
		}
		return nil
	}
	for _, m := range a.Models {
		if gemini.MatchesModel(m, w) {
			found := m
			return &found
		}
	}
	return nil
}

func (s *Server) markAccountQuarantined(a *AccountState, reason string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.Quarantined = true
	a.QuarantineReason = reason
	if a.Surfaces == nil {
		a.Surfaces = make(map[string]string)
	}
	a.Surfaces["geminiWeb"] = "blocked"
	c := a.Client
	a.mu.Unlock()

	if ac, ok := c.(*gemini.AccountClient); ok {
		ac.SetQuarantined(true, reason)
	}
	log.Printf("[quarantine] account=%s quarantined reason=%q", a.ID, reason)
}

func (s *Server) checkAccountRateLimit(a *AccountState) error {
	if a == nil {
		return nil
	}
	now := s.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if s.Cfg.MaxConcurrentPerAccount > 0 && a.activeStreams >= s.Cfg.MaxConcurrentPerAccount {
		return transport.NewUpstreamError("account concurrent stream limit reached", 429, "rate_limit_error", map[string]string{"retry-after": "2"}, nil)
	}
	if s.Cfg.MaxRPMPerAccount > 0 {
		cutoff := now.Add(-60 * time.Second)
		kept := a.reqTimestamps[:0]
		for _, ts := range a.reqTimestamps {
			if ts.After(cutoff) {
				kept = append(kept, ts)
			}
		}
		a.reqTimestamps = kept
		if len(a.reqTimestamps) >= s.Cfg.MaxRPMPerAccount {
			waitSec := int(math.Ceil(a.reqTimestamps[0].Add(60 * time.Second).Sub(now).Seconds()))
			if waitSec < 1 {
				waitSec = 1
			}
			return transport.NewUpstreamError(
				fmt.Sprintf("account %s RPM limit (%d/min) reached", a.ID, s.Cfg.MaxRPMPerAccount),
				429,
				"rate_limit_error",
				map[string]string{"retry-after": strconv.Itoa(waitSec)},
				nil,
			)
		}
		a.reqTimestamps = append(a.reqTimestamps, now)
	}
	return nil
}

func (s *Server) checkPreFlightQuota(a *AccountState, model gemini.ModelInfo, mid string) error {
	return s.checkPreFlightQuotaInternal(a, model, mid, false)
}

func (s *Server) checkPreFlightQuotaInternal(a *AccountState, model gemini.ModelInfo, mid string, forSelection bool) error {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	quarantined := a.Quarantined
	qReason := a.QuarantineReason
	quota := a.Quota
	a.mu.RUnlock()
	if quarantined {
		return transport.NewUpstreamError(fmt.Sprintf("account %s is quarantined: %s", a.ID, qReason), 403, "account_banned", nil, nil)
	}

	mode := strings.ToLower(strings.TrimSpace(s.Cfg.QuotaPreflight))
	if mode == "off" {
		return nil
	}
	if !s.Cfg.RejectDowngrade && (mode == "" || mode == "strict") {
		return nil
	}
	name := strings.ToLower(model.ModelName)
	if strings.Contains(name, "lite") {
		return nil
	}
	if len(quota) == 0 {
		return nil
	}
	now := s.now()
	family := modelFamilyTokens(name)
	var spent []gemini.QuotaBucket
	for _, q := range quota {
		if q.Unlimited || q.Total <= 0 || q.Remaining > 0 || q.ResetAt == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, q.ResetAt); err == nil && t.After(now) {
			spent = append(spent, q)
		}
	}
	if len(spent) == 0 {
		return nil
	}
	var related []gemini.QuotaBucket
	for _, q := range spent {
		lbl := strings.ToLower(q.Label)
		for _, tok := range family {
			if strings.Contains(lbl, tok) {
				related = append(related, q)
				break
			}
		}
	}
	if len(family) > 0 && len(related) == 0 {
		return nil
	}
	activeSpent := spent
	if len(related) > 0 {
		activeSpent = related
	}
	if mode == "advisory" && !forSelection {
		log.Printf("[quota] advisory preflight warning: model %s appears exhausted on account %s", mid, a.ID)
		return nil
	}
	var earliestReal time.Time
	var earliestAny time.Time
	hasReal := false
	for _, q := range activeSpent {
		isSynth := q.Synthetic || strings.HasPrefix(q.ID, "sub:")
		if t, err := time.Parse(time.RFC3339, q.ResetAt); err == nil {
			if earliestAny.IsZero() || t.Before(earliestAny) {
				earliestAny = t
			}
			if !isSynth {
				hasReal = true
				if earliestReal.IsZero() || t.Before(earliestReal) {
					earliestReal = t
				}
			}
		}
	}
	if !hasReal {
		return transport.NewUpstreamError(
			fmt.Sprintf("quota exceeded: %s is in synthetic substitution cooldown on account %s until %s", mid, a.ID, earliestAny.UTC().Format(time.RFC3339)),
			429,
			"rate_limit_error",
			map[string]string{
				"x-gsb-retry-reason": "model_substitution",
			},
			nil,
		)
	}
	retrySec := int(math.Ceil(earliestReal.Sub(now).Seconds()))
	if retrySec < 1 {
		retrySec = 1
	}
	return transport.NewUpstreamError(
		fmt.Sprintf("quota exceeded: %s is exhausted on account %s until %s", mid, a.ID, earliestReal.UTC().Format(time.RFC3339)),
		429,
		"rate_limit_error",
		map[string]string{
			"retry-after": strconv.Itoa(retrySec),
			"x-resets-at": earliestReal.UTC().Format(time.RFC3339),
		},
		nil,
	)
}

func (s *Server) classifyEmpty(ctx context.Context, a *AccountState, modelName string) error {
	c := s.clientFor(a)
	qCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	quota := c.GetQuota(qCtx)
	cancel()
	if len(quota) > 0 {
		s.mergeAccountQuota(a, quota)
	} else {
		a.mu.RLock()
		quota = a.Quota
		a.mu.RUnlock()
	}
	var spent []gemini.QuotaBucket
	for _, q := range quota {
		if !q.Unlimited && q.Total > 0 && q.Remaining <= 0 {
			spent = append(spent, q)
		}
	}
	if len(spent) > 0 {
		family := modelFamilyTokens(modelName)
		var related []gemini.QuotaBucket
		for _, q := range spent {
			lbl := strings.ToLower(q.Label)
			for _, tok := range family {
				if strings.Contains(lbl, tok) {
					related = append(related, q)
					break
				}
			}
		}
		if len(family) > 0 && len(related) == 0 {
			return transport.NewUpstreamError("empty upstream response", 502, "empty_upstream_response", nil, nil)
		}
		chosen := spent
		if len(related) > 0 {
			chosen = related
		}
		labels := make([]string, 0, len(chosen))
		var earliest time.Time
		now := time.Now()
		for _, q := range chosen {
			labels = append(labels, q.Label)
			if q.ResetAt != "" {
				if t, err := time.Parse(time.RFC3339, q.ResetAt); err == nil && t.After(now) {
					if earliest.IsZero() || t.Before(earliest) {
						earliest = t
					}
				}
			}
		}
		var hdrs map[string]string
		if !earliest.IsZero() {
			retrySec := int(math.Ceil(earliest.Sub(now).Seconds()))
			if retrySec < 1 {
				retrySec = 1
			}
			hdrs = map[string]string{
				"retry-after": strconv.Itoa(retrySec),
				"x-resets-at": earliest.UTC().Format(time.RFC3339),
			}
		}
		ident := a.Email
		if ident == "" {
			ident = a.ID
		}
		return transport.NewUpstreamError(
			fmt.Sprintf("quota exceeded: %s exhausted on %s", strings.Join(labels, ", "), ident),
			429,
			"rate_limit_error",
			hdrs,
			nil,
		)
	}
	return transport.NewUpstreamError("empty upstream response", 502, "empty_upstream_response", nil, nil)
}

func modelFamilyTokens(modelName string) []string {
	name := strings.ToLower(modelName)
	var family []string
	if strings.Contains(name, "argon") {
		family = append(family, "argon", "pro")
	}
	for _, tok := range []string{"pro", "flash", "thinking"} {
		if strings.Contains(name, tok) {
			family = append(family, tok)
		}
	}
	return family
}

func (s *Server) recordModelExhausted(a *AccountState, modelName string) {
	if a == nil || modelName == "" {
		return
	}
	subID := "sub:" + strings.ToLower(strings.TrimSpace(modelName))
	now := s.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.subStrikes == nil {
		a.subStrikes = make(map[string]int)
	}
	strikes := a.subStrikes[subID] + 1
	a.subStrikes[subID] = strikes
	var dur time.Duration
	switch {
	case strikes == 1:
		dur = 1 * time.Minute
	case strikes == 2:
		dur = 5 * time.Minute
	default:
		dur = 15 * time.Minute
	}
	resetAt := now.UTC().Add(dur).Format(time.RFC3339)
	bucket := gemini.QuotaBucket{
		ID:        subID,
		Label:     modelName,
		Total:     1,
		Remaining: 0,
		ResetAt:   resetAt,
		Synthetic: true,
		Strikes:   strikes,
	}
	for i, q := range a.Quota {
		if q.ID == subID {
			a.Quota[i] = bucket
			return
		}
	}
	a.Quota = append(a.Quota, bucket)
}

func (s *Server) clearModelExhausted(a *AccountState, modelName string) {
	if a == nil || modelName == "" {
		return
	}
	subID := "sub:" + strings.ToLower(strings.TrimSpace(modelName))
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.subStrikes != nil {
		delete(a.subStrikes, subID)
	}
	if len(a.Quota) == 0 {
		return
	}
	kept := a.Quota[:0]
	for _, q := range a.Quota {
		if q.ID == subID {
			continue
		}
		kept = append(kept, q)
	}
	a.Quota = kept
}

func (s *Server) mergeAccountQuota(a *AccountState, fresh []gemini.QuotaBucket) []gemini.QuotaBucket {
	now := s.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	merged := make([]gemini.QuotaBucket, 0, len(fresh)+2)
	merged = append(merged, fresh...)
	for _, old := range a.Quota {
		if (old.Synthetic || strings.HasPrefix(old.ID, "sub:")) && old.ResetAt != "" {
			if t, err := time.Parse(time.RFC3339, old.ResetAt); err == nil && t.After(now) {
				old.Synthetic = true
				merged = append(merged, old)
			}
		}
	}
	a.Quota = merged
	a.QuotaAt = now
	a.QuotaBackoffStep = 0
	a.QuotaNextAllowAt = time.Time{}
	return merged
}

func (s *Server) isAccountBusy(acctID string) bool {
	if s.Pool == nil || acctID == "" {
		return false
	}
	if sess := s.Pool.Existing(acctID); sess != nil {
		return sess.Busy()
	}
	return false
}

func (s *Server) hasSessionCacheHit(a *AccountState, modelName string, hasher *prompt.HistoryHasher) bool {
	if !s.Cfg.SessionCache || a == nil || modelName == "" || hasher == nil || hasher.Len() < 3 {
		return false
	}
	for split := hasher.Len() - 1; split >= 2; split-- {
		if len(s.getSessionCache(hasher.HashPrefix(a.ID, modelName, split))) > 0 {
			return true
		}
	}
	return false
}

func (s *Server) pickFallbackAccount(accts []*AccountState, tried map[string]bool, originGroup string, requested string, hasher *prompt.HistoryHasher) (*AccountState, *gemini.ModelInfo) {
	var idleCand *AccountState
	var idleModel *gemini.ModelInfo
	var busyCand *AccountState
	var busyModel *gemini.ModelInfo
	for _, cand := range accts {
		if tried[cand.ID] {
			continue
		}
		cand.mu.RLock()
		qState := cand.Quarantined
		cGroup := cand.AccountGroup
		cand.mu.RUnlock()
		if qState {
			continue
		}
		if (originGroup != "" || cGroup != "") && cGroup != originGroup {
			continue
		}
		m := resolveModelOnAccount(cand, requested)
		if m == nil {
			continue
		}
		if err := s.checkPreFlightQuotaInternal(cand, *m, m.ModelName, true); err != nil {
			continue
		}
		if !s.isAccountBusy(cand.ID) {
			if s.hasSessionCacheHit(cand, m.ModelName, hasher) {
				return cand, m
			}
			cand.mu.RLock()
			cFails := cand.Fails
			cand.mu.RUnlock()
			if idleCand == nil {
				idleCand = cand
				idleModel = m
			} else {
				idleCand.mu.RLock()
				iFails := idleCand.Fails
				idleCand.mu.RUnlock()
				if cFails < iFails {
					idleCand = cand
					idleModel = m
				}
			}
			continue
		}
		if busyCand == nil {
			busyCand = cand
			busyModel = m
		}
	}
	if idleCand != nil {
		return idleCand, idleModel
	}
	return busyCand, busyModel
}

func (s *Server) lookupQuotaResetHeaders(_ context.Context, a *AccountState, modelName string) map[string]string {
	a.mu.RLock()
	quota := a.Quota
	a.mu.RUnlock()
	if len(quota) == 0 {
		return nil
	}

	family := modelFamilyTokens(modelName)
	var spent []gemini.QuotaBucket
	for _, q := range quota {
		if !q.Unlimited && q.Total > 0 && q.Remaining <= 0 {
			spent = append(spent, q)
		}
	}
	var related []gemini.QuotaBucket
	for _, q := range spent {
		lbl := strings.ToLower(q.Label)
		for _, tok := range family {
			if strings.Contains(lbl, tok) {
				related = append(related, q)
				break
			}
		}
	}
	chosen := spent
	if len(related) > 0 {
		chosen = related
	}
	now := s.now()
	var earliestReal time.Time
	sawSynthetic := false
	for _, q := range chosen {
		isSynth := q.Synthetic || strings.HasPrefix(q.ID, "sub:")
		if isSynth {
			sawSynthetic = true
			continue
		}
		if q.ResetAt != "" {
			if t, err := time.Parse(time.RFC3339, q.ResetAt); err == nil && t.After(now) {
				if earliestReal.IsZero() || t.Before(earliestReal) {
					earliestReal = t
				}
			}
		}
	}
	if earliestReal.IsZero() {
		if sawSynthetic {
			return map[string]string{
				"x-gsb-retry-reason": "model_substitution",
			}
		}
		return nil
	}
	retrySec := int(math.Ceil(earliestReal.Sub(now).Seconds()))
	if retrySec < 1 {
		retrySec = 1
	}
	out := map[string]string{
		"retry-after": strconv.Itoa(retrySec),
		"x-resets-at": earliestReal.UTC().Format(time.RFC3339),
	}
	if sawSynthetic {
		out["x-gsb-retry-reason"] = "model_substitution"
	}
	return out
}

func (s *Server) markAccountFailure(a *AccountState, msg string) {
	a.mu.Lock()
	a.Fails++
	a.LastFail = s.now()
	a.LastError = msg
	a.mu.Unlock()
}

func (s *Server) resolveChatAccount(accts []*AccountState, scope AuthScope, requested string, hasher *prompt.HistoryHasher) *AccountState {
	if scope.AccountID != "" {
		for _, a := range accts {
			if a.ID == scope.AccountID {
				return a
			}
		}
		return nil
	}
	if !scope.Admin {
		s.mu.RLock()
		explicitTokens := len(s.Cfg.AccountTokens) > 0
		s.mu.RUnlock()
		if explicitTokens {
			return nil
		}
	}
	var best *AccountState
	bestBusy := false
	bestHasCache := false
	bestQuotaOK := false
	for _, a := range accts {
		a.mu.RLock()
		qState := a.Quarantined
		a.mu.RUnlock()
		if qState {
			continue
		}
		aBusy := s.isAccountBusy(a.ID)
		aHasCache := false
		aQuotaOK := true
		if m := resolveModelOnAccount(a, requested); m != nil {
			if s.checkPreFlightQuotaInternal(a, *m, m.ModelName, true) == nil {
				aHasCache = s.hasSessionCacheHit(a, m.ModelName, hasher)
			} else {
				aQuotaOK = false
			}
		}
		if best == nil {
			best = a
			bestBusy = aBusy
			bestHasCache = aHasCache
			bestQuotaOK = aQuotaOK
			continue
		}
		if aQuotaOK && !bestQuotaOK {
			best = a
			bestBusy = aBusy
			bestHasCache = aHasCache
			bestQuotaOK = true
			continue
		}
		if bestQuotaOK && !aQuotaOK {
			continue
		}
		if bestBusy && !aBusy {
			best = a
			bestBusy = false
			bestHasCache = aHasCache
			bestQuotaOK = aQuotaOK
			continue
		}
		if !bestBusy && aBusy {
			continue
		}
		if aHasCache && !bestHasCache {
			best = a
			bestBusy = aBusy
			bestHasCache = true
			bestQuotaOK = aQuotaOK
			continue
		}
		if bestHasCache && !aHasCache {
			continue
		}
		a.mu.RLock()
		aFails, aUsed := a.Fails, a.LastUsed
		a.mu.RUnlock()
		best.mu.RLock()
		bFails, bUsed := best.Fails, best.LastUsed
		best.mu.RUnlock()
		if aFails < bFails || (aFails == bFails && aUsed.Before(bUsed)) {
			best = a
			bestBusy = aBusy
			bestHasCache = aHasCache
			bestQuotaOK = aQuotaOK
		}
	}
	return best
}

const quotaRefreshCooldown = 45 * time.Second

func (s *Server) refreshQuotaAsync(a *AccountState, c UpstreamClient) {
	if a == nil || c == nil {
		return
	}
	now := s.now()
	a.mu.Lock()
	if a.quotaRefreshing || a.Quarantined {
		a.mu.Unlock()
		return
	}
	if !a.QuotaNextAllowAt.IsZero() && now.Before(a.QuotaNextAllowAt) {
		a.mu.Unlock()
		return
	}
	lowQuota := false
	for _, q := range a.Quota {
		if !q.Synthetic && !strings.HasPrefix(q.ID, "sub:") && !q.Unlimited && q.Total > 0 && q.Remaining <= 2 {
			lowQuota = true
			break
		}
	}
	if !lowQuota && !a.QuotaAt.IsZero() && now.Sub(a.QuotaAt) < quotaRefreshCooldown {
		a.mu.Unlock()
		return
	}
	a.quotaRefreshing = true
	a.mu.Unlock()

	go func() {
		defer func() {
			a.mu.Lock()
			a.quotaRefreshing = false
			a.mu.Unlock()
		}()
		if s.Pool != nil {
			if sess := s.Pool.Existing(a.ID); sess != nil && sess.Busy() {
				return
			}
		}
		qCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		q := c.GetQuota(qCtx)
		if len(q) > 0 {
			s.mergeAccountQuota(a, q)
		} else {
			a.mu.Lock()
			a.QuotaBackoffStep++
			shift := a.QuotaBackoffStep
			if shift > 5 {
				shift = 5
			}
			backoff := quotaRefreshCooldown * time.Duration(1<<shift)
			if backoff > 15*time.Minute {
				backoff = 15 * time.Minute
			}
			a.QuotaNextAllowAt = s.now().Add(backoff)
			a.mu.Unlock()
		}
	}()
}

type preparedGeneration struct {
	accts               []*AccountState
	account             *AccountState
	model               *gemini.ModelInfo
	mid                 string
	requested           string
	effortStr           string
	contract            *toolbridge.Contract
	hasTools            bool
	stream              bool
	buffered            bool
	opts                gemini.StreamOptions
	fullPrompt          string
	curPrompt           string
	flattenStats        prompt.FlattenStats
	overflowParts       []prompt.AttachmentPart
	imageParts          []prompt.AttachmentPart
	fullAttachmentParts []prompt.AttachmentPart
	attachmentParts     []prompt.AttachmentPart
	hasher              *prompt.HistoryHasher
}

func (s *Server) prepareGeneration(
	ctx context.Context,
	body map[string]any,
	scope AuthScope,
	messages []map[string]any,
	toolsRaw any,
	toolChoiceRaw any,
) (*preparedGeneration, error) {
	rawModel, _ := body["model"].(string)
	requested, inlineThink := gemini.ParseModelRequest(rawModel)

	accts := s.Accounts()
	if len(accts) == 0 {
		return nil, transport.NewUpstreamError("no accounts configured", 503, "auth_unavailable", nil, nil)
	}

	var hasher *prompt.HistoryHasher
	if s.Cfg.SessionCache && len(messages) >= 3 {
		hasher = prompt.NewHistoryHasher(messages, toolsRaw, toolChoiceRaw)
	}

	account := s.resolveChatAccount(accts, scope, requested, hasher)
	if account == nil {
		for _, a := range accts {
			a.mu.RLock()
			q := a.Quarantined
			r := a.QuarantineReason
			a.mu.RUnlock()
			if q {
				return nil, transport.NewUpstreamError(fmt.Sprintf("all accounts are quarantined (last: %s)", r), 403, "account_banned", nil, nil)
			}
		}
		msg := "this endpoint requires a per-account token; the admin key cannot serve chat"
		if scope.AccountID != "" {
			msg = fmt.Sprintf("token is bound to account %q, which is not loaded", scope.AccountID)
		}
		return nil, transport.NewUpstreamError(msg, 401, "authentication_error", nil, nil)
	}

	if err := s.checkAccountRateLimit(account); err != nil {
		return nil, err
	}

	contract, err := toolbridge.ValidateToolRequest(toolsRaw, toolChoiceRaw)
	if err != nil {
		return nil, err
	}

	account.mu.RLock()
	hasModels := len(account.Models) > 0
	account.mu.RUnlock()
	if !hasModels {
		s.RefreshAccount(ctx, account, false)
	}

	account.mu.RLock()
	hasModels = len(account.Models) > 0
	mErr := account.ModelsError
	account.mu.RUnlock()
	if !hasModels && mErr != nil {
		return nil, mErr
	}

	model := resolveModelOnAccount(account, requested)
	if model == nil {
		reqLabel := requested
		if reqLabel == "" {
			reqLabel = "(unspecified)"
		}
		return nil, transport.NewUpstreamError(fmt.Sprintf("account %s cannot serve model %s", account.ID, reqLabel), 404, "model_not_found", nil, nil)
	}
	mid := model.ModelName

	if err := s.checkPreFlightQuota(account, *model, mid); err != nil {
		return nil, err
	}

	fullPrompt, overflowParts, flattenStats, err := prompt.FlattenMessagesWithStats(messages, toolsRaw, toolChoiceRaw, prompt.FlattenOptions{
		StartIndex:      0,
		MaxPromptChars:  s.Cfg.MaxPromptChars,
		MaxOldToolChars: s.Cfg.MaxOldToolChars,
		AllowOverflow:   true,
	})
	if err != nil {
		return nil, err
	}

	imageParts := prompt.ExtractImageParts(messages)
	fullAttachmentParts := make([]prompt.AttachmentPart, 0, len(overflowParts)+len(imageParts))
	fullAttachmentParts = append(fullAttachmentParts, overflowParts...)
	fullAttachmentParts = append(fullAttachmentParts, imageParts...)
	attachmentParts := fullAttachmentParts

	hasTools := len(contract.Tools) > 0 && contract.ChoiceMode != "none"
	stream, _ := body["stream"].(bool)
	buffered := s.Cfg.RejectDowngrade || hasTools
	if strings.EqualFold(s.Cfg.StreamBufferMode, "eager") {
		buffered = hasTools
	}

	tempReq, _ := body["temporary"].(bool)
	effortVal := extractEffortFromBody(body)
	if (effortVal == nil || effortVal == "") && inlineThink != "" {
		effortVal = inlineThink
	}
	thinkLvl := gemini.ThinkingLevelFor(effortVal, mid, strconv.Itoa(s.Cfg.DefaultThinking))
	effortStr := "low"
	switch thinkLvl {
	case 4:
		effortStr = "medium"
	case 2:
		effortStr = "high"
	}
	opts := gemini.StreamOptions{
		Model:         model,
		ThinkingLevel: thinkLvl,
		Temporary:     tempReq || s.Cfg.Temporary,
		Language:      s.Cfg.Locale,
		DisableTools:  !strings.EqualFold(s.Cfg.ProtocolMode, "web") || hasTools,
		HasTools:      hasTools,
		ProtocolMode:  s.Cfg.ProtocolMode,
	}

	curPrompt := fullPrompt
	if s.Cfg.SessionCache && !opts.Temporary && hasher != nil {
		for split := len(messages) - 1; split >= 2; split-- {
			prefixKey := hasher.HashPrefix(account.ID, mid, split)
			if cachedMeta := s.getSessionCache(prefixKey); len(cachedMeta) > 0 {
				opts.Metadata = cachedMeta
				deltaPrompt, _, dErr := prompt.FlattenMessages(messages, toolsRaw, toolChoiceRaw, prompt.FlattenOptions{
					StartIndex:      split,
					MaxPromptChars:  s.Cfg.MaxPromptChars,
					MaxOldToolChars: s.Cfg.MaxOldToolChars,
					AllowOverflow:   false,
				})
				if dErr == nil {
					curPrompt = deltaPrompt
					attachmentParts = prompt.ExtractImageParts(messages[split:])
				}
				break
			}
		}
	}

	return &preparedGeneration{
		accts:               accts,
		account:             account,
		model:               model,
		mid:                 mid,
		requested:           requested,
		effortStr:           effortStr,
		contract:            contract,
		hasTools:            hasTools,
		stream:              stream,
		buffered:            buffered,
		opts:                opts,
		fullPrompt:          fullPrompt,
		curPrompt:           curPrompt,
		flattenStats:        flattenStats,
		overflowParts:       overflowParts,
		imageParts:          imageParts,
		fullAttachmentParts: fullAttachmentParts,
		attachmentParts:     attachmentParts,
		hasher:              hasher,
	}, nil
}
