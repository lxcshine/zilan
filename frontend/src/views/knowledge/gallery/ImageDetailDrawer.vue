<script setup lang="ts">
// FR-18 单图详情：来源元数据 + 原文位置（复用 FR-05）。
// - 元数据：来源文档（可跳文档详情）、所属 chunk 类型、说明/OCR、时间戳。
// - 原文位置：与聊天引用相同的 chunk-by-id 接口拉取所属 chunk，按
//   image_info 里的 start_pos/end_pos（FR-05 的精确定位数据）把 chunk 原文
//   中该图片的引用片段高亮出来；定位不到时按 URL 兜底搜索，仍找不到则
//   展示 chunk 开头（图片类 chunk 本身就是图片，说明其内容即 OCR/说明文本）。
import { ref, computed, watch, nextTick, onBeforeUnmount } from 'vue';
import { useI18n } from 'vue-i18n';
import { getChunkByIdOnly } from '@/api/knowledge-base';
import { hydrateProtectedFileImages } from '@/utils/security';
import type { ImageAsset } from '@/api/image-gallery';

const props = defineProps<{ asset: ImageAsset; kbId: string }>();
const emit = defineEmits<{
  (e: 'close'): void;
  (e: 'open-document', knowledgeId: string): void;
}>();
const { t } = useI18n();

const PLACEHOLDER = 'data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///ywAAAAAAQABAAACAUwAOw==';
const isProviderScheme = (url: string) => /^(resource|local|minio|cos|tos|s3|oss|ks3|obs):\/\//i.test(url);

const dialogVisible = ref(true);
const imageRef = ref<HTMLElement | null>(null);

// 弹窗标题：优先图片说明，其次来源文档名，最后"未命名图片"。
const cardTitleText = computed(() => props.asset.caption || props.asset.source_name || t('imageGallery.untitled'));

// FR-05 原文位置状态。
interface SourceExcerpt {
  before: string;
  hit: string;
  after: string;
  kind: 'position' | 'url' | 'head';
}
const sourceLoading = ref(false);
const sourceFailed = ref(false);
const sourceExcerpt = ref<SourceExcerpt | null>(null);

const chunkTypeLabel = computed(() => {
  const map: Record<string, string> = {
    text: t('imageGallery.chunkTypeText'),
    image_ocr: t('imageGallery.chunkTypeImageOCR'),
    image_caption: t('imageGallery.chunkTypeImageCaption'),
  };
  return props.asset.chunk_type ? (map[props.asset.chunk_type] || props.asset.chunk_type) : t('imageGallery.chunkTypeText');
});

// 按 asset.id 的 "<chunkID>#<index>" 解析数组下标。
const assetIndex = computed(() => {
  const idx = Number(props.asset.id.split('#')[1]);
  return Number.isInteger(idx) && idx >= 0 ? idx : -1;
});

function excerptFromPosition(content: string, start: number, end: number, kind: SourceExcerpt['kind']): SourceExcerpt | null {
  if (!Number.isInteger(start) || !Number.isInteger(end) || start < 0 || end <= start || end > content.length) return null;
  return { before: content.slice(Math.max(0, start - 160), start), hit: content.slice(start, end), after: content.slice(end, start + 320), kind };
}

