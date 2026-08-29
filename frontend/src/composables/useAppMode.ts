import type { RouteLocationNormalizedLoaded } from 'vue-router'
import type { AppWorkMode } from '@/stores/ui'

/**
 * 三栏布局的模式定义与路由映射（PRD ui-layout-visual-redesign §3.4 / §4）。
 * Rail 与列表栏共用同一份定义，保证高亮与面板内容一致。
 */
export interface RailModeDefinition {
  mode: AppWorkMode
  /** i18n key（rail 标题 tooltip） */
  labelKey: string
  /** NewUserGuide 的 data-guide 锚点沿用旧侧栏命名，保证新手引导不回退 */
  guide: string
  /** 模式默认落地路由 */
  path: string
}

export const RAIL_MODES: RailModeDefinition[] = [
  { mode: 'chat', labelKey: 'rail.chat', guide: 'nav-creatChat', path: '/platform/creatChat' },
  { mode: 'knowledge', labelKey: 'rail.knowledgeBases', guide: 'nav-knowledge-bases', path: '/platform/knowledge-bases' },
  { mode: 'documents', labelKey: 'rail.documents', guide: 'nav-documents', path: '/platform/documents' },
  { mode: 'agents', labelKey: 'rail.agents', guide: 'nav-agents', path: '/platform/agents' },
  { mode: 'memory', labelKey: 'rail.memory', guide: 'nav-memory', path: '/platform/memory' },
]

/** 路由名 → 工作模式；settings 等非模式路由返回 null（保持上一次模式高亮） */
export function resolveModeFromRoute(route: RouteLocationNormalizedLoaded | { name?: unknown }): AppWorkMode | null {
  const name = typeof route.name === 'string' ? route.name : route.name ? String(route.name) : ''
  switch (name) {
    case 'chat':
    case 'globalCreatChat':
    case 'kbCreatChat':
      return 'chat'
    case 'knowledgeBaseList':
    case 'knowledgeBaseDetail':
    case 'knowledgeBaseSettings':
      return 'knowledge'
    case 'documentList':
    case 'documentDetail':
      return 'documents'
    case 'agentList':
      return 'agents'
    case 'memoryList':
      return 'memory'
    default:
      return null
  }
}
