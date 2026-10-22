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
				State:          "complete",
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
