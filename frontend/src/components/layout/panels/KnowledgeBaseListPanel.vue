<template>
    <!-- 知识库模式列表（PRD §5.3）：库名 + 文档数 + 启用状态点 -->
    <div class="kb-panel">
        <template v-if="loading">
            <div v-for="n in 5" :key="'kb-skel-' + n" class="kb-panel__row kb-panel__row--skeleton">
                <t-skeleton animation="gradient" :row-col="[{ width: '100%', height: '14px' }]" />
            </div>
        </template>
        <div v-else-if="filteredKbs.length === 0" class="kb-panel__empty">
            {{ keyword ? t('panel.noSearchResults') : t('panel.noKnowledgeBases') }}
        </div>
        <template v-else>
            <button v-for="kb in filteredKbs" :key="kb.id" type="button" class="kb-panel__row"
                :class="{ 'kb-panel__row--active': kb.id === activeKbId }" :title="kb.name"
                @click="openKnowledgeBase(kb.id)">
                <span class="kb-panel__status" :class="kb.enable_status === 'enabled' ? 'kb-panel__status--on' : ''"
                    :aria-label="kb.enable_status === 'enabled' ? t('panel.kbEnabled') : t('panel.kbDisabled')"></span>
                <span class="kb-panel__name">{{ kb.name }}</span>
                <span class="kb-panel__count">{{ kb.type === 'faq' ? (kb.chunk_count || 0) : (kb.knowledge_count || 0)
                    }}</span>
            </button>
        </template>
    </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { listKnowledgeBases } from '@/api/knowledge-base'

const props = defineProps<{ keyword?: string }>()

const { t } = useI18n()
const route = useRoute()
const router = useRouter()

const kbs = ref<any[]>([])
const loading = ref(true)

const activeKbId = computed(() => (route.params as any)?.kbId as string | undefined)

const filteredKbs = computed(() => {
    const kw = (props.keyword || '').trim().toLowerCase()
    if (!kw) return kbs.value
    return kbs.value.filter((kb) => (kb.name || '').toLowerCase().includes(kw))
})

const openKnowledgeBase = (kbId: string) => {
    router.push(`/platform/knowledge-bases/${kbId}`)
}

onMounted(async () => {
    try {
        const res: any = await listKnowledgeBases()
        kbs.value = res?.data || []
    } catch {
        kbs.value = []
    } finally {
        loading.value = false
    }
})
</script>

<style lang="less" scoped>
.kb-panel {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
    padding: 4px 8px;
}

.kb-panel__row {
    width: 100%;
    display: flex;
    align-items: center;
    gap: 8px;
    height: 40px;
    padding: 0 8px;
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
        height: 36px;
    }
}

.kb-panel__status {
    width: 6px;
    height: 6px;
    flex-shrink: 0;
    border-radius: 999px;
    background: var(--td-gray-color-6);

    &--on {
        background: var(--td-success-color);
    }
}

.kb-panel__name {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    text-align: left;
    color: var(--td-text-color-primary);
    font: var(--td-font-body-small);
}

.kb-panel__count {
    flex-shrink: 0;
    color: var(--td-text-color-placeholder);
    font: var(--td-font-link-small);
    font-variant-numeric: tabular-nums;
}

.kb-panel__empty {
    padding: 32px 16px;
    text-align: center;
    color: var(--td-text-color-placeholder);
    font: var(--td-font-body-small);
}
</style>
