<template>
  <div class="memory-list-container">
    <div class="memory-list-content">
      <div class="header">
        <div class="header-title">
          <h2>{{ t('memory.title') }}</h2>
          <p class="header-subtitle">{{ t('memory.subtitle', { count: factCount }) }}</p>
        </div>
        <div class="header-actions">
          <span class="switch-label">{{ t('memory.switchLabel') }}</span>
          <t-switch :value="memoryEnabled" :loading="switchLoading" :disabled="statusLoading" @change="handleSwitchChange" />
          <t-button theme="danger" variant="outline" size="small" :disabled="factCount === 0 || clearing" @click="openClearDialog">
            {{ t('memory.clear.button') }}
          </t-button>
        </div>
      </div>

      <div class="memory-list-main">
        <!-- 记忆功能关闭横幅：记忆仍保留，只是停止抽取与召回 -->
        <div v-if="!memoryEnabled && !statusLoading" class="memory-disabled-banner">
          <t-icon name="info-circle" size="16px" />
          <span>{{ t('memory.disabledBanner') }}</span>
        </div>

        <div class="memory-filter-bar">
          <div class="memory-category-tabs">
            <button
              v-for="tab in categoryTabs"
              :key="tab.value"
              type="button"
              class="memory-tab"
              :class="{ 'is-active': category === tab.value }"
              @click="selectCategory(tab.value)"
            >
              <span>{{ tab.label }}</span>
              <span v-if="counts[tab.value] !== undefined" class="memory-tab-count">{{ counts[tab.value] }}</span>
            </button>
          </div>
          <div class="memory-filter-right">
            <t-input
              v-model="searchKeyword"
              clearable
              :placeholder="t('memory.searchPlaceholder')"
              class="memory-search"
            >
              <template #prefix-icon>
                <t-icon name="search" />
              </template>
            </t-input>
            <t-select
              v-model="statusFilter"
              :options="statusOptions"
              class="memory-status-select"
            />
          </div>
        </div>

        <!-- 记忆路由未接线（404）：环境级兜底，不白屏 -->
        <div v-if="notAvailable" class="memory-state-block">
          <t-icon name="server" size="40px" class="memory-state-icon" />
          <div class="memory-state-title">{{ t('memory.notAvailable.title') }}</div>
          <div class="memory-state-desc">{{ t('memory.notAvailable.description') }}</div>
        </div>

        <!-- 首次加载骨架屏 -->
        <div v-else-if="loading && items.length === 0" class="memory-card-list">
          <div v-for="n in 5" :key="'skel-' + n" class="memory-card memory-card--skeleton">
            <t-skeleton
              animation="gradient"
              :row-col="[
                { width: '18%', height: '20px' },
                { width: '92%', height: '16px' },
                { width: '45%', height: '14px' },
              ]"
            />
          </div>
        </div>

        <!-- 加载失败 -->
        <div v-else-if="loadError" class="memory-state-block">
          <t-icon name="error-circle" size="40px" class="memory-state-icon" />
          <div class="memory-state-title">{{ t('memory.error.title') }}</div>
          <div class="memory-state-desc">{{ t('memory.error.description') }}</div>
          <t-button variant="outline" size="small" @click="refreshAll">
            {{ t('memory.error.retry') }}
          </t-button>
        </div>

        <!-- 空态 -->
        <div v-else-if="items.length === 0 && !loading" class="memory-state-block memory-empty-state">
          <div class="memory-empty-icon">
            <t-icon name="bookmark" size="26px" />
          </div>
          <div class="memory-state-title">{{ hasFilter ? t('memory.empty.filteredTitle') : t('memory.empty.title') }}</div>
          <div class="memory-state-desc">
            {{ hasFilter ? t('memory.empty.filteredDescription') : t('memory.empty.description') }}
          </div>
          <t-button v-if="!hasFilter" theme="primary" size="small" @click="goToNewChat">
            {{ t('memory.empty.cta') }}
          </t-button>
        </div>

        <!-- 记忆卡片列表 -->
        <template v-else>
          <div v-loading="loading" class="memory-card-list">
            <div
              v-for="fact in items"
              :key="fact.id"
              class="memory-card"
              :class="{ 'is-done': fact.status === 'done' }"
            >
              <div class="memory-card-main">
                <span class="memory-category-tag" :data-category="fact.category">
                  {{ categoryLabel(fact.category) }}
                </span>
                <span class="memory-content" :title="fact.content">{{ fact.content }}</span>
                <span class="memory-card-actions">
                  <t-tooltip :content="t('memory.edit.title')" placement="top">
                    <t-button variant="text" shape="square" size="small" @click.stop="openEdit(fact)">
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
                    @confirm="confirmDelete(fact)"
                  >
                    <t-button variant="text" shape="square" size="small" @click.stop>
                      <template #icon>
                        <t-icon name="delete" size="15px" />
                      </template>
                    </t-button>
                  </t-popconfirm>
                </span>
              </div>
              <div class="memory-card-meta">
                <span v-if="fact.status === 'done'" class="memory-done-chip">
                  <t-icon name="check" size="12px" />
                  {{ t('memory.status.done') }}
                </span>
                <span class="memory-meta-item">
                  <span class="memory-imp" :title="t('memory.card.importance')">
                    <i
                      v-for="seg in 5"
                      :key="seg"
                      class="memory-imp-seg"
                      :class="{ 'is-on': seg <= importanceLevel(fact) }"
                    />
                  </span>
                  {{ t('memory.card.importance') }}
                </span>
                <span class="memory-meta-sep">·</span>
                <span class="memory-meta-item">{{ t('memory.card.confidence', { value: confidencePct(fact) }) }}</span>
                <template v-if="fact.category === 'todo' && fact.due_at">
                  <span class="memory-meta-sep">·</span>
                  <span class="memory-due-chip" :class="{ 'is-overdue': isOverdue(fact) }">
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
          </div>

          <div v-if="total > pageSize" class="memory-pagination">
            <t-pagination
              :current="page"
              :total="total"
              :page-size="pageSize"
              :show-jumper="total > pageSize * 5"
              @current-change="handlePageChange"
            />
          </div>
        </template>
      </div>
    </div>

    <!-- 编辑抽屉 -->
    <t-drawer
      v-model:visible="editVisible"
      :header="t('memory.edit.title')"
      size="440px"
      :footer="false"
      :close-on-overlay-click="false"
    >
      <div class="memory-edit-form">
        <div class="memory-edit-field">
          <label class="memory-edit-label">{{ t('memory.edit.category') }}</label>
          <div class="memory-edit-category">
            <span class="memory-category-tag" :data-category="editingFact?.category">{{ categoryLabel(editingFact?.category) }}</span>
            <span class="memory-edit-category-hint">{{ t('memory.edit.categoryHint') }}</span>
          </div>
        </div>

        <div class="memory-edit-field">
          <label class="memory-edit-label">
            {{ t('memory.edit.content') }}
            <span class="memory-edit-required">*</span>
          </label>
          <t-textarea
            v-model="editForm.content"
            :autosize="{ minRows: 3, maxRows: 8 }"
            :placeholder="t('memory.edit.contentPlaceholder')"
            :status="editSubmitted && !editForm.content.trim() ? 'error' : undefined"
          />
          <div v-if="editSubmitted && !editForm.content.trim()" class="memory-edit-error">
            {{ t('memory.edit.contentRequired') }}
          </div>
        </div>

        <div class="memory-edit-field">
          <label class="memory-edit-label">{{ t('memory.edit.object') }}</label>
          <t-input v-model="editForm.object" :placeholder="t('memory.edit.objectPlaceholder')" />
        </div>

        <div class="memory-edit-field">
          <label class="memory-edit-label">{{ t('memory.edit.importance') }}</label>
          <div class="memory-importance-row">
            <t-slider v-model="editForm.importance" :min="0.1" :max="1" :step="0.1" />
            <span class="memory-importance-value">{{ editForm.importance.toFixed(1) }}</span>
          </div>
        </div>

        <div class="memory-edit-field">
          <label class="memory-edit-label">{{ t('memory.edit.status') }}</label>
          <t-radio-group v-model="editForm.status" variant="default-filled">
            <t-radio-button value="active">{{ t('memory.status.active') }}</t-radio-button>
            <t-radio-button value="done">{{ t('memory.status.done') }}</t-radio-button>
            <t-radio-button value="archived">{{ t('memory.status.archived') }}</t-radio-button>
          </t-radio-group>
        </div>

        <div v-if="editingFact?.category === 'todo'" class="memory-edit-field">
          <label class="memory-edit-label">{{ t('memory.edit.due') }}</label>
          <t-date-picker
            v-model="editForm.dueAt"
            mode="date"
            clearable
            :placeholder="t('memory.edit.duePlaceholder')"
          />
        </div>

        <div class="memory-reembed-hint">{{ t('memory.edit.reembedHint') }}</div>

        <div class="memory-edit-footer">
          <t-button variant="outline" :disabled="editSaving" @click="editVisible = false">
            {{ t('common.cancel') }}
          </t-button>
          <t-button theme="primary" :loading="editSaving" @click="saveEdit">
            {{ t('memory.edit.save') }}
          </t-button>
        </div>
      </div>
    </t-drawer>

    <!-- 清空全部（强确认） -->
    <t-dialog
      v-model:visible="clearVisible"
      :header="t('memory.clear.title')"
      :confirm-btn="{
        content: t('memory.clear.confirm'),
        theme: 'danger',
        disabled: !clearArmed,
        loading: clearing,
      }"
      :cancel-btn="{ content: t('memory.clear.cancel') }"
      :close-on-overlay-click="!clearing"
      @closed="clearText = ''"
    >
      <div class="memory-clear-dialog">
        <div class="memory-clear-warning">
          <t-icon name="error-circle" size="16px" />
          <span>{{ t('memory.clear.description', { count: factCount }) }}</span>
        </div>
        <div class="memory-clear-tip">{{ t('memory.clear.tip') }}</div>
        <div class="memory-clear-input">
          <t-input
            v-model="clearText"
            :placeholder="t('memory.clear.inputPlaceholder', { word: t('memory.clear.confirmWord') })"
            :disabled="clearing"
          />
        </div>
      </div>
    </t-dialog>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { MessagePlugin } from 'tdesign-vue-next'
