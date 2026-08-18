package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/config"
	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// ---------------------------------------------------------------------------
// Fakes (memoryFakeChat / memoryFakeModelService / memoryFakeEnqueuer from
// memory_test.go are reused — same package)
// ---------------------------------------------------------------------------

type precipFakeSessionRepo struct {
	interfaces.SessionRepository
	session *types.Session
	err     error
}

func (f *precipFakeSessionRepo) Get(ctx context.Context, tenantID uint64, userID, id string) (*types.Session, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.session, nil
}

type precipFakeMessageRepo struct {
	interfaces.MessageRepository
	messages []*types.Message
}

func (f *precipFakeMessageRepo) GetMessagesBySession(ctx context.Context, sessionID string, page, pageSize int) ([]*types.Message, error) {
	if len(f.messages) > pageSize {
		return f.messages[:pageSize], nil
	}
	return f.messages, nil
}

type precipFakeFavoriteRepo struct {
	interfaces.UserResourceFavoriteRepository
	favorite bool
}

func (f *precipFakeFavoriteRepo) IsFavorite(ctx context.Context, userID string, tenantID uint64, resourceType, resourceID string) (bool, error) {
	return f.favorite && resourceType == types.ResourceTypeSession, nil
}

type precipFakeKnowledgeRepo struct {
	interfaces.KnowledgeRepository
	stored map[string]*types.Knowledge
}

func (f *precipFakeKnowledgeRepo) GetKnowledgeByID(ctx context.Context, tenantID uint64, id string) (*types.Knowledge, error) {
	return f.stored[id], nil
}

func (f *precipFakeKnowledgeRepo) UpdateKnowledge(ctx context.Context, knowledge *types.Knowledge) error {
	f.stored[knowledge.ID] = knowledge
	return nil
}

type precipFakeKnowledgeService struct {
	interfaces.KnowledgeService
	list          []*types.Knowledge
	createPayload *types.ManualKnowledgePayload
	createChannel string
	createCalls   int
	updateCalls   int
	updateID      string
	created       *types.Knowledge
}

func (f *precipFakeKnowledgeService) CreateKnowledgeFromManual(ctx context.Context, kbID string, payload *types.ManualKnowledgePayload, channel string) (*types.Knowledge, error) {
	f.createCalls++
	f.createPayload = payload
	f.createChannel = channel
	f.created = &types.Knowledge{ID: "kn-1", TenantID: 1, KnowledgeBaseID: kbID, Title: payload.Title}
	return f.created, nil
}

func (f *precipFakeKnowledgeService) UpdateManualKnowledge(ctx context.Context, knowledgeID string, payload *types.ManualKnowledgePayload) (*types.Knowledge, error) {
	f.updateCalls++
	f.updateID = knowledgeID
	return &types.Knowledge{ID: knowledgeID}, nil
}

func (f *precipFakeKnowledgeService) ListKnowledgeByKnowledgeBaseID(ctx context.Context, kbID string) ([]*types.Knowledge, error) {
	return f.list, nil
}

type precipFakeKBService struct {
	interfaces.KnowledgeBaseService
	kbs     []*types.KnowledgeBase
	created *types.KnowledgeBase
}

func (f *precipFakeKBService) ListKnowledgeBasesByTenantID(ctx context.Context, tenantID uint64) ([]*types.KnowledgeBase, error) {
	return f.kbs, nil
}

func (f *precipFakeKBService) CreateKnowledgeBase(ctx context.Context, kb *types.KnowledgeBase) (*types.KnowledgeBase, error) {
	kb.ID = "kb-created"
	f.created = kb
	return kb, nil
}

func (f *precipFakeKBService) GetKnowledgeBaseByID(ctx context.Context, id string) (*types.KnowledgeBase, error) {
	for _, kb := range f.kbs {
		if kb.ID == id {
			return kb, nil
		}
	}
	return nil, fmt.Errorf("kb not found")
}

type precipFakeWikiService struct {
	interfaces.WikiPageService
	page         *types.WikiPage
	getErr       error
	created      *types.WikiPage
	updated      *types.WikiPage
	crossLinked  []string
	indexRebuilt bool
}

func (f *precipFakeWikiService) GetPageBySlug(ctx context.Context, kbID, slug string) (*types.WikiPage, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.page, nil
}

func (f *precipFakeWikiService) CreatePage(ctx context.Context, page *types.WikiPage) (*types.WikiPage, error) {
	f.created = page
	return page, nil
}

