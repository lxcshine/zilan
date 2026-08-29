<template>
    <!-- 文档模式列表（PRD §5.3）：跨库聚合 + 解析状态筛选 + 时间线归档 -->
    <div class="docs-panel">
        <!-- 状态筛选 chips -->
        <div class="docs-panel__chips" role="tablist" :aria-label="t('panel.filterByStatus')">
            <button v-for="opt in statusOptions" :key="opt.value" type="button" class="docs-panel__chip"
                :class="{ 'docs-panel__chip--active': statusFilter === opt.value }" role="tab"
                :aria-selected="statusFilter === opt.value" @click="statusFilter = opt.value">
                {{ opt.label }}
                <span v-if="opt.count > 0" class="docs-panel__chip-count">{{ opt.count }}</span>
            </button>
        </div>

        <div class="docs-panel__list" ref="scrollContainer">
            <template v-if="loading && documents.length === 0">
                <div v-for="n in 6" :key="'doc-skel-' + n" class="docs-panel__row docs-panel__row--skeleton">
                    <t-skeleton animation="gradient" :row-col="[{ width: '100%', height: '14px' }]" />
                </div>
            </template>
            <div v-else-if="groupedDocs.length === 0" class="docs-panel__empty">
                {{ t('panel.noDocuments') }}
            </div>
            <template v-else>
                <template v-for="group in groupedDocs" :key="group.label">
                    <div class="docs-panel__group">{{ group.label }}</div>
                    <button v-for="doc in group.items" :key="doc.id" type="button" class="docs-panel__row"
                        :class="{ 'docs-panel__row--active': doc.id === activeDocId }" :title="doc.title"
                        @click="openDocument(doc)">
                        <span class="docs-panel__status" :class="`docs-panel__status--${statusGroup(doc.parse_status)}`">
                            <t-icon v-if="statusGroup(doc.parse_status) === 'processing'" name="loading" size="12px" />
                        </span>
                        <span class="docs-panel__body">
                            <span class="docs-panel__name">{{ doc.title || t('panel.untitledDocument') }}</span>
                            <span class="docs-panel__meta">{{ doc.kbName }}</span>
                        </span>
                    </button>
                </template>
            </template>
        </div>

        <!-- 上传文档：先选目标库（PRD §9） -->
        <div v-if="kbPickerVisible" class="docs-panel__picker-popover">
            <div class="docs-panel__picker">
                <div class="docs-panel__picker-title">{{ t('panel.chooseKnowledgeBase') }}</div>
                <div v-if="kbs.length === 0" class="docs-panel__picker-empty">{{ t('panel.noKnowledgeBases') }}</div>
                <button v-for="kb in kbs" :key="kb.id" type="button" class="docs-panel__picker-item"
                    @click="chooseTargetKb(kb)">
                    {{ kb.name }}
                </button>
            </div>
        </div>
    </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { listKnowledgeBases } from '@/api/knowledge-base'
import { useGlobalDocuments, docStatusGroup, type GlobalDocumentItem } from '@/composables/useGlobalDocuments'
import { classifyDateBucket, type DateBucketKey } from '@/components/sessionGrouping'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()

const { documents, loading, ensure, refresh } = useGlobalDocuments()

const statusFilter = ref<'all' | 'processing' | 'completed' | 'failed'>('all')
const kbPickerVisible = ref(false)
const kbs = ref<any[]>([])

const activeDocId = computed(() => (route.params as any)?.knowledgeId as string | undefined)

const statusGroup = docStatusGroup

const statusOptions = computed(() => [
    { value: 'all' as const, label: t('panel.statusAll'), count: documents.value.length },
    { value: 'processing' as const, label: t('panel.statusProcessing'), count: countBy('processing') },
    { value: 'completed' as const, label: t('panel.statusCompleted'), count: countBy('completed') },
    { value: 'failed' as const, label: t('panel.statusFailed'), count: countBy('failed') },
])

const countBy = (group: 'processing' | 'completed' | 'failed') =>
    documents.value.filter((doc) => docStatusGroup(doc.parse_status) === group).length

const dateBucketLabels = computed<Record<DateBucketKey, string>>(() => ({
    pinned: t('time.pinned'),
    today: t('time.today'),
    yesterday: t('time.yesterday'),
    last7Days: t('time.last7Days'),
    last30Days: t('time.last30Days'),
    lastYear: t('time.lastYear'),
    earlier: t('time.earlier'),
}))

const groupedDocs = computed(() => {
    const filtered = statusFilter.value === 'all'
        ? documents.value
        : documents.value.filter((doc) => docStatusGroup(doc.parse_status) === statusFilter.value)

    const order: DateBucketKey[] = ['pinned', 'today', 'yesterday', 'last7Days', 'last30Days', 'lastYear', 'earlier']
    const groups = new Map<DateBucketKey, GlobalDocumentItem[]>()
    for (const doc of filtered) {
        const key = classifyDateBucket(doc.updated_at || doc.created_at)
        if (!groups.has(key)) groups.set(key, [])
        groups.get(key)!.push(doc)
    }
    return order
        .filter((key) => groups.has(key))
        .map((key) => ({ label: dateBucketLabels.value[key], items: groups.get(key)! }))
})

