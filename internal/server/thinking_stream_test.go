package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
)

// TestThinkingStreamEmitsReasoningLifecycleAndActionTitle verifies the additive
// Gemini extension frames: reasoning start/delta/done stay separate from the
// upstream thinking header, which is relayed as action-status progress_detail.
func TestThinkingStreamEmitsReasoningLifecycleAndActionTitle(t *testing.T) {
	srv, acct := newTestHarness(false, true)
	client := &mockUpstream{streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
		if err := onFrame(&gemini.LineParseResult{
			CID: "c", RID: "r", AnsweredModelID: testModel.ModelID,
			Action: &gemini.ActionStatus{ThinkingTitle: "Analyzing Video Content"},
			Frames: []gemini.Frame{makeFrame("", "step one", false, testModel.ModelID)},
		}); err != nil {
			return nil, err
		}
		if err := onFrame(&gemini.LineParseResult{
			CID: "c", RID: "r", AnsweredModelID: testModel.ModelID,
			Frames: []gemini.Frame{makeFrame("hello", "", true, testModel.ModelID)},
		}); err != nil {
			return nil, err
		}
		return &gemini.StreamResponse{Status: 200, CID: "c", RID: "r"}, nil
	}}

	rec := runChat(t, srv, acct, client, map[string]any{"stream": true})
	body := rec.Body.String()

	for _, want := range []string{
		`"x_gemini_reasoning"`,
		`"event":"start"`,
		`"event":"delta"`,
		`"event":"done"`,
		`"text":"step one"`,
		`"x_gemini_action_status"`,
		`"progress_detail":"Analyzing Video Content"`,
		`"source_type":"thinking_progress"`,
		`"phase":"thinking"`,
		`"reasoning_content":"step one"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected SSE body to contain %s, got:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("expected stream terminator, got:\n%s", body)
	}

	start := strings.Index(body, `"event":"start"`)
	delta := strings.Index(body, `"event":"delta"`)
	done := strings.Index(body, `"event":"done"`)
	action := strings.Index(body, `"x_gemini_action_status"`)
	finish := strings.Index(body, `"finish_reason"`)
	terminator := strings.Index(body, "data: [DONE]")
	if start < 0 || delta < 0 || done < 0 || action < 0 || finish < 0 || !(start < delta && delta < done && action < terminator && finish < terminator) {
		t.Fatalf("invalid SSE ordering: start=%d delta=%d done=%d action=%d finish=%d terminator=%d\n%s", start, delta, done, action, finish, terminator, body)
	}
}

// TestThinkingActionToolCardKeepsIconInputs verifies that a tool card relays
// only the upstream provider/status-template pair the bot needs for its icon;
// no icon or title is synthesized.
func TestThinkingActionToolCardKeepsIconInputs(t *testing.T) {
	srv, acct := newTestHarness(false, true)
	client := &mockUpstream{streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
		if err := onFrame(&gemini.LineParseResult{
			CID: "c", RID: "r", AnsweredModelID: testModel.ModelID,
			Action: &gemini.ActionStatus{
				ToolID:         "skills",
				ToolTitle:      "Skill Title",
				StatusTemplate: "{tool_logo_placeholder} Skills: Research",
			},
			Frames: []gemini.Frame{makeFrame("answer", "", true, testModel.ModelID)},
		}); err != nil {
			return nil, err
		}
		return &gemini.StreamResponse{Status: 200, CID: "c", RID: "r"}, nil
	}}

	rec := runChat(t, srv, acct, client, map[string]any{"stream": true})
	body := rec.Body.String()

	for _, want := range []string{
		`"x_gemini_action_status"`,
		`"provider":"skills"`,
		`"status_template":"{tool_logo_placeholder} Skills: Research"`,
		`"progress_detail":"Skill Title"`,
		`"source_type":"tool_progress"`,
		`"state":"active"`,
		`"terminal":false`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected SSE body to contain %s, got:\n%s", want, body)
		}
	}
}

// TestYouTubeStreamTimestampLinkificationDoesNotTriggerUpstreamRevision verifies
// that when Gemini Web streams >240 bytes containing plain [00:00:16] timestamps
// and retroactively rewrites them into [[00:16](https://www.youtube.com/watch?v=...)]
// on the final frame (done=true), the stream completes cleanly without emitting an
// upstream_revision error frame after the reply.
func TestYouTubeStreamTimestampLinkificationDoesNotTriggerUpstreamRevision(t *testing.T) {
	srv, acct := newTestHarness(false, true)
	padding := strings.Repeat("这是一段关于视频内容的详细解析文字。", 10)
	streamingText := padding + "\n* **核心概念** [00:00:16]：打破传统固定思维。\n* **扩展模式** [00:02:26]：支持双人同打。\n\n👉 **视频链接**：[[https://www.youtube](https://www.youtube)."
	finalText := padding + "\n* **核心概念** [[00:16](https://www.youtube.com/watch?v=SntsJWcCEhs&t=16)]：打破传统固定思维。\n* **扩展模式** [[02:26](https://www.youtube.com/watch?v=SntsJWcCEhs&t=146)]：支持双人同打。\n\n👉 **视频链接**：[https://www.youtube.com/watch?v=SntsJWcCEhs](https://www.youtube.com/watch?v=SntsJWcCEhs)"

	client := &mockUpstream{streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
		if err := onFrame(&gemini.LineParseResult{
			CID: "c", RID: "r",
			Action: &gemini.ActionStatus{
				ToolID:         "youtube_tool",
				ToolTitle:      "YouTube",
				StatusTemplate: "正在连接 {tool_logo_placeholder} {tool_name_placeholder}",
			},
		}); err != nil {
			return nil, err
		}
		if err := onFrame(&gemini.LineParseResult{
			CID: "c", RID: "r", AnsweredModelID: testModel.ModelID,
			Frames: []gemini.Frame{makeFrame(streamingText, "", false, testModel.ModelID)},
		}); err != nil {
			return nil, err
		}
		if err := onFrame(&gemini.LineParseResult{
			CID: "c", RID: "r", AnsweredModelID: testModel.ModelID,
			Frames: []gemini.Frame{makeFrame(finalText, "", true, testModel.ModelID)},
		}); err != nil {
			return nil, err
		}
		return &gemini.StreamResponse{Status: 200, CID: "c", RID: "r"}, nil
	}}

	rec := runChat(t, srv, acct, client, map[string]any{"stream": true})
	body := rec.Body.String()

	if strings.Contains(body, "upstream_revision") || strings.Contains(body, `"error"`) {
		t.Fatalf("unexpected error in SSE stream after YouTube timestamp linkification:\n%s", body)
	}
	for _, want := range []string{
		`"provider":"youtube_tool"`,
		`"status_template":"正在连接 {tool_logo_placeholder} YouTube"`,
		`data: [DONE]`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected SSE body to contain %s, got:\n%s", want, body)
		}
	}
}

func TestStreamParenthesizedURLAcrossFramesDoesNotTriggerUpstreamRevision(t *testing.T) {
	srv, acct := newTestHarness(false, true)
	padding := strings.Repeat("这是一段足够超过流式缓冲阈值的说明文字。", 10)
	parseWireCandidate := func(text string, status float64) *gemini.LineParseResult {
		inner, err := json.Marshal([]any{
			nil,
			[]any{"c_url", "r_url"},
			nil, nil,
			[]any{
				[]any{"rc_url", []any{text}, nil, nil, nil, nil, nil, nil, []any{status}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		env, err := json.Marshal([]any{[]any{"wrb.fr", nil, string(inner)}})
		if err != nil {
			t.Fatal(err)
		}
		res, err := gemini.ParseFrameLine(string(env))
		if err != nil || res == nil || len(res.Frames) != 1 {
			t.Fatalf("ParseFrameLine failed: %+v err=%v", res, err)
		}
		res.AnsweredModelID = testModel.ModelID
		res.Frames[0].AnsweredModelID = testModel.ModelID
		return res
	}

	u1 := parseWireCandidate(padding+"详见 (", 1)
	u2 := parseWireCandidate(padding+"详见 (https://example.com/docs/path", 1)
	u3 := parseWireCandidate(padding+"详见 (https://example.com/docs/path) 说明。", 2)

	client := &mockUpstream{streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
		for _, u := range []*gemini.LineParseResult{u1, u2, u3} {
			if err := onFrame(u); err != nil {
				return nil, err
			}
		}
		return &gemini.StreamResponse{Status: 200, CID: "c_url", RID: "r_url"}, nil
	}}

	rec := runChat(t, srv, acct, client, map[string]any{"stream": true})
	body := rec.Body.String()
	if strings.Contains(body, "upstream_revision") || strings.Contains(body, `"error"`) {
		t.Fatalf("unexpected error streaming parenthesized URL across frames:\n%s", body)
	}
	if !strings.Contains(body, "https://example.com/docs/path) 说明。") {
		t.Fatalf("expected held-back URL flushed intact on completion, got:\n%s", body)
	}
}

// TestStreamTimestampStrippingDoesNotTriggerUpstreamRevision verifies that when
// Gemini Web streams unlinked clock timestamps (e.g. " [00:00:30]") and strips
// them on the final frame (done=true) because no video metadata is attached to
// the turn, the stream completes cleanly without upstream_revision.
func TestStreamTimestampStrippingDoesNotTriggerUpstreamRevision(t *testing.T) {
	srv, acct := newTestHarness(false, true)
	padding := strings.Repeat("从医学和生理学角度来说，适度清理确实能让人身心放松、缓解压力。", 5)
	streamingText := padding + "甚至对健康有一定的好处 [00:00:30]，完全属于正常需求。但如果在纯粹浪费体力了的说 [00:02:11]！"
	finalText := padding + "甚至对健康有一定的好处，完全属于正常需求。但如果在纯粹浪费体力了的说！\n\n总之全凭个人习惯。"

	client := &mockUpstream{streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
		if err := onFrame(&gemini.LineParseResult{
			CID: "c", RID: "r", AnsweredModelID: testModel.ModelID,
			Frames: []gemini.Frame{makeFrame(streamingText, "", false, testModel.ModelID)},
		}); err != nil {
			return nil, err
		}
		if err := onFrame(&gemini.LineParseResult{
			CID: "c", RID: "r", AnsweredModelID: testModel.ModelID,
			Frames: []gemini.Frame{makeFrame(finalText, "", true, testModel.ModelID)},
		}); err != nil {
			return nil, err
		}
		return &gemini.StreamResponse{Status: 200, CID: "c", RID: "r"}, nil
	}}

	rec := runChat(t, srv, acct, client, map[string]any{"stream": true})
	body := rec.Body.String()

	if strings.Contains(body, "upstream_revision") || strings.Contains(body, `"error"`) {
		t.Fatalf("unexpected error in SSE stream after timestamp stripping:\n%s", body)
	}
	if !strings.Contains(body, "总之全凭个人习惯。") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("expected final tail and stream terminator, got:\n%s", body)
	}

	if tail, ok := remainingStreamTail("This has benefits for health.", "This has benefits [00:00:30] for"); !ok || tail != " health." {
		t.Fatalf("expected English stripped timestamp tail ' health.', got ok=%v tail=%q", ok, tail)
	}
	if tail, ok := remainingStreamTail("好处", "好处 [00:00:30]"); !ok || tail != "" {
		t.Fatalf("expected trailing stripped timestamp to match with empty tail, got ok=%v tail=%q", ok, tail)
	}
	if hasCompatibleStreamPrefix("好处，完全不同", "好处 [1]，完全不同") {
		t.Fatalf("non-clock bracket [1] should not be treated as stripped timestamp")
	}
	if hasCompatibleStreamPrefix("好处，其实不然", "好处 [00:00:30]，完全不同") {
		t.Fatalf("genuine text revision must still be rejected")
	}
}

func TestThinkingStreamClosesReasoningBeforeMidStreamError(t *testing.T) {
	srv, acct := newTestHarness(false, true)
	srv.Cfg.MaxAttempts = 1
	client := &mockUpstream{streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
		if err := onFrame(&gemini.LineParseResult{
			CID: "c", RID: "r", AnsweredModelID: testModel.ModelID,
			Frames: []gemini.Frame{makeFrame("", "partial thought", false, testModel.ModelID)},
		}); err != nil {
			return nil, err
		}
		return nil, transport.NewUpstreamError("stream interrupted", 502, "upstream_error", nil, nil)
	}}

	rec := runChat(t, srv, acct, client, map[string]any{"stream": true})
	body := rec.Body.String()

	doneIdx := strings.Index(body, `"event":"done"`)
	errIdx := strings.Index(body, `"error":`)
	if doneIdx < 0 || errIdx < 0 {
		t.Fatalf("expected reasoning done and error chunk in SSE body:\n%s", body)
	}
	if doneIdx >= errIdx {
		t.Fatalf("expected reasoning done (%d) before error (%d):\n%s", doneIdx, errIdx, body)
	}
}
