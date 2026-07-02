package proxy

import (
	"regexp"
	"strings"

	"kiro-go/config"
	"kiro-go/logger"
)

// ==================== Response Content Replacement ====================
//
// 背景：某些场景需要在【返回给客户端的可见回答文本】里做查找替换（品牌词替换、
//       脱敏、措辞改写等）。规则来自 admin「过滤」面板的 responseReplaceRules，
//       仅作用于 Claude (/v1/messages) 路径的可见正文——thinking 推理链与
//       tool_use 参数一律豁免（与 stop_sequences 的口径一致）。
//
// 非流式：整段正文一次性跑规则即可（applyResponseReplacements）。
//
// 流式：正文是边收边发的，一次匹配可能被上游分帧切成两半（如 "foob" | "ar" 之于
//       "foobar"）。为此过滤器按【字节】滚动扫描而非按 event 个数：每条规则声明一个
//       MaxLen（该规则单次匹配可能达到的最大字节数），过滤器在帧边界扣留所有规则里
//       最大的 MaxLen 个尾字节，保证任何一次匹配都不会被切断；已经"落定"（不可能再被
//       后续输入影响）的前缀替换后立刻下发。流结束时 flush 剩余扣留内容。
//
// 注意：MaxLen 必须是该规则匹配长度的真实上限。含无限量词（.* 等）的正则无法给出有限
//       上限，不适合流式；此实现假定规则均为有界长度（用户在面板按规则填写 MaxLen）。

// responseReplaceRule is a compiled response-content replacement rule.
type responseReplaceRule struct {
	name    string
	literal string         // set when !isRegex
	re      *regexp.Regexp // set when isRegex && compiled ok
	replace string
	maxLen  int // upper bound (bytes) on a single match of this rule
}

// compileResponseReplaceRules turns the config rules into ready-to-run rules,
// dropping disabled/empty/uncompilable ones. Returns nil when nothing applies so
// callers can cheaply short-circuit. maxLen defaults to the literal match length
// when unset; a regex rule with no positive maxLen falls back to len(match) and
// logs a warning (a too-small bound can miss a match split exactly across a
// frame boundary, so operators should set it explicitly for regex rules).
func compileResponseReplaceRules(rules []config.ResponseReplaceRule) []responseReplaceRule {
	var out []responseReplaceRule
	for _, r := range rules {
		if !r.Enabled || r.Match == "" {
			continue
		}
		cr := responseReplaceRule{name: r.Name, replace: r.Replace, maxLen: r.MaxLen}
		if r.IsRegex {
			re, err := regexp.Compile(r.Match)
			if err != nil {
				logger.Warnf("[response-replace] rule %q: invalid regex %q, skipped: %v", r.Name, r.Match, err)
				continue
			}
			cr.re = re
			if cr.maxLen <= 0 {
				cr.maxLen = len(r.Match)
				logger.Warnf("[response-replace] rule %q: regex rule has no maxLen, defaulting to %d bytes; set maxLen to this rule's true max match length to stay stream-safe", r.Name, cr.maxLen)
			}
		} else {
			cr.literal = r.Match
			if cr.maxLen < len(r.Match) {
				cr.maxLen = len(r.Match)
			}
		}
		out = append(out, cr)
	}
	return out
}

// applyOne applies a single compiled rule to s.
func (r *responseReplaceRule) applyOne(s string) string {
	if r.re != nil {
		return r.re.ReplaceAllString(s, r.replace)
	}
	if r.literal == "" {
		return s
	}
	return strings.ReplaceAll(s, r.literal, r.replace)
}

// applyResponseReplacements runs every rule, in list order, over s. This is the
// single source of truth shared by the non-streaming path and the streaming
// filter, so both produce identical output.
func applyResponseReplacements(rules []responseReplaceRule, s string) string {
	for i := range rules {
		s = rules[i].applyOne(s)
	}
	return s
}

// responseReplaceFilter holds cross-frame state for streaming replacement.
type responseReplaceFilter struct {
	rules   []responseReplaceRule
	carry   string
	maxHold int
}

// newResponseReplaceFilter builds a streaming filter from the current config.
// Returns an inert filter (feed is pass-through) when no rules apply.
func newResponseReplaceFilter() *responseReplaceFilter {
	fc := config.GetFilterConfig()
	rules := compileResponseReplaceRules(fc.ResponseReplaceRules)
	f := &responseReplaceFilter{rules: rules}
	for i := range rules {
		if rules[i].maxLen > f.maxHold {
			f.maxHold = rules[i].maxLen
		}
	}
	return f
}

// active reports whether the filter has any rule to enforce.
func (f *responseReplaceFilter) active() bool {
	return f != nil && len(f.rules) > 0
}

// feed appends delta to the carry buffer and returns the replaced text that is
// safe to emit now — i.e. text that no future input can retroactively change.
//
// It holds back the last maxHold bytes (the largest single-match upper bound
// across all rules) so a match split across frames is never cut. The provisional
// cut point len(carry)-maxHold is then pulled back past any raw match that
// straddles it, guaranteeing applyResponseReplacements over the emitted prefix
// equals what a whole-buffer pass would produce for that prefix.
func (f *responseReplaceFilter) feed(delta string) string {
	if !f.active() {
		return delta
	}
	f.carry += delta

	if len(f.carry) <= f.maxHold {
		return ""
	}
	cut := len(f.carry) - f.maxHold
	cut = f.pullBackStraddlingMatches(cut)
	if cut <= 0 {
		return ""
	}

	emit := applyResponseReplacements(f.rules, f.carry[:cut])
	f.carry = f.carry[cut:]
	return emit
}

// flush replaces and returns whatever is left in the carry at stream end.
func (f *responseReplaceFilter) flush() string {
	if !f.active() || f.carry == "" {
		return ""
	}
	out := applyResponseReplacements(f.rules, f.carry)
	f.carry = ""
	return out
}

// pullBackStraddlingMatches lowers cut to the start of any raw match (across all
// rules) that begins before cut but ends after it, so no match is split by the
// emit boundary. Iterated to a fixed point since lowering cut can expose an
// earlier straddling match.
func (f *responseReplaceFilter) pullBackStraddlingMatches(cut int) int {
	for {
		newCut := cut
		for i := range f.rules {
			r := &f.rules[i]
			for _, loc := range r.findAll(f.carry) {
				start, end := loc[0], loc[1]
				if start < newCut && end > newCut {
					newCut = start
				}
			}
		}
		if newCut == cut {
			return cut
		}
		cut = newCut
		if cut <= 0 {
			return 0
		}
	}
}

// findAll returns [start,end) byte index pairs for every non-overlapping match
// of the rule in s.
func (r *responseReplaceRule) findAll(s string) [][]int {
	if r.re != nil {
		return r.re.FindAllStringIndex(s, -1)
	}
	if r.literal == "" {
		return nil
	}
	var out [][]int
	from := 0
	for {
		idx := strings.Index(s[from:], r.literal)
		if idx < 0 {
			break
		}
		abs := from + idx
		out = append(out, []int{abs, abs + len(r.literal)})
		from = abs + len(r.literal)
	}
	return out
}
