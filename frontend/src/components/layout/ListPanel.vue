<template>
    <!-- 二级列表栏（264px，PRD ui-layout-visual-redesign §5）：内容随 Rail 模式联动。
         问答模式的历史会话逻辑完整迁移自旧 menu.vue（会话桶、来源筛选、批量管理），
         其余模式为轻量对象列表。折叠（⌘B / Rail 点击）时整体收起为拖拽展开条。 -->
    <aside class="list-panel" :class="{ 'list-panel--collapsed': uiStore.sidebarCollapsed }">
        <template v-if="!uiStore.sidebarCollapsed">
            <!-- 模式标题行 + 折叠按钮 -->
            <div class="list-panel__header">
                <span class="list-panel__title">{{ panelTitle }}</span>
                <t-tooltip placement="bottom">
                    <template #content>
                        <span class="cmdk-tip">
                            <span class="cmdk-tip-label">{{ t('menu.collapseSidebar') }}</span>
                            <span class="cmdk-tip-keys">{{ cmdModKeyLabel }}B</span>
                        </span>
                    </template>
                    <button type="button" class="list-panel__collapse" :aria-label="t('menu.collapseSidebar')"
                        @click="uiStore.toggleSidebar()">
                        <svg viewBox="0 0 20 20" width="16" height="16" fill="none" aria-hidden="true">
                            <rect x="1.5" y="1.5" width="17" height="17" rx="3" stroke="currentColor"
                                stroke-width="1.2" />
                            <line x1="12.5" y1="1.5" x2="12.5" y2="18.5" stroke="currentColor" stroke-width="1.2" />
                            <line x1="8" y1="7.5" x2="10" y2="9.5" stroke="currentColor" stroke-width="1.2"
                                stroke-linecap="round" />
                            <line x1="8" y1="11.5" x2="10" y2="9.5" stroke="currentColor" stroke-width="1.2"
                                stroke-linecap="round" />
                        </svg>
                    </button>
                </t-tooltip>
            </div>

            <div class="list-panel__body">
                <!-- 问答模式：空间选择器 + 会话列表 -->
                <template v-if="uiStore.activeMode === 'chat'">
                    <TenantSelector v-if="canAccessAllTenants" />
                    <div class="list-panel__primary">
                        <button type="button" class="primary-btn" @click="startNewChat">
                            <svg viewBox="0 0 20 20" width="16" height="16" fill="none" aria-hidden="true">
                                <line x1="10" y1="4" x2="10" y2="16" stroke="currentColor" stroke-width="1.6"
                                    stroke-linecap="round" />
                                <line x1="4" y1="10" x2="16" y2="10" stroke="currentColor" stroke-width="1.6"
                                    stroke-linecap="round" />
                            </svg>
                            {{ t('menu.newChat') }}
                        </button>
                    </div>
                    <div class="list-panel__search">
                        <svg viewBox="0 0 20 20" width="15" height="15" fill="none" aria-hidden="true">
                            <circle cx="9" cy="9" r="6" stroke="currentColor" stroke-width="1.4" />
                            <line x1="13.5" y1="13.5" x2="16.5" y2="16.5" stroke="currentColor" stroke-width="1.4"
                                stroke-linecap="round" />
                        </svg>
                        <input v-model="sessionKeyword" type="text" :placeholder="t('panel.searchSessions')"
                            :aria-label="t('panel.searchSessions')" />
                    </div>

                    <div class="submenu" ref="scrollContainer" @scroll="handleScroll">
                        <!-- 来源筛选（稳定挂载，避免切换来源时控件跳动） -->
                        <div v-if="showSessionSourceFilter && !batchMode" class="session-list-scope-header">
                            <SessionSourceFilter inline :emphasized="sessionScopeFilterPinned"
                                :sources="sessionSourceOptions" :current="activeSessionBucketKey"
                                @select="switchSessionBucket" />
                        </div>
                        <template v-if="sessionListBooting && !hasAnySession">
                            <div v-for="n in 4" :key="'skel-' + n" class="submenu_item_p session-chat-row">
                                <div class="session-list-row session-list-row--flat">
                                    <t-skeleton animation="gradient" class="session-list-row__body"
                                        :row-col="[{ width: '100%', height: '14px' }]" />
                                </div>
                            </div>
                        </template>

                        <div v-else class="session-filtered-list">
                            <template
                                v-if="activeBucket?.loading && !activeBucket.loaded && filteredGroupedSessions.length === 0">
                                <div v-for="n in 4" :key="'bucket-skel-' + n"
                                    class="submenu_item_p session-chat-row">
                                    <div class="session-list-row session-list-row--flat">
                                        <t-skeleton animation="gradient" class="session-list-row__body"
                                            :row-col="[{ width: '100%', height: '14px' }]" />
                                    </div>
                                </div>
                            </template>
                            <template v-else-if="activeBucket?.loaded && filteredGroupedSessions.length === 0">
                                <div class="submenu_empty">
                                    {{ sessionKeyword ? t('panel.noSearchResults') : t('menu.noSessions') }}
                                </div>
                            </template>
                            <template v-else>
                                <template v-for="group in filteredGroupedSessions" :key="group.key">
                                    <div v-if="group.label" class="timeline_header session-list-row session-list-row--flat">
                                        <span class="session-list-row__body">
                                            <span class="timeline_header-label">{{ group.label }}</span>
                                        </span>
                                    </div>
                                    <div v-for="subitem in group.items" :key="subitem.id"
                                        class="submenu_item_p session-chat-row" :class="{
                                            'session-chat-row--active': !batchMode && subitem.path === currentSecondpath,
                                            'session-chat-row--selected': batchMode && batchSelectedIds.includes(subitem.id),
                                        }">
                                        <div class="session-list-row session-list-row--flat">
                                            <div class="session-list-row__body">
                                                <SessionSidebarRow :item="subitem" :batch-mode="batchMode"
                                                    :active-path="currentSecondpath" :selected-ids="batchSelectedIds"
                                                    :menu-options="buildSessionMenuOptions(subitem)"
                                                    @navigate="gotoSession(subitem.path)"
                                                    @toggle-select="toggleBatchSelect(subitem.id)"
                                                    @menu-click="handleSessionMenuClick($event, subitem)"
                                                    @rename-submit="renameSessionTitle(subitem, $event.title)"
                                                    @hover-in="mouseenteBotDownr(subitem.id)"
                                                    @hover-out="mouseleaveBotDown" />
                                            </div>
                                        </div>
                                    </div>
                                </template>
                                <div v-if="activeBucket?.loading && filteredGroupedSessions.length > 0"
                                    class="session-list-loading session-list-row session-list-row--flat">
                                    <span class="session-list-row__body">
                                        <t-loading size="small" />
                                    </span>
                                </div>
                            </template>
                        </div>
                    </div>

                    <!-- 批量管理底部操作条 -->
                    <div v-if="batchMode" class="batch-inline-footer">
                        <div class="batch-footer-left">
                            <t-checkbox :checked="isAllBatchSelected" :indeterminate="isBatchIndeterminate"
                                @change="toggleBatchSelectAll">
                                {{ t('batchManage.selectAll') }}
                            </t-checkbox>
                        </div>
                        <div class="batch-footer-right">
                            <t-button size="small" variant="text" @click="exitBatchMode">
                                {{ t('batchManage.cancel') }}
                            </t-button>
                            <t-button size="small" theme="danger" variant="base"
                                :disabled="batchSelectedIds.length === 0" :loading="batchDeleting"
                                @click="handleInlineBatchDelete">
                                {{ t('batchManage.delete') }}{{ batchSelectedIds.length > 0 ? `(${batchDisplayCount})` : '' }}
                            </t-button>
                        </div>
                    </div>
                </template>

                <!-- 知识库模式 -->
                <template v-else-if="uiStore.activeMode === 'knowledge'">
                    <div class="list-panel__primary">
                        <button type="button" class="primary-btn" @click="createKnowledgeBase">
                            <svg viewBox="0 0 20 20" width="16" height="16" fill="none" aria-hidden="true">
                                <line x1="10" y1="4" x2="10" y2="16" stroke="currentColor" stroke-width="1.6"
                                    stroke-linecap="round" />
                                <line x1="4" y1="10" x2="16" y2="10" stroke="currentColor" stroke-width="1.6"
                                    stroke-linecap="round" />
                            </svg>
                            {{ t('panel.newKnowledgeBase') }}
                        </button>
                    </div>
                    <div class="list-panel__search">
                        <svg viewBox="0 0 20 20" width="15" height="15" fill="none" aria-hidden="true">
                            <circle cx="9" cy="9" r="6" stroke="currentColor" stroke-width="1.4" />
                            <line x1="13.5" y1="13.5" x2="16.5" y2="16.5" stroke="currentColor" stroke-width="1.4"
                                stroke-linecap="round" />
                        </svg>
                        <input v-model="objectKeyword" type="text" :placeholder="t('panel.searchKnowledgeBases')"
                            :aria-label="t('panel.searchKnowledgeBases')" />
                    </div>
                    <KnowledgeBaseListPanel :keyword="objectKeyword" />
                </template>

                <!-- 文档模式 -->
                <template v-else-if="uiStore.activeMode === 'documents'">
                    <div class="list-panel__primary">
                        <button type="button" class="primary-btn" @click="uploadDocument">
                            <svg viewBox="0 0 20 20" width="16" height="16" fill="none" aria-hidden="true">
                                <path d="M10 13V4.5M10 4.5 6.8 7.7M10 4.5l3.2 3.2" stroke="currentColor"
                                    stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" />
                                <path d="M4 12.5v2A1.5 1.5 0 0 0 5.5 16h9a1.5 1.5 0 0 0 1.5-1.5v-2" stroke="currentColor"
                                    stroke-width="1.6" stroke-linecap="round" />
                            </svg>
                            {{ t('panel.uploadDocument') }}
                        </button>
                    </div>
                    <DocumentsListPanel ref="documentsPanelRef" />
                </template>

                <!-- 智能体模式 -->
                <template v-else-if="uiStore.activeMode === 'agents'">
                    <div class="list-panel__primary">
                        <button type="button" class="primary-btn" @click="router.push('/platform/agents')">
                            <svg viewBox="0 0 20 20" width="16" height="16" fill="none" aria-hidden="true">
                                <line x1="10" y1="4" x2="10" y2="16" stroke="currentColor" stroke-width="1.6"
                                    stroke-linecap="round" />
                                <line x1="4" y1="10" x2="16" y2="10" stroke="currentColor" stroke-width="1.6"
                                    stroke-linecap="round" />
                            </svg>
                            {{ t('panel.newAgent') }}
                        </button>
                    </div>
                    <div class="list-panel__search">
                        <svg viewBox="0 0 20 20" width="15" height="15" fill="none" aria-hidden="true">
                            <circle cx="9" cy="9" r="6" stroke="currentColor" stroke-width="1.4" />
                            <line x1="13.5" y1="13.5" x2="16.5" y2="16.5" stroke="currentColor" stroke-width="1.4"
                                stroke-linecap="round" />
                        </svg>
                        <input v-model="objectKeyword" type="text" :placeholder="t('panel.searchAgents')"
                            :aria-label="t('panel.searchAgents')" />
                    </div>
                    <AgentListPanel :keyword="objectKeyword" />
                </template>

                <!-- 记忆模式 -->
                <template v-else-if="uiStore.activeMode === 'memory'">
                    <MemoryListPanel />
                </template>
            </div>
        </template>

        <!-- 折叠态：拖拽/点击展开条 -->
        <div v-else class="list-panel__expand-strip" :title="t('menu.expandSidebar')"
            @click="uiStore.expandSidebar()" @mousedown="onDragHandleMouseDown">
            <svg viewBox="0 0 20 20" width="14" height="14" fill="none" aria-hidden="true">
                <polyline points="12 5 7 10 12 15" stroke="currentColor" stroke-width="1.4" fill="none"
                    stroke-linecap="round" stroke-linejoin="round" />
            </svg>
        </div>
    </aside>
