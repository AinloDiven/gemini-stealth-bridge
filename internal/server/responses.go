package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/toolbridge"
	"github.com/azrinaldi/gemini-stealth-bridge/internal/transport"
)

// NormalizeResponsesInput converts an OpenAI Responses API body (`instructions`,
// `input`, `tools`, `tool_choice`) into canonical Chat Completions messages and tools.
func NormalizeResponsesInput(body map[string]any) (messages []map[string]any, tools any, toolChoice any, err error) {
	if body == nil {
		return nil, nil, nil, transport.NewUpstreamError("request body must be a JSON object", 400, "invalid_request_error", nil, nil)
	}

	if instr, ok := body["instructions"].(string); ok && strings.TrimSpace(instr) != "" {
		messages = append(messages, map[string]any{
			"role":    "system",
			"content": instr,
		})
	}

	// Support callers that send `messages` to /v1/responses as well as `input`.
	inputRaw, hasInput := body["input"]
	if !hasInput || inputRaw == nil {
		inputRaw = body["messages"]
	}

	if singleMap, ok := inputRaw.(map[string]any); ok && singleMap != nil {
		inputRaw = []any{singleMap}
	}

	switch inp := inputRaw.(type) {
	case string:
		if strings.TrimSpace(inp) != "" {
			messages = append(messages, map[string]any{
				"role":    "user",
				"content": inp,
			})
		}
	case []any:
		var (
			pendingAssistant map[string]any
			pendingToolCalls []any
			pendingReasoning []string
			callIDToName     = make(map[string]string)
		)
		recordToolCallNames := func(tcs []any) {
			for _, tcAny := range tcs {
				tcMap, ok := tcAny.(map[string]any)
				if !ok {
					continue
				}
				cid, _ := tcMap["id"].(string)
				fnMap, _ := tcMap["function"].(map[string]any)
				fnName, _ := fnMap["name"].(string)
				if cid != "" && fnName != "" {
					callIDToName[cid] = fnName
				}
			}
		}
		flushPendingAssistant := func() {
			if pendingAssistant == nil && len(pendingToolCalls) == 0 {
				return
			}
			var msg map[string]any
			if pendingAssistant != nil {
				msg = pendingAssistant
			} else {
				msg = map[string]any{"role": "assistant", "content": nil}
			}
			if len(pendingToolCalls) > 0 {
				tcCopy := make([]any, len(pendingToolCalls))
				copy(tcCopy, pendingToolCalls)
				msg["tool_calls"] = tcCopy
				recordToolCallNames(tcCopy)
			}
			if len(pendingReasoning) > 0 {
				existing, _ := msg["reasoning_content"].(string)
				if existing != "" {
					msg["reasoning_content"] = existing + "\n" + strings.Join(pendingReasoning, "\n")
				} else {
					msg["reasoning_content"] = strings.Join(pendingReasoning, "\n")
				}
			}
			messages = append(messages, msg)
			pendingAssistant = nil
			pendingToolCalls = nil
			pendingReasoning = nil
		}

		for i, itemAny := range inp {
			switch item := itemAny.(type) {
			case string:
				flushPendingAssistant()
				if strings.TrimSpace(item) != "" {
					messages = append(messages, map[string]any{
						"role":    "user",
						"content": item,
					})
				}
			case map[string]any:
				if item == nil {
					continue
				}
				itemType, _ := item["type"].(string)
				role, _ := item["role"].(string)

				switch itemType {
				case "function_call", "custom_tool_call":
					callID, _ := item["call_id"].(string)
					if callID == "" {
						callID, _ = item["id"].(string)
					}
					if callID == "" {
						callID = fmt.Sprintf("call_%d", i+1)
					}
					name, _ := item["name"].(string)
					if callID != "" && name != "" {
						callIDToName[callID] = name
					}
					argsStr := "{}"
					if itemType == "custom_tool_call" {
						inStr, _ := item["input"].(string)
						b, _ := json.Marshal(map[string]any{"input": inStr})
						argsStr = string(b)
					} else if a, ok := item["arguments"].(string); ok && a != "" {
						argsStr = a
					} else if aObj, ok := item["arguments"].(map[string]any); ok {
						b, _ := json.Marshal(aObj)
						argsStr = string(b)
					}
					pendingToolCalls = append(pendingToolCalls, map[string]any{
						"id":   callID,
						"type": "function",
						"function": map[string]any{
							"name":      name,
							"arguments": argsStr,
						},
					})
					continue

				case "function_call_output", "custom_tool_call_output":
					flushPendingAssistant()
					callID, _ := item["call_id"].(string)
					if callID == "" {
						callID, _ = item["id"].(string)
					}
					name, _ := item["name"].(string)
					if name == "" && callID != "" {
						name = callIDToName[callID]
					}
					var outStr string
					switch o := item["output"].(type) {
					case string:
						outStr = o
					case nil:
						outStr = ""
					default:
						b, _ := json.Marshal(o)
						outStr = string(b)
					}
					messages = append(messages, map[string]any{
						"role":         "tool",
						"tool_call_id": callID,
						"name":         name,
						"content":      outStr,
					})
					continue
				case "reasoning":
					if pendingAssistant != nil {
						flushPendingAssistant()
					}
					if sumArr, ok := item["summary"].([]any); ok {
						for _, sAny := range sumArr {
							if sMap, ok := sAny.(map[string]any); ok {
								if t, _ := sMap["text"].(string); strings.TrimSpace(t) != "" {
									pendingReasoning = append(pendingReasoning, strings.TrimSpace(t))
								}
							}
						}
					} else if t, _ := item["text"].(string); strings.TrimSpace(t) != "" {
						pendingReasoning = append(pendingReasoning, strings.TrimSpace(t))
					}
					continue

				case "input_text", "input_image", "image", "input_file", "file", "input_audio", "audio", "input_video", "video", "video_url", "document":
					flushPendingAssistant()
					messages = append(messages, map[string]any{
						"role":    "user",
						"content": []any{item},
					})
					continue
				}

				if role == "" {
					role = "user"
				}
				if role == "assistant" || role == "model" {
					flushPendingAssistant()
					msg := normalizeResponsesAssistantMessage(item, i)
					if len(pendingReasoning) > 0 {
						existing, _ := msg["reasoning_content"].(string)
						if existing != "" {
							msg["reasoning_content"] = strings.Join(pendingReasoning, "\n") + "\n" + existing
						} else {
							msg["reasoning_content"] = strings.Join(pendingReasoning, "\n")
						}
						pendingReasoning = nil
					}
					if tcs, ok := msg["tool_calls"].([]any); ok {
						pendingToolCalls = append(pendingToolCalls[:0], tcs...)
						delete(msg, "tool_calls")
						recordToolCallNames(tcs)
					}
					pendingAssistant = msg
				} else {
					flushPendingAssistant()
					messages = append(messages, map[string]any{
						"role":    role,
						"content": normalizeResponsesContentParts(item["content"]),
					})
				}
			}
		}
		flushPendingAssistant()
	}

	if len(messages) == 0 {
		return nil, nil, nil, transport.NewUpstreamError("input must contain at least one message or non-empty string", 400, "invalid_request_error", nil, nil)
	}

	tools = normalizeResponsesTools(body["tools"])
	toolChoice = normalizeResponsesToolChoice(body["tool_choice"])
	return messages, tools, toolChoice, nil
}