import {
  deleteAllMemories,
  deleteMemoryFact,
  getMemoryStatus,
  listMemoryFacts,
  updateMemoryFact,
  type MemoryCategory,
  type MemoryFact,
  type MemoryStatus,
} from '@/api/memory'
import { updateMyPreferences } from '@/api/auth'

const { t } = useI18n()
const router = useRouter()

const MEMORY_CATEGORIES: MemoryCategory[] = ['profile', 'fact', 'preference', 'todo', 'feedback']

const pageSize = 20

// ---- 状态 ----
const memoryEnabled = ref(true)
const statusLoading = ref(true)
const switchLoading = ref(false)
const loading = ref(false)
const loadError = ref(false)
const notAvailable = ref(false)

const items = ref<MemoryFact[]>([])
const total = ref(0)
const factCount = ref(0)
const page = ref(1)
const category = ref<'' | MemoryCategory>('')
const statusFilter = ref<'active' | 'done' | 'archived' | 'all'>('active')
const searchKeyword = ref('')
const counts = ref<Record<string, number>>({})

// ---- 编辑抽屉 ----
const editVisible = ref(false)
const editingFact = ref<MemoryFact | null>(null)
const editSaving = ref(false)
const editSubmitted = ref(false)
const editForm = reactive({
  content: '',
  object: '',
  importance: 0.5,
  status: 'active' as MemoryStatus,
  dueAt: '',
})

