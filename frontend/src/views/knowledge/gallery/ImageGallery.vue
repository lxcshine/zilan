<script setup lang="ts">
// FR-18 文档图片画廊：瀑布流无限滚动主视图。
// 对齐上游 WeKnora-main frontend/src/views/knowledge/gallery/ImageGallery.vue
// 的交互形态（瀑布流 + caption/OCR 关键词搜索 + 来源过滤 + 单图详情），
// 但 Zilan 暂无属性观测管线（上游的画廊契约面板不适用）：
// - 属性过滤保留"来源文档"多选；"尺寸/类型"因 Zilan 的 image_info 不携带
//   width/height/format 而按 PRD 优雅降级为不展示（不报错）。
// - 图片加载复用 security.ts 的 KB 域鉴权代理（data-protected-src + 水合）。
// - "跳转原文位置"复用 FR-05：单图详情内按 image_info 的 start/end 偏移
//   高亮所属 chunk 的原文片段（见 ImageDetailDrawer.vue）。
import { ref, computed, watch, onMounted, onBeforeUnmount, nextTick } from 'vue';
import { useI18n } from 'vue-i18n';
import { listKBImages, type ImageAsset } from '@/api/image-gallery';
import { listKnowledgeFiles } from '@/api/knowledge-base';
import { hydrateProtectedFileImages } from '@/utils/security';

const props = defineProps<{ kbId: string }>();
const emit = defineEmits<{ (e: 'open-source-doc', knowledgeId: string): void }>();
const { t } = useI18n();

const PAGE_SIZE = 40;
// 透明 1px 占位：provider 协议图片由 hydrateProtectedFileImages 换成 blob。
const PLACEHOLDER = 'data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///ywAAAAAAQABAAACAUwAOw==';

const items = ref<ImageAsset[]>([]);
const total = ref(0);
const page = ref(1);
const loading = ref(false);
const loadingDocs = ref(false);
const errorTip = ref('');
const noMore = computed(() => page.value * PAGE_SIZE >= total.value && total.value >= 0 && items.value.length >= total.value);

const keyword = ref('');
const sortKey = ref<'newest' | 'oldest' | 'caption'>('newest');
const selectedDocIds = ref<string[]>([]);
const enabledOnly = ref(false);

interface DocOption { label: string; value: string }
const docOptions = ref<DocOption[]>([]);

const sortParams = computed(() => {
  if (sortKey.value === 'caption') return { sort_by: 'caption' as const, sort_order: 'asc' as const };
  if (sortKey.value === 'oldest') return { sort_by: 'created_at' as const, sort_order: 'asc' as const };
  return { sort_by: 'created_at' as const, sort_order: 'desc' as const };
});

const isProviderScheme = (url: string) => /^(resource|local|minio|cos|tos|s3|oss|ks3|obs):\/\//i.test(url);
const cardText = (a: ImageAsset) => a.caption || a.ocr_text || t('imageGallery.untitled');

async function fetchPage(target: number, append: boolean) {
  if (!props.kbId || loading.value) return;
  loading.value = true;
  errorTip.value = '';
  try {
    const res = await listKBImages(props.kbId, {
      page: target,
      page_size: PAGE_SIZE,
      keyword: keyword.value || undefined,
      knowledge_ids: selectedDocIds.value.length ? selectedDocIds.value : undefined,
      is_enabled: enabledOnly.value ? true : undefined,
      ...sortParams.value,
    });
    const rows = res?.data || [];
    total.value = res?.total || 0;
    if (append) items.value = items.value.concat(rows);
    else items.value = rows;
    page.value = target;
    await nextTick();
    // 新一页插入 DOM 后再水合 provider 图片（blob 有全局缓存，翻页不重复拉取）。
    if (gridRef.value) await hydrateProtectedFileImages(gridRef.value, undefined, props.kbId);
  } catch (err: unknown) {
    // 画廊查询失败只提示本面板，不影响知识库其它功能（PRD §5.3）。
    errorTip.value = t('imageGallery.loadFailed');
    console.warn('[ImageGallery] load failed:', err);
  } finally {
    loading.value = false;
    // 追加后哨兵若仍在视口内（短屏/大 rootMargin），IntersectionObserver 不会
    // 再触发，手动续载直到填满一屏或取完。
    if (append && sentinelRef.value) {
      const rect = sentinelRef.value.getBoundingClientRect();
      if (rect.top < window.innerHeight + 600) loadNext();
    }
  }
}

