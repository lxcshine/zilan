<template>
    <!-- 记忆模式列表（PRD §5.3）：四模块入口 + 条数摘要 -->
    <div class="memory-panel">
        <template v-if="loading">
            <div v-for="n in 4" :key="'mem-skel-' + n" class="memory-panel__row memory-panel__row--skeleton">
                <t-skeleton animation="gradient" :row-col="[{ width: '100%', height: '14px' }]" />
            </div>
        </template>
        <template v-else>
            <button v-for="mod in moduleRows" :key="mod.key" type="button" class="memory-panel__row"
                :title="mod.label" @click="openMemory">
                <span class="memory-panel__icon" v-html="mod.icon"></span>
                <span class="memory-panel__name">{{ mod.label }}</span>
                <span class="memory-panel__count">{{ mod.count }}</span>
            </button>
        </template>
    </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { getMemoryModules } from '@/api/memory'

const { t } = useI18n()
const router = useRouter()

const modules = ref<any[]>([])
const loading = ref(true)

const MODULE_META: Record<string, { labelKey: string; icon: string }> = {
    soul: {
        labelKey: 'panel.memorySoul',
        icon: `<svg viewBox="0 0 20 20" width="16" height="16" fill="none"><circle cx="10" cy="10" r="6.5" stroke="currentColor" stroke-width="1.2"/><path d="M10 6.8a3.2 3.2 0 0 1 0 6.4 3.2 3.2 0 0 1 0-6.4Z" stroke="currentColor" stroke-width="1.2"/></svg>`,
    },
    user: {
        labelKey: 'panel.memoryProfile',
        icon: `<svg viewBox="0 0 20 20" width="16" height="16" fill="none"><circle cx="10" cy="7.2" r="3" stroke="currentColor" stroke-width="1.2"/><path d="M4.6 16.4c.8-2.9 2.9-4.4 5.4-4.4s4.6 1.5 5.4 4.4" stroke="currentColor" stroke-width="1.2" stroke-linecap="round"/></svg>`,
    },
    memory: {
        labelKey: 'panel.memoryStream',
        icon: `<svg viewBox="0 0 20 20" width="16" height="16" fill="none"><path d="M4 5.5c2-1.6 4-1.6 6 0s4 1.6 6 0M4 10c2-1.6 4-1.6 6 0s4 1.6 6 0M4 14.5c2-1.6 4-1.6 6 0s4 1.6 6 0" stroke="currentColor" stroke-width="1.2" stroke-linecap="round"/></svg>`,
    },
    agent: {
        labelKey: 'panel.memoryTips',
        icon: `<svg viewBox="0 0 20 20" width="16" height="16" fill="none"><path d="M10 2.8v1.8" stroke="currentColor" stroke-width="1.2" stroke-linecap="round"/><rect x="4.2" y="5.4" width="11.6" height="9" rx="2.4" stroke="currentColor" stroke-width="1.2"/><path d="M7.6 9.4l1.8 1.8 3.2-3.4" stroke="currentColor" stroke-width="1.2" stroke-linecap="round" stroke-linejoin="round"/></svg>`,
    },
}

const moduleRows = computed(() => {
    const order = ['soul', 'user', 'memory', 'agent']
    return order.map((key) => {
        const found = modules.value.find((m) => m.module === key)
        const meta = MODULE_META[key]
        return {
            key,
            label: t(meta.labelKey),
            icon: meta.icon,
            // 计数口径与画布「我的记忆」页保持一致：仅统计事实条数，
            // 不叠加 L2 会话摘要（summary_count），避免同屏出现两个不同数字。
            count: found ? (found.fact_count ?? 0) : 0,
        }
    })
})

// 记忆画布为「我的记忆」管理页：面板行统一进入该页
const openMemory = () => {
    router.push('/platform/memory')
}

onMounted(async () => {
    try {
        modules.value = await getMemoryModules()
    } catch {
        modules.value = []
    } finally {
        loading.value = false
    }
})
</script>

<style lang="less" scoped>
.memory-panel {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
    padding: 4px 8px;
}

.memory-panel__row {
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
    color: var(--td-font-gray-2);
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

.memory-panel__icon {
    display: flex;
    align-items: center;
    flex-shrink: 0;

    :deep(svg) {
        display: block;
    }
}

.memory-panel__name {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    text-align: left;
    color: var(--td-text-color-primary);
    font: var(--td-font-body-small);
}

.memory-panel__count {
    flex-shrink: 0;
    color: var(--td-text-color-placeholder);
    font: var(--td-font-link-small);
    font-variant-numeric: tabular-nums;
}
</style>