// ---- 清空确认 ----
const clearVisible = ref(false)
const clearText = ref('')
const clearing = ref(false)

// ---- 计算属性 ----
const categoryTabs = computed(() => [
  { value: '' as const, label: t('memory.categories.all') },
  ...MEMORY_CATEGORIES.map(c => ({ value: c, label: t(`memory.categories.${c}`) })),
])

const statusOptions = computed(() => [
  { label: t('memory.status.active'), value: 'active' },
  { label: t('memory.status.done'), value: 'done' },
  { label: t('memory.status.archived'), value: 'archived' },
  { label: t('memory.status.all'), value: 'all' },
])

const hasFilter = computed(
  () => category.value !== '' || statusFilter.value !== 'active' || searchKeyword.value.trim() !== '',
)

const clearArmed = computed(() => clearText.value.trim() === t('memory.clear.confirmWord'))

// ---- 数据加载 ----
// 请求序号：防止快速切换筛选时旧响应覆盖新响应（竞态防护）。
let listRequestId = 0

async function fetchStatus() {
  statusLoading.value = true
  try {
    const data = await getMemoryStatus()
    memoryEnabled.value = data.enabled
    factCount.value = data.fact_count
  } catch {
    // 状态获取失败不阻塞列表；开关保持默认显示，用户可重试。
  } finally {
    statusLoading.value = false
  }
}

