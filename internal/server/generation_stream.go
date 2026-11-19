package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/prompt"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/toolbridge"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
)

// buildActionStatusPayload projects an upstream ActionStatus frame into pure
// API pass-through fields without client/bot scheduling or lease state.
func buildActionStatusPayload(a *gemini.ActionStatus) map[string]any {
	if a == nil {
		return nil
	}
	sourceType := "thinking_progress"
	phase := "thinking"
	progress := strings.TrimSpace(a.ThinkingTitle)
	if strings.Contains(progress, "{tool_logo_placeholder}") || strings.Contains(progress, "{tool_name_placeholder}") {
		progress = ""
	}
	toolProgress := strings.TrimSpace(a.ToolTitle)
	if toolProgress == "" {
		toolProgress = stripToolLogoPlaceholder(a.BranchTitle)
	}
	switch a.ToolID {
	case "web_search":
		sourceType = "web_search"
		phase = "searching"
		if progress == "" {
			progress = toolProgress
		}
	case "image_generation", "video_generation", "music_gen":
		sourceType = "tool_progress"
		phase = "generating_media"
		if toolProgress != "" {
			progress = toolProgress
		}
	default:
		if progress == "" {
			sourceType = "tool_progress"
			progress = toolProgress
		}
	}
	template := strings.TrimSpace(a.StatusTemplate)
	if strings.Contains(template, "{tool_name_placeholder}") {
		template = strings.TrimSpace(strings.ReplaceAll(template, "{tool_name_placeholder}", strings.TrimSpace(a.ToolTitle)))
	}
	if !strings.Contains(template, "{tool_logo_placeholder}") {
		template = ""
	}
	if progress == "" && template == "" && phase != "generating_media" {
		return nil
	}
	payload := map[string]any{
		"source_type": sourceType,
		"phase":       phase,
	}
	if progress != "" {
		payload["progress_detail"] = progress
	}
	if a.ToolID != "" {
		payload["provider"] = a.ToolID
	}
	if template != "" {
		payload["status_template"] = template
	}
	return payload
}

// generationSession owns shared retry, failover, SSE commit/keepalive, action
// status queueing, citation deduplication, and request telemetry for both
// /v1/chat/completions and /v1/responses.
type generationSession struct {
	s             *Server
	ctx           context.Context
	w             http.ResponseWriter
	flusher       http.Flusher
	prep          *preparedGeneration
	messages      []map[string]any
	toolsRaw      any
	toolChoiceRaw any

	reqID        string
	endpoint     string
	reqStartedAt time.Time
	prepareMs    int64
	eagerStream  bool
	stream       bool
	buffered     bool

	accts                   []*AccountState
	account                 *AccountState
	model                   *gemini.ModelInfo
	mid                     string
	requested               string
	effortStr               string
	hasTools                bool
	opts                    gemini.StreamOptions
	fullPrompt              string
	curPrompt               string
	flattenStats            prompt.FlattenStats
	overflowParts           []prompt.AttachmentPart
	imageParts              []prompt.AttachmentPart
	fullAttachmentParts     []prompt.AttachmentPart
	attachmentParts         []prompt.AttachmentPart
	hasher                  *prompt.HistoryHasher
	cacheKey                string
	cacheUsed               bool
	originGroup             string
	maxAttempts             int
	maxUpstream             int
	upstreamCalls           int
	retryDeadline           time.Time
	triedAccounts           map[string]bool
	freshSocketNext         bool
	rebootstrapNext         bool
	priorAttemptSentThought bool
	serverMediaRounds       int
	skillTurn               skillTurnState
	sendPrompt              string
	curAttempt              int
	lastUpstreamStatus      int
	lastRetryReason         string

	ttftMs           atomic.Int64
	headerMs         atomic.Int64
	identityMs       atomic.Int64
	firstReasoningMs atomic.Int64
	firstContentMs   atomic.Int64
	firstToolMs      atomic.Int64
	uploadMs         atomic.Int64
	accountSwitches  atomic.Int64

	streamMu          sync.Mutex
	headersSent       bool
	handlerDone       atomic.Bool
	pingerMu          sync.Mutex
	pinger            *streamPinger
	streamStarted     chan struct{}
	streamStartedOnce sync.Once

	pendingAction    map[string]any
	lastActionSig    string
	sentCitationKeys map[string]bool

	// All locked hooks run while streamMu is held and ctx/handlerDone are checked.
	onStreamCommittedLocked func()
	emitActionLocked        func(payload map[string]any)
	emitCitationsLocked     func(items []gemini.GroundingSource)
	onKeepalive             func()
}

