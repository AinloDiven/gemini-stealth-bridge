package gemini

import (
	"encoding/json"
	"strings"
	"testing"
)

func parseActionFromFrame(t *testing.T, inner []any) *LineParseResult {
	t.Helper()
	innerJSON, err := json.Marshal(inner)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal([]any{[]any{"wrb.fr", nil, string(innerJSON)}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := ParseFrameLine(string(envelope))
	if err != nil || res == nil {
		t.Fatalf("ParseFrameLine failed: %+v err=%v", res, err)
	}
	return res
}

func TestParseActionStatusThinkingTitle(t *testing.T) {
	res := parseActionFromFrame(t, []any{
		nil,
		[]any{"c_1", "r_1"},
		map[string]any{"7": []any{nil, nil, nil, nil, nil, []any{"Analyzing Video Content"}}},
	})
	if res.Action == nil || res.Action.ThinkingTitle != "Analyzing Video Content" {
		t.Fatalf("expected thinking title, got %+v", res.Action)
	}
}

func TestParseActionStatusToolCardRelaysIconInputs(t *testing.T) {
	template := "{tool_logo_placeholder} Skills: Research"
	res := parseActionFromFrame(t, []any{
		nil,
		[]any{"c_1", "r_1"},
		map[string]any{"7": []any{
			template,
			[]any{"skills", []any{nil, nil, "Skill Title", "https://example.invalid/logo.svg", nil, template}, float64(1)},
		}},
	})
	if res.Action == nil {
		t.Fatal("expected action card")
	}
	if res.Action.ToolID != "skills" || res.Action.ToolTitle != "Skill Title" || res.Action.StatusTemplate != template {
		t.Fatalf("unexpected action card: %+v", res.Action)
	}
}

func TestParseActionStatusMirrorSlot(t *testing.T) {
	action := []any{nil, nil, nil, nil, nil, []any{"Mirrored title"}}
	inner := make([]any, 12)
	inner[1] = []any{"c_1", "r_1"}
	inner[2] = nil
	inner[11] = []any{action}
	res := parseActionFromFrame(t, inner)
	if res.Action == nil || res.Action.ThinkingTitle != "Mirrored title" {
		t.Fatalf("expected mirrored thinking title, got %+v", res.Action)
	}
}

func TestParseActionStatusYouTubeInterpolatesToolNameAndDropsOrphanTemplate(t *testing.T) {
	res := parseActionFromFrame(t, []any{
		nil,
		[]any{"c_1", "r_1"},
		map[string]any{"7": []any{
			nil,
			[]any{"youtube_tool", []any{nil, nil, "YouTube", "https://example.invalid/tool.svg", nil, "Connecting to {tool_logo_placeholder} {tool_name_placeholder}"}, float64(1)},
		}},
	})
	if res.Action == nil {
		t.Fatal("expected action card for youtube_tool")
	}
	if res.Action.ToolID != "youtube_tool" || res.Action.ToolTitle != "YouTube" || res.Action.StatusTemplate != "Connecting to {tool_logo_placeholder} YouTube" {
		t.Fatalf("unexpected youtube_tool action card: %+v", res.Action)
	}

	// Subsequent orphan completion template in slot 5 without a tool array must be ignored
	// so it never clobbers pending tool cards or leaks raw brace placeholders.
	orphan := parseActionFromFrame(t, []any{
		nil,
		[]any{"c_1", "r_1"},
		map[string]any{"7": []any{nil, nil, nil, nil, nil, []any{"Connected to {tool_logo_placeholder} {tool_name_placeholder}"}}},
	})
	if orphan.Action != nil {
		t.Fatalf("expected nil Action for orphan tool placeholder template in slot 5, got %+v", orphan.Action)
	}
}

func TestParseActionStatusMediaToolsAndImageMode20(t *testing.T) {
	// data_analysis_tool with mode 20 in toolArr[3][1][7][0] is image generation, even with empty title.
	imgRes := parseActionFromFrame(t, []any{
		nil,
		[]any{"c_img", "r_img"},
		map[string]any{"7": []any{
			nil,
			[]any{"data_analysis_tool", []any{nil, nil, "", ""}, float64(1), []any{nil, []any{nil, nil, nil, nil, nil, nil, nil, []any{float64(20)}}, false}},
		}},
	})
	if imgRes.Action == nil || imgRes.Action.ToolID != "image_generation" || !imgRes.MediaToolActive {
		t.Fatalf("expected image_generation with MediaToolActive=true, got Action=%+v MediaToolActive=%v", imgRes.Action, imgRes.MediaToolActive)
	}

	// Ordinary data_analysis_tool (mode 11) must stay data_analysis_tool and not set MediaToolActive.
	dataRes := parseActionFromFrame(t, []any{
		nil,
		[]any{"c_da", "r_da"},
		map[string]any{"7": []any{
			nil,
			[]any{"data_analysis_tool", []any{nil, nil, "Analysis", ""}, float64(1), []any{float64(11), nil, false}},
		}},
	})
	if dataRes.Action == nil || dataRes.Action.ToolID != "data_analysis_tool" || dataRes.MediaToolActive {
		t.Fatalf("expected ordinary data_analysis_tool with MediaToolActive=false, got Action=%+v MediaToolActive=%v", dataRes.Action, dataRes.MediaToolActive)
	}

	// video_generation in pj[2]["7"] sets MediaToolActive, AsyncVideoCID, and AsyncVideoStatus.
	vidRes := parseActionFromFrame(t, []any{
		nil,
		[]any{"c_vid", "r_vid"},
		map[string]any{"7": []any{
			"Generating your video",
			[]any{"video_generation", []any{nil, nil, "Video", ""}, float64(1)},
		}},
	})
	if !vidRes.MediaToolActive || vidRes.AsyncVideoCID != "c_vid" || vidRes.AsyncVideoStatus != "Generating your video" {
		t.Fatalf("unexpected video_generation parse result: %+v", vidRes)
	}
}

func TestParseCandidateSliceStripsYouTubeArtifactAndHoldsTrailingAutoLink(t *testing.T) {
	cStreaming := []any{
		"rc_yt",
		[]any{"Video link: [[https://www.youtube](https://www.youtube)."},
		nil, nil, nil, nil, nil, nil,
		[]any{float64(1)},
	}
	f1, ok := parseCandidateSlice(cStreaming, "id1", "Flash")
	if !ok {
		t.Fatal("expected parseCandidateSlice f1 ok")
	}
	if f1.Text != "Video link: " {
		t.Fatalf("expected incomplete trailing auto-link held back while streaming, got %q", f1.Text)
	}

	cDone := []any{
		"rc_yt",
		[]any{"Video link: [https://www.youtube.com/watch?v=test123](https://www.youtube.com/watch?v=test123)\nhttp://googleusercontent.com/youtube_content/1"},
		nil, nil, nil, nil, nil, nil,
		[]any{float64(2)},
	}
	f2, ok := parseCandidateSlice(cDone, "id1", "Flash")
	if !ok {
		t.Fatal("expected parseCandidateSlice f2 ok")
	}
	want := "Video link: [https://www.youtube.com/watch?v=test123](https://www.youtube.com/watch?v=test123)"
	if f2.Text != want {
		t.Fatalf("expected youtube_content placeholder stripped and final link preserved, got %q, want %q", f2.Text, want)
	}
}

func TestTrimIncompleteTrailingURLPreservesPrecedingASCIIAndIsMonotonic(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"See (https://example.com/docs", "See ("},
		{"Read [guide](https://example.com/guide", "Read [guide]("},
		{`{"url":"https://example.com/api`, `{"url":"`},
		{"Video link: [[https://www.youtube](https://www.youtube).", "Video link: "},
		{"plain text without url", "plain text without url"},
	}
	for _, tc := range cases {
		if got := trimIncompleteTrailingURL(tc.in); got != tc.want {
			t.Fatalf("trimIncompleteTrailingURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	streams := []string{
		"See (https://example.com/docs) for details.",
		"Read [the guide](https://example.com/guide) next.",
		`{"tool_calls":[{"name":"fetch","arguments":{"url":"https://example.com/api"}}]} `,
		"Video link: [[https://www.youtube](https://www.youtube).",
	}
	for _, full := range streams {
		prev := ""
		for k := 1; k <= len(full); k++ {
			// Only check valid UTF-8 prefix boundaries.
			if k < len(full) && (full[k]&0xc0) == 0x80 {
				continue
			}
			cur := trimIncompleteTrailingURL(full[:k])
			if !strings.HasPrefix(cur, prev) {
				t.Fatalf("non-monotonic trim on %q at %d: prev=%q cur=%q", full, k, prev, cur)
			}
			prev = cur
		}
	}
}

func TestParseConversationPollResponseForRIDAndVideoHints(t *testing.T) {
	if !PromptRequestsVideo("generate a short cat walking video") {
		t.Fatal("expected video prompt to match PromptRequestsVideo")
	}
	if !isPendingAsyncVideoFrame(Frame{Done: true}, "", "") {
		t.Fatal("expected empty frame without videos to be treated as pending")
	}
	if !isPendingAsyncVideoFrame(Frame{Done: true, Text: "Generating your video..."}, "Generating your video", "") {
		t.Fatal("expected wire status match to be treated as pending in English")
	}
	if !isPendingAsyncVideoFrame(Frame{Done: true, Text: "Status: pending..."}, "Status: pending", "") {
		t.Fatal("expected wire status match to work for arbitrary localized wire status")
	}
	if !isPendingAsyncVideoFrame(Frame{Done: true, Text: "Still working"}, "", "Still working") {
		t.Fatal("expected unchanged stream placeholder to stay pending across polls")
	}
	if isPendingAsyncVideoFrame(Frame{Done: true, Text: "I cannot generate that video because of policy."}, "Generating your video", "Generating your video...") {
		t.Fatal("did not expect terminal refusal text to be treated as pending")
	}
	if isPendingAsyncVideoFrame(Frame{Done: true, Text: "Video ready", GeneratedVideos: []GeneratedVideo{{URL: "https://example.com/v.mp4"}}}, "Generating your video", "") {
		t.Fatal("did not expect frame with generated videos to remain pending")
	}

	// Build a multi-turn hNvQHb response where turns[0] is an older turn (r_old)
	// and turns[1] is the active turn (r_new).
	makeCand := func(rcid, text string) []any {
		return []any{rcid, []any{text}, nil, nil, nil, nil, nil, nil, []any{float64(2)}}
	}
	turnOld := []any{[]any{"c_1", "r_old"}, nil, nil, []any{[]any{makeCand("rc_old", "older answer")}}}
	turnNew := []any{[]any{"c_1", "r_new"}, nil, nil, []any{[]any{makeCand("rc_new", "video done")}}}
	innerJSON, _ := json.Marshal([]any{[]any{turnOld, turnNew}})
	bodyBytes, _ := json.Marshal([]any{[]any{"wrb.fr", "hNvQHb", string(innerJSON)}})

	res, err := ParseConversationPollResponseForRID(string(bodyBytes), "r_new")
	if err != nil || res == nil || len(res.Frames) != 1 {
		t.Fatalf("expected target RID frame, got res=%+v err=%v", res, err)
	}
	if res.RID != "r_new" || res.Frames[0].Text != "video done" {
		t.Fatalf("expected r_new 'video done', got RID=%q Text=%q", res.RID, res.Frames[0].Text)
	}

	missing, err := ParseConversationPollResponseForRID(string(bodyBytes), "r_pending")
	if err != nil || missing != nil {
		t.Fatalf("expected nil result when active RID is not yet present among RID-bearing turns, got %+v err=%v", missing, err)
	}
}

func TestParseCandidateSliceStripsGeminiWebMediaComponentTagsCleanly(t *testing.T) {
	// Gemini Web inline JSX media mount tags mixed with surrounding prose.
	cReal := []any{
		"rc_img_tag",
		[]any{"Here is your generated image.\n\n<Image alt=\"Minimalist landscape poster\" src=\"image_generation_content/123456\"/>\n\nLet me know if you want changes."},
		nil, nil, nil, nil, nil, nil,
		[]any{float64(2)},
	}
	fReal, ok := parseCandidateSlice(cReal, "id1", "4 Hydrogen Medium")
	if !ok {
		t.Fatal("expected parseCandidateSlice ok")
	}
	if strings.Contains(fReal.Text, "<Image") || strings.Contains(fReal.Text, "image_generation_content/") {
		t.Fatalf("expected Gemini Web <Image> mount tag stripped, got %q", fReal.Text)
	}
	if !strings.Contains(fReal.Text, "Here is your generated image.") || !strings.Contains(fReal.Text, "Let me know if you want changes.") {
		t.Fatalf("expected surrounding prose preserved, got %q", fReal.Text)
	}

	// Standalone placeholder between paragraphs must not leave triple blank lines,
	// and paired/markdown/googleusercontent tag forms must all be stripped.
	cases := []struct {
		in   string
		want string
	}{
		{
			in:   "<Image alt=\"Generated artwork\" src=\"image_generation_content/123456\"/>",
			want: "",
		},
		{
			in:   "First paragraph.\n\n<Image alt=\"poster\" src=\"image_generation_content/123456\"></Image>\n\nSecond paragraph.",
			want: "First paragraph.\n\nSecond paragraph.",
		},
		{
			in:   "Watch the clip:\n<Video title=\"clip\" src=\"generated_video_content/98765\"/>\nMore details below.",
			want: "Watch the clip:\nMore details below.",
		},
		{
			in:   "![poster](https://googleusercontent.com/image_generation_content/0)\nBody starts here.",
			want: "Body starts here.",
		},
		{
			// Code blocks and normal external image tags must remain untouched.
			in:   "Example:\n```tsx\n<Image alt=\"demo\" src=\"image_generation_content/123\"/>\n```\nExternal image: <Image alt=\"hero\" src=\"https://example.com/hero.png\"/>",
			want: "Example:\n```tsx\n<Image alt=\"demo\" src=\"image_generation_content/123\"/>\n```\nExternal image: <Image alt=\"hero\" src=\"https://example.com/hero.png\"/>",
		},
	}
	for _, tc := range cases {
		c := []any{"rc_case", []any{tc.in}, nil, nil, nil, nil, nil, nil, []any{float64(2)}}
		f, ok := parseCandidateSlice(c, "id1", "Flash")
		if !ok {
			t.Fatalf("expected parseCandidateSlice ok for %q", tc.in)
		}
		if f.Text != tc.want {
			t.Fatalf("parseCandidateSlice(%q) = %q, want %q", tc.in, f.Text, tc.want)
		}
	}

	// Streaming prefixes across a split <Image .../> tag and markdown embed must be monotonic
	// and never emit partial <Image markup.
	streams := []string{
		"Here is your generated image.\n\n<Image alt=\"Generated artwork\" src=\"image_generation_content/123456\"/>\n\nLet me know if you want changes.",
		"Preview ![poster](image_generation_content/12345) looks sharp.",
	}
	for _, full := range streams {
		prev := ""
		for k := 1; k <= len(full); k++ {
			if k < len(full) && (full[k]&0xc0) == 0x80 {
				continue
			}
			cur := cleanGeminiMediaPlaceholders(full[:k], false)
			if strings.Contains(cur, "<Image") || strings.Contains(cur, "image_generation_content") {
				t.Fatalf("streaming prefix %d leaked mount placeholder: %q", k, cur)
			}
			if !strings.HasPrefix(cur, prev) {
				t.Fatalf("non-monotonic streaming cleanup at %d on %q: prev=%q cur=%q", k, full, prev, cur)
			}
			prev = cur
		}
		final := cleanGeminiMediaPlaceholders(full, true)
		if !strings.HasPrefix(final, prev) {
			t.Fatalf("final cleanup broke monotonicity on %q: prev=%q final=%q", full, prev, final)
		}
	}
}
