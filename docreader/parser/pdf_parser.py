"""PDF parsing with per-page routing between native text and scanned images.

Design (aligned with how MinerU / Docling / DeepDoc route PDFs):

* The dominant signal for "this page is scanned" is the **image-area coverage
  ratio** (image bounding-box area / page area), not the raw character count.
  A scanned page is essentially one big image covering the whole page, even
  when it carries a (often low-quality) embedded OCR text layer. Trusting that
  embedded text layer is what produced garbled RAG content before.
* Pages are classified independently so hybrid PDFs (some native, some scanned)
  are handled correctly. Native pages contribute their text layer; scanned
  pages are rendered to JPEG and tagged ``image_source_type=scanned_pdf`` so the
  Go App performs OCR/VLM on them (docreader itself never runs OCR).

No external services (e.g. MinerU) are required: the builtin engine is fully
self-sufficient using pypdfium2 + the Go-side OCR that already exists.
"""

import base64
import io
import logging
import os
import re
import statistics
import threading
from collections import deque
from dataclasses import dataclass, field
from typing import Any, Iterator, Optional

from docreader.config import CONFIG
from docreader.models.document import Document
from docreader.parser.base_parser import BaseParser
from docreader.parser.concurrency import parser_worker_limit
from docreader.parser.source_locator import (
    PageGeometry,
    page_blocks,
    rebase_blocks,
    union_boxes,
)
from docreader.parser.stream import (
    ParsedDocumentStream,
    ParseBudget,
    StreamedImage,
    track_stream,
)

logger = logging.getLogger(__name__)

# pdfium (the C library behind pypdfium2) is process-global and NOT
# thread-safe: two gRPC worker threads parsing PDFs at the same time corrupt
# its shared state and can deadlock the whole process indefinitely (observed
# as requests stuck in "Parsing document with PDFParser" forever, taking down
# all subsequent uploads too). Every pdfium touch — text extraction, page
# rendering, image extraction — must be serialized behind this single global
# lock. Concurrent PDF uploads are then processed one at a time instead of
# hanging. Non-PDF parsers (docx, xlsx, ...) are unaffected and still run
# concurrently across the gRPC thread pool.
_PDFIUM_LOCK = threading.Lock()


def _env_float(name: str, default: float) -> float:
    v = os.environ.get(name)
    if v is None or not str(v).strip():
        return default
    try:
        return float(v)
    except ValueError:
        return default


def _env_int(name: str, default: int) -> int:
    v = os.environ.get(name)
    if v is None or not str(v).strip():
        return default
    try:
        return int(str(v).strip())
    except ValueError:
        return default


def _env_bool(name: str, default: bool) -> bool:
    v = os.environ.get(name)
    if v is None or not str(v).strip():
        return default
    return str(v).strip().lower() in {"1", "true", "yes", "y", "on"}


# A page whose image objects cover at least this fraction of the page area is
# treated as scanned (image-dominated). Native digital pages measure ~0.0-0.05;
# scanned pages measure ~1.0+, so 0.5 leaves a wide safety margin.
SCAN_IMAGE_AREA_RATIO = _env_float("DOCREADER_PDF_SCAN_IMAGE_RATIO", 0.5)
# Below this many characters a page is considered to have no usable text layer.
SCAN_MIN_CHARS_PER_PAGE = _env_int("DOCREADER_PDF_SCAN_MIN_CHARS", 10)
# A near-empty-text page is only rendered as an image if it actually contains
# some image content (avoids rendering genuinely blank pages).
_LOW_TEXT_IMAGE_RATIO = 0.1

# --- Embedded figure extraction (text pages) ------------------------------
# Native pages can embed figures/charts. We surface them as image references so
# the Go App can OCR/caption them (docreader does not caption). Logos, icons,
# watermarks and tiny decorations are filtered out by size, page-area share and
# cross-page repetition.
EXTRACT_EMBEDDED_IMAGES = _env_bool("DOCREADER_PDF_EXTRACT_EMBEDDED_IMAGES", True)
# Minimum pixel width AND height for an embedded image to be kept.
EMBED_MIN_PIXELS = _env_int("DOCREADER_PDF_EMBED_MIN_PIXELS", 80)
# Minimum share of the page area for an embedded image to be kept.
EMBED_MIN_AREA_RATIO = _env_float("DOCREADER_PDF_EMBED_MIN_AREA_RATIO", 0.01)
# An identical image appearing on at least this fraction of text pages is
# treated as a running logo/watermark and dropped.
EMBED_REPEAT_PAGE_FRAC = _env_float("DOCREADER_PDF_EMBED_REPEAT_PAGE_FRAC", 0.5)
# Hard cap on the number of embedded images extracted per document.
EMBED_MAX_IMAGES = _env_int("DOCREADER_PDF_EMBED_MAX_IMAGES", 50)

# --- Layout-aware text extraction (native text pages) ---------------------
# Reconstruct reading order with a geometric XY-cut so multi-column pages are
# linearised column-by-column instead of line-interleaved.
LAYOUT_ORDERING = _env_bool("DOCREADER_PDF_LAYOUT_ORDERING", True)
# When glyphs are positioned without explicit space characters (common in OCR /
# search text layers), insert a space if the horizontal gap exceeds this
# multiple of the line's median glyph width.
WORD_GAP_WIDTH_RATIO = _env_float("DOCREADER_PDF_WORD_GAP_WIDTH_RATIO", 0.4)
# Promote visually larger lines to markdown headings (font-size proxy = rect
# height relative to the page's median line height).
DETECT_HEADINGS = _env_bool("DOCREADER_PDF_DETECT_HEADINGS", True)
# Drop invisible (render-mode 3), off-page and degenerate text — a cheap guard
# against hidden-text prompt injection and OCR artefacts.
FILTER_HIDDEN_TEXT = _env_bool("DOCREADER_PDF_FILTER_HIDDEN_TEXT", True)
# Narrow side strips (arXiv watermarks, page labels) narrower than this share of
# page width are dropped when they look like vertical / single-glyph noise.
MARGIN_COL_WIDTH_RATIO = _env_float("DOCREADER_PDF_MARGIN_COL_WIDTH_RATIO", 0.12)
# Minimum characters on a line before font-size heuristics may promote it to a
# markdown heading (avoids ``### C`` from margin glyphs).
MIN_HEADING_LINE_CHARS = _env_int("DOCREADER_PDF_MIN_HEADING_LINE_CHARS", 8)
# Strip pdfium placeholder glyphs (U+FFFE) and soft hyphens; remove axis/legend text
# from vector figures when a Figure caption is present on the page.
SANITIZE_PDF_TEXT = _env_bool("DOCREADER_PDF_SANITIZE_TEXT", True)
STRIP_CHART_TEXT_DEBRIS = _env_bool("DOCREADER_PDF_STRIP_CHART_DEBRIS", True)
# Render detected vector chart regions (no embedded bitmap) as JPEG for VLM/OCR.
RENDER_VECTOR_FIGURES = _env_bool("DOCREADER_PDF_RENDER_VECTOR_FIGURES", True)
MIN_CHART_REGION_CHARS = _env_int("DOCREADER_PDF_MIN_CHART_REGION_CHARS", 18)
MIN_CHART_REGION_AREA_RATIO = _env_float("DOCREADER_PDF_MIN_CHART_REGION_AREA", 0.015)
MAX_CHART_REGION_AREA_RATIO = _env_float("DOCREADER_PDF_MAX_CHART_REGION_AREA", 0.42)
MAX_FIGURE_HEIGHT_RATIO = _env_float("DOCREADER_PDF_MAX_FIGURE_HEIGHT_RATIO", 0.38)

# --- 引用溯源定位（FR-05）---------------------------------------------------
# 为原生文本页额外抽取视觉行 bbox，用于把解析结果区间映射到页内几何区域。
# 关闭后仍会产出页级定位（只有 page，没有 bbox）。
SOURCE_BOXES = _env_bool("DOCREADER_PDF_SOURCE_BOXES", True)

# --- Force scanned mode ------------------------------------------------------
# When True, ALL PDF pages are rendered as images and routed through OCR/VLM,
# bypassing the automatic text/scanned page classification. Useful for PDFs
# with a low-quality or misleading text layer (web-print, scanned, image-heavy).
# Can be overridden per-upload via parser_engine_overrides.pdf_force_scanned.
FORCE_SCANNED_PDF = _env_bool("DOCREADER_PDF_FORCE_SCANNED", False)

# --- OCR 流水线可配置化（FR-13）----------------------------------------------
# Go 侧把知识库的 OCR 流水线配置摊平进 ``parser_engine_overrides``（既有通道，
# 不改 proto），键名形如 ``ocr_pipeline`` / ``manual.render_dpi`` /
# ``smart.scan_image_ratio`` / ``preprocess.denoise.sigma``。这里按
# 「override > 环境变量 > 默认值」的既有优先级读取（与 pdf_force_scanned 一致）。
#
# 取值校验对齐上游 image_preprocess.resolve_threshold：**越界拒绝并回落默认值
# + warning，不做 clamp**。悄悄把 601 改成 600 会让用户以为自己的配置生效了。
OCR_PIPELINE_MANUAL = "manual"
OCR_PIPELINE_SMART = "smart"
OCR_PIPELINE_OVERRIDE_KEY = "ocr_pipeline"
# 前处理参数在 overrides 里的统一前缀（Go 侧 types.PreprocessParamPrefix）。
PREPROCESS_OVERRIDE_PREFIX = "preprocess."

# page_classes 写进 metadata 的页数上限：上千页的文档拼出的逗号串会白白撑大
# 响应体，此时只保留计数。
PAGE_CLASSES_METADATA_LIMIT = _env_int("DOCREADER_PDF_PAGE_CLASSES_LIMIT", 200)


def _override_bool(params: dict, key: str, default: bool) -> bool:
    """读一个布尔 override；无法解析时回落默认值（不抛异常）。"""
    if key not in params:
        return default
    raw = params[key]
    if isinstance(raw, bool):
        return raw
    text = str(raw or "").strip().lower()
    if text in {"1", "true", "yes", "y", "on"}:
        return True
    if text in {"0", "false", "no", "n", "off", ""}:
        return False
    logger.warning("ocr: %s=%r 不是合法布尔值，回落默认值 %s", key, raw, default)
    return default


def _override_number(
    params: dict,
    key: str,
    default: float | None,
    low: float,
    high: float,
    *,
    integer: bool = False,
):
    """读一个数值 override：越界或不可解析一律拒绝并回落默认值。

    ``default`` 为 ``None`` 表示「知识库没配就用服务默认」，此时返回 None，
    由调用方回落到 CONFIG / 环境变量 —— 这样运维用 ``DOCREADER_PDF_RENDER_DPI``
    调过的部署不会被 Go 侧声明的默认值悄悄覆盖。
    """
    if key not in params:
        return default
    raw = params[key]
    try:
        value = float(str(raw).strip())
    except (TypeError, ValueError):
        logger.warning("ocr: %s=%r 无法解析为数值，回落默认值", key, raw)
        return default
    if value < low or value > high:
        logger.warning(
            "ocr: %s=%.4g 超出取值范围 [%.4g, %.4g]，拒绝并回落默认值",
            key, value, low, high,
        )
        return default
    return int(value) if integer else value


@dataclass(frozen=True)
class OCRPipelineSettings:
    """一次解析生效的 OCR 流水线配置（手动 / 智能）。"""

    pipeline: str = OCR_PIPELINE_MANUAL
    # 手动流水线：全部按扫描页处理（等价于既有 pdf_force_scanned）。
    force_scanned: bool = False
    # 渲染分辨率与 JPEG 质量；0 表示沿用服务默认（CONFIG.pdf_render_*）。
    render_dpi: int = 0
    jpeg_quality: int = 0
    # 智能流水线：页面分类阈值与混合页策略。
    scan_image_ratio: float | None = None
    min_chars_per_page: int | None = None
    mixed_use_text_layer: bool = True
    # 智能流水线：需要渲染的页面占比达到该值时判定为扫描密集文档，提高 DPI。
    scan_dense_ratio: float = 0.5
    heavy_render_dpi: int = 300
    # 前处理参数（裸名 -> 字符串），交给 parser.image_preprocess 解析。
    preprocess_params: dict = field(default_factory=dict)
    # 可解释的决策轨迹，最终写进 metadata 便于诊断。
    decisions: tuple = ()

    @property
    def is_smart(self) -> bool:
        return self.pipeline == OCR_PIPELINE_SMART

    @property
    def text_layer_classes(self) -> tuple:
        """哪些页类保留文本层 —— 这是两条流水线唯一的行为分歧点。"""
        if self.is_smart and self.mixed_use_text_layer:
            return SMART_TEXT_LAYER_CLASSES
        return MANUAL_TEXT_LAYER_CLASSES

    @property
    def preprocess_mode(self) -> str:
        return str(self.preprocess_params.get("mode") or "off").strip().lower()


def resolve_ocr_settings(overrides: dict | None) -> OCRPipelineSettings:
    """从 parser overrides 解析出本次解析生效的 OCR 流水线配置。

    未识别的流水线 id 回落手动；缺失的参数一律回落服务默认，因此 FR-13 之前
    的调用方（不传任何 ocr 键）行为逐字不变。
    """
    params = dict(overrides or {})
    raw_pipeline = str(params.get(OCR_PIPELINE_OVERRIDE_KEY) or "").strip().lower()
    if raw_pipeline and raw_pipeline not in (OCR_PIPELINE_MANUAL, OCR_PIPELINE_SMART):
        logger.warning(
            "ocr: 未知的流水线 id %r，回落 %s", raw_pipeline, OCR_PIPELINE_MANUAL
        )
    pipeline = raw_pipeline if raw_pipeline in (OCR_PIPELINE_MANUAL, OCR_PIPELINE_SMART) \
        else OCR_PIPELINE_MANUAL
    prefix = pipeline + "."

    # 前处理参数：Go 侧下发的键形如 preprocess.denoise.sigma，摘掉前缀后交给
    # image_preprocess 自己解析（它有同一套取值范围声明）。
    preprocess_params: dict = {}
    for key, value in params.items():
        if isinstance(key, str) and key.startswith(PREPROCESS_OVERRIDE_PREFIX):
            bare = key[len(PREPROCESS_OVERRIDE_PREFIX):]
            if bare:
                preprocess_params[bare] = value
    # 流水线字段里也带一个 preprocess.mode（面板上它属于该流水线），
    # 顶层键优先，其次取带流水线前缀的那份。
    slot_mode = params.get(prefix + "preprocess.mode")
    if "mode" not in preprocess_params and slot_mode is not None:
        preprocess_params["mode"] = slot_mode

    return OCRPipelineSettings(
        pipeline=pipeline,
        force_scanned=_override_bool(params, prefix + "force_scanned", False),
        render_dpi=int(_override_number(params, prefix + "render_dpi", 0, 72, 600, integer=True) or 0),
        jpeg_quality=int(_override_number(params, prefix + "jpeg_quality", 0, 1, 95, integer=True) or 0),
        scan_image_ratio=_override_number(params, prefix + "scan_image_ratio", None, 0.0, 1.0),
        min_chars_per_page=_override_number(
            params, prefix + "min_chars_per_page", None, 0, 2000, integer=True
        ),
        mixed_use_text_layer=_override_bool(params, prefix + "mixed_use_text_layer", True),
        scan_dense_ratio=float(_override_number(params, prefix + "scan_dense_ratio", 0.5, 0.0, 1.0)),
        heavy_render_dpi=int(_override_number(params, prefix + "heavy_render_dpi", 300, 72, 600, integer=True)),
        preprocess_params=preprocess_params,
    )


