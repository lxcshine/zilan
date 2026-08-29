<template>
    <!-- 文档模式画布（PRD §9「文档」页）：
         · 列表态（/platform/documents）：概览统计卡片 + 最近文档流
         · 详情态（/platform/documents/:kbId/:knowledgeId）：解析进度时间线 + 元信息 + 操作 -->
    <div class="documents-view">
        <!-- ===== 详情态 ===== -->
        <template v-if="knowledgeId">
            <div class="doc-detail">
                <div class="doc-detail__topbar">
                    <button type="button" class="doc-detail__back" :aria-label="t('documents.backToList')"
                        @click="router.push('/platform/documents')">
                        <t-icon name="chevron-left" size="18px" />
                        <span>{{ t('documents.backToList') }}</span>
                    </button>
                    <div class="doc-detail__actions">
                        <t-button size="small" variant="outline" :loading="reparsing" :disabled="reparsing"
                            @click="handleReparse">
                            {{ t('documents.reparse') }}
                        </t-button>
                        <t-button size="small" theme="danger" variant="outline" :disabled="deleting"
                            @click="handleDelete">
                            {{ t('upload.deleteRecord') }}
                        </t-button>
                    </div>
                </div>

                <div class="doc-detail__card">
                    <div class="doc-detail__header">
                        <div class="doc-detail__title-block">
                            <h1 class="doc-detail__title">{{ docMeta?.title || t('panel.untitledDocument') }}</h1>
                            <div class="doc-detail__meta">
                                <button type="button" class="doc-detail__kb-link"
                                    @click="router.push(`/platform/knowledge-bases/${kbId}`)">
                                    <t-icon name="books" size="13px" />
                                    {{ docMeta?.kbName || '' }}
                                </button>
                                <span v-if="docMeta?.file_type" class="doc-detail__meta-item">{{ docMeta.file_type
                                    }}</span>
                                <span class="doc-detail__meta-item">{{ statusLabel }}</span>
                                <span v-if="docMeta?.updated_at" class="doc-detail__meta-item">{{ docMeta.updated_at
                                    }}</span>
                            </div>
                        </div>
                    </div>

                    <div class="doc-detail__timeline">
                        <KnowledgeProcessingTimeline v-if="knowledgeId" :key="knowledgeId" :knowledge-id="knowledgeId"
                            :parse-status="docMeta?.parse_status || ''" :doc-title="docMeta?.title || ''"
                            :auto-poll="true" @update:summary="onTimelineSummary" />
                        <div v-else class="doc-detail__placeholder">{{ t('documents.loadingDoc') }}</div>
                    </div>
                </div>
            </div>
        </template>

        <!-- ===== 列表态（概览） ===== -->
        <template v-else>
            <div class="docs-overview">
                <header class="docs-overview__header">
                    <h1 class="docs-overview__title">{{ t('rail.documents') }}</h1>
                    <p class="docs-overview__subtitle">{{ t('documents.subtitle') }}</p>
                </header>

                <!-- 统计卡片 -->
                <div class="docs-overview__stats">
                    <div class="stat-card">
                        <span class="stat-card__value">{{ totalCount }}</span>
                        <span class="stat-card__label">{{ t('documents.totalDocuments') }}</span>
                    </div>
                    <div class="stat-card">
                        <span class="stat-card__value">{{ processingCount }}</span>
                        <span class="stat-card__label">{{ t('panel.statusProcessing') }}</span>
                    </div>
                    <div class="stat-card">
                        <span class="stat-card__value">{{ completedCount }}</span>
                        <span class="stat-card__label">{{ t('panel.statusCompleted') }}</span>
                    </div>
                    <div class="stat-card">
                        <span class="stat-card__value">{{ failedCount }}</span>
                        <span class="stat-card__label">{{ t('panel.statusFailed') }}</span>
                    </div>
                </div>

                <!-- 最近文档流 -->
                <div class="docs-overview__list">
                    <div class="docs-overview__list-header">
                        <span>{{ t('documents.recentDocuments') }}</span>
                        <t-button size="small" variant="text" :loading="loading" @click="refresh">
                            {{ t('chat.refreshSuggestedQuestions') }}
                        </t-button>
                    </div>
                    <template v-if="loading && documents.length === 0">
                        <div v-for="n in 5" :key="'ov-skel-' + n" class="docs-overview__row docs-overview__row--skeleton">
                            <t-skeleton animation="gradient" :row-col="[{ width: '100%', height: '14px' }]" />
                        </div>
                    </template>
                    <div v-else-if="documents.length === 0" class="docs-overview__empty">
                        {{ t('panel.noDocuments') }}
                    </div>
                    <template v-else>
                        <button v-for="doc in documents.slice(0, 20)" :key="doc.id" type="button"
                            class="docs-overview__row" @click="openDocument(doc)">
                            <span class="docs-overview__status"
                                :class="`docs-overview__status--${statusGroup(doc.parse_status)}`"></span>
                            <span class="docs-overview__name">{{ doc.title || t('panel.untitledDocument') }}</span>
                            <span class="docs-overview__kb">{{ doc.kbName }}</span>
                        </button>
                    </template>
                </div>
            </div>
        </template>
    </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { MessagePlugin, DialogPlugin } from 'tdesign-vue-next'
