<template>
  <div class="login-layout">
    <!-- 顶部右侧：语言切换 -->
    <div class="header-links">
      <div class="language-switch">
        <button @click="toggleLanguageMenu" class="lang-button" :title="currentLangOption?.label">
          <span class="lang-flag-icon">{{ currentLangOption?.flag }}</span>
          <span class="link-text">{{ currentLangOption?.shortLabel }}</span>
          <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"
            stroke-linecap="round">
            <polyline points="6 9 12 15 18 9" />
          </svg>
        </button>

        <!-- Language Dropdown -->
        <div v-if="showLanguageMenu" class="language-dropdown">
          <div v-for="lang in languageOptions" :key="lang.value" @click="selectLanguage(lang.value)"
            class="language-option" :class="{ active: currentLanguage === lang.value }">
            <span class="lang-flag">{{ lang.flag }}</span>
            <span class="lang-label">{{ lang.label }}</span>
            <span v-if="currentLanguage === lang.value" class="check-icon">✓</span>
          </div>
        </div>
      </div>
    </div>

    <!-- 居中区域：品牌 + 表单 -->
    <div class="login-center">
      <div class="brand-block">
        <div class="brand-mark" aria-hidden="true">知</div>
        <h1 class="brand-name">知澜</h1>
        <p class="brand-slogan">{{ $t('auth.subtitle') }}</p>
      </div>

      <div class="form-panel">
        <!-- Login Card -->
        <div class="form-card" v-if="!isRegisterMode">
          <div class="form-header">
            <h2 class="form-title">{{ $t('auth.login') }}</h2>
            <p v-if="registrationEnabled" class="form-hint">{{ $t('auth.loginHint') }}</p>
          </div>

          <div class="form-content">
            <t-form ref="formRef" :data="formData" :rules="formRules" @submit="handleLogin" layout="vertical"
              label-align="top">
              <!-- P0-4：手机号 / 邮箱双通道登录，标签随输入内容自适应 -->
              <t-form-item :label="loginIdentifierLabel" name="identifier">
                <t-input v-model="formData.identifier" :placeholder="$t('auth.accountPlaceholder')" type="text"
                  autocomplete="username" size="large" :disabled="loading" />
              </t-form-item>

              <t-form-item :label="$t('auth.password')" name="password">
                <t-input v-model="formData.password" :placeholder="$t('auth.passwordPlaceholder')" type="password"
                  autocomplete="current-password" size="large" :disabled="loading" @enter="handleLogin" />
              </t-form-item>

              <!-- 登录人机验证：auth.captcha.login_required 开启（默认）时必过，
                   验证通过后点亮提交按钮；captcha_token 一次性，提交后复位 -->
              <div v-if="loginCaptchaRequired" class="captcha-field">
                <span class="captcha-field__label">{{ $t('auth.captcha.title') }}</span>
                <SliderCaptcha v-if="captchaType === 'slider'" ref="loginCaptchaRef"
                  @verified="onLoginCaptchaVerified" />
                <TextCaptcha v-else ref="loginCaptchaRef" @verified="onLoginCaptchaVerified" />
              </div>

              <t-button type="submit" theme="primary" size="large" block :loading="loading"
                :disabled="loginSubmitDisabled" class="submit-button">
                {{ loading ? $t('auth.loggingIn') : $t('auth.login') }}
              </t-button>

              <div class="register-cta" v-if="registrationEnabled">
                <div class="register-cta__divider">
                  <span>{{ $t('auth.firstTime') }}</span>
                </div>
                <t-button theme="default" variant="outline" size="large" block class="register-cta__button"
                  :disabled="loading" @click="toggleMode">
                  {{ $t('auth.createAccount') }}
                </t-button>
              </div>

              <div v-if="oidcEnabled" class="oidc-divider">
                <span>{{ $t('auth.orContinueWith') }}</span>
              </div>

              <t-button v-if="oidcEnabled" theme="default" size="large" block :loading="oidcLoading" :disabled="loading"
                class="oidc-button" @click="handleOIDCLogin">
                {{ oidcLoading ? $t('auth.redirectingToOIDC') : oidcLoginText }}
              </t-button>
            </t-form>
          </div>
        </div>

        <!-- Register Card. Renders when the user is in register mode
             AND either self-service registration is enabled OR they
             arrived with a valid share-link token (which bypasses the
             invite_only gate). -->
        <div class="form-card" v-if="isRegisterMode && (registrationEnabled || inviteLookup)">
          <!-- Share-link banner: shown only when ?token= resolved to a
               real invitation row. Sits above the form header so the
               invitee instantly sees who invited them and into which
               workspace, without bumping the existing register UX. -->
          <div v-if="inviteLookup" class="invite-banner">
            <t-icon name="link" class="invite-banner__icon" />
            <div class="invite-banner__text">
              <div class="invite-banner__title">
                {{ $t('inviteRegister.bannerTitle', { tenant: inviteLookup.tenant_name || '' }) }}
              </div>
              <div class="invite-banner__hint">
                {{ $t('inviteRegister.bannerHint') }}
              </div>
            </div>
          </div>
          <div v-else-if="inviteLookupError" class="invite-banner invite-banner--error">
            {{ inviteLookupError }}
          </div>
          <div class="form-header">
            <h2 class="form-title">{{ $t('auth.createAccount') }}</h2>
            <p class="form-subtitle">{{ $t('auth.registerSubtitle') }}</p>
          </div>

          <div class="form-content">
            <!-- ============ 通道式注册（P0-4 双通道） ============
                 短信 / 邮箱验证码证明联系方式所有权，用户名由服务端生成。
                 任一验证码通道可用即进入该表单；均不可用（零配置部署）或
                 邀请链接注册时回退到下方经典表单。 -->
            <template v-if="!useClassicRegister">
              <!-- 通道 Tab：双通道可用时展示，单通道静默固定 -->
              <div v-if="smsEnabled && emailCodeEnabled" class="channel-tabs" role="tablist">
                <button type="button" role="tab" class="channel-tab" :class="{ 'is-active': registerTab === 'sms' }"
                  @click="switchRegisterTab('sms')">
                  {{ $t('auth.phoneTab') }}
                </button>
                <button type="button" role="tab" class="channel-tab" :class="{ 'is-active': registerTab === 'email' }"
                  @click="switchRegisterTab('email')">
                  {{ $t('auth.emailTab') }}
                </button>
              </div>

              <t-form ref="codeFormRef" :data="codeRegisterData" :rules="codeRegisterRules" @submit="handleCodeRegister"
                layout="vertical" label-align="top">
                <t-form-item v-if="registerTab === 'sms'" :label="$t('auth.phone')" name="smsTarget">
                  <t-input v-model="codeRegisterData.smsTarget" :placeholder="$t('auth.phonePlaceholder')" type="text"
                    autocomplete="tel" size="large" :disabled="loading" />
                </t-form-item>
                <t-form-item v-else :label="$t('auth.email')" name="emailTarget">
                  <t-input v-model="codeRegisterData.emailTarget" :placeholder="$t('auth.emailPlaceholder')"
                    type="text" autocomplete="email" size="large" :disabled="loading" />
                </t-form-item>

                <!-- 人机验证：发送验证码的前置门槛（防短信/邮件轰炸） -->
                <div class="captcha-field">
                  <span class="captcha-field__label">{{ $t('auth.captcha.title') }}</span>
                  <SliderCaptcha v-if="captchaType === 'slider'" ref="registerCaptchaRef"
                    @verified="onRegisterCaptchaVerified" />
                  <TextCaptcha v-else ref="registerCaptchaRef" @verified="onRegisterCaptchaVerified" />
                </div>

                <t-form-item :label="registerTab === 'sms' ? $t('auth.verification.smsCode') : $t('auth.verification.emailCode')"
                  :name="registerTab === 'sms' ? 'smsCode' : 'emailCode'">
                  <div class="code-input-row">
                    <t-input v-model="activeCodeModel" :placeholder="$t('auth.verification.codePlaceholder')"
                      type="text" inputmode="numeric" maxlength="6" size="large" :disabled="loading" />
                    <t-button theme="default" variant="outline" size="large" class="send-code-button"
                      :disabled="!canSendCode" :loading="sendingCode" @click="sendCode">
                      {{ sendButtonText }}
                    </t-button>
                  </div>
                </t-form-item>

                <t-form-item :label="$t('auth.password')" name="password">
                  <t-input v-model="codeRegisterData.password" :placeholder="$t('auth.registerPasswordPlaceholder')"
                    type="password" autocomplete="new-password" size="large" :disabled="loading" />
                  <div v-if="codeRegisterData.password" class="password-strength">
                    <div class="password-strength__bars">
                      <span v-for="i in 3" :key="i" class="strength-bar"
                        :class="strengthBarClass(i)"></span>
                    </div>
                    <span class="password-strength__label" :class="`is-level-${registerStrength}`">{{
                      registerStrengthLabel }}</span>
                  </div>
                </t-form-item>

                <t-form-item :label="$t('auth.confirmPassword')" name="confirmPassword">
                  <t-input v-model="codeRegisterData.confirmPassword" :placeholder="$t('auth.confirmPasswordPlaceholder')"
                    type="password" autocomplete="new-password" size="large" :disabled="loading"
                    @enter="handleCodeRegister" />
                </t-form-item>

                <t-button type="submit" theme="primary" size="large" block :loading="loading" class="submit-button">
                  {{ loading ? $t('auth.registering') : $t('auth.register') }}
                </t-button>
              </t-form>
            </template>

            <!-- ============ 经典注册（邀请链接 / 零配置回退） ============ -->
            <t-form v-else ref="registerFormRef" :data="registerData" :rules="registerRules" @submit="handleRegister"
              layout="vertical" label-align="top">
              <t-form-item :label="$t('auth.username')" name="username">
                <t-input v-model="registerData.username" :placeholder="$t('auth.usernamePlaceholder')" size="large"
                  :disabled="loading" />
              </t-form-item>

              <t-form-item :label="$t('auth.email')" name="email">
                <t-input v-model="registerData.email" :placeholder="$t('auth.emailPlaceholder')" type="text"
                  autocomplete="email" size="large" :disabled="loading" />
              </t-form-item>

              <t-form-item :label="$t('auth.password')" name="password">
                <t-input v-model="registerData.password" :placeholder="$t('auth.registerPasswordPlaceholder')"
                  type="password" autocomplete="new-password" size="large" :disabled="loading" />
              </t-form-item>

              <t-form-item :label="$t('auth.confirmPassword')" name="confirmPassword">
                <t-input v-model="registerData.confirmPassword" :placeholder="$t('auth.confirmPasswordPlaceholder')"
                  type="password" autocomplete="new-password" size="large" :disabled="loading" @enter="handleRegister" />
              </t-form-item>

              <t-button type="submit" theme="primary" size="large" block :loading="loading" class="submit-button">
                {{ loading ? $t('auth.registering') : $t('auth.register') }}
              </t-button>
            </t-form>

            <div class="form-footer">
              <span>{{ $t('auth.haveAccount') }}</span>
              <a href="#" @click.prevent="toggleMode" class="link-button">
                {{ $t('auth.backToLogin') }}
              </a>
            </div>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, reactive, nextTick, onMounted, onBeforeUnmount, computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { MessagePlugin } from 'tdesign-vue-next'