def _apply_preprocess(rendered: dict, settings: OCRPipelineSettings) -> tuple:
    """对渲染出的整页图执行可选的 OCR 前处理（FR-13）。

    返回 ``(处理后的 rendered, 汇总字典)``。默认关闭；依赖缺失、解码失败或
    任何异常都**原样返回入参图片**，绝不因为前处理失败而阻断解析。
    """
    summary: dict = {"mode": settings.preprocess_mode}
    if not rendered:
        return rendered, summary
    try:
        from docreader.parser import image_preprocess
    except Exception as exc:  # pragma: no cover - 模块自身有 import 守卫
        logger.warning("ocr: 前处理模块不可用，跳过: %s", exc)
        summary["status"] = "unavailable"
        summary["notes"] = str(exc)
        return rendered, summary

    if not image_preprocess.is_available():
        summary["status"] = "unavailable"
        summary["notes"] = image_preprocess.unavailable_reason()
        logger.warning("ocr: 前处理不可用（%s），跳过", summary["notes"])
        return rendered, summary

    effective = image_preprocess.effective_thresholds(settings.preprocess_params)
    summary["mode"] = effective.get("mode", image_preprocess.MODE_OFF)
    if summary["mode"] == image_preprocess.MODE_OFF:
        summary["status"] = "skipped"
        return rendered, summary
    if not image_preprocess.any_step_enabled(effective):
        summary["status"] = "skipped"
        summary["notes"] = "未启用任何前处理步骤"
        return rendered, summary

    logger.info("ocr: 前处理生效参数 %s", image_preprocess.summarize(effective))
    quality = _normalize_image_quality(settings.jpeg_quality or CONFIG.pdf_jpeg_quality)
    out: dict = {}
    steps: dict = {}
    processed = 0
    notes: list = []
    for index, data in rendered.items():
        try:
            new_data, result = image_preprocess.preprocess_jpeg(
                data, settings.preprocess_params, quality=quality
            )
        except Exception as exc:  # pragma: no cover - 防御性
            logger.warning("ocr: 第 %d 页前处理失败，保留原图: %s", index + 1, exc)
            out[index] = data
            continue
        out[index] = new_data or data
        if result.status == "ok":
            processed += 1
            for step in result.applied:
                steps[step] = steps.get(step, 0) + 1
        elif result.notes:
            notes.append(result.notes)
    summary["status"] = "ok" if processed else "no_change"
    summary["pages"] = processed
    summary["steps"] = ",".join(sorted(steps))
    if notes:
        # 只保留第一条原因，避免同样的话在 metadata 里重复上百遍。
        summary["notes"] = notes[0]
    return out, summary


def _preprocess_metadata(summary: dict | None) -> dict:
    """把前处理汇总转成 metadata 键值（可解释、可诊断）。

    前处理默认关闭，此时不产生任何 metadata 键，因此默认配置下的 metadata
    与 FR-13 之前保持一致。
    """
    summary = summary or {}
    mode = summary.get("mode") or "off"
    if mode == "off":
        # 前处理关闭（默认）时不产生任何 metadata 键，保持与 FR-13 之前一致。
        return {}
    out = {"preprocess_mode": mode}
    if summary.get("status"):
        out["preprocess_status"] = summary["status"]
    if summary.get("steps"):
        out["preprocess_steps"] = summary["steps"]
    if summary.get("pages"):
        out["preprocess_pages"] = summary["pages"]
    if summary.get("notes"):
        out["preprocess_notes"] = summary["notes"]
    return out


# pdfium / Adobe text layers often emit U+FFFE for missing hyphenation or ligatures.
_PDF_ARTIFACT_RE = re.compile(r"[\u00ad\u200b-\u200f\ufeff\ufffe\uffff]")
_PDF_ARTIFACT_JOIN_RE = re.compile(r"(\w)[\u00ad\ufffe](\w)")
_CHART_DEBRIS_LINE_RE = re.compile(
    r"^(?:"
    r"[\d\s.]+|"
    r"\d{1,2}|"
    r"\d+-layer|"
    r"iter\.\s*\(1e4\)|"
    r"(?:training|test)\s+error\s*\(%\)"
    r")$",
    re.IGNORECASE,
)
_CHART_LAYER_RE = re.compile(r"^\d+-layer$", re.IGNORECASE)
_FIGURE_CAPTION_RE = re.compile(r"^Figure\s+\d+\b", re.IGNORECASE)
_FIGURE_CAPTION_SEARCH_RE = re.compile(r"\bFigure\s+(\d+)\b", re.IGNORECASE)
_ARXIV_LINE_RE = re.compile(r"^arXiv:\s*\S+", re.IGNORECASE)
_PAGE_NUM_LINE_RE = re.compile(r"^\d{1,3}$")


def _close_pdfium_resource(resource) -> None:
    close = getattr(resource, "close", None)
    if close:
        close()


def _normalize_image_quality(quality: int) -> int:
    return min(95, max(1, quality))


# --- 页面四分类（FR-13）------------------------------------------------------
# 分类阈值沿用 Zilan 既有的「图像面积占比 + 文本层字符数」判定，本项补齐的是
# 把分类结果用于**路由不同处理路径**，而不是只写进 metadata 打标。
#
#	text     原生数字页：文本层可用，取文本层，不整页渲染；
#	scanned  扫描页：图像占比不高但几乎没有文本层（多为局部扫描/图片型页面），
#	         整页渲染交给 Go 侧 OCR/VLM；
#	image    图片页：图像铺满整页且没有可用文本层，整页渲染；
#	mixed    混合页：图像铺满整页但存在文本层（典型是带 OCR 文本层的扫描件，
#	         或大图 + 正文的原生页）。**这一类是两条流水线唯一的分歧点**：
#	         手动流水线按 FR-13 之前的行为只渲染、不信任文本层；智能流水线
#	         （需显式开启）在 mixed_use_text_layer=true 时两者兼取。
PAGE_CLASS_TEXT = "text"
PAGE_CLASS_SCANNED = "scanned"
PAGE_CLASS_IMAGE = "image"
PAGE_CLASS_MIXED = "mixed"

# 需要整页渲染成图片的页类（两条流水线一致）。
RENDER_PAGE_CLASSES = (PAGE_CLASS_SCANNED, PAGE_CLASS_IMAGE, PAGE_CLASS_MIXED)
# 取文本层的页类：手动流水线只有文本页 —— 与 FR-13 之前逐字一致，因此默认
# 配置下存量部署的解析结果不变（PRD §5.4）。
MANUAL_TEXT_LAYER_CLASSES = (PAGE_CLASS_TEXT,)
# 智能流水线额外把混合页的文本层也保留下来。
SMART_TEXT_LAYER_CLASSES = (PAGE_CLASS_TEXT, PAGE_CLASS_MIXED)


def _classify_page(
    image_area_ratio: float,
    text_len: int,
    *,
    scan_ratio: float | None = None,
    min_chars: int | None = None,
) -> str:
    """把一页判定为 ``"text"`` / ``"scanned"`` / ``"image"`` / ``"mixed"``。

    图像面积占比是主信号，文本层字符数是次信号；阈值可由智能流水线的知识库
    参数覆盖（``smart.scan_image_ratio`` / ``smart.min_chars_per_page``），
    未覆盖时用既有的 ``DOCREADER_PDF_SCAN_*`` 环境变量。
    """
    area_threshold = SCAN_IMAGE_AREA_RATIO if scan_ratio is None else float(scan_ratio)
    chars_threshold = SCAN_MIN_CHARS_PER_PAGE if min_chars is None else int(min_chars)
    if image_area_ratio >= area_threshold:
        # 图像铺满整页：有可用文本层是混合页，没有则是纯图片页。
        if text_len < chars_threshold:
            return PAGE_CLASS_IMAGE
        return PAGE_CLASS_MIXED
    if text_len < chars_threshold and image_area_ratio >= _LOW_TEXT_IMAGE_RATIO:
        return PAGE_CLASS_SCANNED
    return PAGE_CLASS_TEXT


def _page_image_area_ratio(page, raw) -> float:
    """Return the fraction of the page area covered by image objects.

    Overlapping images can push the ratio above 1.0; callers only compare it
    against a threshold so that is harmless.
    """
    width, height = page.get_size()
    page_area = float(width) * float(height)
    if page_area <= 0:
        return 0.0

    image_area = 0.0
    for obj in page.get_objects():
        try:
            if obj.type == raw.FPDF_PAGEOBJ_IMAGE:
                left, bottom, right, top = obj.get_bounds()
                image_area += abs((right - left) * (top - bottom))
        except Exception:
            continue
    return image_area / page_area


def _extract_page_text(page) -> str:
    """Plain top-to-bottom text extraction (fallback path)."""
    textpage = None
    try:
        textpage = page.get_textpage()
        return textpage.get_text_range()
    finally:
        _close_pdfium_resource(textpage)


def _sanitize_pdf_text(text: str) -> str:
    """Remove PDF text-layer placeholders and repair broken hyphenations."""
    if not text:
        return text
    text = _PDF_ARTIFACT_RE.sub("", text)
    text = _PDF_ARTIFACT_JOIN_RE.sub(r"\1\2", text)
    return text


def _is_chart_debris_line(line: str) -> bool:
    t = line.strip()
    if not t:
        return False
    if _CHART_DEBRIS_LINE_RE.match(t):
        return True
    if _CHART_LAYER_RE.match(t):
        return True
    # Tick labels like "0 1 2 3 4 5 6 0"
    if re.fullmatch(r"[\d\s.()-]+", t) and len(t) <= 24 and sum(c.isdigit() for c in t) >= 3:
        return True
    return False


def _strip_chart_text_debris(text: str) -> str:
    """Drop runs of axis/legend lines leaked from vector figures into the text layer."""
    if not text:
        return text
    lines = text.replace("\r\n", "\n").replace("\r", "\n").split("\n")
    out: list = []
    i = 0
    while i < len(lines):
        if _is_chart_debris_line(lines[i]):
            j = i
            while j < len(lines) and (
                _is_chart_debris_line(lines[j]) or not lines[j].strip()
            ):
                j += 1
            if j - i >= 3:
                i = j
                continue
        out.append(lines[i])
        i += 1
    return "\n".join(out)


def _strip_arxiv_and_page_num_lines(text: str) -> str:
    lines = text.replace("\r\n", "\n").replace("\r", "\n").split("\n")
    kept: list = []
    for ln in lines:
        t = ln.strip()
        if _ARXIV_LINE_RE.match(t):
            continue
        if _PAGE_NUM_LINE_RE.match(t):
            continue
        if "arXiv:" in ln:
            ln = re.sub(r"\s*arXiv:\s*\S+\s*(?:\[[^\]]+\])?\s*[^\n]*", "", ln).strip()
            if not ln:
                continue
        kept.append(ln)
    return "\n".join(kept)


def _strip_lines_above_figure_captions(text: str) -> str:
    """Remove diagram/chart label lines that sit immediately above a Figure caption."""
    lines = text.replace("\r\n", "\n").replace("\r", "\n").split("\n")
    out: list = []
    for ln in lines:
        if _line_has_figure_caption(ln):
            while out and _is_figure_interior_line(out[-1]):
                out.pop()
            out.append(ln)
        else:
            out.append(ln)
    return "\n".join(out)


def _is_body_paragraph_line(text: str) -> bool:
    t = text.strip()
    if len(t) < 48:
        return False
    return len(t.split()) >= 8


def _is_figure_interior_line(text: str) -> bool:
    """Short, non-body line directly above a Figure caption (diagram labels, ticks)."""
    t = text.strip()
    if not t or _FIGURE_CAPTION_RE.match(t):
        return False
    if _ARXIV_LINE_RE.match(t) or _PAGE_NUM_LINE_RE.match(t):
        return True
    if _is_body_paragraph_line(t):
        return False
    if _is_chart_debris_line(t):
        return True
    # Prose sentence above a figure (wrapped paragraph tail) — keep in text.
    if t.endswith((".", "。", "!", "?", "！")) and len(t) >= 15:
        return False
    if len(t.split()) >= 7:
        return False
    if len(t) <= 40:
        return True
    return False


def _postprocess_pdf_text(text: str) -> str:
    if SANITIZE_PDF_TEXT:
        text = _sanitize_pdf_text(text)
    text = _strip_arxiv_and_page_num_lines(text)
    text = _strip_lines_above_figure_captions(text)
    if STRIP_CHART_TEXT_DEBRIS:
        text = _strip_chart_text_debris(text)
    return text


def _char_looks_chart_axis_tick(ch: str) -> bool:
    """Axis tick / numeric chart labels only (not words like ``layer`` in diagrams)."""
    t = ch.strip()
    if not t:
        return False
    if len(t) == 1 and t in "0123456789.%()-":
        return True
    if _CHART_LAYER_RE.match(t):
        return True
    if re.fullmatch(r"iter\.\s*\(1e4\)", t, re.I):
        return True
    if re.fullmatch(r"(?:training|test)\s+error\s*\(%\)", t, re.I):
        return True
    return False


def _chars_bbox(char_list: list) -> tuple:
    return (
        min(c["x0"] for c in char_list),
        min(c["y0"] for c in char_list),
        max(c["x1"] for c in char_list),
        max(c["y1"] for c in char_list),
    )


