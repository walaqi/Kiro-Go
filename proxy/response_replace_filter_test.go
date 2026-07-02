package proxy

import (
	"strings"
	"testing"

	"kiro-go/config"
)

// streamAll feeds s to the filter one byte at a time and returns the full
// concatenated output (feeds + flush). One-byte chunks are the worst case for
// cross-frame splitting.
func streamAll(f *responseReplaceFilter, s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		b.WriteString(f.feed(s[i : i+1]))
	}
	b.WriteString(f.flush())
	return b.String()
}

// streamChunks feeds s in fixed-size chunks.
func streamChunks(f *responseReplaceFilter, s string, size int) string {
	var b strings.Builder
	for i := 0; i < len(s); i += size {
		end := i + size
		if end > len(s) {
			end = len(s)
		}
		b.WriteString(f.feed(s[i:end]))
	}
	b.WriteString(f.flush())
	return b.String()
}

func newFilterFromRules(rules []responseReplaceRule) *responseReplaceFilter {
	f := &responseReplaceFilter{rules: rules}
	for i := range rules {
		if rules[i].maxLen > f.maxHold {
			f.maxHold = rules[i].maxLen
		}
	}
	return f
}

func TestResponseReplace_LiteralWholeString(t *testing.T) {
	rules := compileResponseReplaceRules([]config.ResponseReplaceRule{
		{Enabled: true, Match: "foo", Replace: "bar"},
	})
	got := applyResponseReplacements(rules, "a foo b foo c")
	want := "a bar b bar c"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// The core property: streaming (any chunking) == single whole-string pass.
func TestResponseReplace_StreamingEqualsWholePass(t *testing.T) {
	cfgRules := []config.ResponseReplaceRule{
		{Enabled: true, Match: "foobar", Replace: "X"},
		{Enabled: true, Match: "hello", Replace: "hi"},
		{Enabled: true, IsRegex: true, Match: `\d{3}`, Replace: "###", MaxLen: 3},
	}
	inputs := []string{
		"foobar",
		"say hello to foobar and 123 then hello",
		"nomatch here at all",
		"foofoobarbar",       // overlapping-ish literal
		"hellohello foobar",  // adjacent
		"12 123 1234 foobar", // regex digit runs
		"foobarfoobarfoobar",
		"a" + strings.Repeat("foobar", 5) + "z",
	}
	for _, in := range inputs {
		rules := compileResponseReplaceRules(cfgRules)
		want := applyResponseReplacements(rules, in)

		// byte-by-byte
		if got := streamAll(newFilterFromRules(compileResponseReplaceRules(cfgRules)), in); got != want {
			t.Errorf("byte-stream %q:\n got %q\nwant %q", in, got, want)
		}
		// every fixed chunk size
		for size := 1; size <= len(in)+1 && size <= 8; size++ {
			f := newFilterFromRules(compileResponseReplaceRules(cfgRules))
			if got := streamChunks(f, in, size); got != want {
				t.Errorf("chunk=%d %q:\n got %q\nwant %q", size, in, got, want)
			}
		}
	}
}

// A literal match split exactly across a frame boundary must still replace.
func TestResponseReplace_SplitMatchAcrossFrames(t *testing.T) {
	rules := compileResponseReplaceRules([]config.ResponseReplaceRule{
		{Enabled: true, Match: "foobar", Replace: "X"},
	})
	f := newFilterFromRules(rules)
	var b strings.Builder
	b.WriteString(f.feed("foob")) // partial — must hold back
	b.WriteString(f.feed("ar"))   // completes the match
	b.WriteString(f.flush())
	if got := b.String(); got != "X" {
		t.Fatalf("split match: got %q want %q", got, "X")
	}
}

// A partial that never completes must be emitted verbatim at flush (not dropped).
func TestResponseReplace_UnfinishedPartialFlushed(t *testing.T) {
	rules := compileResponseReplaceRules([]config.ResponseReplaceRule{
		{Enabled: true, Match: "foobar", Replace: "X"},
	})
	f := newFilterFromRules(rules)
	var b strings.Builder
	b.WriteString(f.feed("foo")) // looks like a prefix of foobar
	b.WriteString(f.feed("baz")) // diverges — never matches
	b.WriteString(f.flush())
	if got := b.String(); got != "foobaz" {
		t.Fatalf("unfinished partial: got %q want %q", got, "foobaz")
	}
}

func TestResponseReplace_EmptyReplaceDeletes(t *testing.T) {
	rules := compileResponseReplaceRules([]config.ResponseReplaceRule{
		{Enabled: true, Match: "secret", Replace: ""},
	})
	f := newFilterFromRules(rules)
	if got := streamAll(f, "a secret b secret c"); got != "a  b  c" {
		t.Fatalf("delete: got %q", got)
	}
}

func TestResponseReplace_InertWhenNoRules(t *testing.T) {
	f := newFilterFromRules(nil)
	if f.active() {
		t.Fatal("expected inert filter")
	}
	if got := streamAll(f, "unchanged text foobar"); got != "unchanged text foobar" {
		t.Fatalf("inert pass-through: got %q", got)
	}
}

func TestResponseReplace_DisabledAndInvalidSkipped(t *testing.T) {
	rules := compileResponseReplaceRules([]config.ResponseReplaceRule{
		{Enabled: false, Match: "foo", Replace: "X"},        // disabled
		{Enabled: true, Match: "", Replace: "X"},            // empty match
		{Enabled: true, IsRegex: true, Match: "(", MaxLen: 5}, // bad regex
		{Enabled: true, Match: "keep", Replace: "K"},         // valid
	})
	if len(rules) != 1 {
		t.Fatalf("expected 1 usable rule, got %d", len(rules))
	}
	if got := applyResponseReplacements(rules, "keep foo"); got != "K foo" {
		t.Fatalf("got %q", got)
	}
}

// Regex whose actual match can exceed a too-small maxLen: within maxLen it is
// still stream-safe. Confirms the bound is honored on the whole-pass path.
func TestResponseReplace_RegexBounded(t *testing.T) {
	rules := compileResponseReplaceRules([]config.ResponseReplaceRule{
		{Enabled: true, IsRegex: true, Match: `\[\[[a-z]+\]\]`, Replace: "LINK", MaxLen: 16},
	})
	in := "see [[foo]] and [[bar]] here"
	want := "see LINK and LINK here"
	if got := applyResponseReplacements(rules, in); got != want {
		t.Fatalf("whole: got %q", got)
	}
	if got := streamAll(newFilterFromRules(rules), in); got != want {
		t.Fatalf("stream: got %q", got)
	}
}

// Two rules where the first's replacement feeds the second is applied in list
// order (documented behavior), consistently between whole-pass and stream.
func TestResponseReplace_ListOrderStreamParity(t *testing.T) {
	cfgRules := []config.ResponseReplaceRule{
		{Enabled: true, Match: "cat", Replace: "dog"},
		{Enabled: true, Match: "dog", Replace: "fish"},
	}
	in := "the cat and the dog"
	whole := applyResponseReplacements(compileResponseReplaceRules(cfgRules), in)
	stream := streamAll(newFilterFromRules(compileResponseReplaceRules(cfgRules)), in)
	if whole != stream {
		t.Fatalf("parity: whole %q != stream %q", whole, stream)
	}
}