import { useRoleLabel } from '@/composables/useRoleLabel'
import { notifyLoginSuccess } from '@/utils/loginNotify'
import {
  login,
  register,
  getOIDCAuthorizationURL,
  getOIDCConfig,
  autoSetup,
  getAuthConfig,
  sendVerificationCode,
  userInfoFromApi,
  getInvitationByToken,
  registerByInvite,
  type InviteLookup,
} from '@/api/auth'
import SliderCaptcha from '@/components/auth/SliderCaptcha.vue'
import TextCaptcha from '@/components/auth/TextCaptcha.vue'
import {
  detectIdentifier,
  isMainlandChinaMobile,
  isEmailFormat,
  passwordStrengthLevel,
} from '@/utils/identifier'
import { useAuthStore } from '@/stores/auth'
import { useI18n } from 'vue-i18n'

const router = useRouter()
const route = useRoute()
const authStore = useAuthStore()
const { t, tm, locale } = useI18n()
const { formatRole, roleIcon } = useRoleLabel()

// Form references
const formRef = ref()
const registerFormRef = ref()
const codeFormRef = ref()

// State management
const loading = ref(false)
const oidcLoading = ref(false)
const isRegisterMode = ref(false)
const showLanguageMenu = ref(false)
const oidcEnabled = ref(false)
const oidcProviderName = ref('')
// registrationEnabled defaults to true so that on first paint the Register
// link is visible; the actual mode is fetched from /auth/config in onMounted.
// In invite_only mode the link/card are hidden.
const registrationEnabled = ref(true)

