package proxy

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"math"
	"strings"
	"testing"
)

// makePNGBase64 renders a w x h opaque PNG and returns its standard base64
// encoding. It gives tests a real image header for image.DecodeConfig to read.
func makePNGBase64(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// A single filled pixel is enough; the estimator only reads the header.
	img.Set(0, 0, color.RGBA{R: 1, G: 2, B: 3, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestEstimateImageTokensMatchesFormula(t *testing.T) {
	const w, h = 300, 200 // below both clamps
	data := makePNGBase64(t, w, h)

	got := estimateImageTokens(data)
	want := int(math.Ceil(float64(w*h) / pixelsPerImageToken))
	if got != want {
		t.Fatalf("estimateImageTokens(%dx%d) = %d, want %d", w, h, got, want)
	}
}

func TestEstimateImageTokensDataURLPrefix(t *testing.T) {
	const w, h = 128, 64
	raw := makePNGBase64(t, w, h)
	dataURL := "data:image/png;base64," + raw

	if got, want := estimateImageTokens(dataURL), estimateImageTokens(raw); got != want {
		t.Fatalf("data URL = %d, bare base64 = %d; want equal", got, want)
	}
}

func TestEstimateImageTokensDoesNotScaleWithPayloadLength(t *testing.T) {
	// A large-byte but small-dimension image must not be counted like its
	// encoded length. This is the core regression: base64 payload size must
	// not drive the estimate.
	const w, h = 40, 40
	data := makePNGBase64(t, w, h)

	tokens := estimateImageTokens(data)
	payloadTokens := len(data) / 4 // rough "as text" lower bound
	if tokens >= payloadTokens {
		t.Fatalf("image tokens %d should be far below payload-as-text %d", tokens, payloadTokens)
	}
	// Formula ceiling for a 40x40 image is tiny.
	if want := int(math.Ceil(float64(w*h) / pixelsPerImageToken)); tokens != want {
		t.Fatalf("estimateImageTokens = %d, want %d", tokens, want)
	}
}

func TestEstimateImageTokensClampsLargeImage(t *testing.T) {
	// 4000x4000 exceeds both the edge and area limits; the result must equal
	// the area-clamped formula value and be well under the raw pixel count.
	const w, h = 4000, 4000
	data := makePNGBase64(t, w, h)

	got := estimateImageTokens(data)
	want := int(math.Ceil(maxImagePixels / pixelsPerImageToken))
	if got != want {
		t.Fatalf("clamped tokens = %d, want %d", got, want)
	}
	if raw := int(math.Ceil(float64(w*h) / pixelsPerImageToken)); got >= raw {
		t.Fatalf("clamped tokens %d should be below unclamped %d", got, raw)
	}
}

func TestEstimateImageTokensFallbacks(t *testing.T) {
	cases := map[string]string{
		"garbage base64":  "!!!!not base64!!!!",
		"empty":           "",
		"remote url":      "https://example.com/cat.png",
		"non-image bytes": base64.StdEncoding.EncodeToString([]byte("hello world, not an image")),
	}
	for name, in := range cases {
		if got := estimateImageTokens(in); got != fallbackImageTokens {
			t.Errorf("%s: got %d, want fallback %d", name, got, fallbackImageTokens)
		}
	}
}

func TestEstimateClaudeValueTokensImageIsBounded(t *testing.T) {
	data := makePNGBase64(t, 512, 512)
	block := map[string]interface{}{
		"type": "image",
		"source": map[string]interface{}{
			"type":       "base64",
			"media_type": "image/png",
			"data":       data,
		},
	}

	got := estimateClaudeValueTokens(block)
	// Must be the vision estimate, not the base64-as-JSON count.
	want := estimateImageTokens(data)
	if got != want {
		t.Fatalf("claude image block tokens = %d, want %d", got, want)
	}
	// Sanity: far below counting the encoded block as text.
	if got > 2000 {
		t.Fatalf("claude image tokens %d unexpectedly large (looks like payload was counted as text)", got)
	}
}

func TestEstimateOpenAIContentTokensImageIsBounded(t *testing.T) {
	data := makePNGBase64(t, 512, 512)
	content := []interface{}{
		map[string]interface{}{"type": "text", "text": "describe this"},
		map[string]interface{}{
			"type":      "image_url",
			"image_url": map[string]interface{}{"url": "data:image/png;base64," + data},
		},
	}

	got := estimateOpenAIContentTokens(content)
	want := estimateApproxTokens("describe this") + estimateImageTokens(data)
	if got != want {
		t.Fatalf("openai content tokens = %d, want %d", got, want)
	}
	if got > 2000 {
		t.Fatalf("openai image tokens %d unexpectedly large (payload counted as text?)", got)
	}
}

func TestImageDataFromClaudeBlockPrefersSourceData(t *testing.T) {
	block := map[string]interface{}{
		"type": "image",
		"source": map[string]interface{}{
			"data": "AAAA",
			"url":  "https://example.com/x.png",
		},
	}
	if got := imageDataFromClaudeBlock(block); got != "AAAA" {
		t.Fatalf("got %q, want source.data", got)
	}
}

func TestStripDataURLPrefix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"data:image/png;base64,ABCD", "ABCD"},
		{"data:image/jpeg;base64, ABCD ", "ABCD"},
		{"ABCD", "ABCD"},
		{"https://example.com/a.png", ""},
		{"http://example.com/a.png", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := stripDataURLPrefix(c.in); got != c.want {
			t.Errorf("stripDataURLPrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFallbackEqualsAreaCeiling(t *testing.T) {
	// The fallback constant should equal the per-image formula ceiling so an
	// unmeasurable image is treated like a full-size one, not under-counted.
	want := int(math.Ceil(maxImagePixels / pixelsPerImageToken))
	if fallbackImageTokens != want {
		t.Fatalf("fallbackImageTokens = %d, want area ceiling %d", fallbackImageTokens, want)
	}
}

func TestToolOutputContentPreservesImageParts(t *testing.T) {
	parts := []interface{}{
		map[string]interface{}{"type": "input_image", "image_url": "data:image/png;base64,AAAA"},
	}
	got := toolOutputContent(parts)
	if _, ok := got.([]interface{}); !ok {
		t.Fatalf("expected structured parts preserved, got %T (%v)", got, got)
	}
}

func TestToolOutputContentStringifiesNonImage(t *testing.T) {
	if got := toolOutputContent("plain text"); got != "plain text" {
		t.Fatalf("string output = %v, want passthrough", got)
	}
	got := toolOutputContent(map[string]interface{}{"result": "ok"})
	s, ok := got.(string)
	if !ok || !strings.Contains(s, "result") {
		t.Fatalf("non-image object should stringify, got %T (%v)", got, got)
	}
	if toolOutputContent(nil) != nil {
		t.Fatalf("nil output should return nil")
	}
	if toolOutputContent("") != nil {
		t.Fatalf("empty string output should return nil")
	}
}

// TestClassifyImagePartMatchesTranslator asserts the estimator's recognition
// oracle stays aligned with the translator's image extractor: every shape the
// translator would upload as an image is classified as one, and shapes it
// forwards as text are not. This is the invariant that keeps the estimate
// consistent with what is actually sent upstream.
func TestClassifyImagePartMatchesTranslator(t *testing.T) {
	imgData := makePNGBase64(t, 16, 16)
	dataURL := "data:image/png;base64," + imgData

	// dialect selects which real translator extractor a shape must agree with,
	// so the cross-check is per-call-site rather than "either one succeeds":
	// Claude messages use extractImageFromClaudeBlock, OpenAI/Responses use
	// extractImageFromOpenAIPart.
	const (
		claude = "claude"
		openai = "openai"
		both   = "both"
	)

	cases := []struct {
		name      string
		part      map[string]interface{}
		wantImage bool
		dialect   string // which extractor must agree when wantImage is true
	}{
		{
			name: "claude source base64",
			part: map[string]interface{}{
				"type":   "image",
				"source": map[string]interface{}{"type": "base64", "media_type": "image/png", "data": imgData},
			},
			wantImage: true,
			dialect:   claude,
		},
		{
			name:      "openai image_url data url",
			part:      map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": dataURL}},
			wantImage: true,
			dialect:   both,
		},
		{
			name:      "file wrapper with data url",
			part:      map[string]interface{}{"type": "file", "file": map[string]interface{}{"data": dataURL}},
			wantImage: true,
			dialect:   openai,
		},
		{
			name:      "typeless block carrying image payload",
			part:      map[string]interface{}{"image_url": map[string]interface{}{"url": dataURL}},
			wantImage: true,
			dialect:   both,
		},
		{
			name:      "output_image excluded (translator rejects)",
			part:      map[string]interface{}{"type": "output_image", "image_url": map[string]interface{}{"url": dataURL}},
			wantImage: false,
		},
		{
			name:      "remote url not local image",
			part:      map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "https://example.com/x.png"}},
			wantImage: false,
		},
		{
			name:      "non-image mime gated out",
			part:      map[string]interface{}{"type": "file", "mime_type": "application/pdf", "data": dataURL},
			wantImage: false,
		},
		{
			name:      "plain text block",
			part:      map[string]interface{}{"type": "text", "text": "hello"},
			wantImage: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotData, gotImage := classifyImagePart(c.part)
			if gotImage != c.wantImage {
				t.Fatalf("isImage = %v, want %v (data=%q)", gotImage, c.wantImage, gotData)
			}
			if !c.wantImage || gotData == "" {
				return
			}
			// Per-dialect cross-check: the extractor that actually runs for this
			// shape's call site must agree that it is an image, so the estimate
			// never diverges from what is uploaded.
			claudeOK := extractImageFromClaudeBlock(c.part) != nil
			openaiOK := extractImageFromOpenAIPart(c.part) != nil
			switch c.dialect {
			case claude:
				if !claudeOK {
					t.Fatalf("classified image but Claude extractor would not pick it up")
				}
			case openai:
				if !openaiOK {
					t.Fatalf("classified image but OpenAI extractor would not pick it up")
				}
			case both:
				if !claudeOK || !openaiOK {
					t.Fatalf("classified image but not both extractors agree (claude=%v openai=%v)", claudeOK, openaiOK)
				}
			}
		})
	}
}

// TestClaudeFileToolResultImageIsBounded covers Codex finding #2: a Claude
// tool_result carrying a file-wrapped image must be costed by dimensions, not
// counted as base64 text (the old narrow image-type switch missed "file").
func TestClaudeFileToolResultImageIsBounded(t *testing.T) {
	data := makePNGBase64(t, 256, 256)
	block := map[string]interface{}{
		"type": "tool_result",
		"content": []interface{}{
			map[string]interface{}{
				"type": "file",
				"file": map[string]interface{}{"data": "data:image/png;base64," + data},
			},
		},
	}
	got := estimateClaudeValueTokens(block)
	if want := estimateImageTokens(data); got != want {
		t.Fatalf("file tool_result image tokens = %d, want %d", got, want)
	}
	if got > 2000 {
		t.Fatalf("file tool_result tokens %d too large (base64 counted as text?)", got)
	}
}

// TestResponsesToolOutputMixedImageAndObject covers Codex finding #3: an image
// part is preserved structurally while a sibling non-image object is folded to
// text rather than dropped, keeping the old stringify content.
func TestResponsesToolOutputMixedImageAndObject(t *testing.T) {
	dataURL := "data:image/png;base64," + makePNGBase64(t, 32, 32)
	out := toolOutputContent([]interface{}{
		map[string]interface{}{"type": "input_image", "image_url": map[string]interface{}{"url": dataURL}},
		map[string]interface{}{"note": "some structured metadata"},
		"trailing text",
	})

	parts, ok := out.([]interface{})
	if !ok {
		t.Fatalf("expected []interface{}, got %T (%v)", out, out)
	}

	var images, texts int
	var textContent strings.Builder
	for _, p := range parts {
		m, ok := p.(map[string]interface{})
		if !ok {
			t.Fatalf("part is not a map: %T", p)
		}
		if _, isImage := classifyImagePart(m); isImage {
			images++
			continue
		}
		if typ, _ := m["type"].(string); typ == "input_text" {
			texts++
			textContent.WriteString(m["text"].(string))
		}
	}
	if images != 1 {
		t.Fatalf("expected 1 image part, got %d", images)
	}
	if texts != 1 {
		t.Fatalf("expected sibling content folded into 1 text part, got %d", texts)
	}
	s := textContent.String()
	if !strings.Contains(s, "metadata") || !strings.Contains(s, "trailing text") {
		t.Fatalf("sibling content lost: %q", s)
	}
}

// TestResponsesToolOutputOutputImageStringifies covers Codex finding #3: an
// output_image-only tool output must NOT be preserved as structured parts,
// because the translator would forward it as text; stringify keeps the
// estimate/reality alignment.
func TestResponsesToolOutputOutputImageStringifies(t *testing.T) {
	dataURL := "data:image/png;base64," + makePNGBase64(t, 32, 32)
	out := toolOutputContent([]interface{}{
		map[string]interface{}{"type": "output_image", "image_url": map[string]interface{}{"url": dataURL}},
	})
	if _, ok := out.([]interface{}); ok {
		t.Fatalf("output_image should not be preserved as structured parts, got %T", out)
	}
	if _, ok := out.(string); !ok {
		t.Fatalf("output_image-only output should stringify, got %T (%v)", out, out)
	}
}
