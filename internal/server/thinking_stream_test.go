package server

import (
	"context"
	"strings"
	"testing"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
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
				StatusTemplate: "{tool_logo_placeholder} Skills: Eksie",
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
		`"status_template":"{tool_logo_placeholder} Skills: Eksie"`,
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
	streamingText := padding + "\n* **核心概念** [00:00:16]：打破传统固定思维。\n* **扩展模式** [00:01:29]：支持双人同打。"
	finalText := padding + "\n* **核心概念** [[00:16](https://www.youtube.com/watch?v=DAn34l_YrUM&t=16)]：打破传统固定思维。\n* **扩展模式** [[01:29](https://www.youtube.com/watch?v=DAn34l_YrUM&t=89)]：支持双人同打。\n✅ 验证完毕。"

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
		`✅ 验证完毕。`,
		`data: [DONE]`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected SSE body to contain %s, got:\n%s", want, body)
		}
	}
}