function reload() {
  items.value = [];
  page.value = 1;
  fetchPage(1, false);
}

function loadNext() {
  if (loading.value || noMore.value || errorTip.value) return;
  fetchPage(page.value + 1, true);
}

// 来源文档下拉：分页拉全量文档列表（服务端 page_size 上限 1000，分批循环）。
async function loadDocumentOptions() {
  if (!props.kbId || loadingDocs.value || docOptions.value.length) return;
  loadingDocs.value = true;
  try {
    const collected: DocOption[] = [];
    const pageSize = 100;
    for (let p = 1; p <= 10; p++) {
      const res: any = await listKnowledgeFiles(props.kbId, { page: p, page_size: pageSize });
      const rows: any[] = res?.data || [];
      rows.forEach((k) => {
        const name = k?.title || k?.file_name || k?.name || k?.id || '';
        if (k?.id) collected.push({ label: name, value: k.id });
      });
      if (rows.length < pageSize) break;
    }
    docOptions.value = collected;
  } catch (err: unknown) {
    console.warn('[ImageGallery] load documents failed:', err);
  } finally {
    loadingDocs.value = false;
  }
}

const selected = ref<ImageAsset | null>(null);

// —— 无限滚动：哨兵进入视口即取下一页 ——
const gridRef = ref<HTMLElement | null>(null);
const sentinelRef = ref<HTMLElement | null>(null);
let observer: IntersectionObserver | null = null;

onMounted(() => {
  reload();
  observer = new IntersectionObserver((entries) => {
    if (entries.some((e) => e.isIntersecting)) loadNext();
  }, { rootMargin: '600px 0px' });
  if (sentinelRef.value) observer.observe(sentinelRef.value);
});

onBeforeUnmount(() => {
  observer?.disconnect();
  observer = null;
});

// 关键词防抖。
let keywordTimer: ReturnType<typeof setTimeout> | null = null;
watch(keyword, () => {
  if (keywordTimer) clearTimeout(keywordTimer);
  keywordTimer = setTimeout(reload, 350);
});

// 排序 / 来源文档过滤变化立即刷新。
watch([sortKey, selectedDocIds], () => reload());

function onImgError(event: Event) {
  const img = event.target as HTMLImageElement;
  if (img) img.style.visibility = 'hidden';
}

watch(() => props.kbId, () => {
  docOptions.value = [];
  selectedDocIds.value = [];
  selected.value = null;
  reload();
});

function openDetail(asset: ImageAsset) {
  selected.value = asset;
}
function clearFilters() {
  keyword.value = '';
  selectedDocIds.value = [];
  enabledOnly.value = false;
  sortKey.value = 'newest';
}
function openDocument(asset: ImageAsset) {
  if (asset.knowledge_id) emit('open-source-doc', asset.knowledge_id);
}
function openSource(asset: ImageAsset) {
  openDetail(asset);
}
</script>

