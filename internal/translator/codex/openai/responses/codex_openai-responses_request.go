package responses

import (
	"bytes"
	"encoding/json"
	"fmt"

	log "github.com/sirupsen/logrus"
)

func ConvertOpenAIResponsesRequestToCodex(modelName string, inputRawJSON []byte, _ bool) []byte {
	_ = modelName

	var m map[string]json.RawMessage
	if err := json.Unmarshal(inputRawJSON, &m); err != nil {
		return inputRawJSON
	}

	if inputRaw, ok := m["input"]; ok {
		trimmed := bytes.TrimSpace(inputRaw)
		if len(trimmed) > 0 {
			switch trimmed[0] {
			case '"':
				var s string
				if err := json.Unmarshal(trimmed, &s); err == nil {
					wrapped, err := json.Marshal([]codexInputMessage{{
						Type: "message",
						Role: "user",
						Content: []codexInputContent{
							{Type: "input_text", Text: s},
						},
					}})
					if err == nil {
						m["input"] = wrapped
					}
				}
			case '[':
				if rewritten := convertInputArraySystemRole(trimmed); !bytes.Equal(rewritten, trimmed) {
					m["input"] = rewritten
				}
			}
		}
	}

	m["stream"] = json.RawMessage("true")
	m["store"] = json.RawMessage("false")
	m["parallel_tool_calls"] = json.RawMessage("true")
	m["include"] = json.RawMessage(`["reasoning.encrypted_content"]`)

	delete(m, "max_output_tokens")
	delete(m, "max_completion_tokens")
	delete(m, "temperature")
	delete(m, "top_p")
	delete(m, "truncation")
	delete(m, "user")
	delete(m, "context_management")

	if raw, ok := m["service_tier"]; ok {
		var tier string
		if err := json.Unmarshal(raw, &tier); err != nil || tier != "priority" {
			delete(m, "service_tier")
		}
	}

	out, err := json.Marshal(m)
	if err != nil {
		return inputRawJSON
	}

	out = normalizeCodexBuiltinTools(out)
	return out
}

type codexInputContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type codexInputMessage struct {
	Type    string              `json:"type"`
	Role    string              `json:"role"`
	Content []codexInputContent `json:"content"`
}

func convertInputArraySystemRole(rawJSON []byte) []byte {
	var elems []json.RawMessage
	if err := json.Unmarshal(rawJSON, &elems); err != nil {
		return rawJSON
	}

	changed := false
	for i, elem := range elems {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(elem, &obj); err != nil {
			continue
		}
		roleRaw, ok := obj["role"]
		if !ok {
			continue
		}
		var role string
		if err := json.Unmarshal(roleRaw, &role); err != nil {
			continue
		}
		if role != "system" {
			continue
		}

		obj["role"] = json.RawMessage(`"developer"`)
		newElem, err := json.Marshal(obj)
		if err != nil {
			continue
		}
		elems[i] = newElem
		changed = true
	}

	if !changed {
		return rawJSON
	}
	out, err := json.Marshal(elems)
	if err != nil {
		return rawJSON
	}
	return out
}

// convertSystemRoleToDeveloper extracts the "input" array from a full
// Responses API request JSON, converts any "system" roles to "developer",
// and returns the updated JSON.
func convertSystemRoleToDeveloper(rawJSON []byte) []byte {
	inputRaw := json.RawMessage(rawJSON)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(inputRaw, &m); err != nil {
		return rawJSON
	}
	inputArr, ok := m["input"]
	if !ok {
		return rawJSON
	}
	rewritten := convertInputArraySystemRole(inputArr)
	if bytes.Equal(rewritten, inputArr) {
		return rawJSON
	}
	m["input"] = rewritten
	out, err := json.Marshal(m)
	if err != nil {
		return rawJSON
	}
	return out
}

// normalizeCodexBuiltinTools rewrites legacy/preview built-in tool variants to the
// stable names expected by the current Codex upstream.
func normalizeCodexBuiltinTools(rawJSON []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(rawJSON, &m); err != nil {
		return rawJSON
	}

	changed := false

	if rawTools, ok := m["tools"]; ok {
		if updated := normalizeBuiltinToolArray(rawTools); updated != nil {
			m["tools"] = updated
			changed = true
		}
	}

	if rawChoice, ok := m["tool_choice"]; ok {
		first := firstNonSpaceByte(rawChoice)
		if first == '{' {
			var choiceObj map[string]json.RawMessage
			if err := json.Unmarshal(rawChoice, &choiceObj); err == nil {
				if updatedType := normalizeBuiltinToolAtKey(choiceObj, "type"); updatedType {
					if newChoice, err := json.Marshal(choiceObj); err == nil {
						m["tool_choice"] = newChoice
						changed = true
					}
				}
				if rawChoiceTools, ok := choiceObj["tools"]; ok {
					if updated := normalizeBuiltinToolArray(rawChoiceTools); updated != nil {
						choiceObj["tools"] = updated
						if newChoice, err := json.Marshal(choiceObj); err == nil {
							m["tool_choice"] = newChoice
							changed = true
						}
					}
				}
			}
		}
	}

	if !changed {
		return rawJSON
	}
	out, err := json.Marshal(m)
	if err != nil {
		return rawJSON
	}
	return out
}

func normalizeBuiltinToolArray(raw json.RawMessage) json.RawMessage {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}

	changed := false
	for i, item := range items {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(item, &obj); err != nil {
			continue
		}
		typeRaw, ok := obj["type"]
		if !ok {
			continue
		}
		var toolType string
		if err := json.Unmarshal(typeRaw, &toolType); err != nil {
			continue
		}
		normalized := normalizeCodexBuiltinToolType(toolType)
		if normalized == "" {
			continue
		}
		log.Debugf("codex responses: normalized builtin tool type at tools.%d.type from %q to %q", i, toolType, normalized)
		obj["type"] = json.RawMessage(fmt.Sprintf(`"%s"`, normalized))
		if newItem, err := json.Marshal(obj); err == nil {
			items[i] = newItem
		}
		changed = true
	}

	if !changed {
		return nil
	}
	out, err := json.Marshal(items)
	if err != nil {
		return nil
	}
	return out
}

func normalizeBuiltinToolAtKey(obj map[string]json.RawMessage, key string) bool {
	raw, ok := obj[key]
	if !ok {
		return false
	}
	var toolType string
	if err := json.Unmarshal(raw, &toolType); err != nil {
		return false
	}
	normalized := normalizeCodexBuiltinToolType(toolType)
	if normalized == "" {
		return false
	}
	log.Debugf("codex responses: normalized builtin tool type at %s from %q to %q", key, toolType, normalized)
	obj[key] = json.RawMessage(fmt.Sprintf(`"%s"`, normalized))
	return true
}

// normalizeCodexBuiltinToolType centralizes the current known Codex Responses
// built-in tool alias compatibility.
func normalizeCodexBuiltinToolType(toolType string) string {
	switch toolType {
	case "web_search_preview", "web_search_preview_2025_03_11":
		return "web_search"
	default:
		return ""
	}
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
