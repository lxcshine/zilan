/**
 * 账号标识符与密码强度校验（P0-4 双通道注册登录）。
 *
 * 与后端 internal/types/captcha.go 保持逐条镜像——正则和策略修改时
 * 两处必须同步，否则前端放行、后端拦截会造成“提交后才报错”的
 * 糟糕体验；反之则是前端过度拦截。这里不做任何后端已存在的策略
 * 之外的“前端自创规则”。
 */

// 中国大陆手机号：11 位，1 开头，第二位 3-9。
export const MAINLAND_CHINA_MOBILE_REGEX = /^1[3-9]\d{9}$/

// RFC-lite 邮箱格式（与后端 EmailRegex 一致，不做完整 RFC 5322）。
export const EMAIL_REGEX = /^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$/

// 密码允许的字符集（可打印 ASCII 常用子集，与后端 passwordAllowed 一致）。
const PASSWORD_ALLOWED_REGEX = /^[A-Za-z0-9!@#$%^&*()_+\-=[\]{};':"\\|,.<>/?~`]{8,32}$/

export type IdentifierKind = 'phone' | 'email'

export function isMainlandChinaMobile(value: string): boolean {
  return MAINLAND_CHINA_MOBILE_REGEX.test((value || '').trim())
}

export function isEmailFormat(value: string): boolean {
  return EMAIL_REGEX.test((value || '').trim())
}

/**
 * 自动识别输入内容是手机号还是邮箱。
 * 返回 null 表示两者都不是（格式错误）。
 */
export function detectIdentifier(value: string): IdentifierKind | null {
  const v = (value || '').trim()
  if (!v) return null
  if (isMainlandChinaMobile(v)) return 'phone'
  if (isEmailFormat(v)) return 'email'
  return null
}

/**
 * P0-4 密码策略：8-32 位，必须同时包含大写字母、小写字母和数字。
 * 仅用于新注册 / 改密码；存量用户登录不受此限制。
 */
export function validatePasswordStrength(password: string): boolean {
  const pw = password || ''
  if (!PASSWORD_ALLOWED_REGEX.test(pw)) return false
  return /[a-z]/.test(pw) && /[A-Z]/.test(pw) && /\d/.test(pw)
}

export type PasswordStrengthLevel = 0 | 1 | 2 | 3

/**
 * 密码强度评分（仅用于注册表单的实时指示器，不是拦截规则）：
 *   0 无输入；1 弱；2 中；3 强。
 * 未满足 P0-4 硬性策略（大小写 + 数字 + 8 位）之前一律显示“弱”，
 * 引导用户补齐要求；满足后按长度/符号加分。
 */
export function passwordStrengthLevel(password: string): PasswordStrengthLevel {
  const pw = password || ''
  if (!pw) return 0
  const hasLower = /[a-z]/.test(pw)
  const hasUpper = /[A-Z]/.test(pw)
  const hasDigit = /\d/.test(pw)
  const hasSymbol = /[^A-Za-z0-9]/.test(pw)
  if (!(hasLower && hasUpper && hasDigit && pw.length >= 8)) return 1
  if (pw.length >= 12 || hasSymbol) return 3
  return 2
}