const openDocument = (doc: GlobalDocumentItem) => {
    router.push(`/platform/documents/${doc.kbId}/${doc.id}`)
}

/** 供 ListPanel 主操作「上传文档」调用：弹出目标库选择 */
const openKbPicker = async () => {
    if (kbs.value.length === 0) {
        try {
            const res: any = await listKnowledgeBases()
            kbs.value = res?.data || []
        } catch {
            kbs.value = []
        }
    }
    kbPickerVisible.value = true
}

const chooseTargetKb = (kb: any) => {
    kbPickerVisible.value = false
    // 进入目标库详情页完成上传（画布内拖拽/上传区）
    router.push(`/platform/knowledge-bases/${kb.id}`)
}

/** 文档画布会触发刷新（重新解析/删除后） */
defineExpose({ openKbPicker, refresh })

onMounted(() => {
    void ensure()
})
</script>

<style lang="less" scoped>
.docs-panel {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
    overflow: hidden;
    position: relative;
}

.docs-panel__picker-popover {
    position: absolute;
    top: 4px;
    left: 12px;
    right: 12px;
    z-index: 20;
    border-radius: var(--td-radius-large);
    background: var(--td-bg-color-container);
    border: 1px solid var(--td-component-border);
    box-shadow: var(--td-shadow-2);
}

.docs-panel__chips {
    display: flex;
    align-items: center;
    gap: 4px;
    padding: 8px 12px 4px;
    flex-wrap: wrap;
    flex-shrink: 0;
}

.docs-panel__chip {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    height: 24px;
    padding: 0 10px;
    border: 1px solid var(--td-component-stroke);
    border-radius: 999px;
    background: transparent;
    color: var(--td-text-color-secondary);
    font: var(--td-font-link-small);
    cursor: pointer;
    transition: all 0.15s ease;

    &:hover {
        border-color: var(--td-brand-color);
        color: var(--td-brand-color);
    }

    &--active {
        background: var(--td-brand-color);
        border-color: var(--td-brand-color);
        color: var(--td-text-color-anti);
    }

    .docs-panel__chip-count {
        font-variant-numeric: tabular-nums;
        opacity: 0.75;
    }
}

.docs-panel__list {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
    padding: 0 8px;
}

.docs-panel__group {
    padding: 10px 8px 4px;
    font: var(--td-font-link-small);
    color: var(--td-font-gray-3);
}

.docs-panel__row {
    width: 100%;
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 6px 8px;
    border: none;
    border-radius: var(--td-radius-default);
    background: transparent;
    cursor: pointer;
    transition: background-color 0.15s ease;

    &:hover {
        background: var(--td-bg-color-container-hover);
    }

    &:focus-visible {
        outline: 2px solid var(--td-brand-color);
        outline-offset: -2px;
    }

    &--active {
        background: var(--td-bg-color-container-hover);
        box-shadow: inset 2px 0 0 var(--td-brand-color);
    }

    &--skeleton {
        cursor: default;
        height: 40px;
    }
}

.docs-panel__status {
    width: 14px;
    height: 14px;
    flex-shrink: 0;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    border-radius: 999px;

    &--processing {
        color: var(--td-warning-color);
        animation: docs-spin 1.2s linear infinite;
    }

    &--completed {
        &::after {
            content: '';
            width: 6px;
            height: 6px;
            border-radius: 999px;
            background: var(--td-gray-color-6);
        }
    }

    &--failed {
        &::after {
            content: '';
            width: 6px;
            height: 6px;
            border-radius: 999px;
            background: var(--td-error-color);
        }
    }
}

@keyframes docs-spin {
    from {
        transform: rotate(0deg);
    }

    to {
        transform: rotate(360deg);
    }
}

.docs-panel__body {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 1px;
    text-align: left;
}

.docs-panel__name {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: var(--td-text-color-primary);
    font: var(--td-font-body-small);
}

.docs-panel__meta {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: var(--td-text-color-placeholder);
    font: var(--td-font-link-small);
}

.docs-panel__empty {
    padding: 32px 16px;
    text-align: center;
    color: var(--td-text-color-placeholder);
    font: var(--td-font-body-small);
}

.docs-panel__picker {
    padding: 8px;
    max-height: 260px;
    overflow-y: auto;
}

.docs-panel__picker-title {
    padding: 4px 8px 8px;
    font: var(--td-font-link-small);
    color: var(--td-font-gray-3);
}

.docs-panel__picker-empty {
    padding: 12px 8px;
    color: var(--td-text-color-placeholder);
    font: var(--td-font-body-small);
}

.docs-panel__picker-item {
    display: block;
    width: 100%;
    padding: 8px 10px;
    border: none;
    border-radius: var(--td-radius-small);
    background: transparent;
    color: var(--td-text-color-primary);
    font: var(--td-font-body-small);
    text-align: left;
    cursor: pointer;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;

    &:hover {
        background: var(--td-bg-color-container-hover);
    }
}
</style>