def _bbox_area_ratio(bbox, page_w: float, page_h: float) -> float:
    page_area = float(page_w) * float(page_h)
    if page_area <= 0:
        return 0.0
    x0, y0, x1, y1 = bbox
    return max(0.0, (x1 - x0) * (y1 - y0) / page_area)


def _chart_region_bbox(chars: list, page_w: float, page_h: float):
    """Bounding box of numeric chart axis labels (fallback when caption walk fails)."""
    chart = [c for c in chars if _char_looks_chart_axis_tick(c["ch"])]
    if len(chart) < MIN_CHART_REGION_CHARS:
        return None
    bbox = _chars_bbox(chart)
    ratio = _bbox_area_ratio(bbox, page_w, page_h)
    if ratio < MIN_CHART_REGION_AREA_RATIO or ratio > MAX_CHART_REGION_AREA_RATIO:
        return None
    x0, y0, x1, y1 = bbox
    pad_x = max(8.0, (x1 - x0) * 0.08)
    pad_y = max(8.0, (y1 - y0) * 0.08)
    return (
        max(0.0, x0 - pad_x),
        max(0.0, y0 - pad_y),
        min(page_w, x1 + pad_x),
        min(page_h, y1 + pad_y),
    )


def _expand_chart_bbox(bbox, page_w: float, page_h: float, margin_frac: float = 0.18):
    x0, y0, x1, y1 = bbox
    dx = (x1 - x0) * margin_frac
    dy = (y1 - y0) * margin_frac
    return (
        max(0.0, x0 - dx),
        max(0.0, y0 - dy),
        min(page_w, x1 + dx),
        min(page_h, y1 + dy),
    )


def _render_page_clip_jpeg(page, bbox, scale: float, quality: int, max_edge: int) -> bytes:
    """Render a PDF page region to JPEG (bbox in PDF points, bottom-left origin)."""
    left, bottom, right, top = bbox
    scale_eff = _effective_scale(page, scale, max_edge)
    bitmap = None
    try:
        bitmap = page.render(scale=scale_eff)
        pil = bitmap.to_pil().convert("RGB")
    finally:
        _close_pdfium_resource(bitmap)
    page_w, page_h = page.get_size()
    x0 = int(left * scale_eff)
    x1 = int(right * scale_eff)
    y0 = int((page_h - top) * scale_eff)
    y1 = int((page_h - bottom) * scale_eff)
    if x1 <= x0 or y1 <= y0:
        raise ValueError("degenerate clip bbox")
    return _pil_to_jpeg_bytes(pil.crop((x0, y0, x1, y1)), quality)


def _pil_to_jpeg_bytes(pil, quality: int) -> bytes:
    buf = io.BytesIO()
    if pil.mode not in ("RGB", "L"):
        pil = pil.convert("RGB")
    pil.save(buf, format="JPEG", quality=quality, optimize=True)
    return buf.getvalue()


def _group_lines_with_chars(chars: list) -> list:
    """Group glyphs into lines; each line includes its char list and bbox."""
    if not chars:
        return []
    heights = [c["y1"] - c["y0"] for c in chars if c["y1"] > c["y0"]]
    med_h = statistics.median(heights) if heights else 1.0
    ordered = sorted(chars, key=lambda c: -(c["y0"] + c["y1"]) / 2)
    groups: list = []
    cur: list = []
    ref = None
    for c in ordered:
        yc = (c["y0"] + c["y1"]) / 2
        if ref is None or abs(yc - ref) <= 0.5 * med_h:
            cur.append(c)
            ref = yc if ref is None else ref
        else:
            groups.append(cur)
            cur = [c]
            ref = yc
    if cur:
        groups.append(cur)

    lines: list = []
    for grp in groups:
        grp_sorted = sorted(grp, key=lambda c: c["x0"])
        text = _join_line_glyphs(grp_sorted)
        if not text:
            continue
        hs = [c["y1"] - c["y0"] for c in grp_sorted if c["y1"] > c["y0"]]
        lines.append(
            {
                "text": text,
                "h": statistics.median(hs) if hs else med_h,
                "chars": grp_sorted,
                "bbox": _chars_bbox(grp_sorted),
            }
        )
    return lines


def _page_line_boxes(page, raw) -> list:
    """原生文本页的视觉行，形如 ``{"text", "bbox"}``（PDF 点、左下原点）。

    行按栏分组，双栏页不会把两栏的字形合并成一行（多栏感知，FR-05 规则 2）。
    任何异常都返回空列表——那只会把该页的定位降级为页码，不影响解析结果。
    """
    textpage = None
    try:
        textpage = page.get_textpage()
        chars, width = _page_chars(textpage, page, raw)
        if not chars:
            return []
        heights = [c["y1"] - c["y0"] for c in chars if c["y1"] - c["y0"] > 0]
        scale = (statistics.median(heights) if heights else 1.0) or 1.0
        lines = []
        for col in _split_columns(chars, scale, width):
            for ln in _group_lines_with_chars(col):
                lines.append({"text": ln["text"], "bbox": ln["bbox"]})
        return lines
    except Exception:
        logger.debug("line box extraction failed", exc_info=True)
        return []
    finally:
        _close_pdfium_resource(textpage)


def _page_geometry(page):
    """页面几何（CropBox + /Rotate），用于把 PDF 点换算成 0-1 归一化 bbox。"""
    try:
        return PageGeometry(page.get_cropbox(), page.get_rotation())
    except Exception:
        logger.debug("page geometry unavailable", exc_info=True)
        return None


def _line_has_figure_caption(text: str) -> bool:
    return bool(_FIGURE_CAPTION_SEARCH_RE.search((text or "").strip()))


def _bbox_above_caption(lines: list, cap_i: int, page_w: float, page_h: float):
    """Region above a Figure caption line (PDF coords, bottom-left origin)."""
    cap_bbox = lines[cap_i]["bbox"]
    cap_top = cap_bbox[3]
    x0, x1 = cap_bbox[0], cap_bbox[2]
    fig_h = page_h * min(MAX_FIGURE_HEIGHT_RATIO, 0.35)
    y_bottom = cap_top
    y_top = min(page_h, cap_top + fig_h)

    for j in range(cap_i - 1, -1, -1):
        t = lines[j]["text"]
        b = lines[j]["bbox"]
        if b[3] < y_bottom - 4:
            continue
        if b[1] > y_top + 4:
            break
        if _is_body_paragraph_line(t) and not _is_figure_interior_line(t):
            break
        if _is_figure_interior_line(t) or _is_chart_debris_line(t) or not t.strip():
            x0 = min(x0, b[0])
            x1 = max(x1, b[2])
            y_top = max(y_top, min(page_h, b[3] + fig_h * 0.15))

    min_h = page_h * 0.08
    if y_top - y_bottom < min_h:
        y_top = min(page_h, y_bottom + min_h)
    margin_x = max(8.0, (x1 - x0) * 0.05)
    return (
        max(0.0, x0 - margin_x),
        y_bottom,
        min(page_w, x1 + margin_x),
        y_top,
    )


def _cap_bbox_height(bbox, page_h: float, cap_y_top: float) -> tuple:
    """Limit figure bbox height (PDF coords, bottom-left origin)."""
    x0, y0, x1, y1 = bbox
    max_top = min(y1, cap_y_top + page_h * MAX_FIGURE_HEIGHT_RATIO)
    if max_top <= y0:
        return bbox
    return (x0, y0, x1, max_top)


def _inject_figure_markdown_before_captions(text: str, clips: list) -> str:
    """Place ``![...]()`` immediately before each Figure caption line in page text."""
    if not clips:
        return text
    lines = text.replace("\r\n", "\n").replace("\r", "\n").split("\n")
    clip_idx = 0
    for i, ln in enumerate(lines):
        if clip_idx >= len(clips):
            break
        if not _line_has_figure_caption(ln):
            continue
        if i > 0 and lines[i - 1].lstrip().startswith("!["):
            continue
        ref_path = clips[clip_idx][0]
        fname = os.path.basename(ref_path)
        img_md = f"![{fname}]({ref_path})"
        lines[i] = f"{img_md}\n\n{ln}"
        clip_idx += 1
    return "\n".join(lines)


def _extract_vector_figure_clips(
    page,
    page_index: int,
    plain_text: str,
    raw,
    base_name: str,
    scale: float,
    quality: int,
    max_edge: int,
) -> list:
    """Render vector figure regions anchored at each ``Figure N.`` caption on the page.

    Returns ``[(ref_path, b64, y_sort, caption_line), ...]`` for markdown injection.
    """
    if not RENDER_VECTOR_FIGURES or not re.search(r"\bFigure\s+\d+", plain_text, re.I):
        return []
    textpage = None
    try:
        textpage = page.get_textpage()
        chars, page_w = _page_chars(textpage, page, raw)
        if not chars:
            return []
        page_h = page.get_size()[1]
        lines = _merge_orphan_punctuation_lines(_group_lines_with_chars(chars))
        caption_indices = [
            i for i, ln in enumerate(lines) if _line_has_figure_caption(ln["text"])
        ]
        if not caption_indices:
            return []

        results: list = []
        for fig_idx, cap_i in enumerate(caption_indices):
            cap_line = lines[cap_i]["text"].strip()
            m = _FIGURE_CAPTION_SEARCH_RE.search(cap_line)
            if m:
                cap_line = cap_line[m.start() :].split("\n", 1)[0].strip()

            bbox = _bbox_above_caption(lines, cap_i, page_w, page_h)
            if bbox is None:
                bbox = _chart_region_bbox(chars, page_w, page_h)
            if bbox is None:
                continue

            ratio = _bbox_area_ratio(bbox, page_w, page_h)
            if ratio > MAX_CHART_REGION_AREA_RATIO:
                bbox = _cap_bbox_height(bbox, page_h, lines[cap_i]["bbox"][3])
                ratio = _bbox_area_ratio(bbox, page_w, page_h)
                if ratio > MAX_CHART_REGION_AREA_RATIO:
                    continue
            if ratio < MIN_CHART_REGION_AREA_RATIO:
                continue

            bbox = _expand_chart_bbox(bbox, page_w, page_h, margin_frac=0.06)
            jpeg = _render_page_clip_jpeg(page, bbox, scale, quality, max_edge)
            fname = f"{base_name}_p{page_index + 1}_fig{fig_idx + 1}.jpg"
            ref_path = f"images/{fname}"
            results.append(
                (
                    ref_path,
                    base64.b64encode(jpeg).decode("utf-8"),
                    bbox[3],
                    cap_line,
                )
            )
        return results
    except Exception:
        logger.debug("vector figure clip failed on page %d", page_index, exc_info=True)
        return []
    finally:
        _close_pdfium_resource(textpage)


def _collect_invisible_boxes(page, raw) -> list:
    """Bounding boxes of invisible (render-mode 3) text objects on the page."""
    boxes: list = []
    try:
        for obj in page.get_objects():
            if obj.type != raw.FPDF_PAGEOBJ_TEXT:
                continue
            try:
                mode = raw.FPDFTextObj_GetTextRenderMode(obj.raw)
            except Exception:
                continue
            if mode != raw.FPDF_TEXTRENDERMODE_INVISIBLE:
                continue
            try:
                left, bottom, right, top = obj.get_bounds()
            except Exception:
                continue
            boxes.append(
                (min(left, right), min(bottom, top), max(left, right), max(bottom, top))
            )
    except Exception:
        return []
    return boxes


def _point_in_boxes(x: float, y: float, boxes: list) -> bool:
    for x0, y0, x1, y1 in boxes:
        if x0 <= x <= x1 and y0 <= y <= y1:
            return True
    return False


def _page_chars(textpage, page, raw) -> tuple:
    """Return ``(chars, page_width)`` with hidden/off-page glyphs filtered.

    Working at the glyph level (instead of pdfium rect segments) keeps mixed
    CJK + Latin/number lines in their true left-to-right order, which the
    rect-level ``get_text_bounded`` API scrambles.
    """
    n = textpage.count_chars()
    if n <= 0:
        return [], 0.0
    width, height = page.get_size()
    invisible = _collect_invisible_boxes(page, raw) if FILTER_HIDDEN_TEXT else []

    chars: list = []
    for i in range(n):
        try:
            left, bottom, right, top = textpage.get_charbox(i)
        except Exception:
            continue
        ch = textpage.get_text_range(i, 1)
        if ch in ("\r", "\n"):
            continue
        x0, x1 = (left, right) if left <= right else (right, left)
        y0, y1 = (bottom, top) if bottom <= top else (top, bottom)
        if FILTER_HIDDEN_TEXT:
            if x1 < 0 or x0 > width or y1 < 0 or y0 > height:
                continue  # off-page glyph
            if invisible and _point_in_boxes((x0 + x1) / 2, (y0 + y1) / 2, invisible):
                continue  # covered by an invisible text object
        chars.append({"x0": x0, "y0": y0, "x1": x1, "y1": y1, "ch": ch})
    return chars, width


def _find_split(items: list, axis: str, min_gap: float):
    """Return a coordinate at the widest clean gap on ``axis`` ('x'), or None.

    A "clean" gap means no item interval bridges it — i.e. a full-height column
    gutter. Used to detect multi-column layouts.
    """
    lo, hi = ("x0", "x1") if axis == "x" else ("y0", "y1")
    intervals = sorted(((s[lo], s[hi]) for s in items), key=lambda iv: iv[0])
    cur_end = intervals[0][1]
    best_gap, best_cut = 0.0, None
    for a, b in intervals[1:]:
        gap = a - cur_end
        if gap >= min_gap and gap > best_gap:
            best_gap, best_cut = gap, cur_end + gap / 2
        if b > cur_end:
            cur_end = b
    return best_cut


def _split_columns(chars: list, scale: float, width: float, depth: int = 0) -> list:
    """Split glyphs into reading-order columns at full-height gutters."""
    if len(chars) <= 1 or depth > 10:
        return [chars]
    min_gap = max(scale * 2.5, width * 0.04)
    cut = _find_split(chars, "x", min_gap)
    if cut is None:
        return [chars]
    left = [c for c in chars if (c["x0"] + c["x1"]) / 2 < cut]
    right = [c for c in chars if (c["x0"] + c["x1"]) / 2 >= cut]
    if not left or not right:
        return [chars]
    return _split_columns(left, scale, width, depth + 1) + _split_columns(
        right, scale, width, depth + 1
    )


def _column_x_span(chars: list) -> float:
    if not chars:
        return 0.0
    return max(c["x1"] for c in chars) - min(c["x0"] for c in chars)


def _column_single_line_fraction(lines: list) -> float:
    if not lines:
        return 0.0
    single = sum(1 for ln in lines if len(ln["text"]) <= 2)
    return single / len(lines)


