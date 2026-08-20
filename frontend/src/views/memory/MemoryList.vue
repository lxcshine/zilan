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

        <!-- 记忆路由未接线（404）：环境级兜底，不白屏 -->
        <div v-if="notAvailable" class="memory-state-block">
          <t-icon name="server" size="40px" class="memory-state-icon" />
          <div class="memory-state-title">{{ t('memory.notAvailable.title') }}</div>
          <div class="memory-state-desc">{{ t('memory.notAvailable.description') }}</div>
        </div>

        <template v-else>
          <!-- 一级导航：四模块（Soul / User / Memory / Agent） -->
          <div class="memory-module-nav">
            <button
              v-for="m in moduleTabs"
              :key="m.value"
              type="button"
              class="memory-module-tab"
              :class="{ 'is-active': activeModule === m.value }"
              :aria-current="activeModule === m.value ? 'page' : undefined"
              @click="selectModule(m.value)"
            >
              <span class="memory-module-name">{{ m.label }}</span>
              <span class="memory-module-desc">{{ m.desc }}</span>
              <span
                class="memory-module-count"
                :title="m.value === 'memory' && summaryCount > 0 ? t('memory.modules.summaryTitle', { n: summaryCount }) : undefined"
                :aria-label="t('memory.modules.countLabel', { module: m.label, count: moduleCounts[m.value] ?? 0 })"
              >
                {{ moduleCounts[m.value] ?? 0 }}
              </span>
            </button>
          </div>

          <!-- ===== Memory 记忆流模块（P0-1 列表，零回退） ===== -->
          <template v-if="activeModule === 'memory'">
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

            <!-- 首次加载骨架屏 -->
            <div v-if="loading && items.length === 0" class="memory-card-list">
              <div v-for="n in 5" :key="'skel-' + n" class="memory-skeleton-card">
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
                <MemoryFactCard
                  v-for="fact in items"
                  :key="fact.id"
                  :fact="fact"
                  @edit="openEdit"
                  @delete="confirmDelete"
                />
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
          </template>

          <!-- ===== Soul 灵魂模块 ===== -->
          <template v-else-if="activeModule === 'soul'">
            <!-- 首次加载骨架屏 -->
            <div v-if="moduleLoading && !soulData" class="memory-card-list">
              <div v-for="n in 3" :key="'soul-skel-' + n" class="memory-skeleton-card">
                <t-skeleton
                  animation="gradient"
                  :row-col="[
                    { width: '24%', height: '20px' },
                    { width: '88%', height: '16px' },
                    { width: '40%', height: '14px' },
                  ]"
                />
              </div>
            </div>

            <!-- 加载失败 -->
            <div v-else-if="moduleError" class="memory-state-block">
              <t-icon name="error-circle" size="40px" class="memory-state-icon" />
              <div class="memory-state-title">{{ t('memory.error.title') }}</div>
              <div class="memory-state-desc">{{ t('memory.error.description') }}</div>
              <t-button variant="outline" size="small" @click="fetchModuleCard">
                {{ t('memory.error.retry') }}
              </t-button>
            </div>

            <!-- 整体空态：无人设且无微调 -->
            <div v-else-if="soulData && !hasPersona && soulData.adjustments.length === 0" class="memory-state-block memory-empty-state">
              <div class="memory-empty-icon">
                <t-icon name="heart" size="26px" />
              </div>
              <div class="memory-state-title">{{ t('memory.soul.emptyTitle') }}</div>
              <div class="memory-state-desc">{{ t('memory.soul.emptyDescription') }}</div>
              <t-button theme="primary" size="small" @click="goToNewChat">
                {{ t('memory.soul.cta') }}
              </t-button>
            </div>

            <template v-else-if="soulData">
              <!-- 全局人设卡（只读，模板缺失时优雅降级隐藏） -->
              <div v-if="hasPersona" class="memory-persona-card">
                <div class="memory-persona-head">
                  <div class="memory-persona-icon">
                    <t-icon name="heart" size="18px" />
                  </div>
                  <div class="memory-persona-title">
                    <div class="memory-persona-name">{{ soulData.global_persona.name }}</div>
                    <div v-if="soulData.global_persona.description" class="memory-persona-desc">
                      {{ soulData.global_persona.description }}
                    </div>
                  </div>
                  <span class="memory-persona-badge">
                    <t-icon name="lock-on" size="12px" />
                    {{ t('memory.soul.personaBadge') }}
                  </span>
                </div>
                <div
                  v-if="soulData.global_persona.content"
                  class="memory-persona-content"
                  :class="{ 'is-expanded': personaExpanded }"
                >
                  {{ soulData.global_persona.content }}
                </div>
                <button
                  v-if="personaContentLong"
                  type="button"
                  class="memory-persona-toggle"
                  @click="personaExpanded = !personaExpanded"
                >
                  {{ personaExpanded ? t('memory.soul.collapse') : t('memory.soul.expand') }}
                  <t-icon :name="personaExpanded ? 'chevron-up' : 'chevron-down'" size="14px" />
                </button>
              </div>

              <!-- 我的风格指令（微调） -->
              <div class="memory-section-head">
                <h3>{{ t('memory.soul.adjustmentsTitle') }}</h3>
                <span class="memory-section-count">{{ soulData.adjustments.length }}</span>
              </div>
              <div class="memory-section-hint">{{ t('memory.soul.adjustmentsHint') }}</div>

              <div v-if="soulData.adjustments.length === 0" class="memory-guide-block">
                <t-icon name="chat" size="20px" class="memory-guide-icon" />
                <div class="memory-guide-text">{{ t('memory.soul.emptyDescription') }}</div>
                <t-button theme="primary" size="small" @click="goToNewChat">
                  {{ t('memory.soul.cta') }}
                </t-button>
              </div>
              <div v-else class="memory-card-list">
                <MemoryFactCard
                  v-for="fact in soulData.adjustments"
                  :key="fact.id"
                  :fact="fact"
                  @edit="openEdit"
                  @delete="confirmDelete"
                />
              </div>
            </template>
          </template>

          <!-- ===== User 用户档案模块 ===== -->
          <template v-else-if="activeModule === 'user'">
            <!-- 首次加载骨架屏 -->
            <div v-if="moduleLoading && !profileData" class="memory-profile-grid">
              <div v-for="n in 4" :key="'prof-skel-' + n" class="memory-skeleton-card">
                <t-skeleton
                  animation="gradient"
                  :row-col="[
                    { width: '40%', height: '18px' },
                    { width: '92%', height: '14px' },
                    { width: '72%', height: '14px' },
                  ]"
                />
              </div>
            </div>

            <!-- 加载失败 -->
            <div v-else-if="moduleError" class="memory-state-block">
              <t-icon name="error-circle" size="40px" class="memory-state-icon" />
              <div class="memory-state-title">{{ t('memory.error.title') }}</div>
              <div class="memory-state-desc">{{ t('memory.error.description') }}</div>
              <t-button variant="outline" size="small" @click="fetchModuleCard">
                {{ t('memory.error.retry') }}
              </t-button>
            </div>

            <!-- 档案全空 -->
            <div v-else-if="profileData && profileTotalCount === 0" class="memory-state-block memory-empty-state">
              <div class="memory-empty-icon">
                <t-icon name="user-circle" size="26px" />
              </div>
              <div class="memory-state-title">{{ t('memory.profile.emptyTitle') }}</div>
              <div class="memory-state-desc">{{ t('memory.profile.emptyDescription') }}</div>
              <t-button theme="primary" size="small" @click="goToNewChat">
                {{ t('memory.profile.cta') }}
              </t-button>
            </div>

            <template v-else-if="profileData">
              <!-- 完整度 -->
              <div class="memory-profile-summary">
                <div class="memory-profile-summary-row">
                  <span class="memory-profile-summary-label">{{ t('memory.profile.completeness') }}</span>
                  <span class="memory-profile-percent">{{ Math.round(profileData.completeness * 100) }}%</span>
                </div>
                <t-progress
                  :percentage="Math.round(profileData.completeness * 100)"
                  :show-label="false"
                  class="memory-profile-progress"
                />
                <div class="memory-profile-hint">{{ t('memory.profile.completenessHint') }}</div>
              </div>

              <!-- 四个分组卡 -->
              <div class="memory-profile-grid">
                <div v-for="section in profileSections" :key="section.key" class="memory-profile-section">
                  <div class="memory-profile-section-head">
                    <span class="memory-profile-section-title">{{ t(`memory.profile.sections.${section.key}`) }}</span>
                    <span class="memory-section-count">{{ section.items.length }}</span>
                    <button
                      type="button"
                      class="memory-profile-drill"
                      :disabled="section.items.length === 0"
                      @click="drillToCategory(section.key)"
                    >
                      {{ t('memory.profile.viewInMemory') }}
                      <t-icon name="chevron-right" size="12px" />
                    </button>
                  </div>
                  <div v-if="section.items.length === 0" class="memory-profile-empty">
                    {{ t('memory.profile.emptySection') }}
                  </div>
                  <div v-else class="memory-profile-items">
                    <div
                      v-for="fact in section.items"
                      :key="fact.id"
                      class="memory-profile-item"
                      role="button"
                      tabindex="0"
                      @click="openEdit(fact)"
                      @keydown.enter="openEdit(fact)"
                    >
                      <span class="memory-profile-item-content">{{ fact.content }}</span>
                      <span class="memory-profile-item-conf">{{ Math.round((fact.confidence ?? 0) * 100) }}%</span>
                    </div>
                  </div>
                </div>
              </div>
            </template>
          </template>

          <!-- ===== Agent 经验技巧模块 ===== -->
          <template v-else-if="activeModule === 'agent'">
            <!-- 首次加载骨架屏 -->
            <div v-if="moduleLoading && !agentData" class="memory-card-list">
              <div v-for="n in 3" :key="'agent-skel-' + n" class="memory-skeleton-card">
                <t-skeleton
                  animation="gradient"
                  :row-col="[
                    { width: '20%', height: '20px' },
                    { width: '90%', height: '16px' },
                    { width: '45%', height: '14px' },
                  ]"
                />
              </div>
            </div>

            <!-- 加载失败 -->
            <div v-else-if="moduleError" class="memory-state-block">
              <t-icon name="error-circle" size="40px" class="memory-state-icon" />
              <div class="memory-state-title">{{ t('memory.error.title') }}</div>
              <div class="memory-state-desc">{{ t('memory.error.description') }}</div>
              <t-button variant="outline" size="small" @click="fetchModuleCard">
                {{ t('memory.error.retry') }}
              </t-button>
            </div>

            <!-- 整体空态：无技巧且无反馈 -->
            <div v-else-if="agentData && agentData.skills.length === 0 && agentData.feedback_total === 0" class="memory-state-block memory-empty-state">
              <div class="memory-empty-icon">
                <t-icon name="lightbulb" size="26px" />
              </div>
              <div class="memory-state-title">{{ t('memory.agent.emptyTitle') }}</div>
              <div class="memory-state-desc">{{ t('memory.agent.emptyDescription') }}</div>
              <t-button theme="primary" size="small" @click="goToNewChat">
                {{ t('memory.empty.cta') }}
              </t-button>
            </div>

            <template v-else-if="agentData">
              <!-- 技巧列表 -->
              <div class="memory-section-head">
                <h3>{{ t('memory.agent.skillsTitle') }}</h3>
                <span class="memory-section-count">{{ agentData.skills.length }}</span>
              </div>
              <div class="memory-section-hint">{{ t('memory.agent.skillsHint') }}</div>

              <div v-if="agentData.skills.length === 0" class="memory-guide-block">
                <t-icon name="lightbulb" size="20px" class="memory-guide-icon" />
                <div class="memory-guide-text">{{ t('memory.agent.skillsEmptyDescription') }}</div>
                <t-button theme="primary" size="small" @click="goToNewChat">
                  {{ t('memory.agent.distillCta') }}
                </t-button>
              </div>
              <div v-else class="memory-card-list">
                <MemoryFactCard
                  v-for="skill in agentData.skills"
                  :key="skill.id"
                  :fact="skill"
                  :dom-id="`skill-card-${skill.id}`"
                  :highlighted="highlightedSkillId === skill.id"
                  @edit="openEdit"
                  @delete="confirmDelete"
                >
                  <template v-if="sourceFeedbackOf(skill)" #extra>
                    <div class="memory-skill-source">
                      <t-icon name="chat" size="13px" />
                      <span class="memory-skill-source-text">
                        {{ t('memory.agent.fromFeedback', { content: sourceFeedbackOf(skill)!.content }) }}
                      </span>
                    </div>
                  </template>
                </MemoryFactCard>
              </div>

              <!-- 反馈墙 -->
              <div class="memory-section-head">
                <h3>{{ t('memory.agent.feedbackTitle') }}</h3>
                <span class="memory-section-count">{{ agentData.feedback_total }}</span>
              </div>
              <div class="memory-section-hint">{{ t('memory.agent.feedbackHint') }}</div>

              <div v-if="agentData.feedback.length === 0" class="memory-guide-block memory-guide-block--flat">
                <div class="memory-guide-text">{{ t('memory.agent.feedbackEmpty') }}</div>
              </div>
              <template v-else>
                <div class="memory-feedback-list">
                  <div v-for="fb in agentData.feedback" :key="fb.id" class="memory-feedback-item">
                    <div class="memory-feedback-main">
                      <span class="memory-feedback-content">{{ fb.content }}</span>
                      <button
                        v-if="fb.upgraded_to"
                        type="button"
                        class="memory-feedback-chip is-upgraded"
                        @click="locateSkill(fb.upgraded_to)"
                      >
                        <t-icon name="check-circle" size="12px" />
                        {{ t('memory.agent.upgraded') }}
                      </button>
                      <span v-else class="memory-feedback-chip">
                        <t-icon name="time" size="12px" />
                        {{ t('memory.agent.pending') }}
                      </span>
                    </div>
                    <div class="memory-feedback-foot">
                      <span class="memory-feedback-time">{{ relativeTime(fb.updated_at) }}</span>
                      <span class="memory-feedback-actions">
                        <t-tooltip v-if="!fb.upgraded_to" :content="t('memory.agent.distillCta')" placement="top">
                          <t-button variant="text" shape="square" size="small" @click.stop="goToNewChat">
                            <template #icon>
                              <t-icon name="lightbulb" size="14px" />
                            </template>
                          </t-button>
                        </t-tooltip>
                        <t-tooltip :content="t('memory.edit.title')" placement="top">
                          <t-button variant="text" shape="square" size="small" @click.stop="openEdit(fb)">
                            <template #icon>
                              <t-icon name="edit" size="14px" />
                            </template>
                          </t-button>
                        </t-tooltip>
                        <t-popconfirm
                          theme="danger"
                          :content="t('memory.delete.confirm')"
                          :confirm-btn="{ content: t('memory.delete.confirmButton'), theme: 'danger' }"
                          :cancel-btn="{ content: t('common.cancel') }"
                          @confirm="confirmDelete(fb)"
                        >
                          <t-button variant="text" shape="square" size="small" @click.stop>
                            <template #icon>
                              <t-icon name="delete" size="14px" />
                            </template>
                          </t-button>
                        </t-popconfirm>
                      </span>
                    </div>
                  </div>
                </div>

                <div v-if="agentData.feedback_total > feedbackPageSize" class="memory-pagination">
                  <t-pagination
                    :current="feedbackPage"
                    :total="agentData.feedback_total"
                    :page-size="feedbackPageSize"
                    @current-change="handleFeedbackPageChange"
                  />
                </div>
              </template>
            </template>
          </template>
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
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { MessagePlugin } from 'tdesign-vue-next'
import MemoryFactCard from '@/components/memory/MemoryFactCard.vue'
import {
  deleteAllMemories,
  deleteMemoryFact,
  getAgentTips,
  getMemoryModules,
  getMemoryStatus,
  getProfileCard,
  getSoulCard,
  listMemoryFacts,
  updateMemoryFact,
  type AgentFeedbackItem,
  type AgentTipsCardData,
  type MemoryCategory,
  type MemoryFact,
  type MemoryModule,
  type MemoryProfileSectionKey,
  type MemoryStatus,
  type ProfileCardData,
  type SoulCardData,
} from '@/api/memory'
import { updateMyPreferences } from '@/api/auth'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()

