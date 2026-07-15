package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

func parseResponsesInput(raw json.RawMessage) ([]OpenAIMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}

	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("invalid input string: %w", err)
		}
		if strings.TrimSpace(s) == "" {
			return nil, nil
		}
		return []OpenAIMessage{{Role: "user", Content: s}}, nil
	}

	if trimmed[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("invalid input array: %w", err)
		}
		return convertResponsesInputItems(items)
	}

	if trimmed[0] == '{' {
		return convertResponsesInputItems([]json.RawMessage{raw})
	}

	return nil, fmt.Errorf("unsupported input shape")
}

func convertResponsesInputItems(items []json.RawMessage) ([]OpenAIMessage, error) {
	messages := make([]OpenAIMessage, 0, len(items))
	pendingUserParts := []interface{}{}

	flushPendingUser := func() {
		if len(pendingUserParts) == 0 {
			return
		}
		messages = append(messages, OpenAIMessage{
			Role:    "user",
			Content: pendingUserParts,
		})
		pendingUserParts = nil
	}

	for _, item := range items {
		var obj map[string]interface{}
		if err := json.Unmarshal(item, &obj); err != nil {
			continue
		}

		typ, _ := obj["type"].(string)
		role, _ := obj["role"].(string)

		switch {
		case typ == "message" || (typ == "" && role != ""):
			flushPendingUser()
			msg := buildMessageFromInputItem(obj, role)
			if msg != nil {
				messages = append(messages, *msg)
			}

		case typ == "function_call_output" || typ == "tool_result":
			flushPendingUser()
			callID, _ := obj["call_id"].(string)
			if callID == "" {
				callID, _ = obj["tool_call_id"].(string)
			}
			out := toolOutputContent(obj["output"])
			if out == nil {
				out = toolOutputContent(obj["content"])
			}
			messages = append(messages, OpenAIMessage{
				Role:       "tool",
				Content:    out,
				ToolCallID: callID,
			})

		case typ == "function_call":
			flushPendingUser()
			tc := ToolCall{
				ID:   stringField(obj, "call_id", "id"),
				Type: "function",
			}
			tc.Function.Name, _ = obj["name"].(string)
			tc.Function.Arguments = stringifyArbitrary(obj["arguments"])
			// Merge consecutive function_call items into a single assistant
			// message so parallel tool calls stay grouped in one turn. The
			// Responses API emits each parallel call as a separate input item;
			// keeping them in one assistant message preserves the tool_use /
			// tool_result pairing that Kiro requires.
			if n := len(messages); n > 0 &&
				messages[n-1].Role == "assistant" &&
				len(messages[n-1].ToolCalls) > 0 &&
				strings.TrimSpace(extractOpenAIMessageText(messages[n-1].Content)) == "" {
				messages[n-1].ToolCalls = append(messages[n-1].ToolCalls, tc)
			} else {
				messages = append(messages, OpenAIMessage{
					Role:      "assistant",
					Content:   "",
					ToolCalls: []ToolCall{tc},
				})
			}

		case typ == "input_text" || typ == "text":
			text, _ := obj["text"].(string)
			if text != "" {
				pendingUserParts = append(pendingUserParts, map[string]interface{}{
					"type": "input_text",
					"text": text,
				})
			}

		case typ == "input_image", typ == "image", typ == "image_url":
			pendingUserParts = append(pendingUserParts, map[string]interface{}(obj))

		case typ == "output_text":
			flushPendingUser()
			text, _ := obj["text"].(string)
			if text != "" {
				messages = append(messages, OpenAIMessage{Role: "assistant", Content: text})
			}

		default:
			if role != "" {
				flushPendingUser()
				msg := buildMessageFromInputItem(obj, role)
				if msg != nil {
					messages = append(messages, *msg)
				}
			}
		}
	}

	flushPendingUser()
	return messages, nil
}