// ---- P0-4 双通道注册/登录（docs/prd/auth-dual-channel-verification.md）----
// 人机验证与验证码通道可用性，来自 GET /auth/config；字段缺失（旧后端
// 或拉取失败）一律按“未启用”降级——经典邮箱注册/登录不受影响。
const captchaCfg = ref<{ enabled: boolean; login_required: boolean; type: string } | null>(null)
const smsEnabled = ref(false)
const emailCodeEnabled = ref(false)

// 登录人机验证（captcha_token 一次性：提交后必须重新验证）
const loginCaptchaRef = ref()
const loginCaptchaToken = ref('')

// 通道式注册状态。smsTarget/emailTarget 分开存，切 Tab 不丢已填内容；
// password 跨 Tab 共享（用户切换通道时不必重输密码）。
const registerTab = ref<'sms' | 'email'>('sms')
const registerCaptchaRef = ref()
const registerCaptchaToken = ref('')
const sendingCode = ref(false)
const smsCountdown = ref(0)
const emailCountdown = ref(0)
let countdownTimer: ReturnType<typeof setInterval> | null = null

const codeRegisterData = reactive<{ [key: string]: any }>({
  smsTarget: '',
  smsCode: '',
  emailTarget: '',
  emailCode: '',
  password: '',
  confirmPassword: '',
})

// invite-link state. When the URL carries ?token=xxx we resolve it to
// the originating tenant + role and switch the form into a "register
// via invitation" mode. The token bypasses the normal invite_only
// gate — possessing it IS the authorisation. Submitting the register
// form with this set hits /auth/register-by-invite (auto-login on
// success) instead of /auth/register.
const inviteToken = ref('')
const inviteLookup = ref<InviteLookup | null>(null)
const inviteLookupError = ref('')
const inviteLookupLoading = ref(false)

// Language options
const languageOptions = [
  { value: 'zh-CN', label: '简体中文', shortLabel: '中文', flag: '🇨🇳' },
  { value: 'en-US', label: 'English', shortLabel: 'EN', flag: '🇺🇸' },
  { value: 'ru-RU', label: 'Русский', shortLabel: 'RU', flag: '🇷🇺' },
  { value: 'ko-KR', label: '한국어', shortLabel: '한국어', flag: '🇰🇷' }
]

const currentLanguage = computed(() => locale.value)
const oidcLoginText = computed(() => {
  if (oidcProviderName.value) {
    return t('auth.oidcLoginWithProvider', { provider: oidcProviderName.value })
  }
  return t('auth.oidcLogin')
})
const currentLangOption = computed(() => languageOptions.find(l => l.value === currentLanguage.value))

// ---- P0-4 computed ------------------------------------------------------

// 登录是否强制人机验证（auth.captcha.login_required，默认开）
const loginCaptchaRequired = computed(() => !!(captchaCfg.value?.enabled && captchaCfg.value?.login_required))
// 人机验证形态：slider（默认）| text
const captchaType = computed(() => captchaCfg.value?.type || 'slider')
// 人机验证通过前提交按钮置灰
const loginSubmitDisabled = computed(() => loading.value || (loginCaptchaRequired.value && !loginCaptchaToken.value))
// 注册表单形态：邀请链接注册（经典表单）或验证码通道全不可用（零配置
// 部署回退）时走经典用户名+邮箱注册；否则走通道式注册。
const useClassicRegister = computed(() => !!inviteToken.value || !(smsEnabled.value || emailCodeEnabled.value))

// 登录标识符标签随输入内容自适应（手机号 / 邮箱 / 账号）
const loginIdentifierLabel = computed(() => {
  const kind = detectIdentifier(formData.identifier)
  if (kind === 'phone') return t('auth.phone')
  if (kind === 'email') return t('auth.email')
  return t('auth.account')
})

// 当前激活通道的目标 / 验证码 / 倒计时（Tab 切换的读写收口）
const activeTarget = computed(() =>
  (registerTab.value === 'sms' ? codeRegisterData.smsTarget : codeRegisterData.emailTarget).trim())
const activeCodeModel = computed<string>({
  get: () => (registerTab.value === 'sms' ? codeRegisterData.smsCode : codeRegisterData.emailCode),
  set: (v: string) => {
    if (registerTab.value === 'sms') codeRegisterData.smsCode = v
    else codeRegisterData.emailCode = v
  },
})
const activeCountdown = computed(() => (registerTab.value === 'sms' ? smsCountdown.value : emailCountdown.value))

// 发送验证码前置条件：目标格式合法 + 人机验证已通过 + 不在倒计时/发送中
const canSendCode = computed(() => {
  if (sendingCode.value || activeCountdown.value > 0 || !registerCaptchaToken.value) return false
  return registerTab.value === 'sms' ? isMainlandChinaMobile(activeTarget.value) : isEmailFormat(activeTarget.value)
})
const sendButtonText = computed(() =>
  activeCountdown.value > 0 ? t('auth.verification.resendIn', { seconds: activeCountdown.value }) : t('auth.verification.send'))

// 注册密码强度指示器（弱/中/强）
const registerStrength = computed(() => passwordStrengthLevel(codeRegisterData.password))
const registerStrengthLabel = computed(() => {
  const labels = ['auth.passwordStrengthEmpty', 'auth.passwordStrengthWeak', 'auth.passwordStrengthMedium', 'auth.passwordStrengthStrong']
  return t(labels[registerStrength.value])
})
function strengthBarClass(i: number) {
  const level = registerStrength.value
  if (i > level) return ''
  return level === 1 ? 'is-weak' : level === 2 ? 'is-medium' : 'is-strong'
}

