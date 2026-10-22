package server

import (
	"strings"
	"testing"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
)

// TestThinkingStreamEmitsReasoningLifecycleAndActionTitle verifies the additive
// Gemini extension frames: reasoning start/delta/done stay separate from the
// upstream thinking header, which is relayed as action-status progress_detail.
func TestThinkingStreamEmitsReasoningLifecycleAndActionTitle(t *testing.T) {
	srv, acct := newTestHarness(false, true)
	first := makeFrame("", "step one", false, testModel.ModelID)
	first.Action = &gemini.ActionStatus{ThinkingTitle: "Analyzing Video Content"}
	second := makeFrame("hello", "", true, testModel.ModelID)
	client := clientFromFrames([]gemini.Frame{first, second})

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
	frame := makeFrame("answer", "", true, testModel.ModelID)
	frame.Action = &gemini.ActionStatus{
		ToolID:         "skills",
		ToolTitle:      "Skill Title",
		StatusTemplate: "{tool_logo_placeholder} Skills: Eksie",
		State:          "active",
	}
	rec := runChat(t, srv, acct, clientFromFrames([]gemini.Frame{frame}), map[string]any{"stream": true})
	body := rec.Body.String()

	for _, want := range []string{
		`"x_gemini_action_status"`,
		`"provider":"skills"`,
		`"status_template":"{tool_logo_placeholder} Skills: Eksie"`,
		`"progress_detail":"Skill Title"`,
		`"source_type":"tool_progress"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected SSE body to contain %s, got:\n%s", want, body)
		}
	}
}