</template>

<script setup lang="ts">
import { storeToRefs } from 'pinia';
import { onMounted, onUnmounted, watch, computed, ref, h, nextTick } from 'vue';
import { useRoute, useRouter } from 'vue-router';
import { getSessionsList, batchDelSessions, deleteAllSessions, getSession } from "@/api/chat/index";
import { listAllIMChannels } from '@/api/agent/index';
import SessionSidebarRow from '../SessionSidebarRow.vue';
import {
    clearSession,
    removeSession,
    renameSession,
    SESSION_MUTATION_EVENT,
    setSessionPinned,
    type SessionMutationDetail,
} from '../sessionMutations';
import SessionSourceFilter from '../SessionSourceFilter.vue';
import {
    SIDEBAR_BUCKET_PAGE_SIZE,
    applyBucketCountProbe,
    buildBucketDefinitions,
    bucketHasMore,
    bucketVisible,
    createEmptyBucket,
    flattenBucketItems,
    isChannelBucket,
    isChannelBucketKey,
    mergeBucketPage,
    prependSessionToWebBucket,
    removeSessionFromBuckets,
    type SidebarSessionBucket,
} from '../sessionSidebarBuckets';
import type { SessionForGrouping } from '../sessionGrouping';
import { listAllEmbedChannels } from '@/api/embed/index';
import {
    classifyDateBucket,
    configuredPlatforms,
    groupSessionsByDate,
    originGroupKey,
    resolveSessionOrigin,
    type DateBucketKey,
} from '../sessionGrouping';
import {
    DEFAULT_SESSION_BUCKET_KEY,
    buildSessionSourceOptions,
    findSessionBucketKey,
    shouldShowSessionSourceFilter,
} from '../sessionSidebarSourceFilter';
import { useMenuStore } from '@/stores/menu';
import { useAuthStore } from '@/stores/auth';
import { useOrganizationStore } from '@/stores/organization';
import { useUIStore } from '@/stores/ui';
import { MessagePlugin, DialogPlugin, Icon as TIcon } from "tdesign-vue-next";
import TenantSelector from '@/components/TenantSelector.vue';
import KnowledgeBaseListPanel from '@/components/layout/panels/KnowledgeBaseListPanel.vue';
import DocumentsListPanel from '@/components/layout/panels/DocumentsListPanel.vue';
import AgentListPanel from '@/components/layout/panels/AgentListPanel.vue';
import MemoryListPanel from '@/components/layout/panels/MemoryListPanel.vue';
import { useI18n } from 'vue-i18n';
import { getSystemInfo } from '@/api/system';
import { resolveModeFromRoute } from '@/composables/useAppMode';

