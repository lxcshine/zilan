<template>
    <!-- 一级全局导航栏（Rail，60px）：Logo + 五个工作模式 + 底部设置/头像。
         PRD ui-layout-visual-redesign §4：图标线框风格、选中态黑底白字胶囊、
         点击切换列表栏内容并路由到该模式默认页。 -->
    <nav class="app-rail" :aria-label="t('rail.ariaLabel')">
        <!-- Logo：点击回到新对话态 -->
        <t-tooltip :content="t('rail.backHome')" placement="right">
            <button type="button" class="rail-logo" :aria-label="t('rail.backHome')" @click="goHome">
                <svg viewBox="0 0 24 24" width="24" height="24" fill="none" aria-hidden="true">
                    <circle cx="12" cy="12" r="3.2" class="rail-logo__core" />
                    <path d="M12 5.4c4.3 0 7.8 3 7.8 6.6" class="rail-logo__ripple" />
                    <path d="M12 2.6c5.9 0 10.7 4.2 10.7 9.4" class="rail-logo__ripple rail-logo__ripple--outer" />
                </svg>
            </button>
        </t-tooltip>

        <!-- 模式图标组 -->
        <div class="rail-modes">
            <t-tooltip v-for="item in modes" :key="item.mode" :content="t(item.labelKey)" placement="right">
                <button type="button" class="rail-item" :class="{ 'rail-item--active': uiStore.activeMode === item.mode }"
                    :data-guide="item.guide" :aria-label="t(item.labelKey)"
                    :aria-current="uiStore.activeMode === item.mode ? 'page' : undefined" @click="selectMode(item)">
                    <span class="rail-item__icon" v-html="ICONS[item.mode]"></span>
                </button>
            </t-tooltip>
        </div>

        <div class="rail-spacer"></div>

        <!-- 底部固定组：系统设置 + 用户头像菜单 -->
        <div class="rail-bottom">
            <t-tooltip :content="t('menu.settings')" placement="right">
                <button type="button" class="rail-item" :class="{ 'rail-item--active': isSettingsActive }"
                    :aria-label="t('menu.settings')" @click="openSettings">
                    <span class="rail-item__icon">
                        <t-icon name="setting" size="20px" />
                    </span>
                </button>
            </t-tooltip>
            <UserMenu compact />
        </div>
    </nav>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useUIStore } from '@/stores/ui'
import UserMenu from '@/components/UserMenu.vue'
import { RAIL_MODES, type RailModeDefinition } from '@/composables/useAppMode'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const uiStore = useUIStore()

const modes = RAIL_MODES

const isSettingsActive = computed(() => route.name === 'settings')

// 线框图标（1.2px 描边，20×20）：统一单色语言，避免彩色图标破坏低饱和风格。
const ICONS: Record<string, string> = {
    chat: `<svg viewBox="0 0 20 20" width="20" height="20" fill="none" xmlns="http://www.w3.org/2000/svg"><path d="M3.5 4.5h13a1 1 0 0 1 1 1v7.5a1 1 0 0 1-1 1H8.2l-3.7 3v-3h-1a1 1 0 0 1-1-1V5.5a1 1 0 0 1 1-1Z" stroke="currentColor" stroke-width="1.2" stroke-linejoin="round"/><path d="M6.5 8h7M6.5 10.8h4.5" stroke="currentColor" stroke-width="1.2" stroke-linecap="round"/></svg>`,
    knowledge: `<svg viewBox="0 0 20 20" width="20" height="20" fill="none" xmlns="http://www.w3.org/2000/svg"><path d="M10 4.2c-1.5-1.2-3.6-1.5-5.8-1v12.4c2.2-.5 4.3-.2 5.8 1 1.5-1.2 3.6-1.5 5.8-1V3.2c-2.2-.5-4.3-.2-5.8 1Z" stroke="currentColor" stroke-width="1.2" stroke-linejoin="round"/><path d="M10 4.2v12.4" stroke="currentColor" stroke-width="1.2"/></svg>`,
    documents: `<svg viewBox="0 0 20 20" width="20" height="20" fill="none" xmlns="http://www.w3.org/2000/svg"><rect x="4.2" y="3.2" width="9.2" height="12.4" rx="1.2" stroke="currentColor" stroke-width="1.2"/><path d="M8 16.4v.8a1.2 1.2 0 0 0 1.2 1.2h6.6a1.2 1.2 0 0 0 1.2-1.2V6.6a1.2 1.2 0 0 0-1.2-1.2h-.8" stroke="currentColor" stroke-width="1.2" stroke-linecap="round"/><path d="M6.5 6.5h4.6M6.5 9.3h4.6M6.5 12.1h3" stroke="currentColor" stroke-width="1.2" stroke-linecap="round"/></svg>`,
    agents: `<svg viewBox="0 0 20 20" width="20" height="20" fill="none" xmlns="http://www.w3.org/2000/svg"><path d="M10 2.8v1.8" stroke="currentColor" stroke-width="1.2" stroke-linecap="round"/><rect x="4.2" y="5.4" width="11.6" height="9" rx="2.4" stroke="currentColor" stroke-width="1.2"/><circle cx="7.8" cy="9.9" r="1" fill="currentColor"/><circle cx="12.2" cy="9.9" r="1" fill="currentColor"/><path d="M2.6 9v2.6M17.4 9v2.6" stroke="currentColor" stroke-width="1.2" stroke-linecap="round"/></svg>`,
    memory: `<svg viewBox="0 0 20 20" width="20" height="20" fill="none" xmlns="http://www.w3.org/2000/svg"><path d="M3.6 8.4V6a1.4 1.4 0 0 1 1.4-1.4h10A1.4 1.4 0 0 1 16.4 6v2.4" stroke="currentColor" stroke-width="1.2" stroke-linecap="round"/><rect x="2.4" y="8.4" width="15.2" height="6.8" rx="1.4" stroke="currentColor" stroke-width="1.2"/><path d="M6.2 11.8h7.6" stroke="currentColor" stroke-width="1.2" stroke-linecap="round"/></svg>`,
}

