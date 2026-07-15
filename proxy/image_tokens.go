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

// classifyImagePart reports whether a content part is an image the translator
// would upload as image bytes and, if so, returns the raw candidate payload
// (data URL or base64) without decoding or validating it. It is the single
// recognition oracle shared by the token estimator; the rule mirrors the
// translator's image extractors (extractImageFromClaudeBlock /
// extractImageFromOpenAIPart), which treat a block as an image only when they
// can pull a *local* base64/data-URL payload out of it:
//
//   - Explicit text-type blocks are never images.
//   - output_image is excluded: the translator's extractors reject that type
//     and forward it as text, so counting it as text here keeps the estimate
//     consistent with what is actually sent upstream.
//   - Any other block is an image iff a local base64 payload is extractable.
//     A remote http(s) URL or a bare file reference carries no local bytes —
//     the proxy forwards it as text rather than fetching it — so it is not
//     treated as an image (it is counted as its short text form instead).
//
// Aligning recognition this way prevents both failure modes: an image's base64
// counted as text (the orders-of-magnitude over-count this change fixes) and a
// non-image charged image tokens while the translator ships its bytes as text.
func classifyImagePart(part map[string]interface{}) (data string, isImage bool) {
	switch typ, _ := part["type"].(string); typ {
	case "text", "input_text", "output_text", "thinking", "output_image":
		return "", false
	}
	// Mirror the translator's MIME gate: a block that declares a non-image
	// content type is forwarded as text, so it must not be classified as an
	// image here (otherwise we'd charge vision tokens while the base64 is
	// actually uploaded as text — a large under-count for e.g. PDF file parts).
	if hasNonImageMIME(part) {
		return "", false
	}
	raw := imageDataFromClaudeBlock(part)
	if raw == "" || stripDataURLPrefix(raw) == "" {
		return "", false
	}
	return raw, true
}

// hasNonImageMIME reports whether the part declares a top-level content type
// that is present but not image/*. It matches the gate in
// extractImageFromOpenAIPart so recognition stays aligned with what the
// translator actually uploads as an image.
func hasNonImageMIME(part map[string]interface{}) bool {
	for _, key := range []string{"mime", "media_type", "mime_type"} {
		if raw, ok := part[key].(string); ok && raw != "" {
			if !strings.HasPrefix(strings.ToLower(raw), "image/") {
				return true
			}
		}
	}
	return false
}

// imageDataFromClaudeBlock returns the raw image payload string (data URL or
// bare base64) from a Claude-style image block, without validating or decoding
// it. It mirrors the key lookup in extractImageFromClaudeBlock but skips the
// full base64 validation, since the token estimator only needs a cheap size
// probe. Empty string means no usable payload was found.
func imageDataFromClaudeBlock(block map[string]interface{}) string {
	if source, ok := block["source"].(map[string]interface{}); ok {
		if data, ok := source["data"].(string); ok && data != "" {
			return data
		}
		if url, ok := source["url"].(string); ok && url != "" {
			return url
		}
	}
	if data, ok := block["data"].(string); ok && data != "" {
		return data
	}
	return imageDataFromOpenAIPart(block)
}

// imageDataFromOpenAIPart returns the raw image payload string from an
// OpenAI-style content part, without the full base64 validation that
// extractImageFromOpenAIPart performs. It is the estimator-path counterpart of
// that function: a cheap candidate-string lookup rather than a decode. Empty
// string means the part carries no image payload.
func imageDataFromOpenAIPart(part map[string]interface{}) string {
	if fileObj, ok := part["file"].(map[string]interface{}); ok {
		if s := imageDataFromOpenAIPart(fileObj); s != "" {
			return s
		}
	}
	if sourceObj, ok := part["source"].(map[string]interface{}); ok {
		if s := imageDataFromOpenAIPart(sourceObj); s != "" {
			return s
		}
	}
	if raw, ok := part["url"].(string); ok && raw != "" {
		return raw
	}
	if raw, ok := part["b64_json"].(string); ok && raw != "" {
		return raw
	}
	if raw, ok := part["image_url"]; ok {
		switch v := raw.(type) {
		case string:
			if v != "" {
				return v
			}
		case map[string]interface{}:
			if u, ok := v["url"].(string); ok && u != "" {
				return u
			}
		}
	}
	if raw, ok := part["image_base64"].(string); ok && raw != "" {
		return raw
	}
	if raw, ok := part["data"].(string); ok && raw != "" {
		return raw
	}
	return ""
}
