<template>
    <!-- 底部快捷入口（PRD §7.2）：4 个圆形 Icon 快捷组件，映射知澜真实能力 -->
    <div class="quick-actions" role="toolbar" :aria-label="t('quickActions.title')">
        <!-- 文档解读：选文件 → 注入输入框附件，进入以该文档为上下文的新对话 -->
        <input ref="fileInputRef" type="file" multiple class="quick-actions__file-input"
            @change="onFileChosen" />
        <div class="quick-actions__item-wrap">
            <button type="button" class="quick-actions__item" :aria-label="t('quickActions.docAnalyze')"
                :title="t('quickActions.docAnalyze')" @click="fileInputRef?.click()">
                <svg viewBox="0 0 20 20" width="20" height="20" fill="none" aria-hidden="true">
                    <path d="M4.5 3.5h7l4 4v9a1 1 0 0 1-1 1h-10a1 1 0 0 1-1-1v-12a1 1 0 0 1 1-1Z"
                        stroke="currentColor" stroke-width="1.2" stroke-linejoin="round" />
                    <path d="M11.5 3.5v4h4" stroke="currentColor" stroke-width="1.2" stroke-linejoin="round" />
                    <circle cx="9.5" cy="11" r="2.6" stroke="currentColor" stroke-width="1.2" />
                    <line x1="11.6" y1="13.1" x2="13.8" y2="15.3" stroke="currentColor" stroke-width="1.2"
                        stroke-linecap="round" />
                </svg>
                <span class="quick-actions__label">{{ t('quickActions.docAnalyze') }}</span>
            </button>
        </div>

        <!-- 智能写作：模板选择后预填输入框 -->
        <div class="quick-actions__item-wrap" ref="writingWrapRef">
            <button type="button" class="quick-actions__item" :aria-label="t('quickActions.smartWriting')"
                :title="t('quickActions.smartWriting')" @click="writingMenuVisible = !writingMenuVisible">
                <svg viewBox="0 0 20 20" width="20" height="20" fill="none" aria-hidden="true">
                    <path d="M13.2 3.6 16.4 6.8 7.6 15.6 4 16.6l1-3.6 8.2-9.4Z" stroke="currentColor"
                        stroke-width="1.2" stroke-linejoin="round" />
                    <path d="M11.8 5 14.9 8.1" stroke="currentColor" stroke-width="1.2" />
                </svg>
                <span class="quick-actions__label">{{ t('quickActions.smartWriting') }}</span>
            </button>
            <transition name="qa-pop">
                <div v-if="writingMenuVisible" class="quick-actions__menu" role="menu">
                    <button v-for="tpl in writingTemplates" :key="tpl.key" type="button"
                        class="quick-actions__menu-item" role="menuitem" @click="chooseWritingTemplate(tpl)">
                        {{ t(tpl.labelKey) }}
                    </button>
                </div>
            </transition>
        </div>

        <!-- 网页摘要：预填 prompt + 聚焦 -->
        <div class="quick-actions__item-wrap">
            <button type="button" class="quick-actions__item" :aria-label="t('quickActions.webSummary')"
                :title="t('quickActions.webSummary')" @click="$emit('prefill', t('quickActions.webSummaryPrompt'))">
                <svg viewBox="0 0 20 20" width="20" height="20" fill="none" aria-hidden="true">
                    <path d="M8.4 11.6a2.6 2.6 0 1 1 3.6-3.6 2.6 2.6 0 0 1-3.6 3.6Z" stroke="currentColor"
                        stroke-width="1.2" />
                    <path d="m10.6 9.4 4.2-4.2M12.9 3.9l3.2 3.2M8.4 11.6 5 15" stroke="currentColor"
                        stroke-width="1.2" stroke-linecap="round" />
                </svg>
                <span class="quick-actions__label">{{ t('quickActions.webSummary') }}</span>
            </button>
        </div>

        <!-- 新建知识库：打开 KB 创建弹窗 -->
        <div class="quick-actions__item-wrap">
            <button type="button" class="quick-actions__item" :aria-label="t('quickActions.newKnowledgeBase')"
                :title="t('quickActions.newKnowledgeBase')" @click="$emit('create-kb')">
                <svg viewBox="0 0 20 20" width="20" height="20" fill="none" aria-hidden="true">
                    <path d="M10 4.2c-1.5-1.2-3.6-1.5-5.8-1v12.4c2.2-.5 4.3-.2 5.8 1 1.5-1.2 3.6-1.5 5.8-1V3.2c-2.2-.5-4.3-.2-5.8 1Z"
                        stroke="currentColor" stroke-width="1.2" stroke-linejoin="round" />
                    <path d="M10 4.2v12.4" stroke="currentColor" stroke-width="1.2" />
                </svg>
                <span class="quick-actions__label">{{ t('quickActions.newKnowledgeBase') }}</span>
            </button>
        </div>
    </div>