func (f *precipFakeWikiService) UpdatePage(ctx context.Context, page *types.WikiPage) (*types.WikiPage, error) {
	f.updated = page
	return page, nil
}

func (f *precipFakeWikiService) InjectCrossLinks(ctx context.Context, kbID string, slugs []string) {
	f.crossLinked = slugs
}

func (f *precipFakeWikiService) RebuildIndexPage(ctx context.Context, kbID string) error {
	f.indexRebuilt = true
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newPrecipitationServiceForTest() (*sessionPrecipitationService, *precipFakeSessionRepo, *precipFakeMessageRepo, *precipFakeFavoriteRepo, *precipFakeKnowledgeService, *precipFakeKBService, *precipFakeWikiService, *memoryFakeChat, *memoryFakeEnqueuer) {
	sessionRepo := &precipFakeSessionRepo{}
	msgRepo := &precipFakeMessageRepo{}
	favRepo := &precipFakeFavoriteRepo{}
	knRepo := &precipFakeKnowledgeRepo{stored: map[string]*types.Knowledge{}}
	knSvc := &precipFakeKnowledgeService{}
	kbSvc := &precipFakeKBService{}
	wikiSvc := &precipFakeWikiService{}
	chatModel := &memoryFakeChat{}
	enqueuer := &memoryFakeEnqueuer{}
	modelSvc := &memoryFakeModelService{
		chatModel: chatModel,
		models: []*types.Model{
			{ID: "chat-1", Type: types.ModelTypeKnowledgeQA},
			{ID: "emb-1", Type: types.ModelTypeEmbedding},
		},
	}
	svc := &sessionPrecipitationService{
		cfg:              &config.Config{Conversation: &config.ConversationConfig{}},
		sessionRepo:      sessionRepo,
		messageRepo:      msgRepo,
		favoriteRepo:     favRepo,
		knowledgeRepo:    knRepo,
		knowledgeService: knSvc,
		kbService:        kbSvc,
		wikiPageService:  wikiSvc,
		modelService:     modelSvc,
		taskEnqueuer:     enqueuer,
	}
	return svc, sessionRepo, msgRepo, favRepo, knSvc, kbSvc, wikiSvc, chatModel, enqueuer
}

func newAsynqTask(taskType string, payload []byte) *asynq.Task {
	return asynq.NewTask(taskType, payload)
}

func precipSession() *types.Session {
	return &types.Session{ID: "sess-12345678-abcd", TenantID: 1, UserID: "alice", Title: "部署方案讨论"}
}

func precipMessages(n int) []*types.Message {
	msgs := make([]*types.Message, 0, n)
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, &types.Message{ID: fmt.Sprintf("m-%d", i), SessionID: "sess-12345678-abcd", Role: role, Content: fmt.Sprintf("内容 %d", i)})
	}
	return msgs
}

// ---------------------------------------------------------------------------
// MaybeEnqueuePrecipitation
// ---------------------------------------------------------------------------

func TestMaybeEnqueue_FavoriteTrigger(t *testing.T) {
	svc, _, msgRepo, favRepo, _, _, _, _, enqueuer := newPrecipitationServiceForTest()
	favRepo.favorite = true
	msgRepo.messages = precipMessages(2) // below follow-up threshold

	svc.MaybeEnqueuePrecipitation(memoryTestCtx(), &types.SessionPrecipitatePayload{
		TenantID: 1, UserID: "alice", SessionID: "sess-12345678-abcd",
	})

	require.Len(t, enqueuer.tasks, 1)
	require.Equal(t, types.TypeSessionPrecipitate, enqueuer.tasks[0].Type())
	var payload types.SessionPrecipitatePayload
	require.NoError(t, json.Unmarshal(enqueuer.tasks[0].Payload(), &payload))
	require.Equal(t, "favorite", payload.Trigger)
	require.Equal(t, "sess-12345678-abcd", payload.SessionID)
}

func TestMaybeEnqueue_FollowupsTrigger(t *testing.T) {
	svc, _, msgRepo, favRepo, _, _, _, _, enqueuer := newPrecipitationServiceForTest()
	favRepo.favorite = false
	msgRepo.messages = precipMessages(types.DefaultPrecipitateMessageThreshold)

	svc.MaybeEnqueuePrecipitation(memoryTestCtx(), &types.SessionPrecipitatePayload{
		TenantID: 1, UserID: "alice", SessionID: "sess-12345678-abcd",
	})

	require.Len(t, enqueuer.tasks, 1)
	var payload types.SessionPrecipitatePayload
	require.NoError(t, json.Unmarshal(enqueuer.tasks[0].Payload(), &payload))
	require.Equal(t, "followups", payload.Trigger)
}

