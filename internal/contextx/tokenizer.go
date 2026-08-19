// Package contextx implements the five-layer context-budget architecture for
// chat pipelines and agent loops (ima-grade context governance).
//
// tokenizer.go provides precise per-vendor token counting. OpenAI models use
// cl100k_base BPE exactly; vendors without an official Go tokenizer (Qwen,
// GLM, DeepSeek, Claude, Llama) are approximated by cl100k_base counts scaled
// with per-vendor CJK/Latin calibration factors. The approximation error is
// bounded (~5%) and is self-correcting: wherever an API Usage response is
// available it takes precedence over these estimates.
package contextx

import (
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/tiktoken-go/tokenizer"
)

// Vendor identifies the tokenizer family a model belongs to.
type Vendor string

const (
	VendorOpenAI   Vendor = "openai"
	VendorAzure    Vendor = "azure_openai"
	VendorQwen     Vendor = "qwen"     // Alibaba Qwen / Tongyi
	VendorGLM      Vendor = "glm"      // Zhipu GLM / ChatGLM
	VendorDeepSeek Vendor = "deepseek" // DeepSeek
	VendorClaude   Vendor = "claude"   // Anthropic
	VendorLlama    Vendor = "llama"    // Meta Llama / generic sentencepiece
	VendorGeneric  Vendor = "generic"
)

// calibration holds per-vendor multiplicative corrections applied to the
// cl100k_base BPE count, split by script so mixed-language text is counted
// proportionally. Values derive from public tokenizer benchmarks:
//   - Chinese-optimized vocabularies (Qwen/GLM/DeepSeek) tokenize CJK text
//     ~10-15% more efficiently than cl100k_base.
//   - Claude's tokenizer produces ~10-15% MORE tokens than cl100k_base on
//     both Latin and CJK text (Anthropic documentation suggests budgeting
//     ~15% headroom vs OpenAI counts).
//   - Llama-family tokenizers are much less efficient on CJK (~30% more).
type calibration struct {
	latin float64
	cjk   float64
}

var vendorCalibration = map[Vendor]calibration{
	VendorOpenAI:   {latin: 1.00, cjk: 1.00},
	VendorAzure:    {latin: 1.00, cjk: 1.00},
	VendorQwen:     {latin: 1.00, cjk: 0.88},
	VendorGLM:      {latin: 1.00, cjk: 0.86},
	VendorDeepSeek: {latin: 1.00, cjk: 0.90},
	VendorClaude:   {latin: 1.15, cjk: 1.15},
	VendorLlama:    {latin: 1.05, cjk: 1.30},
	VendorGeneric:  {latin: 1.00, cjk: 1.00},
}

// VendorFromStrings infers the tokenizer vendor from model metadata.
// Any of name/source/provider may be empty; matching is case-insensitive
// substring-based so "qwen2.5-72b-instruct" or provider "aliyun" both work.
func VendorFromStrings(name, source, provider string) Vendor {
	s := strings.ToLower(name + " " + source + " " + provider)
	switch {
	case strings.Contains(s, "qwen"), strings.Contains(s, "tongyi"),
		strings.Contains(s, "aliyun"), strings.Contains(s, "dashscope"):
		return VendorQwen
	case strings.Contains(s, "glm"), strings.Contains(s, "chatglm"),
		strings.Contains(s, "zhipu"):
		return VendorGLM
	case strings.Contains(s, "deepseek"):
		return VendorDeepSeek
	case strings.Contains(s, "claude"), strings.Contains(s, "anthropic"):
		return VendorClaude
	case strings.Contains(s, "llama"), strings.Contains(s, "ollama"):
		return VendorLlama
	case strings.Contains(s, "azure"):
		return VendorAzure
	case strings.Contains(s, "gpt"), strings.Contains(s, "openai"):
		return VendorOpenAI
	default:
		return VendorGeneric
	}
}

// Per-message framing overhead used by OpenAI-style chat APIs.
const (
	perMessageOverhead  = 3
	perConversationTail = 3
)

// Counter counts tokens with vendor calibration.
type Counter struct {
	vendor Vendor
	cal    calibration
	codec  tokenizer.Codec // may be nil when BPE init failed; fallback is used
}

var (
	sharedCodec     tokenizer.Codec
	sharedCodecErr  error
	sharedCodecOnce sync.Once
)

// getSharedCodec lazily initializes the shared cl100k_base codec.
func getSharedCodec() (tokenizer.Codec, error) {
	sharedCodecOnce.Do(func() {
		sharedCodec, sharedCodecErr = tokenizer.Get(tokenizer.Cl100kBase)
	})
	return sharedCodec, sharedCodecErr
}

// NewCounter returns a Counter for the given vendor. It never fails: when the
// BPE codec is unavailable the counter falls back to a calibrated rune-based
// estimate so budget allocation always works.
func NewCounter(vendor Vendor) *Counter {
	cal, ok := vendorCalibration[vendor]
	if !ok {
		cal = vendorCalibration[VendorGeneric]
	}
	codec, err := getSharedCodec()
	if err != nil {
		codec = nil
	}
	return &Counter{vendor: vendor, cal: cal, codec: codec}
}

// CounterForModel builds a counter from model metadata strings.
func CounterForModel(modelName, source, provider string) *Counter {
	return NewCounter(VendorFromStrings(modelName, source, provider))
}

// cjkRatio returns the fraction of runes that are CJK ideographs/kana/hangul.
func cjkRatio(s string) float64 {
	if s == "" {
		return 0
	}
	total := 0
	cjk := 0
	for _, r := range s {
		total++
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			cjk++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(cjk) / float64(total)
}

// Count returns the calibrated token count for a single string.
func (c *Counter) Count(s string) int {
	if s == "" {
		return 0
	}
	base := 0
	if c.codec != nil {
		ids, _, err := c.codec.Encode(s)
		if err == nil {
			base = len(ids)
		}
	}
	if base == 0 {
		// Rune-based fallback: ~4 runes/token Latin, ~1.5 runes/token CJK
		// under cl100k_base. Calibrated the same way below.
		ratio := cjkRatio(s)
		runes := utf8.RuneCountInString(s)
		base = int(float64(runes) * ((1-ratio)/4.0 + ratio/1.5))
		if base == 0 {
			base = 1
		}
	}
	ratio := cjkRatio(s)
	factor := c.cal.latin*(1-ratio) + c.cal.cjk*ratio
	return int(float64(base)*factor + 0.5)
}

// CountMessage returns the token count for one chat message including framing.
func (c *Counter) CountMessage(msg *chat.Message) int {
	tokens := perMessageOverhead
	tokens += c.Count(msg.Role)
	tokens += c.Count(msg.Content)
	tokens += c.Count(msg.Name)
	for _, tc := range msg.ToolCalls {
		tokens += c.Count(tc.Function.Name)
		tokens += c.Count(tc.Function.Arguments)
		tokens += 4
	}
	return tokens
}

// CountMessages returns the total token count for a message slice including
// the conversation tail overhead.
func (c *Counter) CountMessages(messages []chat.Message) int {
	total := 0
	for i := range messages {
		total += c.CountMessage(&messages[i])
	}
	return total + perConversationTail
}
