// 我的记忆 — 用户级长期记忆管理 API。
// 后端契约见 internal/handler/memory.go 与 internal/types/memory.go：
// 所有操作按调用用户的 (tenant, user) 隔离，前端无需也无法跨用户访问。
import { get, put, del } from '@/utils/request'

/** 记忆分类：画像 / 事实 / 偏好 / 待办 / 反馈 */
export type MemoryCategory = 'profile' | 'fact' | 'preference' | 'todo' | 'feedback'

/** 记忆状态：生效中 / 已完成（todo）/ 已归档 */
export type MemoryStatus = 'active' | 'done' | 'archived'

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