func normalizeResponsesAssistantMessage(item map[string]any, idx int) map[string]any {
	var textParts []string
	var reasoningParts []string
	var toolCalls []any

	if tcs, ok := item["tool_calls"].([]any); ok {
		toolCalls = append(toolCalls, tcs...)
	}

	switch cp := item["content"].(type) {
	case string:
		if cp != "" {
			textParts = append(textParts, cp)
		}
	case []any:
		for cIdx, cAny := range cp {
			cMap, ok := cAny.(map[string]any)
			if !ok || cMap == nil {
				continue
			}
			cType, _ := cMap["type"].(string)
			switch cType {
			case "output_text", "text", "input_text":
				if t, _ := cMap["text"].(string); t != "" {
					textParts = append(textParts, t)
				}
			case "reasoning", "summary_text", "thinking":
				if t, _ := cMap["text"].(string); t != "" {
					reasoningParts = append(reasoningParts, t)
				} else if t, _ := cMap["thinking"].(string); t != "" {
					reasoningParts = append(reasoningParts, t)
				}
			case "function_call", "tool_use":
				callID, _ := cMap["call_id"].(string)
				if callID == "" {
					callID, _ = cMap["id"].(string)
				}
				if callID == "" {
					callID = fmt.Sprintf("call_%d_%d", idx+1, cIdx+1)
				}
				name, _ := cMap["name"].(string)
				argsStr := "{}"
				if a, ok := cMap["arguments"].(string); ok && a != "" {
					argsStr = a
				} else if aObj, ok := cMap["arguments"].(map[string]any); ok {
					b, _ := json.Marshal(aObj)
					argsStr = string(b)
				} else if inObj, ok := cMap["input"].(map[string]any); ok {
					b, _ := json.Marshal(inObj)
					argsStr = string(b)
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   callID,
					"type": "function",
					"function": map[string]any{
						"name":      name,
						"arguments": argsStr,
					},
				})
			}
		}
	}

	var content any
	if len(textParts) > 0 {
		content = strings.Join(textParts, "\n")
	}
	msg := map[string]any{
		"role":    "assistant",
		"content": content,
	}
	if len(reasoningParts) > 0 {
		msg["reasoning_content"] = strings.Join(reasoningParts, "\n")
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	return msg
}