const goHome = () => {
    uiStore.setActiveMode('chat')
    router.push('/platform/creatChat')
}

const selectMode = (item: RailModeDefinition) => {
    if (uiStore.activeMode !== item.mode) {
        uiStore.setActiveMode(item.mode)
    }
    // 知识库模式沿用旧交互：已在库详情内时回到当前库，否则去列表
    if (item.mode === 'knowledge') {
        const kbId = (route.params as any)?.kbId as string | undefined
        if (kbId && String(route.name || '').startsWith('knowledgeBase')) {
            router.push(`/platform/knowledge-bases/${kbId}`)
        } else {
            router.push(item.path)
        }
    } else {
        router.push(item.path)
    }
    // 列表栏处于折叠态时，选模式意味着要看到该模式的列表
    if (uiStore.sidebarCollapsed) {
        uiStore.expandSidebar()
    }
}

const openSettings = () => {
    uiStore.openSettings()
    router.push('/platform/settings')
}
</script>

<style lang="less" scoped>
.app-rail {
    width: var(--app-rail-width);
    flex-shrink: 0;
    display: flex;
    flex-direction: column;
    align-items: center;
    padding: var(--spacer-12, 12px) 0 var(--spacer-8, 8px);
    gap: var(--spacer-4, 4px);
    background: var(--td-bg-color-sidebar);
    border-right: 1px solid var(--td-component-stroke);
    box-sizing: border-box;
    height: 100%;
    overflow: visible;
    z-index: 5;
}

.rail-logo {
    width: 40px;
    height: 40px;
    display: flex;
    align-items: center;
    justify-content: center;
    border: none;
    border-radius: var(--td-radius-medium);
    background: transparent;
    color: var(--td-text-color-primary);
    cursor: pointer;
    margin-bottom: var(--spacer-8, 8px);
    transition: background-color 0.15s ease;

    &:hover {
        background: var(--td-bg-color-container-hover);
    }

    &:focus-visible {
        outline: 2px solid var(--td-brand-color);
        outline-offset: 2px;
    }

    .rail-logo__core {
        fill: currentColor;
    }

    .rail-logo__ripple {
        stroke: currentColor;
        stroke-width: 1.4;
        stroke-linecap: round;
        opacity: 0.75;
    }
}

.rail-modes {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--spacer-4, 4px);
}

.rail-item {
    position: relative;
    width: 40px;
    height: 40px;
    display: flex;
    align-items: center;
    justify-content: center;
    border: none;
    border-radius: var(--td-radius-medium);
    background: transparent;
    color: var(--td-font-gray-2);
    cursor: pointer;
    transition: background-color 0.15s ease, color 0.15s ease;

    &:hover {
        background: var(--td-bg-color-container-hover);
        color: var(--td-text-color-primary);
    }

    &:focus-visible {
        outline: 2px solid var(--td-brand-color);
        outline-offset: 2px;
    }

    // 选中态：黑底白字胶囊——黑白反转即最强对比，不引入彩色
    &--active {
        background: var(--td-brand-color);
        color: var(--td-text-color-anti);

        &:hover {
            background: var(--td-brand-color-active);
            color: var(--td-text-color-anti);
        }
    }

    .rail-item__icon {
        display: flex;
        align-items: center;
        justify-content: center;
        line-height: 0;

        :deep(svg) {
            display: block;
        }
    }
}

.rail-spacer {
    flex: 1;
    min-height: var(--spacer-12, 12px);
}

.rail-bottom {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--spacer-4, 4px);
}

/* Rail 内的 UserMenu 永远以紧凑（仅头像）形态呈现 */
.rail-bottom :deep(.user-menu) {
    width: auto;
}
</style>
