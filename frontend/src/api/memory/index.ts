// 我的记忆 — 用户级长期记忆管理 API。
// 后端契约见 internal/handler/memory.go 与 internal/types/memory.go：
// 所有操作按调用用户的 (tenant, user) 隔离，前端无需也无法跨用户访问。
import { get, put, del } from '@/utils/request'

/** 记忆分类：画像 / 事实 / 偏好 / 待办 / 反馈 / 风格指令(soul) / 助手技巧(skill) */
export type MemoryCategory =
  | 'profile'
  | 'fact'
  | 'preference'
  | 'todo'
  | 'feedback'
  | 'soul'
  | 'skill'

/** 记忆状态：生效中 / 已完成（todo）/ 已归档 */
export type MemoryStatus = 'active' | 'done' | 'archived'

/**
 * 常驻注入类别（P0-3）：soul 风格指令 / profile 画像 / preference 偏好 / skill 技巧
 * 无条件注入每次对话，不依赖语义相关性；fact / todo / 反馈走语义评分召回。
 * 镜像后端 internal/types/memory.go 的 IsResidentMemoryCategory（单一事实源在后端）。
 */
export const RESIDENT_MEMORY_CATEGORIES: ReadonlySet<MemoryCategory> = new Set([
  'soul',
  'profile',
  'preference',
  'skill',
])

/** 判断某条记忆是否常驻注入（仅 active 状态参与注入） */
export function isResidentMemory(fact: Pick<MemoryFact, 'category' | 'status'>): boolean {
  return fact.status === 'active' && RESIDENT_MEMORY_CATEGORIES.has(fact.category)
}

export interface MemoryFact {
  id: string
  category: MemoryCategory
  /** 人类可读主文本（抽取三元组的规范化渲染），同时是 embedding 输入 */
  content: string
  /** 三元组 object；编辑时可通过 PUT 修改 */
  object: string
  confidence: number
  importance: number
  status: MemoryStatus
  access_count: number
  last_accessed_at?: string
  /** 截止日期，仅 todo 分类 */
  due_at?: string
  created_at: string
  updated_at: string
}

export interface MemoryFactListParams {
  category?: MemoryCategory | ''
  status?: 'active' | 'done' | 'archived' | 'all' | ''
  keyword?: string
  page?: number
  page_size?: number
}

export interface MemoryFactListData {
  items: MemoryFact[]
  total: number
  page: number
  page_size: number
}

export interface MemoryStatusData {
  enabled: boolean
  fact_count: number
}

/** 记忆功能状态（开关 + 总条数） */
export async function getMemoryStatus(): Promise<MemoryStatusData> {
  const res = await get('/api/v1/memory/status') as any
  return res?.data ?? { enabled: true, fact_count: 0 }
}

/** 分页列出长期记忆；status 缺省时后端只返回 active */
export async function listMemoryFacts(params: MemoryFactListParams = {}): Promise<MemoryFactListData> {
  const query = new URLSearchParams()
  if (params.category) query.set('category', params.category)
  if (params.status) query.set('status', params.status)
  if (params.keyword) query.set('keyword', params.keyword)
  if (params.page) query.set('page', String(params.page))
  if (params.page_size) query.set('page_size', String(params.page_size))
  const qs = query.toString()
  const res = await get(`/api/v1/memory/facts${qs ? `?${qs}` : ''}`) as any
  return (
    res?.data ?? {
      items: [],
      total: 0,
      page: params.page ?? 1,
      page_size: params.page_size ?? 20,
    }
  )
}

export interface UpdateMemoryFactBody {
  content?: string
  object?: string
  status?: MemoryStatus
  importance?: number
  /** ISO 日期（YYYY-MM-DD）或 RFC3339；分类不可改，由后端保持原值 */
  due_at?: string
}

/** 编辑一条记忆；内容变更后后端自动重建语义向量 */
export async function updateMemoryFact(id: string, body: UpdateMemoryFactBody): Promise<void> {
  await put(`/api/v1/memory/facts/${id}`, body)
}

/** 删除一条记忆（软删除，召回立即失效） */
export async function deleteMemoryFact(id: string): Promise<void> {
  await del(`/api/v1/memory/facts/${id}`)
}

/** 清空全部记忆（GDPR 遗忘权；同时清除会话滚动摘要，不可恢复） */
export async function deleteAllMemories(): Promise<number> {
  const res = await del('/api/v1/memory') as any
  return res?.data?.deleted ?? 0
}

// ---------------------------------------------------------------------------
// 四模块聚合 API（P0-2：Soul / User / Memory / Agent）
// ---------------------------------------------------------------------------

/** 记忆模块键：灵魂 / 用户档案 / 记忆流 / 经验技巧 */
export type MemoryModule = 'soul' | 'user' | 'memory' | 'agent'

export interface MemoryModuleOverview {
  module: MemoryModule
  fact_count: number
  /** 仅 memory 模块返回：L2 会话摘要条数 */
  summary_count?: number
}

/** 全局人设（只读，来自系统提示词模板；未配置时为空对象） */
export interface SoulPersona {
  name?: string
  description?: string
  content?: string
}

export interface SoulCardData {
  global_persona: SoulPersona
  /** 用户的风格微调指令（category=soul） */
  adjustments: MemoryFact[]
}

/** 档案分组键：身份 / 职责 / 偏好 / 事实 */
export type MemoryProfileSectionKey = 'identity' | 'role' | 'preference' | 'fact'

export interface MemoryProfileSection {
  key: MemoryProfileSectionKey
  items: MemoryFact[]
}

export interface ProfileCardData {
  sections: MemoryProfileSection[]
  /** 0-1，非空分组加权占比（identity/role 权重加倍） */
  completeness: number
}

/** 反馈墙条目：原始反馈 + 升级关联的技巧 ID（同轮抽取产出） */
export interface AgentFeedbackItem extends MemoryFact {
  upgraded_to?: string
}

export interface AgentTipsCardData {
  skills: MemoryFact[]
  feedback: AgentFeedbackItem[]
  feedback_total: number
}

/** 四模块总览（各模块记忆计数，memory 模块含摘要数） */
export async function getMemoryModules(): Promise<MemoryModuleOverview[]> {
  const res = await get('/api/v1/memory/modules') as any
  return res?.data?.modules ?? []
}

/** Soul 灵魂卡：全局人设（只读）+ 用户风格微调指令 */
export async function getSoulCard(): Promise<SoulCardData> {
  const res = await get('/api/v1/memory/soul') as any
  return res?.data ?? { global_persona: {}, adjustments: [] }
}

/** User 用户档案卡：身份/职责/偏好/事实分组 + 完整度 */
export async function getProfileCard(): Promise<ProfileCardData> {
  const res = await get('/api/v1/memory/profile') as any
  return res?.data ?? { sections: [], completeness: 0 }
}

/** Agent 经验技巧卡：技巧列表 + 反馈墙（带升级关联）；反馈支持分页 */
export async function getAgentTips(params: { page?: number; page_size?: number } = {}): Promise<AgentTipsCardData> {
  const query = new URLSearchParams()
  if (params.page) query.set('page', String(params.page))
  if (params.page_size) query.set('page_size', String(params.page_size))
  const qs = query.toString()
  const res = await get(`/api/v1/memory/agent-tips${qs ? `?${qs}` : ''}`) as any
  return res?.data ?? { skills: [], feedback: [], feedback_total: 0 }
}
