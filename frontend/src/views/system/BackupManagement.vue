<template>
  <!--
    BackupManagement — 备份与恢复分区（PRD docs/prd/data-backup-recovery.md §6.3）。

    三块能力：
      1. 状态卡：上次成功备份时间 + RPO 倒计时 + 手动触发按钮。
      2. 记录列表：状态筛选、统计、按空间导出、删除过期快照。
      3. 恢复向导：选快照 → 选范围 → 冲突策略 → dry-run 报告确认 →
         执行 → 阶段进度轮询（校验/恢复/索引重建）。

    后端整组 RequireSystemAdmin；子系统未启用时接口返回 503，页面
    展示专用提示而不是报错。术语遵循项目规范：空间。
  -->
  <div class="backup-management">
    <header class="section-header">
      <h2>{{ t('system.globalSettings.backup.tabLabel') }}</h2>
      <p class="section-description">{{ t('system.globalSettings.backup.description') }}</p>
    </header>

    <!-- 未启用 / 加载失败分支 -->
    <t-alert
      v-if="loadError === 'disabled'"
      class="backup-branch"
      theme="warning"
      :title="t('system.globalSettings.backup.disabledTitle')"
      :message="t('system.globalSettings.backup.disabledMessage')"
    />
    <t-alert
      v-else-if="loadError"
      class="backup-branch"
      theme="error"
      :message="loadError"
    >
      <template #operation>
        <t-button size="small" @click="reload">{{ t('system.globalSettings.backup.retry') }}</t-button>
      </template>
    </t-alert>

    <div v-else-if="loading" class="backup-branch">
      <t-loading :text="t('system.globalSettings.backup.loading')" />
    </div>

    <template v-else>
      <!-- 状态卡 -->
      <div class="backup-status-card">
        <div class="backup-status-item">
          <span class="backup-status-label">{{ t('system.globalSettings.backup.lastSuccess') }}</span>
          <span class="backup-status-value">{{ lastSuccessText }}</span>
        </div>
        <div class="backup-status-item">
          <span class="backup-status-label">{{ t('system.globalSettings.backup.rpoCountdown') }}</span>
          <span class="backup-status-value" :class="{ 'backup-status-value--warn': rpoOverdue }">
            {{ rpoCountdownText }}
          </span>
        </div>
        <div class="backup-status-item">
          <span class="backup-status-label">{{ t('system.globalSettings.backup.retentionPolicy') }}</span>
          <span class="backup-status-value">{{ retentionText }}</span>
        </div>
        <t-button
          class="backup-run-button"
          theme="primary"
          size="small"
          :loading="running"
          :disabled="hasRunningRecord"
          @click="runBackupNow"
        >
          {{ t('system.globalSettings.backup.runNow') }}
        </t-button>
      </div>

      <!-- 记录列表 -->
      <div class="backup-records-header">
        <h3>{{ t('system.globalSettings.backup.records.title') }}</h3>
        <t-select
          v-model="statusFilter"
          size="small"
          class="backup-status-filter"
          :placeholder="t('system.globalSettings.backup.records.filterAll')"
          clearable
          @change="reload"
        >
          <t-option value="running">{{ t('system.globalSettings.backup.status.running') }}</t-option>
          <t-option value="succeeded">{{ t('system.globalSettings.backup.status.succeeded') }}</t-option>
          <t-option value="failed">{{ t('system.globalSettings.backup.status.failed') }}</t-option>
        </t-select>
      </div>

      <div v-if="records.length === 0" class="backup-branch">
        <t-empty :description="t('system.globalSettings.backup.records.empty')" />
      </div>

      <div v-else class="data-table-shell">
        <t-table row-key="id" :data="records" :columns="recordColumns" size="medium" hover>
          <template #id="{ row }">
            <div class="backup-record-id">
              <span class="backup-record-id-text">{{ row.id }}</span>
              <t-tag v-if="row.retention_tag" size="small" variant="light">
                {{ t(`system.globalSettings.backup.retention.${row.retention_tag}`) }}
              </t-tag>
            </div>
          </template>
          <template #status="{ row }">
            <t-tag :theme="statusTheme(row.status)" size="small" variant="light">
              {{ t(`system.globalSettings.backup.status.${row.status}`) }}
            </t-tag>
          </template>
          <template #trigger="{ row }">
            {{ t(`system.globalSettings.backup.trigger.${row.trigger_type}`) }}
          </template>
          <template #started_at="{ row }">
            <div class="backup-time">
              <span>{{ formatDate(row.started_at) }}</span>
              <span class="backup-time-clock">{{ formatTime(row.started_at) }}</span>
            </div>
          </template>
          <template #stats="{ row }">
            <div class="backup-stats">
              <span>{{ t('system.globalSettings.backup.stats.workspaces', { n: row.stats?.workspaces ?? 0 }) }}</span>
              <span>{{ t('system.globalSettings.backup.stats.files', { n: row.stats?.files ?? 0 }) }}</span>
              <span>{{ formatBytes(row.stats?.bytes ?? 0) }}</span>
              <span v-if="row.stats?.duration_ms">{{ formatDuration(row.stats.duration_ms) }}</span>
            </div>
          </template>
          <template #error="{ row }">
            <span class="backup-error-text" :title="row.error">{{ row.error || '—' }}</span>
          </template>
          <template #op="{ row }">
            <div class="backup-row-actions">
              <t-button
                size="small"
                variant="text"
                theme="primary"
                :disabled="row.status !== 'succeeded'"
                @click="openRestore(row)"
              >
                {{ t('system.globalSettings.backup.restore.action') }}
              </t-button>
              <t-popconfirm
                :content="t('system.globalSettings.backup.deleteConfirm')"
                @confirm="deleteRecord(row)"
              >
                <t-button size="small" variant="text" theme="danger">{{ t('common.delete') }}</t-button>
              </t-popconfirm>
            </div>
          </template>
        </t-table>
      </div>

      <!-- 按空间导出 -->
      <div class="backup-export-panel">
        <h3>{{ t('system.globalSettings.backup.export.title') }}</h3>
        <p class="backup-export-hint">{{ t('system.globalSettings.backup.export.hint') }}</p>
        <div class="backup-export-row">
          <t-input-number
            v-model="exportTenantId"
            :min="1"
            :step="1"
            theme="normal"
            :placeholder="t('system.globalSettings.backup.export.tenantId')"
            style="width: 180px"
          />
          <t-button
            theme="default"
            size="medium"
            :loading="exporting"
            :disabled="!exportTenantId"
            @click="doExportTenant"
          >
            {{ t('system.globalSettings.backup.export.download') }}
          </t-button>
        </div>
      </div>
    </template>

    <!-- 恢复向导 -->
    <SettingDrawer
      v-model:visible="restoreVisible"
      class="backup-restore-drawer"
      :title="t('system.globalSettings.backup.restore.title')"
      :description="t('system.globalSettings.backup.restore.description')"
      icon="restore"
      width="640px"
      :min-width="480"
      :max-width="900"
      storage-key="setting-drawer:width:system-backup-restore"
      hide-footer
    >
      <div class="backup-restore-body">
        <!-- 步骤 1：范围 -->
        <section class="backup-restore-section">
          <h4>{{ t('system.globalSettings.backup.restore.stepScope') }}</h4>
          <t-radio-group v-model="restoreForm.scope" variant="default radio">
            <t-radio value="tenant">{{ t('system.globalSettings.backup.restore.scopeTenant') }}</t-radio>
            <t-radio value="instance">{{ t('system.globalSettings.backup.restore.scopeInstance') }}</t-radio>
          </t-radio-group>
        </section>

        <!-- 步骤 2：空间与冲突策略 -->
        <section v-if="restoreForm.scope === 'tenant'" class="backup-restore-section">
          <h4>{{ t('system.globalSettings.backup.restore.stepTenant') }}</h4>
          <t-input-number
            v-model="restoreForm.tenantId"
            :min="1"
            :step="1"
            :placeholder="t('system.globalSettings.backup.export.tenantId')"
            style="width: 200px"
          />
          <t-radio-group
            v-model="restoreForm.conflictMode"
            variant="default radio"
            class="backup-conflict-group"
          >
            <t-radio value="new">{{ t('system.globalSettings.backup.restore.modeNew') }}</t-radio>
            <t-radio value="overwrite">{{ t('system.globalSettings.backup.restore.modeOverwrite') }}</t-radio>
          </t-radio-group>
          <p class="backup-restore-note">{{ t('system.globalSettings.backup.restore.modeNote') }}</p>
        </section>

        <!-- dry-run 报告 -->
        <section v-if="dryRunReport" class="backup-restore-section">
          <h4>{{ t('system.globalSettings.backup.restore.dryRunTitle') }}</h4>
          <div class="backup-dryrun-grid">
            <div class="backup-dryrun-cell">
              <span class="backup-status-label">{{ t('system.globalSettings.backup.restore.wouldRows') }}</span>
              <span class="backup-status-value">{{ dryRunReport.would_restore_rows ?? 0 }}</span>
            </div>
            <div class="backup-dryrun-cell">
              <span class="backup-status-label">{{ t('system.globalSettings.backup.restore.wouldFiles') }}</span>
              <span class="backup-status-value">{{ dryRunReport.would_restore_files ?? 0 }}</span>
            </div>
          </div>
        </section>

        <!-- 执行进度 -->
        <section v-if="activeJob" class="backup-restore-section">
          <h4>{{ t('system.globalSettings.backup.restore.progressTitle') }}</h4>
          <div class="backup-progress">
            <t-tag :theme="statusTheme(activeJob.status)" size="small" variant="light">
              {{ t(`system.globalSettings.backup.jobStatus.${activeJob.status}`) }}
            </t-tag>
            <span v-if="activeJob.progress?.message" class="backup-progress-message">
              {{ activeJob.progress.message }}
            </span>
          </div>
          <div class="backup-progress-counters">
            <span>{{ t('system.globalSettings.backup.restore.rowsRestored', { n: activeJob.progress?.rows_restored ?? 0 }) }}</span>
            <span>{{ t('system.globalSettings.backup.restore.filesRestored', { n: activeJob.progress?.files_restored ?? 0 }) }}</span>
          </div>
          <div v-if="activeJob.report" class="backup-report-summary">
            <span>{{ t('system.globalSettings.backup.restore.rowsSkipped', { n: activeJob.report.rows_skipped ?? 0 }) }}</span>
            <span>{{ t('system.globalSettings.backup.restore.reindexQueued', { n: activeJob.report.reindex_queued ?? 0 }) }}</span>
            <span v-if="activeJob.report.new_tenant_id">
              {{ t('system.globalSettings.backup.restore.newTenant', { id: activeJob.report.new_tenant_id }) }}
            </span>
          </div>
        </section>

        <t-alert
          v-if="restoreError"
          class="backup-branch"
          theme="error"
          :message="restoreError"
        />

        <!-- 操作区 -->
        <div class="backup-restore-actions">
          <t-button
            variant="outline"
            :loading="restoreBusy === 'dry'"
            :disabled="!!restoreBusy || jobFinished"
            @click="doDryRun"
          >
            {{ t('system.globalSettings.backup.restore.dryRun') }}
          </t-button>
          <t-popconfirm
            :content="restoreConfirmText"
            :disabled="!!dryRunReport === false && restoreForm.conflictMode === 'overwrite'"
            @confirm="doRestore"
          >
            <t-button
              theme="primary"
              :loading="restoreBusy === 'run'"
              :disabled="!!restoreBusy || jobFinished"
            >
              {{ t('system.globalSettings.backup.restore.execute') }}
            </t-button>
          </t-popconfirm>
        </div>
      </div>
    </SettingDrawer>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { MessagePlugin } from 'tdesign-vue-next'
