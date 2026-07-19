// Package openai provides utilities to translate OpenAI Chat Completions
// request JSON into OpenAI Responses API request JSON.
// It supports tools, multimodal text/image inputs, and Structured Outputs.
// The package handles the conversion of OpenAI API requests into the format
// expected by the OpenAI Responses API, including proper mapping of messages,
// tools, and generation parameters.
package chat_completions

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Input structures (minimal – only fields actually used)

type chatReqInput struct {
	ReasoningEffort string          `json:"reasoning_effort"`
	Messages        []chatMessage   `json:"messages"`
	Tools           []chatTool      `json:"tools"`
	ToolChoice      json.RawMessage `json:"tool_choice"`
	ResponseFormat  *chatRespFormat `json:"response_format"`
	Text            *chatTextCfg    `json:"text"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []chatToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

type chatTool struct {
	Type     string        `json:"type"`
	Function *chatToolFunc `json:"function"`
	Raw      json.RawMessage
}

// UnmarshalJSON for chatTool stores the raw bytes for passthrough of
// non-function (built-in) tools.
func (t *chatTool) UnmarshalJSON(data []byte) error {
	type alias chatTool
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*t = chatTool(a)
	t.Raw = data
	return nil
}

type chatToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict"`
}

type chatToolCall struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
	Custom struct {
		Name  string `json:"name"`
		Input string `json:"input"`
	} `json:"custom"`
}

type chatRespFormat struct {
	Type       string          `json:"type"`
	JSONSchema *chatJSONSchema `json:"json_schema"`
}