<template>
  <div class="image-gallery">
    <div class="gallery-toolbar">
      <t-input v-model="keyword" class="gallery-search" clearable :placeholder="$t('imageGallery.searchPlaceholder')">
        <template #prefix-icon><t-icon name="search" size="16px" /></template>
      </t-input>
      <t-select v-model="selectedDocIds" class="gallery-doc-filter" multiple filterable clearable
        :min-collapsed-num="1" :max="1000" :loading="loadingDocs" :placeholder="$t('imageGallery.docFilterPlaceholder')"
        :options="docOptions" @visible-change="(v: any) => { if (v) loadDocumentOptions(); }">
        <template #empty>{{ $t('imageGallery.docFilterEmpty') }}</template>
      </t-select>
      <t-select v-model="sortKey" class="gallery-sort">
        <t-option value="newest" :label="$t('imageGallery.sortNewest')" />
        <t-option value="oldest" :label="$t('imageGallery.sortOldest')" />
        <t-option value="caption" :label="$t('imageGallery.sortCaption')" />
      </t-select>
      <t-checkbox v-model="enabledOnly" class="gallery-enabled-only" @change="reload">
        {{ $t('imageGallery.enabledOnly') }}
      </t-checkbox>
      <span class="gallery-total">{{ $t('imageGallery.totalCount', { count: total }) }}</span>
    </div>

    <div v-if="errorTip" class="gallery-error">{{ errorTip }}</div>

    <div v-if="items.length" ref="gridRef" class="gallery-grid">
      <div v-for="asset in items" :key="asset.id" class="gallery-cell" @click="openSource(asset)">
        <img class="gallery-img" :src="isProviderScheme(asset.url) ? PLACEHOLDER : (asset.url || asset.original_url)"
          :data-protected-src="isProviderScheme(asset.url) ? asset.url : null" :alt="cardText(asset)" loading="lazy"
          @error="onImgError" />
        <div class="gallery-cell-caption" :title="cardText(asset)">{{ cardText(asset) }}</div>
      </div>
    </div>

    <div v-if="!loading && !items.length && !errorTip" class="gallery-empty">
      <t-icon name="image" size="44px" />
      <p class="gallery-empty-title">{{ $t('imageGallery.empty') }}</p>
      <p class="gallery-empty-hint">{{ $t('imageGallery.emptyHint') }}</p>
      <t-button v-if="keyword || selectedDocIds.length || enabledOnly || sortKey !== 'newest'" variant="outline"
        size="small" @click="clearFilters">{{ $t('imageGallery.clearFilters') }}</t-button>
    </div>

    <div v-if="loading" class="gallery-loading">
      <t-loading size="small" /> {{ $t('imageGallery.loading') }}
    </div>
    <div v-else-if="items.length && noMore" class="gallery-nomore">{{ $t('imageGallery.noMore') }}</div>

    <div ref="sentinelRef" class="gallery-sentinel" aria-hidden="true"></div>

    <!-- 单图详情：来源元数据 + FR-05 原文位置跳转 -->
    <ImageDetailDrawer v-if="selected" :asset="selected" :kb-id="kbId" @close="selected = null"
      @open-document="openDocument" />
  </div>
</template>

<style scoped>
.image-gallery {
  padding: 16px 24px 32px;
  min-height: 100%;
}

.gallery-toolbar {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
  margin-bottom: 16px;
}

.gallery-search {
  width: 260px;
}

.gallery-doc-filter {
  width: 280px;
}

.gallery-sort {
  width: 140px;
}

.gallery-enabled-only {
  white-space: nowrap;
}

.gallery-total {
  margin-left: auto;
  color: var(--td-text-color-placeholder);
  font-size: 12px;
  white-space: nowrap;
}

.gallery-error {
  margin: 8px 0 16px;
  padding: 8px 12px;
  border-radius: 6px;
  background: var(--td-error-color-1);
  color: var(--td-error-color);
  font-size: 13px;
}

/* CSS 多列实现瀑布流：列数随宽度自适应，列内纵向排布。 */
.gallery-grid {
  column-gap: 12px;
  columns: 5 200px;
}

.gallery-cell {
  break-inside: avoid;
  margin-bottom: 12px;
  border-radius: 8px;
  overflow: hidden;
  cursor: pointer;
  background: var(--td-bg-color-secondarycontainer);
  border: 1px solid var(--td-component-stroke);
  transition: box-shadow 0.15s ease, transform 0.15s ease;
}

.gallery-cell:hover {
  box-shadow: 0 4px 16px rgba(0, 0, 0, 0.12);
  transform: translateY(-1px);
}

.gallery-img {
  display: block;
  width: 100%;
  min-height: 48px;
  object-fit: cover;
}

.gallery-cell-caption {
  padding: 6px 10px;
  font-size: 12px;
  line-height: 1.5;
  color: var(--td-text-color-secondary);
  overflow: hidden;
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
}

.gallery-empty {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 6px;
  padding: 72px 0;
  color: var(--td-text-color-placeholder);
}

.gallery-empty-title {
  margin: 8px 0 0;
  font-size: 14px;
  color: var(--td-text-color-secondary);
}

.gallery-empty-hint {
  margin: 0 0 12px;
  font-size: 12px;
}

.gallery-loading,
.gallery-nomore {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  padding: 20px 0;
  color: var(--td-text-color-placeholder);
  font-size: 13px;
}

.gallery-sentinel {
  height: 1px;
}
</style>
