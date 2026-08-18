package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/tracing/langfuse"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

// graphCommunityBuildUniqueTTL debounces rebuild triggers: chunk-level graph
// extraction fires once per chunk, but community detection is KB-scoped, so
// bursts collapse into one rebuild per TTL window per KB.
const graphCommunityBuildUniqueTTL = 15 * time.Minute

// communitySummaryPromptTemplate asks the LLM for a title + thematic summary
// of one entity community. Output format is two prefixed lines so parsing is
// robust without JSON mode.
const communitySummaryPromptTemplate = `You are analyzing an entity community extracted from a knowledge graph.

Entities (with attributes):
%s

Relationships:
%s

Please describe this community:
1. Title: a short thematic title (at most 15 words)
2. Summary: 3-5 sentences describing what these entities collectively cover, the key relationships between them, and what kind of questions this community can answer. Write in the same language as the entity names.

Output exactly in this format:
Title: <title>
Summary: <summary>`

// graphCommunityService implements interfaces.GraphCommunityService.
type graphCommunityService struct {
	graphEngine       interfaces.RetrieveGraphRepository
	communityRepo     interfaces.GraphCommunityRepository
	knowledgeBaseRepo interfaces.KnowledgeBaseRepository
	modelService      interfaces.ModelService
	taskEnqueuer      interfaces.TaskEnqueuer
}

// NewGraphCommunityService creates a new graph community service.
func NewGraphCommunityService(
	graphEngine interfaces.RetrieveGraphRepository,
	communityRepo interfaces.GraphCommunityRepository,
	knowledgeBaseRepo interfaces.KnowledgeBaseRepository,
	modelService interfaces.ModelService,
	taskEnqueuer interfaces.TaskEnqueuer,
) interfaces.GraphCommunityService {
	return &graphCommunityService{
		graphEngine:       graphEngine,
		communityRepo:     communityRepo,
		knowledgeBaseRepo: knowledgeBaseRepo,
		modelService:      modelService,
		taskEnqueuer:      taskEnqueuer,
	}
}

func graphRAGEnabled() bool {
	return strings.ToLower(os.Getenv("NEO4J_ENABLE")) == "true"
}

// MaybeEnqueueBuild enqueues a debounced community rebuild for the KB. It is
// the caller's responsibility to only call this for KBs with entity
// extraction enabled; the worker re-validates before doing any work.
func (s *graphCommunityService) MaybeEnqueueBuild(ctx context.Context, tenantID uint64, kbID, trigger string) {
	if !graphRAGEnabled() || kbID == "" || s.taskEnqueuer == nil {
		return
	}
	if err := s.enqueue(ctx, tenantID, kbID, trigger); err != nil {
		// ErrTaskAlreadyExists from the Unique TTL is the debounce working as
		// intended — log at debug, everything else is worth a warning.
		logger.Debugf(ctx, "[GraphCommunity] enqueue rebuild for KB %s skipped: %v", kbID, err)
	}
}

func (s *graphCommunityService) enqueue(ctx context.Context, tenantID uint64, kbID, trigger string) error {
	payload := types.GraphCommunityBuildPayload{
		TenantID:        tenantID,
		KnowledgeBaseID: kbID,
		Trigger:         trigger,
	}
	langfuse.InjectTracing(ctx, &payload)
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	task := asynq.NewTask(types.TypeGraphCommunityBuild, payloadBytes,
		asynq.Queue(types.QueueGraph),
		asynq.MaxRetry(2),
		asynq.Timeout(30*time.Minute),
		asynq.Unique(graphCommunityBuildUniqueTTL),
	)
	_, err = s.taskEnqueuer.Enqueue(task)
	return err
}

// EnqueueRebuild validates the caller-visible KB and enqueues a manual
// rebuild. Returns an error when graph RAG is unavailable for this KB.
func (s *graphCommunityService) EnqueueRebuild(ctx context.Context, kbID string) error {
	if !graphRAGEnabled() {
		return fmt.Errorf("graph RAG is not enabled (NEO4J_ENABLE)")
	}
	tenantID := types.MustTenantIDFromContext(ctx)
	kb, err := s.knowledgeBaseRepo.GetKnowledgeBaseByID(ctx, kbID)
	if err != nil {
		return fmt.Errorf("failed to get knowledge base: %w", err)
	}
	if kb == nil {
		return fmt.Errorf("knowledge base not found")
	}
	if kb.ExtractConfig == nil || !kb.ExtractConfig.Enabled {
		return fmt.Errorf("knowledge base has no entity extraction enabled")
	}
	if err := s.enqueue(ctx, tenantID, kbID, "manual"); err != nil {
		return fmt.Errorf("failed to enqueue community rebuild: %w", err)
	}
	return nil
}