type chatJSONSchema struct {
	Name   string          `json:"name"`
	Strict *bool           `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

type chatTextCfg struct {
	Verbosity json.RawMessage `json:"verbosity"`
}

type chatContentPart struct {
	Type     string               `json:"type"`
	Text     string               `json:"text"`
	ImageURL *chatContentImageURL `json:"image_url"`
	File     *chatContentFile     `json:"file"`
	Audio    *chatContentAudio    `json:"input_audio"`
}

type chatContentImageURL struct {
	URL    string `json:"url"`
	FileID string `json:"file_id"`
	Detail string `json:"detail"`
}

type chatContentFile struct {
	FileData string `json:"file_data"`
	FileID   string `json:"file_id"`
	FileURL  string `json:"file_url"`
	Filename string `json:"filename"`
}

type chatContentAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

// ConvertOpenAIRequestToCodex

// ConvertOpenAIRequestToCodex converts an OpenAI Chat Completions request JSON
// into an OpenAI Responses API request JSON. The transformation follows the
// examples defined in docs/2.md exactly, including tools, multi-turn dialog,
// multimodal text/image handling, and Structured Outputs mapping.
func ConvertOpenAIRequestToCodex(modelName string, inputRawJSON []byte, stream bool) []byte {
	var req chatReqInput
	_ = json.Unmarshal(inputRawJSON, &req)

	// Build tool-name shortening map from all function tools in the request.
	var funcNames []string
	for _, t := range req.Tools {
		if t.Type == "function" && t.Function != nil {
			funcNames = append(funcNames, t.Function.Name)
		}
	}
	originalToolNameMap := buildShortNameMap(funcNames)

	// Build output map
	out := map[string]any{
		"instructions":        "",
		"stream":              stream,
		"parallel_tool_calls": true,
		"include":             []string{"reasoning.encrypted_content"},
		"model":               modelName,
		"store":               false,
	}

	effort := req.ReasoningEffort
	if effort == "" {
		effort = "medium"
	}
	out["reasoning"] = map[string]any{
		"effort":  effort,
		"summary": "auto",
	}

	// Build input array
	input := make([]any, 0, len(req.Messages))
	type pendingToolCall struct {
		callID       string
		sourceCallID string
		callType     string
		consumed     bool
	}
	var pendingToolCalls []pendingToolCall
	ambiguousToolCallIDs := map[string]struct{}{}

	for i, m := range req.Messages {
		role := m.Role

		switch role {
		case "tool":
			toolCallID := m.ToolCallID
			if _, ambiguous := ambiguousToolCallIDs[toolCallID]; toolCallID != "" && ambiguous {
				continue
			}

			pendingIndex := -1
			for idx := range pendingToolCalls {
				pc := &pendingToolCalls[idx]
				if pc.consumed {
					continue
				}
				if toolCallID == "" || pc.sourceCallID == toolCallID || pc.callID == toolCallID {
					pendingIndex = idx
					break
				}
			}
			if pendingIndex < 0 {
				continue
			}
			pc := &pendingToolCalls[pendingIndex]
			pc.consumed = true
			toolCallID = pc.callID
			outputType := "function_call_output"
			if pc.callType == "custom" {
				outputType = "custom_tool_call_output"
			}

			item := map[string]any{"type": outputType, "call_id": toolCallID}
			setToolCallOutput(item, m.Content)
			input = append(input, item)

		default:
			pendingToolCalls = nil
			ambiguousToolCallIDs = map[string]struct{}{}

			displayRole := role
			if role == "system" {
				displayRole = "developer"
			}

			contentParts := buildContentParts(role, m.Content)
			msg := map[string]any{
				"type":    "message",
				"role":    displayRole,
				"content": contentParts,
			}

			// Don't emit empty assistant messages when only tool_calls present.
			if role != "assistant" || len(contentParts) > 0 {
				input = append(input, msg)
			}

			// Handle tool calls for assistant messages as separate top-level objects.
			if role == "assistant" && len(m.ToolCalls) > 0 {
				callIDCounts := map[string]int{}
				usedCallIDs := map[string]struct{}{}
				for _, tc := range m.ToolCalls {
					tcType := tc.Type
					callID := tc.ID
					if (tcType == "function" || tcType == "custom") && callID != "" {
						callIDCounts[callID]++
						usedCallIDs[callID] = struct{}{}
					}
				}
				for callID, count := range callIDCounts {
					if count > 1 {
						ambiguousToolCallIDs[callID] = struct{}{}
					}
				}

				for j, tc := range m.ToolCalls {
					tcType := tc.Type
					if tcType != "function" && tcType != "custom" {
						continue
					}
					sourceCallID := tc.ID
					if _, ambiguous := ambiguousToolCallIDs[sourceCallID]; sourceCallID != "" && ambiguous {
						continue
					}
					callID := sourceCallID
					if callID == "" {
						baseCallID := "call_missing_" + strconv.Itoa(i) + "_" + strconv.Itoa(j)
						callID = baseCallID
						for suffix := 1; ; suffix++ {
							if _, used := usedCallIDs[callID]; !used {
								break
							}
							callID = baseCallID + "_" + strconv.Itoa(suffix)
						}
						usedCallIDs[callID] = struct{}{}
					}
					pendingToolCalls = append(pendingToolCalls, pendingToolCall{
						callID:       callID,
						sourceCallID: sourceCallID,
						callType:     tcType,
					})

					switch tcType {
					case "function":
						name := tc.Function.Name
						if short, ok := originalToolNameMap[name]; ok {
							name = short
						} else {
							name = shortenNameIfNeeded(name)
						}
						input = append(input, map[string]any{
							"type":      "function_call",
							"call_id":   callID,
							"name":      name,
							"arguments": tc.Function.Arguments,
						})
					case "custom":
						name := tc.Custom.Name
						if short, ok := originalToolNameMap[name]; ok {
							name = short
						} else {
							name = shortenNameIfNeeded(name)
						}
						input = append(input, map[string]any{
							"type":    "custom_tool_call",
							"call_id": callID,
							"name":    name,
							"input":   tc.Custom.Input,
						})
					}
				}
			}
		}
	}
	out["input"] = input

	// response_format / text
	textObj := buildTextObject(req.ResponseFormat, req.Text)
	if textObj != nil {
		out["text"] = textObj
	}

	// tools
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			toolType := t.Type
			if toolType != "" && toolType != "function" && len(t.Raw) > 0 {
				var v any
				_ = json.Unmarshal(t.Raw, &v)
				tools = append(tools, v)
				continue
			}
			if toolType == "function" && t.Function != nil {
				item := map[string]any{"type": "function"}
				name := t.Function.Name
				if short, ok := originalToolNameMap[name]; ok {
					name = short
				} else {
					name = shortenNameIfNeeded(name)
				}
				item["name"] = name
				if t.Function.Description != "" {
					item["description"] = t.Function.Description
				}
				if len(t.Function.Parameters) > 0 {
					var params any
					_ = json.Unmarshal(t.Function.Parameters, &params)
					item["parameters"] = params
				}
				if t.Function.Strict != nil {
					item["strict"] = *t.Function.Strict
				}
				tools = append(tools, item)
			}
		}
		out["tools"] = tools
	}

	// tool_choice
	if len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
		var strVal string
		if err := json.Unmarshal(req.ToolChoice, &strVal); err == nil {
			out["tool_choice"] = strVal
		} else {
			var objVal map[string]any
			if err2 := json.Unmarshal(req.ToolChoice, &objVal); err2 == nil {
				tcType, _ := objVal["type"].(string)
				if tcType == "function" {
					name := ""
					if fn, ok := objVal["function"].(map[string]any); ok {
						name, _ = fn["name"].(string)
					}
					if name != "" {
						if short, ok := originalToolNameMap[name]; ok {
							name = short
						} else {
							name = shortenNameIfNeeded(name)
						}
					}
					choice := map[string]any{"type": "function"}
					if name != "" {
						choice["name"] = name
					}
					out["tool_choice"] = choice
				} else if tcType != "" {
					out["tool_choice"] = objVal
				}
			}
		}
	}

	b, _ := json.Marshal(out)
	return b
}

// Helpers

func buildContentParts(role string, raw json.RawMessage) []any {
	parts := make([]any, 0)
	if len(raw) == 0 {
		return parts
	}

	first := firstNonSpaceByte(raw)
	switch first {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return parts
		}
		if s == "" {
			return parts
		}
		partType := "input_text"
		if role == "assistant" {
			partType = "output_text"
		}
		parts = append(parts, map[string]any{
			"type": partType,
			"text": s,
		})
		return parts
	case '[':
	default:
		return parts
	}

	var arr []chatContentPart
	if err := json.Unmarshal(raw, &arr); err != nil {
		return parts
	}
	for _, item := range arr {
		switch item.Type {
		case "text":
			partType := "input_text"
			if role == "assistant" {
				partType = "output_text"
			}
			parts = append(parts, map[string]any{
				"type": partType,
				"text": item.Text,
			})
		case "image_url":
			if role == "user" && item.ImageURL != nil {
				url := item.ImageURL.URL
				fileID := item.ImageURL.FileID
				if url == "" && fileID == "" {
					continue
				}
				part := map[string]any{"type": "input_image"}
				if url != "" {
					part["image_url"] = url
				}
				if fileID != "" {
					part["file_id"] = fileID
				}
				if item.ImageURL.Detail != "" {
					part["detail"] = item.ImageURL.Detail
				}
				parts = append(parts, part)
			}
		case "file":
			if role == "user" && item.File != nil {
				fileData := item.File.FileData
				fileID := item.File.FileID
				fileURL := item.File.FileURL
				if fileData == "" && fileID == "" && fileURL == "" {
					continue
				}
				part := map[string]any{"type": "input_file"}
				if fileData != "" {
					part["file_data"] = fileData
				}
				if fileID != "" {
					part["file_id"] = fileID
				}
				if fileURL != "" {
					part["file_url"] = fileURL
				}
				if item.File.Filename != "" {
					part["filename"] = item.File.Filename
				}
				parts = append(parts, part)
			}
		case "input_audio":
			if role == "user" && item.Audio != nil && item.Audio.Data != "" {
				part := map[string]any{
					"type": "input_audio",
					"data": item.Audio.Data,
				}
				if item.Audio.Format != "" {
					part["format"] = item.Audio.Format
				}
				parts = append(parts, part)
			}
		}
	}
	return parts
}

func setToolCallOutput(item map[string]any, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	first := firstNonSpaceByte(raw)
	switch first {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			item["output"] = s
		}
	case '[':
		var arr []toolOutputContentPart
		if err := json.Unmarshal(raw, &arr); err != nil {
			item["output"] = string(raw)
			return
		}
		items := make([]any, 0, len(arr))
		for _, p := range arr {
			items = append(items, buildToolOutputPart(p))
		}
		item["output"] = items
	default:
		item["output"] = string(raw)
	}
}

type toolOutputContentPart struct {
	Type     string              `json:"type"`
	Text     string              `json:"text"`
	ImageURL *toolOutputImageURL `json:"image_url"`
	File     *toolOutputFile     `json:"file"`
	// catch-all for unknown part types
	Raw json.RawMessage `json:"-"`
}

func (p *toolOutputContentPart) UnmarshalJSON(data []byte) error {
	type alias toolOutputContentPart
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*p = toolOutputContentPart(a)
	p.Raw = data
	return nil
}

type toolOutputImageURL struct {
	URL    string `json:"url"`
	FileID string `json:"file_id"`
	Detail string `json:"detail"`
}

type toolOutputFile struct {
	FileID   string `json:"file_id"`
	FileData string `json:"file_data"`
	FileURL  string `json:"file_url"`
	Filename string `json:"filename"`
}

func buildToolOutputPart(p toolOutputContentPart) map[string]any {
	switch p.Type {
	case "text":
		return map[string]any{"type": "input_text", "text": p.Text}
	case "image_url":
		if p.ImageURL == nil || (p.ImageURL.URL == "" && p.ImageURL.FileID == "") {
			return toolOutputFallbackPartFromRaw(p.Raw)
		}
		part := map[string]any{"type": "input_image"}
		if p.ImageURL.URL != "" {
			part["image_url"] = p.ImageURL.URL
		}
		if p.ImageURL.FileID != "" {
			part["file_id"] = p.ImageURL.FileID
		}
		if p.ImageURL.Detail != "" {
			part["detail"] = p.ImageURL.Detail
		}
		return part
	case "file":
		if p.File == nil || (p.File.FileID == "" && p.File.FileData == "" && p.File.FileURL == "") {
			return toolOutputFallbackPartFromRaw(p.Raw)
		}
		part := map[string]any{"type": "input_file"}
		if p.File.FileID != "" {
			part["file_id"] = p.File.FileID
		}
		if p.File.FileData != "" {
			part["file_data"] = p.File.FileData
		}
		if p.File.FileURL != "" {
			part["file_url"] = p.File.FileURL
		}
		if p.File.Filename != "" {
			part["filename"] = p.File.Filename
		}
		return part
	default:
		return toolOutputFallbackPartFromRaw(p.Raw)
	}
}

func toolOutputFallbackPartFromRaw(raw json.RawMessage) map[string]any {
	text := string(raw)
	part := map[string]any{"type": "input_text"}
	if text != "" {
		part["text"] = text
	}
	return part
}

func firstNonSpaceByte(raw json.RawMessage) byte {
	for _, b := range raw {
		switch b {
		case ' ', '\n', '\r', '\t':
			continue
		default:
			return b
		}
	}
	return 0
}

func buildTextObject(rf *chatRespFormat, tc *chatTextCfg) map[string]any {
	if rf == nil && tc == nil {
		return nil
	}
	textObj := map[string]any{}
	if rf != nil {
		format := map[string]any{}
		switch rf.Type {
		case "text":
			format["type"] = "text"
		case "json_schema":
			format["type"] = "json_schema"
			if rf.JSONSchema != nil {
				if rf.JSONSchema.Name != "" {
					format["name"] = rf.JSONSchema.Name
				}
				if rf.JSONSchema.Strict != nil {
					format["strict"] = *rf.JSONSchema.Strict
				}
				if len(rf.JSONSchema.Schema) > 0 {
					var schema any
					_ = json.Unmarshal(rf.JSONSchema.Schema, &schema)
					format["schema"] = schema
				}
			}
		}
		if len(format) > 0 {
			textObj["format"] = format
		}
	}
	if tc != nil && len(tc.Verbosity) > 0 && string(tc.Verbosity) != "null" {
		var v any
		_ = json.Unmarshal(tc.Verbosity, &v)
		textObj["verbosity"] = v
	}
	if len(textObj) == 0 {
		return nil
	}
	return textObj
}

// shortenNameIfNeeded applies the simple shortening rule for a single name.
// If the name length exceeds 64, it will try to preserve the "mcp__" prefix and last segment.
// Otherwise it truncates to 64 characters.
func shortenNameIfNeeded(name string) string {
	const limit = 64
	if len(name) <= limit {
		return name
	}
	if strings.HasPrefix(name, "mcp__") {
		// Keep prefix and last segment after '__'
		idx := strings.LastIndex(name, "__")
		if idx > 0 {
			candidate := "mcp__" + name[idx+2:]
			if len(candidate) > limit {
				return candidate[:limit]
			}
			return candidate
		}
	}
	return name[:limit]
}

// buildShortNameMap generates unique short names (<=64) for the given list of names.
// It preserves the "mcp__" prefix with the last segment when possible and ensures uniqueness
// by appending suffixes like "_1", "_2" if needed.
func buildShortNameMap(names []string) map[string]string {
	const limit = 64
	used := map[string]struct{}{}
	m := map[string]string{}

	baseCandidate := func(n string) string {
		if len(n) <= limit {
			return n
		}
		if strings.HasPrefix(n, "mcp__") {
			idx := strings.LastIndex(n, "__")
			if idx > 0 {
				cand := "mcp__" + n[idx+2:]
				if len(cand) > limit {
					cand = cand[:limit]
				}
				return cand
			}
		}
		return n[:limit]
	}

	makeUnique := func(cand string) string {
		if _, ok := used[cand]; !ok {
			return cand
		}
		base := cand
		for i := 1; ; i++ {
			suffix := "_" + strconv.Itoa(i)
			allowed := limit - len(suffix)
			if allowed < 0 {
				allowed = 0
			}
			tmp := base
			if len(tmp) > allowed {
				tmp = tmp[:allowed]
			}
			tmp = tmp + suffix
			if _, ok := used[tmp]; !ok {
				return tmp
			}
		}
	}

	for _, n := range names {
		cand := baseCandidate(n)
		uniq := makeUnique(cand)
		used[uniq] = struct{}{}
		m[n] = uniq
	}
	return m
}
