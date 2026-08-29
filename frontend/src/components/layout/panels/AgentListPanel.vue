<template>
    <!-- 智能体模式列表（PRD §5.3）：名称 + 模型标签 -->
    <div class="agent-panel">
        <template v-if="loading">
            <div v-for="n in 5" :key="'agent-skel-' + n" class="agent-panel__row agent-panel__row--skeleton">
                <t-skeleton animation="gradient" :row-col="[{ width: '100%', height: '14px' }]" />
            </div>
        </template>
        <div v-else-if="filteredAgents.length === 0" class="agent-panel__empty">
            {{ keyword ? t('panel.noSearchResults') : t('panel.noAgents') }}
        </div>
        <template v-else>
            <button v-for="agent in filteredAgents" :key="agent.id" type="button" class="agent-panel__row"
                :title="agent.name || agent.description" @click="openAgents">
                <span class="agent-panel__name">{{ agent.name }}</span>
                <span v-if="isBuiltin(agent)" class="agent-panel__tag">{{ t('panel.builtinTag') }}</span>
            </button>
        </template>
    </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { listAgents, isBuiltinAgent } from '@/api/agent'

const props = defineProps<{ keyword?: string }>()

const { t } = useI18n()
const router = useRouter()

const agents = ref<any[]>([])
const loading = ref(true)

const isBuiltin = (agent: any) => isBuiltinAgent(String(agent.id))

const filteredAgents = computed(() => {
    const kw = (props.keyword || '').trim().toLowerCase()
    if (!kw) return agents.value
    return agents.value.filter((agent) => (agent.name || '').toLowerCase().includes(kw))
})

// 智能体画布为列表/编排页：面板行统一进入该页，具体 Agent 在画布内选择
const openAgents = () => {
    router.push('/platform/agents')
}

onMounted(async () => {
    try {
        const res: any = await listAgents()
        agents.value = res?.data || []
    } catch {
        agents.value = []
    } finally {
        loading.value = false
    }
})
</script>

<style lang="less" scoped>
.agent-panel {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
    padding: 4px 8px;
}

.agent-panel__row {
    width: 100%;
    display: flex;
    align-items: center;
    gap: 8px;
    height: 40px;
    padding: 0 10px;
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

    &--skeleton {
        cursor: default;
        height: 36px;
    }
}

.agent-panel__name {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    text-align: left;
    color: var(--td-text-color-primary);
    font: var(--td-font-body-small);
}

.agent-panel__tag {
    flex-shrink: 0;
    height: 18px;
    padding: 0 6px;
    border-radius: 999px;
    background: var(--td-bg-color-secondarycontainer);
    color: var(--td-text-color-secondary);
    font: var(--td-font-link-small);
    line-height: 18px;
}

.agent-panel__empty {
    padding: 32px 16px;
    text-align: center;
    color: var(--td-text-color-placeholder);
    font: var(--td-font-body-small);
}
</style>