// Platform logos reused from IMChannelsOverviewPanel — keeps the session list
// visually consistent with the channels admin view.
import wecomLogo from '@/assets/img/im/wecom.svg';
import feishuLogo from '@/assets/img/im/feishu.svg';
import larkLogo from '@/assets/img/im/lark.svg';
import slackLogo from '@/assets/img/im/slack.svg';
import telegramLogo from '@/assets/img/im/telegram.svg';
import dingtalkLogo from '@/assets/img/im/dingtalk.svg';
import mattermostLogo from '@/assets/img/im/mattermost.svg';
import wechatLogo from '@/assets/img/im/wechat.svg';
import qqbotLogo from '@/assets/img/im/qqbot.png';

const PLATFORM_LOGO: Record<string, string> = {
    wecom: wecomLogo,
    feishu: feishuLogo,
    lark: larkLogo,
    slack: slackLogo,
    telegram: telegramLogo,
    dingtalk: dingtalkLogo,
    mattermost: mattermostLogo,
    wechat: wechatLogo,
    qqbot: qqbotLogo,
};

const platformLogo = (p: string): string => (p ? PLATFORM_LOGO[p] || '' : '');

const { t } = useI18n();
const usemenuStore = useMenuStore();
const authStore = useAuthStore();
const orgStore = useOrganizationStore();
const uiStore = useUIStore();
const route = useRoute();
const router = useRouter();

const isMacLike = typeof navigator !== 'undefined' && /Mac|iPod|iPhone|iPad/.test(navigator.platform || '');
const cmdModKeyLabel = isMacLike ? '⌘' : 'Ctrl';

// ===== 模式面板元信息 =====
const panelTitle = computed(() => {
    switch (uiStore.activeMode) {
        case 'chat': return t('panel.historySessions');
        case 'knowledge': return t('rail.knowledgeBases');
        case 'documents': return t('rail.documents');
        case 'agents': return t('rail.agents');
        case 'memory': return t('rail.memory');
        default: return '';
    }
});

const objectKeyword = ref('');
const sessionKeyword = ref('');
const documentsPanelRef = ref<InstanceType<typeof DocumentsListPanel>>();

const canAccessAllTenants = computed(() => authStore.canAccessAllTenants);

const startNewChat = () => {
    router.push('/platform/creatChat');
};

const createKnowledgeBase = () => {
    uiStore.openCreateKB('document');
};

const uploadDocument = () => {
    // 「上传文档」：先选目标库再选文件（PRD §9）——由文档面板弹出库选择
    documentsPanelRef.value?.openKbPicker();
};