async function fetchList() {
  const requestId = ++listRequestId
  loading.value = true
  loadError.value = false
  try {
    const data = await listMemoryFacts({
      category: category.value,
      status: statusFilter.value,
      keyword: searchKeyword.value.trim(),
      page: page.value,
      page_size: pageSize,
    })
    if (requestId !== listRequestId) return
    items.value = data.items ?? []
    total.value = data.total ?? 0
  } catch (err: any) {
    if (requestId !== listRequestId) return
    items.value = []
    total.value = 0
    if (err?.status === 404) {
      // 后端未接线 memory 依赖时路由不注册（RegisterMemoryRoutes no-op），
      // 表现为 404：这是环境形态而非瞬时故障，走专用兜底态。
      notAvailable.value = true
    } else {
      loadError.value = true
    }
  } finally {
    if (requestId === listRequestId) {
      loading.value = false
    }
  }
}

// 分类计数：并行 6 个 page_size=1 轻量请求（全部 + 5 分类），按当前状态筛选。
async function fetchCounts() {
  if (notAvailable.value) return
  const statusParam = statusFilter.value
  const keys: string[] = ['', ...MEMORY_CATEGORIES]
  try {
    const results = await Promise.all(
      keys.map(key =>
        listMemoryFacts({
          category: key as '' | MemoryCategory,
          status: statusParam,
          page: 1,
          page_size: 1,
        }).then(data => data.total ?? 0),
      ),
    )
    const next: Record<string, number> = {}
    keys.forEach((key, i) => {
      next[key] = results[i]
    })
    counts.value = next
  } catch {
    // 计数属增强信息，失败静默（Tabs 退化为无计数展示）。
  }
}

function refreshAll() {
  fetchStatus()
  fetchList()
  fetchCounts()
}

// ---- 筛选交互 ----
function selectCategory(value: '' | MemoryCategory) {
  if (category.value === value) return
  category.value = value
  page.value = 1
  fetchList()
}

function handlePageChange(next: number) {
  page.value = next
  fetchList()
}

// 搜索防抖 300ms
let searchTimer: ReturnType<typeof setTimeout> | null = null
watch(searchKeyword, () => {
  if (searchTimer) clearTimeout(searchTimer)
  searchTimer = setTimeout(() => {
    page.value = 1
    fetchList()
  }, 300)
})

watch(statusFilter, () => {
  page.value = 1
  fetchList()
  fetchCounts()
})

// ---- 记忆开关 ----
async function handleSwitchChange(value: unknown) {
  const next = value === true
  const prev = memoryEnabled.value
  if (next === prev) return
  memoryEnabled.value = next
  switchLoading.value = true
  try {
    await updateMyPreferences({ memory_enabled: next })
    MessagePlugin.success(next ? t('memory.switchEnabled') : t('memory.switchDisabled'))
  } catch {
    // 失败回滚 UI，保持与服务端一致
    memoryEnabled.value = prev
    MessagePlugin.error(t('memory.switchFailed'))
  } finally {
    switchLoading.value = false
  }
}

// ---- 编辑 ----
function categoryLabel(c?: string): string {
  if (!c) return ''
  return t(`memory.categories.${c}`)
}

function openEdit(fact: MemoryFact) {
  editingFact.value = fact
  editSubmitted.value = false
  editForm.content = fact.content
  editForm.object = fact.object ?? ''
  editForm.importance = fact.importance > 0 ? fact.importance : 0.5
  editForm.status = fact.status
  editForm.dueAt = fact.due_at ? fact.due_at.slice(0, 10) : ''
  editVisible.value = true
}