func newGenerationSession(
	s *Server,
	ctx context.Context,
	w http.ResponseWriter,
	prep *preparedGeneration,
	messages []map[string]any,
	toolsRaw, toolChoiceRaw any,
	reqID, endpoint string,
	reqStartedAt time.Time,
	prepareMs int64,
) *generationSession {
	flusher, _ := w.(http.Flusher)
	prep.account.mu.RLock()
	originGroup := prep.account.AccountGroup
	prep.account.mu.RUnlock()

	maxAttempts := s.Cfg.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	maxUpstream := s.Cfg.MaxUpstreamAttempts
	if maxUpstream < 1 {
		maxUpstream = maxAttempts
	}
	var retryDeadline time.Time
	if s.Cfg.RetryBudget > 0 {
		retryDeadline = reqStartedAt.Add(s.Cfg.RetryBudget)
	}

	g := &generationSession{
		s:                   s,
		ctx:                 ctx,
		w:                   w,
		flusher:             flusher,
		prep:                prep,
		messages:            messages,
		toolsRaw:            toolsRaw,
		toolChoiceRaw:       toolChoiceRaw,
		reqID:               reqID,
		endpoint:            endpoint,
		reqStartedAt:        reqStartedAt,
		prepareMs:           prepareMs,
		eagerStream:         strings.EqualFold(s.Cfg.StreamBufferMode, "eager"),
		stream:              prep.stream,
		buffered:            prep.buffered,
		accts:               prep.accts,
		account:             prep.account,
		model:               prep.model,
		mid:                 prep.mid,
		requested:           prep.requested,
		effortStr:           prep.effortStr,
		hasTools:            prep.hasTools,
		opts:                prep.opts,
		fullPrompt:          prep.fullPrompt,
		curPrompt:           prep.curPrompt,
		flattenStats:        prep.flattenStats,
		overflowParts:       prep.overflowParts,
		imageParts:          prep.imageParts,
		fullAttachmentParts: prep.fullAttachmentParts,
		attachmentParts:     prep.attachmentParts,
		hasher:              prep.hasher,
		cacheKey:            prep.cacheKey,
		cacheUsed:           prep.cacheKey != "",
		originGroup:         originGroup,
		maxAttempts:         maxAttempts,
		maxUpstream:         maxUpstream,
		retryDeadline:       retryDeadline,
		triedAccounts:       map[string]bool{prep.account.ID: true},
		sendPrompt:          prep.curPrompt,
		streamStarted:       make(chan struct{}),
		sentCitationKeys:    make(map[string]bool),
	}
	g.ttftMs.Store(-1)
	g.headerMs.Store(-1)
	g.identityMs.Store(-1)
	g.firstReasoningMs.Store(-1)
	g.firstContentMs.Store(-1)
	g.firstToolMs.Store(-1)
	return g
}

func (g *generationSession) markPhase(target *atomic.Int64) {
	target.CompareAndSwap(-1, time.Since(g.reqStartedAt).Milliseconds())
}

func (g *generationSession) markMeaningfulOutput(target *atomic.Int64) {
	g.markPhase(target)
	g.markPhase(&g.ttftMs)
}

func (g *generationSession) assignAccount(next *AccountState) {
	if next != nil {
		g.streamMu.Lock()
		if next != g.account {
			g.accountSwitches.Add(1)
		}
		g.account = next
		g.streamMu.Unlock()
	}
}