// ===== 以下为问答模式历史会话逻辑（自 menu.vue 迁移，行为不变） =====
const total = ref(0);
const sessionBuckets = ref<Record<string, SidebarSessionBucket>>({});
const bucketOrder = ref<string[]>([]);
let bucketRequestToken = 0;
const sessionListBooting = ref(false);
const currentSecondpath = ref('');
const scrollContainer = ref<HTMLElement | null>(null);
const imPlatforms = ref<string[]>([]);
const embedChannelNames = ref<Record<string, string>>({});
const activeSessionBucketKey = ref(DEFAULT_SESSION_BUCKET_KEY);
const visibleChannelBuckets = computed(() =>
    bucketOrder.value
        .map((key) => sessionBuckets.value[key])
        .filter((bucket): bucket is SidebarSessionBucket => !!bucket && isChannelBucket(bucket) && bucketVisible(bucket)),
);
const showSessionSourceFilter = computed(() =>
    shouldShowSessionSourceFilter(visibleChannelBuckets.value.length),
);
const sessionScopeFilterPinned = computed(() =>
    activeSessionBucketKey.value !== DEFAULT_SESSION_BUCKET_KEY,
);
const sessionSourceOptions = computed(() =>
    buildSessionSourceOptions(
        t('menu.myChats'),
        visibleChannelBuckets.value.map((bucket) => ({
            key: bucket.key,
            label: bucket.label,
            platform: bucket.platform,
        })),
        (platform) => platformLogo(platform),
    ),
);
const activeBucket = computed(() => sessionBuckets.value[activeSessionBucketKey.value]);
const hasAnySession = computed(() =>
    Object.values(sessionBuckets.value).some((bucket) => bucket.items.length > 0),
);

type MenuItem = { title: string; icon: string; path: string; childrenPath?: string; children?: any[] };
const { menuArr } = storeToRefs(usemenuStore);
let activeSubmenu = ref<string>('');

// 批量管理状态
const batchMode = ref(false)
const batchSelectedIds = ref<string[]>([])
const batchDeleting = ref(false)

const allSessionIds = computed(() => {
    const chatMenu = (menuArr.value as unknown as MenuItem[]).find((item: MenuItem) => item.path === 'creatChat');
    if (!chatMenu?.children) return [];
    return (chatMenu.children as any[]).map((s: any) => s.id);
})

const isAllBatchSelected = computed(() =>
    allSessionIds.value.length > 0 && batchSelectedIds.value.length === allSessionIds.value.length
)

const isBatchIndeterminate = computed(() =>
    batchSelectedIds.value.length > 0 && batchSelectedIds.value.length < allSessionIds.value.length
)

const batchDisplayCount = computed(() =>
    isAllBatchSelected.value ? total.value : batchSelectedIds.value.length
)

// 进行中的置顶/取消置顶请求，避免重复点击
const pinningIds = ref<Set<string>>(new Set())

/** 「聊天」区内按日期分组（当前筛选来源 + 标题关键词过滤） */
const dateBucketLabels = computed<Record<DateBucketKey, string>>(() => ({
    pinned: t('time.pinned'),
    today: t('time.today'),
    yesterday: t('time.yesterday'),
    last7Days: t('time.last7Days'),
    last30Days: t('time.last30Days'),
    lastYear: t('time.lastYear'),
    earlier: t('time.earlier'),
}));

const filteredGroupedSessions = computed(() => {
    const bucket = activeBucket.value;
    if (!bucket?.items.length) return [];
    const keyword = sessionKeyword.value.trim().toLowerCase();
    const items = keyword
        ? bucket.items.filter((item) => (item.title || '').toLowerCase().includes(keyword))
        : bucket.items;
    return groupSessionsByDate(
        items.map((item) => ({
            ...item,
            path: `chat/${item.id}`,
            title: item.title || '',
        })),
        dateBucketLabels.value,
        (session) => classifyDateBucket(session.updated_at || session.created_at),
    );
});

/** 列表未撑满滚动区时自动续页（按当前可见 DOM 测量） */
const ensureBucketFillsViewport = async (key: string) => {
    const MAX_ITERATIONS = 20;
    for (let i = 0; i < MAX_ITERATIONS; i++) {
        await nextTick();
        await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
        const container = scrollContainer.value;
        const bucket = sessionBuckets.value[key];
        if (!container || !bucket || !bucketHasMore(bucket) || bucket.loading) break;

        const hasOverflow = container.scrollHeight > container.clientHeight + 1;
        if (hasOverflow) break;

        const prevCount = bucket.items.length;
        await loadBucketPage(key);
        if ((sessionBuckets.value[key]?.items.length ?? 0) <= prevCount) break;
    }
};

const mouseenteBotDownr = (val: string) => {
    activeSubmenu.value = val;
}
const mouseleaveBotDown = () => {
    activeSubmenu.value = '';
}

const enterBatchMode = () => {
    batchMode.value = true
    batchSelectedIds.value = []
}

const exitBatchMode = () => {
    batchMode.value = false
    batchSelectedIds.value = []
}

const toggleBatchSelect = (id: string) => {
    const idx = batchSelectedIds.value.indexOf(id)
    if (idx > -1) {
        batchSelectedIds.value.splice(idx, 1)
    } else {
        batchSelectedIds.value.push(id)
    }
}

const toggleBatchSelectAll = (checked: boolean) => {
    batchSelectedIds.value = checked ? [...allSessionIds.value] : []
}

