package proxy

import (
	"encoding/json"
	"math"
)

func estimateApproxTokens(text string) int {
	if text == "" {
		return 0
	}

	runes := []rune(text)
	length := len(runes)
	if length == 0 {
		return 0
	}
	if length < 5 {
		return max(1, int(math.Ceil(float64(length)/3.0)))
	}

	var regularAscii, digits, symbols, nonASCII int
	for _, r := range runes {
		switch {
		case r >= 0x80:
			nonASCII++
		case r >= '0' && r <= '9':
			digits++
		case (r >= '!' && r <= '/') || (r >= ':' && r <= '@') || (r >= '[' && r <= '`') || (r >= '{' && r <= '~'):
			symbols++
		default:
			regularAscii++
		}
	}

	estimated := int(math.Ceil(
		float64(regularAscii)/4.5 +
			float64(digits)/2.0 +
			float64(symbols)/1.5 +
			float64(nonASCII)/1.5,
	))

	if estimated < 1 {
		return 1
	}
	return estimated
}

func estimateClaudeRequestInputTokens(req *ClaudeRequest) int {
	if req == nil {
		return 0
	}

	total := estimateClaudeValueTokens(req.System)

	for _, msg := range req.Messages {
		total += estimateClaudeValueTokens(msg.Content)
	}

	for _, tool := range req.Tools {
		total += countTokens(tool.Name)
		total += countTokens(tool.Description)
		total += countClaudeJSONTokens(tool.InputSchema)
	}

	return total
}

// countClaudeJSONTokens is the Claude-input-path counterpart of
// estimateJSONTokens: it marshals v and counts the result with the shared
// tiktoken encoder (countTokens) instead of the character-class heuristic. It is
// defined separately so the Claude input estimator can move to tiktoken without
// disturbing estimateJSONTokens, which the OpenAI input path still shares.
func countClaudeJSONTokens(v interface{}) int {
	if v == nil {
		return 0
	}
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return countTokens(string(b))
}

func estimateClaudeOutputTokens(content, thinkingContent string, toolUses []KiroToolUse) int {
	total := countOutputTokens(content)
	total += countOutputTokens(thinkingContent)

	for _, tu := range toolUses {
		total += countOutputTokens(tu.Name)
		if b, err := json.Marshal(tu.Input); err == nil {
			total += countOutputTokens(string(b))
		}
	}

	return total
}

func estimateClaudeValueTokens(v interface{}) int {
	switch value := v.(type) {
	case nil:
		return 0
	case string:
		return countTokens(value)
	case []interface{}:
		total := 0
		for _, part := range value {
			total += estimateClaudeValueTokens(part)
		}
		return total
	case map[string]interface{}:
		typeName, _ := value["type"].(string)
		switch typeName {
		case "text":
			if text, ok := value["text"].(string); ok {
				return countTokens(text)
			}
		case "thinking":
			if thinking, ok := value["thinking"].(string); ok {
				return countTokens(thinking)
			}
		case "tool_use":
			total := 0
			if name, ok := value["name"].(string); ok {
				total += countTokens(name)
			}
			if input, ok := value["input"]; ok {
				total += countClaudeJSONTokens(input)
			}
			if total > 0 {
				return total
			}
		case "tool_result":
			if content, ok := value["content"]; ok {
				return estimateClaudeValueTokens(content)
			}
		}

		// Any image block (image/image_url/input_image, file/input_file, or a
		// typeless block carrying an image payload) is costed by its dimensions
		// rather than letting the base64 fall through to countClaudeJSONTokens
		// below, which would count the encoded bytes as text and inflate the
		// estimate by 1-2 orders of magnitude. classifyImagePart mirrors the
		// translator's recognition so we never diverge from what is actually
		// uploaded as an image.
		if data, isImage := classifyImagePart(value); isImage {
			if data == "" {
				return fallbackImageTokens
			}
			return estimateImageTokens(data)
		}

		total := 0
		if text, ok := value["text"].(string); ok {
			total += countTokens(text)
		}
		if thinking, ok := value["thinking"].(string); ok {
			total += countTokens(thinking)
		}
		if content, ok := value["content"]; ok {
			total += estimateClaudeValueTokens(content)
		}
		if total > 0 {
			return total
		}

		return countClaudeJSONTokens(value)
	default:
		return countClaudeJSONTokens(value)
	}
}

func estimateJSONTokens(v interface{}) int {
	if v == nil {
		return 0
	}

	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}

	return estimateApproxTokens(string(b))
}

func estimateOpenAIRequestInputTokens(req *OpenAIRequest) int {
	if req == nil {
		return 0
	}

	total := 0

	for _, msg := range req.Messages {
		total += estimateOpenAIContentTokens(msg.Content)
		total += estimateApproxTokens(msg.ToolCallID)
		for _, tc := range msg.ToolCalls {
			total += estimateApproxTokens(tc.Function.Name)
			total += estimateApproxTokens(tc.Function.Arguments)
		}
	}

	for _, tool := range req.Tools {
		total += estimateApproxTokens(tool.Function.Name)
		total += estimateApproxTokens(tool.Function.Description)
		total += estimateJSONTokens(tool.Function.Parameters)
	}

	return total
}

func estimateOpenAIContentTokens(content interface{}) int {
	switch value := content.(type) {
	case nil:
		return 0
	case string:
		return estimateApproxTokens(value)
	case []interface{}:
		total := 0
		for _, part := range value {
			total += estimateOpenAIPartTokens(part)
		}
		return total
	case map[string]interface{}:
		return estimateOpenAIPartTokens(value)
	default:
		return estimateJSONTokens(value)
	}
}

// estimateOpenAIPartTokens estimates one OpenAI content part. Image parts are
// costed by their dimensions via estimateImageTokens (using the shared
// classifyImagePart oracle so recognition matches the translator); text parts
// use the character heuristic. Image is checked before text so a single part
// that carries both a caption field and an image payload is not under-counted
// as text-only. This deliberately avoids extractOpenAIMessageText, whose
// JSON-marshal fallback would emit an image part's base64 payload and count it
// as text.
func estimateOpenAIPartTokens(part interface{}) int {
	m, ok := part.(map[string]interface{})
	if !ok {
		return estimateOpenAIContentTokens(part)
	}
	if data, isImage := classifyImagePart(m); isImage {
		if data == "" {
			return fallbackImageTokens
		}
		return estimateImageTokens(data)
	}
	if t, ok := extractOpenAITextPart(m); ok {
		return estimateApproxTokens(t)
	}
	if nested, ok := m["content"]; ok {
		return estimateOpenAIContentTokens(nested)
	}
	return estimateJSONTokens(m)
}

func estimateOpenAIOutputTokens(content, reasoningContent string, toolUses []KiroToolUse) int {
	return estimateClaudeOutputTokens(content, reasoningContent, toolUses)
}
