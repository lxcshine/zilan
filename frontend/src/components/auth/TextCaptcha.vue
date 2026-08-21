<template>
  <div class="text-captcha" :class="{ 'is-shake': shaking }">
    <!-- 挑战行：数字图 + 输入框；点击图片换一张 -->
    <div class="captcha-row">
      <div class="captcha-image" v-if="challenge" :title="t('auth.captcha.refresh')" @click="reset">
        <img :src="challenge.text_image" alt="captcha" draggable="false" />
      </div>
      <div class="captcha-image captcha-image--skeleton" v-else></div>

      <input v-model="answer" class="captcha-input" type="text" inputmode="numeric"
        autocomplete="one-time-code" maxlength="4" :disabled="!editable"
        :placeholder="t('auth.captcha.textPlaceholder')" @keyup.enter="submitAnswer"
        @input="onInput" />
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
 * 数字图形验证码（P0-4 §5.1 的 text 形态，auth.captcha.type=text 启用）。
 *
 * 与 SliderCaptcha 同契约：挂载拉挑战 → 用户按图输入 4 位数字 →
 * 满 4 位或回车自动提交 → 通过 emit('verified', token)，失败清空并
 * 换图。token 被父组件消费后调用 reset()。
 */
const emit = defineEmits<{
  (e: 'verified', token: string): void
  (e: 'failed'): void
}>()

const { t } = useI18n()
const { challenge, status, load, verify, reset } = useCaptchaChallenge()

const answer = ref('')
const shaking = ref(false)
const failedOnce = ref(false)

const editable = computed(() => status.value === 'ready')

function onInput() {
  // 只留数字；满 4 位自动提交
  answer.value = answer.value.replace(/\D/g, '')
  if (answer.value.length === 4) submitAnswer()
}

async function submitAnswer() {
  const val = answer.value.trim()
  if (val.length !== 4 || status.value !== 'ready') return
  const token = await verify({ answer: val })
  if (token) {
    failedOnce.value = false
    emit('verified', token)
    return
  }
  failedOnce.value = true
  answer.value = ''
  shaking.value = true
  setTimeout(() => (shaking.value = false), 500)
  emit('failed')
}

defineExpose({ reset })

onMounted(load)
</script>

<style lang="less" scoped>
.text-captcha {
  width: 300px;
  margin: 0 auto;
  user-select: none;

  &.is-shake {
    animation: captcha-shake 0.45s ease;
  }
}

.captcha-row {
  display: flex;
  gap: 10px;
  align-items: stretch;
}

.captcha-image {
  flex-shrink: 0;
  width: 160px;
  height: 60px;
  border-radius: 10px;
  overflow: hidden;
  border: 1px solid var(--td-component-stroke);
  background: var(--td-bg-color-secondarycontainer);
  cursor: pointer;

  img {
    display: block;
    width: 160px;
    height: 60px;
  }

  &:hover {
    border-color: var(--td-brand-color);
  }

  &--skeleton {
    cursor: default;
    background: linear-gradient(90deg,
        var(--td-bg-color-secondarycontainer) 25%,
        var(--td-bg-color-container) 50%,
        var(--td-bg-color-secondarycontainer) 75%);
    background-size: 200% 100%;
    animation: captcha-skeleton 1.2s ease infinite;
  }
}

.captcha-input {
  flex: 1;
  min-width: 0;
  height: 60px;
  border: 1px solid var(--td-component-stroke);
  border-radius: 10px;
  background: var(--td-bg-color-container);
  color: var(--td-text-color-primary);
  font-size: 22px;
  letter-spacing: 0.35em;
  text-align: center;
  font-family: var(--app-font-family);
  outline: none;
  transition: all 0.2s;

  &::placeholder {
    font-size: 13px;
    letter-spacing: normal;
    color: var(--td-text-color-placeholder);
  }

  &:focus {
    border-color: var(--td-brand-color);
    box-shadow: 0 0 0 3px var(--td-brand-color-focus, rgba(51, 51, 51, 0.08));
  }

  &:disabled {
    opacity: 0.6;
    cursor: not-allowed;
  }
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

@keyframes captcha-shake {
  0%, 100% { transform: translateX(0); }
  20% { transform: translateX(-6px); }
  40% { transform: translateX(6px); }
  60% { transform: translateX(-4px); }
  80% { transform: translateX(4px); }
}

@keyframes captcha-skeleton {
  0% { background-position: 200% 0; }
  100% { background-position: -200% 0; }
}
</style>