func normalizeResponsesContentParts(raw any) any {
	arr, ok := raw.([]any)
	if !ok {
		return raw
	}
	out := make([]any, 0, len(arr))
	for _, pAny := range arr {
		pMap, ok := pAny.(map[string]any)
		if !ok || pMap == nil {
			out = append(out, pAny)
			continue
		}
		pType, _ := pMap["type"].(string)
		if pType == "output_text" {
			t, _ := pMap["text"].(string)
			out = append(out, map[string]any{"type": "text", "text": t})
			continue
		}
		out = append(out, pMap)
	}
	return out
}

func normalizeResponsesTools(raw any) any {
	out, _, _ := normalizeResponsesToolsWithMeta(raw)
	return out
}

func normalizeResponsesToolsWithMeta(raw any) (normalized any, customTools map[string]bool, namespaceOf map[string]string) {
	arr, ok := raw.([]any)
	if !ok {
		return raw, nil, nil
	}
	customTools = make(map[string]bool)
	namespaceOf = make(map[string]string)
	out := make([]any, 0, len(arr))
	var visit func(items []any, ns string)
	visit = func(items []any, ns string) {
		for _, tAny := range items {
			tMap, ok := tAny.(map[string]any)
			if !ok || tMap == nil {
				out = append(out, tAny)
				continue
			}
			tType, _ := tMap["type"].(string)
			if tType == "namespace" {
				nsName, _ := tMap["name"].(string)
				if subTools, ok := tMap["tools"].([]any); ok {
					visit(subTools, nsName)
				}
				continue
			}
			if fnSub, hasFn := tMap["function"].(map[string]any); hasFn && fnSub != nil {
				if fnName, _ := fnSub["name"].(string); fnName != "" && ns != "" {
					namespaceOf[fnName] = ns
				}
				if tType == "custom" {
					if fnName, _ := fnSub["name"].(string); fnName != "" {
						customTools[fnName] = true
					}
				}
				out = append(out, tMap)
				continue
			}
			name, _ := tMap["name"].(string)
			if name == "" && tType == "local_shell" {
				name = "local_shell"
			}
			if name != "" {
				if ns != "" {
					namespaceOf[name] = ns
				}
				fnObj := map[string]any{
					"name": name,
				}
				if desc, ok := tMap["description"].(string); ok {
					fnObj["description"] = desc
				} else if tType == "local_shell" {
					fnObj["description"] = "Execute a shell command in the local workspace."
				}
				if params, exists := tMap["parameters"]; exists {
					fnObj["parameters"] = params
				} else if schema, exists := tMap["input_schema"]; exists {
					fnObj["input_schema"] = schema
				} else if tType == "custom" {
					customTools[name] = true
					fnObj["parameters"] = map[string]any{
						"type": "object",
						"properties": map[string]any{
							"input": map[string]any{
								"type":        "string",
								"description": "Raw input string for " + name + ".",
							},
						},
						"required": []any{"input"},
					}
				} else if tType == "local_shell" {
					fnObj["parameters"] = map[string]any{
						"type": "object",
						"properties": map[string]any{
							"command": map[string]any{
								"type":        "array",
								"items":       map[string]any{"type": "string"},
								"description": "Command and arguments to execute.",
							},
						},
						"required": []any{"command"},
					}
				}
				out = append(out, map[string]any{
					"type":     "function",
					"function": fnObj,
				})
				continue
			}
			out = append(out, tMap)
		}
	}
	visit(arr, "")
	return out, customTools, namespaceOf
}

