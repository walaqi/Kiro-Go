package proxy

import (
	"strings"
	"testing"
)

// runStopFilter feeds chunks through the filter (one feed per chunk like the
// stream loop), then flushes, returning the concatenated emitted text, whether
// a stop fired, and the matched sequence.
func runStopFilter(seqs []string, chunks []string) (string, bool, string) {
	f := newStopSequenceFilter(seqs)
	var sb strings.Builder
	for _, c := range chunks {
		sb.WriteString(f.feed(c))
	}
	sb.WriteString(f.flush())
	return sb.String(), f.stopped, f.matched
}

func TestStopFilterInertWithoutSequences(t *testing.T) {
	out, stopped, _ := runStopFilter(nil, []string{"hello ", "world"})
	if out != "hello world" {
		t.Fatalf("inert filter must pass text through, got %q", out)
	}
	if stopped {
		t.Fatalf("inert filter must never stop")
	}
}

func TestStopFilterInertWithOnlyEmptySequences(t *testing.T) {
	f := newStopSequenceFilter([]string{"", ""})
	if f.active() {
		t.Fatalf("filter with only empty sequences should be inert")
	}
	if got := f.feed("abc"); got != "abc" {
		t.Fatalf("inert feed should pass through, got %q", got)
	}
}

func TestStopFilterTruncatesAtMatch(t *testing.T) {
	out, stopped, matched := runStopFilter([]string{"STOP"}, []string{"keep this STOP discard this"})
	if out != "keep this " {
		t.Fatalf("expected text before match only, got %q", out)
	}
	if !stopped {
		t.Fatalf("expected stop to fire")
	}
	if matched != "STOP" {
		t.Fatalf("expected matched sequence STOP, got %q", matched)
	}
}

func TestStopFilterCrossFrameSplit(t *testing.T) {
	// The sequence "END" is split across frame boundaries: "...E" | "N" | "D...".
	out, stopped, matched := runStopFilter([]string{"END"}, []string{"abcE", "N", "Dxyz"})
	if out != "abc" {
		t.Fatalf("cross-frame match should emit only text before it, got %q", out)
	}
	if !stopped || matched != "END" {
		t.Fatalf("expected cross-frame stop on END, stopped=%v matched=%q", stopped, matched)
	}
}

func TestStopFilterHoldsBackPartialTailThenReleasesOnFlush(t *testing.T) {
	// "ENORMOUS" shares the prefix "EN" with the stop sequence "END" but never
	// completes it; the held-back tail must be released intact at flush.
	out, stopped, _ := runStopFilter([]string{"END"}, []string{"size is EN", "ORMOUS"})
	if stopped {
		t.Fatalf("partial prefix that never completes must not stop")
	}
	if out != "size is ENORMOUS" {
		t.Fatalf("held-back tail must be released on flush, got %q", out)
	}
}

func TestStopFilterEarliestMatchWins(t *testing.T) {
	out, _, matched := runStopFilter([]string{"BBB", "A"}, []string{"xxAyyBBB"})
	if out != "xx" {
		t.Fatalf("earliest match (A) should truncate first, got %q", out)
	}
	if matched != "A" {
		t.Fatalf("expected earliest match A, got %q", matched)
	}
}

func TestStopFilterSuppressesAfterStop(t *testing.T) {
	f := newStopSequenceFilter([]string{"STOP"})
	first := f.feed("before STOP after")
	if first != "before " {
		t.Fatalf("expected text before stop, got %q", first)
	}
	if got := f.feed("more text"); got != "" {
		t.Fatalf("feeds after stop must return empty, got %q", got)
	}
	if got := f.flush(); got != "" {
		t.Fatalf("flush after stop must return empty, got %q", got)
	}
}

func TestNormalizeOpenAIStopVariants(t *testing.T) {
	if got := normalizeOpenAIStop(nil); got != nil {
		t.Fatalf("nil raw should yield nil, got %#v", got)
	}
	if got := normalizeOpenAIStop([]byte(`"END"`)); len(got) != 1 || got[0] != "END" {
		t.Fatalf("single string stop should yield [END], got %#v", got)
	}
	if got := normalizeOpenAIStop([]byte(`["A","","B"]`)); len(got) != 2 || got[0] != "A" || got[1] != "B" {
		t.Fatalf("array stop should drop empties, got %#v", got)
	}
	if got := normalizeOpenAIStop([]byte(`""`)); got != nil {
		t.Fatalf("empty string stop should yield nil, got %#v", got)
	}
	if got := normalizeOpenAIStop([]byte(`123`)); got != nil {
		t.Fatalf("invalid stop should yield nil, got %#v", got)
	}
}
