package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	apprepo "github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type memoryFakeChat struct {
	response string
	err      error
	calls    int
	lastUser string
}

func (f *memoryFakeChat) Chat(ctx context.Context, messages []chat.Message, opts *chat.ChatOptions) (*types.ChatResponse, error) {
	f.calls++
	if len(messages) > 1 {
		f.lastUser = messages[len(messages)-1].Content
	}
	if f.err != nil {
		return nil, f.err
	}
	return &types.ChatResponse{Content: f.response}, nil
}

func (f *memoryFakeChat) ChatStream(ctx context.Context, messages []chat.Message, opts *chat.ChatOptions) (<-chan types.StreamResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (f *memoryFakeChat) GetModelName() string { return "fake-chat" }
func (f *memoryFakeChat) GetModelID() string   { return "fake-chat-id" }

// memoryFakeEmbedder derives a deterministic unit-ish vector from the text so
// semantic recall tests can control similarity via shared prefixes.
type memoryFakeEmbedder struct{}

func (e *memoryFakeEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	vecs, err := e.BatchEmbed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}

func (e *memoryFakeEmbedder) BatchEmbed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		// 8-dim toy embedding: byte histogram buckets, good enough for cosine
		// equality/inequality assertions.
		v := make([]float32, 8)
		for _, r := range t {
			v[int(r)%8]++
		}
		out[i] = v
	}
	return out, nil
}

func (e *memoryFakeEmbedder) GetModelName() string { return "fake-embedder" }
func (e *memoryFakeEmbedder) GetDimensions() int   { return 8 }
func (e *memoryFakeEmbedder) GetModelID() string   { return "fake-embedder-id" }

func (e *memoryFakeEmbedder) BatchEmbedWithPool(ctx context.Context, model embedding.Embedder, texts []string) ([][]float32, error) {
	return e.BatchEmbed(ctx, texts)
}

type memoryFakeModelService struct {
	interfaces.ModelService
	chatModel chat.Chat
	embedder  embedding.Embedder
	models    []*types.Model
	chatErr   error
}

func (f *memoryFakeModelService) ListModels(ctx context.Context) ([]*types.Model, error) {
	return f.models, nil
}

func (f *memoryFakeModelService) GetChatModel(ctx context.Context, modelID string) (chat.Chat, error) {
	if f.chatErr != nil {
		return nil, f.chatErr
	}
	return f.chatModel, nil
}

func (f *memoryFakeModelService) GetEmbeddingModel(ctx context.Context, modelID string) (embedding.Embedder, error) {
	if f.embedder == nil {
		return nil, fmt.Errorf("no embedding model")
	}
	return f.embedder, nil
}

type memoryFakeMessageRepo struct {
	interfaces.MessageRepository
	messages []*types.Message
	err      error
}

func (f *memoryFakeMessageRepo) GetRecentMessagesBySession(ctx context.Context, sessionID string, limit int) ([]*types.Message, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.messages, nil
}

type memoryFakeUserRepo struct {
	interfaces.UserRepository
	users map[string]*types.User
}

func (f *memoryFakeUserRepo) GetUserByID(ctx context.Context, id string) (*types.User, error) {
	if u, ok := f.users[id]; ok {
		return u, nil
	}
	return nil, fmt.Errorf("user not found")
}

type memoryFakeEnqueuer struct {
	tasks []*asynq.Task
	err   error
}

func (f *memoryFakeEnqueuer) Enqueue(task *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.tasks = append(f.tasks, task)
	return &asynq.TaskInfo{ID: "t-1"}, nil
}

// newMemoryServiceForTest wires a memoryService with a real sqlite-backed
// repository plus controllable fakes for everything else. The *gorm.DB is
// returned so tests can backdate rows directly (the repo interface does not
// expose timestamp writes).
func newMemoryServiceForTest(t *testing.T) (*memoryService, *gorm.DB, *memoryFakeChat, *memoryFakeMessageRepo, *memoryFakeUserRepo, *memoryFakeEnqueuer) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.MemoryFact{}, &types.MemorySessionSummary{}))
	require.NoError(t, db.Exec(`CREATE UNIQUE INDEX uq_memory_facts_triple
		ON memory_facts(tenant_id, user_id, triple_hash)
		WHERE deleted_at IS NULL AND triple_hash <> ''`).Error)
	require.NoError(t, db.Exec(`CREATE UNIQUE INDEX uq_memory_session_summaries_session
		ON memory_session_summaries(tenant_id, session_id) WHERE deleted_at IS NULL`).Error)

	chatModel := &memoryFakeChat{}
	msgRepo := &memoryFakeMessageRepo{}
	userRepo := &memoryFakeUserRepo{users: map[string]*types.User{}}
	enqueuer := &memoryFakeEnqueuer{}
	modelSvc := &memoryFakeModelService{
		chatModel: chatModel,
		embedder:  &memoryFakeEmbedder{},
		models: []*types.Model{
			{ID: "chat-1", Type: types.ModelTypeKnowledgeQA},
			{ID: "emb-1", Type: types.ModelTypeEmbedding},
		},
	}

	svc := &memoryService{
		cfg:          &config.Config{Conversation: &config.ConversationConfig{}},
		memoryRepo:   apprepo.NewMemoryRepository(db),
		messageRepo:  msgRepo,
		userRepo:     userRepo,
		modelService: modelSvc,
		taskEnqueuer: enqueuer,
	}
	return svc, db, chatModel, msgRepo, userRepo, enqueuer
}

func memoryTestCtx() context.Context {
	ctx := context.Background()
	ctx = context.WithValue(ctx, types.TenantIDContextKey, uint64(1))
	ctx = context.WithValue(ctx, types.UserIDContextKey, "alice")
	return ctx
}

