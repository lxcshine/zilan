<script setup lang="ts">
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'
const { t } = useI18n()

const props = defineProps<{ kbId?: string }>()

const fileInputRef = ref<HTMLInputElement>()
const triggerFilePicker = () => {
    fileInputRef.value?.click()
}
const onFilesChosen = (event: Event) => {
    const input = event.target as HTMLInputElement
    const files = input.files && input.files.length > 0 ? Array.from(input.files) : []
    input.value = ''
    if (files.length === 0) return
    // 复用 platform 层全局上传通道：与拖拽 drop 相同的事件协议（kbId 必带，
    // KnowledgeBase 页的监听方按 kbId 过滤，防止误投到其他库）
    window.dispatchEvent(new CustomEvent('zilan:knowledge-file-drop', {
        detail: { kbId: props.kbId, files }
    }))
}
</script>
<template>
    <!-- PRD ui-layout-visual-redesign §9：画布内虚线卡片拖拽区。
         拖拽由 platform 层全局 dropzone 兜底，卡片自身支持点击触发系统文件选择，
         与全局上传入口（KbUploadSourceDropdown）行为一致。 -->
    <div class="empty" role="button" tabindex="0" :aria-label="t('knowledgeBase.emptyKnowledgeDragDrop')"
        @click="triggerFilePicker" @keydown.enter.prevent="triggerFilePicker">
        <input ref="fileInputRef" type="file" multiple class="empty__file-input"
            accept=".pdf,.docx,.pptx,.xlsx,.txt,.md,.csv,.html,.json,.xml,.yaml,.yml,.epub,.ipynb,image/*"
            @change="onFilesChosen">
        <svg class="empty-icon" viewBox="0 0 48 48" width="56" height="56" fill="none" aria-hidden="true">
            <rect x="8" y="14" width="32" height="26" rx="3" stroke="currentColor" stroke-width="2" />
            <path d="M17 14V9a2 2 0 0 1 2-2h10a2 2 0 0 1 2 2v5" stroke="currentColor" stroke-width="2"
                stroke-linejoin="round" />
            <path d="M24 22v11M18.5 27.5 24 22l5.5 5.5" stroke="currentColor" stroke-width="2" stroke-linecap="round"
                stroke-linejoin="round" />
        </svg>
        <span class="empty-txt">{{ t('knowledgeBase.emptyKnowledgeDragDrop') }}</span>
        <span class="empty-type-txt">{{ t('knowledgeBase.pdfDocFormat') }}</span>
        <span class="empty-type-txt">{{ t('knowledgeBase.textMarkdownFormat') }}</span>
    </div>
</template>
<style scoped lang="less">
.empty {
    flex: 1;
    display: flex;
    flex-flow: column;
    justify-content: center;
    align-items: center;
    margin: 16px;
    padding: 40px 24px;
    border: 1.5px dashed var(--td-component-border);
    border-radius: var(--td-radius-large, 14px);
    background: var(--td-bg-color-page);
    color: var(--td-text-color-placeholder);
    cursor: pointer;
    transition: border-color 0.2s ease, background-color 0.2s ease, color 0.2s ease;
    outline: none;

    &:hover,
    &:focus-visible {
        border-color: var(--td-brand-color);
        background: var(--td-brand-color-light);
        color: var(--td-text-color-secondary);
    }
}

.empty__file-input {
    display: none;
}

.empty-icon {
    color: inherit;
    opacity: 0.75;
}

.empty-txt {
    color: var(--td-text-color-placeholder);
    font-family: var(--app-font-family);
    font-size: 16px;
    font-weight: 600;
    line-height: 26px;
    margin: 12px 0 16px 0;
}

.empty-type-txt {
    color: var(--td-text-color-disabled);
    text-align: center;
    font-family: var(--app-font-family);
    font-size: 12px;
    font-weight: 400;
    width: 217px;
}
</style>