// Login form data
const formData = reactive<{ [key: string]: any }>({
  identifier: '',
  password: '',
})

// Register form data（经典注册：邀请链接 / 零配置回退）
const registerData = reactive<{ [key: string]: any }>({
  username: '',
  email: '',
  password: '',
  confirmPassword: ''
})

// Login form validation rules。
// 密码只做非空校验——存量用户可能持有旧策略密码，强度策略只约束新注册。
const formRules = computed(() => ({
  identifier: [
    { required: true, message: t('auth.identifierRequired'), type: 'error' },
    {
      validator: (v: string) => detectIdentifier(v ?? '') !== null,
      message: t('auth.identifierInvalid'),
      type: 'error'
    }
  ],
  password: [
    { required: true, message: t('auth.passwordRequired'), type: 'error' }
  ]
}))

// P0-4 注册密码策略：8-32 位，同时包含大写、小写、数字（前后端一致）。
const passwordPolicyRules = () => ([
  { required: true, message: t('auth.passwordRequired'), type: 'error' },
  { min: 8, message: t('auth.passwordMinLength'), type: 'error' },
  { max: 32, message: t('auth.passwordMaxLength'), type: 'error' },
  { pattern: /[a-z]/, message: t('auth.passwordMustContainLowercase'), type: 'error' },
  { pattern: /[A-Z]/, message: t('auth.passwordMustContainUppercase'), type: 'error' },
  { pattern: /\d/, message: t('auth.passwordMustContainNumber'), type: 'error' }
])

// Register form validation rules（经典注册）
const registerRules = computed(() => ({
  username: [
    { required: true, message: t('auth.usernameRequired'), type: 'error' },
    { min: 2, message: t('auth.usernameMinLength'), type: 'error' },
    { max: 20, message: t('auth.usernameMaxLength'), type: 'error' },
    {
      pattern: /^[a-zA-Z0-9_\u4e00-\u9fa5]+$/,
      message: t('auth.usernameInvalid'),
      type: 'error'
    }
  ],
  email: [
    { required: true, message: t('auth.emailRequired'), type: 'error' },
    { email: true, message: t('auth.emailInvalid'), type: 'error' }
  ],
  password: passwordPolicyRules(),
  confirmPassword: [
    { required: true, message: t('auth.confirmPasswordRequired'), type: 'error' },
    {
      validator: (val: string) => val === registerData.password,
      message: t('auth.passwordMismatch'),
      type: 'error'
    }
  ]
}))

// 通道式注册校验规则。rules 只包含当前 Tab 渲染的字段——TDesign 表单
// 按 rules key 逐项校验，未渲染字段的规则不应参与提交校验。
const codeRegisterRules = computed(() => {
  const targetRules = (phone: boolean) => ([
    { required: true, message: phone ? t('auth.phoneRequired') : t('auth.emailRequired'), type: 'error' },
    {
      validator: (v: string) => (phone ? isMainlandChinaMobile(v ?? '') : isEmailFormat(v ?? '')),
      message: phone ? t('auth.phoneInvalid') : t('auth.emailInvalid'),
      type: 'error'
    }
  ])
  const codeRules = [
    { required: true, message: t('auth.verification.codeRequired'), type: 'error' },
    { pattern: /^\d{6}$/, message: t('auth.verification.codeInvalid'), type: 'error' }
  ]
  const base = {
    password: passwordPolicyRules(),
    confirmPassword: [
      { required: true, message: t('auth.confirmPasswordRequired'), type: 'error' },
      {
        validator: (val: string) => val === codeRegisterData.password,
        message: t('auth.passwordMismatch'),
        type: 'error'
      }
    ]
  }
  return registerTab.value === 'sms'
    ? { ...base, smsTarget: targetRules(true), smsCode: codeRules }
    : { ...base, emailTarget: targetRules(false), emailCode: codeRules }
})

// Toggle login/register mode
const toggleMode = () => {
  isRegisterMode.value = !isRegisterMode.value

  Object.keys(registerData).forEach(key => {
    (registerData as any)[key] = ''
  })
  resetCodeRegisterForm()
}

// ---- P0-4 人机验证 / 验证码交互 -------------------------------------------

const onLoginCaptchaVerified = (token: string) => {
  loginCaptchaToken.value = token
}
const onRegisterCaptchaVerified = (token: string) => {
  registerCaptchaToken.value = token
}

// captcha_token 一次性：被消费（登录提交 / 发送验证码）后必须复位，
// 让用户重新完成人机验证。
function resetLoginCaptcha() {
  loginCaptchaToken.value = ''
  loginCaptchaRef.value?.reset?.()
}
function resetRegisterCaptcha() {
  registerCaptchaToken.value = ''
  registerCaptchaRef.value?.reset?.()
}

// 通道 Tab 切换：清掉上一个 Tab 的校验错误（目标/验证码字段被 v-if
// 卸载再挂载，错误态不会自动消失）。
const switchRegisterTab = (tab: 'sms' | 'email') => {
  if (registerTab.value === tab || loading.value) return
  registerTab.value = tab
  nextTick(() => codeFormRef.value?.clearValidate?.())
}

function startCountdown(channel: 'sms' | 'email') {
  if (channel === 'sms') smsCountdown.value = 60
  else emailCountdown.value = 60
  if (countdownTimer) return
  countdownTimer = setInterval(() => {
    smsCountdown.value = Math.max(0, smsCountdown.value - 1)
    emailCountdown.value = Math.max(0, emailCountdown.value - 1)
    if (smsCountdown.value === 0 && emailCountdown.value === 0 && countdownTimer) {
      clearInterval(countdownTimer)
      countdownTimer = null
    }
  }, 1000)
}

// 后端验证码服务的机器码（error.details）→ i18n 文案
function verificationErrorMessage(detail: string | undefined, fallback?: string): string {
  const keyMap: Record<string, string> = {
    invalid_target: 'auth.verification.invalidTarget',
    channel_disabled: 'auth.verification.channelDisabled',
    captcha_required: 'auth.captcha.required',
    resend_too_frequent: 'auth.verification.resendTooFrequent',
    daily_limit_exceeded: 'auth.verification.dailyLimit',
  }
  if (detail && keyMap[detail]) return t(keyMap[detail])
  return fallback || t('auth.verification.sendFailed')
}