</template>

<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'

const { t } = useI18n()

const emit = defineEmits<{
    (e: 'files', files: File[]): void
    (e: 'prefill', text: string): void
    (e: 'create-kb'): void
}>()

const fileInputRef = ref<HTMLInputElement>()
const writingMenuVisible = ref(false)
const writingWrapRef = ref<HTMLElement | null>(null)

const writingTemplates = [
    { key: 'continue', labelKey: 'quickActions.writingContinue', promptKey: 'quickActions.writingContinuePrompt' },
    { key: 'polish', labelKey: 'quickActions.writingPolish', promptKey: 'quickActions.writingPolishPrompt' },
    { key: 'summarize', labelKey: 'quickActions.writingSummarize', promptKey: 'quickActions.writingSummarizePrompt' },
    { key: 'translate', labelKey: 'quickActions.writingTranslate', promptKey: 'quickActions.writingTranslatePrompt' },
] as const

const onFileChosen = (event: Event) => {
    const input = event.target as HTMLInputElement
    if (!input.files || input.files.length === 0) return
    const files = Array.from(input.files)
    input.value = ''
    emit('files', files)
}

const chooseWritingTemplate = (tpl: (typeof writingTemplates)[number]) => {
    writingMenuVisible.value = false
    emit('prefill', t(tpl.promptKey))
}

// 点击外部关闭写作模板菜单
const onDocClick = (e: MouseEvent) => {
    if (!writingMenuVisible.value) return
    if (writingWrapRef.value && !writingWrapRef.value.contains(e.target as Node)) {
        writingMenuVisible.value = false
    }
}

onMounted(() => document.addEventListener('mousedown', onDocClick))
onBeforeUnmount(() => document.removeEventListener('mousedown', onDocClick))
</script>

<style lang="less" scoped>
.quick-actions {
    display: flex;
    align-items: flex-start;
    justify-content: center;
    gap: 24px;
}

.quick-actions__file-input {
    display: none;
}

.quick-actions__item-wrap {
    position: relative;
    display: flex;
    flex-direction: column;
    align-items: center;
}

// 48px 圆形白卡片 + 12px caption（PRD §7.2）
.quick-actions__item {
    width: 48px;
    height: 48px;
    display: flex;
    align-items: center;
    justify-content: center;
    border: 1px solid var(--td-component-border);
    border-radius: 999px;
    background: var(--td-bg-color-container);
    color: var(--td-text-color-secondary);
    cursor: pointer;
    box-shadow: var(--td-shadow-1);
    transition: transform 0.15s ease, box-shadow 0.15s ease, color 0.15s ease, border-color 0.15s ease;

    &:hover {
        transform: translateY(-2px);
        box-shadow: var(--td-shadow-2);
        color: var(--td-brand-color);
        border-color: var(--td-brand-color);
    }

    &:focus-visible {
        outline: 2px solid var(--td-brand-color);
        outline-offset: 2px;
    }
}

.quick-actions__label {
    margin-top: 8px;
    font: var(--td-font-link-small);
    color: var(--td-text-color-placeholder);
    white-space: nowrap;
}

.quick-actions__menu {
    position: absolute;
    top: calc(100% + 30px);
    left: 50%;
    transform: translateX(-50%);
    min-width: 140px;
    padding: 6px;
    border-radius: var(--td-radius-large);
    background: var(--td-bg-color-container);
    border: 1px solid var(--td-component-border);
    box-shadow: var(--td-shadow-2);
    z-index: 30;
}

.quick-actions__menu-item {
    display: block;
    width: 100%;
    padding: 8px 12px;
    border: none;
    border-radius: var(--td-radius-small);
    background: transparent;
    color: var(--td-text-color-primary);
    font: var(--td-font-body-small);
    text-align: left;
    cursor: pointer;
    white-space: nowrap;

    &:hover {
        background: var(--td-bg-color-container-hover);
    }
}

.qa-pop-enter-active,
.qa-pop-leave-active {
    transition: opacity 0.15s ease, transform 0.15s ease;
}

.qa-pop-enter-from,
.qa-pop-leave-to {
    opacity: 0;
    transform: translateX(-50%) translateY(-4px);
}
</style>