import KnowledgeProcessingTimeline from '@/components/knowledge-processing-timeline.vue'
import { getKnowledgeDetails, reparseKnowledge, delKnowledgeDetails } from '@/api/knowledge-base'
import { useGlobalDocuments, docStatusGroup, type GlobalDocumentItem } from '@/composables/useGlobalDocuments'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()

const { documents, loading, ensure, refresh } = useGlobalDocuments()

const statusGroup = docStatusGroup

const kbId = computed(() => (route.params as any)?.kbId as string | undefined)
const knowledgeId = computed(() => (route.params as any)?.knowledgeId as string | undefined)

const docMeta = ref<any>(null)
const reparsing = ref(false)
const deleting = ref(false)

const totalCount = computed(() => documents.value.length)
const processingCount = computed(() => countBy('processing'))
const completedCount = computed(() => countBy('completed'))
const failedCount = computed(() => countBy('failed'))

const countBy = (group: 'processing' | 'completed' | 'failed') =>
    documents.value.filter((doc) => statusGroup(doc.parse_status) === group).length

const statusLabel = computed(() => {
    if (!docMeta.value) return ''
    switch (statusGroup(docMeta.value.parse_status)) {
        case 'processing': return t('panel.statusProcessing')
        case 'completed': return t('panel.statusCompleted')
        case 'failed': return t('panel.statusFailed')
        default: return docMeta.value.parse_status || ''
    }
})

const openDocument = (doc: GlobalDocumentItem) => {
    router.push(`/platform/documents/${doc.kbId}/${doc.id}`)
}

const loadDocMeta = async () => {
    if (!knowledgeId.value) {
        docMeta.value = null
        return
    }
    // 先用聚合列表里的信息即时渲染，再拉详情补齐
    const fromCache = documents.value.find((doc) => doc.id === knowledgeId.value)
    docMeta.value = fromCache ? { ...fromCache } : null
    try {
        const res: any = await getKnowledgeDetails(knowledgeId.value)
        const data = res?.data || {}
        docMeta.value = {
            ...docMeta.value,
            ...data,
            kbName: docMeta.value?.kbName || data.kb_name || '',
        }
    } catch {
        // 详情拉取失败时保持聚合缓存信息
    }
}

const onTimelineSummary = (summary?: { status?: string }) => {
    // 时间线轮询到终态（状态变化）后刷新聚合列表一次，保持概览/列表面板状态一致；
    // 避免每次 poll 都全量刷新跨库聚合。
    const status = summary?.status || ''
    if (status && status !== lastTimelineStatus) {
        lastTimelineStatus = status
        if (['completed', 'failed', 'timeout', 'error'].includes(status.toLowerCase())) {
            void refresh()
        }
    }
}
let lastTimelineStatus = ''

const handleReparse = async () => {
    if (!knowledgeId.value || reparsing.value) return
    reparsing.value = true
    try {
        await reparseKnowledge(knowledgeId.value)
        MessagePlugin.success(t('documents.reparseStarted'))
        void refresh()
    } catch {
        MessagePlugin.error(t('documents.reparseFailed'))
    } finally {
        reparsing.value = false
    }
}

const handleDelete = () => {
    if (!knowledgeId.value) return
    const confirmDialog = DialogPlugin.confirm({
        header: t('documents.deleteConfirmTitle'),
        body: t('documents.deleteConfirmBody'),
        confirmBtn: { content: t('batchManage.delete'), theme: 'danger' as const },
        cancelBtn: t('batchManage.cancel'),
        theme: 'warning',
        onConfirm: async () => {
            deleting.value = true
            try {
                await delKnowledgeDetails(knowledgeId.value!)
                MessagePlugin.success(t('documents.deleteSuccess'))
                await refresh()
                router.push('/platform/documents')
            } catch {
                MessagePlugin.error(t('documents.deleteFailed'))
            } finally {
                deleting.value = false
                confirmDialog.destroy()
            }
        },
    })
}

watch(knowledgeId, () => {
    void loadDocMeta()
})

onMounted(() => {
    void ensure()
    void loadDocMeta()
})
</script>

<style lang="less" scoped>
.documents-view {
    flex: 1;
    min-width: 0;
    min-height: 0;
    display: flex;
    flex-direction: column;
    overflow-y: auto;
    background: var(--td-bg-color-page);
}

// ===== 概览态 =====
.docs-overview {
    width: min(960px, 100%);
    margin: 0 auto;
    padding: 32px 24px 48px;
    display: flex;
    flex-direction: column;
    gap: 24px;
}

.docs-overview__header {
    .docs-overview__title {
        margin: 0;
        font: var(--td-font-title-large);
        color: var(--td-text-color-primary);
    }

    .docs-overview__subtitle {
        margin: 4px 0 0;
        font: var(--td-font-body-small);
        color: var(--td-text-color-placeholder);
    }
}