import SettingDrawer from '@/components/settings/SettingDrawer.vue'
import {
  deleteBackup,
  exportTenantArchive,
  getBackupConfig,
  getRestoreJob,
  listBackupRecords,
  runBackup,
  startRestore,
  type BackupConfigInfo,
  type BackupRecord,
  type BackupRestoreJob,
  type RestoreReport,
} from '@/api/system/backup'

const { t } = useI18n()

const loading = ref(false)
const loadError = ref('')
const records = ref<BackupRecord[]>([])
const latestSucceeded = ref<BackupRecord | null>(null)
const statusFilter = ref('')
const configInfo = ref<BackupConfigInfo | null>(null)

const running = ref(false)
const exporting = ref(false)
const exportTenantId = ref<number | null>(null)

// 恢复向导状态
const restoreVisible = ref(false)
const restoreForm = ref({
  backupId: '',
  scope: 'tenant' as 'tenant' | 'instance',
  tenantId: null as number | null,
  conflictMode: 'new' as 'new' | 'overwrite',
})
const dryRunReport = ref<RestoreReport | null>(null)
const activeJob = ref<BackupRestoreJob | null>(null)
const restoreError = ref('')
const restoreBusy = ref<'' | 'dry' | 'run'>('')

let jobPollTimer: ReturnType<typeof setInterval> | null = null