// ListCommunities returns the current community summaries of one KB.
func (s *graphCommunityService) ListCommunities(
	ctx context.Context, kbID string,
) ([]*types.GraphCommunity, error) {
	return s.communityRepo.ListCommunities(ctx, types.MustTenantIDFromContext(ctx), kbID)
}

// ProcessGraphCommunityBuild is the asynq handler for
// types.TypeGraphCommunityBuild: export the KB graph, detect communities,
// summarize each with an LLM, embed, and upsert — then drop stale rows.
func (s *graphCommunityService) ProcessGraphCommunityBuild(ctx context.Context, task *asynq.Task) error {
	var payload types.GraphCommunityBuildPayload
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		logger.Errorf(ctx, "[GraphCommunity] failed to unmarshal payload: %v", err)
		return err
	}
	ctx = logger.WithRequestID(ctx, uuid.New().String())
	ctx = logger.WithField(ctx, "knowledge_base", payload.KnowledgeBaseID)
	ctx = context.WithValue(ctx, types.TenantIDContextKey, payload.TenantID)

	if !graphRAGEnabled() {
		logger.Infof(ctx, "[GraphCommunity] NEO4J disabled, skip community build for KB %s",
			payload.KnowledgeBaseID)
		return nil
	}

	kb, err := s.knowledgeBaseRepo.GetKnowledgeBaseByID(ctx, payload.KnowledgeBaseID)
	if err != nil {
		return fmt.Errorf("failed to get knowledge base: %w", err)
	}
	if kb == nil {
		logger.Warnf(ctx, "[GraphCommunity] KB %s gone, skip community build", payload.KnowledgeBaseID)
		return nil
	}
	if kb.ExtractConfig == nil || !kb.ExtractConfig.Enabled {
		logger.Infof(ctx, "[GraphCommunity] KB %s has no extract config, skip", payload.KnowledgeBaseID)
		return nil
	}

	graph, err := s.graphEngine.GetGraph(ctx, types.NameSpace{KnowledgeBase: payload.KnowledgeBaseID})
	if err != nil {
		return fmt.Errorf("failed to export graph: %w", err)
	}
	if graph == nil || len(graph.Node) == 0 {
		logger.Infof(ctx, "[GraphCommunity] KB %s graph is empty, skip", payload.KnowledgeBaseID)
		return nil
	}

	communities := detectCommunities(graph)
	// Keep only sizeable communities, capped: LLM cost scales with count.
	kept := make([][]string, 0, len(communities))
	for _, members := range communities {
		if len(members) < types.GraphCommunityMinSize {
			continue
		}
		kept = append(kept, members)
		if len(kept) >= types.MaxGraphCommunitiesPerKB {
			break
		}
	}
	if len(kept) == 0 {
		logger.Infof(ctx, "[GraphCommunity] KB %s: no community >= %d nodes (nodes=%d rels=%d)",
			payload.KnowledgeBaseID, types.GraphCommunityMinSize, len(graph.Node), len(graph.Relation))
		return nil
	}

	summaryModel, err := s.modelService.GetChatModel(ctx, kb.SummaryModelID)
	if err != nil {
		return fmt.Errorf("failed to get summary model: %w", err)
	}
	embedder, err := s.modelService.GetEmbeddingModel(ctx, kb.EmbeddingModelID)
	if err != nil {
		logger.Warnf(ctx, "[GraphCommunity] embedding model %s unavailable, summaries stored without embedding: %v",
			kb.EmbeddingModelID, err)
		embedder = nil
	}

	relIndex := buildRelationIndex(graph)
	rows := make([]*types.GraphCommunity, 0, len(kept))
	failures := 0
	for _, members := range kept {
		title, summary, serr := s.summarizeCommunity(ctx, summaryModel, members, relIndex)
		if serr != nil {
			failures++
			logger.Warnf(ctx, "[GraphCommunity] summarize community failed (KB %s, %d members): %v",
				payload.KnowledgeBaseID, len(members), serr)
			continue
		}
		var embeddingVec types.VectorBlob
		if embedder != nil {
			if vec, eerr := embedder.Embed(ctx, title+"\n"+summary); eerr == nil {
				embeddingVec = types.VectorBlob(vec)
			} else {
				logger.Warnf(ctx, "[GraphCommunity] embed community summary failed: %v", eerr)
			}
		}
		rows = append(rows, &types.GraphCommunity{
			TenantID:         payload.TenantID,
			KnowledgeBaseID:  payload.KnowledgeBaseID,
			CommunityKey:     types.ComputeGraphCommunityKey(payload.KnowledgeBaseID, members),
			Title:            title,
			Summary:          summary,
			NodeNames:        types.StringArray(members),
			NodeCount:        len(members),
			RelCount:         countInternalRelations(members, graph),
			SummaryModelID:   kb.SummaryModelID,
			EmbeddingModelID: kb.EmbeddingModelID,
			Embedding:        embeddingVec,
		})
	}
	if len(rows) == 0 && failures > 0 {
		return fmt.Errorf("all %d community summaries failed", failures)
	}

	if err := s.communityRepo.UpsertCommunities(ctx, rows); err != nil {
		return fmt.Errorf("failed to upsert communities: %w", err)
	}
	keepKeys := make([]string, 0, len(rows))
	for _, row := range rows {
		keepKeys = append(keepKeys, row.CommunityKey)
	}
	if err := s.communityRepo.DeleteCommunitiesNotIn(ctx, payload.TenantID, payload.KnowledgeBaseID, keepKeys); err != nil {
		// Stale-row cleanup failure is not worth a task retry — the next
		// rebuild converges again.
		logger.Warnf(ctx, "[GraphCommunity] failed to prune stale communities for KB %s: %v",
			payload.KnowledgeBaseID, err)
	}
	logger.Infof(ctx, "[GraphCommunity] KB %s: built %d communities (%d failed, nodes=%d rels=%d, trigger=%s)",
		payload.KnowledgeBaseID, len(rows), failures, len(graph.Node), len(graph.Relation), payload.Trigger)
	return nil
}