.docs-overview__stats {
    display: grid;
    grid-template-columns: repeat(4, 1fr);
    gap: 16px;

    @media (max-width: 900px) {
        grid-template-columns: repeat(2, 1fr);
    }
}

.stat-card {
    display: flex;
    flex-direction: column;
    gap: 4px;
    padding: 20px;
    border-radius: var(--td-radius-large);
    background: var(--td-bg-color-container);
    border: 1px solid var(--td-component-stroke);

    .stat-card__value {
        font: 600 24px/32px var(--app-font-family);
        color: var(--td-text-color-primary);
        font-variant-numeric: tabular-nums;
    }

    .stat-card__label {
        font: var(--td-font-link-small);
        color: var(--td-text-color-placeholder);
    }
}

.docs-overview__list {
    display: flex;
    flex-direction: column;
    border-radius: var(--td-radius-large);
    background: var(--td-bg-color-container);
    border: 1px solid var(--td-component-stroke);
    overflow: hidden;
}

.docs-overview__list-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 12px 20px;
    border-bottom: 1px solid var(--td-component-stroke);
    font: var(--td-font-title-small);
    color: var(--td-text-color-primary);
}

.docs-overview__row {
    width: 100%;
    display: flex;
    align-items: center;
    gap: 12px;
    padding: 12px 20px;
    border: none;
    border-bottom: 1px solid var(--td-component-stroke);
    background: transparent;
    cursor: pointer;
    text-align: left;
    transition: background-color 0.15s ease;

    &:last-child {
        border-bottom: none;
    }

    &:hover {
        background: var(--td-bg-color-container-hover);
    }

    &:focus-visible {
        outline: 2px solid var(--td-brand-color);
        outline-offset: -2px;
    }

    &--skeleton {
        cursor: default;
    }
}

.docs-overview__status {
    width: 8px;
    height: 8px;
    flex-shrink: 0;
    border-radius: 999px;
    background: var(--td-gray-color-6);

    &--processing {
        background: var(--td-warning-color);
    }

    &--failed {
        background: var(--td-error-color);
    }
}

.docs-overview__name {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: var(--td-text-color-primary);
    font: var(--td-font-body-medium);
}

.docs-overview__kb {
    flex-shrink: 0;
    max-width: 200px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: var(--td-text-color-placeholder);
    font: var(--td-font-link-small);
}

.docs-overview__empty {
    padding: 48px 16px;
    text-align: center;
    color: var(--td-text-color-placeholder);
    font: var(--td-font-body-small);
}

// ===== 详情态 =====
.doc-detail {
    width: min(960px, 100%);
    margin: 0 auto;
    padding: 20px 24px 48px;
    display: flex;
    flex-direction: column;
    gap: 16px;
}

.doc-detail__topbar {
    display: flex;
    align-items: center;
    justify-content: space-between;
}

.doc-detail__back {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    padding: 6px 10px;
    border: none;
    border-radius: var(--td-radius-default);
    background: transparent;
    color: var(--td-text-color-secondary);
    font: var(--td-font-body-small);
    cursor: pointer;
    transition: background-color 0.15s ease, color 0.15s ease;

    &:hover {
        background: var(--td-bg-color-container-hover);
        color: var(--td-text-color-primary);
    }

    &:focus-visible {
        outline: 2px solid var(--td-brand-color);
        outline-offset: 1px;
    }
}

.doc-detail__actions {
    display: flex;
    align-items: center;
    gap: 8px;
}

.doc-detail__card {
    display: flex;
    flex-direction: column;
    border-radius: var(--td-radius-large);
    background: var(--td-bg-color-container);
    border: 1px solid var(--td-component-stroke);
    overflow: hidden;
}

.doc-detail__header {
    padding: 20px 24px;
    border-bottom: 1px solid var(--td-component-stroke);
}

.doc-detail__title {
    margin: 0 0 8px;
    font: var(--td-font-title-large);
    color: var(--td-text-color-primary);
    word-break: break-all;
}

.doc-detail__meta {
    display: flex;
    align-items: center;
    gap: 12px;
    flex-wrap: wrap;
    font: var(--td-font-link-small);
    color: var(--td-text-color-placeholder);
}

.doc-detail__kb-link {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    padding: 2px 8px;
    border: 1px solid var(--td-component-stroke);
    border-radius: 999px;
    background: transparent;
    color: var(--td-text-color-secondary);
    font: var(--td-font-link-small);
    cursor: pointer;
    transition: border-color 0.15s ease, color 0.15s ease;

    &:hover {
        border-color: var(--td-brand-color);
        color: var(--td-brand-color);
    }
}

.doc-detail__meta-item {
    white-space: nowrap;
}

.doc-detail__timeline {
    padding: 20px 24px;
    max-height: 60vh;
    overflow-y: auto;
}

.doc-detail__placeholder {
    padding: 32px 0;
    text-align: center;
    color: var(--td-text-color-placeholder);
    font: var(--td-font-body-small);
}
</style>