func TestMaybeEnqueue_LowValueSessionSkipped(t *testing.T) {
	svc, _, msgRepo, favRepo, _, _, _, _, enqueuer := newPrecipitationServiceForTest()
	favRepo.favorite = false
	msgRepo.messages = precipMessages(4)

	svc.MaybeEnqueuePrecipitation(memoryTestCtx(), &types.SessionPrecipitatePayload{
		TenantID: 1, UserID: "alice", SessionID: "sess-12345678-abcd",
	})

	require.Empty(t, enqueuer.tasks)
}

func TestMaybeEnqueue_NilPayloadSkipped(t *testing.T) {
	svc, _, _, _, _, _, _, _, enqueuer := newPrecipitationServiceForTest()
	svc.MaybeEnqueuePrecipitation(memoryTestCtx(), nil)
	svc.MaybeEnqueuePrecipitation(memoryTestCtx(), &types.SessionPrecipitatePayload{})
	require.Empty(t, enqueuer.tasks)
}

// ---------------------------------------------------------------------------
// ProcessSessionPrecipitate
// ---------------------------------------------------------------------------

func TestProcessSessionPrecipitate_CreatesDocumentWithSessionSource(t *testing.T) {
	svc, sessionRepo, msgRepo, _, knSvc, kbSvc, _, chatModel, _ := newPrecipitationServiceForTest()
	sessionRepo.session = precipSession()
	msgRepo.messages = precipMessages(6)
	chatModel.response = "# 部署方案\n\n## 关键结论\n- 使用 Helm 管理部署"
	kbSvc.kbs = []*types.KnowledgeBase{{ID: "kb-insights", Name: types.SessionPrecipitationKBName, TenantID: 1}}

	payload, _ := json.Marshal(types.SessionPrecipitatePayload{
		TenantID: 1, UserID: "alice", SessionID: "sess-12345678-abcd", Trigger: "favorite",
	})
	err := svc.ProcessSessionPrecipitate(context.Background(), newAsynqTask(types.TypeSessionPrecipitate, payload))
	require.NoError(t, err)

	// Draft create → provenance stamp → publish update.
	require.Equal(t, 1, knSvc.createCalls)
	require.Equal(t, types.ManualKnowledgeStatusDraft, knSvc.createPayload.Status)
	require.Equal(t, types.SessionPrecipitationChannel, knSvc.createChannel)
	require.Contains(t, knSvc.createPayload.Title, "会话沉淀")
	require.Contains(t, knSvc.createPayload.Title, "sess-123")
	require.Equal(t, 1, knSvc.updateCalls) // publish flip

	// The provenance marker must be stamped between draft create and publish.
	stored := svc.knowledgeRepo.(*precipFakeKnowledgeRepo).stored["kn-1"]
	require.NotNil(t, stored)
	require.Equal(t, types.SessionKnowledgeSource("sess-12345678-abcd"), stored.Source)
}

func TestProcessSessionPrecipitate_UpdatesExistingDocument(t *testing.T) {
	svc, sessionRepo, msgRepo, _, knSvc, kbSvc, _, chatModel, _ := newPrecipitationServiceForTest()
	sessionRepo.session = precipSession()
	msgRepo.messages = precipMessages(6)
	chatModel.response = "# 更新后的沉淀文档"
	kbSvc.kbs = []*types.KnowledgeBase{{ID: "kb-insights", Name: types.SessionPrecipitationKBName, TenantID: 1}}
	knSvc.list = []*types.Knowledge{{
		ID: "kn-existing", TenantID: 1, KnowledgeBaseID: "kb-insights",
		Source: types.SessionKnowledgeSource("sess-12345678-abcd"),
	}}

	payload, _ := json.Marshal(types.SessionPrecipitatePayload{
		TenantID: 1, UserID: "alice", SessionID: "sess-12345678-abcd", Trigger: "followups",
	})
	err := svc.ProcessSessionPrecipitate(context.Background(), newAsynqTask(types.TypeSessionPrecipitate, payload))
	require.NoError(t, err)

	require.Equal(t, 0, knSvc.createCalls, "must not duplicate the document")
	require.Equal(t, 1, knSvc.updateCalls)
	require.Equal(t, "kn-existing", knSvc.updateID)
}

