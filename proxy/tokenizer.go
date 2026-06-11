package proxy

import (
	"sync"

	tiktoken "github.com/pkoukk/tiktoken-go"
	tikloader "github.com/pkoukk/tiktoken-go-loader"

	"kiro-go/logger"
)

var (
	tke     *tiktoken.Tiktoken
	tkeOnce sync.Once
)

func initTokenizer() {
	tiktoken.SetBpeLoader(tikloader.NewOfflineLoader())
	enc, err := tiktoken.GetEncoding("cl100k_base")
	if err != nil {
		logger.Warnf("[Tokenizer] Failed to load cl100k_base encoding: %v; falling back to char-class estimator", err)
		return
	}
	tke = enc
}

// countOutputTokens counts tokens using cl100k_base BPE when available.
// Falls back to the char-class heuristic if the encoding fails to load.
func countOutputTokens(text string) int {
	if text == "" {
		return 0
	}
	tkeOnce.Do(initTokenizer)
	if tke == nil {
		return estimateApproxTokens(text)
	}
	return len(tke.Encode(text, nil, nil))
}
