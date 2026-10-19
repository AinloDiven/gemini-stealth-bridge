package toolbridge

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
)

var toolNameRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// BuiltinMediaToolName is the server-side tool injected into agent tool lists
// so Gemini can invoke native Imagen / Nano Banana (image) and Veo (video)
// generation during tool-enabled coding/agent sessions.
const BuiltinMediaToolName = "generate_media"

// AugmentToolsWithMediaGenerator appends the server-side generate_media tool
// when tools are enabled in "auto" mode and the caller has not already defined
// a tool with that name.
func AugmentToolsWithMediaGenerator(tools any, toolChoice any) any {
	contract, err := ValidateToolRequest(tools, toolChoice)
	if err != nil || len(contract.Tools) == 0 || (contract.ChoiceMode != "auto" && contract.ChoiceMode != "") {
		return tools
	}
	for _, t := range contract.Tools {
		if t.Name == BuiltinMediaToolName {
			return tools
		}
	}
	mediaTool := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        BuiltinMediaToolName,
			"description": "Generate a real high-definition image (Imagen / Nano Banana JPEG) or video with audio (Veo MP4, ~8-10 seconds per clip) using Gemini's native media generator. ALWAYS call this tool directly whenever the user asks to create, make, or revise any video or image—NEVER inspect or run ffmpeg, ffprobe, espeak, or python scripts unless the user explicitly asks to edit/concatenate media with ffmpeg. Returns a local downloadable HTTP URL (http://.../v1/media/...) and a curl command to save it to the workspace.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"media_type": map[string]any{
						"type":        "string",
						"enum":        []string{"image", "video"},
						"description": "Type of media to generate: 'image' or 'video'.",
					},
					"prompt": map[string]any{
						"type":        "string",
						"description": "Detailed visual description of the image or video to generate.",
					},
				},
				"required": []string{"media_type", "prompt"},
			},
		},
	}
	if rawSlice, ok := tools.([]any); ok {
		out := make([]any, 0, len(rawSlice)+1)
		out = append(out, rawSlice...)
		out = append(out, mediaTool)
		return out
	}
	return tools
}

// ToolDef is a normalized tool definition.
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// Contract holds the validated tools and normalized toolChoice for a request.
type Contract struct {
	Tools          []ToolDef
	ChoiceMode     string // "auto", "none", "required", "function"
	ForcedFunction string
}

// Call represents a single parsed tool call.
type Call struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// OpenAIToolCall is the wire format returned in OpenAI chat completion responses.
type OpenAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function OpenAIFunctionCall `json:"function"`
}

// OpenAIFunctionCall is the function sub-object of an OpenAIToolCall.
type OpenAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ParseResult is the result of ParseToolReply.
type ParseResult struct {
	Calls  []Call
	Reason string
	Text   string
}

// InterpretResult is the result of InterpretReply when tool calls are present.
type InterpretResult struct {
	Calls []Call
	Text  string
}

func toolCallError(msg string) error {
	return transport.NewUpstreamError(msg, 502, "tool_call_error", nil, nil)
}

func invalidRequest(msg string) error {
	return transport.NewUpstreamError(msg, 400, "invalid_request_error", nil, nil)
}