def _is_artifact_column(chars: list, width: float) -> bool:
    """Detect margin strips and vertical watermarks (e.g. arXiv sidebar).

    Docling / MinerU solve this with learned layout regions; here we use
    geometry only: a narrow column whose lines are mostly one glyph tall is not
    part of the reading order.
    """
    if not chars or width <= 0:
        return True
    span = _column_x_span(chars)
    if span <= 0:
        return True
    lines = _group_lines(chars)
    single_frac = _column_single_line_fraction(lines)
    narrow = span / width < MARGIN_COL_WIDTH_RATIO
    if narrow and single_frac >= 0.45:
        return True
    ys = [(c["y0"] + c["y1"]) / 2 for c in chars]
    y_span = max(ys) - min(ys)
    # Vertical text: tall stack, narrow horizontal extent, mostly one char/line.
    if y_span > span * 3.5 and len(chars) >= 8 and single_frac >= 0.35:
        return True
    return False


def _filter_reading_columns(chars: list, scale: float, width: float) -> list:
    """Split into columns and drop margin / watermark strips."""
    cols = _split_columns(chars, scale, width)
    kept = [c for c in cols if not _is_artifact_column(c, width)]
    if kept:
        return kept
    # All columns looked like noise — keep the widest glyph set (main body).
    if len(cols) > 1:
        return [max(cols, key=_column_x_span)]
    return cols


def _merge_orphan_punctuation_lines(lines: list) -> list:
    """Attach lines that are only punctuation to the previous visual line.

    Many PDFs place ``.`` in figure labels or footnotes on a slightly different
    baseline; grouping by y then leaves ``Figure 1`` and ``2:`` on separate lines.
    """
    if not lines:
        return []
    merged: list = []
    for ln in lines:
        t = ln["text"].strip()
        if (
            merged
            and t
            and len(t) <= 4
            and all(c in ".,;:!?…·" or c.isspace() for c in t)
        ):
            suffix = "".join(t.split())
            prev = merged[-1]["text"]
            if suffix and prev and not prev.endswith((" ", "-")):
                merged[-1]["text"] = prev + suffix
            else:
                merged[-1]["text"] = (prev + " " + t).strip()
            continue
        merged.append(dict(ln))
    return merged


def _join_line_glyphs(ln_sorted: list) -> str:
    """Join a visual line's glyphs, inferring word spaces from horizontal gaps."""
    if not ln_sorted:
        return ""
    widths = [c["x1"] - c["x0"] for c in ln_sorted if c["x1"] > c["x0"]]
    med_w = statistics.median(widths) if widths else 1.0
    gap_threshold = med_w * WORD_GAP_WIDTH_RATIO

    parts: list[str] = []
    for i, cur in enumerate(ln_sorted):
        ch = cur["ch"]
        if i == 0:
            parts.append(ch)
            continue
        prev = ln_sorted[i - 1]
        if ch.isspace() or prev["ch"].isspace():
            if not ch.isspace() or (parts and not parts[-1].endswith(" ")):
                parts.append(ch)
            continue
        if cur["x0"] - prev["x1"] > gap_threshold:
            parts.append(" ")
        parts.append(ch)
    return "".join(parts).strip()


def _group_lines(chars: list) -> list:
    """Group a column's glyphs into lines (top-to-bottom, glyphs sorted by x)."""
    if not chars:
        return []
    heights = [c["y1"] - c["y0"] for c in chars if c["y1"] - c["y0"] > 0]
    med_h = statistics.median(heights) if heights else 1.0

    ordered = sorted(chars, key=lambda c: -(c["y0"] + c["y1"]) / 2)
    lines: list = []
    cur: list = []
    ref = None
    for c in ordered:
        yc = (c["y0"] + c["y1"]) / 2
        if ref is None or abs(yc - ref) <= 0.5 * med_h:
            cur.append(c)
            ref = yc if ref is None else ref
        else:
            lines.append(cur)
            cur = [c]
            ref = yc
    if cur:
        lines.append(cur)

    out: list = []
    for ln in lines:
        ln_sorted = sorted(ln, key=lambda c: c["x0"])
        text = _join_line_glyphs(ln_sorted)
        if not text:
            continue
        hs = [c["y1"] - c["y0"] for c in ln_sorted if c["y1"] - c["y0"] > 0]
        out.append({"h": statistics.median(hs) if hs else med_h, "text": text})
    return out


def _segments_to_markdown(lines: list) -> str:
    """Render merged lines to text, promoting visually large lines to headings."""
    if not lines:
        return ""
    body = statistics.median([ln["h"] for ln in lines])

    def level(ln) -> int:
        txt = ln["text"]
        if (
            not DETECT_HEADINGS
            or body <= 0
            or len(txt) > 80
            or len(txt) < MIN_HEADING_LINE_CHARS
        ):
            return 0
        if txt[-1:] in ".。!！?？,，;；:：":
            return 0
        r = ln["h"] / body
        if r >= 2.0:
            return 1
        if r >= 1.6:
            return 2
        if r >= 1.35:
            return 3
        return 0

    levels = [level(ln) for ln in lines]
    # If too many lines qualify, the font sizes are too uniform/noisy to trust.
    if sum(1 for x in levels if x) > max(1, int(0.4 * len(lines))):
        levels = [0] * len(lines)

    out = []
    for ln, lv in zip(lines, levels):
        out.append(("#" * lv + " " + ln["text"]) if lv else ln["text"])
    return "\n".join(out)


def _chars_to_layout_markdown(chars: list, scale: float, width: float) -> str:
    # §5.3 dual-column enhancement: academic two/three-column layouts with
    # full-width blocks (title spanning the gutter, abstract, wide figures).
    # The plain recursive gutter split below cannot cut a page whose title
    # bridges both columns (no clean full-height gap exists); the histogram
    # approach in pdf_dual_column handles this by first lifting out the
    # full-width blocks and only then splitting the remainder.
    try:
        from docreader.parser.pdf_dual_column import (
            detect_column_count,
            detect_full_width_block,
            reconstruct_reading_order,
        )

        n_cols = detect_column_count(chars, width)
        if n_cols >= 2:
            fw_blocks = detect_full_width_block(chars, width, n_cols)
            if fw_blocks:
                blocks: list = []
                for group in reconstruct_reading_order(chars, width, fw_blocks):
                    lines = _merge_orphan_punctuation_lines(_group_lines(group))
                    md = _segments_to_markdown(lines)
                    if md:
                        blocks.append(md)
                if blocks:
                    return "\n".join(blocks)
    except Exception:
        logger.debug("dual-column enhanced ordering failed; using gutter split", exc_info=True)

    blocks = []
    for col in _filter_reading_columns(chars, scale, width):
        lines = _merge_orphan_punctuation_lines(_group_lines(col))
        md = _segments_to_markdown(lines)
        if md:
            blocks.append(md)
    return "\n".join(blocks)


def _layout_line_stats(text: str) -> tuple:
    """Return (line_count, single_char_line_count, punct_only_line_count)."""
    lines = [ln.strip() for ln in text.splitlines() if ln.strip()]
    if not lines:
        return 0, 0, 0
    single = sum(1 for ln in lines if len(ln) <= 2)
    punct_only = sum(
        1
        for ln in lines
        if len(ln) <= 4 and re.fullmatch(r"[\s.,;:!?…·\-–—]+", ln)
    )
    return len(lines), single, punct_only


def _layout_garbled_line_fraction(text: str) -> float:
    """Share of lines that look like broken OCR (many 1–2 letter tokens)."""
    lines = [ln.strip() for ln in text.splitlines() if ln.strip()]
    if not lines:
        return 0.0
    garbled = 0
    for ln in lines:
        words = ln.split()
        if len(words) >= 6 and sum(1 for w in words if len(w) <= 2) / len(words) > 0.45:
            garbled += 1
    return garbled / len(lines)


def _plain_is_well_formed(plain: str) -> bool:
    """True when pdfium plain text already has usable words and punctuation.

    Academic PDFs (arXiv) and TOCs already expose a good text layer; running
    geometric layout on them often destroys citations and words. Scanned books
    with a poor text layer (no commas in refs, short glued tokens) still need
    layout gap inference.
    """
    plain = (plain or "").strip()
    if not plain:
        return False
    if re.search(r"\[\w+,\s", plain):
        return True
    if plain.count(" . . ") >= 2:
        return True
    words = re.findall(r"\S+", plain)
    if len(words) < 30:
        return False
    avg_len = sum(len(w) for w in words) / len(words)
    return avg_len >= 5.0


def _should_prefer_plain(plain: str, layout: str) -> bool:
    """Fall back to pdfium plain text when layout reconstruction looks broken."""
    layout = (layout or "").strip()
    plain = (plain or "").strip()
    if not layout:
        return True
    if not plain:
        return False
    n, single, punct_only = _layout_line_stats(layout)
    if n == 0:
        return True
    if single / n >= 0.18 or punct_only / n >= 0.12:
        return True
    garbled = _layout_garbled_line_fraction(layout)
    if garbled >= 0.20 and _layout_garbled_line_fraction(plain) < 0.08:
        return True
    if re.search(r"\[\w+,\s", plain) and re.search(
        r"\[\w+\s+\w+\s+\d", layout
    ):
        return True
    # Title / lead sentence from plain should survive in layout.
    for ln in plain.splitlines():
        probe = ln.strip()
        if len(probe) < 24:
            continue
        alnum = "".join(c for c in probe if c.isalnum())[:16]
        if len(alnum) < 12:
            continue
        layout_alnum = "".join(c for c in layout if c.isalnum())
        if alnum not in layout_alnum:
            return True
        break
    return False


def _extract_layout_text(page, raw) -> str:
    """Layout-aware extraction: reading order + headings + hidden-text filter.

    Falls back to plain extraction on any failure so a single odd page never
    breaks the document.
    """
    textpage = None
    try:
        textpage = page.get_textpage()
        chars, width = _page_chars(textpage, page, raw)
        if not chars:
            return ""
        heights = [c["y1"] - c["y0"] for c in chars if c["y1"] - c["y0"] > 0]
        scale = (statistics.median(heights) if heights else 1.0) or 1.0
        return _chars_to_layout_markdown(chars, scale, width)
    except Exception:
        logger.debug("layout extraction failed; using plain text", exc_info=True)
        return _extract_page_text(page)
    finally:
        _close_pdfium_resource(textpage)


def _effective_scale(page, scale: float, max_edge: int) -> float:
    """Reduce ``scale`` so the rendered long edge never exceeds ``max_edge`` px.

    Some scanned PDFs declare enormous page boxes; rendering those at the raw
    DPI scale produces 100+ MP JPEGs that exceed the gRPC message limit and are
    far higher resolution than OCR needs.
    """
    if max_edge <= 0:
        return scale
    width, height = page.get_size()
    longest_pt = max(float(width), float(height))
    if longest_pt <= 0:
        return scale
    return min(scale, max_edge / longest_pt)


def _render_page_to_jpeg(page, scale: float, quality: int, max_edge: int = 0) -> bytes:
    bitmap = None
    try:
        bitmap = page.render(scale=_effective_scale(page, scale, max_edge))
        img_obj = bitmap.to_pil()
        if img_obj.mode != "RGB":
            img_obj = img_obj.convert("RGB")
        buf = io.BytesIO()
        img_obj.save(buf, format="JPEG", quality=quality, optimize=True)
        return buf.getvalue()
    finally:
        _close_pdfium_resource(bitmap)


# --- Parallel scanned-page rendering --------------------------------------
# pdfium is NOT thread-safe (concurrent get_page on one document crashes), so
# we parallelise across *processes*: each worker opens its own PdfDocument from
# a temp file and renders an assigned slice of pages. This turns the serial
# per-page render (the dominant cost for big scanned PDFs — hours on
# CPU-constrained containers) into a near-linear speedup.

# Per-worker document handle, populated by the pool initializer.
_WORKER_RENDER_DOC = None

# FR-16：PDFium 在部分输入上会一直缓存已解码的页资源，直到文档关闭才释放。
# 流式路径因此每渲染固定页数就重启一次渲染器，让缓存上界与文档总页数无关，
# 同时保留足够的并行度。
PDF_STREAM_RENDER_BATCH_PAGES = 16


def _render_pool_init(pdf_path: str) -> None:
    global _WORKER_RENDER_DOC
    import pypdfium2 as pdfium

    with open(pdf_path, "rb") as f:
        _WORKER_RENDER_DOC = pdfium.PdfDocument(f.read())


def _render_pool_task(args):
    index, scale, quality, max_edge = args
    page = _WORKER_RENDER_DOC[index]
    try:
        return index, _render_page_to_jpeg(page, scale, quality, max_edge)
    finally:
        _close_pdfium_resource(page)


def _select_mp_context():
    """Pick the safest available multiprocessing start method.

    ``forkserver`` forks workers from a clean, single-threaded server process,
    avoiding the fork-in-a-multithreaded-process hazards of the gRPC server.
    Falls back to ``fork`` and finally returns ``None`` (serial) when neither
    is available (e.g. Windows/dev).
    """
    import multiprocessing as mp

    for method in ("forkserver", "fork"):
        try:
            return mp.get_context(method)
        except ValueError:
            continue
    return None


def _render_pages_parallel(
    content: bytes, indices: list, scale: float, quality: int, max_edge: int, workers: int
) -> dict | None:
    """Render ``indices`` in parallel. Returns ``{index: jpeg_bytes}`` or None.

    Returns None to signal the caller to fall back to serial rendering (when
    parallelism is disabled, only one page is requested, or no usable
    multiprocessing start method exists).
    """
    if workers <= 1 or len(indices) <= 1:
        return None
    ctx = _select_mp_context()
    if ctx is None:
        return None

    import tempfile
    from concurrent.futures import ProcessPoolExecutor

    tmp_path = None
    try:
        with tempfile.NamedTemporaryFile(
            prefix="docreader_render_", suffix=".pdf", delete=False
        ) as tmp:
            tmp.write(content)
            tmp_path = tmp.name

        max_workers = min(workers, len(indices))
        tasks = [(i, scale, quality, max_edge) for i in indices]
        result: dict = {}
        with ProcessPoolExecutor(
            max_workers=max_workers,
            mp_context=ctx,
            initializer=_render_pool_init,
            initargs=(tmp_path,),
        ) as ex:
            for index, jpeg in ex.map(_render_pool_task, tasks, chunksize=4):
                result[index] = jpeg
        return result
    except Exception:
        logger.warning(
            "parallel page rendering failed; falling back to serial",
            exc_info=True,
        )
        return None
    finally:
        if tmp_path:
            try:
                os.unlink(tmp_path)
            except OSError:
                pass


