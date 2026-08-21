import { ref } from 'vue'
import { getCaptchaChallenge, verifyCaptchaChallenge, type CaptchaChallengeResponse } from '@/api/auth'

export type CaptchaStatus = 'loading' | 'ready' | 'verifying' | 'success' | 'error'

/**
 * 人机验证挑战的共享状态机（P0-4 §5）。
 *
 * SliderCaptcha / TextCaptcha 两个组件共用：拉挑战 → 渲染 →
 * 提交答案 → 成功签发一次性 captcha_token / 失败自动刷新。
 * 组件只负责各自的交互形态（拖拽 / 输入），协议逻辑收敛在这里。
 *
 * captcha_token 的生命周期语义由后端保证：一次性消费。父组件在
 * 用掉 token（登录提交 / 发送验证码）后必须调用 load() 重新走一遍
 * 人机验证，不能复用。
 */
export function useCaptchaChallenge() {
  const challenge = ref<CaptchaChallengeResponse | null>(null)
  const status = ref<CaptchaStatus>('loading')

  /**
   * 拉取新挑战。status:
   *   loading → ready          正常
   *   loading → error          服务不可用 / 网络异常（可重试）
   */
  async function load(): Promise<void> {
    status.value = 'loading'
    challenge.value = null
    const resp = await getCaptchaChallenge()
    if (resp?.captcha_id) {
      challenge.value = resp
      status.value = 'ready'
    } else {
      status.value = 'error'
    }
  }

  /**
   * 提交答案（x = 滑块横向偏移；answer = 数字验证码）。
   * 返回 captcha_token；答案错误返回 null 并自动刷新挑战（挑战已
   * 被服务端作废或计错），组件据此外观复位。
   */
  async function verify(payload: { x?: number; answer?: string }): Promise<string | null> {
    if (!challenge.value || status.value !== 'ready') return null
    status.value = 'verifying'
    const resp = await verifyCaptchaChallenge({
      captcha_id: challenge.value.captcha_id,
      ...payload,
    })
    if (resp.success && resp.captcha_token) {
      status.value = 'success'
      return resp.captcha_token
    }
    // 答案错误（或挑战过期/次数耗尽）：刷新挑战让用户重试。
    await load()
    return null
  }

  /** 复位（父组件消费掉 token 后调用，重新进入待验证状态）。 */
  async function reset(): Promise<void> {
    await load()
  }

  return { challenge, status, load, verify, reset }
}