async function loadSourceExcerpt() {
  sourceLoading.value = true;
  sourceFailed.value = false;
  sourceExcerpt.value = null;
  try {
    const res: any = await getChunkByIdOnly(props.asset.chunk_id);
    const chunk = res?.data || res;
    const content: string = typeof chunk?.content === 'string' ? chunk.content : '';
    if (!content) {
      sourceExcerpt.value = null;
      sourceLoading.value = false;
      return;
    }
    // image_info 是 JSON 字符串（types.Chunk.ImageInfo），空串代表无图片信息。
    let infos: any[] = [];
    try {
      if (chunk?.image_info) infos = JSON.parse(chunk.image_info);
      if (!Array.isArray(infos)) infos = [];
    } catch (_) { /* 非法 JSON 时按无 image_info 处理 */ }

    // 1) 首选 FR-05 的 start_pos/end_pos（数组下标优先，其次 URL 匹配）。
    const byIndex = assetIndex.value >= 0 ? infos[assetIndex.value] : null;
    const candidates = [];
    if (byIndex) candidates.push(byIndex);
    for (const info of infos) {
      if (info && (info.url === props.asset.url || info.original_url === props.asset.url)) candidates.push(info);
    }
    for (const info of candidates) {
      if (!info) continue;
      const cut = excerptFromPosition(content, Number(info.start_pos), Number(info.end_pos), 'position');
      if (cut) {
        sourceExcerpt.value = cut;
        sourceLoading.value = false;
        return;
      }
    }
    // 2) 兜底：在 chunk 原文中按 URL 搜索图片引用片段。
    const needle = props.asset.url || props.asset.original_url;
    if (needle) {
      const at = content.indexOf(needle);
      if (at >= 0) {
        // 图片 markdown 引用一般以 "![caption](" 开始，")" 结束。
        let start = content.lastIndexOf('![', at);
        start = start >= 0 ? start : at;
        let end = content.indexOf(')', at + needle.length);
        end = end >= 0 ? end + 1 : at + needle.length;
        sourceExcerpt.value = excerptFromPosition(content, start, end, 'url');
      }
    }
    // 3) 仍无定位：展示 chunk 开头（至少给出原文上下文）。
    if (!sourceExcerpt.value && content.trim()) {
      sourceExcerpt.value = { before: '', hit: content.slice(0, 480), after: '', kind: 'head' };
    }
  } catch (err: unknown) {
    sourceFailed.value = true;
    console.warn('[ImageDetailDrawer] load chunk failed:', err);
  } finally {
    sourceLoading.value = false;
  }
}

async function hydrateImage() {
  await nextTick();
  if (imageRef.value) await hydrateProtectedFileImages(imageRef.value, undefined, props.kbId);
}

watch(() => props.asset.id, () => {
  loadSourceExcerpt();
  hydrateImage();
}, { immediate: true });

function openDocument() {
  if (props.asset.knowledge_id) emit('open-document', props.asset.knowledge_id);
}

function formatTime(v: string) {
  if (!v) return '-';
  try {
    return new Date(v).toLocaleString();
  } catch (_) {
    return v;
  }
}

onBeforeUnmount(() => {
  dialogVisible.value = false;
});
</script>