// 注册接口的英文错误文案 → i18n（防重复注册、密码策略、验证码失败等）
function registerErrorMessage(message?: string): string {
  const msg = message || ''
  if (msg.includes('phone number already exists')) return t('auth.phoneTaken')
  if (msg.includes('email already exists')) return t('auth.emailTaken')
  if (msg.includes('username already exists')) return t('auth.usernameTaken')
  if (msg.includes('password must be')) return t('auth.passwordPolicyHint')
  if (msg.includes('expired')) return t('auth.verification.codeExpired')
  if (msg.includes('too many failed attempts')) return t('auth.verification.tooManyAttempts')
  if (msg.includes('code')) return t('auth.verification.codeMismatch')
  return msg || t('auth.registerFailed')
}

// 发送短信/邮箱验证码。服务端在频控检查之前就烧掉 captcha_token——
// 因此除格式/通道错误外的所有失败路径都要重新人机验证。
const sendCode = async () => {
  if (!canSendCode.value) return
  sendingCode.value = true
  const channel = registerTab.value
  try {
    await sendVerificationCode({
      channel,
      target: activeTarget.value,
      purpose: 'register',
      captcha_token: registerCaptchaToken.value,
    })
    resetRegisterCaptcha()
    startCountdown(channel)
    MessagePlugin.success(t(channel === 'sms' ? 'auth.verification.smsSent' : 'auth.verification.emailSent'))
  } catch (error: any) {
    const detail = error?.error?.details || error?.details || ''
    if (detail !== 'invalid_target' && detail !== 'channel_disabled') {
      resetRegisterCaptcha()
    }
    if (detail === 'resend_too_frequent') startCountdown(channel)
    MessagePlugin.error(verificationErrorMessage(detail, error?.message))
  } finally {
    sendingCode.value = false
  }
}

function resetCodeRegisterForm() {
  Object.keys(codeRegisterData).forEach(key => {
    (codeRegisterData as any)[key] = ''
  })
  resetRegisterCaptcha()
  if (countdownTimer) {
    clearInterval(countdownTimer)
    countdownTimer = null
  }
  smsCountdown.value = 0
  emailCountdown.value = 0
}

// Toggle language menu
const toggleLanguageMenu = () => {
  showLanguageMenu.value = !showLanguageMenu.value
}

// Select language
const selectLanguage = (lang: string) => {
  locale.value = lang
  localStorage.setItem('locale', lang)
  showLanguageMenu.value = false
  MessagePlugin.success(t('language.languageSaved'))
}

// Close language menu when clicking outside
const handleClickOutside = (event: MouseEvent) => {
  const target = event.target as HTMLElement
  if (!target.closest('.language-switch')) {
    showLanguageMenu.value = false
  }
}

// Add click outside listener
onMounted(() => {
  document.addEventListener('click', handleClickOutside)
})

onBeforeUnmount(() => {
  document.removeEventListener('click', handleClickOutside)
  if (countdownTimer) {
    clearInterval(countdownTimer)
    countdownTimer = null
  }
})

const persistLoginResponse = async (response: any) => {
  // Backend renamed `tenant` to `active_tenant` and added `memberships`
  // when tenant-level RBAC landed (issue #1303). The two are otherwise
  // identical — `active_tenant` is the tenant whose ID is encoded in the
  // JWT, defaulting to the user's home tenant on a fresh login.
  const activeTenant = response.active_tenant || response.tenant
  if (response.user && response.token) {
    // user.tenant_id must be the user's HOME tenant (the immutable row
    // on the users table); useHomeTenant() and the home-badge logic both
    // assume so. The ACTIVE tenant (which can differ from home when the
    // server honoured a remembered last-active-tenant preference) is
    // expressed separately via setSelectedTenant below.
    const homeTenantIdRaw = response.user.tenant_id ?? activeTenant?.id ?? ''
    authStore.setUser(userInfoFromApi(response.user, homeTenantIdRaw))
    authStore.setToken(response.token)
    if (response.refresh_token) {
      authStore.setRefreshToken(response.refresh_token)
    }
    if (activeTenant) {
      authStore.setTenant({
        id: String(activeTenant.id) || '',
        name: activeTenant.name || '',
        owner_id: response.user.id || '',
        created_at: activeTenant.created_at || new Date().toISOString(),
        updated_at: activeTenant.updated_at || new Date().toISOString()
      })
    } else {
      authStore.setTenant(null)
    }
    if (Array.isArray(response.memberships)) {
      authStore.setMemberships(response.memberships)
    }
    // If the backend dropped us into a non-home tenant (honoured a
    // remembered "last active tenant" preference), set the override so
    // subsequent requests carry X-Tenant-ID and the UI stays consistent.
    // Otherwise clear any stale override left in localStorage by a
    // previous session for a different account.
    const activeIdNum = Number(activeTenant?.id)
    const homeIdNum = Number(homeTenantIdRaw)
    if (Number.isFinite(activeIdNum) && Number.isFinite(homeIdNum) && activeIdNum !== homeIdNum) {
      authStore.setSelectedTenant(activeIdNum, activeTenant?.name || null)
    } else {
      authStore.setSelectedTenant(null, null)
    }
  }

  // Pull runtime capabilities (including whether ordinary users may create
  // workspaces) before entering the main UI so create actions never flash
  // briefly when the deployment is invitation-only.
  await authStore.refreshFromAuthMe()
  await nextTick()
  router.replace(authStore.hasValidTenant ? '/platform/knowledge-bases' : '/onboarding/workspace')
}

const getBackendOIDCRedirectURI = () => `${window.location.origin}/api/v1/auth/oidc/callback`

const loadOIDCConfig = async () => {
  try {
    const response = await getOIDCConfig()
    oidcEnabled.value = !!response.success && !!response.enabled
    oidcProviderName.value = response.provider_display_name || ''
  } catch {
    oidcEnabled.value = false
    oidcProviderName.value = ''
  }
}

