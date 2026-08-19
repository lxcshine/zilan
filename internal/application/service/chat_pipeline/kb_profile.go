package chatpipeline

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// kbProfileStats holds the structural statistics collected from a sample of
// chunks inside one knowledge base. These statistics drive the automatic
// document-class classification.
type kbProfileStats struct {
	// Total sampled chunks.
	SampleCount int
	// Average rune length of chunk content.
	AvgContentLen float64
	// Ratio of chunks that contain at least one markdown heading marker.
	HeadingDensity float64
	// Ratio of chunks whose content contains table-like pipe syntax.
	TableRatio float64
	// Ratio of chunks that look like FAQ Q/A pairs (short question + answer).
	FAQRatio float64
	// Ratio of chunks containing legal/regulatory markers (条款/第X条/第X章).
	RegulationRatio float64
}

// profileKnowledgeBase samples chunks from the given knowledge base and
// classifies it into one of the KBClass* categories. The result is persisted
// back to the knowledge_bases.document_class column.
//
// The profiler is intentionally lightweight: it samples up to maxSample
// chunks, computes structural statistics, and applies a rule-based classifier.
// It runs lazily from ROUTE_RETRIEVAL when a KB has no cached class.
func profileKnowledgeBase(
	ctx context.Context,
	kbService interfaces.KnowledgeBaseService,
	knowledgeService interfaces.KnowledgeService,
	chunkRepo interfaces.ChunkRepository,
	kbID string,
	tenantID uint64,
) (string, error) {
	kb, err := kbService.GetKnowledgeBaseByIDOnly(ctx, kbID)
	if err != nil || kb == nil {
		return "", err
	}
	if kb.DocumentClass != "" {
		return kb.DocumentClass, nil
	}

	// Load a sample of knowledge entries, then sample chunks from each.
	knowledges, err := knowledgeService.ListKnowledgeByKnowledgeBaseID(ctx, kbID)
	if err != nil || len(knowledges) == 0 {
		return "", err
	}

	const maxSample = 200
	var sampled []*types.Chunk
	for _, k := range knowledges {
		if len(sampled) >= maxSample {
			break
		}
		chunks, err := chunkRepo.ListChunksByKnowledgeID(ctx, tenantID, k.ID)
		if err != nil {
			continue
		}
		// Take up to 20 chunks per document to keep the sample balanced.
		limit := 20
		if len(chunks) < limit {
			limit = len(chunks)
		}
		sampled = append(sampled, chunks[:limit]...)
	}

	if len(sampled) == 0 {
		return types.KBClassGeneral, nil
	}

	stats := computeKBProfileStats(sampled)
	class := classifyKB(stats)

	// Persist the classification so future requests skip the profiling cost.
	kb.DocumentClass = class
	if err := kbService.GetRepository().UpdateKnowledgeBase(ctx, kb); err != nil {
		pipelineWarn(ctx, "KBProfile", "persist_failed", map[string]interface{}{
			"kb_id": kbID,
			"error": err.Error(),
		})
	}

	pipelineInfo(ctx, "KBProfile", "classified", map[string]interface{}{
		"kb_id":            kbID,
		"document_class":   class,
		"sample_count":     stats.SampleCount,
		"heading_density":  stats.HeadingDensity,
		"table_ratio":      stats.TableRatio,
		"faq_ratio":        stats.FAQRatio,
		"regulation_ratio": stats.RegulationRatio,
	})
	return class, nil
}

// computeKBProfileStats extracts structural statistics from a chunk sample.
func computeKBProfileStats(chunks []*types.Chunk) kbProfileStats {
	if len(chunks) == 0 {
		return kbProfileStats{}
	}
	stats := kbProfileStats{SampleCount: len(chunks)}
	var totalLen int
	var headingCount, tableCount, faqCount, regulationCount int

	for _, c := range chunks {
		content := strings.TrimSpace(c.Content)
		if content == "" {
			continue
		}
		totalLen += utf8.RuneCountInString(content)

		// Heading detection: markdown headings or numbered section headers.
		if strings.Contains(content, "#") || strings.Contains(content, "第") &&
			(strings.Contains(content, "章") || strings.Contains(content, "节")) {
			headingCount++
		}
		// Table detection: markdown table syntax or dense pipe separators.
		if strings.Contains(content, "|") && strings.Count(content, "|") >= 4 {
			tableCount++
		}
		// FAQ detection: short question followed by answer pattern.
		if isFAQChunk(content) {
			faqCount++
		}
		// Regulation detection: legal article markers.
		if strings.Contains(content, "第") && strings.Contains(content, "条") {
			regulationCount++
		}
	}

	stats.AvgContentLen = float64(totalLen) / float64(len(chunks))
	stats.HeadingDensity = float64(headingCount) / float64(len(chunks))
	stats.TableRatio = float64(tableCount) / float64(len(chunks))
	stats.FAQRatio = float64(faqCount) / float64(len(chunks))
	stats.RegulationRatio = float64(regulationCount) / float64(len(chunks))
	return stats
}

// isFAQChunk reports whether the chunk content looks like a FAQ Q/A pair.
func isFAQChunk(content string) bool {
	// Very short chunks are likely questions; medium chunks with question
	// marks and answer-like structure also qualify.
	if utf8.RuneCountInString(content) < 80 {
		return strings.Contains(content, "?") || strings.Contains(content, "？")
	}
	// Look for explicit Q/A markers.
	lower := strings.ToLower(content)
	return strings.HasPrefix(lower, "q:") || strings.HasPrefix(lower, "a:") ||
		strings.Contains(content, "问:") || strings.Contains(content, "答:")
}

// classifyKB maps profile statistics to a document class using a rule-based
// decision tree. The rules are ordered by specificity: regulation > FAQ >
// paper > manual > general.
func classifyKB(stats kbProfileStats) string {
	// Regulation: strong legal markers + headings + long content.
	if stats.RegulationRatio > 0.15 && stats.HeadingDensity > 0.25 && stats.AvgContentLen > 300 {
		return types.KBClassRegulation
	}
	// FAQ: high FAQ ratio + short average length.
	if stats.FAQRatio > 0.35 && stats.AvgContentLen < 250 {
		return types.KBClassFAQ
	}
	// Paper: dense headings + long content + tables (references, figures).
	if stats.HeadingDensity > 0.30 && stats.AvgContentLen > 400 && stats.TableRatio > 0.08 {
		return types.KBClassPaper
	}
	// Manual: moderate headings + medium length + some tables.
	if stats.HeadingDensity > 0.15 && stats.AvgContentLen > 200 {
		return types.KBClassManual
	}
	return types.KBClassGeneral
}
