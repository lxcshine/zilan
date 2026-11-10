// FR-18 文档图片画廊 API。
// 对应后端 internal/handler/knowledge_images.go：
//   GET  /api/v1/knowledge-bases/:id/images   查询串过滤
//   POST /api/v1/knowledge-bases/:id/images   JSON 过滤（多来源文档组合时用，
//                                            避免 GET 查询串超长）
// 分页参数（page/page_size）两种方式都走查询串，与后端 ShouldBindQuery 一致。
import { get, post } from '../../utils/request';

// 单张图片资产（后端 types.ImageAsset）。
export interface ImageAsset {
  id: string
  chunk_id: string
  knowledge_id: string
  source_name: string
  chunk_type: string
  url: string
  original_url: string
  caption: string
  ocr_text: string
  attrs: Record<string, unknown> | null
  is_enabled: boolean
  status: number
  created_at: string
  updated_at: string
}

export interface ListKBImagesParams {
  page?: number
  page_size?: number
  keyword?: string
  sort_by?: 'created_at' | 'updated_at' | 'caption'
  sort_order?: 'asc' | 'desc'
  knowledge_ids?: string[]
  is_enabled?: boolean
}

export interface ListKBImagesResponse {
  data: ImageAsset[]
  total: number
  page: number
  page_size: number
}

function buildPagination(params: ListKBImagesParams): URLSearchParams {
  const query = new URLSearchParams()
  query.set('page', String(params.page ?? 1))
  query.set('page_size', String(params.page_size ?? 40))
  return query
}

// 列出知识库图片（每图去重）。knowledge_ids 非空时走 POST（后端 GET 逗号
// 分隔有 50 上限且 URL 易超长；POST 数组上限 1000）。
export async function listKBImages(kbId: string, params: ListKBImagesParams = {}): Promise<ListKBImagesResponse> {
  const query = buildPagination(params)
  const hasKnowledgeFilter = (params.knowledge_ids?.length ?? 0) > 0
  if (hasKnowledgeFilter) {
    return (await post(
      `/api/v1/knowledge-bases/${encodeURIComponent(kbId)}/images?${query.toString()}`,
      {
        keyword: params.keyword || undefined,
        sort_by: params.sort_by,
        sort_order: params.sort_order,
        knowledge_ids: params.knowledge_ids,
        is_enabled: params.is_enabled,
      },
    )) as unknown as ListKBImagesResponse
  }
  if (params.keyword) query.set('keyword', params.keyword)
  if (params.sort_by) query.set('sort_by', params.sort_by)
  if (params.sort_order) query.set('sort_order', params.sort_order)
  if (params.is_enabled !== undefined) query.set('is_enabled', String(params.is_enabled))
  return (await get(
    `/api/v1/knowledge-bases/${encodeURIComponent(kbId)}/images?${query.toString()}`,
  )) as unknown as ListKBImagesResponse
}