def _render_scanned_pages(
    pdf, content: bytes, indices: list, scale: float, quality: int, max_edge: int
) -> dict:
    """Render the given (scanned) page indices to JPEG bytes.

    Tries process-parallel rendering first (big win for large scanned PDFs),
    transparently falling back to serial rendering on the already-open ``pdf``
    handle when parallelism is unavailable or fails.
    """
    parallel = _render_pages_parallel(
        content, indices, scale, quality, max_edge, CONFIG.pdf_render_parallelism
    )
    if parallel is not None:
        return parallel

    out: dict = {}
    for i in indices:
        page = pdf[i]
        try:
            out[i] = _render_page_to_jpeg(page, scale, quality, max_edge)
        finally:
            _close_pdfium_resource(page)
    return out


# --- FR-16 流式（懒渲染）逐页产出 -----------------------------------------
# 与 _render_scanned_pages 的区别：后者一次性把所有页图放进 dict（大扫描件会在
# 首帧发出前就把上百张 JPEG 全驻内存），下面这组生成器按页产出、按批重启渲染器，
# 内存峰值只与批大小相关。ref_path 里编码了页码，因此顺序与来源关联都不丢：
# 帧的先后即页序，正文里的 images/{base}_page_{n}.jpg 与帧一一对应（FR-05 溯源
# 依赖的 source_blocks 在首帧 metadata 里，先于任何图像发出）。


def _write_pdf_temp_file(content: bytes) -> str:
    """把请求字节落盘，供流式渲染按批反复打开（渲染完即可释放请求内存）。"""
    import tempfile

    path = None
    try:
        with tempfile.NamedTemporaryFile(
            prefix="docreader_stream_", suffix=".pdf", delete=False
        ) as tmp:
            path = tmp.name
            tmp.write(content)
        return path
    except Exception:
        if path is not None:
            _unlink_pdf_temp_file(path)
        raise


def _unlink_pdf_temp_file(path: str) -> None:
    try:
        os.unlink(path)
    except FileNotFoundError:
        pass
    except OSError:
        logger.warning("Failed to remove temporary PDF %s", path, exc_info=True)


def _iter_scanned_pages_from_open_pdf(
    pdf,
    indices: list,
    base_name: str,
    scale: float,
    quality: int,
    max_edge: int,
) -> Iterator[StreamedImage]:
    """Render one page at a time from an already-open PdfDocument."""
    for index in indices:
        with _PDFIUM_LOCK, parser_worker_limit(
            "pdf_render", CONFIG.pdf_render_max_workers
        ):
            page = pdf[index]
            try:
                image_bytes = _render_page_to_jpeg(page, scale, quality, max_edge)
            finally:
                _close_pdfium_resource(page)
        yield StreamedImage(
            ref_path=f"images/{base_name}_page_{index + 1}.jpg",
            image_data=image_bytes,
        )


def _iter_scanned_pages_serial_batch(
    pdf_path: str,
    indices: list,
    base_name: str,
    scale: float,
    quality: int,
    max_edge: int,
) -> Iterator[StreamedImage]:
    import pypdfium2 as pdfium

    with _PDFIUM_LOCK:
        pdf = pdfium.PdfDocument(pdf_path)
    try:
        yield from _iter_scanned_pages_from_open_pdf(
            pdf, indices, base_name, scale, quality, max_edge
        )
    finally:
        with _PDFIUM_LOCK:
            _close_pdfium_resource(pdf)


def _iter_scanned_pages_parallel_batch(
    pdf_path: str,
    indices: list,
    base_name: str,
    scale: float,
    quality: int,
    max_edge: int,
    workers: int,
    mp_context,
) -> Iterator[StreamedImage]:
    from concurrent.futures import ProcessPoolExecutor

    max_workers = min(workers, len(indices))
    # 滑动窗口：在途 future 数固定为 worker 数的两倍，既让 worker 不空转，又
    # 不让「已渲染但还没被消费」的 JPEG 无上限堆积。
    window_size = max_workers * 2
    pending = deque()
    executor = None
    sent = False
    fallback = False

    # Hold the limiter until the executor has shut down, including when the
    # consumer cancels mid-batch. Otherwise another request could start a new
    # pool while the old worker processes still hold their PDFium documents.
    with parser_worker_limit("pdf_render", CONFIG.pdf_render_max_workers):
        try:
            executor = ProcessPoolExecutor(
                max_workers=max_workers,
                mp_context=mp_context,
                initializer=_render_pool_init,
                initargs=(pdf_path,),
            )
            indices_iter = iter(indices)
            for _ in range(window_size):
                try:
                    index = next(indices_iter)
                except StopIteration:
                    break
                pending.append(
                    executor.submit(
                        _render_pool_task,
                        (index, scale, quality, max_edge),
                    )
                )

            while pending:
                future = pending.popleft()
                index, image_bytes = future.result()
                try:
                    next_index = next(indices_iter)
                except StopIteration:
                    pass
                else:
                    pending.append(
                        executor.submit(
                            _render_pool_task,
                            (next_index, scale, quality, max_edge),
                        )
                    )
                sent = True
                yield StreamedImage(
                    ref_path=f"images/{base_name}_page_{index + 1}.jpg",
                    image_data=image_bytes,
                )
        except Exception:
            if sent:
                raise
            logger.warning(
                "parallel streamed PDF rendering failed before the first image; "
                "falling back to serial rendering for this batch",
                exc_info=True,
            )
            fallback = True
        finally:
            if executor is not None:
                for future in pending:
                    future.cancel()
                executor.shutdown(wait=True, cancel_futures=True)
                pending.clear()

    if fallback:
        yield from _iter_scanned_pages_serial_batch(
            pdf_path, indices, base_name, scale, quality, max_edge
        )


def _iter_scanned_pages_from_path(
    pdf_path: str,
    indices: list,
    base_name: str,
    scale: float,
    quality: int,
    max_edge: int,
    workers: int,
) -> Iterator[StreamedImage]:
    """Yield images in fixed-size batches to cap PDFium and result memory."""
    for start in range(0, len(indices), PDF_STREAM_RENDER_BATCH_PAGES):
        batch = indices[start : start + PDF_STREAM_RENDER_BATCH_PAGES]
        mp_context = _select_mp_context() if workers > 1 and len(batch) > 1 else None
        if workers > 1 and len(batch) > 1 and mp_context is not None:
            yield from _iter_scanned_pages_parallel_batch(
                pdf_path,
                batch,
                base_name,
                scale,
                quality,
                max_edge,
                workers,
                mp_context,
            )
        else:
            yield from _iter_scanned_pages_serial_batch(
                pdf_path, batch, base_name, scale, quality, max_edge
            )


def _iter_preprocess_stream(
    images: Iterator[StreamedImage],
    settings: "OCRPipelineSettings",
    quality: int,
) -> Iterator[StreamedImage]:
    """对流式产出的整页图逐帧套用可选 OCR 前处理（FR-13）。

    与批量路径 ``_apply_preprocess`` 等价，只是不一次性持有全部页图。默认关闭
    时零开销直通；依赖缺失或单帧失败都保留原图，绝不因为前处理而中断解析。
    """
    if settings.preprocess_mode == "off":
        yield from images
        return
    try:
        from docreader.parser import image_preprocess
    except Exception as exc:  # pragma: no cover - 模块自身有 import 守卫
        logger.warning("ocr: 前处理模块不可用，流式路径跳过: %s", exc)
        yield from images
        return
    if not image_preprocess.is_available():
        logger.warning(
            "ocr: 前处理不可用（%s），流式路径跳过",
            image_preprocess.unavailable_reason(),
        )
        yield from images
        return

    logger.info(
        "ocr: 流式前处理生效参数 %s",
        image_preprocess.summarize(
            image_preprocess.effective_thresholds(settings.preprocess_params)
        ),
    )
    for image in images:
        try:
            new_data, _result = image_preprocess.preprocess_jpeg(
                image.image_data, settings.preprocess_params, quality=quality
            )
        except Exception as exc:  # pragma: no cover - 防御性
            logger.debug("ocr: 流式前处理失败，保留原图: %s", exc)
            yield image
            continue
        yield StreamedImage(
            ref_path=image.ref_path, image_data=new_data or image.image_data
        )


def _stream_page_count(pdf_path: str) -> int:
    """只读页数，不做任何渲染（流式路径的首帧 metadata 需要它）。"""
    import pypdfium2 as pdfium

    with _PDFIUM_LOCK:
        pdf = pdfium.PdfDocument(pdf_path)
    try:
        return len(pdf)
    finally:
        with _PDFIUM_LOCK:
            _close_pdfium_resource(pdf)


def _select_embedded_images(
    meta: list,
    num_text_pages: int,
    *,
    min_pixels: int = EMBED_MIN_PIXELS,
    min_area_ratio: float = EMBED_MIN_AREA_RATIO,
    repeat_frac: float = EMBED_REPEAT_PAGE_FRAC,
    max_images: int = EMBED_MAX_IMAGES,
) -> list:
    """Decide which embedded-image candidates to keep (pure function).

    ``meta`` is a list of dicts with keys ``page``, ``width``, ``height``,
    ``area_ratio`` and ``hash``. Returns the indices (into ``meta``) to keep,
    after filtering by size, page-area share, cross-page repetition (logos /
    watermarks), exact in-page duplicates and a hard count cap.
    """
    from collections import defaultdict

    hash_pages = defaultdict(set)
    for m in meta:
        hash_pages[m["hash"]].add(m["page"])

    repeat_threshold = max(2, int(num_text_pages * repeat_frac)) if num_text_pages else 2
    banned = {h for h, pages in hash_pages.items() if len(pages) >= repeat_threshold}

    kept: list = []
    seen = set()
    for idx, m in enumerate(meta):
        if m["area_ratio"] < min_area_ratio:
            continue
        if m["width"] < min_pixels or m["height"] < min_pixels:
            continue
        if m["hash"] in banned:
            continue
        key = (m["page"], m["hash"])
        if key in seen:
            continue
        seen.add(key)
        kept.append(idx)
        if len(kept) >= max_images:
            break
    return kept


def _extract_embedded_images(pdf, classes, raw, base_name: str, quality: int) -> dict:
    """Extract filtered embedded figures from native text pages.

    Returns ``{page_index: [(ref_path, base64_jpeg, y_top), ...]}`` ordered so
    callers can place figures after the page text in top-to-bottom order.

    只处理 ``"text"`` 页（四分类下同样如此）：``scanned``/``image``/``mixed``
    三类都会被整页渲染，页面里的图已经在整页图中，再单独抽一遍只会让同一张图
    在正文里出现两次。
    """
    import hashlib

    text_indices = [i for i, c in enumerate(classes) if c == PAGE_CLASS_TEXT]
    if not text_indices:
        return {}

    candidates: list = []  # parallel to meta; holds heavy pixel data
    meta: list = []
    for i in text_indices:
        page = pdf[i]
        try:
            width, height = page.get_size()
            page_area = float(width) * float(height)
            if page_area <= 0:
                continue
            for obj in page.get_objects():
                if obj.type != raw.FPDF_PAGEOBJ_IMAGE:
                    continue
                try:
                    left, bottom, right, top = obj.get_bounds()
                except Exception:
                    continue
                area_ratio = abs((right - left) * (top - bottom)) / page_area
                if area_ratio < EMBED_MIN_AREA_RATIO:
                    continue  # cheap skip before decoding (logos/decorations)
                try:
                    pil = obj.get_bitmap().to_pil()
                except Exception:
                    continue
                content_hash = hashlib.md5(pil.tobytes()).hexdigest()
                candidates.append((i, top, pil))
                meta.append(
                    {
                        "page": i,
                        "width": pil.width,
                        "height": pil.height,
                        "area_ratio": area_ratio,
                        "hash": content_hash,
                    }
                )
        finally:
            _close_pdfium_resource(page)

    kept_idx = _select_embedded_images(meta, len(text_indices))
    if not kept_idx:
        return {}

    from collections import defaultdict

    result: dict = defaultdict(list)
    per_page_count: dict = defaultdict(int)
    max_edge = CONFIG.pdf_render_max_edge
    for idx in kept_idx:
        page_i, y_top, pil = candidates[idx]
        if pil.mode not in ("RGB", "L"):
            pil = pil.convert("RGB")
        if max_edge > 0 and max(pil.size) > max_edge:
            ratio = max_edge / max(pil.size)
            pil = pil.resize(
                (max(1, int(pil.width * ratio)), max(1, int(pil.height * ratio)))
            )
        buf = io.BytesIO()
        pil.save(buf, format="JPEG", quality=quality, optimize=True)
        per_page_count[page_i] += 1
        fname = f"{base_name}_p{page_i+1}_img{per_page_count[page_i]}.jpg"
        ref_path = f"images/{fname}"
        result[page_i].append(
            (ref_path, base64.b64encode(buf.getvalue()).decode("utf-8"), y_top)
        )

    # Top-to-bottom within each page (PDF y grows upward, so larger y first).
    for page_i in result:
        result[page_i].sort(key=lambda item: item[2], reverse=True)
    return result


def _strip_repeating_lines(
    texts: list,
    classes: list,
    text_classes: tuple = MANUAL_TEXT_LAYER_CLASSES,
) -> list:
    """Remove running headers/footers that repeat across most text pages.

    Conservative: only the first/last non-empty line of each text page is a
    candidate, the line must be short, and it must appear on at least 60% of the
    text pages (and there must be enough pages to judge). Mirrors DeepDoc's
    cross-page "garbage set" idea without risking removal of real content.

    ``text_classes`` 是本次解析保留文本层的页类（FR-13）：只有这些页的文本会进
    正文，因此也只在这些页上统计页眉页脚 —— 否则被丢弃的整页渲染页会把阈值算歪。
    """
    from collections import Counter

    text_indices = [i for i, c in enumerate(classes) if c in text_classes]
    if len(text_indices) < 4:
        return list(texts)

    counter: Counter = Counter()
    for i in text_indices:
        lines = [ln.strip() for ln in texts[i].splitlines() if ln.strip()]
        if not lines:
            continue
        for edge in {lines[0], lines[-1]}:
            if len(edge) <= 80:
                counter[edge] += 1

    threshold = max(2, int(len(text_indices) * 0.6))
    repeating = {line for line, count in counter.items() if count >= threshold}
    if not repeating:
        return list(texts)

    cleaned = []
    for i, text in enumerate(texts):
        if classes[i] not in text_classes:
            cleaned.append(text)
            continue
        kept = [ln for ln in text.splitlines() if ln.strip() not in repeating]
        cleaned.append("\n".join(kept))
    return cleaned


