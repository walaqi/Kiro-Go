package proxy

import (
	"encoding/base64"
	"image"
	_ "image/gif"  // register GIF decoder for image.DecodeConfig
	_ "image/jpeg" // register JPEG decoder for image.DecodeConfig
	_ "image/png"  // register PNG decoder for image.DecodeConfig
	"io"
	"math"
	"strings"
)

const (
	// fallbackImageTokens is charged when an image's dimensions can't be
	// measured locally (webp and other unregistered formats, remote URLs, or a
	// decode failure). It equals the per-image ceiling the size formula below
	// produces at the maximum allowed area, so an unmeasurable image is treated
	// like a full-size one rather than being under-counted.
	fallbackImageTokens = 1534

	// maxImageEdge and maxImagePixels mirror Anthropic's documented image
	// down-scaling limits (long edge <= 1568px, area <~ 1.15M px). They are
	// approximate published values, not an exact protocol constant.
	maxImageEdge   = 1568.0
	maxImagePixels = 1_150_000.0

	// pixelsPerImageToken is Anthropic's ~(width*height)/750 vision-token rule.
	pixelsPerImageToken = 750.0

	// maxImageHeaderBytes caps how many source (base64) bytes we hand to the
	// streaming decoder. Image dimensions live in the file header, so this is
	// far more than any header needs; the cap exists so a crafted file (e.g. a
	// JPEG with bloated leading metadata) can't turn a "read the header" probe
	// into a full-stream scan.
	maxImageHeaderBytes = 512 * 1024
)

// estimateImageTokens approximates the vision-token cost of an image using
// Anthropic's ceil(width*height/750) rule (after clamping to the long-edge and
// area limits), instead of counting the base64 payload as if it were text. It
// reads only the image header through a bounded, streaming base64 decoder, so a
// multi-megabyte image is never fully decoded here. data may be a data URL, a
// bare base64 string, or a remote URL; anything that can't be measured returns
// fallbackImageTokens.
func estimateImageTokens(data string) int {
	payload := stripDataURLPrefix(data)
	if payload == "" {
		return fallbackImageTokens
	}

	cfg, err := decodeImageConfig(payload)
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return fallbackImageTokens
	}

	w, h := float64(cfg.Width), float64(cfg.Height)
	scale := 1.0
	if m := math.Max(w, h); m > maxImageEdge {
		scale = math.Min(scale, maxImageEdge/m)
	}
	if area := w * h; area > maxImagePixels {
		scale = math.Min(scale, math.Sqrt(maxImagePixels/area))
	}
	w *= scale
	h *= scale

	tokens := int(math.Ceil(w * h / pixelsPerImageToken))
	if tokens < 1 {
		return 1
	}
	return tokens
}

// decodeImageConfig reads just the image header from a base64 payload without
// materializing the whole decoded image. It tries the standard and URL-safe
// alphabets (padded and raw); the header lives at the front of the stream, so
// DecodeConfig stops early and trailing-padding differences between variants
// rarely matter.
func decodeImageConfig(payload string) (image.Config, error) {
	encodings := []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}

	var lastErr error
	for _, enc := range encodings {
		src := io.LimitReader(strings.NewReader(payload), maxImageHeaderBytes)
		dec := base64.NewDecoder(enc, src)
		cfg, _, err := image.DecodeConfig(dec)
		if err == nil {
			return cfg, nil
		}
		lastErr = err
	}
	return image.Config{}, lastErr
}

// stripDataURLPrefix returns the raw base64 payload from a data URL, the input
// unchanged if it is already bare base64, or "" for input that can't be
// measured locally (a remote http/https URL, or a data: URL that isn't
// base64-encoded). It checks the scheme prefix first so a bare base64 payload
// is never scanned end-to-end for "base64," and a remote URL that happens to
// contain "base64," in its query string isn't mistaken for inline image data.
func stripDataURLPrefix(data string) string {
	s := strings.TrimSpace(data)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return ""
	}
	if strings.HasPrefix(s, "data:") {
		if i := strings.Index(s, "base64,"); i >= 0 {
			return strings.TrimSpace(s[i+len("base64,"):])
		}
		return "" // data: URL without base64 (e.g. utf8) — not measurable
	}
	return s // assume bare base64
}

// The token estimator recognizes images through two dialect-specific oracles,
// one per translator extractor, because the two extractors are NOT equivalent
// and each estimation call site feeds exactly one of them:
//
//   - Claude /v1/messages content -> classifyClaudeImagePart (mirrors
//     extractImageFromClaudeBlock: type-agnostic, reads source.data directly).
//   - OpenAI /v1/chat/completions and Responses content/tool-output ->
//     classifyOpenAIImagePart (mirrors extractImageFromOpenAIPart: type-gated,
//     MIME-gated, rejects output_image and source:{type:"base64"}).
//
// Keeping each oracle faithful to the extractor that actually runs for its call
// site guarantees the estimate matches what is uploaded: an image's base64 is
// never counted as text (the orders-of-magnitude over-count this change fixes),
// and a block the translator ships as text is never charged vision tokens.
// Both return the raw candidate payload (data URL or bare base64) without
// decoding; a "" data with isImage true means charge fallbackImageTokens.