func buildMessageFromInputItem(obj map[string]interface{}, role string) *OpenAIMessage {
	if role == "" {
		role = "user"
	}

	if content, ok := obj["content"]; ok {
		switch v := content.(type) {
		case string:
			return &OpenAIMessage{Role: role, Content: v}
		case []interface{}:
			parts := make([]interface{}, 0, len(v))
			textOnly := strings.Builder{}
			anyNonText := false
			for _, p := range v {
				part, ok := p.(map[string]interface{})
				if !ok {
					continue
				}
				ptype, _ := part["type"].(string)
				switch ptype {
				case "input_text", "text":
					if t, ok := part["text"].(string); ok {
						textOnly.WriteString(t)
						parts = append(parts, map[string]interface{}{"type": "input_text", "text": t})
					}
				case "output_text":
					if t, ok := part["text"].(string); ok {
						textOnly.WriteString(t)
						parts = append(parts, map[string]interface{}{"type": "input_text", "text": t})
					}
				case "input_image", "image", "image_url":
					anyNonText = true
					parts = append(parts, part)
				default:
					if t, ok := part["text"].(string); ok && t != "" {
						textOnly.WriteString(t)
						parts = append(parts, map[string]interface{}{"type": "input_text", "text": t})
					}
				}
			}
			if !anyNonText {
				return &OpenAIMessage{Role: role, Content: textOnly.String()}
			}
			return &OpenAIMessage{Role: role, Content: parts}
		case map[string]interface{}:
			return buildMessageFromInputItem(v, role)
		}
	}

	if text, ok := obj["text"].(string); ok && text != "" {
		return &OpenAIMessage{Role: role, Content: text}
	}

	return nil
}

func stringifyArbitrary(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// toolOutputContent normalizes a Responses tool-output value into an
// OpenAIMessage.Content. When the output is an array containing an image part
// that the translator can actually upload (classifyImagePart), it returns a
// normalized []interface{} so downstream conversion and token estimation see
// the image instead of a base64 blob buried in a JSON string. To avoid changing
// the old stringify semantics for everything else, non-image parts in that same
// array are collapsed into text (image parts kept as-is), rather than dropping
// siblings or preserving unknown objects verbatim. Text-only or arbitrary
// output keeps the previous single-string form. Returns nil for an
// empty/absent value so the caller can fall back to the alternate field.
func toolOutputContent(v interface{}) interface{} {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if t == "" {
			return nil
		}
		return t
	case []interface{}:
		if parts := normalizeToolOutputParts(t); parts != nil {
			return parts
		}
	case map[string]interface{}:
		if parts := normalizeToolOutputParts([]interface{}{t}); parts != nil {
			return parts
		}
	}
	if s := stringifyArbitrary(v); s != "" {
		return s
	}
	return nil
}

// normalizeToolOutputParts returns a normalized content-part slice when the
// array carries at least one uploadable image part, or nil to signal the caller
// should fall back to plain stringify. Image parts are kept structurally; every
// other element is folded into a single input_text block so no sibling content
// is lost (the pre-change behavior stringified the whole value). It keys image
// recognition off classifyImagePart so it matches exactly what the translator
// will forward as an image.
func normalizeToolOutputParts(parts []interface{}) []interface{} {
	hasImage := false
	var texts []string

	for _, p := range parts {
		if m, ok := p.(map[string]interface{}); ok {
			if _, isImage := classifyImagePart(m); isImage {
				hasImage = true
				continue
			}
			if t, ok := extractOpenAITextPart(m); ok {
				texts = append(texts, t)
				continue
			}
		}
		// Unknown element: preserve its content as text rather than dropping it.
		if s := stringifyArbitrary(p); s != "" {
			texts = append(texts, s)
		}
	}

	if !hasImage {
		return nil
	}

	// Emit image parts in their original order, then the folded text. Element
	// boundaries are preserved by joining with newlines so distinct siblings
	// (e.g. "ab", {"x":1}, "cd") don't merge into one run.
	out := make([]interface{}, 0, len(parts)+1)
	for _, p := range parts {
		if m, ok := p.(map[string]interface{}); ok {
			if _, isImage := classifyImagePart(m); isImage {
				out = append(out, m)
			}
		}
	}
	if len(texts) > 0 {
		out = append(out, map[string]interface{}{
			"type": "input_text",
			"text": strings.Join(texts, "\n"),
		})
	}
	return out
}

func stringField(obj map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := obj[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
