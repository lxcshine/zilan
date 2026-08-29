import { ref } from 'vue'
import { listKnowledgeBases, listKnowledgeFiles } from '@/api/knowledge-base'

/**
 * 全局文档聚合（PRD ui-layout-visual-redesign §5.3「文档」模式）。
 *
 * 现有文档接口按知识库维度查询，暂无跨库聚合端点（PRD §14 已记录该前提），
 * 首期采用前端按库聚合：拉取知识库列表 → 每库拉一页文档 → 合并归一化。
 * 模块级共享状态，Rail 列表面板与文档画布复用同一份数据。
 */
export interface GlobalDocumentItem {
  id: string
  kbId: string
  kbName: string
  title: string
  file_type: string
  source: string
  parse_status: string
  created_at: string
  updated_at: string
}

const documents = ref<GlobalDocumentItem[]>([])
const loading = ref(false)
const loaded = ref(false)
let inFlight: Promise<void> | null = null

/** 单库最多取一页，控制聚合请求总量；库数量本身由后端分页保证有限 */
const PER_KB_PAGE_SIZE = 50

function normalizeRow(row: any, kbId: string, kbName: string): GlobalDocumentItem {
  return {
    id: String(row.id ?? ''),
    kbId,
    kbName,
    title: row.title || row.file_name || row.name || '',
    file_type: row.file_type || row.source_type || '',
    source: row.source || '',
    parse_status: row.parse_status || '',
    created_at: row.created_at || '',
    updated_at: row.updated_at || row.created_at || '',
  }
}

async function fetchAll(): Promise<void> {
  loading.value = true
  try {
    const kbRes: any = await listKnowledgeBases()
    const kbs: any[] = kbRes?.data || []
    const pages = await Promise.all(
      kbs.map(async (kb) => {
        try {
          const res: any = await listKnowledgeFiles(String(kb.id), {
            page: 1,
            page_size: PER_KB_PAGE_SIZE,
          })
          const rows: any[] = res?.data || []
          return rows.map((row) => normalizeRow(row, String(kb.id), kb.name || ''))
        } catch {
          // 单库失败不阻塞整体聚合（共享库权限缺失等场景）
          return [] as GlobalDocumentItem[]
        }
      })
    )
    documents.value = pages
      .flat()
      .sort((a, b) => (a.updated_at < b.updated_at ? 1 : -1))
    loaded.value = true
  } finally {
    loading.value = false
  }
}

export function useGlobalDocuments() {
  const refresh = async (): Promise<void> => {
    if (inFlight) return inFlight
    inFlight = fetchAll().finally(() => {
      inFlight = null
    })
    return inFlight
  }

  const ensure = async (): Promise<void> => {
    if (!loaded.value && !loading.value) {
      await refresh()
    }
  }

  return { documents, loading, loaded, refresh, ensure }
}

/** 解析状态 → 简化分组（用于状态筛选 chips 与概览统计） */
export function docStatusGroup(parseStatus: string): 'processing' | 'completed' | 'failed' {
  const s = (parseStatus || '').toLowerCase()
  if (['completed', 'complete', 'done', 'success'].includes(s)) return 'completed'
  if (['failed', 'error', 'timeout'].includes(s)) return 'failed'
  // pending / processing / parsing 等一切中间态
  return 'processing'
}