const hasRunningRecord = computed(() => records.value.some((r) => r.status === 'running'))
const jobFinished = computed(() => {
  const status = activeJob.value?.status
  return status === 'succeeded' || status === 'failed' || status === 'dry-run'
})

const lastSuccessText = computed(() => {
  if (!latestSucceeded.value) return t('system.globalSettings.backup.never')
  const at = latestSucceeded.value.finished_at ?? latestSucceeded.value.started_at
  return `${formatDate(at)} ${formatTime(at)}`
})

// RPO 倒计时：距下次计划窗口（次日 03:00）的剩余时间；超过则标警示。
const rpoOverdue = computed(() => {
  if (!latestSucceeded.value) return true
  const at = new Date(latestSucceeded.value.finished_at ?? latestSucceeded.value.started_at)
  return Date.now() - at.getTime() > 26 * 3600 * 1000
})

const rpoCountdownText = computed(() => {
  const next = new Date()
  next.setHours(3, 0, 0, 0)
  if (next.getTime() <= Date.now()) next.setDate(next.getDate() + 1)
  const hours = Math.floor((next.getTime() - Date.now()) / 3600000)
  return t('system.globalSettings.backup.rpoIn', { hours })
})

const retentionText = computed(() =>
  t('system.globalSettings.backup.retentionPolicyValue', {
    d: configInfo.value?.retention_daily ?? 7,
    w: configInfo.value?.retention_weekly ?? 4,
    m: configInfo.value?.retention_monthly ?? 6,
  }),
)

