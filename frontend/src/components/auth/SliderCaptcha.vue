<template>
  <div class="slider-captcha" :class="{ 'is-shake': shaking }">
    <!-- 挑战画布：300×180 背景拼块，与后端渲染尺寸 1:1（±6px 容差依赖此宽度） -->
    <div class="captcha-canvas" v-if="challenge">
      <img class="captcha-bg" :src="challenge.background_image" alt="" draggable="false" />
      <img class="captcha-piece" :src="challenge.piece_image" alt="" draggable="false"
        :style="pieceStyle" />
      <transition name="captcha-fade">
        <div v-if="status === 'success'" class="captcha-success">
          <svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor"
            stroke-width="3" stroke-linecap="round" stroke-linejoin="round">
            <polyline points="20 6 9 17 4 12" />
          </svg>
          <span>{{ t('auth.captcha.passed') }}</span>
        </div>
      </transition>
    </div>

    <!-- 滑轨：滑块 x 偏移即拼块 x 坐标，松手提交 -->
    <div class="captcha-track" :class="{ 'is-disabled': !draggable }" ref="trackRef">
      <div class="track-fill" :style="{ width: handleX + HANDLE_SIZE + 'px' }"></div>
      <div class="track-handle" :style="{ left: handleX + 'px' }" @pointerdown="onPointerDown"
        @pointermove="onPointerMove" @pointerup="onPointerUp" @pointercancel="onPointerUp"
        role="slider" :aria-valuenow="handleX" aria-valuemin="0" :aria-valuemax="MAX_X"
        :tabindex="draggable ? 0 : -1">
        <svg v-if="status === 'success'" width="18" height="18" viewBox="0 0 24 24" fill="none"
          stroke="currentColor" stroke-width="3" stroke-linecap="round" stroke-linejoin="round">
          <polyline points="20 6 9 17 4 12" />
        </svg>
        <svg v-else width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor"
          stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
          <polyline points="9 18 15 12 9 6" />
        </svg>
      </div>
      <span v-if="showHint" class="track-hint">{{ t('auth.captcha.sliderHint') }}</span>
    </div>

    <!-- 底部状态行 -->
    <div class="captcha-foot">
      <template v-if="status === 'loading'">{{ t('auth.captcha.loading') }}</template>
      <template v-else-if="status === 'error'">
        <span class="foot-error">{{ t('auth.captcha.loadFailed') }}</span>
        <a class="foot-action" @click.prevent="reset">{{ t('auth.captcha.retry') }}</a>
      </template>
      <template v-else-if="status === 'success'">{{ t('auth.captcha.passedHint') }}</template>
      <template v-else>
        <span v-if="failedOnce" class="foot-error">{{ t('auth.captcha.failedHint') }}</span>
        <a v-if="status === 'ready'" class="foot-action" @click.prevent="reset">{{
          t('auth.captcha.refresh') }}</a>
      </template>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useCaptchaChallenge } from '@/composables/useCaptchaChallenge'

/**
 * 滑块拼图人机验证（P0-4 §5.1）。
 *
 * 自包含组件：挂载即拉挑战；拖动滑块带动拼块，松手提交 x 偏移；
 * 验证通过 emit('verified', captcha_token)，失败自动刷新挑战。
 * token 被父组件消费后调用 reset() 重新验证（token 一次性）。
 *
 * 几何约定：背景 300×180、拼块 48×48，前端按 1:1 渲染（无缩放
 * 误差），后端容差 ±6px 依此成立。滑块位移 == 拼块 x 坐标。
 */
const emit = defineEmits<{
  (e: 'verified', token: string): void
  (e: 'failed'): void
}>()

const { t } = useI18n()
const { challenge, status, load, verify, reset } = useCaptchaChallenge()

const CANVAS_WIDTH = 300
const HANDLE_SIZE = 40
const MAX_X = CANVAS_WIDTH - HANDLE_SIZE // 260；后端 targetX ∈ [70, 232]，覆盖充足

const trackRef = ref<HTMLElement>()
const handleX = ref(0)
const dragging = ref(false)
const shaking = ref(false)
const failedOnce = ref(false)

let dragStartClientX = 0
let dragStartHandleX = 0

const draggable = computed(() => status.value === 'ready')
const showHint = computed(() => !dragging.value && handleX.value === 0 && status.value !== 'success')
const pieceStyle = computed(() => ({
  left: `${handleX.value}px`,
  top: `${challenge.value?.piece_y ?? 0}px`,
  width: `${challenge.value?.piece_size ?? 48}px`,
  height: `${challenge.value?.piece_size ?? 48}px`,
}))