const handleInlineBatchDelete = () => {
    if (batchSelectedIds.value.length === 0) return
    const isDeleteAll = isAllBatchSelected.value
    const displayCount = batchDisplayCount.value
    const confirmDialog = DialogPlugin.confirm({
        header: t('batchManage.deleteConfirmTitle'),
        body: isDeleteAll
            ? t('batchManage.deleteAllConfirmBody') || t('batchManage.deleteConfirmBody', { count: displayCount })
            : t('batchManage.deleteConfirmBody', { count: displayCount }),
        confirmBtn: { content: t('batchManage.delete'), theme: 'danger' as const },
        cancelBtn: t('batchManage.cancel'),
        theme: 'warning',
        onConfirm: async () => {
            batchDeleting.value = true
            try {
                let res: any
                if (isDeleteAll) {
                    res = await deleteAllSessions()
                } else {
                    res = await batchDelSessions([...batchSelectedIds.value])
                }
                if (res && res.success === true) {
                    if (isDeleteAll) {
                        usemenuStore.clearMenuArr();
                        total.value = 0;
                        await getMessageList();
                    } else {
                        let next = sessionBuckets.value;
                        for (const id of batchSelectedIds.value) {
                            next = removeSessionFromBuckets(next, id);
                        }
                        sessionBuckets.value = next;
                        syncMenuStoreFromBuckets();
                    }
                    const currentChatId = route.params.chatid as string;
                    if (currentChatId && (isDeleteAll || batchSelectedIds.value.includes(currentChatId))) {
                        router.push('/platform/creatChat');
                    }
                    batchSelectedIds.value = []
                    MessagePlugin.success(t('batchManage.deleteSuccess'))
                    exitBatchMode()
                } else {
                    MessagePlugin.error(t('batchManage.deleteFailed'))
                }
            } catch {
                MessagePlugin.error(t('batchManage.deleteFailed'))
            }
            batchDeleting.value = false
            confirmDialog.destroy()
        },
    })
}

const handleSessionMenuClick = (data: { value: string }, item: any) => {
    if (data?.value === 'delete') {
        delCard(item);
    } else if (data?.value === 'clearMessages') {
        clearMessages(item);
    } else if (data?.value === 'batchManage') {
        enterBatchMode()
    } else if (data?.value === 'pin' || data?.value === 'unpin') {
        togglePin(item, data.value === 'pin');
    }
};

const buildSessionMenuOptions = (item: any) => {
    const options: any[] = [];
    if (item.is_pinned) {
        options.push({
            content: t('menu.unpin'),
            value: 'unpin',
            prefixIcon: () => h(TIcon, { name: 'pin-filled', size: '16px' }),
        });
    } else {
        options.push({
            content: t('menu.pin'),
            value: 'pin',
            prefixIcon: () => h(TIcon, { name: 'pin', size: '16px' }),
        });
    }
    options.push(
        { content: t('menu.renameSession'), value: 'rename', prefixIcon: () => h(TIcon, { name: 'edit-1', size: '16px' }) },
        { content: t('menu.clearMessages'), value: 'clearMessages', prefixIcon: () => h(TIcon, { name: 'clear', size: '16px' }) },
        { content: t('menu.batchManage'), value: 'batchManage', prefixIcon: () => h(TIcon, { name: 'queue', size: '16px' }) },
        { content: t('upload.deleteRecord'), value: 'delete', theme: 'error', prefixIcon: () => h(TIcon, { name: 'delete', size: '16px' }) },
    );
    return options;
};

const updateSessionInBuckets = (
    sessionId: string,
    patch: Partial<{ is_pinned: boolean; pinned_at: string | null; title: string; isNoTitle?: boolean }>,
) => {
    const next: Record<string, SidebarSessionBucket> = {};
    for (const [key, bucket] of Object.entries(sessionBuckets.value)) {
        next[key] = {
            ...bucket,
            items: bucket.items.map((row) => (row.id === sessionId ? { ...row, ...patch } : row)),
        };
    }
    sessionBuckets.value = next;
    syncMenuStoreFromBuckets();
};

const renameSessionTitle = async (item: any, title: string) => {
    try {
        await renameSession(item.id, title, item.description || '');
        MessagePlugin.success(t('menu.renameSessionSuccess'));
    } catch {
        MessagePlugin.error(t('menu.renameSessionFailed'));
    }
};

const togglePin = (item: any, pin: boolean) => {
    if (pinningIds.value.has(item.id)) return;
    pinningIds.value.add(item.id);

    setSessionPinned(item.id, pin).catch(() => {
        MessagePlugin.error(pin ? t('menu.pinFailed') : t('menu.unpinFailed'));
    }).finally(() => {
        pinningIds.value.delete(item.id);
    });
};

const clearMessages = (item: any) => {
    clearSession(item.id).then(() => {
        MessagePlugin.success(t('menu.clearMessagesSuccess'));
    }).catch(() => {
        MessagePlugin.error(t('menu.clearMessagesFailed'));
    });
};

const delCard = (item: any) => {
    removeSession(item.id).catch(() => MessagePlugin.error(t('chat.deleteSessionFailed')))
}

const debounce = (fn: (...args: any[]) => void, delay: number) => {
    let timer: ReturnType<typeof setTimeout>
    return (...args: any[]) => {
        clearTimeout(timer)
        timer = setTimeout(() => fn(...args), delay)
    }
}
const mapSessionRow = (item: any) => ({
    title: item.title ? item.title : t('menu.newSession'),
    path: `chat/${item.id}`,
    id: item.id,
    isMore: false,
    isNoTitle: item.title ? false : true,
    created_at: item.created_at,
    updated_at: item.updated_at,
    is_pinned: !!item.is_pinned,
    pinned_at: item.pinned_at || null,
    im_platform: item.im_platform || '',
    description: item.description || '',
    user_id: item.user_id || '',
});

const syncMenuStoreFromBuckets = () => {
    usemenuStore.clearMenuArr();
    const flat = flattenBucketItems(sessionBuckets.value, bucketOrder.value);
    flat.forEach((item) => usemenuStore.updatemenuArr(item));
    total.value = flat.length;
};

