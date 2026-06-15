package proxy

import "strings"

// ==================== Stop-Sequence Adapter-Side Enforcement ====================
//
// Kiro 的上游 API 不接受 stop 参数，所以客户端传入的 stop_sequences 无法转发给
// 上游生效，只能由本进程在响应流里软实现：边收文本边扫描停止序列，命中即截断流、
// 丢弃其后内容，并把 stop_reason 标成 stop_sequence（回填命中的序列值）。
//
// 该过滤器是跨帧安全的：停止序列可能被流式分帧切断（如某帧以 "\nus" 结尾，是
// "\nuser" 的前缀），所以未完整命中时会扣留缓冲末尾"可能是某序列前缀"的最长后缀，
// 等下一帧补全再判断，避免把半截序列先吐成可见文本。

// stopSequenceFilter scans streamed text for any of the configured stop
// sequences, holding back partial-match tails across frame boundaries.
type stopSequenceFilter struct {
	sequences []string
	pending   string
	stopped   bool
	matched   string
}

// newStopSequenceFilter builds a filter for the given sequences. Empty entries
// are dropped. A filter with no usable sequences is inert (feed is pass-through).
func newStopSequenceFilter(seqs []string) *stopSequenceFilter {
	f := &stopSequenceFilter{}
	for _, s := range seqs {
		if s != "" {
			f.sequences = append(f.sequences, s)
		}
	}
	return f
}

// active reports whether the filter has any sequences to enforce.
func (f *stopSequenceFilter) active() bool {
	return f != nil && len(f.sequences) > 0
}

// feed appends delta to the pending buffer and returns the text that is safe to
// emit (cannot be part of a future stop-sequence match). When a sequence is
// fully matched it records the match, sets stopped, emits everything before the
// match, and discards the rest — subsequent feeds return "".
func (f *stopSequenceFilter) feed(delta string) string {
	if f.stopped {
		return ""
	}
	if !f.active() {
		return delta
	}

	f.pending += delta

	// Find the earliest full match across the whole pending buffer.
	earliest := -1
	for _, s := range f.sequences {
		if idx := strings.Index(f.pending, s); idx != -1 {
			if earliest == -1 || idx < earliest {
				earliest = idx
				f.matched = s
			}
		}
	}

	if earliest != -1 {
		out := f.pending[:earliest]
		f.pending = ""
		f.stopped = true
		return out
	}

	// No full match: hold back the longest suffix of pending that could be the
	// prefix of any stop sequence, emit the rest.
	hold := 0
	for _, s := range f.sequences {
		maxK := len(s) - 1
		if len(f.pending) < maxK {
			maxK = len(f.pending)
		}
		for k := maxK; k >= 1; k-- {
			if strings.HasSuffix(f.pending, s[:k]) {
				if k > hold {
					hold = k
				}
				break
			}
		}
	}

	emitEnd := len(f.pending) - hold
	out := f.pending[:emitEnd]
	f.pending = f.pending[emitEnd:]
	return out
}

// flush returns any held-back pending text at stream end. When the stream ended
// without matching a stop sequence, the held-back tail was real text (a partial
// match that never completed) and must not be dropped. After a match, pending is
// already empty so flush returns "".
func (f *stopSequenceFilter) flush() string {
	if f.stopped {
		return ""
	}
	out := f.pending
	f.pending = ""
	return out
}
