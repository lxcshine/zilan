<template>
  <div
    :id="domId || undefined"
    class="memory-card"
    :class="{ 'is-done': fact.status === 'done', 'is-highlight': highlighted }"
  >
    <div class="memory-card-main">
      <span class="memory-category-tag" :data-category="fact.category">
        {{ t(`memory.categories.${fact.category}`) }}
      </span>
      <span class="memory-content" :title="fact.content">{{ fact.content }}</span>
      <span class="memory-card-actions">
        <t-tooltip :content="t('memory.edit.title')" placement="top">
          <t-button variant="text" shape="square" size="small" @click.stop="emit('edit', fact)">
            <template #icon>
              <t-icon name="edit" size="15px" />
            </template>
          </t-button>
        </t-tooltip>
        <t-popconfirm
          theme="danger"
          :content="t('memory.delete.confirm')"
          :confirm-btn="{ content: t('memory.delete.confirmButton'), theme: 'danger' }"
          :cancel-btn="{ content: t('common.cancel') }"
          @confirm="emit('delete', fact)"
        >
          <t-button variant="text" shape="square" size="small" @click.stop>
            <template #icon>
              <t-icon name="delete" size="15px" />
            </template>
          </t-button>
        </t-popconfirm>
      </span>
    </div>

    <div v-if="$slots.extra" class="memory-card-extra">
      <slot name="extra" />
    </div>

    <div class="memory-card-meta">
      <span v-if="fact.status === 'done'" class="memory-done-chip">
        <t-icon name="check" size="12px" />
        {{ t('memory.status.done') }}
      </span>
      <span class="memory-meta-item">
        <span class="memory-imp" :title="t('memory.card.importance')">
          <i v-for="seg in 5" :key="seg" class="memory-imp-seg" :class="{ 'is-on': seg <= importanceLevel }" />
        </span>
        {{ t('memory.card.importance') }}
      </span>
      <span class="memory-meta-sep">·</span>
      <span class="memory-meta-item">{{ t('memory.card.confidence', { value: confidencePct }) }}</span>
      <template v-if="fact.category === 'todo' && fact.due_at">
        <span class="memory-meta-sep">·</span>
        <span class="memory-due-chip" :class="{ 'is-overdue': isOverdue }">
          <t-icon name="time" size="12px" />
          {{ t('memory.card.due', { date: shortDate(fact.due_at) }) }}
        </span>
      </template>
      <span class="memory-meta-sep">·</span>
      <t-tooltip :disabled="!fact.last_accessed_at" placement="top">
        <template #content>
          {{ t('memory.card.lastRecall') }}: {{ formatDateTime(fact.last_accessed_at) }}
        </template>
        <span class="memory-meta-item">
          {{ fact.access_count > 0 ? t('memory.card.recalled', { n: fact.access_count }) : t('memory.card.neverRecalled') }}
        </span>
      </t-tooltip>
      <span class="memory-meta-sep">·</span>
      <span class="memory-meta-item">{{ relativeTime(fact.updated_at) }}</span>
    </div>
  </div>
</template>

<script setup lang="ts">
// 长期记忆卡片：记忆流列表 / Soul 微调指令 / Agent 技巧三处复用。
// extra 插槽用于附加展示（如技巧卡的"来自你的反馈"来源标签）。
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { MemoryFact } from '@/api/memory'

const props = withDefaults(
  defineProps<{
    fact: MemoryFact
    /** 定位锚点 id（反馈墙点击"已升级为技巧"时滚动定位到对应技巧卡） */
    domId?: string
    /** 短暂高亮态（被定位时） */
    highlighted?: boolean
  }>(),
  { domId: '', highlighted: false },
)

const emit = defineEmits<{
  (e: 'edit', fact: MemoryFact): void
  (e: 'delete', fact: MemoryFact): void
}>()

const { t } = useI18n()

const importanceLevel = computed(() => Math.min(5, Math.max(0, Math.round((props.fact.importance ?? 0) * 5))))
const confidencePct = computed(() => Math.round((props.fact.confidence ?? 0) * 100))
const isOverdue = computed(() => {
  if (!props.fact.due_at) return false
  const due = new Date(props.fact.due_at).getTime()
  return Number.isFinite(due) && due < Date.now()
})