const menuChildToSessionRow = (item: Record<string, unknown>): SessionForGrouping & { path: string } => {
    const id = String(item.id);
    return {
        id,
        path: typeof item.path === 'string' ? item.path : `chat/${id}`,
        title: typeof item.title === 'string' ? item.title : undefined,
        is_pinned: !!item.is_pinned,
        created_at: typeof item.created_at === 'string' ? item.created_at : undefined,
        updated_at: typeof item.updated_at === 'string' ? item.updated_at : undefined,
        im_platform: typeof item.im_platform === 'string' ? item.im_platform : '',
        description: typeof item.description === 'string' ? item.description : '',
        user_id: typeof item.user_id === 'string' ? item.user_id : '',
    };
};

const sessionExistsInBuckets = (sessionId: string) =>
    Object.values(sessionBuckets.value).some((bucket) => bucket.items.some((row) => row.id === sessionId));

/** 创建会话后 menuStore 已乐观写入，但列表实际渲染自 sessionBuckets，需补齐。 */
const ensureSessionInSidebar = (sessionId: string) => {
    if (!sessionId || sessionExistsInBuckets(sessionId)) return;

    const web = sessionBuckets.value.web;
    if (!web) return;

    const chatMenu = (menuArr.value as unknown as MenuItem[]).find((item: MenuItem) => item.path === 'creatChat');
    const fromStore = (chatMenu?.children as Record<string, unknown>[] | undefined)
        ?.find((item) => item.id === sessionId);
    if (!fromStore) return;

    sessionBuckets.value = {
        ...sessionBuckets.value,
        web: prependSessionToWebBucket(web, menuChildToSessionRow(fromStore)),
    };
    total.value = flattenBucketItems(sessionBuckets.value, bucketOrder.value).length;
};

const rebuildBucketDefinitions = () => buildBucketDefinitions(
    imPlatforms.value,
    embedChannelNames.value,
    {
        web: t('menu.myChats'),
        imPlatform: (platform) => t(`agentEditor.im.${platform}`),
        embedChannel: (name) => name,
        api: t('menu.apiChats'),
    },
    { includeAdminChannelBuckets: authStore.hasRole('admin') },
);

/** 首屏轻量探测各渠道是否有会话（page_size=1 只取 total），避免展示空文件夹 */
const probeChannelBucketCounts = async (keys: string[], token: number) => {
    const targets = keys.filter((key) => isChannelBucketKey(key));
    await Promise.all(
        targets.map(async (key) => {
            const bucket = sessionBuckets.value[key];
            if (!bucket) return;
            try {
                const res: any = await getSessionsList(1, 1, bucket.apiSource);
                if (token !== bucketRequestToken) return;
                sessionBuckets.value = {
                    ...sessionBuckets.value,
                    [key]: applyBucketCountProbe(bucket, res?.total ?? 0),
                };
            } catch {
                if (token !== bucketRequestToken) return;
                sessionBuckets.value = {
                    ...sessionBuckets.value,
                    [key]: applyBucketCountProbe(bucket, 0),
                };
            }
        }),
    );
};

const loadBucketPage = async (key: string, page?: number, token?: number) => {
    const activeToken = token ?? bucketRequestToken;
    const bucket = sessionBuckets.value[key];
    if (!bucket || bucket.loading) return;

    const nextPage = page ?? bucket.page + 1;
    sessionBuckets.value = {
        ...sessionBuckets.value,
        [key]: { ...bucket, loading: true },
    };

    try {
        const res: any = await getSessionsList(nextPage, SIDEBAR_BUCKET_PAGE_SIZE, bucket.apiSource);
        if (activeToken !== bucketRequestToken) return;
        const rows = (res?.data || []).map((item: any) => mapSessionRow(item));
        const current = sessionBuckets.value[key];
        sessionBuckets.value = {
            ...sessionBuckets.value,
            [key]: mergeBucketPage(current, rows, res?.total ?? rows.length, nextPage),
        };
        syncMenuStoreFromBuckets();
    } catch {
        if (activeToken !== bucketRequestToken) return;
        const current = sessionBuckets.value[key];
        sessionBuckets.value = {
            ...sessionBuckets.value,
            [key]: { ...current, loading: false, loaded: true },
        };
    }
};

const switchSessionBucket = async (key: string) => {
    if (key === activeSessionBucketKey.value) return;
    activeSessionBucketKey.value = key;
    const bucket = sessionBuckets.value[key];
    if (bucket && !bucket.loaded && !bucket.loading) {
        await loadBucketPage(key, 1);
    }
    await ensureBucketFillsViewport(key);
};

const syncActiveBucketFromChat = async (sessionId: string | undefined) => {
    if (!sessionId) return;

    let bucketKey = findSessionBucketKey(sessionBuckets.value, sessionId);
    if (!bucketKey) {
        const chatMenu = (menuArr.value as unknown as MenuItem[]).find((item: MenuItem) => item.path === 'creatChat');
        const fromStore = (chatMenu?.children as Record<string, unknown>[] | undefined)
            ?.find((item) => item.id === sessionId);
        if (fromStore) {
            bucketKey = originGroupKey(resolveSessionOrigin(menuChildToSessionRow(fromStore)));
        }
    }
    // 硬刷新时只有 web 桶已加载：其他来源的会话需要拉详情归位来源文件夹
    if (!bucketKey) {
        try {
            const res: any = await getSession(sessionId);
            const candidate = originGroupKey(resolveSessionOrigin({
                id: sessionId,
                im_platform: res?.data?.im_platform || '',
                description: res?.data?.description || '',
                user_id: res?.data?.user_id || '',
            }));
            if (sessionBuckets.value[candidate]) {
                bucketKey = candidate;
            }
        } catch {
            // 查询失败时保持默认桶
        }
    }
    if (!bucketKey || bucketKey === activeSessionBucketKey.value) return;

    activeSessionBucketKey.value = bucketKey;
    const bucket = sessionBuckets.value[bucketKey];
    if (bucket && !bucket.loaded && !bucket.loading) {
        await loadBucketPage(bucketKey, 1);
    }
};