const memoryExtractionLLMJSON = `{"memories":[
  {"category":"todo","subject":"用户","predicate":"需要完成","object":"演示 Prometheus 部署方案","content":"用户需要向团队演示 Prometheus 部署方案","confidence":0.95,"importance":0.9,"due_at":"2026-08-20"},
  {"category":"preference","subject":"用户","predicate":"偏好","object":"Helm","content":"用户偏好使用 Helm 管理部署","confidence":0.9,"importance":0.6,"due_at":""}
],"session_summary":"用户在准备 Prometheus 部署演示，倾向用 Helm。","key_topics":["Prometheus","Helm"]}`

// ---------------------------------------------------------------------------
// Extraction pipeline
// ---------------------------------------------------------------------------

func TestMemoryServiceProcessMemoryExtractEndToEnd(t *testing.T) {
	svc, _, chatModel, msgRepo, _, _ := newMemoryServiceForTest(t)
	chatModel.response = memoryExtractionLLMJSON
	msgRepo.messages = []*types.Message{
		{ID: "u1", SessionID: "s-1", Role: "user", Content: "我下周三要演示 Prometheus 部署方案，倾向用 Helm"},
		{ID: "a1", SessionID: "s-1", Role: "assistant", Content: "可以按 values.yaml 分层覆盖组织演示"},
	}

	payload := &types.MemoryExtractPayload{
		TenantID: 1, UserID: "alice", SessionID: "s-1",
		UserMessageID: "u1", AssistantMessageID: "a1",
	}
	payloadBytes, err := json.Marshal(payload)
	require.NoError(t, err)

	err = svc.ProcessMemoryExtract(memoryTestCtx(), asynq.NewTask(types.TypeMemoryExtract, payloadBytes))
	require.NoError(t, err)
	require.Equal(t, 1, chatModel.calls)
	require.Contains(t, chatModel.lastUser, "Prometheus", "extraction prompt must carry the conversation")

	// L3 facts persisted with embeddings and parsed due dates.
	facts, total, err := svc.memoryRepo.ListFacts(memoryTestCtx(), 1, "alice", &types.MemoryFactQuery{Page: 1, PageSize: 10})
	require.NoError(t, err)
	require.EqualValues(t, 2, total)
	require.Len(t, facts, 2)
	var todo *types.MemoryFact
	for _, f := range facts {
		require.NotEmpty(t, f.Embedding, "fact must carry an embedding")
		require.Equal(t, types.MemoryStatusActive, f.Status)
		if f.Category == types.MemoryCategoryTodo {
			todo = f
		}
	}
	require.NotNil(t, todo)
	require.NotNil(t, todo.DueAt)
	require.Equal(t, 2026, todo.DueAt.Year())

	// L2 summary persisted.
	summary, err := svc.memoryRepo.GetSessionSummary(memoryTestCtx(), 1, "s-1")
	require.NoError(t, err)
	require.NotNil(t, summary)
	require.Contains(t, summary.Summary, "Prometheus")
	require.Equal(t, 2, summary.MessageCount)
	require.NotEmpty(t, summary.Embedding)
}

func TestMemoryServiceProcessMemoryExtractDedupRefreshes(t *testing.T) {
	svc, _, chatModel, msgRepo, _, _ := newMemoryServiceForTest(t)
	chatModel.response = memoryExtractionLLMJSON
	msgRepo.messages = []*types.Message{
		{ID: "u1", SessionID: "s-1", Role: "user", Content: "我偏好 Helm"},
	}
	payload := &types.MemoryExtractPayload{TenantID: 1, UserID: "alice", SessionID: "s-1", UserMessageID: "u1", AssistantMessageID: "a1"}
	payloadBytes, _ := json.Marshal(payload)
	task := asynq.NewTask(types.TypeMemoryExtract, payloadBytes)

	require.NoError(t, svc.ProcessMemoryExtract(memoryTestCtx(), task))
	require.NoError(t, svc.ProcessMemoryExtract(memoryTestCtx(), task))

	_, total, err := svc.memoryRepo.ListFacts(memoryTestCtx(), 1, "alice", &types.MemoryFactQuery{Page: 1, PageSize: 10})
	require.NoError(t, err)
	require.EqualValues(t, 2, total, "re-extracting the same turn must upsert, not duplicate")
}

func TestMemoryServiceProcessMemoryExtractSkipsWhenDisabled(t *testing.T) {
	svc, _, chatModel, msgRepo, userRepo, _ := newMemoryServiceForTest(t)
	off := false
	userRepo.users["alice"] = &types.User{ID: "alice", Preferences: types.UserPreferences{MemoryEnabled: &off}}
	chatModel.response = memoryExtractionLLMJSON
	msgRepo.messages = []*types.Message{{ID: "u1", SessionID: "s-1", Role: "user", Content: "hi"}}

	payload := &types.MemoryExtractPayload{TenantID: 1, UserID: "alice", SessionID: "s-1", UserMessageID: "u1", AssistantMessageID: "a1"}
	payloadBytes, _ := json.Marshal(payload)
	require.NoError(t, svc.ProcessMemoryExtract(memoryTestCtx(), asynq.NewTask(types.TypeMemoryExtract, payloadBytes)))
	require.Equal(t, 0, chatModel.calls, "no LLM call when memory is disabled for the user")

	_, total, err := svc.memoryRepo.ListFacts(memoryTestCtx(), 1, "alice", &types.MemoryFactQuery{Page: 1, PageSize: 10})
	require.NoError(t, err)
	require.EqualValues(t, 0, total)
}

