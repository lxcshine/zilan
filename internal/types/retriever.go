package types

// RetrieverEngineType represents the type of retriever engine
type RetrieverEngineType string

// RetrieverEngineType constants
const (
	PostgresRetrieverEngineType        RetrieverEngineType = "postgres"
	ElasticsearchRetrieverEngineType   RetrieverEngineType = "elasticsearch"
	InfinityRetrieverEngineType        RetrieverEngineType = "infinity"
	ElasticFaissRetrieverEngineType    RetrieverEngineType = "elasticfaiss"
	QdrantRetrieverEngineType          RetrieverEngineType = "qdrant"
	MilvusRetrieverEngineType          RetrieverEngineType = "milvus"
	WeaviateRetrieverEngineType        RetrieverEngineType = "weaviate"
	DorisRetrieverEngineType           RetrieverEngineType = "doris"
	SQLiteRetrieverEngineType          RetrieverEngineType = "sqlite"
	TencentVectorDBRetrieverEngineType RetrieverEngineType = "tencent_vectordb"
	// OpenSearchRetrieverEngineType identifies the OpenSearch k-NN driver
	// introduced in Phase 3 (see issue tracker). The driver itself ships
	// in a subsequent PR; this constant exists so that
	// EngineAwareNormalizer can register the OpenSearch case ahead of the
	// driver landing and the AuditAction constants can reference it
	// without forward dependencies. Wire value "opensearch" mirrors the
	// official product name and matches the value used in
	// retrieverEngineMapping / GetVectorStoreTypes once activation lands.
	OpenSearchRetrieverEngineType RetrieverEngineType = "opensearch"
)

// RetrieverType represents the type of retriever
type RetrieverType string

// RetrieverType constants
const (
	KeywordsRetrieverType  RetrieverType = "keywords"  // Keywords retriever
	VectorRetrieverType    RetrieverType = "vector"    // Vector retriever
	WebSearchRetrieverType RetrieverType = "websearch" // Web search retriever
)

// RetrieveParams represents the parameters for retrieval
type RetrieveParams struct {
	// Query text
	Query string
	// Query embedding (used for vector retrieval)
	Embedding []float32
	// Knowledge base IDs
	KnowledgeBaseIDs []string
	// Knowledge IDs
	KnowledgeIDs []string
	// Tag IDs for filtering (used for FAQ priority filtering)
	TagIDs []string
	// Excluded knowledge IDs
	ExcludeKnowledgeIDs []string
	// Excluded chunk IDs
	ExcludeChunkIDs []string
	// Number of results to return
	TopK int
	// Similarity threshold
	Threshold float64
	// Knowledge type (e.g., "faq", "manual") - determines which index to use
	KnowledgeType string
	// Additional parameters, different retrievers may require different parameters
	AdditionalParams map[string]interface{}
	// Retriever type
	RetrieverType RetrieverType // Retriever type
}

// RetrieverEngineParams represents the parameters for retriever engine
type RetrieverEngineParams struct {
	// Retriever engine type
	RetrieverEngineType RetrieverEngineType `yaml:"retriever_engine_type" json:"retriever_engine_type"`
	// Retriever type
	RetrieverType RetrieverType `yaml:"retriever_type"        json:"retriever_type"`
}

// IndexWithScore represents the index with score
type IndexWithScore struct {
	// ID
	ID string
	// Content
	Content string
	// Source ID
	SourceID string
	// Source type
	SourceType SourceType
	// Chunk ID
	ChunkID string
	// Knowledge ID
	KnowledgeID string
	// Knowledge base ID
	KnowledgeBaseID string
	// Tag ID
	TagID string
	// Score
	Score float64
	// VectorScore 向量腿原始相似度（FR-17）：向量检索命中行的原始向量分。
	// 图片命中行被文本重排器拒绝后凭该分保留（KeptByImageVector）。
	VectorScore float64
	// Match type
	MatchType MatchType
	// IsEnabled
	IsEnabled bool
}

// GetScore returns the score for ScoreComparable interface
func (i *IndexWithScore) GetScore() float64 {
	return i.Score
}

// RetrieveResult represents the result of retrieval
type RetrieveResult struct {
	Results             []*IndexWithScore   // Retrieval results
	RetrieverEngineType RetrieverEngineType // Retrieval source type
	RetrieverType       RetrieverType       // Retrieval type
	// Error 上报本结果集的“部分失败”：Results 是成功应答的部分，Error 说明
	// 哪一部分没有应答。完整检索（包括真的零命中）时为 nil。整库级别的失败
	// 仍然作为调用的 error 返回、且不带结果集。
	// CompositeRetrieveEngine 会把非 nil 的 Error 连同结果一起向上抛，因此
	// 读取结果的调用方不得忽略这个 error——至少要记录“本次答案不完整”。
	Error error // Retrieval error
}