// ValidateToolRequest validates the caller's tools and tool_choice, returning a normalized Contract.
func ValidateToolRequest(tools any, toolChoice any) (*Contract, error) {
	if tools == nil {
		return &Contract{ChoiceMode: "auto"}, nil
	}
	rawSlice, ok := tools.([]any)
	if !ok {
		if typed, ok2 := tools.([]ToolDef); ok2 {
			rawSlice = make([]any, len(typed))
			for i := range typed {
				rawSlice[i] = typed[i]
			}
		} else {
			return nil, invalidRequest("tools must be an array")
		}
	}

	defs := make([]ToolDef, 0, len(rawSlice))
	names := make(map[string]bool, len(rawSlice))

	for _, item := range rawSlice {
		def, err := extractToolDef(item)
		if err != nil {
			return nil, err
		}
		if !toolNameRe.MatchString(def.Name) {
			return nil, invalidRequest(fmt.Sprintf(`invalid tool name %q — use 1-64 chars of A-Z a-z 0-9 _ -`, def.Name))
		}
		if names[def.Name] {
			return nil, invalidRequest(fmt.Sprintf(`duplicate tool name %q`, def.Name))
		}
		names[def.Name] = true
		defs = append(defs, def)
	}

	mode := "auto"
	forced := ""
	switch tc := toolChoice.(type) {
	case nil:
		mode = "auto"
	case string:
		switch tc {
		case "auto", "":
			mode = "auto"
		case "none":
			mode = "none"
		case "required":
			mode = "required"
		default:
			return nil, invalidRequest(`tool_choice must be "auto", "none", "required", or a function choice`)
		}
	case map[string]any:
		tStr, _ := tc["type"].(string)
		fnMap, _ := tc["function"].(map[string]any)
		fnName, _ := fnMap["name"].(string)
		if tStr == "function" && fnName != "" {
			if !names[fnName] {
				return nil, invalidRequest(fmt.Sprintf(`tool_choice names unknown tool %q`, fnName))
			}
			mode = "function"
			forced = fnName
		} else {
			return nil, invalidRequest(`tool_choice must be "auto", "none", "required", or a function choice`)
		}
	default:
		return nil, invalidRequest(`tool_choice must be "auto", "none", "required", or a function choice`)
	}

	if len(defs) == 0 && mode != "auto" && mode != "none" {
		return nil, invalidRequest("tool_choice requires at least one tool")
	}

	return &Contract{
		Tools:          defs,
		ChoiceMode:     mode,
		ForcedFunction: forced,
	}, nil
}