func TestMemoryServiceProcessMemoryExtractBadJSONSkipped(t *testing.T) {
	svc, _, chatModel, msgRepo, _, _ := newMemoryServiceForTest(t)
	chatModel.response = "I could not extract anything useful."
	msgRepo.messages = []*types.Message{{ID: "u1", SessionID: "s-1", Role: "user", Content: "hi"}}

	payload := &types.MemoryExtractPayload{TenantID: 1, UserID: "alice", SessionID: "s-1", UserMessageID: "u1", AssistantMessageID: "a1"}
	payloadBytes, _ := json.Marshal(payload)
	// Bad output must be acked (no retry), not loop forever.
	require.NoError(t, svc.ProcessMemoryExtract(memoryTestCtx(), asynq.NewTask(types.TypeMemoryExtract, payloadBytes)))

	_, total, err := svc.memoryRepo.ListFacts(memoryTestCtx(), 1, "alice", &types.MemoryFactQuery{Page: 1, PageSize: 10})
	require.NoError(t, err)
	require.EqualValues(t, 0, total)
}

func TestMemoryServiceProcessMemoryExtractMergesPreviousSummary(t *testing.T) {
	svc, _, chatModel, msgRepo, _, _ := newMemoryServiceForTest(t)
	chatModel.response = memoryExtractionLLMJSON
	msgRepo.messages = []*types.Message{{ID: "u1", SessionID: "s-1", Role: "user", Content: "继续聊部署"}}

	// Pre-existing L2 summary of the same session.
	require.NoError(t, svc.memoryRepo.UpsertSessionSummary(memoryTestCtx(), &types.MemorySessionSummary{
		TenantID: 1, UserID: "alice", SessionID: "s-1", Summary: "用户在评估监控方案。",
	}))

	payload := &types.MemoryExtractPayload{TenantID: 1, UserID: "alice", SessionID: "s-1", UserMessageID: "u1", AssistantMessageID: "a1"}
	payloadBytes, _ := json.Marshal(payload)
	require.NoError(t, svc.ProcessMemoryExtract(memoryTestCtx(), asynq.NewTask(types.TypeMemoryExtract, payloadBytes)))
	require.Contains(t, chatModel.lastUser, "Previous summary: 用户在评估监控方案。",
		"extraction prompt must include the previous rolling summary for merging")
}

// ---------------------------------------------------------------------------
// Enqueue
// ---------------------------------------------------------------------------

func TestMemoryServiceEnqueueMemoryExtract(t *testing.T) {
	svc, _, _, _, _, enqueuer := newMemoryServiceForTest(t)
	payload := &types.MemoryExtractPayload{TenantID: 1, UserID: "alice", SessionID: "s-1", UserMessageID: "u1", AssistantMessageID: "a1"}
	svc.EnqueueMemoryExtract(memoryTestCtx(), payload)

	require.Len(t, enqueuer.tasks, 1)
	require.Equal(t, types.TypeMemoryExtract, enqueuer.tasks[0].Type())
	var got types.MemoryExtractPayload
	require.NoError(t, json.Unmarshal(enqueuer.tasks[0].Payload(), &got))
	require.Equal(t, "s-1", got.SessionID)
	require.Equal(t, "alice", got.UserID)
}

func TestMemoryServiceEnqueueSkippedWhenDisabled(t *testing.T) {
	svc, _, _, _, userRepo, enqueuer := newMemoryServiceForTest(t)
	off := false
	userRepo.users["alice"] = &types.User{ID: "alice", Preferences: types.UserPreferences{MemoryEnabled: &off}}

	svc.EnqueueMemoryExtract(memoryTestCtx(), &types.MemoryExtractPayload{TenantID: 1, UserID: "alice", SessionID: "s-1"})
	require.Empty(t, enqueuer.tasks)
}

// ---------------------------------------------------------------------------
// Recall & formatting
// ---------------------------------------------------------------------------

func TestMemoryServiceRecallRanksByScore(t *testing.T) {
	svc, db, _, _, _, _ := newMemoryServiceForTest(t)
	ctx := memoryTestCtx()
	now := time.Now()
	embedder := &memoryFakeEmbedder{}

	// Two facts: one recent+matching, one stale and unrelated.
	matching, err := embedder.Embed(ctx, "用户偏好 Python 异步框架")
	require.NoError(t, err)
	recent := &types.MemoryFact{
		TenantID: 1, UserID: "alice", SessionID: "s-1", Category: types.MemoryCategoryPreference,
		Subject: "用户", Predicate: "偏好", Object: "Python 异步框架",
		Content: "用户偏好 Python 异步框架", Importance: 0.6, Confidence: 0.9,
		Embedding: types.VectorBlob(matching), AccessCount: 3,
	}
	stale := &types.MemoryFact{
		TenantID: 1, UserID: "alice", SessionID: "s-1", Category: types.MemoryCategoryFact,
		Subject: "用户", Predicate: "提过", Object: "完全不相关的旧事实",
		Content: "完全不相关的旧事实 zzqxv", Importance: 0.2, Confidence: 0.5,
	}
	require.NoError(t, svc.memoryRepo.CreateFact(ctx, recent))
	require.NoError(t, svc.memoryRepo.CreateFact(ctx, stale))
	// Backdate the stale fact by ~10 days so time decay suppresses it.
	old := now.Add(-10 * 24 * time.Hour)
	require.NoError(t, db.Model(&types.MemoryFact{}).Where("id = ?", stale.ID).
		Updates(map[string]interface{}{"updated_at": old, "created_at": old}).Error)

	queryVec, err := embedder.Embed(ctx, "用户偏好 Python 异步框架")
	require.NoError(t, err)
	got, err := svc.Recall(ctx, &types.MemoryRecallParams{
		Query: "Python 框架", QueryEmbedding: queryVec, Limit: 5, Now: now,
	})
	require.NoError(t, err)
	require.NotEmpty(t, got)
	require.Equal(t, "fact", got[0].Kind)
	require.Equal(t, recent.ID, got[0].Fact.ID, "semantically matching + recent fact must rank first")
}