// loadAuthConfig fetches /auth/config and caches whether self-service
// registration is allowed. Failures fall back to "enabled" so a transient
// network glitch doesn't lock new users out of an open deployment.
// P0-4: also caches captcha + verification-code channel availability;
// on failure everything degrades to the pre-P0-4 classic forms.
const loadAuthConfig = async () => {
  try {
    const response = await getAuthConfig()
    registrationEnabled.value = response.registration_mode !== 'invite_only'
    captchaCfg.value = response.captcha ?? null
    smsEnabled.value = !!response.channels?.sms_enabled
    emailCodeEnabled.value = !!response.channels?.email_enabled
  } catch {
    registrationEnabled.value = true
    captchaCfg.value = null
    smsEnabled.value = false
    emailCodeEnabled.value = false
  }
  // 默认通道：短信可用时优先手机注册，否则邮箱
  registerTab.value = smsEnabled.value ? 'sms' : 'email'
}

const handleOIDCLogin = async () => {
  try {
    oidcLoading.value = true
    const response = await getOIDCAuthorizationURL(getBackendOIDCRedirectURI())
    const authorizationURL = response.authorization_url

    if (!response.success || !authorizationURL) {
      MessagePlugin.error(response.message || t('auth.oidcLoginFailed'))
      return
    }

    window.location.href = authorizationURL
  } catch (error: any) {
    console.error('OIDC 登录跳转失败:', error)
    MessagePlugin.error(error.message || t('auth.oidcLoginFailed'))
  } finally {
    oidcLoading.value = false
  }
}

// Handle login
const handleLogin = async () => {
  try {
    const valid = await formRef.value?.validate()
    if (valid !== true) return

    loading.value = true

    const response = await login({
      identifier: (formData.identifier || '').trim(),
      password: formData.password,
      captcha_token: loginCaptchaToken.value || undefined,
    })

    // captcha_token 一次性：每次提交（无论成败）后都要重新人机验证
    if (loginCaptchaRequired.value) {
      resetLoginCaptcha()
    }

    if (response.success) {
      await persistLoginResponse(response)
      notifyLoginSuccess(response, t, tm, formatRole, roleIcon)
    } else if (response.message?.includes('captcha')) {
      // 客户端已置灰按钮，此处仅剩 token 过期（10 分钟闲置）场景
      MessagePlugin.error(t('auth.captcha.required'))
    } else {
      MessagePlugin.error(response.message || t('auth.loginError'))
    }
  } catch (error: any) {
    console.error('登录错误:', error)
    MessagePlugin.error(error.message || t('auth.loginErrorRetry'))
  } finally {
    loading.value = false
  }
}

// Handle registration. Dispatches based on whether the user arrived
// with a share-link token: with token -> register-by-invite (auto-
// login on success); without -> the normal self-service register
// (drops back to the login form for the user to sign in).
// This is the CLASSIC (username/email/password) path — the dual-channel
// code registration lives in handleCodeRegister below.
const handleRegister = async () => {
  try {
    const valid = await registerFormRef.value?.validate()
    if (valid !== true) return

    loading.value = true

    if (inviteToken.value) {
      const response = await registerByInvite({
        token: inviteToken.value,
        username: registerData.username,
        email: registerData.email,
        password: registerData.password,
      })
      if (!response.success) {
        MessagePlugin.error(registerErrorMessage(response.message))
        return
      }
      MessagePlugin.success(t('auth.registerSuccess'))
      // register-by-invite returns the same shape as login (token +
      // active_tenant + memberships), so reuse the login persistence
      // path — same store writes, same redirect target.
      await persistLoginResponse(response)
      return
    }

    const response = await register({
      username: registerData.username,
      email: registerData.email,
      password: registerData.password
    })

    if (response.success) {
      MessagePlugin.success(t('auth.registerSuccess'))

      // Switch to login mode and fill in the account
      isRegisterMode.value = false
      formData.identifier = registerData.email

      // Clear register form
      Object.keys(registerData).forEach(key => {
        (registerData as any)[key] = ''
      })
    } else {
      MessagePlugin.error(registerErrorMessage(response.message))
    }
  } catch (error: any) {
    console.error('注册错误:', error)
    MessagePlugin.error(error.message || t('auth.registerError'))
  } finally {
    loading.value = false
  }
}

// Handle dual-channel (verification-code) registration: ownership of the
// phone/email is proven by the code, the username is generated server-side.
const handleCodeRegister = async () => {
  try {
    const valid = await codeFormRef.value?.validate()
    if (valid !== true) return

    loading.value = true

    const response = await register({
      channel: registerTab.value,
      target: activeTarget.value,
      code: activeCodeModel.value.trim(),
      password: codeRegisterData.password,
    })

    if (response.success) {
      MessagePlugin.success(t('auth.registerSuccess'))

      // Switch to login mode, prefill the just-registered account
      const registered = activeTarget.value
      isRegisterMode.value = false
      formData.identifier = registered
      resetCodeRegisterForm()
    } else {
      MessagePlugin.error(registerErrorMessage(response.message))
    }
  } catch (error: any) {
    console.error('注册错误:', error)
    MessagePlugin.error(error.message || t('auth.registerError'))
  } finally {
    loading.value = false
  }
}

// Check if already logged in; for lite edition, attempt transparent auto-setup
onMounted(async () => {
  // Share-link landing: ?token=xxx switches the form into invite-
  // register mode before any other auto-flow (logged-in redirect /
  // auto-setup / OIDC) gets a chance to redirect. Resolution failure
  // surfaces inline; the user can still log in normally if they
  // already have an account. We check this BEFORE the isLoggedIn
  // redirect so an existing session doesn't bounce the user to
  // /platform (and possibly back to /login if the session is stale),
  // dropping the invite token along the way.
  const tokenFromQuery = String(route.query.token || '').trim()
  if (tokenFromQuery) {
    inviteToken.value = tokenFromQuery
    inviteLookupLoading.value = true
    try {
      const resp = await getInvitationByToken(tokenFromQuery)
      if (resp.success && resp.data) {
        inviteLookup.value = resp.data
        // Token bypasses invite_only — show the register card even
        // when self-service registration is otherwise disabled.
        registrationEnabled.value = true
        isRegisterMode.value = true
      } else {
        inviteLookupError.value = resp.message || t('inviteRegister.invalidBody')
      }
    } catch {
      inviteLookupError.value = t('inviteRegister.invalidBody')
    } finally {
      inviteLookupLoading.value = false
    }
    // Don't run auto-setup when the user came in via an invite link —
    // they're explicitly trying to register, not bootstrap a Lite
    // single-user instance.
    loadOIDCConfig()
    return
  }

  if (authStore.isLoggedIn) {
    router.replace('/platform/knowledge-bases')
    return
  }

  const AUTO_SETUP_FAILED_KEY = 'zilan_auto_setup_failed'
  if (localStorage.getItem(AUTO_SETUP_FAILED_KEY) !== 'true') {
    try {
      const response = await autoSetup()
      if (response.success) {
        authStore.setLiteMode(true)
        await persistLoginResponse(response)
        return
      } else {
        localStorage.setItem(AUTO_SETUP_FAILED_KEY, 'true')
      }
    } catch {
      localStorage.setItem(AUTO_SETUP_FAILED_KEY, 'true')
    }
  }

  loadOIDCConfig()
  loadAuthConfig()
})
</script>