const initSessionBuckets = async () => {
    const token = ++bucketRequestToken;
    sessionListBooting.value = true;

    const defs = rebuildBucketDefinitions();
    bucketOrder.value = defs.map((def) => def.key);
    const buckets: Record<string, SidebarSessionBucket> = {};
    for (const def of defs) {
        buckets[def.key] = createEmptyBucket(def);
    }
    sessionBuckets.value = buckets;

    // 首屏：拉 web 会话 + 轻量探测各渠道 count；有会话的渠道才展示文件夹
    const channelKeys = defs.map((def) => def.key).filter((key) => isChannelBucketKey(key));
    await Promise.all([
        loadBucketPage('web', 1, token),
        probeChannelBucketCounts(channelKeys, token),
    ]);

    if (token === bucketRequestToken) {
        sessionListBooting.value = false;
        syncMenuStoreFromBuckets();
        await ensureBucketFillsViewport('web');
    }
};

const getMessageList = async () => {
    await initSessionBuckets();
};

// 滚动到底时为当前筛选来源加载下一页
const checkScrollBottom = async () => {
    const container = scrollContainer.value;
    const key = activeSessionBucketKey.value;
    const bucket = sessionBuckets.value[key];
    if (!container || !bucket || !bucketHasMore(bucket) || bucket.loading) return;

    const { scrollTop, scrollHeight, clientHeight } = container;
    const hasOverflow = scrollHeight > clientHeight + 1;
    if (!hasOverflow) {
        await ensureBucketFillsViewport(key);
        return;
    }

    const isNearBottom = scrollHeight - (scrollTop + clientHeight) < 100;
    if (!isNearBottom) return;

    await loadBucketPage(key);
};

const handleScroll = debounce(checkScrollBottom, 200);

const gotoSession = (path: string) => {
    router.push(`/platform/${path}`);
};

const loadSessionOriginMeta = async () => {
    try {
        const res: any = await listAllIMChannels();
        imPlatforms.value = configuredPlatforms(res?.data || []);
    } catch {
        imPlatforms.value = [];
    }
    try {
        const res: any = await listAllEmbedChannels();
        const names: Record<string, string> = {};
        for (const ch of res?.data || []) {
            if (ch?.id && ch?.name) names[ch.id] = ch.name;
        }
        embedChannelNames.value = names;
    } catch {
        embedChannelNames.value = {};
    }
};

const handleSessionMutation = (event: Event) => {
    const detail = (event as CustomEvent<SessionMutationDetail>).detail;
    if (!detail?.sessionId) return;
    if (detail.patch) {
        updateSessionInBuckets(detail.sessionId, {
            ...detail.patch,
            ...(detail.patch.title ? { isNoTitle: false } : {}),
        });
    }
    if (detail.removed) {
        sessionBuckets.value = removeSessionFromBuckets(sessionBuckets.value, detail.sessionId);
        syncMenuStoreFromBuckets();
        if (detail.sessionId === route.params.chatid) {
            router.push('/platform/creatChat');
        }
    }
};

onMounted(async () => {
    // PRD §3.2 断点行为：768–1023px 列表栏默认折叠（小屏画布优先）
    if (typeof window !== 'undefined' && window.innerWidth < 1024 && !uiStore.sidebarCollapsed) {
        uiStore.collapseSidebar();
    }

    if (route.params.chatid) {
        currentSecondpath.value = `chat/${route.params.chatid}`;
    }

    window.addEventListener(SESSION_MUTATION_EVENT, handleSessionMutation);

    getSystemInfo().then(res => {
        if (res.data?.edition === 'lite') {
            authStore.setLiteMode(true)
        }
    }).catch(() => { })

    await loadSessionOriginMeta();
    await getMessageList();
    const initialChatId = route.params.chatid as string | undefined;
    if (initialChatId) {
        ensureSessionInSidebar(initialChatId);
        await syncActiveBucketFromChat(initialChatId);
    }
    // 若组织列表未加载则拉取一次，用于铃铛/用户菜单「我的空间」的待审批角标（PRD §4.4）
    if (orgStore.organizations.length === 0) {
        orgStore.fetchOrganizations();
    }
});

onUnmounted(() => {
    window.removeEventListener(SESSION_MUTATION_EVENT, handleSessionMutation);
});

watch([() => route.name, () => route.params], (newvalue, oldvalue) => {
    // 路由 → 工作模式联动（settings 等非模式路由保持上一次模式）
    const mode = resolveModeFromRoute(route);
    if (mode && uiStore.activeMode !== mode) {
        uiStore.setActiveMode(mode);
    }

    if (newvalue[1].chatid) {
        currentSecondpath.value = `chat/${newvalue[1].chatid}`;
    } else {
        currentSecondpath.value = "";
    }

    // 创建新会话时 creatChat 会先 updataMenuChildren，再跳转 chat/:id。
    const newChatId = (newvalue[1] as any)?.chatid as string | undefined;
    if (newChatId) {
        ensureSessionInSidebar(newChatId);
        void syncActiveBucketFromChat(newChatId);
    }
});

// 模式切回问答时若列表为空（首次直连其他模式），补拉会话
watch(() => uiStore.activeMode, (mode) => {
    if (mode === 'chat' && !hasAnySession.value && !sessionListBooting.value) {
        void getMessageList();
    }
});