const MEMORY_CATEGORIES: MemoryCategory[] = ['profile', 'fact', 'preference', 'todo', 'feedback', 'soul', 'skill']
const MODULE_KEYS: MemoryModule[] = ['soul', 'user', 'memory', 'agent']
const DEFAULT_MODULE: MemoryModule = 'memory'

const pageSize = 20
const feedbackPageSize = 10

// ---- 状态 ----
const memoryEnabled = ref(true)
const statusLoading = ref(true)
const switchLoading = ref(false)
const loading = ref(false)
const loadError = ref(false)
const notAvailable = ref(false)

// 一级四模块导航（路由参数 ?module=soul|user|memory|agent）
const activeModule = ref<MemoryModule>(DEFAULT_MODULE)
const moduleCounts = ref<Record<string, number>>({})
const summaryCount = ref(0)

// Memory 模块列表（P0-1）
const items = ref<MemoryFact[]>([])
const total = ref(0)
const factCount = ref(0)
const page = ref(1)
const category = ref<'' | MemoryCategory>('')
const statusFilter = ref<'active' | 'done' | 'archived' | 'all'>('active')
const searchKeyword = ref('')
const counts = ref<Record<string, number>>({})

// Soul / User / Agent 聚合卡数据
const soulData = ref<SoulCardData | null>(null)
const profileData = ref<ProfileCardData | null>(null)
const agentData = ref<AgentTipsCardData | null>(null)
const feedbackPage = ref(1)
const moduleLoading = ref(false)
const moduleError = ref(false)
const personaExpanded = ref(false)
const highlightedSkillId = ref('')
let highlightTimer: ReturnType<typeof setTimeout> | null = null

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
const moduleTabs = computed(() => [
  { value: 'soul' as const, label: t('memory.modules.soul'), desc: t('memory.modules.soulDesc') },
  { value: 'user' as const, label: t('memory.modules.user'), desc: t('memory.modules.userDesc') },
  { value: 'memory' as const, label: t('memory.modules.memory'), desc: t('memory.modules.memoryDesc') },
  { value: 'agent' as const, label: t('memory.modules.agent'), desc: t('memory.modules.agentDesc') },
])

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