func (g *generationSession) stopKeepalive() {
	g.pingerMu.Lock()
	p := g.pinger
	g.pinger = nil
	g.pingerMu.Unlock()
	p.Stop()
}

func (g *generationSession) close() {
	g.handlerDone.Store(true)
	g.stopKeepalive()
}

func (g *generationSession) headersCommitted() bool {
	g.streamMu.Lock()
	defer g.streamMu.Unlock()
	return g.headersSent
}

func (g *generationSession) startStreamWith(attempt, upstreamStatus int, retryReason string) {
	g.streamMu.Lock()
	if g.headersSent || g.ctx.Err() != nil || g.handlerDone.Load() {
		g.streamMu.Unlock()
		return
	}
	setGSBHeaders(g.w, false, g.account.ID, g.mid, attempt, upstreamStatus, retryReason)
	g.w.Header().Set("Content-Type", "text/event-stream")
	g.w.Header().Set("Cache-Control", "no-cache")
	g.w.Header().Set("Connection", "keep-alive")
	g.w.WriteHeader(200)
	g.headersSent = true
	g.streamStartedOnce.Do(func() { close(g.streamStarted) })
	g.markPhase(&g.headerMs)
	_, _ = io.WriteString(g.w, ": ping\n\n")
	if g.flusher != nil {
		g.flusher.Flush()
	}
	if g.onStreamCommittedLocked != nil {
		g.onStreamCommittedLocked()
	}
	if g.pendingAction != nil {
		pa := g.pendingAction
		g.pendingAction = nil
		g.emitActionDeduplicatedLocked(pa)
	}
	g.streamMu.Unlock()

	g.pingerMu.Lock()
	if g.pinger == nil && !g.handlerDone.Load() {
		g.pinger = startStreamPinger(&g.streamMu, g.w, g.flusher, defaultKeepaliveInterval, g.onKeepalive)
	}
	g.pingerMu.Unlock()
}

func (g *generationSession) startStream() {
	g.startStreamWith(g.curAttempt, g.lastUpstreamStatus, g.lastRetryReason)
}

func (g *generationSession) startEarlyOrGrace() {
	if g.stream && g.s.Cfg.EarlySSE && (g.eagerStream || !g.s.Cfg.RejectDowngrade) {
		g.startStream()
	} else if g.stream && g.buffered && g.s.Cfg.EarlySSEGrace > 0 {
		go func() {
			timer := time.NewTimer(g.s.Cfg.EarlySSEGrace)
			defer timer.Stop()
			select {
			case <-timer.C:
				// No upstream call has completed yet, so attempt/status/retry are
				// unknown here; pass -1 to omit them instead of reading variables owned
				// by the request goroutine.
				g.startStreamWith(-1, 0, "")
			case <-g.ctx.Done():
			case <-g.streamStarted:
			}
		}()
	}
}

func (g *generationSession) emitActionDeduplicatedLocked(payload map[string]any) {
	if len(payload) == 0 || g.emitActionLocked == nil || g.ctx.Err() != nil || g.handlerDone.Load() {
		return
	}
	sigBytes, _ := json.Marshal(payload)
	sig := string(sigBytes)
	if sig == g.lastActionSig {
		return
	}
	g.lastActionSig = sig
	g.emitActionLocked(payload)
}

func (g *generationSession) emitActionStatus(a *gemini.ActionStatus) {
	payload := buildActionStatusPayload(a)
	if payload == nil {
		return
	}
	g.streamMu.Lock()
	defer g.streamMu.Unlock()
	if !g.headersSent {
		g.pendingAction = payload
		return
	}
	g.emitActionDeduplicatedLocked(payload)
}