async function saveEdit() {
  editSubmitted.value = true
  const fact = editingFact.value
  if (!fact) return
  const content = editForm.content.trim()
  if (!content) return

  editSaving.value = true
  try {
    // 后端语义：due_at 传空 = 保持原值（不支持清除），仅在用户填了日期时传递。
    await updateMemoryFact(fact.id, {
      content,
      object: editForm.object.trim(),
      status: editForm.status,
      importance: editForm.importance,
      ...(fact.category === 'todo' && editForm.dueAt ? { due_at: editForm.dueAt } : {}),
    })
    MessagePlugin.success(t('memory.edit.success'))
    editVisible.value = false
    fetchList()
    fetchCounts()
    // 重要性/状态变更会改变 active 集合，同步刷新头部计数。
    if (fact.status !== editForm.status || editForm.status === 'done' || fact.status === 'done') {
      fetchStatus()
    }
  } catch {
    MessagePlugin.error(t('memory.edit.failed'))
  } finally {
    editSaving.value = false
  }
}

// ---- 删除单条 ----
async function confirmDelete(fact: MemoryFact) {
  try {
    await deleteMemoryFact(fact.id)
    MessagePlugin.success(t('memory.delete.success'))
    // 当前页删空时回退一页，避免停留在空页。
    if (items.value.length === 1 && page.value > 1) {
      page.value -= 1
    }
    fetchList()
    fetchCounts()
    fetchStatus()
  } catch {
    MessagePlugin.error(t('memory.delete.failed'))
  }
}

// ---- 清空全部 ----
function openClearDialog() {
  clearText.value = ''
  clearVisible.value = true
}

async function confirmClear() {
  if (!clearArmed.value || clearing.value) return
  clearing.value = true
  try {
    const deleted = await deleteAllMemories()
    MessagePlugin.success(t('memory.clear.success', { count: deleted }))
    clearVisible.value = false
    page.value = 1
    category.value = ''
    searchKeyword.value = ''
    items.value = []
    total.value = 0
    refreshAll()
  } catch {
    MessagePlugin.error(t('memory.clear.failed'))
  } finally {
    clearing.value = false
  }
}

// ---- 展示工具 ----
function importanceLevel(fact: MemoryFact): number {
  return Math.min(5, Math.max(0, Math.round((fact.importance ?? 0) * 5)))
}

function confidencePct(fact: MemoryFact): number {
  return Math.round((fact.confidence ?? 0) * 100)
}

function isOverdue(fact: MemoryFact): boolean {
  if (!fact.due_at) return false
  const due = new Date(fact.due_at).getTime()
  return Number.isFinite(due) && due < Date.now()
}

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

function goToNewChat() {
  router.push('/platform/creatChat')
}

onMounted(() => {
  refreshAll()
})

onBeforeUnmount(() => {
  if (searchTimer) clearTimeout(searchTimer)
})
</script>

<style scoped lang="less">
.memory-list-container {
  margin: 0;
  height: 100%;
  box-sizing: border-box;
  flex: 1;
  display: flex;
  position: relative;
  min-height: 0;
}

.memory-list-content {
  flex: 1;
  display: flex;
  flex-direction: column;
  min-width: 0;
  padding: 20px 0 0 28px;
}

.header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 16px;
  padding-right: 28px;

  .header-title {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  h2 {
    margin: 0;
    color: var(--td-text-color-primary);
    font-family: var(--app-font-family);
    font-size: 24px;
    font-weight: 600;
    line-height: 32px;
  }
}

.header-subtitle {
  margin: 0;
  color: var(--td-text-color-secondary);
  font-size: 13px;
  line-height: 20px;
}

.header-actions {
  display: flex;
  align-items: center;
  gap: 12px;
  flex: none;
}

.switch-label {
  color: var(--td-text-color-secondary);
  font-size: 13px;
}

.memory-list-main {
  flex: 1;
  min-width: 0;
  overflow-y: auto;
  overflow-x: hidden;
  padding: 0 28px 8px 0;
  scrollbar-width: auto;
  scrollbar-color: auto;
}

.memory-disabled-banner {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 10px 14px;
  margin-bottom: 12px;
  border: 1px solid var(--td-warning-color-3);
  border-radius: 8px;
  background: var(--td-warning-color-1);
  color: var(--td-text-color-primary);
  font-size: 13px;
  line-height: 20px;

  .t-icon {
    color: var(--td-warning-color-5);
    flex: none;
  }
}