func extractToolDef(raw any) (ToolDef, error) {
	if td, ok := raw.(ToolDef); ok {
		if td.Name == "" {
			return ToolDef{}, invalidRequest("each tool must declare function.name")
		}
		if td.Parameters == nil {
			td.Parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		return td, nil
	}
	m, ok := raw.(map[string]any)
	if !ok || m == nil {
		return ToolDef{}, invalidRequest("each tool must declare function.name")
	}
	fn := m
	if sub, ok := m["function"].(map[string]any); ok {
		fn = sub
	}
	name, _ := fn["name"].(string)
	if name == "" {
		return ToolDef{}, invalidRequest("each tool must declare function.name")
	}
	desc, _ := fn["description"].(string)
	var params map[string]any
	if pRaw, exists := fn["parameters"]; exists {
		if pRaw == nil {
			return ToolDef{}, invalidRequest(fmt.Sprintf(`parameters for tool %q must be a JSON schema object`, name))
		}
		pMap, ok := pRaw.(map[string]any)
		if !ok {
			return ToolDef{}, invalidRequest(fmt.Sprintf(`parameters for tool %q must be a JSON schema object`, name))
		}
		params = pMap
	} else if sRaw, exists := fn["input_schema"]; exists {
		if sMap, ok := sRaw.(map[string]any); ok {
			params = sMap
		} else {
			return ToolDef{}, invalidRequest(fmt.Sprintf(`parameters for tool %q must be a JSON schema object`, name))
		}
	} else {
		params = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return ToolDef{
		Name:        name,
		Description: desc,
		Parameters:  params,
	}, nil
}

var (
	instrCacheMu    sync.RWMutex
	instrCacheMap   = make(map[string]string, 32)
	instrCacheOrder = make([]string, 0, 32)

	compactMu          sync.RWMutex
	compactEnabled     = true
	compactDescMaxChar = 400
)

const toolCacheMax = 32

// SetCompactOptions configures whether BuildToolInstructions and BuildToolReminder
// compact tool schemas and descriptions before rendering into the prompt.
func SetCompactOptions(enabled bool, maxDescChars int) {
	if maxDescChars < 64 {
		maxDescChars = 400
	}
	compactMu.Lock()
	changed := compactEnabled != enabled || compactDescMaxChar != maxDescChars
	compactEnabled = enabled
	compactDescMaxChar = maxDescChars
	compactMu.Unlock()
	if changed {
		instrCacheMu.Lock()
		instrCacheMap = make(map[string]string, 32)
		instrCacheOrder = instrCacheOrder[:0]
		instrCacheMu.Unlock()
	}
}

func getCompactOptions() (bool, int) {
	compactMu.RLock()
	defer compactMu.RUnlock()
	return compactEnabled, compactDescMaxChar
}

// BuildToolInstructions renders the system-prompt tool calling instructions (cached by tool schema + choice).
func BuildToolInstructions(tools any, toolChoice any) (string, error) {
	contract, err := ValidateToolRequest(tools, toolChoice)
	if err != nil {
		return "", err
	}
	if len(contract.Tools) == 0 || contract.ChoiceMode == "none" {
		return "", nil
	}

	compactOn, maxDesc := getCompactOptions()
	defsBytes, _ := json.Marshal(contract.Tools)
	cacheKey := fmt.Sprintf("%s:%s:%v:%d\x00%s", contract.ChoiceMode, contract.ForcedFunction, compactOn, maxDesc, string(defsBytes))

	instrCacheMu.RLock()
	if cached, ok := instrCacheMap[cacheKey]; ok {
		instrCacheMu.RUnlock()
		return cached, nil
	}
	instrCacheMu.RUnlock()

	listParts := make([]string, 0, len(contract.Tools))
	for _, d := range contract.Tools {
		toRender := d
		if compactOn {
			toRender = ToolDef{
				Name:        d.Name,
				Description: TruncateUTF8(d.Description, maxDesc),
				Parameters:  CompactSchemaMap(d.Parameters, maxDesc),
			}
		}
		b, _ := json.Marshal(toRender)
		listParts = append(listParts, string(b))
	}
	list := strings.Join(listParts, "\n")
	lines := []string{
		"# Tool calling",
		"",
		"You can call the following tools:",
		list,
		"",
		"To call a tool, reply with ONLY this JSON object (wrapped in a ```json code block so HTML/JSX tags in arguments are never altered) and nothing else:",
		"```json",
		`{"tool_calls":[{"name":"<tool name>","arguments":{<arguments>}}]}`,
		"```",
		"",
		"Rules:",
		"- Respond directly and deterministically without conversational filler.",
		"- Use only the declared tool names and match the exact parameter names, types, and required fields from each tool's JSON Schema.",
		"- Execute multi-step workflows iteratively across turns rather than collapsing an entire multi-step workflow into one giant response: you may batch 2–4 lightweight read-only/inspection/query calls in one turn, but limit heavy state-mutating, execution, or large-payload calls (such as writing/editing files or running commands) to at most 1–2 per turn so the caller can execute and verify each step immediately.",
		"- Never pack 3+ large file or payload creations into a single turn, and never mark a task, checklist, or plan item as completed in the same turn before its actions have actually been executed and their tool results observed in a subsequent turn.",
		`- When calling multiple tools in one turn, put all of them inside a single "tool_calls": [...] array (never emit multiple separate JSON blocks).`,
		"- Always wrap the `{\"tool_calls\":[...]}` JSON in a ```json ... ``` code block and never write draft code blocks, file contents, or conversational prose outside the JSON block when calling tools.",
		"- Never emit `<think>`, `<thinking>`, or `<thought>` tags inside your visible answer or inside tool arguments.",
		"- Emit the `{\"tool_calls\":[...]}` JSON object whenever a declared tool is needed; use the declared JSON tools above rather than built-in web search or code execution.",
		`- ` + "`{\"tool_calls\":[...]}`" + ` blocks are plain-text JSON responses to the caller and remain available on every turn regardless of built-in search or tool counters.`,
		"- Never claim you lack access to local files, directories, external resources, or commands when tools are provided; always invoke the appropriate tools to inspect and verify before answering.",
		"- Only when the task is genuinely complete and no further tool calls are needed, answer the user concisely in plain text.",
	}
	if contract.ChoiceMode == "function" && contract.ForcedFunction != "" {
		lines = append(lines, fmt.Sprintf(`- You MUST call the tool %q.`, contract.ForcedFunction))
	} else if contract.ChoiceMode == "required" {
		lines = append(lines, "- You MUST call at least one tool.")
	}
	rendered := strings.Join(lines, "\n")

	instrCacheMu.Lock()
	if _, exists := instrCacheMap[cacheKey]; !exists {
		if len(instrCacheOrder) >= toolCacheMax {
			oldest := instrCacheOrder[0]
			instrCacheOrder = instrCacheOrder[1:]
			delete(instrCacheMap, oldest)
		}
		instrCacheOrder = append(instrCacheOrder, cacheKey)
	}
	instrCacheMap[cacheKey] = rendered
	instrCacheMu.Unlock()

	return rendered, nil
}

// BuildToolReminder renders the footer reminder placed right before Assistant:.
func BuildToolReminder(tools any, toolChoice any) (string, error) {
	return buildToolReminder(tools, toolChoice, false)
}

// BuildContinuationToolReminder renders the footer reminder for continuation
// turns (StartIndex > 0) where the full BuildToolInstructions block is omitted.
// It always includes required parameter names so smaller models do not forget
// mandatory schema properties on turn 2+.
func BuildContinuationToolReminder(tools any, toolChoice any) (string, error) {
	return buildToolReminder(tools, toolChoice, true)
}

func buildToolReminder(tools any, toolChoice any, forceRequiredArgs bool) (string, error) {
	contract, err := ValidateToolRequest(tools, toolChoice)
	if err != nil {
		return "", err
	}
	if len(contract.Tools) == 0 || contract.ChoiceMode == "none" {
		return "", nil
	}

	compactOn, _ := getCompactOptions()
	summaries := make([]string, 0, len(contract.Tools))
	for _, d := range contract.Tools {
		if compactOn && !forceRequiredArgs {
			summaries = append(summaries, d.Name)
			continue
		}
		reqList := extractStringSlice(d.Parameters["required"])
		if len(reqList) > 0 {
			summaries = append(summaries, fmt.Sprintf("%s(%s)", d.Name, strings.Join(reqList, ", ")))
		} else if compactOn {
			summaries = append(summaries, d.Name)
		} else {
			summaries = append(summaries, d.Name+"()")
		}
	}
	lines := []string{
		fmt.Sprintf("[Tool reminder: Available tools: %s.", strings.Join(summaries, ", ")),
		"JSON tool calls (`{\"tool_calls\":[{\"name\":\"<tool>\",\"arguments\":{...}}]}`) are plain-text JSON replies to the caller and remain available on every turn.",
		"Keep turns fast and iterative (at most 1–2 large or state-mutating tool calls per turn; never write draft payloads or prose outside the JSON block; use declared JSON tools rather than built-in tools).",
	}
	if contract.ChoiceMode == "function" && contract.ForcedFunction != "" {
		lines = append(lines, fmt.Sprintf("You MUST call the tool %q now by outputting ONLY the ```json {\"tool_calls\":[...]} ``` block.]", contract.ForcedFunction))
	} else if contract.ChoiceMode == "required" {
		lines = append(lines, "You MUST call at least one tool now by outputting ONLY the ```json {\"tool_calls\":[...]} ``` block.]")
	} else {
		lines = append(lines, "If further tool calls are needed to inspect, compute, or act, output ONLY the ```json {\"tool_calls\":[...]} ``` block now.]")
	}
	return strings.Join(lines, "\n"), nil
}