func (g *generationSession) emitCitations(sources []gemini.GroundingSource) {
	if len(sources) == 0 || g.emitCitationsLocked == nil {
		return
	}
	withIDs := gemini.GroundingCitationsWithIDs(sources)
	g.streamMu.Lock()
	defer g.streamMu.Unlock()
	if !g.headersSent || g.ctx.Err() != nil || g.handlerDone.Load() {
		return
	}
	var unseen []gemini.GroundingSource
	for _, src := range withIDs {
		if src.URL == "" && src.Title == "" {
			continue
		}
		key := strconv.Itoa(src.ID) + "\x00" + src.URL + "\x00" + src.Title + "\x00" + src.MimeType + "\x00" + src.Snippet
		if g.sentCitationKeys[key] {
			continue
		}
		g.sentCitationKeys[key] = true
		unseen = append(unseen, src)
	}
	if len(unseen) > 0 {
		g.emitCitationsLocked(unseen)
	}
}

func safeErrorReplyPrefix(text string) string {
	return gemini.GeminiErrorReply(text[:snapUTF8Start(text, 240)])
}

// runUpstreamAttempt executes one upload + StreamGenerateLive call and updates
// attemptState via onFrameDelta as frames arrive.
type attemptState struct {
	mu               sync.Mutex
	startedAt        time.Time
	candidateID      string
	selected         bool
	done             bool
	text             string
	thoughts         string
	sentText         string
	sentThought      string
	servedID         string
	servedLabel      string
	identityConflict bool
	mediaToolUsed    bool
	lastImages       []gemini.GeneratedImage
	lastVideos       []gemini.GeneratedVideo
	lastCID          string
	lastRID          string
	lastContextToken string
	grounding        []gemini.GroundingSource
}

func (a *attemptState) observeIdentity(ansID, ansLabel string) {
	if ansID == "" {
		return
	}
	if a.servedID != "" && a.servedID != ansID {
		a.identityConflict = true
	}
	a.servedID = ansID
	if ansLabel != "" {
		a.servedLabel = ansLabel
	}
}