// Recall returns community summaries semantically related to the query
// vector. The per-KB community set is tiny, so cosine scoring in Go beats
// maintaining a vector index.
func (s *graphCommunityService) Recall(
	ctx context.Context, tenantID uint64, kbIDs []string,
	queryVec []float32, topK int, threshold float64,
) ([]*types.GraphCommunity, error) {
	if len(queryVec) == 0 || len(kbIDs) == 0 {
		return nil, nil
	}
	if topK <= 0 {
		topK = types.GraphCommunityRecallTopK
	}
	if threshold <= 0 {
		threshold = types.GraphCommunityRecallThreshold
	}
	type scored struct {
		row   *types.GraphCommunity
		score float64
	}
	var hits []scored
	for _, kbID := range kbIDs {
		rows, err := s.communityRepo.ListCommunities(ctx, tenantID, kbID)
		if err != nil {
			logger.Warnf(ctx, "[GraphCommunity] recall: list communities for KB %s failed: %v", kbID, err)
			continue
		}
		for _, row := range rows {
			if len(row.Embedding) == 0 {
				continue
			}
			sim := types.Cosine(queryVec, row.Embedding)
			if sim >= threshold {
				hits = append(hits, scored{row: row, score: sim})
			}
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].row.ID < hits[j].row.ID
	})
	if len(hits) > topK {
		hits = hits[:topK]
	}
	out := make([]*types.GraphCommunity, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.row)
	}
	return out, nil
}

// summarizeCommunity renders one community for the LLM and parses the
// "Title:/Summary:" response.
func (s *graphCommunityService) summarizeCommunity(
	ctx context.Context, model chat.Chat,
	members []string, relIndex map[string][]string,
) (string, string, error) {
	const (
		maxEntitiesInPrompt  = 50
		maxRelationsInPrompt = 100
	)

	// Rank entities by degree so oversized communities show the hub nodes.
	degree := make(map[string]int, len(members))
	for _, m := range members {
		degree[m] = len(relIndex[m])
	}
	ranked := append([]string(nil), members...)
	sort.Slice(ranked, func(i, j int) bool {
		if degree[ranked[i]] != degree[ranked[j]] {
			return degree[ranked[i]] > degree[ranked[j]]
		}
		return ranked[i] < ranked[j]
	})
	shown := ranked
	if len(shown) > maxEntitiesInPrompt {
		shown = shown[:maxEntitiesInPrompt]
	}

	entityLines := make([]string, 0, len(shown))
	for _, m := range shown {
		entityLines = append(entityLines, "- "+m)
	}
	rels := make([]string, 0, maxRelationsInPrompt)
	seen := make(map[string]bool)
	for _, m := range shown {
		for _, rel := range relIndex[m] {
			if !seen[rel] {
				seen[rel] = true
				rels = append(rels, "- "+rel)
				if len(rels) >= maxRelationsInPrompt {
					break
				}
			}
		}
		if len(rels) >= maxRelationsInPrompt {
			break
		}
	}

	prompt := fmt.Sprintf(communitySummaryPromptTemplate,
		strings.Join(entityLines, "\n"), strings.Join(rels, "\n"))
	thinking := false
	resp, err := model.Chat(ctx, []chat.Message{{Role: "user", Content: prompt}},
		&chat.ChatOptions{Temperature: 0.3, MaxTokens: 768, Thinking: &thinking})
	if err != nil {
		return "", "", err
	}
	title, summary := parseCommunitySummary(resp.Content)
	if summary == "" {
		return "", "", fmt.Errorf("empty community summary from model")
	}
	if title == "" {
		title = members[0]
		if len(members) > 1 {
			title += fmt.Sprintf(" 等 %d 个实体", len(members))
		}
	}
	return title, summary, nil
}