func TestMemoryServiceRecallDisabledReturnsNil(t *testing.T) {
	svc, _, _, _, userRepo, _ := newMemoryServiceForTest(t)
	off := false
	userRepo.users["alice"] = &types.User{ID: "alice", Preferences: types.UserPreferences{MemoryEnabled: &off}}

	got, err := svc.Recall(memoryTestCtx(), &types.MemoryRecallParams{Query: "q"})
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestMemoryServiceFormatRecalledForPrompt(t *testing.T) {
	svc, _, _, _, _, _ := newMemoryServiceForTest(t)
	due := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	block := svc.FormatRecalledForPrompt([]*types.RecalledMemory{
		{Kind: "fact", Fact: &types.MemoryFact{Category: types.MemoryCategoryPreference, Content: "用户偏好 Python"}, Score: 0.9},
		{Kind: "fact", Fact: &types.MemoryFact{Category: types.MemoryCategoryTodo, Content: "演示部署方案", DueAt: &due}, Score: 0.8},
		{Kind: "session_summary", Summary: &types.MemorySessionSummary{Summary: "上次讨论了监控方案"}, Score: 0.5},
	})
	require.Contains(t, block, "用户偏好 Python")
	require.Contains(t, block, "截止 2026-08-20")
	require.Contains(t, block, "上次讨论了监控方案")

	require.Empty(t, svc.FormatRecalledForPrompt(nil), "no memories -> no prompt block")
}

// ---------------------------------------------------------------------------
// Management surface
// ---------------------------------------------------------------------------

func TestMemoryServiceUpdateFactMergesAndRehashes(t *testing.T) {
	svc, _, _, _, _, _ := newMemoryServiceForTest(t)
	ctx := memoryTestCtx()

	stored := &types.MemoryFact{
		TenantID: 1, UserID: "alice", SessionID: "s-1", Category: types.MemoryCategoryPreference,
		Subject: "用户", Predicate: "偏好", Object: "Python",
		Content: "用户偏好 Python", Importance: 0.5, Confidence: 0.8,
	}
	require.NoError(t, svc.memoryRepo.CreateFact(ctx, stored))

	// Partial edit: only object/content change; confidence/importance fall back.
	require.NoError(t, svc.UpdateFact(ctx, &types.MemoryFact{
		ID: stored.ID, Object: "Golang", Content: "用户偏好 Golang",
	}))

	got, err := svc.memoryRepo.GetFactByID(ctx, 1, "alice", stored.ID)
	require.NoError(t, err)
	require.Equal(t, "用户偏好 Golang", got.Content)
	require.Equal(t, 0.8, got.Confidence, "untouched confidence must survive the edit")
	require.Equal(t, types.ComputeTripleHash(types.MemoryCategoryPreference, "用户", "偏好", "Golang"), got.TripleHash)
	require.NotEmpty(t, got.Embedding, "content edit must trigger re-embedding")
}

func TestMemoryServiceUpdateFactNotFound(t *testing.T) {
	svc, _, _, _, _, _ := newMemoryServiceForTest(t)
	err := svc.UpdateFact(memoryTestCtx(), &types.MemoryFact{ID: "missing", Content: "x"})
	require.Error(t, err)
}

func TestMemoryServiceDeleteAllForUser(t *testing.T) {
	svc, _, _, _, _, _ := newMemoryServiceForTest(t)
	ctx := memoryTestCtx()

	require.NoError(t, svc.memoryRepo.CreateFact(ctx, &types.MemoryFact{
		TenantID: 1, UserID: "alice", Category: types.MemoryCategoryFact, Content: "f1",
	}))
	require.NoError(t, svc.memoryRepo.UpsertSessionSummary(ctx, &types.MemorySessionSummary{
		TenantID: 1, UserID: "alice", SessionID: "s-1", Summary: "sum",
	}))

	n, err := svc.DeleteAllForUser(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 2, n)

	_, total, err := svc.memoryRepo.ListFacts(ctx, 1, "alice", &types.MemoryFactQuery{Status: "all"})
	require.NoError(t, err)
	require.EqualValues(t, 0, total)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func TestExtractJSONObject(t *testing.T) {
	require.Equal(t, `{"a":1}`, extractJSONObject("```json\n{\"a\":1}\n```"))
	require.Equal(t, `{"a":1}`, extractJSONObject("prose before {\"a\":1} trailing"))
	require.Empty(t, extractJSONObject("no json here"))
	require.Empty(t, extractJSONObject(""))
}

func TestParseMemoryDueAt(t *testing.T) {
	require.Nil(t, parseMemoryDueAt(""))
	require.Nil(t, parseMemoryDueAt("not-a-date"))
	d := parseMemoryDueAt("2026-08-20")
	require.NotNil(t, d)
	require.Equal(t, time.August, d.Month())
}

func TestValidMemoryCategory(t *testing.T) {
	for _, c := range []string{
		types.MemoryCategoryProfile, types.MemoryCategoryFact,
		types.MemoryCategoryPreference, types.MemoryCategoryTodo, types.MemoryCategoryFeedback,
		types.MemoryCategorySoul, types.MemoryCategorySkill,
	} {
		require.True(t, validMemoryCategory(c))
	}
	require.False(t, validMemoryCategory("bogus"))
	require.False(t, validMemoryCategory(""))
}

func TestMemoryServiceIsEnabled(t *testing.T) {
	svc, _, _, _, userRepo, _ := newMemoryServiceForTest(t)
	ctx := memoryTestCtx()

	// Unknown principal-derived IDs default to enabled.
	require.True(t, svc.IsEnabled(ctx, "ext-user-1"))
	// Empty user is never enabled.
	require.False(t, svc.IsEnabled(ctx, ""))
	// Explicit off wins.
	off := false
	userRepo.users["bob"] = &types.User{ID: "bob", Preferences: types.UserPreferences{MemoryEnabled: &off}}
	require.False(t, svc.IsEnabled(ctx, "bob"))
	// nil preference defaults to enabled.
	userRepo.users["carol"] = &types.User{ID: "carol"}
	require.True(t, svc.IsEnabled(ctx, "carol"))
}

func TestMemoryServiceExtractionTruncatesLongMessages(t *testing.T) {
	svc, _, chatModel, msgRepo, _, _ := newMemoryServiceForTest(t)
	chatModel.response = `{"memories":[],"session_summary":"s","key_topics":[]}`
	msgRepo.messages = []*types.Message{
		{ID: "u1", SessionID: "s-1", Role: "user", Content: strings.Repeat("长", memoryExtractionMaxContentChars*2)},
	}
	payload := &types.MemoryExtractPayload{TenantID: 1, UserID: "alice", SessionID: "s-1", UserMessageID: "u1", AssistantMessageID: "a1"}
	payloadBytes, _ := json.Marshal(payload)
	require.NoError(t, svc.ProcessMemoryExtract(memoryTestCtx(), asynq.NewTask(types.TypeMemoryExtract, payloadBytes)))
	require.Less(t, len(chatModel.lastUser), memoryExtractionMaxContentChars*2,
		"over-long messages must be truncated before entering the extraction prompt")
}

// ---------------------------------------------------------------------------
// Four-module architecture (P0-2)
// ---------------------------------------------------------------------------

func TestMemoryModuleOf(t *testing.T) {
	cases := map[string]string{
		types.MemoryCategorySoul:       types.MemoryModuleSoul,
		types.MemoryCategoryProfile:    types.MemoryModuleUser,
		types.MemoryCategoryFact:       types.MemoryModuleUser,
		types.MemoryCategoryPreference: types.MemoryModuleUser,
		types.MemoryCategorySkill:      types.MemoryModuleAgent,
		types.MemoryCategoryFeedback:   types.MemoryModuleAgent,
		types.MemoryCategoryTodo:       types.MemoryModuleMemory,
		"unknown":                      types.MemoryModuleMemory,
	}
	for category, module := range cases {
		require.Equal(t, module, types.MemoryModuleOf(category), "category %q", category)
	}
}

func TestMemoryProfileSectionOf(t *testing.T) {
	require.Equal(t, types.MemoryProfileSectionIdentity, types.MemoryProfileSectionOf(
		&types.MemoryFact{Category: types.MemoryCategoryProfile}))
	require.Equal(t, types.MemoryProfileSectionPreference, types.MemoryProfileSectionOf(
		&types.MemoryFact{Category: types.MemoryCategoryPreference}))
	require.Equal(t, types.MemoryProfileSectionRole, types.MemoryProfileSectionOf(
		&types.MemoryFact{Category: types.MemoryCategoryFact, Predicate: "负责"}))
	require.Equal(t, types.MemoryProfileSectionRole, types.MemoryProfileSectionOf(
		&types.MemoryFact{Category: types.MemoryCategoryFact, Predicate: "Responsible for delivery"}))
	require.Equal(t, types.MemoryProfileSectionFact, types.MemoryProfileSectionOf(
		&types.MemoryFact{Category: types.MemoryCategoryFact, Predicate: "喜欢"}))
	require.Equal(t, types.MemoryProfileSectionFact, types.MemoryProfileSectionOf(nil))
}

func TestMemoryServiceFormatRecalledForPromptFourModules(t *testing.T) {
	svc, _, _, _, _, _ := newMemoryServiceForTest(t)

	memories := []*types.RecalledMemory{
		{Kind: "fact", Fact: &types.MemoryFact{Category: types.MemoryCategorySoul, Content: "用户希望助手称呼他为张工"}, Score: 0.9},
		{Kind: "fact", Fact: &types.MemoryFact{Category: types.MemoryCategoryProfile, Content: "用户是 XX 公司运维负责人"}, Score: 0.85},
		{Kind: "fact", Fact: &types.MemoryFact{Category: types.MemoryCategoryFact, Predicate: "负责", Content: "用户负责 Project X 交付"}, Score: 0.8},
		{Kind: "fact", Fact: &types.MemoryFact{Category: types.MemoryCategoryPreference, Content: "用户偏好用 Helm 管理部署"}, Score: 0.75},
		{Kind: "fact", Fact: &types.MemoryFact{Category: types.MemoryCategorySkill, Content: "给该用户的回答先给结论再展开"}, Score: 0.7},
		{Kind: "fact", Fact: &types.MemoryFact{Category: types.MemoryCategoryFeedback, Content: "用户反馈助手回答太啰嗦"}, Score: 0.65},
	}
	block := svc.FormatRecalledForPrompt(memories)

	// Fixed section order: soul → profile → memory → agent.
	soulIdx := strings.Index(block, "### 助手风格指令（用户设定）")
	profileIdx := strings.Index(block, "### 用户档案")
	memoryIdx := strings.Index(block, "### 相关长期记忆")
	agentIdx := strings.Index(block, "### 助手经验")
	require.GreaterOrEqual(t, soulIdx, 0)
	require.Greater(t, profileIdx, soulIdx)
	require.Greater(t, memoryIdx, profileIdx)
	require.Greater(t, agentIdx, memoryIdx)

	require.Contains(t, block, "用户希望助手称呼他为张工")
	require.Contains(t, block, "用户是 XX 公司运维负责人")
	require.Contains(t, block, "用户负责 Project X 交付")
	require.Contains(t, block, "用户偏好用 Helm 管理部署")
	require.Contains(t, block, "给该用户的回答先给结论再展开")
	// Raw feedback is archived but never injected.
	require.NotContains(t, block, "用户反馈助手回答太啰嗦")

	// Empty input still yields no block.
	require.Empty(t, svc.FormatRecalledForPrompt([]*types.RecalledMemory{
		{Kind: "fact", Fact: &types.MemoryFact{Category: types.MemoryCategoryFeedback, Content: "x"}},
	}))
}

func TestMemoryServiceFormatRecalledForPromptProfileBudget(t *testing.T) {
	svc, _, _, _, _, _ := newMemoryServiceForTest(t)

	memories := make([]*types.RecalledMemory, 0, memoryInjectProfileMax+2)
	for i := 0; i < memoryInjectProfileMax+2; i++ {
		memories = append(memories, &types.RecalledMemory{Kind: "fact", Fact: &types.MemoryFact{
			Category: types.MemoryCategoryProfile,
			Content:  fmt.Sprintf("档案条目 %d", i),
			// The first entries are the least important; the budget must
			// keep the highest-importance rows.
			Importance: float64(i) / 10,
		}})
	}
	block := svc.FormatRecalledForPrompt(memories)

	require.Contains(t, block, fmt.Sprintf("档案条目 %d", memoryInjectProfileMax+1))
	require.Contains(t, block, fmt.Sprintf("档案条目 %d", memoryInjectProfileMax))
	require.NotContains(t, block, "档案条目 0", "lowest-importance profile rows are truncated")
	require.NotContains(t, block, "档案条目 1")
}

func TestMemoryServiceFormatRecalledForPromptSkillBudget(t *testing.T) {
	svc, _, _, _, _, _ := newMemoryServiceForTest(t)

	memories := make([]*types.RecalledMemory, 0, memoryInjectSkillMax+2)
	for i := 0; i < memoryInjectSkillMax+2; i++ {
		memories = append(memories, &types.RecalledMemory{Kind: "fact", Fact: &types.MemoryFact{
			Category:   types.MemoryCategorySkill,
			Content:    fmt.Sprintf("技巧条目 %d", i),
			Confidence: 0.5,
			// importance×confidence is lowest for the first entries; the
			// budget must keep the strongest skills.
			Importance: float64(i) / 10,
		}})
	}
	block := svc.FormatRecalledForPrompt(memories)

	require.Contains(t, block, fmt.Sprintf("技巧条目 %d", memoryInjectSkillMax+1))
	require.Contains(t, block, fmt.Sprintf("技巧条目 %d", memoryInjectSkillMax))
	require.NotContains(t, block, "技巧条目 0", "weakest skills are truncated")
	require.NotContains(t, block, "技巧条目 1")
}

func TestSelectRecalledWithSoulQuota(t *testing.T) {
	mk := func(category string, score float64) *types.RecalledMemory {
		return &types.RecalledMemory{Kind: "fact", Fact: &types.MemoryFact{Category: category}, Score: score}
	}

	// Soul directives survive even when ranked below the regular top-k, up
	// to the overflow tolerance (PRD FR2: total ≤ limit + 2).
	ranked := []*types.RecalledMemory{
		mk(types.MemoryCategoryPreference, 0.9),
		mk(types.MemoryCategoryPreference, 0.8),
		mk(types.MemoryCategorySoul, 0.5),
		mk(types.MemoryCategorySoul, 0.4),
	}
	got := selectRecalledWithSoulQuota(ranked, 2)
	require.Len(t, got, 4, "two topical + two souls fills limit + tolerance")
	require.Equal(t, types.MemoryCategorySoul, got[2].Fact.Category)
	require.Equal(t, types.MemoryCategorySoul, got[3].Fact.Category)

	// The overflow tolerance bounds the total: a third soul is dropped.
	ranked = []*types.RecalledMemory{
		mk(types.MemoryCategoryPreference, 0.9),
		mk(types.MemoryCategoryPreference, 0.8),
		mk(types.MemoryCategorySoul, 0.7),
		mk(types.MemoryCategorySoul, 0.5),
		mk(types.MemoryCategorySoul, 0.4),
	}
	got = selectRecalledWithSoulQuota(ranked, 2)
	require.Len(t, got, 4)
	require.Equal(t, 0.5, got[3].Score, "the higher-ranked soul wins the last slot")

	// The soul quota is bounded defensively.
	manySouls := make([]*types.RecalledMemory, 0, soulQuotaMax+2)
	for i := 0; i < soulQuotaMax+2; i++ {
		manySouls = append(manySouls, mk(types.MemoryCategorySoul, 1.0-float64(i)/100))
	}
	got = selectRecalledWithSoulQuota(manySouls, 8)
	require.Len(t, got, soulQuotaMax)

	// Regular memories alone obey the plain limit.
	regular := []*types.RecalledMemory{
		mk(types.MemoryCategoryFact, 0.9), mk(types.MemoryCategoryFact, 0.8), mk(types.MemoryCategoryFact, 0.7),
	}
	got = selectRecalledWithSoulQuota(regular, 2)
	require.Len(t, got, 2)
}

func TestMemoryServiceSkillConfidenceGate(t *testing.T) {
	svc, _, chatModel, msgRepo, _, _ := newMemoryServiceForTest(t)
	chatModel.response = `{"memories":[
  {"category":"skill","subject":"assistant","predicate":"回答风格","object":"要点式","content":"回答保持要点式","confidence":0.5,"importance":0.8,"due_at":""},
  {"category":"skill","subject":"assistant","predicate":"回答语言","object":"中文","content":"默认用中文回答","confidence":0.9,"importance":0.7,"due_at":""}
],"session_summary":"s","key_topics":[]}`
	msgRepo.messages = []*types.Message{{ID: "u1", SessionID: "s-1", Role: "user", Content: "回答要点式"}}

	payload := &types.MemoryExtractPayload{TenantID: 1, UserID: "alice", SessionID: "s-1", UserMessageID: "u1", AssistantMessageID: "a1"}
	payloadBytes, _ := json.Marshal(payload)
	require.NoError(t, svc.ProcessMemoryExtract(memoryTestCtx(), asynq.NewTask(types.TypeMemoryExtract, payloadBytes)))

	facts, _, err := svc.ListFacts(memoryTestCtx(), &types.MemoryFactQuery{Category: types.MemoryCategorySkill, Page: 1, PageSize: 10})
	require.NoError(t, err)
	require.Len(t, facts, 1, "low-confidence skills must be dropped; explicit ones kept")
	require.Equal(t, "默认用中文回答", facts[0].Content)
	require.Equal(t, "assistant", facts[0].Subject, "skills are normalized to assistant subject")
}

// seedModuleFact inserts one active fact for the ctx user.
func seedModuleFact(t *testing.T, svc *memoryService, category, content string, importance float64) *types.MemoryFact {
	t.Helper()
	fact := &types.MemoryFact{
		TenantID: 1, UserID: "alice", SessionID: "s-1", MessageID: "u1",
		Category: category, Subject: "用户", Predicate: "偏好", Object: content,
		Content: content, Confidence: 0.9, Importance: importance,
		Status: types.MemoryStatusActive,
	}
	require.NoError(t, svc.memoryRepo.CreateFact(memoryTestCtx(), fact))
	return fact
}

func TestMemoryServiceModuleOverview(t *testing.T) {
	svc, _, _, _, _, _ := newMemoryServiceForTest(t)
	ctx := memoryTestCtx()

	seedModuleFact(t, svc, types.MemoryCategorySoul, "称呼张工", 0.7)
	seedModuleFact(t, svc, types.MemoryCategoryProfile, "运维负责人", 0.9)
	seedModuleFact(t, svc, types.MemoryCategoryFact, "负责 Project X", 0.8)
	seedModuleFact(t, svc, types.MemoryCategorySkill, "先给结论", 0.8)
	seedModuleFact(t, svc, types.MemoryCategoryFeedback, "太啰嗦", 0.6)
	seedModuleFact(t, svc, types.MemoryCategoryTodo, "演示方案", 0.9)
	require.NoError(t, svc.memoryRepo.UpsertSessionSummary(ctx, &types.MemorySessionSummary{
		TenantID: 1, UserID: "alice", SessionID: "s-1", Summary: "讨论了部署",
	}))

	modules, err := svc.GetModuleOverview(ctx)
	require.NoError(t, err)
	require.Len(t, modules, 4)

	byModule := make(map[string]*types.MemoryModuleOverview, 4)
	for _, m := range modules {
		byModule[m.Module] = m
	}
	require.EqualValues(t, 1, byModule[types.MemoryModuleSoul].FactCount)
	require.EqualValues(t, 2, byModule[types.MemoryModuleUser].FactCount)
	require.EqualValues(t, 2, byModule[types.MemoryModuleAgent].FactCount)
	// The memory stream module counts every fact plus summaries.
	require.EqualValues(t, 6, byModule[types.MemoryModuleMemory].FactCount)
	require.EqualValues(t, 1, byModule[types.MemoryModuleMemory].SummaryCount)

	// Module order is fixed: soul, user, memory, agent.
	require.Equal(t, types.MemoryModuleSoul, modules[0].Module)
	require.Equal(t, types.MemoryModuleUser, modules[1].Module)
	require.Equal(t, types.MemoryModuleMemory, modules[2].Module)
	require.Equal(t, types.MemoryModuleAgent, modules[3].Module)
}

func TestMemoryServiceSoulCard(t *testing.T) {
	svc, _, _, _, _, _ := newMemoryServiceForTest(t)
	ctx := memoryTestCtx()

	// No template configured → empty persona, graceful degradation.
	seedModuleFact(t, svc, types.MemoryCategorySoul, "称呼张工", 0.7)
	card, err := svc.GetSoulCard(ctx)
	require.NoError(t, err)
	require.Empty(t, card.GlobalPersona.Content)
	require.Len(t, card.Adjustments, 1)
	require.Equal(t, "称呼张工", card.Adjustments[0].Content)

	// With a default system prompt template the persona surfaces read-only.
	svc.cfg.PromptTemplates = &config.PromptTemplatesConfig{
		SystemPrompt: []config.PromptTemplate{
			{ID: "default_kb", Name: "Knowledge Base Assistant", Description: "Standard template", Content: "You are WeKnora…", Default: true},
		},
	}
	card, err = svc.GetSoulCard(ctx)
	require.NoError(t, err)
	require.Equal(t, "Knowledge Base Assistant", card.GlobalPersona.Name)
	require.Contains(t, card.GlobalPersona.Content, "WeKnora")
}

func TestMemoryServiceProfileCard(t *testing.T) {
	svc, _, _, _, _, _ := newMemoryServiceForTest(t)
	ctx := memoryTestCtx()

	seedModuleFact(t, svc, types.MemoryCategoryProfile, "用户是运维负责人", 0.9)
	seedModuleFact(t, svc, types.MemoryCategoryPreference, "用户偏好 Python", 0.6)
	// Role-shaped fact: the role section is selected by role predicate
	// keywords ("负责"), so seed it with an explicit predicate.
	roleFact := &types.MemoryFact{
		TenantID: 1, UserID: "alice", SessionID: "s-1", MessageID: "u1",
		Category: types.MemoryCategoryFact, Subject: "用户", Predicate: "负责", Object: "Project X 交付",
		Content: "用户负责 Project X 交付", Confidence: 0.9, Importance: 0.8,
		Status: types.MemoryStatusActive,
	}
	require.NoError(t, svc.memoryRepo.CreateFact(ctx, roleFact))

	card, err := svc.GetProfileCard(ctx)
	require.NoError(t, err)
	require.Len(t, card.Sections, 4)

	byKey := make(map[string]*types.MemoryProfileSection, 4)
	for _, s := range card.Sections {
		byKey[s.Key] = s
	}
	require.Len(t, byKey[types.MemoryProfileSectionIdentity].Items, 1)
	require.Len(t, byKey[types.MemoryProfileSectionRole].Items, 1)
	require.Len(t, byKey[types.MemoryProfileSectionPreference].Items, 1)
	require.Empty(t, byKey[types.MemoryProfileSectionFact].Items)
	// identity + role + preference = (2+2+1)/6.
	require.InDelta(t, 5.0/6.0, card.Completeness, 1e-9)

	// Empty profile: zero completeness, all sections present (frontend shows
	// per-section empty states).
	emptySvc, _, _, _, _, _ := newMemoryServiceForTest(t)
	emptyCard, err := emptySvc.GetProfileCard(ctx)
	require.NoError(t, err)
	require.Len(t, emptyCard.Sections, 4)
	require.Zero(t, emptyCard.Completeness)
}

func TestMemoryServiceAgentTipsCardAndUpgradeLink(t *testing.T) {
	svc, _, _, _, _, _ := newMemoryServiceForTest(t)
	ctx := memoryTestCtx()

	// One extraction turn produces a feedback+skill pair sharing
	// (session, message); an unrelated feedback from another turn stays
	// unlinked.
	feedback := &types.MemoryFact{
		TenantID: 1, UserID: "alice", SessionID: "s-1", MessageID: "u1",
		Category: types.MemoryCategoryFeedback, Subject: "用户", Predicate: "评价", Object: "太啰嗦",
		Content: "用户反馈助手回答太啰嗦", Confidence: 0.95, Importance: 0.7, Status: types.MemoryStatusActive,
	}
	require.NoError(t, svc.memoryRepo.CreateFact(ctx, feedback))
	skill := &types.MemoryFact{
		TenantID: 1, UserID: "alice", SessionID: "s-1", MessageID: "u1",
		Category: types.MemoryCategorySkill, Subject: "assistant", Predicate: "回答风格", Object: "先给结论",
		Content: "给该用户的回答先给结论再展开", Confidence: 0.9, Importance: 0.8, Status: types.MemoryStatusActive,
	}
	require.NoError(t, svc.memoryRepo.CreateFact(ctx, skill))
	loneFeedback := &types.MemoryFact{
		TenantID: 1, UserID: "alice", SessionID: "s-2", MessageID: "u9",
		Category: types.MemoryCategoryFeedback, Subject: "用户", Predicate: "评价", Object: "很好",
		Content: "用户称赞了回答", Confidence: 0.9, Importance: 0.5, Status: types.MemoryStatusActive,
	}
	require.NoError(t, svc.memoryRepo.CreateFact(ctx, loneFeedback))

	card, err := svc.GetAgentTipsCard(ctx, 1, 20)
	require.NoError(t, err)
	require.Len(t, card.Skills, 1)
	require.EqualValues(t, 2, card.FeedbackTotal)

	byID := make(map[string]*types.AgentFeedbackItem, len(card.Feedback))
	for _, fb := range card.Feedback {
		byID[fb.ID] = fb
	}
	require.Equal(t, skill.ID, byID[feedback.ID].UpgradedTo, "feedback links to the skill of the same turn")
	require.Empty(t, byID[loneFeedback.ID].UpgradedTo)
}

func TestMemoryServiceModuleAggregationUserIsolation(t *testing.T) {
	svc, _, _, _, _, _ := newMemoryServiceForTest(t)

	// alice's memory.
	seedModuleFact(t, svc, types.MemoryCategorySoul, "称呼张工", 0.7)
	// bob's memory under the same tenant.
	bobFact := &types.MemoryFact{
		TenantID: 1, UserID: "bob", SessionID: "s-b", MessageID: "u-b",
		Category: types.MemoryCategorySoul, Subject: "用户", Predicate: "称呼", Object: "李工",
		Content: "bob 希望被称作李工", Confidence: 0.9, Importance: 0.7, Status: types.MemoryStatusActive,
	}
	require.NoError(t, svc.memoryRepo.CreateFact(memoryTestCtx(), bobFact))

	modules, err := svc.GetModuleOverview(memoryTestCtx())
	require.NoError(t, err)
	for _, m := range modules {
		if m.Module == types.MemoryModuleSoul {
			require.EqualValues(t, 1, m.FactCount, "bob's memories must never leak into alice's overview")
		}
	}

	card, err := svc.GetSoulCard(memoryTestCtx())
	require.NoError(t, err)
	for _, adj := range card.Adjustments {
		require.NotContains(t, adj.Content, "bob")
	}
}
