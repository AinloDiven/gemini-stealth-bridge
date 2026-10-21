package toolbridge

import (
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"
)

// referenceToolMarkerRe is the original regexp that hasPotentialToolMarker
// replaced. It stays here as the specification the fast scanner must match.
var referenceToolMarkerRe = regexp.MustCompile("(?is)(?:\"\\s*(?:tool_calls|toolcalls|tool_call|function_call|function_calls|functioncalls)\\s*\"|<(?:tool_call|tool_calls|function_call|function_calls)\\b|```\\s*(?:json|tool_call|tool_calls|function_call|function_calls)\\b|\\{\\s*\"\\s*(?:name|tool|tool_name|function|tool_calls|toolcalls|tool_call|function_call|function_calls|functioncalls)\\s*\"\\s*:|\\[\\s*\\{\\s*\"\\s*(?:name|tool|tool_name|function)\\s*\"\\s*:)")

// TestHasPotentialToolMarkerMatchesReference checks the fast scanner against
// the original regexp on hand-picked shapes, including the near-misses that a
// looser implementation would get wrong.
func TestHasPotentialToolMarkerMatchesReference(t *testing.T) {
	cases := []string{
		"",
		"plain prose with no markers at all",
		`he said "tool_calls" and left`,
		`he said "  tool_calls  " and left`,
		`a <tool_call> tag`,
		"a <tool_call arg=1> tag",
		"```json\n{\"name\": \"x\"}\n```",
		"```JSON",
		"```\nplain fence",
		`{"tool_name": "bash"}`,
		`[{"name": "x"}]`,
		`{"NAME": "x"}`,
		"TOOL_CALLS",
		"FunctionCall",
		`{"NaMe": 1}`,
		"contains function inside a longer word: functionality",
		"contains tool inside a longer word: tooling",
		"contains name inside a longer word: namesake",
		"contains json inside a longer word: jsonpath",
		`{"tool": "x"}`,
		`{ "tool_calls" : [] }`,
		`[ { "name" : 1 } ]`,
		// Near-misses the word boundary rules exclude.
		"```tool_callFunctionCall{",
		`"Tool"FunctionCall[{"tool"`,
	}
	for _, c := range cases {
		want := referenceToolMarkerRe.MatchString(c)
		if got := hasPotentialToolMarker(c); got != want {
			t.Fatalf("hasPotentialToolMarker(%q) = %v, reference says %v", c, got, want)
		}
	}
}

// TestHasPotentialToolMarkerMatchesReferenceFuzz compares the two on randomly
// generated strings, so the scanner cannot drift away from the specification.
//
// A false negative would stream a tool call as plain prose, so this guards a
// behaviour that is hard to notice in production.
func TestHasPotentialToolMarkerMatchesReferenceFuzz(t *testing.T) {
	rng := rand.New(rand.NewPCG(20261021, 7))

	fragments := []string{
		`"tool_calls"`, `"toolcalls"`, `"tool_call"`, `"function_call"`,
		`"function_calls"`, `"functioncalls"`, `"name"`, `"tool"`,
		`"tool_name"`, `"function"`, `"NAME"`, `"Tool"`, `"FUNCTION_CALL"`,
		`<tool_call>`, `<tool_calls>`, `<function_call>`, `<function_calls>`,
		"```json", "```\n", "```tool_call", "```JSON",
		`{`, `}`, `[`, `]`, `:`, `,`, `"`, `\`, "\n", "\t", " ",
		`{"name":`, `{"tool":`, `[{"name":`, `{"tool_calls":`, `"tool_calls" :`,
		`prose`, `the`, `quick`, `brown`, `fox`, `tooling`, `functionality`,
		`namesake`, `jsonpath`, `TOOL_CALLS`, `FunctionCall`, `json`,
	}

	for range 200000 {
		var b strings.Builder
		n := 1 + rng.IntN(20)
		for range n {
			b.WriteString(fragments[rng.IntN(len(fragments))])
		}
		s := b.String()
		want := referenceToolMarkerRe.MatchString(s)
		if got := hasPotentialToolMarker(s); got != want {
			t.Fatalf("hasPotentialToolMarker(%q) = %v, reference says %v", s, got, want)
		}
	}
}

func benchmarkMarkerCorpus() string {
	return strings.Repeat("The quick brown fox jumps over the lazy dog and then some other prose about tools and functions in code. ", 300)
}

func BenchmarkPotentialToolMarker_Regexp(b *testing.B) {
	s := benchmarkMarkerCorpus()
	b.ReportAllocs()
	for b.Loop() {
		referenceToolMarkerRe.MatchString(s)
	}
}

func BenchmarkPotentialToolMarker_Scanner(b *testing.B) {
	s := benchmarkMarkerCorpus()
	b.ReportAllocs()
	for b.Loop() {
		hasPotentialToolMarker(s)
	}
}