function clamp(v: number, lo: number, hi: number) {
  return Math.min(Math.max(v, lo), hi)
}

function onPointerDown(e: PointerEvent) {
  if (!draggable.value) return
  dragging.value = true
  dragStartClientX = e.clientX
  dragStartHandleX = handleX.value
  ;(e.currentTarget as HTMLElement).setPointerCapture(e.pointerId)
  e.preventDefault()
}

function onPointerMove(e: PointerEvent) {
  if (!dragging.value) return
  handleX.value = clamp(dragStartHandleX + e.clientX - dragStartClientX, 0, MAX_X)
}

async function onPointerUp() {
  if (!dragging.value) return
  dragging.value = false
  // 几乎没拖动视为误触，弹回不提交
  if (handleX.value < 8) {
    handleX.value = 0
    return
  }
  await submitX(handleX.value)
}

async function submitX(x: number) {
  const token = await verify({ x: Math.round(x) })
  if (token) {
    failedOnce.value = false
    emit('verified', token)
    return
  }
  // 失败：抖动反馈 + 弹回 + 挑战已由 composable 自动刷新
  failedOnce.value = true
  shaking.value = true
  setTimeout(() => (shaking.value = false), 500)
  handleX.value = 0
  emit('failed')
}

defineExpose({ reset })

onMounted(load)
</script>

<style lang="less" scoped>
.slider-captcha {
  width: 300px;
  margin: 0 auto;
  user-select: none;

  &.is-shake {
    animation: captcha-shake 0.45s ease;
  }
}

.captcha-canvas {
  position: relative;
  width: 300px;
  height: 180px;
  border-radius: 10px;
  overflow: hidden;
  border: 1px solid var(--td-component-stroke);
  background: var(--td-bg-color-secondarycontainer);
}

.captcha-bg {
  display: block;
  width: 300px;
  height: 180px;
}

.captcha-piece {
  position: absolute;
  pointer-events: none;
  filter: drop-shadow(0 2px 4px rgba(0, 0, 0, 0.25));
}

.captcha-success {
  position: absolute;
  inset: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  background: rgba(34, 154, 22, 0.14);
  color: var(--td-success-color, #2ba471);
  font-size: 14px;
  font-weight: 600;
  backdrop-filter: blur(1px);
}

.captcha-track {
  position: relative;
  height: 40px;
  margin-top: 10px;
  border-radius: 10px;
  background: var(--td-bg-color-secondarycontainer);
  border: 1px solid var(--td-component-stroke);
  overflow: hidden;

  &.is-disabled {
    opacity: 0.65;
  }
}

.track-fill {
  position: absolute;
  left: 0;
  top: 0;
  bottom: 0;
  background: var(--td-brand-color-focus, rgba(51, 51, 51, 0.08));
  pointer-events: none;
}

.track-handle {
  position: absolute;
  top: 0;
  width: 40px;
  height: 40px;
  display: flex;
  align-items: center;
  justify-content: center;
  border-radius: 10px;
  background: var(--td-bg-color-container);
  border: 1px solid var(--td-component-stroke);
  color: var(--td-text-color-primary);
  cursor: grab;
  touch-action: none; // pointer 拖拽期间禁止页面滚动

  &:hover {
    border-color: var(--td-brand-color);
    color: var(--td-brand-color);
  }

  &:active {
    cursor: grabbing;
  }
}

.track-hint {
  position: absolute;
  inset: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 13px;
  color: var(--td-text-color-placeholder);
  pointer-events: none;
}

.captcha-foot {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 10px;
  min-height: 22px;
  margin-top: 6px;
  font-size: 12px;
  color: var(--td-text-color-secondary);
}

.foot-error {
  color: var(--td-error-color, #d54941);
}

.foot-action {
  color: var(--td-brand-color);
  cursor: pointer;

  &:hover {
    text-decoration: underline;
  }
}

.captcha-fade-enter-active {
  transition: opacity 0.25s ease;
}

.captcha-fade-enter-from {
  opacity: 0;
}

@keyframes captcha-shake {
  0%, 100% { transform: translateX(0); }
  20% { transform: translateX(-6px); }
  40% { transform: translateX(6px); }
  60% { transform: translateX(-4px); }
  80% { transform: translateX(4px); }
}
</style>
