"""Formula (equation) extraction from PDF pages.

Implements §5.2 formula parsing: convert image-based math formulas to
LaTeX using pix2tex (LaTeX-OCR) or Texify, for academic/scientific PDFs.

Strategy:
1. Detect formula regions using heuristics (glyph density, special chars).
2. Render detected regions as image clips.
3. Pass clips to pix2tex/Texify for LaTeX conversion.
4. Emit LaTeX in markdown: $$...$$ for display, $...$ for inline.

All ML dependencies are optional; if none is installed, formula regions
are still detected and rendered as images for the Go-side VLM to caption.
"""

import base64
import io
import logging
import re
import statistics
from typing import List, Optional, Tuple

logger = logging.getLogger(__name__)

# Try importing optional formula OCR backends
try:
    from pix2tex.cli import LatexOCR
    _HAS_PIX2TEX = True
except ImportError:
    _HAS_PIX2TEX = False

try:
    import texify  # Texify by VikParikh
    _HAS_TEXIFY = True
except ImportError:
    _HAS_TEXIFY = False


# Heuristics for detecting formula regions in text layers
# Lines with heavy math symbols but few natural-language words
_MATH_SYMBOLS_RE = re.compile(
    r"[∑∫∂∇√∞∈∉∀∃¬∧∨⊕⊗≤≥≠≈≡∝±×÷≤≥⊂⊃⊆⊇∩∪]"
    r"|\\(?:sum|int|frac|sqrt|alpha|beta|gamma|delta|theta|lambda|mu|pi|sigma|omega)"
)
# Inline math: $...$ or $$...$$
_INLINE_MATH_RE = re.compile(r"\$[^\$]+\$")
# LaTeX-like commands
_LATEX_CMD_RE = re.compile(r"\\[a-zA-Z]+(?:\{[^}]*\})?")
# Lines that are mostly symbols/operators, not words
_SYMBOL_DENSITY_RE = re.compile(r"[^\w\s.,;:!?\'\"()\-]")


def detect_formula_lines(text: str) -> List[Tuple[int, str]]:
    """Return [(line_number, line_text), ...] for lines likely containing formulas."""
    results = []
    for i, line in enumerate(text.splitlines()):
        stripped = line.strip()
        if not stripped:
            continue
        if _INLINE_MATH_RE.search(stripped):
            results.append((i, stripped))
            continue
        if _LATEX_CMD_RE.search(stripped):
            results.append((i, stripped))
            continue
        if _MATH_SYMBOLS_RE.search(stripped):
            results.append((i, stripped))
            continue
        # High symbol density: >40% non-word, non-space chars
        total = len(stripped)
        symbols = len(_SYMBOL_DENSITY_RE.findall(stripped))
        if total > 10 and symbols / total > 0.40:
            results.append((i, stripped))
    return results


def extract_formulas_from_page(
    page,
    raw,
    page_index: int,
    base_name: str,
    scale: float,
    quality: int,
    max_edge: int,
) -> List[dict]:
    """Extract formula regions from a PDF page.

    Returns:
        List of dicts: {
            "ref_path": str,       # image path for VLM fallback
            "latex": Optional[str], # LaTeX if OCR succeeded
            "b64": str,            # base64 JPEG
            "page": int,
            "is_display": bool,    # display (block) vs inline
            # --- FR-21：公式图像裁剪区域的位置，供单元格级溯源使用 ---
            "line_index": int,     # 该公式在页面文本层中的行号（0 基）
            "bbox": tuple,         # 裁剪区域，PDF 点、左下原点
            "bbox_norm": list|None, # 同一区域的 0-1 归一化 bbox（左上原点）
        }
    """
    results = []

    # Step 1: Detect formula regions from the text layer
    textpage = None
    try:
        textpage = page.get_textpage()
        plain = textpage.get_text_range()
    except Exception:
        return results
    finally:
        if textpage:
            try:
                textpage.close()
            except Exception:
                pass

    formula_lines = detect_formula_lines(plain)
    if not formula_lines:
        return results

    geometry = _page_geometry(page)

    # Step 2: For each formula line, get its bounding box and render as image
    for line_idx, line_text in formula_lines:
        bbox = _line_bbox(page, raw, line_idx)
        if bbox is None:
            continue

        try:
            jpeg = _render_clip(page, bbox, scale, quality, max_edge)
        except Exception:
            continue

        ref_path = f"images/{base_name}_p{page_index+1}_eq{line_idx+1}.jpg"
        b64 = base64.b64encode(jpeg).decode("utf-8")

        # Step 3: Try LaTeX OCR
        latex = _ocr_formula(jpeg)

        results.append({
            "ref_path": ref_path,
            "latex": latex,
            "b64": b64,
            "page": page_index,
            "is_display": len(line_text) > 50,  # heuristic
            "line_index": line_idx,
            "bbox": tuple(bbox),
            "bbox_norm": _normalize_bbox(geometry, bbox),
        })

    return results


def _page_geometry(page):
    """页面几何（CropBox + /Rotate）；拿不到就返回 None，定位降级为页级。"""
    try:
        from docreader.parser.source_locator import PageGeometry

        return PageGeometry(page.get_cropbox(), page.get_rotation())
    except Exception:
        logger.debug("formula page geometry unavailable", exc_info=True)
        return None