const restoreConfirmText = computed(() =>
  restoreForm.value.conflictMode === 'overwrite'
    ? t('system.globalSettings.backup.restore.overwriteConfirm')
    : t('system.globalSettings.backup.restore.executeConfirm'),
)

const recordColumns = computed(() => [
  { colKey: 'id', title: t('system.globalSettings.backup.records.snapshot'), width: 210 },
  { colKey: 'status', title: t('system.globalSettings.backup.records.statusCol'), width: 100 },
  { colKey: 'trigger', title: t('system.globalSettings.backup.records.triggerCol'), width: 100 },
  { colKey: 'started_at', title: t('system.globalSettings.backup.records.startedCol'), width: 150 },
  { colKey: 'stats', title: t('system.globalSettings.backup.records.statsCol') },
  { colKey: 'error', title: t('system.globalSettings.backup.records.errorCol'), ellipsis: true },
  { colKey: 'op', title: t('system.globalSettings.backup.records.actionsCol'), width: 150 },
])

function statusTheme(status: string): string {
  switch (status) {
    case 'succeeded':
      return 'success'
    case 'failed':
      return 'danger'
    case 'running':
    case 'verifying':
    case 'restoring':
    case 'reindexing':
    case 'pending':
      return 'primary'
    case 'dry-run':
      return 'warning'
    default:
      return 'default'
  }
}

function formatDate(value: string): string {
  const d = new Date(value)
  return Number.isNaN(d.getTime()) ? value : d.toLocaleDateString()
}

function formatTime(value: string): string {
  const d = new Date(value)
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleTimeString()
}

function formatBytes(bytes: number): string {
  if (!bytes) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let v = bytes
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(v >= 100 || i === 0 ? 0 : 1)} ${units[i]}`
}

function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`
  if (ms < 60000) return `${(ms / 1000).toFixed(1)}s`
  return `${Math.floor(ms / 60000)}m${Math.round((ms % 60000) / 1000)}s`
}