// parseCommunitySummary extracts the Title:/Summary: lines from the model
// output, tolerating case variations and missing title.
func parseCommunitySummary(content string) (title, summary string) {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		switch {
		case strings.HasPrefix(lower, "title:"):
			title = strings.TrimSpace(trimmed[len("title:"):])
		case strings.HasPrefix(lower, "summary:"):
			summary = strings.TrimSpace(trimmed[len("summary:"):])
		case summary != "" && title != "":
			// continuation line of the summary
			summary += " " + trimmed
		}
	}
	return title, strings.TrimSpace(summary)
}

// buildRelationIndex maps entity name -> "a -[type]-> b" renderings of every
// relation touching it. Relations render once per direction endpoint.
func buildRelationIndex(graph *types.GraphData) map[string][]string {
	index := make(map[string][]string, len(graph.Node))
	for _, rel := range graph.Relation {
		rendered := fmt.Sprintf("%s -[%s]-> %s", rel.Node1, rel.Type, rel.Node2)
		index[rel.Node1] = append(index[rel.Node1], rendered)
		index[rel.Node2] = append(index[rel.Node2], rendered)
	}
	return index
}

// countInternalRelations counts graph edges whose both endpoints are inside
// the community — cross-community edges must not inflate RelCount.
func countInternalRelations(members []string, graph *types.GraphData) int {
	in := make(map[string]bool, len(members))
	for _, m := range members {
		in[m] = true
	}
	count := 0
	for _, rel := range graph.Relation {
		if rel.Node1 == rel.Node2 {
			continue // self loops are graph noise, consistent with detectCommunities
		}
		if in[rel.Node1] && in[rel.Node2] {
			count++
		}
	}
	return count
}

// detectCommunities runs deterministic asynchronous label propagation over
// the graph: nodes are processed in name order each round and adopt the most
// frequent neighbor label (ties broken by the lexicographically smallest
// label). Returns communities sorted by size desc, then by first member.
func detectCommunities(graph *types.GraphData) [][]string {
	const maxRounds = 20

	names := make([]string, 0, len(graph.Node))
	seen := make(map[string]bool, len(graph.Node))
	for _, n := range graph.Node {
		if n.Name != "" && !seen[n.Name] {
			seen[n.Name] = true
			names = append(names, n.Name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil
	}

	adjacency := make(map[string][]string, len(names))
	addEdge := func(a, b string) {
		adjacency[a] = append(adjacency[a], b)
	}
	for _, rel := range graph.Relation {
		if rel.Node1 == "" || rel.Node2 == "" || rel.Node1 == rel.Node2 {
			continue
		}
		if !seen[rel.Node1] || !seen[rel.Node2] {
			continue
		}
		addEdge(rel.Node1, rel.Node2)
		addEdge(rel.Node2, rel.Node1)
	}

	label := make(map[string]string, len(names))
	for _, name := range names {
		label[name] = name
	}
	for round := 0; round < maxRounds; round++ {
		changed := false
		for _, name := range names {
			neighbors := adjacency[name]
			if len(neighbors) == 0 {
				continue
			}
			counts := make(map[string]int, len(neighbors))
			best, bestCount := "", 0
			for _, nb := range neighbors {
				l := label[nb]
				counts[l]++
			}
			// deterministic: highest count, then smallest label name
			labels := make([]string, 0, len(counts))
			for l := range counts {
				labels = append(labels, l)
			}
			sort.Strings(labels)
			for _, l := range labels {
				if counts[l] > bestCount {
					best, bestCount = l, counts[l]
				}
			}
			if best != "" && best != label[name] {
				label[name] = best
				changed = true
			}
		}
		if !changed {
			break
		}
	}

	groups := make(map[string][]string)
	for _, name := range names {
		l := label[name]
		groups[l] = append(groups[l], name)
	}
	communities := make([][]string, 0, len(groups))
	for _, members := range groups {
		communities = append(communities, members)
	}
	sort.Slice(communities, func(i, j int) bool {
		if len(communities[i]) != len(communities[j]) {
			return len(communities[i]) > len(communities[j])
		}
		return communities[i][0] < communities[j][0]
	})
	return communities
}