def _normalize_bbox(geometry, bbox) -> Optional[List[float]]:
    """把 PDF 点 bbox 归一化成 0-1（左上原点），失败时返回 None。"""
    if geometry is None or not bbox:
        return None
    try:
        return geometry.bbox(bbox)
    except Exception:
        logger.debug("formula bbox normalization failed", exc_info=True)
        return None


def _line_bbox(page, raw, line_index: int):
    """Get the bounding box of a text line by index on a page."""
    textpage = None
    try:
        textpage = page.get_textpage()
        n = textpage.count_chars()
        if n <= 0:
            return None

        # Group chars into lines
        chars = []
        for i in range(n):
            try:
                left, bottom, right, top = textpage.get_charbox(i)
            except Exception:
                continue
            ch = textpage.get_text_range(i, 1)
            if ch in ("\r", "\n"):
                continue
            chars.append({
                "x0": min(left, right),
                "y0": min(bottom, top),
                "x1": max(left, right),
                "y1": max(bottom, top),
                "ch": ch,
            })

        if not chars:
            return None

        # Group by y (line)
        heights = [c["y1"] - c["y0"] for c in chars if c["y1"] > c["y0"]]
        med_h = statistics.median(heights) if heights else 1.0

        ordered = sorted(chars, key=lambda c: -(c["y0"] + c["y1"]) / 2)
        lines = []
        cur = []
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

        if line_index >= len(lines):
            return None

        line_chars = lines[line_index]
        x0 = min(c["x0"] for c in line_chars)
        y0 = min(c["y0"] for c in line_chars)
        x1 = max(c["x1"] for c in line_chars)
        y1 = max(c["y1"] for c in line_chars)

        # Pad the bbox slightly for better OCR
        pad = max(2.0, (x1 - x0) * 0.05)
        return (max(0, x0 - pad), y0, x1 + pad, y1 + pad)
    except Exception:
        return None
    finally:
        if textpage:
            try:
                textpage.close()
            except Exception:
                pass


def _render_clip(page, bbox, scale: float, quality: int, max_edge: int) -> bytes:
    """Render a page region to JPEG."""
    from docreader.parser.pdf_parser import _render_page_clip_jpeg
    return _render_page_clip_jpeg(page, bbox, scale, quality, max_edge)


def _ocr_formula(jpeg_bytes: bytes) -> Optional[str]:
    """Convert a formula image to LaTeX using available OCR backends."""
    from PIL import Image

    try:
        img = Image.open(io.BytesIO(jpeg_bytes))
    except Exception:
        return None

    # Try pix2tex first (lighter weight)
    if _HAS_PIX2TEX:
        try:
            model = _get_pix2tex_model()
            if model:
                latex = model(img)
                if latex and _validate_latex(latex):
                    return latex.strip()
        except Exception as e:
            logger.debug("pix2tex OCR failed: %s", e)

    # Try Texify
    if _HAS_TEXIFY:
        try:
            latex = texify.latex(img)
            if latex and _validate_latex(latex):
                return latex.strip()
        except Exception as e:
            logger.debug("Texify OCR failed: %s", e)

    return None


# Singleton model instances (loaded once, reused)
_PIX2TEX_MODEL = None


def _get_pix2tex_model():
    """Get or lazily load the pix2tex model singleton."""
    global _PIX2TEX_MODEL
    if _PIX2TEX_MODEL is None:
        try:
            _PIX2TEX_MODEL = LatexOCR()
        except Exception as e:
            logger.warning("Failed to load pix2tex model: %s", e)
            return None
    return _PIX2TEX_MODEL


def _validate_latex(latex: str) -> bool:
    """Basic validation: reject empty or garbage output."""
    if not latex or len(latex.strip()) < 2:
        return False
    # Reject if it's just repeated characters
    if len(set(latex.strip())) < 3:
        return False
    return True


def formula_markdown(formula: dict) -> str:
    """一个公式区域对应的 Markdown 片段；无 LaTeX 也无图时返回空串。"""
    latex = formula.get("latex")
    if latex:
        return f"$$ {latex} $$" if formula.get("is_display") else f"${latex}$"
    ref = formula.get("ref_path", "")
    return f"![formula]({ref})" if ref else ""


def inject_formula_markdown(text: str, formulas: List[dict]) -> str:
    """Inject LaTeX formulas into the page text as $$...$$ blocks.

    注意：把公式追加进页文本会让 ``source_locator.locate_lines`` 的「命中数 ==
    出现数」判定失败（同一行公式在页内出现两次），进而把整页拖回页级定位。
    PDF 流水线因此改为在页文本块之后把公式作为独立块追加（见 pdf_parser）。
    """
    if not formulas:
        return text

    lines = text.splitlines()
    for f in formulas:
        part = formula_markdown(f)
        if part:
            lines.append(part)
    return "\n".join(lines)


def is_available() -> bool:
    """Check if any formula OCR backend is available."""
    return _HAS_PIX2TEX or _HAS_TEXIFY


def availability_info() -> dict:
    return {
        "pix2tex": _HAS_PIX2TEX,
        "texify": _HAS_TEXIFY,
    }