func extractCustomToolInput(argsJSON string) string {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &parsed); err == nil && parsed != nil {
		if s, ok := parsed["input"].(string); ok {
			return s
		}
		for _, v := range parsed {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return argsJSON
}

func normalizeResponsesToolChoice(raw any) any {
	tcMap, ok := raw.(map[string]any)
	if !ok || tcMap == nil {
		return raw
	}
	if _, hasFn := tcMap["function"].(map[string]any); hasFn {
		return tcMap
	}
	tStr, _ := tcMap["type"].(string)
	name, _ := tcMap["name"].(string)
	if tStr == "function" && name != "" {
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": name,
			},
		}
	}
	return raw
}

func buildResponsesUsage(chatUsage map[string]any) map[string]any {
	inTok, _ := chatUsage["prompt_tokens"].(int)
	outTok, _ := chatUsage["completion_tokens"].(int)
	totTok, _ := chatUsage["total_tokens"].(int)
	cachedTok := 0
	if pDetails, ok := chatUsage["prompt_tokens_details"].(map[string]any); ok {
		cachedTok, _ = pDetails["cached_tokens"].(int)
	}
	res := map[string]any{
		"input_tokens":  inTok,
		"output_tokens": outTok,
		"total_tokens":  totTok,
		"input_tokens_details": map[string]any{
			"cached_tokens": cachedTok,
		},
	}
	if details, ok := chatUsage["completion_tokens_details"].(map[string]any); ok {
		if rTok, ok := details["reasoning_tokens"].(int); ok && rTok > 0 {
			res["output_tokens_details"] = map[string]any{
				"reasoning_tokens": rTok,
			}
		}
	}
	return res
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// HandleResponses executes an OpenAI Responses API (`POST /v1/responses`) request
// with real-time SSE streaming, reasoning summary events, tool call events, session
// continuation caching, and 1M-token context offloading.
func (s *Server) HandleResponses(ctx context.Context, w http.ResponseWriter, body map[string]any, scope AuthScope) {
	if body == nil {
		sendChatError(w, false, transport.NewUpstreamError("request body must be a JSON object", 400, "invalid_request_error", nil, nil))
		return
	}
	if mRaw, exists := body["model"]; exists && mRaw != nil {
		if _, ok := mRaw.(string); !ok {
			sendChatError(w, false, transport.NewUpstreamError("model must be a string", 400, "invalid_request_error", nil, nil))
			return
		}
	}
	_, customTools, namespaceOf := normalizeResponsesToolsWithMeta(body["tools"])
	messages, toolsRaw, toolChoiceRaw, normErr := NormalizeResponsesInput(body)
	if normErr != nil {
		sendChatError(w, false, normErr)
		return
	}
	reqStartedAt := time.Now()
	prep, prepErr := s.prepareGeneration(ctx, body, scope, messages, toolsRaw, toolChoiceRaw)
	prepareMs := time.Since(reqStartedAt).Milliseconds()
	if prepErr != nil {
		sendChatError(w, false, prepErr)
		return
	}

	toolsRaw = prep.toolsRaw
	respID := "resp_" + randomHex(8)
	rsID := "rs_" + randomHex(6)
	msgID := "msg_" + randomHex(6)
	created := time.Now().Unix()

	g := newGenerationSession(s, ctx, w, prep, messages, toolsRaw, toolChoiceRaw, respID, "/v1/responses", reqStartedAt, prepareMs)
	defer g.close()

	var (
		seqNum         int
		reasoningAdded bool
		messageAdded   bool
	)
	emitSSELocked := func(eventType string, payload map[string]any) {
		if g.ctx.Err() != nil || g.handlerDone.Load() {
			return
		}
		seqNum++
		ev := make(map[string]any, len(payload)+2)
		for k, v := range payload {
			ev[k] = v
		}
		ev["type"] = eventType
		ev["sequence_number"] = seqNum
		b, _ := json.Marshal(ev)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, string(b))
		if g.flusher != nil {
			g.flusher.Flush()
		}
	}
	emitSSE := func(eventType string, payload map[string]any) {
		if g.ctx.Err() != nil {
			return
		}
		g.streamMu.Lock()
		emitSSELocked(eventType, payload)
		g.streamMu.Unlock()
	}

	g.onStreamCommittedLocked = func() {
		baseResp := map[string]any{
			"id":         respID,
			"object":     "response",
			"created_at": created,
			"model":      g.mid,
			"status":     "in_progress",
			"output":     []any{},
			"usage":      nil,
		}
		emitSSELocked("response.created", map[string]any{"response": baseResp})
		emitSSELocked("response.in_progress", map[string]any{"response": baseResp})
	}
	g.emitActionLocked = func(payload map[string]any) {
		emitSSELocked("response.x_gemini_action_status", map[string]any{
			"x_gemini_protocol_version": 3,
			"action_status":             payload,
		})
	}
	g.emitCitationsLocked = func(items []gemini.GroundingSource) {
		emitSSELocked("response.x_gemini_citations", map[string]any{
			"x_gemini_protocol_version": 3,
			"citations":                 items,
		})
	}

	g.startEarlyOrGrace()

	emitReasoningDelta := func(delta string) {
		if delta == "" {
			return
		}
		g.markMeaningfulOutput(&g.firstReasoningMs)
		g.startStream()
		if !reasoningAdded {
			reasoningAdded = true
			emitSSE("response.output_item.added", map[string]any{
				"output_index": 0,
				"item": map[string]any{
					"id":      rsID,
					"type":    "reasoning",
					"summary": []any{},
				},
			})
			emitSSE("response.reasoning_summary_part.added", map[string]any{
				"item_id":       rsID,
				"output_index":  0,
				"summary_index": 0,
				"part": map[string]any{
					"type": "summary_text",
					"text": "",
				},
			})
		}
		emitSSE("response.reasoning_summary_text.delta", map[string]any{
			"item_id":       rsID,
			"output_index":  0,
			"summary_index": 0,
			"delta":         delta,
		})
	}

	emitTextDelta := func(delta string) {
		if delta == "" {
			return
		}
		g.markMeaningfulOutput(&g.firstContentMs)
		g.startStream()
		outIdx := 0
		if reasoningAdded {
			outIdx = 1
		}
		if !messageAdded {
			messageAdded = true
			emitSSE("response.output_item.added", map[string]any{
				"output_index": outIdx,
				"item": map[string]any{
					"id":      msgID,
					"type":    "message",
					"role":    "assistant",
					"status":  "in_progress",
					"content": []any{},
				},
			})
			emitSSE("response.content_part.added", map[string]any{
				"item_id":       msgID,
				"output_index":  outIdx,
				"content_index": 0,
				"part": map[string]any{
					"type":        "output_text",
					"text":        "",
					"annotations": []any{},
				},
			})
		}
		emitSSE("response.output_text.delta", map[string]any{
			"item_id":       msgID,
			"output_index":  outIdx,
			"content_index": 0,
			"delta":         delta,
		})
	}

	for attempt := 0; attempt < g.maxAttempts; attempt++ {
		g.curAttempt = attempt
		as := &attemptState{startedAt: time.Now()}
		c := s.clientFor(g.account)
		upstream, streamErr := g.runUpstreamAttempt(c, scope, as, nil, nil, func(frameDone bool) error {
			if g.stream && !g.buffered && ctx.Err() == nil {
				tail, textOK := remainingStreamTail(as.text, as.sentText)
				if !textOK || (s.Cfg.LiveThoughts && !strings.HasPrefix(as.thoughts, as.sentThought)) {
					return transport.NewUpstreamError("upstream revised an already streamed answer", 502, "upstream_revision", nil, nil)
				}
				if s.Cfg.LiveThoughts && !g.priorAttemptSentThought && len(as.thoughts) > len(as.sentThought) {
					emitReasoningDelta(as.thoughts[len(as.sentThought):])
					as.sentThought = as.thoughts
				}
				if tail != "" {
					g.startStream()
					g.emitCitations(as.grounding)
					emitTextDelta(tail)
					as.sentText = as.text
				} else if as.text != as.sentText {
					as.sentText = as.text
				}
			} else if g.stream && g.buffered && ctx.Err() == nil && !frameDone {
				if as.sentText != "" && !hasCompatibleStreamPrefix(as.text, as.sentText) {
					return transport.NewUpstreamError("upstream revised an already streamed answer", 502, "upstream_revision", nil, nil)
				}
				identityVerified := canStreamVerified(g.eagerStream, s.Cfg.RejectDowngrade, as.identityConflict, *g.model, as.servedID, as.servedLabel)
				if identityVerified {
					g.startStream()
					g.emitCitations(as.grounding)
				}
				if identityVerified && s.Cfg.LiveThoughts {
					if g.hasTools && !s.Cfg.KeepToolPreamble && as.sentText == "" {
						if pre := toolbridge.ExtractPreToolPreamble(as.text); pre != "" {
							as.thoughts = appendPreambleToThoughts(as.thoughts, pre)
						}
					}
					if !g.priorAttemptSentThought && len(as.thoughts) > len(as.sentThought) && strings.HasPrefix(as.thoughts, as.sentThought) {
						emitReasoningDelta(as.thoughts[len(as.sentThought):])
						as.sentThought = as.thoughts
					} else if as.servedID != "" && time.Since(as.startedAt) >= 5*time.Second {
						g.startStream()
					}
				}
				if identityVerified {
					if !g.hasTools && !as.mediaToolUsed && len(as.text) > 240 && gemini.GeminiErrorReply(as.text) == "" {
						if tail, ok := remainingStreamTail(as.text, as.sentText); ok && tail != "" {
							emitTextDelta(tail)
							as.sentText = as.text
						}
					} else if g.hasTools && !as.mediaToolUsed && len(as.text) > 240 {
						if safePrefix := toolbridge.SafeStreamablePrefix(as.text); len(safePrefix) > len(as.sentText) && strings.HasPrefix(safePrefix, as.sentText) && safeErrorReplyPrefix(as.text) == "" && !toolbridge.IsToolProtocolRefusal(as.text) {
							emitTextDelta(safePrefix[len(as.sentText):])
							as.sentText = safePrefix
						}
					}
				}
			}
			return nil
		})

		interpreted, runCheck := s.evaluateAttemptCompletion(
			ctx,
			g.account,
			*g.model,
			g.mid,
			upstream,
			streamErr,
			as.selected,
			as.done,
			as.text,
			as.sentText,
			len(as.lastImages) > 0 || len(as.lastVideos) > 0,
			as.servedID,
			as.servedLabel,
			as.identityConflict,
			g.hasTools,
			g.toolsRaw,
			g.toolChoiceRaw,
		)

		if runCheck != nil {
			payloadCommitted := as.sentText != "" || messageAdded || as.mediaToolUsed
			if g.handleAttemptFailure(c, scope, as, runCheck, payloadCommitted) {
				continue
			}
			if ctx.Err() == nil {
				sendChatError(w, g.headersCommitted(), runCheck)
			}
			return
		}
		// Success
		calls, retryMedia := g.resolveMediaContinuation(scope, interpreted, as)
		if retryMedia {
			attempt--
			continue
		}

		var contentStr string
		if len(calls) > 0 {
			if (s.Cfg.KeepToolPreamble || as.sentText != "") && interpreted.Text != "" {
				contentStr = interpreted.Text
			} else if s.Cfg.LiveThoughts && interpreted != nil && strings.TrimSpace(interpreted.Text) != "" {
				as.thoughts = appendPreambleToThoughts(as.thoughts, interpreted.Text)
			}
		} else {
			contentStr = as.text
			if as.sentText == "" && as.thoughts != "" {
				contentStr = stripLeakedThoughtsFromContent(contentStr, as.thoughts)
			}
		}
		// Web-search citations are appended to the answer so the client sees the
		// sources the model actually used, in the order Google returned them.
		if s.Cfg.IncludeGrounding && len(as.grounding) > 0 && len(calls) == 0 {
			contentStr = appendGroundingCitations(contentStr, as.grounding)
		}

		oaCalls := toolbridge.ToOpenAIToolCalls(calls)

		chatUsage := s.computeChatUsage(g.fullPrompt, g.curPrompt, g.overflowParts, messages, g.toolsRaw, g.toolChoiceRaw, as.text, as.thoughts)
		respUsage := buildResponsesUsage(chatUsage)
		s.clearModelExhausted(g.account, g.model.ModelName)
		if !g.opts.Temporary {
			var cachedContent any = contentStr
			if len(oaCalls) > 0 && contentStr == "" {
				cachedContent = nil
			}
			s.saveSessionContinuation(g.account.ID, g.mid, messages, g.toolsRaw, g.toolChoiceRaw, cachedContent, oaCalls, as.lastCID, as.lastRID, as.candidateID, as.lastContextToken, g.prep.sessionBindingKey)
		}

		g.stopKeepalive()
		if ctx.Err() != nil {
			return
		}

		var outputItems []map[string]any
		emitReasoningItem := s.Cfg.LiveThoughts && ((as.thoughts != "" && !g.priorAttemptSentThought) || reasoningAdded)
		if emitReasoningItem {
			outputItems = append(outputItems, map[string]any{
				"id":   rsID,
				"type": "reasoning",
				"summary": []map[string]any{
					{
						"type": "summary_text",
						"text": as.thoughts,
					},
				},
			})
		}
		if contentStr != "" || len(oaCalls) == 0 {
			outputItems = append(outputItems, map[string]any{
				"id":     msgID,
				"type":   "message",
				"role":   "assistant",
				"status": "completed",
				"content": []map[string]any{
					{
						"type":        "output_text",
						"text":        contentStr,
						"annotations": []any{},
					},
				},
			})
		}
		fcIDs := make([]string, len(oaCalls))
		for idx, tc := range oaCalls {
			fcID := fmt.Sprintf("fc_%s_%d", randomHex(4), idx+1)
			fcIDs[idx] = fcID
			if customTools[tc.Function.Name] {
				item := map[string]any{
					"id":      fcID,
					"type":    "custom_tool_call",
					"call_id": tc.ID,
					"name":    tc.Function.Name,
					"input":   extractCustomToolInput(tc.Function.Arguments),
					"status":  "completed",
				}
				if ns := namespaceOf[tc.Function.Name]; ns != "" {
					item["namespace"] = ns
				}
				outputItems = append(outputItems, item)
				continue
			}
			item := map[string]any{
				"id":        fcID,
				"type":      "function_call",
				"call_id":   tc.ID,
				"name":      tc.Function.Name,
				"arguments": tc.Function.Arguments,
				"status":    "completed",
			}
			if ns := namespaceOf[tc.Function.Name]; ns != "" {
				item["namespace"] = ns
			}
			outputItems = append(outputItems, item)
		}

		if g.stream {
			g.startStream()
			if emitReasoningItem {
				if !g.priorAttemptSentThought && len(as.thoughts) > len(as.sentThought) {
					emitReasoningDelta(as.thoughts[len(as.sentThought):])
				}
				emitSSE("response.reasoning_summary_text.done", map[string]any{
					"item_id":       rsID,
					"output_index":  0,
					"summary_index": 0,
					"text":          as.thoughts,
				})
				emitSSE("response.output_item.done", map[string]any{
					"output_index": 0,
					"item":         outputItems[0],
				})
			}

			nextOutIdx := 0
			if emitReasoningItem {
				nextOutIdx = 1
			}
			if len(oaCalls) == 0 {
				g.emitCitations(as.grounding)
			}
			if contentStr != "" || len(oaCalls) == 0 {
				if tail, ok := remainingStreamTail(contentStr, as.sentText); ok && tail != "" {
					emitTextDelta(tail)
				} else if !messageAdded {
					emitTextDelta("")
				}
				msgItem := map[string]any{
					"id":     msgID,
					"type":   "message",
					"role":   "assistant",
					"status": "completed",
					"content": []map[string]any{
						{
							"type":        "output_text",
							"text":        contentStr,
							"annotations": []any{},
						},
					},
				}
				emitSSE("response.output_text.done", map[string]any{
					"item_id":       msgID,
					"output_index":  nextOutIdx,
					"content_index": 0,
					"text":          contentStr,
				})
				emitSSE("response.content_part.done", map[string]any{
					"item_id":       msgID,
					"output_index":  nextOutIdx,
					"content_index": 0,
					"part": map[string]any{
						"type":        "output_text",
						"text":        contentStr,
						"annotations": []any{},
					},
				})
				emitSSE("response.output_item.done", map[string]any{
					"output_index": nextOutIdx,
					"item":         msgItem,
				})
				nextOutIdx++
			}

			for idx, tc := range oaCalls {
				fcID := fcIDs[idx]
				fcOutIdx := nextOutIdx + idx
				ns := namespaceOf[tc.Function.Name]
				if customTools[tc.Function.Name] {
					rawIn := extractCustomToolInput(tc.Function.Arguments)
					doneItem := map[string]any{
						"id":      fcID,
						"type":    "custom_tool_call",
						"call_id": tc.ID,
						"name":    tc.Function.Name,
						"input":   rawIn,
						"status":  "completed",
					}
					addedItem := map[string]any{
						"id":      fcID,
						"type":    "custom_tool_call",
						"call_id": tc.ID,
						"name":    tc.Function.Name,
						"input":   "",
						"status":  "in_progress",
					}
					if ns != "" {
						doneItem["namespace"] = ns
						addedItem["namespace"] = ns
					}
					emitSSE("response.output_item.added", map[string]any{
						"output_index": fcOutIdx,
						"item":         addedItem,
					})
					g.markMeaningfulOutput(&g.firstToolMs)
					emitSSE("response.custom_tool_call_input.delta", map[string]any{
						"item_id":      fcID,
						"output_index": fcOutIdx,
						"delta":        rawIn,
					})
					emitSSE("response.custom_tool_call_input.done", map[string]any{
						"item_id":      fcID,
						"output_index": fcOutIdx,
						"input":        rawIn,
					})
					emitSSE("response.output_item.done", map[string]any{
						"output_index": fcOutIdx,
						"item":         doneItem,
					})
					continue
				}
				doneItem := map[string]any{
					"id":        fcID,
					"type":      "function_call",
					"call_id":   tc.ID,
					"name":      tc.Function.Name,
					"arguments": tc.Function.Arguments,
					"status":    "completed",
				}
				addedItem := map[string]any{
					"id":        fcID,
					"type":      "function_call",
					"call_id":   tc.ID,
					"name":      tc.Function.Name,
					"arguments": "",
					"status":    "in_progress",
				}
				if ns != "" {
					doneItem["namespace"] = ns
					addedItem["namespace"] = ns
				}
				emitSSE("response.output_item.added", map[string]any{
					"output_index": fcOutIdx,
					"item":         addedItem,
				})
				// The argument delta is the first client-visible function-call
				// output, including calls whose argument object is empty.
				g.markMeaningfulOutput(&g.firstToolMs)
				emitSSE("response.function_call_arguments.delta", map[string]any{
					"item_id":      fcID,
					"output_index": fcOutIdx,
					"delta":        tc.Function.Arguments,
				})
				emitSSE("response.function_call_arguments.done", map[string]any{
					"item_id":      fcID,
					"output_index": fcOutIdx,
					"arguments":    tc.Function.Arguments,
				})
				emitSSE("response.output_item.done", map[string]any{
					"output_index": fcOutIdx,
					"item":         doneItem,
				})
			}

			var mediaPayload []map[string]any
			if len(oaCalls) == 0 {
				mediaPayload = buildMediaExtensionPayload(as.lastImages, as.lastVideos)
			}
			if len(mediaPayload) > 0 {
				emitSSE("response.x_gemini_media", map[string]any{
					"x_gemini_protocol_version": 3,
					"media":                     mediaPayload,
				})
			}

			emitSSE("response.completed", map[string]any{
				"response": map[string]any{
					"id":         respID,
					"object":     "response",
					"created_at": created,
					"status":     "completed",
					"model":      g.mid,
					"output":     outputItems,
					"usage":      respUsage,
				},
			})
		} else {
			payload := map[string]any{
				"id":         respID,
				"object":     "response",
				"created_at": created,
				"status":     "completed",
				"model":      g.mid,
				"output":     outputItems,
				"usage":      respUsage,
			}
			if len(oaCalls) == 0 {
				if mediaPayload := buildMediaExtensionPayload(as.lastImages, as.lastVideos); len(mediaPayload) > 0 {
					payload["x_gemini_media"] = mediaPayload
				}
				if citPayload := gemini.GroundingCitationsWithIDs(as.grounding); len(citPayload) > 0 {
					payload["x_gemini_citations"] = citPayload
				}
			}
			setGSBHeaders(w, g.headersCommitted(), g.account.ID, g.mid, g.curAttempt, g.lastUpstreamStatus, g.lastRetryReason)
			g.markPhase(&g.headerMs)
			sendJSON(w, 200, payload, nil)
		}

		g.finishSuccess(c, chatUsage, as.servedID, as.servedLabel)
		return
	}
}