func TestProcessSessionPrecipitate_DeletedSessionIsNoop(t *testing.T) {
	svc, sessionRepo, _, _, knSvc, _, _, _, _ := newPrecipitationServiceForTest()
	sessionRepo.err = apperrors.ErrSessionNotFound

	payload, _ := json.Marshal(types.SessionPrecipitatePayload{TenantID: 1, UserID: "alice", SessionID: "gone"})
	require.NoError(t, svc.ProcessSessionPrecipitate(context.Background(), newAsynqTask(types.TypeSessionPrecipitate, payload)))
	require.Equal(t, 0, knSvc.createCalls)
}

func TestProcessSessionPrecipitate_AutoCreatesHiddenKB(t *testing.T) {
	svc, sessionRepo, msgRepo, _, knSvc, kbSvc, _, chatModel, _ := newPrecipitationServiceForTest()
	sessionRepo.session = precipSession()
	msgRepo.messages = precipMessages(6)
	chatModel.response = "# 沉淀"
	kbSvc.kbs = nil // no precipitation KB yet

	payload, _ := json.Marshal(types.SessionPrecipitatePayload{
		TenantID: 1, UserID: "alice", SessionID: "sess-12345678-abcd",
	})
	require.NoError(t, svc.ProcessSessionPrecipitate(context.Background(), newAsynqTask(types.TypeSessionPrecipitate, payload)))

	require.NotNil(t, kbSvc.created)
	require.Equal(t, types.SessionPrecipitationKBName, kbSvc.created.Name)
	require.True(t, kbSvc.created.IsTemporary, "precipitation KB must be hidden like __chat_history__")
	require.Equal(t, "emb-1", kbSvc.created.EmbeddingModelID)
	require.Equal(t, 1, knSvc.createCalls)
}

// ---------------------------------------------------------------------------
// CreateWikiFromSession
// ---------------------------------------------------------------------------

const precipWikiLLMJSON = `{"title":"Prometheus 部署方案","summary":"讨论并确定了基于 Helm 的 Prometheus 部署方案。","content":"## 概述\n团队决定采用 [[helm]] 管理 [[prometheus]] 部署。"}`