// 折叠态拖拽/点击展开
const onDragHandleMouseDown = (e: MouseEvent) => {
    e.preventDefault()
    const startX = e.clientX
    const expandThreshold = 40

    const onMouseMove = (ev: MouseEvent) => {
        if (ev.clientX - startX > expandThreshold) {
            uiStore.expandSidebar()
            cleanup()
        }
    }
    const onMouseUp = () => cleanup()
    const cleanup = () => {
        document.removeEventListener('mousemove', onMouseMove)
        document.removeEventListener('mouseup', onMouseUp)
    }
    document.addEventListener('mousemove', onMouseMove)
    document.addEventListener('mouseup', onMouseUp)
}
</script>

<style lang="less" scoped>
// 深色 tooltip 内容：标签 + 浅灰快捷键内联（自旧 menu.vue 迁移）
.cmdk-tip {
    display: inline-flex;
    align-items: center;
    gap: 8px;
    white-space: nowrap;

    .cmdk-tip-label {
        font-size: 13px;
    }

    .cmdk-tip-keys {
        font-size: 13px;
        opacity: 0.6;
        letter-spacing: 0.5px;
    }
}

.list-panel {
    width: var(--app-listbar-width);
    min-width: var(--app-listbar-width);
    flex-shrink: 0;
    display: flex;
    flex-direction: column;
    background: var(--td-bg-color-page);
    border-right: 1px solid var(--td-component-stroke);
    box-sizing: border-box;
    height: 100%;
    min-height: 0;
    overflow: hidden;
    transition: width 0.2s ease, min-width 0.2s ease;

    &--collapsed {
        width: 18px;
        min-width: 18px;
        background: var(--td-bg-color-sidebar);
        cursor: pointer;
    }
}

.list-panel__expand-strip {
    flex: 1;
    display: flex;
    align-items: center;
    justify-content: center;
    color: var(--td-font-gray-3);

    &:hover {
        color: var(--td-text-color-primary);
        background: var(--td-bg-color-container-hover);
    }
}

.list-panel__header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    height: 32px;
    padding: 0 8px 0 16px;
    flex-shrink: 0;
    margin-top: 10px;

    .list-panel__title {
        font: var(--td-font-title-small);
        color: var(--td-text-color-primary);
    }

    .list-panel__collapse {
        width: 26px;
        height: 26px;
        display: flex;
        align-items: center;
        justify-content: center;
        border: none;
        border-radius: var(--td-radius-small);
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
            outline-offset: 1px;
        }
    }
}

.list-panel__body {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
    padding-bottom: 8px;
    overflow: hidden;
}

// 主操作按钮：全站唯一强视觉按钮（黑底白字，PRD §5.1）
.list-panel__primary {
    padding: 8px 12px 4px;
    flex-shrink: 0;

    .primary-btn {
        width: 100%;
        height: 36px;
        display: flex;
        align-items: center;
        justify-content: center;
        gap: 8px;
        border: none;
        border-radius: var(--td-radius-default);
        background: var(--td-brand-color);
        color: var(--td-text-color-anti);
        font: var(--td-font-body-medium);
        cursor: pointer;
        transition: background-color 0.15s ease;

        &:hover {
            background: var(--td-brand-color-active);
        }

        &:focus-visible {
            outline: 2px solid var(--td-brand-color-focus);
            outline-offset: 2px;
        }
    }
}

// 搜索框：圆角胶囊、浅灰底（PRD §5.1）
.list-panel__search {
    margin: 8px 12px;
    height: 32px;
    flex-shrink: 0;
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 0 12px;
    border-radius: 999px;
    background: var(--td-bg-color-secondarycontainer);
    color: var(--td-font-gray-2);
    transition: box-shadow 0.15s ease;

    &:focus-within {
        box-shadow: 0 0 0 1px var(--td-brand-color);
    }

    input {
        flex: 1;
        min-width: 0;
        border: none;
        outline: none;
        background: transparent;
        color: var(--td-text-color-primary);
        font: var(--td-font-body-small);

        &::placeholder {
            color: var(--td-text-color-placeholder);
        }
    }
}

.submenu {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
    padding: 0 8px;
}

.session-list-scope-header {
    position: sticky;
    top: 0;
    z-index: 2;
    background: var(--td-bg-color-page);
    padding: 4px 4px 6px;
}

.submenu_item_p {
    position: relative;
}

.submenu_empty {
    padding: 32px 16px;
    text-align: center;
    color: var(--td-text-color-placeholder);
    font: var(--td-font-body-small);
}

.timeline_header {
    padding: 10px 4px 4px;

    .timeline_header-label {
        font: var(--td-font-link-small);
        color: var(--td-font-gray-3);
    }
}

.session-chat-row {
    border-radius: var(--td-radius-default);

    &--active {
        background: var(--td-bg-color-container-hover);
        box-shadow: inset 2px 0 0 var(--td-brand-color);
    }

    &--selected {
        background: var(--td-brand-color-light);
    }
}

.session-list-row {
    display: flex;
    align-items: center;

    &--flat {
        width: 100%;
    }

    .session-list-row__body {
        flex: 1;
        min-width: 0;
    }
}

.session-list-loading {
    justify-content: center;
    padding: 8px 0;
}

// 批量管理底部操作条
.batch-inline-footer {
    flex-shrink: 0;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    padding: 10px 12px;
    margin: 0 8px 4px;
    border-top: 1px solid var(--td-component-stroke);

    .batch-footer-left {
        flex-shrink: 0;
    }

    .batch-footer-right {
        display: flex;
        align-items: center;
        gap: 4px;
    }
}

// 断点（PRD §3.2）：1024–1279 固定窄栏；768–1023 默认折叠由 uiStore 初始值处理
@media (max-width: 1279px) {
    .list-panel {
        width: 232px;
        min-width: 232px;
    }
}
</style>