<style lang="less" scoped>
.login-layout {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 100%;
  min-height: 100%;
  position: relative;
  background: var(--td-bg-color-page);
  padding: 24px;
  box-sizing: border-box;
}

/* ---------- 品牌区 ---------- */
.brand-block {
  display: flex;
  flex-direction: column;
  align-items: center;
  margin-bottom: 32px;
}

.brand-mark {
  width: 56px;
  height: 56px;
  border-radius: 16px;
  background: var(--td-brand-color);
  color: #fff;
  font-size: 28px;
  font-weight: 600;
  display: flex;
  align-items: center;
  justify-content: center;
  user-select: none;
  margin-bottom: 16px;
}

.brand-name {
  font-size: 26px;
  font-weight: 600;
  letter-spacing: 0.08em;
  color: var(--td-text-color-primary);
  margin: 0 0 6px 0;
  font-family: var(--app-font-family);
}

.brand-slogan {
  font-size: 14px;
  color: var(--td-text-color-secondary);
  margin: 0;
  font-family: var(--app-font-family);
}

/* ---------- 表单区 ---------- */
.login-center {
  width: 100%;
  max-width: 420px;
  display: flex;
  flex-direction: column;
  align-items: center;
  position: relative;
  z-index: 2;
}

.form-panel {
  width: 100%;
}

.form-card {
  background: var(--td-bg-color-container);
  border-radius: 16px;
  padding: 36px 36px 32px;
  box-shadow: var(--td-shadow-2);
  border: 1px solid var(--td-component-stroke);
  box-sizing: border-box;
  width: 100%;
}

/* Share-link invitation banner. Sits above the register form when the
 * user arrived via /register?token=xxx; gives them confirmation of who
 * invited them before they fill anything in. */
.invite-banner {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  padding: 12px 14px;
  margin-bottom: 20px;
  border-radius: 10px;
  background: var(--td-bg-color-secondarycontainer);
  border: 1px solid var(--td-component-stroke);
  color: var(--td-text-color-primary);
}

.invite-banner__icon {
  margin-top: 2px;
  font-size: 18px;
  flex-shrink: 0;
  color: var(--td-text-color-secondary);
}

.invite-banner__text {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}

.invite-banner__title {
  font-size: 14px;
  font-weight: 600;
  line-height: 1.4;
  color: var(--td-text-color-primary);
}

.invite-banner__hint {
  font-size: 12px;
  color: var(--td-text-color-secondary);
  line-height: 1.5;
}