<template>
  <t-dialog v-model:visible="dialogVisible" :footer="false" width="860px"
    placement="center" destroy-on-close @closed="emit('close')">
    <template #header>
      <div class="detail-title">{{ cardTitleText }}</div>
    </template>
    <div class="detail-body">
      <div ref="imageRef" class="detail-figure">
        <img class="detail-img" :src="isProviderScheme(asset.url) ? PLACEHOLDER : (asset.url || asset.original_url)"
          :data-protected-src="isProviderScheme(asset.url) ? asset.url : null" :alt="asset.caption || 'image'" />
      </div>
      <div class="detail-meta">
        <dl class="detail-fields">
          <div class="detail-row">
            <dt>{{ $t('imageGallery.sourceDoc') }}</dt>
            <dd>
              <t-link v-if="asset.knowledge_id" hover="color" @click="openDocument">
                {{ asset.source_name || asset.knowledge_id }}
              </t-link>
              <span v-else>-</span>
            </dd>
          </div>
          <div class="detail-row">
            <dt>{{ $t('imageGallery.chunkType') }}</dt>
            <dd>{{ chunkTypeLabel }}</dd>
          </div>
          <div v-if="asset.caption" class="detail-row">
            <dt>{{ $t('imageGallery.caption') }}</dt>
            <dd>{{ asset.caption }}</dd>
          </div>
          <div v-if="asset.ocr_text" class="detail-row">
            <dt>{{ $t('imageGallery.ocr') }}</dt>
            <dd class="detail-ocr">{{ asset.ocr_text }}</dd>
          </div>
          <div class="detail-row">
            <dt>{{ $t('imageGallery.createdAt') }}</dt>
            <dd>{{ formatTime(asset.created_at) }}</dd>
          </div>
          <div class="detail-row">
            <dt>{{ $t('imageGallery.updatedAt') }}</dt>
            <dd>{{ formatTime(asset.updated_at) }}</dd>
          </div>
          <div v-if="!asset.is_enabled" class="detail-row">
            <dt>{{ $t('imageGallery.status') }}</dt>
            <dd>{{ $t('imageGallery.disabled') }}</dd>
          </div>
        </dl>

        <div class="detail-source">
          <div class="detail-source-head">
            <span>{{ $t('imageGallery.sourceExcerpt') }}</span>
            <t-button size="small" variant="text" theme="primary" @click="openDocument">
              {{ $t('imageGallery.openDocument') }}
              <template #icon><t-icon name="arrow-right" /></template>
            </t-button>
          </div>
          <div class="detail-source-body">
            <div v-if="sourceLoading" class="detail-source-loading">
              <t-loading size="small" /> {{ $t('imageGallery.loading') }}
            </div>
            <div v-else-if="sourceFailed" class="detail-source-failed">{{ $t('imageGallery.sourceLoadFailed') }}</div>
            <div v-else-if="!sourceExcerpt" class="detail-source-failed">{{ $t('imageGallery.sourceUnavailable') }}</div>
            <!-- FR-05：高亮 chunk 原文中该图片的引用片段 -->
            <div v-else class="detail-source-text">
              <span v-if="sourceExcerpt.before">…{{ sourceExcerpt.before }}</span>
              <mark class="detail-source-hit">{{ sourceExcerpt.hit }}</mark>
              <span v-if="sourceExcerpt.after">{{ sourceExcerpt.after }}…</span>
            </div>
          </div>
        </div>
      </div>
    </div>
  </t-dialog>
</template>

<style scoped>
.detail-title {
  font-size: 15px;
  font-weight: 600;
}

.detail-body {
  display: flex;
  gap: 20px;
  align-items: flex-start;
}

.detail-figure {
  flex: 0 0 52%;
  max-width: 52%;
  border-radius: 8px;
  overflow: hidden;
  background: var(--td-bg-color-secondarycontainer);
  display: flex;
  align-items: center;
  justify-content: center;
  min-height: 200px;
}

.detail-img {
  max-width: 100%;
  max-height: 420px;
  object-fit: contain;
}

.detail-meta {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.detail-fields {
  margin: 0;
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.detail-row {
  display: flex;
  gap: 8px;
  font-size: 13px;
  line-height: 1.6;
}

.detail-row dt {
  flex: 0 0 84px;
  color: var(--td-text-color-placeholder);
}

.detail-row dd {
  margin: 0;
  color: var(--td-text-color-primary);
  word-break: break-all;
}

.detail-ocr {
  max-height: 120px;
  overflow: auto;
}

.detail-source {
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
  overflow: hidden;
}

.detail-source-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 8px 12px;
  font-size: 13px;
  font-weight: 600;
  background: var(--td-bg-color-secondarycontainer);
}

.detail-source-body {
  padding: 12px;
  font-size: 13px;
}

.detail-source-loading,
.detail-source-failed {
  display: flex;
  align-items: center;
  gap: 8px;
  color: var(--td-text-color-placeholder);
}

.detail-source-text {
  line-height: 1.7;
  word-break: break-all;
  max-height: 200px;
  overflow: auto;
}

.detail-source-hit {
  background: var(--td-warning-color-1);
  color: var(--td-warning-color);
  border-radius: 3px;
  padding: 0 2px;
}
</style>