.memory-filter-bar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  flex-wrap: wrap;
  margin-bottom: 14px;
}

.memory-category-tabs {
  display: flex;
  align-items: center;
  gap: 4px;
  flex-wrap: wrap;
}

.memory-tab {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 5px 12px;
  border: none;
  border-radius: 6px;
  background: transparent;
  color: var(--td-text-color-secondary);
  font-size: 14px;
  line-height: 20px;
  cursor: pointer;
  transition: all 0.2s ease;

  &:hover {
    color: var(--td-text-color-primary);
    background: var(--td-bg-color-container-hover);
  }

  &.is-active {
    color: var(--td-brand-color);
    background: var(--td-brand-color-light);
    font-weight: 500;
  }
}

.memory-tab-count {
  font-size: 12px;
  line-height: 16px;
  color: var(--td-text-color-placeholder);

  .is-active & {
    color: var(--td-brand-color);
  }
}

.memory-filter-right {
  display: flex;
  align-items: center;
  gap: 8px;
}

.memory-search {
  width: 220px;
}

.memory-status-select {
  width: 130px;
}

.memory-card-list {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

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

  &.memory-card--skeleton {
    pointer-events: none;

    &:hover {
      border-color: var(--td-component-stroke);
      box-shadow: 0 1px 3px rgba(0, 0, 0, 0.04);
    }
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

.memory-pagination {
  display: flex;
  justify-content: flex-end;
  padding: 16px 0 8px;
}

.memory-state-block {
  flex: 1;
  display: flex;
  flex-direction: column;
  justify-content: center;
  align-items: center;
  gap: 8px;
  padding: 60px 20px;
  text-align: center;
}

.memory-state-icon {
  color: var(--td-text-color-placeholder);
  margin-bottom: 4px;
}

.memory-state-title {
  color: var(--td-text-color-placeholder);
  font-size: 16px;
  font-weight: 600;
  line-height: 24px;
}

.memory-state-desc {
  color: var(--td-text-color-placeholder);
  font-size: 13px;
  line-height: 20px;
  max-width: 420px;
}

.memory-empty-icon {
  width: 56px;
  height: 56px;
  border-radius: 50%;
  display: flex;
  align-items: center;
  justify-content: center;
  background: var(--td-brand-color-light);
  color: var(--td-brand-color);
  margin-bottom: 4px;
}

// ---- 编辑抽屉 ----
.memory-edit-form {
  display: flex;
  flex-direction: column;
  gap: 18px;
}

.memory-edit-field {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.memory-edit-label {
  color: var(--td-text-color-secondary);
  font-size: 13px;
  font-weight: 500;
  line-height: 20px;

  .memory-edit-required {
    color: var(--td-error-color-6);
  }
}

.memory-edit-category {
  display: flex;
  align-items: center;
  gap: 8px;
}

.memory-edit-category-hint {
  color: var(--td-text-color-placeholder);
  font-size: 12px;
}

.memory-edit-error {
  color: var(--td-error-color-6);
  font-size: 12px;
  line-height: 18px;
}

.memory-importance-row {
  display: flex;
  align-items: center;
  gap: 12px;
}

.memory-importance-value {
  flex: none;
  min-width: 28px;
  text-align: right;
  color: var(--td-text-color-primary);
  font-size: 13px;
  font-variant-numeric: tabular-nums;
}

.memory-reembed-hint {
  padding: 8px 12px;
  border-radius: 6px;
  background: var(--td-bg-color-secondarycontainer);
  color: var(--td-text-color-placeholder);
  font-size: 12px;
  line-height: 18px;
}

.memory-edit-footer {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
  padding-top: 4px;
}

// ---- 清空确认 ----
.memory-clear-dialog {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.memory-clear-warning {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  color: var(--td-error-color-6);
  font-size: 13px;
  line-height: 20px;

  .t-icon {
    flex: none;
    margin-top: 2px;
  }
}

.memory-clear-tip {
  color: var(--td-text-color-placeholder);
  font-size: 12px;
  line-height: 18px;
}
</style>