func TestCreateWikiFromSession_RejectsNonWikiKB(t *testing.T) {
	svc, _, _, _, _, kbSvc, _, _, _ := newPrecipitationServiceForTest()
	kbSvc.kbs = []*types.KnowledgeBase{{ID: "kb-doc", Type: types.KnowledgeBaseTypeDocument, TenantID: 1}}

	_, err := svc.CreateWikiFromSession(memoryTestCtx(), "sess-12345678-abcd", &types.SessionWikiRequest{KnowledgeBaseID: "kb-doc"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a wiki")
}

func TestCreateWikiFromSession_PropagatesSessionNotFound(t *testing.T) {
	svc, sessionRepo, _, _, _, kbSvc, _, _, _ := newPrecipitationServiceForTest()
	sessionRepo.err = apperrors.ErrSessionNotFound
	kbSvc.kbs = []*types.KnowledgeBase{{ID: "kb-wiki", Type: types.KnowledgeBaseTypeWiki, TenantID: 1}}

	_, err := svc.CreateWikiFromSession(memoryTestCtx(), "gone", &types.SessionWikiRequest{KnowledgeBaseID: "kb-wiki"})
	require.ErrorIs(t, err, apperrors.ErrSessionNotFound)
}

func TestCreateWikiFromSession_CreatesLinkedPage(t *testing.T) {
	svc, sessionRepo, msgRepo, _, _, kbSvc, wikiSvc, chatModel, _ := newPrecipitationServiceForTest()
	sessionRepo.session = precipSession()
	msgRepo.messages = precipMessages(4)
	chatModel.response = precipWikiLLMJSON
	kbSvc.kbs = []*types.KnowledgeBase{{ID: "kb-wiki", Type: types.KnowledgeBaseTypeWiki, TenantID: 1}}
	wikiSvc.getErr = repository.ErrWikiPageNotFound

	page, err := svc.CreateWikiFromSession(memoryTestCtx(), "sess-12345678-abcd", &types.SessionWikiRequest{KnowledgeBaseID: "kb-wiki"})
	require.NoError(t, err)
	require.NotNil(t, wikiSvc.created)
	require.Nil(t, wikiSvc.updated)

	require.Equal(t, types.SessionWikiSlug("sess-12345678-abcd"), page.Slug)
	require.Equal(t, "Prometheus 部署方案", page.Title)
	require.Equal(t, types.WikiPageTypeSynthesis, page.PageType)
	// 对话→知识→对话 loop: the page links back to its triggering session.
	require.Contains(t, page.SourceRefs, types.SessionSourceRef("sess-12345678-abcd", "部署方案讨论"))
	require.Contains(t, string(page.PageMetadata), "\"source_session_id\":\"sess-12345678-abcd\"")
	// Index + cross-links keep the wiki navigable.
	require.True(t, wikiSvc.indexRebuilt)
	require.Equal(t, []string{types.SessionWikiSlug("sess-12345678-abcd")}, wikiSvc.crossLinked)
}

func TestCreateWikiFromSession_RefreshesExistingPage(t *testing.T) {
	svc, sessionRepo, msgRepo, _, _, kbSvc, wikiSvc, chatModel, _ := newPrecipitationServiceForTest()
	sessionRepo.session = precipSession()
	msgRepo.messages = precipMessages(4)
	chatModel.response = precipWikiLLMJSON
	kbSvc.kbs = []*types.KnowledgeBase{{ID: "kb-wiki", Type: types.KnowledgeBaseTypeWiki, TenantID: 1}}
	wikiSvc.page = &types.WikiPage{
		ID: "page-1", KnowledgeBaseID: "kb-wiki", Slug: types.SessionWikiSlug("sess-12345678-abcd"),
		Title: "旧标题", SourceRefs: types.StringArray{types.SessionSourceRef("sess-12345678-abcd", "部署方案讨论")},
	}

	_, err := svc.CreateWikiFromSession(memoryTestCtx(), "sess-12345678-abcd", &types.SessionWikiRequest{KnowledgeBaseID: "kb-wiki"})
	require.NoError(t, err)
	require.Nil(t, wikiSvc.created, "second click must refresh, not recreate")
	require.NotNil(t, wikiSvc.updated)
	require.Equal(t, "Prometheus 部署方案", wikiSvc.updated.Title)
	// Source ref must not be duplicated on refresh.
	require.Len(t, wikiSvc.updated.SourceRefs, 1)
}

func TestCreateWikiFromSession_TitleOverride(t *testing.T) {
	svc, sessionRepo, msgRepo, _, _, kbSvc, wikiSvc, chatModel, _ := newPrecipitationServiceForTest()
	sessionRepo.session = precipSession()
	msgRepo.messages = precipMessages(4)
	chatModel.response = precipWikiLLMJSON
	kbSvc.kbs = []*types.KnowledgeBase{{ID: "kb-wiki", Type: types.KnowledgeBaseTypeWiki, TenantID: 1}}
	wikiSvc.getErr = repository.ErrWikiPageNotFound

	page, err := svc.CreateWikiFromSession(memoryTestCtx(), "sess-12345678-abcd", &types.SessionWikiRequest{
		KnowledgeBaseID: "kb-wiki", Title: "自定义标题",
	})
	require.NoError(t, err)
	require.Equal(t, "自定义标题", page.Title)
}

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

func TestPrecipitatedTitleCarriesSessionMarker(t *testing.T) {
	title := precipitatedTitle(precipSession())
	require.True(t, strings.HasPrefix(title, "会话沉淀 · 部署方案讨论 · "))
	require.True(t, strings.HasSuffix(title, "sess-123"))
}

func TestFindSessionDocumentFallsBackToTitle(t *testing.T) {
	svc, _, _, _, knSvc, _, _, _, _ := newPrecipitationServiceForTest()
	// Source was clobbered to "manual" (e.g. raced with the indexing task);
	// the title marker must still dedup.
	knSvc.list = []*types.Knowledge{{
		ID: "kn-x", KnowledgeBaseID: "kb-insights", Source: "manual",
		Title: "会话沉淀 · 部署方案讨论 · sess-123",
	}}
	found, err := svc.findSessionDocument(memoryTestCtx(), "kb-insights", "sess-12345678-abcd")
	require.NoError(t, err)
	require.NotNil(t, found)
	require.Equal(t, "kn-x", found.ID)
}
