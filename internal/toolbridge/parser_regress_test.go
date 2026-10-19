package toolbridge

import (
	"strings"
	"testing"
)

func sampleReproTools() any {
	return []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "read_file",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": map[string]any{"type": "string"}},
					"required":   []any{"path"},
				},
			},
		},
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "bash",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{"type": "string"},
						"i":       map[string]any{"type": "string"},
					},
					"required": []any{"command", "i"},
				},
			},
		},
	}
}

// Prose summaries that MENTION the envelope (inline code, fenced example with
// placeholders, prose reference) are final answers, not tool calls. They must
// parse cleanly with zero calls and no error.
func TestProseMentionsAreNotToolCalls(t *testing.T) {
	cases := []struct {
		name  string
		reply string
	}{
		{"fencedexample", "Selesai. Ringkasan perbaikan:\n\n" +
			"- `truncatedEnvRe` hanya dievaluasi saat tidak ada envelope valid, jadi blok ```json {\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"/tmp/x\"}}]} ``` di dalam penjelasan tidak memicu 'malformed or truncated'.\n" +
			"- Semua selesai.\n"},
		{"inlinecode", "Ringkasan: gunakan blok `{\"tool_calls\": [...]}` untuk memanggil tool.\nDilakukan."},
		{"prosetable", "Format yang diminta harness:\n\n| field | isi |\n|---|---|\n| name | nama tool |\n\nContoh: ```json\n{\"tool_calls\":[...]}\n``` (placeholder, jangan dieksekusi).\nSelesai."},
		{"harnessdescription", "Bridge mem-parsing `json {\"tool_calls\":[{\"name\":\"<tool>\",\"arguments\":{...}}]}` sebagai envelope tool call.\nTugas sudah selesai dan tidak ada file yang perlu diubah."},
		{"cardmention", "Ketik `json {\"tool_calls\":[ ... ]}` sesuai skema. Namun tidak ada yang perlu dipanggil — jawaban final: semua target sudah diverifikasi."},
	}
	for _, tc := range cases {
		res, err := InterpretReply(tc.reply, sampleReproTools(), "auto")
		if err != nil {
			t.Errorf("case %q: unexpected error: %v", tc.name, err)
			continue
		}
		if res != nil && len(res.Calls) > 0 {
			t.Errorf("case %q: prose mention wrongly produced calls: %+v", tc.name, res.Calls)
		}
	}
}

// Genuine truncation: envelope stripped brutally by upstream — unclosed call
// array + no closing fence, at the END of the reply.
func TestRealTruncatedEnvelopeStillErrors(t *testing.T) {
	cases := []struct {
		name  string
		reply string
	}{
		{"midarray", `{"tool_calls":[{"name":"read_file","arguments":{"path":"/tmp/a"},`},
		{"noarrayclose", `{"tool_calls":[{"name":"read_file","arguments":{"path":"/tmp/a"}}`},
		{"fencedopencut", "```json\n{\"tool_calls\":[{\"name\":\"read_file\""},
		{"argcut", "```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"pa"},
		{"missingbracketbeforebrace", "```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"/tmp/a\"}}}\n```"},
	}
	for _, tc := range cases {
		_, err := InterpretReply(tc.reply, sampleReproTools(), "auto")
		if err == nil {
			t.Errorf("case %q: expected error, got nil", tc.name)
		} else if !strings.Contains(err.Error(), "malformed or truncated") {
			t.Errorf("case %q: unexpected error %v", tc.name, err)
		}
	}
}

func TestRedundantClosingBracesInToolEnvelope(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		tool  string
	}{
		{
			name:  "extra_trailing_brace_fenced",
			reply: "```json\n{\"tool_calls\":[{\"name\":\"bash\",\"arguments\":{\"command\":\"python3 -c \\\"import sqlite3; conn = sqlite3.connect('transpadang-tracker/data/tracker_202610.db'); print(conn.execute('SELECT name FROM sqlite_master WHERE type=\\\\'table\\\\'').fetchall())\\\"\",\"i\":\"Inspect database tables\"}}]}}\n```",
			tool:  "bash",
		},
		{
			name:  "extra_brace_before_array_close",
			reply: "```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"/tmp/a\"}}}]}\n```",
			tool:  "read_file",
		},
		{
			name:  "extra_bracket_before_envelope_close",
			reply: "```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"/tmp/a\"}}]]}\n```",
			tool:  "read_file",
		},
	}
	for _, tc := range cases {
		res, err := InterpretReply(tc.reply, sampleReproTools(), "auto")
		if err != nil {
			t.Fatalf("case %q: unexpected error: %v", tc.name, err)
		}
		if res == nil || len(res.Calls) != 1 || res.Calls[0].Name != tc.tool {
			t.Fatalf("case %q: expected 1 %q call, got %+v", tc.name, tc.tool, res)
		}
	}
}