async function reload(): Promise<void> {
  loading.value = true
  loadError.value = ''
  try {
    const result = await listBackupRecords({ status: statusFilter.value || undefined, limit: 50 })
    records.value = result.records
    latestSucceeded.value = result.latestSucceeded
  } catch (err: any) {
    if (err?.status === 503) {
      loadError.value = 'disabled'
    } else {
      loadError.value = err?.message || t('system.globalSettings.backup.loadFailed')
    }
  } finally {
    loading.value = false
  }
  // Config enriches the status card (real retention values); failures
  // fall back to the documented defaults silently.
  try {
    configInfo.value = await getBackupConfig()
  } catch {
    configInfo.value = null
  }
}

async function runBackupNow(): Promise<void> {
  running.value = true
  try {
    await runBackup()
    MessagePlugin.success(t('system.globalSettings.backup.runSuccess'))
    await reload()
  } catch (err: any) {
    MessagePlugin.error(err?.message || t('system.globalSettings.backup.runFailed'))
  } finally {
    running.value = false
  }
}

async function deleteRecord(row: BackupRecord): Promise<void> {
  try {
    await deleteBackup(row.id)
    MessagePlugin.success(t('system.globalSettings.backup.deleteSuccess'))
    await reload()
  } catch (err: any) {
    MessagePlugin.error(err?.message || t('system.globalSettings.backup.deleteFailed'))
  }
}

async function doExportTenant(): Promise<void> {
  if (!exportTenantId.value) return
  exporting.value = true
  try {
    await exportTenantArchive(exportTenantId.value)
    MessagePlugin.success(t('system.globalSettings.backup.export.success'))
  } catch (err: any) {
    MessagePlugin.error(err?.message || t('system.globalSettings.backup.export.failed'))
  } finally {
    exporting.value = false
  }
}

function openRestore(row: BackupRecord): void {
  restoreForm.value = {
    backupId: row.id,
    scope: 'tenant',
    tenantId: null,
    conflictMode: 'new',
  }
  dryRunReport.value = null
  activeJob.value = null
  restoreError.value = ''
  restoreBusy.value = ''
  restoreVisible.value = true
}

async function doDryRun(): Promise<void> {
  restoreBusy.value = 'dry'
  restoreError.value = ''
  try {
    const job = await startRestore({
      backup_id: restoreForm.value.backupId,
      scope: restoreForm.value.scope,
      tenant_id: restoreForm.value.scope === 'tenant' ? (restoreForm.value.tenantId ?? 0) : undefined,
      conflict_mode: restoreForm.value.scope === 'tenant' ? restoreForm.value.conflictMode : undefined,
      dry_run: true,
    })
    activeJob.value = job
    await pollJob(job.id)
    if (activeJob.value?.report) {
      dryRunReport.value = activeJob.value.report
    }
  } catch (err: any) {
    restoreError.value = err?.message || t('system.globalSettings.backup.restore.failed')
  } finally {
    restoreBusy.value = ''
  }
}

async function doRestore(): Promise<void> {
  restoreBusy.value = 'run'
  restoreError.value = ''
  try {
    const job = await startRestore({
      backup_id: restoreForm.value.backupId,
      scope: restoreForm.value.scope,
      tenant_id: restoreForm.value.scope === 'tenant' ? (restoreForm.value.tenantId ?? 0) : undefined,
      conflict_mode: restoreForm.value.scope === 'tenant' ? restoreForm.value.conflictMode : undefined,
      dry_run: false,
    })
    activeJob.value = job
    await pollJob(job.id)
    if (activeJob.value?.status === 'succeeded') {
      MessagePlugin.success(t('system.globalSettings.backup.restore.success'))
    } else if (activeJob.value?.status === 'failed') {
      restoreError.value =
        activeJob.value.progress?.message || t('system.globalSettings.backup.restore.failed')
    }
  } catch (err: any) {
    restoreError.value = err?.message || t('system.globalSettings.backup.restore.failed')
  } finally {
    restoreBusy.value = ''
  }
}

