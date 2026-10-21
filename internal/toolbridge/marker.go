package toolbridge

// Tool-envelope detection.
//
// The original implementation used one large case-insensitive regexp to find
// anything that looks like a tool envelope. On a streaming turn that regexp
// ran over the whole accumulated text on every frame, which made it 98% of
// the server's CPU in a 50-frame stream.
//
// The scan below does the same job without the regexp engine. It is checked
// against the original regexp on 200,000 generated strings, so the two cannot
// drift apart; see TestMarkerApproachesAgreeFuzz in the test files.
//
// The marker shapes, and what each one requires:
//
//	"tool_calls"                a quoted marker key, whitespace allowed inside
//	<tool_call>                 an angle-bracket tag, word boundary after it
//	```json                     a fence, word boundary after the marker word
//	{"name":                    a JSON object opener with a marker key
//	[{"name":                   a JSON array opener with a marker key and colon
//
// A bare word is never a marker: "tooling" and "functionality" are prose.

// toolMarkerQuotedKeys are the marker keys that may appear as a quoted string.
var toolMarkerQuotedKeys = []string{
	"tool_calls", "toolcalls", "tool_call",
	"function_call", "function_calls", "functioncalls",
}

// toolMarkerObjectKeys are the marker keys that may open a JSON object.
var toolMarkerObjectKeys = []string{
	"name", "tool", "tool_name", "function",
	"tool_calls", "toolcalls", "tool_call",
	"function_call", "function_calls", "functioncalls",
}

// toolMarkerArrayKeys are the marker keys that may open a JSON array element.
var toolMarkerArrayKeys = []string{
	"name", "tool", "tool_name", "function",
}

// toolMarkerFenceWords are the marker words that may follow a code fence.
var toolMarkerFenceWords = []string{
	"json", "tool_call", "tool_calls", "function_call", "function_calls",
}

// toolMarkerTagWords are the marker words that may follow an angle bracket.
var toolMarkerTagWords = []string{
	"tool_call", "tool_calls", "function_call", "function_calls",
}

// hasPotentialToolMarker reports whether s contains something that looks like
// a tool envelope.
//
// It is the fast equivalent of the original
// `(?is)(?:"\s*(?:tool_calls|...)\s*"|<(?:tool_call|...)\b|...)` regexp.
func hasPotentialToolMarker(s string) bool {
	for i := range len(s) {
		switch s[i] {
		case '"':
			if matchQuotedMarkerKey(s[i:]) {
				return true
			}
		case '<':
			if matchAngleMarkerTag(s[i:]) {
				return true
			}
		case '`':
			if matchMarkerFence(s[i:]) {
				return true
			}
		case '{':
			if matchMarkerObjectOpener(s[i:]) {
				return true
			}
		case '[':
			if matchMarkerArrayOpener(s[i:]) {
				return true
			}
		}
	}
	return false
}

// skipMarkerSpace returns the offset of the first non-space byte in s.
func skipMarkerSpace(s string) int {
	i := 0
	for i < len(s) {
		switch s[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// asciiEqualFold reports whether s starts with the lowercase word.
func asciiEqualFold(s, lower string) bool {
	if len(s) < len(lower) {
		return false
	}
	for i := range len(lower) {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lower[i] {
			return false
		}
	}
	return true
}

// isMarkerWordByte reports whether b continues a word. The regexp's word
// boundary means a marker must not be followed by one of these.
func isMarkerWordByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9', b == '_':
		return true
	}
	return false
}

// matchQuotedMarkerKey matches `"<marker key>"`, whitespace allowed inside.
func matchQuotedMarkerKey(s string) bool {
	if len(s) < 2 || s[0] != '"' {
		return false
	}
	rest := s[1:]
	rest = rest[skipMarkerSpace(rest):]
	for _, key := range toolMarkerQuotedKeys {
		if !asciiEqualFold(rest, key) {
			continue
		}
		after := rest[len(key):]
		after = after[skipMarkerSpace(after):]
		if len(after) > 0 && after[0] == '"' {
			return true
		}
	}
	return false
}

// matchAngleMarkerTag matches `<tool_call ...>` and friends.
func matchAngleMarkerTag(s string) bool {
	rest := s[1:]
	for _, word := range toolMarkerTagWords {
		if !asciiEqualFold(rest, word) {
			continue
		}
		after := rest[len(word):]
		if len(after) == 0 || !isMarkerWordByte(after[0]) {
			return true
		}
	}
	return false
}

// matchMarkerFence matches a ``` fence followed by an optional marker word.
func matchMarkerFence(s string) bool {
	if len(s) < 3 || s[0] != '`' || s[1] != '`' || s[2] != '`' {
		return false
	}
	rest := s[3:]
	rest = rest[skipMarkerSpace(rest):]
	for _, word := range toolMarkerFenceWords {
		if !asciiEqualFold(rest, word) {
			continue
		}
		after := rest[len(word):]
		if len(after) == 0 || !isMarkerWordByte(after[0]) {
			return true
		}
	}
	return false
}

// matchMarkerObjectOpener matches `{"<marker key>":`.
func matchMarkerObjectOpener(s string) bool {
	rest := s[1:]
	rest = rest[skipMarkerSpace(rest):]
	if len(rest) == 0 || rest[0] != '"' {
		return false
	}
	rest = rest[1:]
	for _, key := range toolMarkerObjectKeys {
		if !asciiEqualFold(rest, key) {
			continue
		}
		after := rest[len(key):]
		after = after[skipMarkerSpace(after):]
		if len(after) == 0 || after[0] != '"' {
			continue
		}
		after = after[1:]
		after = after[skipMarkerSpace(after):]
		if len(after) > 0 && after[0] == ':' {
			return true
		}
	}
	return false
}

// matchMarkerArrayOpener matches `[{"<marker key>":`.
func matchMarkerArrayOpener(s string) bool {
	rest := s[1:]
	rest = rest[skipMarkerSpace(rest):]
	if len(rest) == 0 || rest[0] != '{' {
		return false
	}
	rest = rest[1:]
	rest = rest[skipMarkerSpace(rest):]
	if len(rest) == 0 || rest[0] != '"' {
		return false
	}
	rest = rest[1:]
	for _, key := range toolMarkerArrayKeys {
		if !asciiEqualFold(rest, key) {
			continue
		}
		after := rest[len(key):]
		after = after[skipMarkerSpace(after):]
		if len(after) == 0 || after[0] != '"' {
			continue
		}
		after = after[1:]
		after = after[skipMarkerSpace(after):]
		if len(after) > 0 && after[0] == ':' {
			return true
		}
	}
	return false
}