// classifyClaudeImagePart mirrors extractImageFromClaudeBlock. That extractor is
// type-agnostic: it reads source.data / source.url directly (no type or MIME
// gate), then falls through to the OpenAI recognizer, then a top-level data:
// URL. This intentionally recognizes output_image blocks carrying source.data,
// which the Claude tool_result path really uploads as images.
func classifyClaudeImagePart(part map[string]interface{}) (data string, isImage bool) {
	if source, ok := part["source"].(map[string]interface{}); ok {
		if d, ok := source["data"].(string); ok && localImagePayload(d) {
			return d, true
		}
		if u, ok := source["url"].(string); ok && isDataURLImage(u) {
			return u, true
		}
	}
	if d, isImg := classifyOpenAIImagePart(part); isImg {
		return d, true
	}
	if d, ok := part["data"].(string); ok && isDataURLImage(d) {
		return d, true
	}
	return "", false
}

// classifyOpenAIImagePart mirrors extractImageFromOpenAIPart: an explicit,
// non-whitelisted type is rejected (so output_image is not an image here),
// file/source are recursed, a declared non-image MIME gates the block out, and
// the payload is read from url / b64_json / image_url / image_base64 / data.
// url and image_url accept only data: image URLs (parseDataURL); data accepts a
// data: URL or bare base64 (parseDataURL-or-parseBase64Image).
func classifyOpenAIImagePart(part map[string]interface{}) (data string, isImage bool) {
	if typ, _ := part["type"].(string); typ != "" {
		switch typ {
		case "image", "image_url", "input_image", "file", "input_file":
		default:
			return "", false
		}
	}

	if fileObj, ok := part["file"].(map[string]interface{}); ok {
		if d, isImg := classifyOpenAIImagePart(fileObj); isImg {
			return d, true
		}
	}
	if sourceObj, ok := part["source"].(map[string]interface{}); ok {
		if d, isImg := classifyOpenAIImagePart(sourceObj); isImg {
			return d, true
		}
	}

	if hasNonImageMIME(part) {
		return "", false
	}

	if raw, ok := part["url"].(string); ok && isDataURLImage(raw) {
		return raw, true
	}
	if raw, ok := part["b64_json"].(string); ok && raw != "" {
		return raw, true
	}
	if raw, ok := part["image_url"]; ok {
		switch v := raw.(type) {
		case string:
			if isDataURLImage(v) {
				return v, true
			}
		case map[string]interface{}:
			if u, ok := v["url"].(string); ok && isDataURLImage(u) {
				return u, true
			}
		}
	}
	if raw, ok := part["image_base64"].(string); ok && raw != "" {
		return raw, true
	}
	if raw, ok := part["data"].(string); ok && localImagePayload(raw) {
		return raw, true
	}
	return "", false
}

// hasNonImageMIME reports whether the part declares a top-level content-type
// field that is present as a string but not image/*. It matches the gate in
// extractImageFromOpenAIPart exactly: that gate rejects the block when the
// field is present and lacks an "image/" prefix, which includes the empty
// string (an empty MIME is not image/*), so this must not skip empty values.
func hasNonImageMIME(part map[string]interface{}) bool {
	for _, key := range []string{"mime", "media_type", "mime_type"} {
		if raw, ok := part[key].(string); ok {
			if !strings.HasPrefix(strings.ToLower(raw), "image/") {
				return true
			}
		}
	}
	return false
}

// localImagePayload reports whether s is a locally-measurable image payload the
// translator would accept via parseDataURL-or-parseBase64Image: a data:image/
// URL, or a bare (non-URL) base64 string. A remote http(s) URL carries no local
// bytes and a non-image data: URL (e.g. a PDF) is rejected — matching
// parseBase64Image failing to decode the full data: string.
func localImagePayload(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return false
	}
	if strings.HasPrefix(t, "http://") || strings.HasPrefix(t, "https://") {
		return false
	}
	if strings.HasPrefix(strings.ToLower(t), "data:") {
		return isDataURLImage(t)
	}
	return true // bare base64
}

// isDataURLImage reports whether s is a data:image/ URL with a base64 payload,
// mirroring parseDataURL (which rejects remote URLs, bare base64, and non-image
// data URLs for the url/image_url fields).
func isDataURLImage(s string) bool {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(strings.ToLower(t), "data:image/") {
		return false
	}
	return stripDataURLPrefix(t) != ""
}