async function pollJob(jobId: string): Promise<void> {
  stopPolling()
  await new Promise<void>((resolve) => {
    jobPollTimer = setInterval(async () => {
      try {
        activeJob.value = await getRestoreJob(jobId)
        const status = activeJob.value?.status
        if (status === 'succeeded' || status === 'failed' || status === 'dry-run') {
          stopPolling()
          resolve()
        }
      } catch {
        stopPolling()
        resolve()
      }
    }, 1500)
  })
}

function stopPolling(): void {
  if (jobPollTimer) {
    clearInterval(jobPollTimer)
    jobPollTimer = null
  }
}

onMounted(reload)
onBeforeUnmount(stopPolling)
</script>

<style scoped>
.backup-management {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.backup-branch {
  margin-top: 8px;
}

.backup-status-card {
  display: flex;
  align-items: center;
  gap: 32px;
  padding: 16px 20px;
  border: 1px solid var(--td-border-level-1-color);
  border-radius: var(--td-radius-medium);
  background: var(--td-bg-color-container);
}

.backup-status-item {
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.backup-status-label {
  font-size: 12px;
  color: var(--td-text-color-placeholder);
}

.backup-status-value {
  font-size: 14px;
  font-weight: 500;
  color: var(--td-text-color-primary);
}

.backup-status-value--warn {
  color: var(--td-warning-color);
}

.backup-run-button {
  margin-left: auto;
}

.backup-records-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-top: 8px;
}

.backup-records-header h3 {
  margin: 0;
  font-size: 14px;
  font-weight: 600;
}

.backup-status-filter {
  width: 160px;
}

.backup-record-id {
  display: flex;
  align-items: center;
  gap: 6px;
}

.backup-record-id-text {
  font-family: var(--td-font-family-code, monospace);
  font-size: 12px;
}

.backup-time {
  display: flex;
  flex-direction: column;
  line-height: 1.4;
}

.backup-time-clock {
  font-size: 12px;
  color: var(--td-text-color-placeholder);
}

.backup-stats {
  display: flex;
  flex-wrap: wrap;
  gap: 12px;
  font-size: 12px;
  color: var(--td-text-color-secondary);
}

.backup-error-text {
  font-size: 12px;
  color: var(--td-error-color);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  display: block;
  max-width: 260px;
}

.backup-row-actions {
  display: flex;
  align-items: center;
  gap: 4px;
}

.backup-export-panel {
  margin-top: 16px;
  padding: 16px 20px;
  border: 1px solid var(--td-border-level-1-color);
  border-radius: var(--td-radius-medium);
  background: var(--td-bg-color-container);
}

.backup-export-panel h3 {
  margin: 0 0 4px;
  font-size: 14px;
  font-weight: 600;
}

.backup-export-hint {
  margin: 0 0 12px;
  font-size: 12px;
  color: var(--td-text-color-secondary);
}

.backup-export-row {
  display: flex;
  align-items: center;
  gap: 12px;
}

.backup-restore-body {
  display: flex;
  flex-direction: column;
  gap: 20px;
}

.backup-restore-section h4 {
  margin: 0 0 10px;
  font-size: 13px;
  font-weight: 600;
}

.backup-conflict-group {
  display: flex;
  flex-direction: column;
  gap: 6px;
  margin-top: 12px;
}

.backup-restore-note {
  margin: 10px 0 0;
  font-size: 12px;
  color: var(--td-text-color-placeholder);
}

.backup-dryrun-grid {
  display: flex;
  gap: 32px;
}

.backup-dryrun-cell {
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.backup-progress {
  display: flex;
  align-items: center;
  gap: 10px;
}

.backup-progress-message {
  font-size: 12px;
  color: var(--td-text-color-secondary);
}

.backup-progress-counters,
.backup-report-summary {
  display: flex;
  flex-wrap: wrap;
  gap: 16px;
  margin-top: 10px;
  font-size: 12px;
  color: var(--td-text-color-secondary);
}

.backup-restore-actions {
  display: flex;
  justify-content: flex-end;
  gap: 12px;
  padding-top: 8px;
  border-top: 1px solid var(--td-border-level-1-color);
}
</style>