const hasPersona = computed(() => {
  const persona = soulData.value?.global_persona
  return !!(persona && (persona.name || persona.content || persona.description))
})

const personaContentLong = computed(() => {
  const content = soulData.value?.global_persona?.content ?? ''
  return content.length > 160 || content.split('\n').length > 4
})

// User 档案分组：identity/role 内按 importance DESC, updated_at DESC 置顶排序
const profileSections = computed(() => {
  if (!profileData.value) return []
  return (profileData.value.sections ?? []).map(section => {
    const itemsCopy = [...(section.items ?? [])]
    if (section.key === 'identity' || section.key === 'role') {
      itemsCopy.sort((a, b) => {
        const byImportance = (b.importance ?? 0) - (a.importance ?? 0)
        if (byImportance !== 0) return byImportance
        return new Date(b.updated_at).getTime() - new Date(a.updated_at).getTime()
      })
    }
    return { key: section.key, items: itemsCopy }
  })
})

const profileTotalCount = computed(
  () => profileSections.value.reduce((sum, section) => sum + section.items.length, 0),
)

// ---- 数据加载 ----
// 请求序号：防止快速切换筛选/模块时旧响应覆盖新响应（竞态防护）。
let listRequestId = 0
let moduleRequestId = 0

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

// 四模块计数徽标（/memory/modules 一次聚合）
async function fetchModuleCounts() {
  try {
    const modules = await getMemoryModules()
    const next: Record<string, number> = {}
    for (const row of modules) {
      next[row.module] = row.fact_count
      if (row.module === 'memory' && row.summary_count !== undefined) {
        summaryCount.value = row.summary_count
      }
    }
    moduleCounts.value = next
  } catch {
    // 计数属增强信息，失败静默（导航退化为无计数展示）。
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

// 分类计数：并行轻量请求（全部 + 7 分类），按当前状态筛选。
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

// Soul / User / Agent 聚合卡加载（同一请求序号防跨模块竞态）
async function fetchModuleCard() {
  const requestId = ++moduleRequestId
  moduleLoading.value = true
  moduleError.value = false
  try {
    if (activeModule.value === 'soul') {
      const data = await getSoulCard()
      if (requestId !== moduleRequestId) return
      soulData.value = data
    } else if (activeModule.value === 'user') {
      const data = await getProfileCard()
      if (requestId !== moduleRequestId) return
      profileData.value = data
    } else {
      const data = await getAgentTips({ page: feedbackPage.value, page_size: feedbackPageSize })
      if (requestId !== moduleRequestId) return
      agentData.value = data
    }
  } catch (err: any) {
    if (requestId !== moduleRequestId) return
    if (err?.status === 404) {
      notAvailable.value = true
    } else {
      moduleError.value = true
    }
  } finally {
    if (requestId === moduleRequestId) {
      moduleLoading.value = false
    }
  }
}

// 加载当前激活模块的视图数据
function loadActiveModule() {
  if (notAvailable.value) return
  if (activeModule.value === 'memory') {
    fetchList()
    fetchCounts()
  } else {
    fetchModuleCard()
  }
}

function refreshAll() {
  fetchStatus()
  fetchModuleCounts()
  loadActiveModule()
}

// ---- 模块导航 ----
function normalizeModule(value: unknown): MemoryModule {
  return MODULE_KEYS.includes(value as MemoryModule) ? (value as MemoryModule) : DEFAULT_MODULE
}

function selectModule(m: MemoryModule) {
  if (m === activeModule.value) return
  // 一级模块切换走路由参数：支持直链分享与浏览器回退。
  router.push({ query: { ...route.query, module: m } }).catch(() => {})
}

watch(
  () => route.query.module,
  value => {
    const next = normalizeModule(value)
    if (next !== activeModule.value) {
      activeModule.value = next
      personaExpanded.value = false
      loadActiveModule()
    }
  },
)

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

function handleFeedbackPageChange(next: number) {
  feedbackPage.value = next
  fetchModuleCard()
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

// 增删后的统一刷新：当前模块视图 + 一级导航计数 + 头部状态
function refreshAfterMutation() {
  loadActiveModule()
  fetchModuleCounts()
  fetchStatus()
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
    refreshAfterMutation()
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
    // Memory 模块：当前页删空时回退一页，避免停留在空页。
    if (activeModule.value === 'memory' && items.value.length === 1 && page.value > 1) {
      page.value -= 1
    }
    if (activeModule.value === 'agent' && agentData.value) {
      // 删除后若当前反馈页删空且不是第一页，回退一页。
      const remaining = (agentData.value.feedback?.length ?? 0) - 1
      if (remaining === 0 && feedbackPage.value > 1) {
        feedbackPage.value -= 1
        fetchModuleCard()
        fetchModuleCounts()
        fetchStatus()
        return
      }
    }
    refreshAfterMutation()
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
    feedbackPage.value = 1
    items.value = []
    total.value = 0
    soulData.value = null
    profileData.value = null
    agentData.value = null
    refreshAll()
  } catch {
    MessagePlugin.error(t('memory.clear.failed'))
  } finally {
    clearing.value = false
  }
}

// ---- User 档案下钻：跳转 Memory 模块并携带分类筛选 ----
const PROFILE_SECTION_CATEGORY: Record<string, MemoryCategory> = {
  identity: 'profile',
  role: 'fact',
  preference: 'preference',
  fact: 'fact',
}

function drillToCategory(key: MemoryProfileSectionKey) {
  const target = PROFILE_SECTION_CATEGORY[key] ?? ''
  if (activeModule.value === 'memory') {
    selectCategory(target)
    return
  }
  category.value = target
  page.value = 1
  selectModule('memory')
}

// ---- Agent：反馈升级关联 ----
// 技巧来源反馈：从已加载的反馈墙中反查 upgraded_to 指向该技巧的条目。
function sourceFeedbackOf(skill: MemoryFact): AgentFeedbackItem | undefined {
  return agentData.value?.feedback?.find(fb => fb.upgraded_to === skill.id)
}

// 定位技巧卡：滚动到对应卡片并短暂高亮
function locateSkill(skillId: string) {
  highlightedSkillId.value = skillId
  nextTick(() => {
    document.getElementById(`skill-card-${skillId}`)?.scrollIntoView({ behavior: 'smooth', block: 'center' })
  })
  if (highlightTimer) clearTimeout(highlightTimer)
  highlightTimer = setTimeout(() => {
    highlightedSkillId.value = ''
  }, 2400)
}

// ---- 展示工具 ----
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
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

function goToNewChat() {
  router.push('/platform/creatChat')
}

onMounted(() => {
  // 直链支持：/platform/memory?module=user 直接打开档案视图
  activeModule.value = normalizeModule(route.query.module)
  refreshAll()
})

onBeforeUnmount(() => {
  if (searchTimer) clearTimeout(searchTimer)
  if (highlightTimer) clearTimeout(highlightTimer)
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

// ---- 一级模块导航 ----
.memory-module-nav {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: 10px;
  margin-bottom: 16px;
}

.memory-module-tab {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 3px;
  position: relative;
  padding: 12px 14px;
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
  background: var(--td-bg-color-container);
  box-shadow: 0 1px 3px rgba(0, 0, 0, 0.04);
  cursor: pointer;
  text-align: left;
  transition: all 0.2s ease;

  &:hover {
    border-color: var(--td-brand-color);
  }

  &.is-active {
    border-color: var(--td-brand-color);
    background: var(--td-brand-color-light);
    box-shadow: 0 2px 8px rgba(7, 192, 95, 0.12);

    .memory-module-name {
      color: var(--td-brand-color);
    }

    .memory-module-count {
      background: var(--td-brand-color);
      color: #fff;
    }
  }
}

.memory-module-name {
  color: var(--td-text-color-primary);
  font-size: 14px;
  font-weight: 600;
  line-height: 20px;
}

.memory-module-desc {
  color: var(--td-text-color-placeholder);
  font-size: 12px;
  line-height: 16px;
  padding-right: 36px;
}

.memory-module-count {
  position: absolute;
  top: 10px;
  right: 12px;
  min-width: 20px;
  padding: 0 7px;
  border-radius: 999px;
  background: var(--td-bg-color-secondarycontainer);
  color: var(--td-text-color-secondary);
  font-size: 12px;
  line-height: 18px;
  font-variant-numeric: tabular-nums;
  text-align: center;
}

// ---- 二级分类 Tabs（Memory 模块） ----
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

// 骨架屏卡（卡片实体样式在 MemoryFactCard 组件内）
.memory-skeleton-card {
  padding: 12px 16px;
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
  background: var(--td-bg-color-container);
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

// ---- 分组标题（Soul 微调 / Agent 技巧与反馈墙共用） ----
.memory-section-head {
  display: flex;
  align-items: center;
  gap: 8px;
  margin: 20px 0 4px;

  h3 {
    margin: 0;
    color: var(--td-text-color-primary);
    font-size: 15px;
    font-weight: 600;
    line-height: 22px;
  }
}

.memory-section-count {
  padding: 0 8px;
  border-radius: 999px;
  background: var(--td-bg-color-secondarycontainer);
  color: var(--td-text-color-secondary);
  font-size: 12px;
  line-height: 18px;
  font-variant-numeric: tabular-nums;
}

.memory-section-hint {
  color: var(--td-text-color-placeholder);
  font-size: 12px;
  line-height: 18px;
  margin-bottom: 10px;
}

// 引导块（列表为空但模块仍有其他内容时）
.memory-guide-block {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 10px;
  padding: 28px 20px;
  border: 1px dashed var(--td-component-stroke);
  border-radius: 8px;
  text-align: center;

  &.memory-guide-block--flat {
    padding: 20px;
    border-style: solid;
  }
}

.memory-guide-icon {
  color: var(--td-brand-color);
}

.memory-guide-text {
  color: var(--td-text-color-placeholder);
  font-size: 13px;
  line-height: 20px;
  max-width: 400px;
}

// ---- Soul 全局人设卡 ----
.memory-persona-card {
  padding: 16px;
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
  background: linear-gradient(135deg, var(--td-brand-color-light) 0%, var(--td-bg-color-container) 55%);
  box-shadow: 0 1px 3px rgba(0, 0, 0, 0.04);
}

.memory-persona-head {
  display: flex;
  align-items: flex-start;
  gap: 12px;
}

.memory-persona-icon {
  flex: none;
  width: 36px;
  height: 36px;
  border-radius: 50%;
  display: flex;
  align-items: center;
  justify-content: center;
  background: var(--td-brand-color-light);
  color: var(--td-brand-color);
}

.memory-persona-title {
  flex: 1;
  min-width: 0;
}

.memory-persona-name {
  color: var(--td-text-color-primary);
  font-size: 15px;
  font-weight: 600;
  line-height: 22px;
}

.memory-persona-desc {
  margin-top: 2px;
  color: var(--td-text-color-secondary);
  font-size: 12px;
  line-height: 18px;
}

.memory-persona-badge {
  flex: none;
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 1px 8px;
  border-radius: 999px;
  border: 1px solid var(--td-component-stroke);
  background: var(--td-bg-color-container);
  color: var(--td-text-color-secondary);
  font-size: 12px;
  line-height: 18px;
  white-space: nowrap;
}

.memory-persona-content {
  margin-top: 10px;
  color: var(--td-text-color-secondary);
  font-size: 13px;
  line-height: 20px;
  white-space: pre-line;
  word-break: break-word;
  display: -webkit-box;
  -webkit-line-clamp: 4;
  -webkit-box-orient: vertical;
  overflow: hidden;

  &.is-expanded {
    display: block;
    -webkit-line-clamp: unset;
    overflow: visible;
  }
}

.memory-persona-toggle {
  display: inline-flex;
  align-items: center;
  gap: 2px;
  margin-top: 6px;
  border: none;
  background: transparent;
  padding: 2px 0;
  color: var(--td-brand-color);
  font-size: 12px;
  line-height: 18px;
  cursor: pointer;

  &:hover {
    text-decoration: underline;
  }
}

// ---- User 档案 ----
.memory-profile-summary {
  padding: 14px 16px;
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
  background: var(--td-bg-color-container);
  margin-bottom: 12px;
}

.memory-profile-summary-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 8px;
}

.memory-profile-summary-label {
  color: var(--td-text-color-primary);
  font-size: 13px;
  font-weight: 500;
  line-height: 20px;
}

.memory-profile-percent {
  color: var(--td-brand-color);
  font-size: 18px;
  font-weight: 600;
  line-height: 24px;
  font-variant-numeric: tabular-nums;
}

.memory-profile-progress {
  margin-bottom: 6px;
}

.memory-profile-hint {
  color: var(--td-text-color-placeholder);
  font-size: 12px;
  line-height: 18px;
}

.memory-profile-grid {
  display: grid;
  grid-template-columns: repeat(2, 1fr);
  gap: 12px;
}

.memory-profile-section {
  padding: 14px 16px;
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
  background: var(--td-bg-color-container);
  box-shadow: 0 1px 3px rgba(0, 0, 0, 0.04);
  display: flex;
  flex-direction: column;
  min-width: 0;
}

.memory-profile-section-head {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 10px;
}

.memory-profile-section-title {
  color: var(--td-text-color-primary);
  font-size: 14px;
  font-weight: 600;
  line-height: 20px;
}

.memory-profile-drill {
  display: inline-flex;
  align-items: center;
  gap: 1px;
  margin-left: auto;
  border: none;
  background: transparent;
  padding: 2px 0;
  color: var(--td-brand-color);
  font-size: 12px;
  line-height: 18px;
  cursor: pointer;
  white-space: nowrap;

  &:hover:not(:disabled) {
    text-decoration: underline;
  }

  &:disabled {
    color: var(--td-text-color-placeholder);
    cursor: default;
  }
}

.memory-profile-empty {
  color: var(--td-text-color-placeholder);
  font-size: 13px;
  line-height: 20px;
  padding: 8px 0;
}

.memory-profile-items {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.memory-profile-item {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  padding: 8px 10px;
  border-radius: 6px;
  background: var(--td-bg-color-secondarycontainer);
  cursor: pointer;
  transition: background 0.2s ease;

  &:hover,
  &:focus-visible {
    background: var(--td-brand-color-light);
  }
}

.memory-profile-item-content {
  flex: 1;
  min-width: 0;
  color: var(--td-text-color-primary);
  font-size: 13px;
  line-height: 20px;
  word-break: break-word;
}

.memory-profile-item-conf {
  flex: none;
  padding: 0 6px;
  border-radius: 999px;
  background: var(--td-bg-color-container);
  border: 1px solid var(--td-component-stroke);
  color: var(--td-text-color-secondary);
  font-size: 11px;
  line-height: 16px;
  font-variant-numeric: tabular-nums;
  margin-top: 2px;
}

// ---- Agent 技巧来源标签 ----
.memory-skill-source {
  display: flex;
  align-items: flex-start;
  gap: 5px;
  padding: 6px 10px;
  border-radius: 6px;
  background: var(--td-bg-color-secondarycontainer);
  color: var(--td-text-color-secondary);
  font-size: 12px;
  line-height: 18px;

  .t-icon {
    flex: none;
    margin-top: 3px;
    color: var(--td-brand-color);
  }
}

.memory-skill-source-text {
  min-width: 0;
  word-break: break-word;
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}

// ---- Agent 反馈墙 ----
.memory-feedback-list {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.memory-feedback-item {
  padding: 10px 14px;
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
  background: var(--td-bg-color-container);
  transition: border-color 0.2s ease;

  &:hover {
    border-color: var(--td-brand-color);

    .memory-feedback-actions {
      opacity: 1;
    }
  }
}

.memory-feedback-main {
  display: flex;
  align-items: flex-start;
  gap: 8px;
}

.memory-feedback-content {
  flex: 1;
  min-width: 0;
  color: var(--td-text-color-primary);
  font-size: 13px;
  line-height: 20px;
  word-break: break-word;
}

.memory-feedback-chip {
  flex: none;
  display: inline-flex;
  align-items: center;
  gap: 3px;
  padding: 0 8px;
  border-radius: 999px;
  border: 1px solid var(--td-component-stroke);
  background: var(--td-bg-color-secondarycontainer);
  color: var(--td-text-color-secondary);
  font-size: 12px;
  line-height: 18px;
  white-space: nowrap;

  &.is-upgraded {
    border-color: transparent;
    background: var(--td-success-color-1);
    color: var(--td-success-color-6);
    cursor: pointer;
  }
}

.memory-feedback-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-top: 6px;
}

.memory-feedback-time {
  color: var(--td-text-color-placeholder);
  font-size: 12px;
  line-height: 18px;
}

.memory-feedback-actions {
  display: inline-flex;
  align-items: center;
  gap: 2px;
  opacity: 0;
  transition: opacity 0.2s ease;
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

// ---- 响应式 ----
@media (max-width: 1080px) {
  .memory-module-nav {
    grid-template-columns: repeat(2, 1fr);
  }

  .memory-profile-grid {
    grid-template-columns: 1fr;
  }
}
</style>
