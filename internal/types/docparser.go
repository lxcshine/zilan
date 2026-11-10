package types

import "strings"

// ReadRequest is the unified transport-agnostic request for document reading.
// Set FileContent for file mode, URL for URL mode.
type ReadRequest struct {
	FileContent           []byte
	FileName              string
	FileType              string
	URL                   string
	Title                 string
	ParserEngine          string
	RequestID             string
	ParserEngineOverrides map[string]string
}

// ReadResult is the transport-agnostic result of document reading.
type ReadResult struct {
	MarkdownContent string
	ImageRefs       []ImageRef
	ImageDirPath    string
	Metadata        map[string]string
	Error           string
	IsAudio         bool   // true when the result contains raw audio data needing ASR transcription
	AudioData       []byte // raw audio bytes for ASR processing
	// SourceBlocks 由解析器上报：Markdown 输出的 rune 区间 → 原文位置（页/行/表格行等），
	// 用于引用溯源定位（FR-05）。best-effort：为空时链路照常工作。
	SourceBlocks []SourceBlock
	// ParseIncomplete 表示解析在触及资源上界（时长 / 内存）或被取消时中止，
	// MarkdownContent 与 ImageRefs 只是**已解析出来的部分**（FR-16）。
	// 这种降级必须显式告知用户，绝不能当成完整结果静默入库（PRD §5.3 规则 3）。
	ParseIncomplete bool
	// ParseIncompleteReason 取值 "timeout" | "memory" | "cancelled"。
	ParseIncompleteReason string
	// PagesDone / PagesTotal 是解析器上报的页级进度；PagesTotal 为 0 表示未知。
	PagesDone  int
	PagesTotal int
}

// FR-16：ParseIncompleteReason 的取值，与 docreader 侧字符串严格一致。
const (
	ParseIncompleteTimeout   = "timeout"
	ParseIncompleteMemory    = "memory"
	ParseIncompleteCancelled = "cancelled"
)

// ParseDeadlineOverrideKey 是下发给 docreader 的「解析截止时刻」（Unix 秒）。
// Go 侧按调用 ctx 的 deadline 计算后经 parser_engine_overrides 传入，Python 侧
// 在逐页渲染循环内检查：超限即中止产出、保留已解析部分并回传 incomplete 标记
// （FR-16 与 FR-07 时长上界的协同点）。它是纯控制信息，解析器不会把它当引擎参数。
const ParseDeadlineOverrideKey = "parse_deadline_unix"

// ImageRef represents an image reference extracted from the document.
type ImageRef struct {
	Filename    string
	OriginalRef string
	MimeType    string
	StorageKey  string
	ImageData   []byte // inline image bytes (universal fallback for cross-machine deployments)
	// IsOriginal marks references that point to the originally uploaded file
	// itself (e.g. when the user uploads a standalone image). Such references
	// must not be dropped by the icon/size filter — otherwise a small image
	// upload would be silently discarded before multimodal processing.
	IsOriginal bool
}

// ParserEngineInfo describes a registered parser engine.
type ParserEngineInfo struct {
	Name              string
	Description       string
	FileTypes         []string
	Available         bool
	UnavailableReason string
}

// --- Internal types used by chunking pipeline ---

type DocParserStorageConfig struct {
	Provider        string
	Region          string
	BucketName      string
	AccessKeyID     string
	SecretAccessKey string
	AppID           string
	PathPrefix      string
	Endpoint        string
}

type DocParserVLMConfig struct {
	ModelName     string
	BaseURL       string
	APIKey        string
	InterfaceType string
}

type ParsedChunk struct {
	Content string
	// ContextHeader is an optional context string (e.g. a Markdown heading
	// breadcrumb) that should be prepended at embedding time but is NOT
	// part of the stored Content. Lets retrieval pipelines see section
	// context without breaking End-Start == len(Content) invariants.
	ContextHeader string
	Seq           int
	Start         int
	End           int
	Images        []ParsedImage
	ChunkID       string // populated by processChunks with the actual DB UUID

	// ParentIndex is set when using parent-child chunking strategy.
	// -1 (or unset/0 for flat chunks) means this is a top-level chunk.
	// >= 0 means this is a child chunk referencing the parent at this index
	// in the ParentChunks slice of ProcessChunksOptions.
	ParentIndex int

	// SourceLocators 由 processChunks 依据解析器定位块计算：分块文本在原始文件中的位置。
	SourceLocators SourceLocators
}

// EmbeddingContent returns the text that should be sent to the embedding
// model: ContextHeader (if any) prepended to Content. Mirrors
// chunker.Chunk.EmbeddingContent so the choice is consistent across the
// chunker output and the indexing pipeline. Surrounding whitespace on
// Content is trimmed so leading/trailing newlines from boundary slicing
// don't dilute the embedded vector.
func (c ParsedChunk) EmbeddingContent() string {
	body := strings.TrimSpace(c.Content)
	if c.ContextHeader == "" {
		return body
	}
	return c.ContextHeader + "\n\n" + body
}

// ParsedParentChunk represents a parent chunk in the parent-child strategy.
// Parent chunks are stored in DB for context retrieval but NOT vector-indexed.
type ParsedParentChunk struct {
	Content string
	Seq     int
	Start   int
	End     int
	// SourceLocators 由 processChunks 依据解析器定位块计算：父块文本在原始文件中的位置。
	SourceLocators SourceLocators
}

type ParsedImage struct {
	URL         string
	Caption     string
	OCRText     string
	OriginalURL string
	Start       int
	End         int
}