function shortDate(dateStr?: string): string {
  if (!dateStr) return ''
  const d = new Date(dateStr)
  if (Number.isNaN(d.getTime())) return ''
  const month = String(d.getMonth() + 1).padStart(2, '0')
  const day = String(d.getDate()).padStart(2, '0')
  return `${month}-${day}`
}

function formatDateTime(dateStr?: string): string {
  if (!dateStr) return ''
  const d = new Date(dateStr)
  if (Number.isNaN(d.getTime())) return ''
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

function relativeTime(dateStr?: string): string {
  if (!dateStr) return ''
  const d = new Date(dateStr)
  if (Number.isNaN(d.getTime())) return ''
  const diff = Date.now() - d.getTime()
  const minute = 60_000
  const hour = 3_600_000
  const day = 86_400_000
  if (diff < minute) return t('memory.time.justNow')
  if (diff < hour) return t('memory.time.minutesAgo', { n: Math.floor(diff / minute) })
  if (diff < day) return t('memory.time.hoursAgo', { n: Math.floor(diff / hour) })
  if (diff < 7 * day) return t('memory.time.daysAgo', { n: Math.floor(diff / day) })
  return formatDateTime(dateStr)
}
</script>

<style scoped lang="less">
.memory-card {
  padding: 12px 16px;
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
  box-shadow: 0 1px 3px rgba(0, 0, 0, 0.04);
  background: var(--td-bg-color-container);
  transition: all 0.25s ease;

  &:hover {
    border-color: var(--td-brand-color);
    box-shadow: 0 4px 12px rgba(7, 192, 95, 0.12);
  }

  &.is-done {
    opacity: 0.62;
    background: var(--td-bg-color-page);

    .memory-content {
      text-decoration: line-through;
      text-decoration-color: var(--td-text-color-placeholder);
    }
  }

  &.is-highlight {
    border-color: var(--td-brand-color);
    box-shadow: 0 0 0 2px rgba(7, 192, 95, 0.18);
  }
}

.memory-card-main {
  display: flex;
  align-items: flex-start;
  gap: 10px;
}

.memory-category-tag {
  flex: none;
  display: inline-flex;
  align-items: center;
  padding: 1px 8px;
  border-radius: 999px;
  border: 1px solid var(--td-component-stroke);
  background: var(--td-bg-color-secondarycontainer);
  color: var(--td-text-color-secondary);
  font-size: 12px;
  line-height: 18px;
  margin-top: 1px;
  white-space: nowrap;
}

.memory-content {
  flex: 1;
  min-width: 0;
  color: var(--td-text-color-primary);
  font-size: 14px;
  line-height: 22px;
  word-break: break-word;
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}

.memory-card-actions {
  flex: none;
  display: inline-flex;
  align-items: center;
  gap: 4px;
  opacity: 0;
  transition: opacity 0.2s ease;

  .memory-card:hover &,
  .memory-card:focus-within & {
    opacity: 1;
  }
}

.memory-card-extra {
  margin-top: 8px;
}

.memory-card-meta {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 4px;
  margin-top: 8px;
  color: var(--td-text-color-placeholder);
  font-size: 12px;
  line-height: 18px;
}

.memory-meta-item {
  display: inline-flex;
  align-items: center;
  gap: 4px;
}

.memory-meta-sep {
  color: var(--td-component-stroke);
  padding: 0 2px;
}

.memory-done-chip {
  display: inline-flex;
  align-items: center;
  gap: 2px;
  padding: 0 8px;
  border-radius: 999px;
  background: var(--td-success-color-1);
  color: var(--td-success-color-6);
}

.memory-imp {
  display: inline-flex;
  align-items: center;
  gap: 2px;
}

.memory-imp-seg {
  width: 12px;
  height: 4px;
  border-radius: 2px;
  background: var(--td-component-stroke);

  &.is-on {
    background: var(--td-brand-color);
  }
}

.memory-due-chip {
  display: inline-flex;
  align-items: center;
  gap: 2px;
  padding: 0 8px;
  border-radius: 999px;
  border: 1px solid var(--td-component-stroke);
  color: var(--td-text-color-secondary);

  &.is-overdue {
    border-color: var(--td-error-color-3);
    background: var(--td-error-color-1);
    color: var(--td-error-color-6);
  }
}
</style>