.invite-banner--error {
  background: var(--td-error-color-1, rgba(220, 38, 38, 0.06));
  border-color: var(--td-error-color-3, rgba(220, 38, 38, 0.2));
  color: var(--td-error-color, #b91c1c);
  font-size: 13px;
}

.form-header {
  text-align: center;
  margin-bottom: 28px;
}

.form-title {
  font-size: 22px;
  font-weight: 600;
  color: var(--td-text-color-primary);
  margin: 0;
  font-family: var(--app-font-family);
}

.form-subtitle {
  font-size: 13px;
  color: var(--td-text-color-secondary);
  margin: 8px 0 0 0;
  font-family: var(--app-font-family);
}

.form-hint {
  margin: 10px 0 0;
  padding: 8px 12px;
  border-radius: 8px;
  background: var(--td-bg-color-secondarycontainer);
  color: var(--td-text-color-secondary);
  font-size: 12.5px;
  line-height: 1.5;
  font-family: var(--app-font-family);
}

/* 注册入口：带分隔线的次级按钮 */
.register-cta {
  margin-top: 8px;

  &__divider {
    position: relative;
    text-align: center;
    margin: 4px 0 14px;
    color: var(--td-text-color-secondary);
    font-size: 13px;
    font-family: var(--app-font-family);

    span {
      position: relative;
      z-index: 1;
      padding: 0 12px;
      background: var(--td-bg-color-container);
    }

    &::before {
      content: '';
      position: absolute;
      left: 0;
      right: 0;
      top: 50%;
      border-top: 1px solid var(--td-component-stroke);
    }
  }

  &__button {
    height: 46px;
    border-radius: 10px;
    font-size: 15px;
    font-weight: 500;
    border-color: var(--td-component-stroke);
    color: var(--td-text-color-primary);

    &:hover {
      border-color: var(--td-brand-color);
      color: var(--td-brand-color);
      background: var(--td-bg-color-secondarycontainer);
    }
  }
}

.form-content {
  :deep(.t-form-item__label) {
    font-size: 14px;
    color: var(--td-text-color-primary);
    font-weight: 500;
    margin-bottom: 8px;
    font-family: var(--app-font-family);
    display: block;
    text-align: left;
  }

  :deep(.t-input) {
    border: 1px solid var(--td-component-stroke);
    border-radius: 10px;
    background: var(--td-bg-color-container);
    transition: all 0.2s;

    &:focus-within {
      border-color: var(--td-brand-color);
      box-shadow: 0 0 0 3px var(--td-brand-color-focus, rgba(51, 51, 51, 0.08));
    }

    &:hover {
      border-color: var(--td-brand-color);
    }

    .t-input__inner {
      border: none !important;
      box-shadow: none !important;
      outline: none !important;
      background: transparent;
      font-size: 15px;
      font-family: var(--app-font-family);

      &:focus {
        border: none !important;
        box-shadow: none !important;
        outline: none !important;
      }
    }

    .t-input__wrap {
      border: none !important;
      box-shadow: none !important;
    }
  }

  :deep(.t-form-item) {
    margin-bottom: 18px;

    &:last-child {
      margin-bottom: 0;
    }
  }

  :deep(.t-form-item__control) {
    width: 100%;
  }
}

/* ---------- P0-4：通道式注册 / 人机验证 ---------- */
.channel-tabs {
  display: flex;
  gap: 6px;
  padding: 4px;
  margin-bottom: 20px;
  border-radius: 10px;
  background: var(--td-bg-color-secondarycontainer);
  border: 1px solid var(--td-component-stroke);
}

.channel-tab {
  flex: 1;
  height: 38px;
  border: none;
  border-radius: 8px;
  background: transparent;
  color: var(--td-text-color-secondary);
  font-size: 14px;
  font-weight: 500;
  font-family: var(--app-font-family);
  cursor: pointer;
  transition: all 0.2s;

  &:hover {
    color: var(--td-text-color-primary);
  }

  &.is-active {
    background: var(--td-bg-color-container);
    color: var(--td-brand-color);
    box-shadow: var(--td-shadow-1);
  }
}

.captcha-field {
  margin-bottom: 18px;

  &__label {
    display: block;
    font-size: 14px;
    color: var(--td-text-color-primary);
    font-weight: 500;
    margin-bottom: 8px;
    font-family: var(--app-font-family);
    text-align: left;
  }
}

.code-input-row {
  display: flex;
  gap: 10px;
  width: 100%;

  :deep(.t-input) {
    flex: 1;
    min-width: 0;
  }
}

.send-code-button {
  flex-shrink: 0;
  height: 40px;
  border-radius: 10px;
  font-size: 13px;
  font-weight: 500;
  padding: 0 14px;
}

.password-strength {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-top: 8px;

  &__bars {
    display: flex;
    gap: 4px;
    flex: 1;
  }

  &__label {
    font-size: 12px;
    flex-shrink: 0;

    &.is-level-1 { color: var(--td-error-color, #d54941); }
    &.is-level-2 { color: var(--td-warning-color, #e37318); }
    &.is-level-3 { color: var(--td-success-color, #2ba471); }
  }
}

.strength-bar {
  height: 4px;
  flex: 1;
  border-radius: 2px;
  background: var(--td-bg-color-secondarycontainer);
  transition: background 0.25s;

  &.is-weak { background: var(--td-error-color, #d54941); }
  &.is-medium { background: var(--td-warning-color, #e37318); }
  &.is-strong { background: var(--td-success-color, #2ba471); }
}

.submit-button {
  height: 46px;
  border-radius: 10px;
  font-size: 16px;
  font-weight: 500;
  font-family: var(--app-font-family);
  margin: 20px 0 16px 0;
}

.oidc-divider {
  position: relative;
  margin: 4px 0 6px;
  text-align: center;
  color: var(--td-text-color-placeholder);
  font-size: 12px;

  span {
    position: relative;
    z-index: 1;
    padding: 0 12px;
    background: var(--td-bg-color-container);
  }

  &::before {
    content: '';
    position: absolute;
    left: 0;
    right: 0;
    top: 50%;
    border-top: 1px solid var(--td-component-stroke);
  }
}

.oidc-button {
  height: 46px;
  border-radius: 10px;
  font-size: 15px;
  font-weight: 500;
}

.form-footer {
  text-align: center;
  font-size: 14px;
  color: var(--td-text-color-secondary);
  font-family: var(--app-font-family);
  margin-top: 16px;

  .link-button {
    color: var(--td-brand-color);
    text-decoration: none;
    margin-left: 4px;
    font-weight: 500;
    transition: all 0.2s;

    &:hover {
      color: var(--td-brand-color);
      text-decoration: underline;
    }
  }
}

/* ---------- 顶部语言切换 ---------- */
.header-links {
  position: fixed;
  top: 24px;
  right: 24px;
  display: flex;
  align-items: center;
  gap: 10px;
  z-index: 100;
}

.language-switch {
  position: relative;

  .lang-button {
    display: flex;
    align-items: center;
    gap: 7px;
    padding: 8px 14px;
    border-radius: 20px;
    background: var(--td-bg-color-container);
    border: 1px solid var(--td-component-stroke);
    color: var(--td-text-color-primary);
    font-size: 13px;
    font-weight: 500;
    font-family: var(--app-font-family);
    cursor: pointer;

    .lang-flag-icon {
      font-size: 16px;
      line-height: 1;
      flex-shrink: 0;
    }

    &:hover {
      background: var(--td-bg-color-secondarycontainer);
    }

    svg:last-child {
      margin-left: 2px;
      flex-shrink: 0;
    }
  }
}

.language-dropdown {
  position: absolute;
  top: calc(100% + 8px);
  right: 0;
  min-width: 160px;
  background: var(--td-bg-color-container);
  border: 1px solid var(--td-component-stroke);
  border-radius: 10px;
  box-shadow: var(--td-shadow-2);
  overflow: hidden;
  z-index: 1000;
}

.language-option {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px 14px;
  cursor: pointer;
  font-size: 13px;
  font-family: var(--app-font-family);
  color: var(--td-text-color-primary);

  .lang-flag {
    font-size: 16px;
    flex-shrink: 0;
  }

  .lang-label {
    flex: 1;
  }

  .check-icon {
    color: var(--td-brand-color);
    font-weight: 700;
    font-size: 14px;
    flex-shrink: 0;
  }

  &:hover {
    background: var(--td-bg-color-secondarycontainer);
  }

  &.active {
    background: var(--td-bg-color-secondarycontainer);
    color: var(--td-brand-color-active);
  }
}

/* ---------- 响应式 ---------- */
@media (max-width: 480px) {
  .login-layout {
    padding: 16px;
  }

  .header-links {
    top: 16px;
    right: 16px;
  }

  .form-card {
    padding: 28px 20px;
  }

  .form-header {
    margin-bottom: 24px;
  }
}
</style>
