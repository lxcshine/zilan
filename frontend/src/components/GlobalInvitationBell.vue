<template>
  <!-- 全局右上角通知铃铛（PRD ui-layout-visual-redesign §4.4）。
       - 汇聚两类通知：待处理邀请（打开收件箱弹窗）+ 共享空间待审批加入请求
         （组织入口从 Rail 移除后，徽标并入铃铛，点击直达待审批列表）。
       - 只在总数 > 0 时渲染，空收件箱场景不占用角落像素。
       - 固定定位、z-index 远低于 t-drawer 默认 2500，业务页面右侧抽屉（FAQ、KB 调试、
         Tenant 审计、SettingDrawer 等）弹出时会自然覆盖铃铛，不需要特意联动隐藏。 -->
  <template v-if="totalNoticeCount > 0">
    <t-badge :count="totalNoticeCount" :max-count="99" :offset="[6, 4]"
      class="global-invitation-bell">
      <button type="button" class="global-invitation-bell__btn"
        :title="bellTooltip" @click="handleClick">
        <t-icon name="notification" size="18px" />
      </button>
    </t-badge>
  </template>
  <MyInvitationsDialog v-model:visible="dialogVisible" />
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useAuthStore } from '@/stores/auth'
import { useOrganizationStore } from '@/stores/organization'
import MyInvitationsDialog from '@/components/MyInvitationsDialog.vue'

const { t } = useI18n()
const router = useRouter()
const authStore = useAuthStore()
const orgStore = useOrganizationStore()

const pendingInvitationCount = computed(() => authStore.pendingInvitationCount)

// 组织待审批加入请求：仅空间管理员可见（与旧侧栏徽标的可见性一致）
const orgPendingCount = computed(() =>
  !authStore.isLiteMode && authStore.hasRole('admin') ? orgStore.totalPendingJoinRequestCount : 0
)

const totalNoticeCount = computed(() => pendingInvitationCount.value + orgPendingCount.value)

const bellTooltip = computed(() =>
  orgPendingCount.value > 0 ? t('organization.settings.pendingJoinRequestsBadge') : t('tenantInvitation.inboxTooltip')
)

const dialogVisible = ref(false)

const handleClick = () => {
  // 有待审批加入请求时直达组织待审批列表；否则回落到邀请收件箱
  if (orgPendingCount.value > 0) {
    router.push('/platform/organizations')
    return
  }
  dialogVisible.value = true
}
</script>

<style lang="less" scoped>
.global-invitation-bell {
  position: fixed;
  top: 12px;
  right: 16px;
  /* 远低于 TDesign 抽屉的默认 z-index (2500)，确保业务页右侧抽屉弹出时能正常盖住铃铛。
     高于普通页面内容（一般 0~10），避免被列表卡片覆盖。 */
  z-index: 100;
}

.global-invitation-bell__btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 32px;
  height: 32px;
  padding: 0;
  border: 1px solid var(--td-component-stroke);
  border-radius: 10px;
  /* 用 container 背景而非透明，铃铛悬在内容区上方时和不同颜色的页面背景都能看清。 */
  background: var(--td-bg-color-container);
  color: var(--td-text-color-secondary);
  cursor: pointer;
  box-shadow: 0 2px 6px rgba(0, 0, 0, 0.04);
  transition: background-color 0.18s ease, color 0.18s ease, box-shadow 0.18s ease;

  &:hover {
    background-color: var(--td-bg-color-secondarycontainer);
    color: var(--td-brand-color);
    box-shadow: 0 4px 12px rgba(0, 0, 0, 0.08);
  }

  &:focus-visible {
    outline: 2px solid var(--td-brand-color-focus);
    outline-offset: 1px;
  }
}
</style>