def _looks_dual_column(text: str) -> bool:
    """Quick heuristic: does page text look dual-column?"""
    if not text or len(text) < 200:
        return False
    lines = [ln.strip() for ln in text.splitlines() if ln.strip()]
    if len(lines) < 10:
        return False
    # Short lines (typical of column-constrained text) dominate dual-column pages
    short = sum(1 for ln in lines if len(ln) < 60)
    return short / len(lines) > 0.6


# Max formula regions per document (each region renders an image clip and
# possibly runs pix2tex OCR — bounded to keep large papers under control).
MAX_FORMULA_REGIONS = 60


def _extract_and_inject_formulas(
    pdf,
    raw,
    classes: list,
    texts: list,
    base_name: str,
    scale: float,
    quality: int,
    max_edge: int,
    images: dict,
    text_classes: tuple = MANUAL_TEXT_LAYER_CLASSES,
) -> tuple:
    """§5.2: detect formula regions on text pages and LaTeX-OCR them.

    Runs inside the pdfium lock (caller holds it). Fills ``images`` with formula
    clips for the Go-side VLM when LaTeX OCR is unavailable.

    ``text_classes``（FR-13）是本次保留文本层的页类：公式的 LaTeX 结构无法从整页
    渲染图的 OCR 结果里恢复，因此凡是保留文本层的页都要跑一遍，混合页不漏。

    FR-21：不再就地改写 ``texts``，而是返回 ``(total, {page_index: [formula]})``。
    把 ``$$…$$`` 追加进页文本会让 ``locate_lines`` 的「命中数 == 出现数」判定失败
    （同一行公式在页内出现了两次），最坏情况是整页退化成页级定位；因此公式改由
    组装阶段在 ``page_blocks`` 之后作为独立块追加，并带上裁剪区域的页内几何。
    ``texts`` 形参保留是为了不改动调用方的位置参数，函数本身不再修改它。
    """
    from docreader.parser.pdf_formula_extractor import (
        extract_formulas_from_page,
        is_available as formula_ocr_available,
    )

    total = 0
    latex_ok = 0
    formulas_by_page: dict = {}
    text_indices = [i for i, c in enumerate(classes) if c in text_classes]
    for i in text_indices:
        if total >= MAX_FORMULA_REGIONS:
            break
        page = None
        try:
            page = pdf[i]
            formulas = extract_formulas_from_page(
                page, raw, i, base_name, scale, quality, max_edge
            )
            if not formulas:
                continue
            room = MAX_FORMULA_REGIONS - total
            formulas = formulas[:room]
            for f in formulas:
                images[f["ref_path"]] = f["b64"]
                if f.get("latex"):
                    latex_ok += 1
            formulas_by_page[i] = formulas
            total += len(formulas)
        except Exception:
            logger.debug("formula extraction failed on page %d", i, exc_info=True)
        finally:
            if page is not None:
                _close_pdfium_resource(page)
    if total:
        logger.info(
            "PDFParser: formula regions=%d latex_ocr=%d (pix2tex/texify=%s)",
            total, latex_ok, formula_ocr_available(),
        )
    return total, formulas_by_page


# 与 Go 侧 types.SourceLocatorQuoteMax 保持一致：定位块里的 quote 只是原始证据
# 的预览（Go 侧会用实际命中区间覆盖它），截断即可，免得宽表格行撑大响应体。
_LOCATOR_QUOTE_MAX = 300


def _locator_quote(text: str) -> str:
    return text[:_LOCATOR_QUOTE_MAX]


def _formula_locator(formula: dict, page_index: int) -> dict:
    """公式图像裁剪区域的溯源定位（FR-21 规则 3）。

    拿到裁剪区域几何才算精确定位；页面几何缺失时降级为页级并标 ``approx``。
    """
    bbox = formula.get("bbox_norm")
    try:
        line_index = int(formula.get("line_index") or 0)
    except (TypeError, ValueError):
        line_index = 0
    locator = {
        "type": "formula",
        "page": page_index + 1,
        "mapping": "exact" if bbox else "approx",
        "source_id": f"formula:{page_index + 1}:{line_index + 1}",
    }
    if bbox:
        locator["bbox"] = list(bbox)
    latex = formula.get("latex")
    if latex:
        locator["quote"] = _locator_quote(latex)
    return locator


# Max pages to run table extraction on (camelot/pdfplumber re-open the PDF,
# so this is the main cost driver for large documents).
MAX_TABLE_PAGES = 120


def _extract_tables_per_page(
    content: bytes,
    classes: list,
    text_classes: tuple = MANUAL_TEXT_LAYER_CLASSES,
) -> dict:
    """§5.2: extract structured tables per text page.

    Returns {page_index: [TableResult, ...]} or {} when no extraction backend
    (camelot/pdfplumber) is installed. ``TableResult`` 带页码与单元格几何，供
    FR-21 的单元格级溯源使用。

    ``text_classes``（FR-13）是本次保留文本层的页类：表格的行列结构在整页渲染图
    的 OCR 结果里会退化成扁平文本，因此凡是保留文本层的页都要抽一遍，混合页不漏。
    """
    from docreader.parser.pdf_table_extractor import extract_tables_detailed

    tables: dict = {}
    text_indices = [i for i, c in enumerate(classes) if c in text_classes]
    for i in text_indices[:MAX_TABLE_PAGES]:
        try:
            found = extract_tables_detailed(content, i)
            if found:
                tables[i] = found
        except Exception:
            logger.debug("table extraction failed on page %d", i, exc_info=True)
    return tables


def _table_row_locator(table, index: int, geometries: dict):
    """一个 Markdown 行的单元格级定位块（FR-21 规则 1/2/5）。

    精度按 §5.3 逐级降级，且**同时**看几何与页面归属：
    * 该行每个单元格都有几何 → 行区域精确，``mapping="exact"``；
    * 只有表级几何、且该行确实属于表所在的首页 → 退化为表区域，``approx``；
    * 都没有（续页行、扫描件、几何缺失）→ 只留页码，``approx``。
    分隔行（``row_numbers == 0``）不对应原文件里的任何行，不产块。
    """
    row_number = table.row_numbers[index] if index < len(table.row_numbers) else 0
    if row_number <= 0:
        return None

    page_index = table.row_pages[index] if index < len(table.row_pages) else table.page_index
    cells = table.row_cells[index] if index < len(table.row_cells) else []
    geometry = geometries.get(page_index)
    bbox = None
    mapping = "approx"
    if geometry is not None:
        boxes = [c.bbox for c in cells]
        if cells and all(boxes):
            bbox = geometry.bbox(union_boxes(boxes))
            mapping = "exact"
        elif page_index == table.page_index and table.bbox:
            bbox = geometry.bbox(table.bbox)

    locator = {
        "type": "table",
        "page": page_index + 1,
        "mapping": mapping,
        "row_start": row_number,
        "row_end": row_number,
        "table_id": table.table_id,
        "source_id": f"table:{table.table_id}:{row_number}:1",
        "quote": _locator_quote(table.lines[index]),
    }
    if table.ncols:
        locator["col"] = 1
        locator["col_end"] = table.ncols
    if bbox:
        locator["bbox"] = list(bbox)
    return locator


def _formula_parts(formulas_by_page: dict) -> dict:
    """把每页的公式结果换成 ``(Markdown 片段, formula)`` 序列（FR-21）。

    ``pdf_formula_extractor`` 的导入是惰性的（模块加载会尝试导入 pix2tex/texify），
    没有公式时连模块都不必加载。空片段（既无 LaTeX 也无图）直接丢掉。
    """
    if not formulas_by_page:
        return {}
    from docreader.parser.pdf_formula_extractor import formula_markdown

    parts: dict = {}
    for page_index, formulas in formulas_by_page.items():
        pairs = [
            (part, formula)
            for part, formula in ((formula_markdown(f), f) for f in formulas)
            if part
        ]
        if pairs:
            parts[page_index] = pairs
    return parts


def _append_tables(content_text: str, merged_tables: list, geometries: dict) -> tuple:
    """把合并后的表格追加到正文尾部，同时为每个 Markdown 行产出定位块。

    偏移按 ``table.lines`` 累加而不是重新切分 ``table.markdown``：单元格文本里
    可能带换行，重新切分会让行与偏移错位，而错误的偏移比没有偏移更糟。
    返回 ``(新正文, 新增定位块)``。
    """
    blocks: list = []
    parts: list = []
    cursor = len(content_text)
    for table in merged_tables:
        parts.append("\n\n")
        cursor += 2
        for index, line in enumerate(table.lines):
            locator = _table_row_locator(table, index, geometries)
            if locator is not None:
                blocks.append(
                    {"start": cursor, "end": cursor + len(line), "locator": locator}
                )
            cursor += len(line) + 1  # 行间 "\n"
        cursor -= 1  # 最后一行没有换行
        parts.append(table.markdown)
    return content_text + "".join(parts), blocks


class PDFScannedParser(BaseParser):
    """Render every PDF page to a JPEG image.

    Used as a robust last-resort fallback and for image-only PDFs. The Go App
    performs OCR on the extracted page images.

    FR-13：这条路径就是「全部按扫描页处理」的手动流水线，渲染参数（DPI / JPEG
    质量）与可选的 OCR 前处理同样生效；未显式传入配置时按 overrides 解析，
    因此行为与 FR-13 之前一致。
    """

    def __init__(self, file_name: str = "", file_type=None, ocr_settings=None, **kwargs):
        super().__init__(file_name=file_name, file_type=file_type, **kwargs)
        self._ocr_settings = ocr_settings or resolve_ocr_settings(kwargs)

    def parse_into_text(self, content: bytes) -> Document:
        import pypdfium2 as pdfium

        images = {}
        markdown_lines = []
        base_name = os.path.splitext(self.file_name or "document")[0]
        settings = self._ocr_settings

        logger.info(
            "PDFScannedParser: Rendering PDF pages to JPEG images for %s",
            self.file_name,
        )

        try:
            # pdfium lock first, then the render worker limit — same order as
            # PDFParser._route so the two never deadlock against each other.
            with _PDFIUM_LOCK, parser_worker_limit(
                "pdf_render", CONFIG.pdf_render_max_workers
            ):
                pdf = pdfium.PdfDocument(content)
                try:
                    page_count = len(pdf)
                    scale = max(1, settings.render_dpi or CONFIG.pdf_render_dpi) / 72
                    quality = _normalize_image_quality(
                        settings.jpeg_quality or CONFIG.pdf_jpeg_quality
                    )

                    rendered = _render_scanned_pages(
                        pdf,
                        content,
                        list(range(page_count)),
                        scale,
                        quality,
                        CONFIG.pdf_render_max_edge,
                    )
                finally:
                    _close_pdfium_resource(pdf)

            # 可选 OCR 前处理（默认关闭）；失败时原样返回渲染结果。
            rendered, preprocess_summary = _apply_preprocess(rendered, settings)

            source_blocks = []
            offset = 0
            for i in range(page_count):
                page_filename = f"{base_name}_page_{i+1}.jpg"
                ref_path = f"images/{page_filename}"
                part = f"![{page_filename}]({ref_path})"
                markdown_lines.append(part)
                images[ref_path] = base64.b64encode(rendered[i]).decode("utf-8")
                # 扫描件没有可对齐的文本层，只产页级定位（FR-05）。
                source_blocks.append(
                    {
                        "start": offset,
                        "end": offset + len(part),
                        "locator": {
                            "type": "pdf",
                            "page": i + 1,
                            "mapping": "exact",
                        },
                    }
                )
                offset += len(part) + 2  # "\n\n" 分隔符

            text = "\n\n".join(markdown_lines)
            metadata = {
                "image_source_type": "scanned_pdf",
                "page_count": page_count,
                # FR-13：这条路径下每一页都被整页渲染，等价于全部按图片页处理。
                "ocr_pipeline": settings.pipeline,
                "ocr_route": "all_pages_rendered",
                "render_dpi": settings.render_dpi or CONFIG.pdf_render_dpi,
            }
            metadata.update(_preprocess_metadata(preprocess_summary))
            return Document(
                content=text,
                images=images,
                metadata=metadata,
                source_blocks=source_blocks,
            )
        except Exception as e:
            logger.exception("PDFScannedParser failed to parse PDF: %s", e)
            raise e

    def parse_stream(
        self, content: bytes, budget: Optional[ParseBudget] = None
    ) -> ParsedDocumentStream:
        """FR-16：先出「每页一图」的正文与页级溯源块，再逐页懒渲染。

        与非流式路径的差别只在于图像何时物化：正文、source_blocks（FR-05）与
        metadata 完全一致，页图不再在首帧之前全部驻留内存。
        """
        base_name = os.path.splitext(self.file_name or "document")[0]
        settings = self._ocr_settings
        quality = _normalize_image_quality(
            settings.jpeg_quality or CONFIG.pdf_jpeg_quality
        )
        scale = max(1, settings.render_dpi or CONFIG.pdf_render_dpi) / 72

        logger.info(
            "PDFScannedParser(stream): lazy-rendering PDF pages for %s",
            self.file_name,
        )

        # 临时文件让渲染可以按批反复打开/关闭文档，从而把 PDFium 的解码缓存
        # 限制在批大小内；请求字节因此在首帧发出前就能被释放。
        pdf_path = _write_pdf_temp_file(content)
        try:
            page_count = _stream_page_count(pdf_path)
        except Exception:
            _unlink_pdf_temp_file(pdf_path)
            raise

        indices = list(range(page_count))
        markdown_lines = []
        source_blocks = []
        offset = 0
        for i in indices:
            page_filename = f"{base_name}_page_{i+1}.jpg"
            ref_path = f"images/{page_filename}"
            part = f"![{page_filename}]({ref_path})"
            markdown_lines.append(part)
            # 扫描件没有可对齐的文本层，只产页级定位（FR-05）。
            source_blocks.append(
                {
                    "start": offset,
                    "end": offset + len(part),
                    "locator": {
                        "type": "pdf",
                        "page": i + 1,
                        "mapping": "exact",
                    },
                }
            )
            offset += len(part) + 2  # "\n\n" 分隔符

        metadata = {
            "image_source_type": "scanned_pdf",
            "page_count": page_count,
            # FR-13：这条路径下每一页都被整页渲染，等价于全部按图片页处理。
            "ocr_pipeline": settings.pipeline,
            "ocr_route": "all_pages_rendered",
            "render_dpi": settings.render_dpi or CONFIG.pdf_render_dpi,
        }
        # 前处理是逐帧套用的，首帧发出时还不知道汇总结果，因此只上报生效模式。
        metadata.update(_preprocess_metadata({"mode": settings.preprocess_mode}))

        stream = ParsedDocumentStream(
            document=Document(
                content="\n\n".join(markdown_lines),
                images={},
                metadata=metadata,
                source_blocks=source_blocks,
            ),
            image_count=page_count,
            pages_total=page_count,
            _cleanup=lambda: _unlink_pdf_temp_file(pdf_path),
        )
        stream.images = track_stream(
            stream,
            _iter_preprocess_stream(
                _iter_scanned_pages_from_path(
                    pdf_path,
                    indices,
                    base_name,
                    scale,
                    quality,
                    CONFIG.pdf_render_max_edge,
                    CONFIG.pdf_render_parallelism,
                ),
                settings,
                quality,
            ),
            budget,
        )
        return stream