func (g *generationSession) runUpstreamAttempt(
	c UpstreamClient,
	scope AuthScope,
	as *attemptState,
	toolStreamer *toolbridge.LiveToolCallStreamer,
	onIdentityVerified func(),
	onFrameDelta func(frameDone bool) error,
) (*gemini.StreamResponse, error) {
	g.opts.FreshSocket = g.freshSocketNext
	if g.rebootstrapNext {
		_ = c.Bootstrap(g.ctx)
		g.skillTurn.invalidate()
	}
	g.freshSocketNext = false
	g.rebootstrapNext = false

	uploadStartedAt := time.Now()
	files, uploadErr := g.s.uploadAttachments(g.ctx, g.account.ID, c, g.attachmentParts)
	g.uploadMs.Add(time.Since(uploadStartedAt).Milliseconds())
	if len(files) > 0 {
		g.opts.Files = files
	}
	if uploadErr != nil {
		return nil, uploadErr
	}

	g.sendPrompt = g.skillTurn.prepare(g.ctx, g.s, scope, g.account, &g.opts, g.curPrompt)
	g.upstreamCalls++
	g.account.mu.Lock()
	g.account.activeStreams++
	g.account.mu.Unlock()
	defer func() {
		g.account.mu.Lock()
		if g.account.activeStreams > 0 {
			g.account.activeStreams--
		}
		g.account.mu.Unlock()
	}()

	upstream, streamErr := c.StreamGenerateLive(g.ctx, g.sendPrompt, g.opts, func(update *gemini.LineParseResult) error {
		as.mu.Lock()
		if update.MediaToolActive {
			as.mediaToolUsed = true
		}
		if as.mediaToolUsed {
			as.servedID = g.model.ModelID
			as.servedLabel = g.model.ModelName
			as.identityConflict = false
		} else {
			as.observeIdentity(update.AnsweredModelID, update.AnsweredModelLabel)
		}
		if as.servedID != "" {
			g.markPhase(&g.identityMs)
		}
		earlyMismatch := !as.mediaToolUsed && g.s.Cfg.RejectDowngrade && (as.servedID != "" || as.servedLabel != "") && !gemini.IsMatchingServedModel(*g.model, as.servedID, as.servedLabel)
		identityVerifiedNow := canStreamVerified(g.eagerStream, g.s.Cfg.RejectDowngrade, as.identityConflict, *g.model, as.servedID, as.servedLabel)
		as.mu.Unlock()
		if earlyMismatch {
			return transport.ErrStopStreaming
		}
		if g.stream && g.buffered && g.ctx.Err() == nil && identityVerifiedNow {
			if onIdentityVerified != nil {
				onIdentityVerified()
			} else {
				g.startStream()
			}
		}
		if update.Action != nil {
			g.emitActionStatus(update.Action)
		}

		if update.CID != "" {
			as.lastCID = update.CID
		}
		if update.RID != "" {
			as.lastRID = update.RID
		}
		if update.ContextToken != "" {
			as.lastContextToken = update.ContextToken
		}
		as.grounding = gemini.MergeGroundingSources(as.grounding, update.GroundingSources)
		if g.stream && !g.buffered {
			g.emitCitations(as.grounding)
		}
		for _, frame := range update.Frames {
			if !as.selected {
				as.candidateID = frame.RCID
				as.selected = true
			} else if frame.RCID != as.candidateID {
				if len(frame.GeneratedVideos) > 0 && len(as.lastVideos) == 0 && as.text == "" && as.sentText == "" {
					as.candidateID = frame.RCID
				} else {
					continue
				}
			}
			as.mu.Lock()
			g.s.rewriteFrameMedia(g.ctx, g.account.ID, &frame)
			if len(frame.GeneratedImages) > 0 || len(frame.GeneratedVideos) > 0 {
				as.mediaToolUsed = true
				if len(frame.GeneratedImages) > 0 {
					as.lastImages = frame.GeneratedImages
				}
				if len(frame.GeneratedVideos) > 0 {
					as.lastVideos = frame.GeneratedVideos
				}
			}
			if as.mediaToolUsed {
				as.servedID = g.model.ModelID
				as.servedLabel = g.model.ModelName
				as.identityConflict = false
			} else {
				as.observeIdentity(frame.AnsweredModelID, frame.AnsweredModelLabel)
			}
			if as.servedID != "" {
				g.markPhase(&g.identityMs)
			}
			if frame.Text != "" || as.text == "" {
				if !shouldPreserveTextOnErrorFrame(as.text, frame.Text, as.sentText, g.hasTools, g.toolsRaw, g.toolChoiceRaw, toolStreamer) {
					as.text = frame.Text
				}
			}
			as.thoughts = mergeStreamedThoughts(as.thoughts, frame.Thoughts)
			as.done = frame.Done
			if as.done && !scope.StructuredMedia {
				as.text = gemini.AppendGeneratedMediaMarkdown(as.text, as.lastImages, as.lastVideos)
			}
			frameMismatch := g.s.Cfg.RejectDowngrade && (as.servedID != "" || as.servedLabel != "") && !gemini.IsMatchingServedModel(*g.model, as.servedID, as.servedLabel)
			as.mu.Unlock()
			if frameMismatch {
				return transport.ErrStopStreaming
			}
			if onFrameDelta != nil {
				if err := onFrameDelta(frame.Done); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if upstream != nil && upstream.Status > 0 {
		g.lastUpstreamStatus = upstream.Status
	}
	if upstream != nil {
		as.grounding = gemini.MergeGroundingSources(as.grounding, upstream.GroundingSources)
	}
	return upstream, streamErr
}

// handleAttemptFailure runs error classification, account cooldown/quarantine,
// cache invalidation, and retry/failover transitions. It returns true when the
// caller should retry the request loop.
func (g *generationSession) handleAttemptFailure(
	c UpstreamClient,
	scope AuthScope,
	as *attemptState,
	runCheck error,
	payloadCommitted bool,
) bool {
	var ue *transport.UpstreamError
	errors.As(runCheck, &ue)
	errCode := ""
	errStatus := 502
	if ue != nil {
		errCode = ue.Code
		errStatus = ue.StatusCode
		if g.lastUpstreamStatus == 0 && errStatus > 0 {
			g.lastUpstreamStatus = errStatus
		}
	}
	if errCode == "account_banned" {
		g.s.markAccountQuarantined(g.account, runCheck.Error())
	} else if errCode != "session_busy" && errCode != "client_cancelled" && errCode != "upstream_error_reply" && errCode != "tool_call_error" && errCode != "model_label_mismatch" {
		g.s.markAccountFailure(g.account, runCheck.Error())
		if errCode != "empty_upstream_response" {
			g.s.refreshQuotaAsync(g.account, c)
		}
	}
	if g.cacheKey != "" && len(g.opts.Metadata) > 0 && !as.mediaToolUsed && (errCode == "empty_upstream_response" || errCode == "incomplete_upstream_response" || errCode == "upstream_error_reply" || errCode == "upstream_error" || (errCode == "upstream_http_error" && errStatus >= 400)) {
		g.s.deleteSessionCache(g.cacheKey)
		g.s.deleteSessionBinding(g.prep.sessionBindingKey)
		g.cacheKey = ""
	}

	responseCommitted := g.headersCommitted() || payloadCommitted || as.sentThought != ""
	inStreamRetryable := (errCode == "tool_call_error" ||
		errCode == "upstream_error_reply" ||
		errCode == "empty_upstream_response" ||
		errCode == "incomplete_upstream_response" ||
		(scope.AccountID == "" && (errCode == "model_substitution" ||
			errStatus == 429 ||
			errCode == "rate_limit_error" ||
			errCode == "session_busy" ||
			errCode == "upstream_auth_error" ||
			errCode == "auth_redirect" ||
			errCode == "upstream_connect_timeout" ||
			errCode == "upstream_stalled" ||
			errCode == "upstream_timeout" ||
			errCode == "upstream_degraded" ||
			errCode == "model_identity_unknown" ||
			errCode == "model_identity_conflict"))) && !payloadCommitted
	canRetryMore := g.curAttempt+1 < g.maxAttempts && g.upstreamCalls < g.maxUpstream &&
		(g.retryDeadline.IsZero() || time.Now().Before(g.retryDeadline)) &&
		(!responseCommitted || inStreamRetryable) && g.ctx.Err() == nil &&
		errCode != "account_banned"

	if canRetryMore {
		if as.sentThought != "" {
			g.priorAttemptSentThought = true
		}
		if g.tryApplyRetryTransition(c, scope, errCode, errStatus, ue, runCheck) {
			return true
		}
	}

	g.stopKeepalive()
	headersSent := g.headersCommitted()
	setGSBHeaders(g.w, headersSent, g.account.ID, g.mid, g.curAttempt, g.lastUpstreamStatus, g.lastRetryReason)
	g.logCompletion(errStatus, errCode, 0, as.servedID, as.servedLabel)
	return false
}

func (g *generationSession) switchFallbackAccount(scope AuthScope) bool {
	if scope.AccountID != "" {
		return false
	}
	nextAcct, nextModel := g.s.pickFallbackAccount(g.accts, g.triedAccounts, g.originGroup, g.requested, g.hasher)
	if nextAcct == nil || nextModel == nil {
		return false
	}
	g.triedAccounts[nextAcct.ID] = true
	g.assignAccount(nextAcct)
	g.model = nextModel
	g.opts.Model = nextModel
	g.opts.Metadata = nil
	g.opts.Files = nil
	g.curPrompt = g.fullPrompt
	g.attachmentParts = g.fullAttachmentParts
	return true
}

func (g *generationSession) tryApplyRetryTransition(
	c UpstreamClient,
	scope AuthScope,
	errCode string,
	errStatus int,
	ue *transport.UpstreamError,
	runCheck error,
) bool {
	if errCode == "session_busy" {
		g.lastRetryReason = "session_busy"
		if g.switchFallbackAccount(scope) {
			return true
		}
		waitDur := parseRetryAfterDuration(ue, 2*time.Second, g.retryDeadline)
		if waitDur > 0 {
			select {
			case <-time.After(waitDur):
			case <-g.ctx.Done():
			}
		}
		return g.ctx.Err() == nil
	}
	if errCode == "model_substitution" || errStatus == 429 || errCode == "rate_limit_error" {
		g.lastRetryReason = errCode
		if g.lastRetryReason == "" {
			g.lastRetryReason = "rate_limit_error"
		}
		if g.switchFallbackAccount(scope) {
			return true
		}
	}
	if errStatus == 401 || errCode == "upstream_auth_error" || errCode == "auth_redirect" {
		g.lastRetryReason = errCode
		c.InvalidateAuth()
		if !g.switchFallbackAccount(scope) {
			g.freshSocketNext = true
			g.rebootstrapNext = true
		}
		return true
	}
	if errCode == "upstream_connect_timeout" || errCode == "upstream_stalled" || errCode == "upstream_timeout" {
		g.lastRetryReason = errCode
		if g.switchFallbackAccount(scope) {
			return true
		}
	}
	if errCode == "upstream_degraded" {
		g.lastRetryReason = errCode
		if g.switchFallbackAccount(scope) {
			return true
		}
		g.freshSocketNext = true
		g.rebootstrapNext = true
		g.opts.Metadata = nil
		g.opts.Files = nil
		g.curPrompt = g.fullPrompt
		g.attachmentParts = g.fullAttachmentParts
		return true
	}
	if errCode == "model_identity_unknown" || errCode == "model_identity_conflict" {
		g.lastRetryReason = errCode
		if g.switchFallbackAccount(scope) {
			return true
		}
	}
	if errCode == "upstream_transport_error" || errCode == "upstream_error" || errCode == "incomplete_upstream_response" || errCode == "tool_call_error" {
		g.lastRetryReason = errCode
		if errCode != "tool_call_error" {
			g.freshSocketNext = true
			g.rebootstrapNext = true
		}
		g.opts.Metadata = nil
		g.opts.Files = nil
		g.curPrompt = g.fullPrompt
		if errCode == "tool_call_error" {
			g.curPrompt = appendToolErrorCorrection(g.fullPrompt, runCheck.Error())
		}
		g.attachmentParts = g.fullAttachmentParts
		return true
	}
	if errCode == "upstream_error_reply" && g.curAttempt == 0 {
		// Soft block: at most 1 retry on the SAME account without failover or GetQuota.
		g.lastRetryReason = errCode
		g.freshSocketNext = true
		g.rebootstrapNext = true
		g.opts.Metadata = nil
		g.opts.Files = nil
		select {
		case <-time.After(100 * time.Millisecond):
		case <-g.ctx.Done():
		}
		if len(g.fullPrompt) > 120000 {
			if compacted, extraOverflow, cErr := prompt.FlattenMessages(g.messages, g.toolsRaw, g.toolChoiceRaw, prompt.FlattenOptions{
				StartIndex:      0,
				MaxPromptChars:  100000,
				MaxOldToolChars: 12000,
				AllowOverflow:   g.s.Cfg.HistoryOffload,
			}); cErr == nil {
				g.curPrompt = appendErrorReplyRecoveryNote(compacted, g.hasTools)
				g.attachmentParts = make([]prompt.AttachmentPart, 0, len(extraOverflow)+len(g.imageParts))
				g.attachmentParts = append(g.attachmentParts, extraOverflow...)
				g.attachmentParts = append(g.attachmentParts, g.imageParts...)
				return true
			}
		}
		g.curPrompt = appendErrorReplyRecoveryNote(g.fullPrompt, g.hasTools)
		g.attachmentParts = g.fullAttachmentParts
		return true
	}
	if errCode == "empty_upstream_response" {
		g.lastRetryReason = errCode
		wasCachedContinuation := len(g.opts.Metadata) > 0
		g.opts.Metadata = nil
		g.opts.Files = nil
		switched := !wasCachedContinuation && g.switchFallbackAccount(scope)
		if !switched {
			g.freshSocketNext = true
			g.rebootstrapNext = true
		}
		g.curPrompt = g.fullPrompt
		g.attachmentParts = g.fullAttachmentParts
		return true
	}
	return false
}

// resolveMediaContinuation handles server-side media tool execution or direct
// media-generation continuation turns, returning whether the caller should
// re-run the upstream loop.
func (g *generationSession) resolveMediaContinuation(
	scope AuthScope,
	interpreted *toolbridge.InterpretResult,
	as *attemptState,
) ([]toolbridge.Call, bool) {
	var calls []toolbridge.Call
	if interpreted != nil {
		calls = interpreted.Calls
	}
	if hasServerSideMediaCalls(calls) && g.serverMediaRounds < 2 {
		mediaStart := time.Now()
		promptAdd, clientCalls, _ := g.s.executeServerSideMediaCalls(g.ctx, g.accts, scope, g.account, g.model, calls)
		if !g.retryDeadline.IsZero() {
			g.retryDeadline = g.retryDeadline.Add(time.Since(mediaStart))
		}
		if len(clientCalls) == 0 {
			g.fullPrompt += promptAdd
			g.curPrompt = g.fullPrompt
			g.opts.Metadata = nil
			g.serverMediaRounds++
			if g.upstreamCalls > 0 {
				g.upstreamCalls--
			}
			return nil, true
		}
		return clientCalls, false
	}
	if g.hasTools && len(calls) == 0 && (len(as.lastImages) > 0 || len(as.lastVideos) > 0) && g.serverMediaRounds < 2 && as.sentText == "" {
		if promptAdd := formatDirectMediaToolContinuation(as.lastImages, as.lastVideos); promptAdd != "" {
			g.fullPrompt += promptAdd
			g.curPrompt = g.fullPrompt
			g.opts.Metadata = nil
			g.serverMediaRounds++
			if g.upstreamCalls > 0 {
				g.upstreamCalls--
			}
			return nil, true
		}
	}
	return calls, false
}

func (g *generationSession) logCompletion(status int, errCode string, cachedTok int, servedID, servedLabel string) {
	totalMs := time.Since(g.reqStartedAt).Milliseconds()
	g.s.logRequestJSON(map[string]any{
		"req_id":             g.reqID,
		"endpoint":           g.endpoint,
		"model":              g.mid,
		"account":            g.account.ID,
		"effort":             g.effortStr,
		"prompt_bytes":       len(g.sendPrompt),
		"header_bytes":       g.flattenStats.HeaderBytes,
		"body_bytes":         g.flattenStats.BodyBytes,
		"footer_bytes":       g.flattenStats.FooterBytes,
		"tool_bytes":         g.flattenStats.ToolSchemaBytes,
		"ttft_ms":            g.ttftMs.Load(),
		"total_ms":           totalMs,
		"status":             status,
		"error_code":         errCode,
		"cached_tokens":      cachedTok,
		"attempts":           g.curAttempt + 1,
		"prepare_ms":         g.prepareMs,
		"cache_hit":          g.cacheUsed,
		"cache_prefix_split": g.prep.cacheSplit,
		"upload_ms":          g.uploadMs.Load(),
		"identity_ms":        g.identityMs.Load(),
		"header_ms":          g.headerMs.Load(),
		"first_reasoning_ms": g.firstReasoningMs.Load(),
		"first_content_ms":   g.firstContentMs.Load(),
		"first_tool_ms":      g.firstToolMs.Load(),
		"done_ms":            totalMs,
		"account_switches":   g.accountSwitches.Load(),
		"requested_model":    g.mid,
		"served_model":       servedID,
		"served_label":       servedLabel,
		"label_mismatch":     gemini.ServedLabelMismatch(*g.model, servedID, servedLabel),
	})
}

func (g *generationSession) finishSuccess(c UpstreamClient, usage map[string]any, servedID, servedLabel string) {
	cachedTok := 0
	if d, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		cachedTok, _ = d["cached_tokens"].(int)
	}
	g.logCompletion(200, "", cachedTok, servedID, servedLabel)
	g.account.mu.Lock()
	g.account.Fails = 0
	g.account.LastUsed = g.s.now()
	g.account.mu.Unlock()
	g.s.refreshQuotaOnSuccess(g.account, c)
}
