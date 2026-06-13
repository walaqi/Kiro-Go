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

// countTokens returns the BPE token count of text using the cl100k_base
// tiktoken encoder, falling back to the character-class heuristic when the
// encoder failed to initialize. It is the shared core for both input and output
// token estimation so the whole usage pipeline keeps a single tokenizer.
func countTokens(text string) int {
	if text == "" {
		return 0
	}
	tkeOnce.Do(initTokenizer)
	if tke == nil {
		return estimateApproxTokens(text)
	}
	return len(tke.Encode(text, nil, nil))
}

// countOutputTokens is retained as the output-side name; it delegates to
// countTokens so input and output share one tokenizer implementation.
func countOutputTokens(text string) int {
	return countTokens(text)
}