class PDFParser(BaseParser):
    """Per-page router between native text extraction and scanned rendering.

    For each page:
      * native text page  -> keep its text layer (fast, pypdfium2)
      * scanned page      -> render to JPEG, tag ``image_source_type=scanned_pdf``
                             so the Go App OCRs it

    Hybrid documents interleave both in reading order. On any unexpected error
    the parser falls back to rendering all pages as images (safe last resort).

    Force-scanned mode (``pdf_force_scanned=true`` override or
    ``DOCREADER_PDF_FORCE_SCANNED=true`` env) skips classification and
    renders every page as an image.

    FR-13 OCR 流水线：

    * **手动**（默认）—— 按固定参数执行，页面四分类只用于决定「取文本层还是
      整页渲染」，混合页与图片页一律整页渲染且不信任其文本层，逐字保持 FR-13
      之前的行为；
    * **智能**（需在知识库显式开启）—— 先做页面分类（文本页/扫描页/图片页/
      混合页），再按类型选路径：混合页同时保留文本层与整页图，扫描密集文档
      自动提高渲染 DPI。决策轨迹写进 metadata 便于诊断。
    """

    def __init__(self, file_name: str = "", file_type=None, **kwargs):
        # Capture per-upload override before BaseParser consumes kwargs.
        raw = kwargs.pop("pdf_force_scanned", None)
        # FR-13：OCR 流水线（manual/smart）、流水线参数与前处理开关同样来自
        # overrides，优先级 override > env > 默认，与 pdf_force_scanned 一致。
        self._ocr_settings = resolve_ocr_settings(kwargs)
        super().__init__(file_name=file_name, file_type=file_type, **kwargs)
        # Priority: per-upload override > global env > default (False).
        if raw is not None:
            forced = str(raw).strip().lower() in {
                "1", "true", "yes", "y", "on",
            }
        else:
            forced = FORCE_SCANNED_PDF
        # 手动流水线的「全部按扫描页处理」与既有 pdf_force_scanned 等效，
        # 任一为真即整份文档走全页渲染。
        self._force_scanned = bool(forced) or self._ocr_settings.force_scanned

    def parse_into_text(self, content: bytes) -> Document:
        # Force-scanned short-circuit: render every page as an image.
        if self._force_scanned:
            logger.info(
                "PDFParser: force scanned mode enabled for %s (pipeline=%s)",
                self.file_name,
                self._ocr_settings.pipeline,
            )
            doc = PDFScannedParser(
                file_name=self.file_name,
                file_type=self.file_type,
                ocr_settings=self._ocr_settings,
            ).parse_into_text(content)

            # Align metadata fields with automatic scanned route
            page_count = doc.metadata.get("page_count", 0)
            doc.metadata.update({
                "scanned_page_count": page_count,
                "text_page_count": 0,
                "embedded_image_count": 0,
                "vector_figure_count": 0,
            })
            return doc

        try:
            return self._route(content)
        except Exception:
            logger.exception(
                "PDFParser: per-page routing failed for %s; "
                "falling back to full image rendering",
                self.file_name,
            )
            return PDFScannedParser(
                file_name=self.file_name,
                file_type=self.file_type,
                ocr_settings=self._ocr_settings,
            ).parse_into_text(content)

    def parse_stream(
        self, content: bytes, budget: Optional[ParseBudget] = None
    ) -> ParsedDocumentStream:
        """FR-16：先产出全部文本与元数据，再按页懒渲染需要整页图的页。

        与非流式 ``parse_into_text`` 的差别只在于「整页渲染何时发生」：文本层、
        FR-13 四分类路由、FR-05 source_blocks 与 metadata 逐字一致，页图不再在
        首帧之前全部驻留内存。``budget`` 给出时长 / 内存 / 取消上界，触及上界时
        停止产出后续页并置 ``incomplete`` —— 已产出的部分是有效结果，会被保留
        并标注「解析未完成」，而不是丢弃或伪装成完整文档。
        """
        if self._force_scanned:
            stream = PDFScannedParser(
                file_name=self.file_name,
                file_type=self.file_type,
                ocr_settings=self._ocr_settings,
            ).parse_stream(content, budget=budget)
            # Align metadata fields with automatic scanned route
            page_count = stream.document.metadata.get("page_count", 0)
            stream.document.metadata.update(
                {
                    "scanned_page_count": page_count,
                    "text_page_count": 0,
                    "embedded_image_count": 0,
                    "vector_figure_count": 0,
                }
            )
            return stream

        try:
            with _PDFIUM_LOCK:
                document, render_indices = self._route_locked(
                    content, include_scanned_images=False
                )
        except Exception:
            logger.exception(
                "PDFParser: per-page streaming preparation failed for %s; "
                "falling back to full image rendering",
                self.file_name,
            )
            return PDFScannedParser(
                file_name=self.file_name,
                file_type=self.file_type,
                ocr_settings=self._ocr_settings,
            ).parse_stream(content, budget=budget)

        settings = self._ocr_settings
        image_count = len(document.images or {}) + len(render_indices)
        if not render_indices:
            return ParsedDocumentStream(document=document, image_count=image_count)

        base_name = os.path.splitext(self.file_name or "document")[0]
        quality = _normalize_image_quality(
            settings.jpeg_quality or CONFIG.pdf_jpeg_quality
        )
        # 智能流水线的「扫描密集提档」已在 _route_locked 内算好并写进 metadata，
        # 这里直接复用，保证流式与非流式的渲染分辨率一致。
        render_dpi = int(
            document.metadata.get("render_dpi")
            or settings.render_dpi
            or CONFIG.pdf_render_dpi
        )
        scale = max(1, render_dpi) / 72

        logger.info(
            "PDFParser(stream): %s -> %d lazy pages (small_images=%d, dpi=%d)",
            self.file_name,
            len(render_indices),
            len(document.images or {}),
            render_dpi,
        )

        # 每种流式模式都用临时源文件，好让每一批渲染前都能关掉 PdfDocument，
        # 把 PDFium 的解码页缓存限制在与文档长度无关的范围内。
        pdf_path = _write_pdf_temp_file(content)
        stream = ParsedDocumentStream(
            document=document,
            image_count=image_count,
            pages_total=len(render_indices),
            _cleanup=lambda: _unlink_pdf_temp_file(pdf_path),
        )
        stream.images = track_stream(
            stream,
            _iter_preprocess_stream(
                _iter_scanned_pages_from_path(
                    pdf_path,
                    render_indices,
                    base_name,
                    scale,
                    quality,
                    CONFIG.pdf_render_max_edge,
                    CONFIG.pdf_render_parallelism,
                ),
                settings,
                quality,
            ),
            budget,
        )
        return stream

    def _route(self, content: bytes) -> Document:
        # Serialize all pdfium work: see _PDFIUM_LOCK. Holding it for the whole
        # route (both the text pass and the render pass) is what prevents the
        # concurrent-upload deadlock; the trailing markdown assembly is cheap
        # pure-Python so keeping it inside the lock costs nothing meaningful.
        with _PDFIUM_LOCK:
            document, _render_indices = self._route_locked(content)
        return document

    def _route_locked(
        self, content: bytes, *, include_scanned_images: bool = True
    ) -> tuple[Document, list]:
        """按页路由并组装 markdown，返回 ``(Document, 需整页渲染的页索引)``。

        ``include_scanned_images=False``（FR-16 流式路径）时跳过 Pass 2 的整页
        渲染：正文仍然写入 ``images/{base}_page_{n}.jpg`` 占位、source_blocks
        与 metadata 与非流式路径逐字一致，页图则交给懒渲染迭代器按帧产出。
        """
        import pypdfium2 as pdfium
        import pypdfium2.raw as pdfium_r

        base_name = os.path.splitext(self.file_name or "document")[0]
        # FR-13：渲染参数来自知识库的 OCR 流水线配置，未配置时回落服务默认。
        settings = self._ocr_settings
        text_classes = settings.text_layer_classes
        base_dpi = settings.render_dpi or CONFIG.pdf_render_dpi
        quality = _normalize_image_quality(settings.jpeg_quality or CONFIG.pdf_jpeg_quality)
        # 矢量图/公式裁剪沿用基准 DPI；整页渲染可能在智能流水线下被提档。
        scale = max(1, base_dpi) / 72
        render_scale = scale
        render_dpi = base_dpi
        decisions: list = [f"pipeline={settings.pipeline}"]

        pdf = pdfium.PdfDocument(content)
        images: dict = {}
        try:
            page_count = len(pdf)

            # Pass 1: cheap text extraction + image-area classification.
            texts: list = []
            classes: list = []
            # FR-13：保存 Pass 1 的真实图像面积占比，供文档画像与引擎路由使用
            # （此前这里用「扫描页 1.0 / 文本页 0.05」的伪造值，会让
            # avg_image_area_ratio 与 select_engine 的判定失真）。
            ratios: list = []
            vector_clips: dict = {}
            # 引用溯源定位（FR-05）：逐页记录几何与视觉行，供组装阶段定位。
            line_boxes: dict = {}
            geometries: dict = {}
            for i in range(page_count):
                page = pdf[i]
                try:
                    geometries[i] = _page_geometry(page)
                    plain = _extract_page_text(page)
                    ratio = _page_image_area_ratio(page, pdfium_r)
                    cls = _classify_page(
                        ratio,
                        len(plain.strip()),
                        scan_ratio=settings.scan_image_ratio,
                        min_chars=settings.min_chars_per_page,
                    )
                    # 只有保留文本层的页才值得花版面重建的开销；整页渲染的页
                    # 直接交给 Go 侧 OCR/VLM。
                    keep_text = cls in text_classes
                    # 矢量图/内嵌图只在「不整页渲染」的页上抽取：混合页虽然保留
                    # 文本层，但整页图里已经包含了它的所有图形，再抽一遍只会让
                    # 同一张图在正文里出现两次。
                    extract_figures = cls == PAGE_CLASS_TEXT
                    # Layout reconstruction only pays off (and is only spent) on
                    # native text pages; scanned pages are rendered, not read.
                    if keep_text and LAYOUT_ORDERING:
                        if _plain_is_well_formed(plain):
                            text = plain
                        else:
                            layout = _extract_layout_text(page, pdfium_r)
                            if layout and not _should_prefer_plain(plain, layout):
                                text = layout
                            else:
                                text = plain
                    else:
                        text = plain
                    if extract_figures:
                        clips = _extract_vector_figure_clips(
                            page,
                            i,
                            plain,
                            pdfium_r,
                            base_name,
                            scale,
                            quality,
                            CONFIG.pdf_render_max_edge,
                        )
                        if clips:
                            vector_clips[i] = clips
                            for ref_path, b64, _y, _cap in clips:
                                images[ref_path] = b64
                    text = _postprocess_pdf_text(text)
                    if keep_text and SOURCE_BOXES:
                        line_boxes[i] = _page_line_boxes(page, pdfium_r)
                    if extract_figures and vector_clips.get(i):
                        text = _inject_figure_markdown_before_captions(
                            text, vector_clips[i]
                        )
                finally:
                    _close_pdfium_resource(page)
                texts.append(text)
                classes.append(cls)
                ratios.append(ratio)

            texts = _strip_repeating_lines(texts, classes, text_classes)
            text_layer_indices = [i for i, c in enumerate(classes) if c in text_classes]
            # 需要整页渲染的页：扫描页 / 图片页 / 混合页（手动流水线下混合页
            # 不保留文本层，因此与 FR-13 之前的 scanned 集合完全一致）。
            render_indices = [i for i, c in enumerate(classes) if c in RENDER_PAGE_CLASSES]

            # FR-13 智能流水线：需要渲染的页面占比达到阈值即判定为扫描密集文档，
            # 提高整页渲染 DPI（细字/公式在 OCR 前更容易糊掉）。只在高于基准
            # DPI 时提档，避免把运维显式调低的分辨率又抬回去。
            if settings.is_smart and page_count and render_indices:
                render_fraction = len(render_indices) / page_count
                if (
                    render_fraction >= settings.scan_dense_ratio
                    and settings.heavy_render_dpi > base_dpi
                ):
                    render_dpi = settings.heavy_render_dpi
                    render_scale = max(1, render_dpi) / 72
                    decisions.append(
                        f"scan_dense={render_fraction:.2f}>={settings.scan_dense_ratio:.2f}"
                        f"->render_dpi={render_dpi}"
                    )

            # Pass 2: render only the pages that need OCR (heavy work, rate-limited).
            # FR-16：流式路径不在这里渲染，页图由懒渲染迭代器按帧产出。
            preprocess_summary: dict = {}
            if render_indices and include_scanned_images:
                with parser_worker_limit("pdf_render", CONFIG.pdf_render_max_workers):
                    rendered = _render_scanned_pages(
                        pdf,
                        content,
                        render_indices,
                        render_scale,
                        quality,
                        CONFIG.pdf_render_max_edge,
                    )
                # 可选 OCR 前处理（去噪/纠偏/对比度增强），默认关闭。
                rendered, preprocess_summary = _apply_preprocess(rendered, settings)
                for i, img_bytes in rendered.items():
                    ref_path = f"images/{base_name}_page_{i+1}.jpg"
                    images[ref_path] = base64.b64encode(img_bytes).decode("utf-8")

            # Pass 3: extract embedded figures from native text pages so the Go
            # App can OCR/caption them (logos/watermarks/tiny images filtered).
            embedded: dict = {}
            if EXTRACT_EMBEDDED_IMAGES:
                embedded = _extract_embedded_images(
                    pdf, classes, pdfium_r, base_name, quality
                )
                for refs in embedded.values():
                    for ref_path, b64, _y in refs:
                        images[ref_path] = b64

            # Pass 4 (§5.2/§5.3): type-aware specialized extraction.
            # Pre-classify from the collected text so the strategy gates
            # (formula extraction, table priority) apply to this document.
            from docreader.parser.pdf_doc_classifier import (
                classify_document as _pre_classify,
                get_strategy_config,
            )

            pre_cls = _pre_classify(
                "\n\n".join(texts[: min(5, len(texts))])[:5000], page_count, {}
            )
            strategy_cfg = get_strategy_config(pre_cls.strategy)

            formula_count = 0
            formulas_by_page: dict = {}
            if strategy_cfg.get("formula_extraction"):
                # Extract from the pages whose text layer we keep — pages that are
                # rendered whole carry no reliable embedded text, so formula region
                # detection there would be noise. The helper filters by
                # ``text_classes``, so a mixed PDF (some rendered pages) still gets
                # formula extraction on every page that contributes text.
                formula_count, formulas_by_page = _extract_and_inject_formulas(
                    pdf,
                    pdfium_r,
                    classes,
                    texts,
                    base_name,
                    scale,
                    quality,
                    CONFIG.pdf_render_max_edge,
                    images,
                    text_classes=text_classes,
                )

            tables_by_page: dict = {}
            if strategy_cfg.get("table_priority"):
                tables_by_page = _extract_tables_per_page(
                    content, classes, text_classes=text_classes
                )
        finally:
            _close_pdfium_resource(pdf)

        # 按阅读顺序组装 markdown，同时记录每一页落在正文中的位置，供引用溯源
        # 定位（FR-05）把解析结果区间指回原文件的页码与页内几何区域。
        embedded_count = 0
        vector_figure_count = 0
        blocks = []
        source_blocks = []
        offset = 0

        def place(part: str) -> int:
            nonlocal offset
            if blocks:
                offset += 2  # "\n\n" 分隔符
            start = offset
            blocks.append(part)
            offset += len(part)
            return start

        render_set = set(render_indices)
        # FR-21：公式的 Markdown 片段在组装前算好，组装阶段只负责放置与产块。
        formula_parts = _formula_parts(formulas_by_page)

        for i in range(page_count):
            page_only = {"type": "pdf", "page": i + 1, "mapping": "exact"}
            # 先放文本层（含矢量图与内嵌图），再放整页渲染图：文本页只出前者，
            # 图片页/扫描页只出后者，混合页（智能流水线）两者都出。
            if classes[i] in text_classes:
                stripped = texts[i].strip()
                if stripped:
                    start = place(stripped)
                    source_blocks.extend(
                        page_blocks(
                            stripped,
                            start,
                            i + 1,
                            line_boxes.get(i) or [],
                            geometries.get(i),
                        )
                    )
                # 公式块必须在 page_blocks 之后 extend：先把页文本按视觉行定位好，
                # 再为每个公式裁剪区域单独产精确块。反过来（把 $$…$$ 塞进页文本）
                # 会让 locate_lines 的「命中数 == 出现数」判定失败，最坏情况把整页
                # 拖回页级定位（FR-21 规则 3）。
                for part, formula in formula_parts.get(i, ()):
                    fstart = place(part)
                    source_blocks.append(
                        {
                            "start": fstart,
                            "end": fstart + len(part),
                            "locator": _formula_locator(formula, i),
                        }
                    )
                vector_figure_count += len(vector_clips.get(i, []))
                page_images = list(embedded.get(i, []))
                page_images.sort(key=lambda item: item[2], reverse=True)
                for ref_path, _b64, _y in page_images:
                    fname = os.path.basename(ref_path)
                    part = f"![{fname}]({ref_path})"
                    start = place(part)
                    # 内嵌图只产页级定位（图片本身由 Go 侧 OCR/描述）。
                    source_blocks.append(
                        {"start": start, "end": start + len(part), "locator": dict(page_only)}
                    )
                    embedded_count += 1
            if i in render_set:
                page_filename = f"{base_name}_page_{i+1}.jpg"
                part = f"![{page_filename}](images/{page_filename})"
                start = place(part)
                # 整页渲染图只产页级定位：正文来自后续 OCR，没有可对齐的文本层。
                source_blocks.append(
                    {"start": start, "end": start + len(part), "locator": dict(page_only)}
                )

        # 每个 part 都已 strip，拼接无需再裁剪，记录的偏移保持精确。
        assembled = "\n\n".join(blocks)
        content_text = assembled.strip()
        if content_text != assembled:
            source_blocks = rebase_blocks(source_blocks, assembled, content_text)
        # 后续处理（跨页表格、文档类型策略）改写正文后需要重新锚定的基准。
        source_base = content_text

        # --- §5.2: merge cross-page tables and append them to the content ---
        merged_tables: list = []
        try:
            if tables_by_page:
                from docreader.parser.pdf_table_extractor import merge_cross_page_tables

                merged_tables = merge_cross_page_tables(tables_by_page)
                if merged_tables:
                    # FR-21：追加表格的同时逐行产出定位块，每行按**原始页**归属，
                    # 跨页合并不会把续页的行算到首页上。
                    content_text, table_blocks = _append_tables(
                        content_text, merged_tables, geometries
                    )
                    source_blocks.extend(table_blocks)
                    # 表格是纯尾部追加，已有块的偏移依旧有效；把 rebase 基准推进到
                    # 追加之后的正文，末尾再对 apply_type_strategy 的改写统一 rebase
                    # 一次——分散 rebase 会产生错误偏移，而错误偏移比没有更糟。
                    source_base = content_text
                    logger.info(
                        "PDFParser: extracted %d tables (%d merged across pages), "
                        "%d table source blocks",
                        sum(len(v) for v in tables_by_page.values()),
                        len(merged_tables),
                        len(table_blocks),
                    )
        except Exception:
            logger.debug("cross-page table merge failed", exc_info=True)

        # Base metadata (created BEFORE classification/quality/routing so the
        # downstream blocks can safely call metadata.update / metadata[...]=).
        class_counts = {
            name: classes.count(name)
            for name in (
                PAGE_CLASS_TEXT,
                PAGE_CLASS_SCANNED,
                PAGE_CLASS_IMAGE,
                PAGE_CLASS_MIXED,
            )
        }
        # FR-13：把「分类结果如何被用于路由」写进 metadata，便于诊断与前端展示。
        decisions.append(
            "pages={}: text={} scanned={} image={} mixed={}".format(
                page_count,
                class_counts[PAGE_CLASS_TEXT],
                class_counts[PAGE_CLASS_SCANNED],
                class_counts[PAGE_CLASS_IMAGE],
                class_counts[PAGE_CLASS_MIXED],
            )
        )
        decisions.append(
            f"rendered={len(render_indices)} text_layer={len(text_layer_indices)} dpi={render_dpi}"
        )
        if settings.is_smart:
            decisions.append(
                f"mixed_use_text_layer={str(settings.mixed_use_text_layer).lower()}"
            )

        metadata = {
            "page_count": page_count,
            # 沿用既有语义：scanned_page_count = 需要整页渲染（交给 Go 侧 OCR/VLM）
            # 的页数，text_page_count = 保留文本层的页数。手动流水线下两者相加
            # 等于 page_count，与 FR-13 之前逐字一致；智能流水线下混合页同时
            # 计入两者（既渲染又保留文本层）。
            "scanned_page_count": len(render_indices),
            "text_page_count": len(text_layer_indices),
            "embedded_image_count": embedded_count,
            "vector_figure_count": vector_figure_count,
            "formula_count": formula_count,
            "table_count": len(merged_tables),
            "image_source_type": "scanned_pdf" if render_indices else "pdf_text_layer",
            # --- FR-13 页面四分类与流水线决策 ---
            "ocr_pipeline": settings.pipeline,
            "ocr_route": "; ".join(decisions),
            "render_dpi": render_dpi,
            "text_class_page_count": class_counts[PAGE_CLASS_TEXT],
            "scanned_class_page_count": class_counts[PAGE_CLASS_SCANNED],
            "image_page_count": class_counts[PAGE_CLASS_IMAGE],
            "mixed_page_count": class_counts[PAGE_CLASS_MIXED],
        }
        if page_count <= PAGE_CLASSES_METADATA_LIMIT:
            # 逐页类别（逗号串，如 "text,text,mixed,image"），供 Go 侧智能路由
            # 与问题排查使用；超长文档只保留计数，避免白白撑大响应体。
            metadata["page_classes"] = ",".join(classes)
        metadata.update(
            _preprocess_metadata(preprocess_summary or {"mode": settings.preprocess_mode})
        )

        # --- §5.3: Document type classification ---
        doc_classification = None
        try:
            from docreader.parser.pdf_doc_classifier import classify_document
            doc_classification = classify_document(
                content_text[:5000], page_count, metadata
            )
            logger.info(
                "PDFParser: doc_type=%s confidence=%.2f strategy=%s signals=%s",
                doc_classification.doc_type,
                doc_classification.confidence,
                doc_classification.strategy,
                doc_classification.signals,
            )
            metadata["doc_type"] = doc_classification.doc_type
            metadata["doc_type_confidence"] = f"{doc_classification.confidence:.2f}"
        except Exception:
            logger.debug("document classification failed", exc_info=True)

        # --- §5.3: apply type-specific deep parsing strategy ---
        # (contract clause/party/date extraction, financial metric structuring,
        #  resume field extraction, academic citation-chain preservation)
        try:
            from docreader.parser.pdf_type_strategies import (
                apply_type_strategy,
                fields_to_metadata,
            )

            table_md_list = [t.markdown for t in merged_tables] if merged_tables else None
            content_text, structured = apply_type_strategy(
                content_text,
                doc_classification.doc_type if doc_classification else "general",
                table_md_list,
            )
            fields_json = fields_to_metadata(structured)
            if fields_json:
                metadata["structured_fields"] = fields_json
                logger.info(
                    "PDFParser: applied %s strategy, structured fields captured",
                    doc_classification.doc_type if doc_classification else "general",
                )
        except Exception:
            logger.debug("type strategy application failed", exc_info=True)

        # --- §5.4: Quality scoring ---
        quality_report = None
        try:
            from docreader.parser.pdf_quality import score_page, build_quality_report

            # Build extracted image path set for image-loss tracking
            extracted_paths = set(images.keys())
            # FR-16：流式路径（include_scanned_images=False）下整页图还没渲染，
            # 但它们随后一定会按帧产出，因此按同样的 ref_path 计入，保证质量评分
            # 与非流式路径逐字一致。
            if not include_scanned_images:
                extracted_paths.update(
                    f"images/{base_name}_page_{i+1}.jpg" for i in render_indices
                )
            expected_image_refs = len(extracted_paths)

            page_reports = []
            for i in range(page_count):
                # 只有保留文本层的页才有可评分的正文；整页渲染的页正文来自
                # 后续 OCR，此处按空页计（与 FR-13 之前的 scanned 处理一致）。
                if classes[i] in text_classes:
                    page_text = texts[i]
                else:
                    page_text = ""
                page_reports.append(score_page(i, page_text, extracted_paths))

            quality_report = build_quality_report(
                page_reports,
                total_image_refs=expected_image_refs,
                extracted_image_count=expected_image_refs,
            )

            if quality_report.should_retry:
                logger.warning(
                    "PDFParser: parse quality below threshold for %s: "
                    "score=%.2f garble=%.2f empty=%.2f table_dmg=%.2f img_loss=%.2f",
                    self.file_name,
                    quality_report.overall_score,
                    quality_report.garble_rate,
                    quality_report.empty_page_rate,
                    quality_report.table_damage_rate,
                    quality_report.image_loss_rate,
                )

            metadata.update(quality_report.to_metadata())
        except Exception:
            logger.debug("quality scoring failed", exc_info=True)

        # --- §5.1: Auto-routing metadata (for Go-side engine selection) ---
        try:
            from docreader.parser.pdf_auto_router import profile_pdf, select_engine

            # FR-13：用 Pass 1 采到的真实图像面积占比，不再用「扫描页 1.0 /
            # 文本页 0.05」的伪造值 —— 伪造值会让 avg_image_area_ratio 与
            # select_engine 的判定失真，智能流水线复用它时会误判。
            image_ratios = list(ratios)
            text_lengths = []
            for i in range(page_count):
                if i < len(texts) and classes[i] in text_classes:
                    text_lengths.append(len(texts[i]))
                else:
                    # 整页渲染的页没有可用文本层，正文将由 OCR 产出。
                    text_lengths.append(0)

            has_tables = bool(quality_report and any(
                pq.has_table for pq in quality_report.page_reports
            )) or bool(merged_tables)
            has_formulas = formula_count > 0

            profile = profile_pdf(
                page_count=page_count,
                page_image_ratios=image_ratios,
                page_text_lengths=text_lengths,
                has_tables=has_tables,
                has_formulas=has_formulas,
                is_dual_column=any(
                    _looks_dual_column(texts[i]) for i in range(min(3, len(texts)))
                ),
            )

            route = select_engine(profile)
            metadata.update({
                "route_engine": route.engine,
                "route_reason": route.reason,
            })
        except Exception:
            logger.debug("auto-routing profile failed", exc_info=True)

        logger.info(
            "PDFParser: %s -> %d pages (rendered=%d text_layer=%d | "
            "text=%d scanned=%d image=%d mixed=%d), pipeline=%s "
            "embedded_images=%d, content_len=%d",
            self.file_name,
            page_count,
            len(render_indices),
            len(text_layer_indices),
            class_counts[PAGE_CLASS_TEXT],
            class_counts[PAGE_CLASS_SCANNED],
            class_counts[PAGE_CLASS_IMAGE],
            class_counts[PAGE_CLASS_MIXED],
            settings.pipeline,
            embedded_count,
            len(content_text),
        )
        # 跨页表格追加与文档类型策略会改写正文，定位块必须重新锚定到最终正文，
        # 否则错误的偏移会把引用高亮到无关位置（FR-05）。
        source_blocks = rebase_blocks(source_blocks, source_base, content_text)

        document = Document(
            content=content_text,
            images=images,
            metadata=metadata,
            source_blocks=source_blocks,
        )
        # FR-16：把「哪些页还需要整页渲染」交给调用方，流式路径据此懒渲染。
        return document, render_indices
