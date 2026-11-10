"""Table extraction from PDF pages.

Implements §5.2: specialised table parsing that goes beyond pypdfium2's
text-layer extraction.

Strategy (enterprise-grade, inspired by Camelot / Table Transformer):
1. Try camelot (lattice + stream modes) — best for ruled and unruled tables.
2. Fall back to pdfplumber table extraction (good for borderless tables).
3. Merge multi-page tables by header-row similarity.
4. Emit standard Markdown tables.

All external dependencies are optional; if neither camelot nor pdfplumber
is installed, the module gracefully returns None and the caller keeps the
text-layer output.

FR-21（单元格级溯源）：除 Markdown 之外，本模块还透出每张表的**页码**、表级
几何区域，以及每个单元格的几何区域（camelot 用 ``table.cells``、pdfplumber 用
``Table.rows[i].cells``）。跨页合并在拼行的**同时**累积每行的原始页
（``MergedTable.row_pages``），因此合并后的续页行不会丢掉自己的页归属。
拿不到单元格几何时按 §5.3 逐级降级（行区域 → 表区域 → 页级），绝不阻断解析。
"""

import io
import logging
import re
from dataclasses import dataclass, field
from typing import Dict, List, Optional, Tuple

from docreader.parser.source_locator import union_boxes

logger = logging.getLogger(__name__)

# Try importing optional table extraction backends
try:
    import camelot
    _HAS_CAMELOT = True
except ImportError:
    _HAS_CAMELOT = False

try:
    import pdfplumber
    _HAS_PDFPLUMBER = True
except ImportError:
    _HAS_PDFPLUMBER = False


_TABLE_ROW_RE = re.compile(r"^\|.*\|\s*$")
_TABLE_SEP_RE = re.compile(r"^\|[\s:|-]+\|\s*$")

# PDF 用户空间 bbox：(x0, y0, x1, y1)，左下原点，y0 为下边、y1 为上边 ——
# 与 source_locator.PageGeometry.bbox 的输入约定一致。
BBox = Tuple[float, float, float, float]


@dataclass
class TableCell:
    """一个单元格的文本与其几何区域；``bbox`` 为 None 表示引擎没给出该框。"""

    text: str
    bbox: Optional[BBox] = None


@dataclass
class TableResult:
    """一页上一张表的结构化提取结果。

    ``lines`` 是 Markdown 行（表头 / 分隔行 / 数据行），``row_cells`` 与它逐项
    对应。定位偏移一律按 ``lines`` 累加，而不是重新切分 ``markdown``：单元格
    文本里可能带换行，重新切分会让行与偏移错位。
    """

    page_index: int  # 0 基
    lines: List[str] = field(default_factory=list)
    row_cells: List[List[TableCell]] = field(default_factory=list)
    bbox: Optional[BBox] = None  # 表级区域（PDF 点）

    @property
    def markdown(self) -> str:
        return "\n".join(self.lines)


@dataclass
class MergedTable:
    """跨页合并后的一张表（FR-21 规则 2：每个 Markdown 行都记得自己的原始页）。

    ``row_pages`` / ``row_numbers`` / ``row_cells`` 与 ``lines`` 逐项对应，其中
    ``row_numbers`` 为 1 基逻辑行号、分隔行为 0（分隔行不产定位块）。
    """

    page_index: int  # 0 基首页
    table_id: str
    lines: List[str] = field(default_factory=list)
    row_pages: List[int] = field(default_factory=list)
    row_numbers: List[int] = field(default_factory=list)
    row_cells: List[List[TableCell]] = field(default_factory=list)
    bbox: Optional[BBox] = None  # 首页表级区域

    @property
    def markdown(self) -> str:
        return "\n".join(self.lines)

    @property
    def ncols(self) -> int:
        return max((len(row) for row in self.row_cells), default=0)


def extract_tables_from_page(
    pdf_content: bytes,
    page_index: int,
    flavor: str = "auto",
) -> List[str]:
    """Extract tables from a single PDF page as Markdown strings.

    向后兼容入口：需要页码与单元格几何时用 :func:`extract_tables_detailed`。

    Args:
        pdf_content: Raw PDF file bytes.
        page_index: 0-based page index.
        flavor: "lattice" (ruled tables), "stream" (borderless), or "auto".

    Returns:
        List of Markdown table strings. Empty if no tables found or
        no extraction backend is available.
    """
    return [
        table.markdown
        for table in extract_tables_detailed(pdf_content, page_index, flavor)
        if table.lines
    ]


def extract_tables_detailed(
    pdf_content: bytes,
    page_index: int,
    flavor: str = "auto",
) -> List[TableResult]:
    """提取一页上的表格，连同页码与单元格几何（FR-21）。

    与 :func:`extract_tables_from_page` 的双引擎策略完全一致（camelot 优先、
    pdfplumber 兜底），只是不再把几何信息丢掉。
    """
    if not _HAS_CAMELOT and not _HAS_PDFPLUMBER:
        return []

    results: List[TableResult] = []

    # --- Camelot path (preferred for structured tables) ---
    if _HAS_CAMELOT:
        try:
            results.extend(_extract_camelot(pdf_content, page_index, flavor))
        except Exception as e:
            logger.debug("Camelot extraction failed on page %d: %s", page_index, e)

    # --- pdfplumber path (fallback for borderless tables) ---
    if not results and _HAS_PDFPLUMBER:
        try:
            results.extend(_extract_pdfplumber(pdf_content, page_index))
        except Exception as e:
            logger.debug("pdfplumber extraction failed on page %d: %s", page_index, e)

    return results


def _extract_camelot(
    pdf_content: bytes, page_index: int, flavor: str
) -> List[TableResult]:
    """Extract tables using Camelot (lattice + stream modes)."""
    import tempfile

    results: List[TableResult] = []

    # Write to temp file because Camelot needs a file path
    with tempfile.NamedTemporaryFile(suffix=".pdf", delete=False) as tmp:
        tmp.write(pdf_content)
        tmp_path = tmp.name

    try:
        page_str = str(page_index + 1)  # Camelot uses 1-based pages

        modes = []
        if flavor == "auto":
            modes = ["lattice", "stream"]
        else:
            modes = [flavor]

        for mode in modes:
            try:
                tables = camelot.read_pdf(
                    tmp_path,
                    pages=page_str,
                    flavor=mode,
                    strip_text="\n",
                )
                for table in tables:
                    result = _camelot_result(table, page_index)
                    if result is not None:
                        results.append(result)
            except Exception:
                continue

            if results:
                break  # First successful mode wins
    finally:
        import os
        try:
            os.unlink(tmp_path)
        except OSError:
            pass

    return results


def _camelot_result(table, page_index: int) -> Optional[TableResult]:
    """把 camelot 的 Table 转成结构化结果，保留 ``table.cells`` 的单元格几何。"""
    df = getattr(table, "df", None)
    if df is None or getattr(df, "empty", True):
        return None
    grid = [[h for h in df.columns]]
    grid.extend([list(row) for row in df.values.tolist()])
    grid = _normalize_grid(grid)
    if not grid:
        return None

    ncols = len(grid[0])
    cells = [_normalize_box(box) for box in (getattr(table, "cells", None) or ())]
    aligned = _align_cell_boxes(cells, len(grid), ncols)
    row_cells = [
        [TableCell(text=grid[r][c], bbox=aligned.get((r, c))) for c in range(ncols)]
        for r in range(len(grid))
    ]

    bbox = _normalize_box(getattr(table, "_bbox", None))
    if bbox is None:
        bbox = union_boxes(aligned.values())
    return TableResult(
        page_index=page_index,
        lines=_grid_to_lines(grid),
        row_cells=row_cells,
        bbox=bbox,
    )


def _align_cell_boxes(
    cells: List[Optional[BBox]], n_rows: int, ncols: int
) -> Dict[Tuple[int, int], Optional[BBox]]:
    """把行主序的单元格框对齐到 ``(行, 列)``。

    camelot 的首行可能已经被 pandas 吃成列名，因此 ``cells`` 的行数既可能等于
    Markdown 行数、也可能少一行；两种都试，都对不上就返回空表（调用方据此退化
    为表级区域，而不是猜一个错位几何）。
    """
    if ncols <= 0 or not cells:
        return {}
    for rows, offset in ((n_rows, 0), (n_rows - 1, 1)):
        if rows > 0 and len(cells) == rows * ncols:
            return {
                (offset + i // ncols, i % ncols): cells[i] for i in range(len(cells))
            }
    return {}


def _extract_pdfplumber(pdf_content: bytes, page_index: int) -> List[TableResult]:
    """Extract tables using pdfplumber (borderless table support)."""
    results: List[TableResult] = []
    with pdfplumber.open(io.BytesIO(pdf_content)) as pdf:
        if page_index >= len(pdf.pages):
            return []
        page = pdf.pages[page_index]
        page_height = float(getattr(page, "height", 0) or 0)

        found = None
        try:
            # find_tables 才带 cells/rows 几何；extract_tables 只有文本。
            found = page.find_tables()
        except Exception:
            logger.debug(
                "pdfplumber find_tables failed on page %d", page_index, exc_info=True
            )
        for table in found or ():
            result = _plumber_result(table, page_index, page_height)
            if result is not None:
                results.append(result)
        if results:
            return results

        # 几何拿不到时退回纯文本抽取：仍然有表格结构，只是定位降级为页级。
        for table_data in page.extract_tables() or ():
            md = _rows_to_markdown(table_data)
            if md:
                results.append(_coerce_table(md, page_index))
    return results


def _plumber_result(table, page_index: int, page_height: float) -> Optional[TableResult]:
    """把 pdfplumber 的 Table 转成结构化结果（坐标换算成左下原点）。"""
    try:
        data = table.extract()
    except Exception:
        logger.debug("pdfplumber table extract failed", exc_info=True)
        return None
    grid = _normalize_grid(data)
    if not grid:
        return None

    rows = list(getattr(table, "rows", None) or ())
    row_cells: List[List[TableCell]] = []
    for r, row in enumerate(grid):
        boxes: List[Optional[BBox]] = []
        if r < len(rows):
            boxes = [
                _plumber_box(box, page_height)
                for box in (getattr(rows[r], "cells", None) or ())
            ]
        row_cells.append(
            [
                TableCell(text=row[c], bbox=boxes[c] if c < len(boxes) else None)
                for c in range(len(row))
            ]
        )
    return TableResult(
        page_index=page_index,
        lines=_grid_to_lines(grid),
        row_cells=row_cells,
        bbox=_plumber_box(getattr(table, "bbox", None), page_height),
    )


def _plumber_box(box, page_height: float) -> Optional[BBox]:
    """pdfplumber 用左上原点坐标，换算成 PageGeometry 需要的左下原点。

    假定 CropBox 原点为 (0, 0)（绝大多数 PDF 如此）；否则几何会整体平移，
    但归一化时会被夹到页面边界内，不会产生跨页的错误定位。
    """
    normalized = _normalize_box(box)
    if normalized is None or page_height <= 0:
        return None
    x0, top, x1, bottom = normalized
    return (x0, page_height - bottom, x1, page_height - top)


def _normalize_box(box) -> Optional[BBox]:
    """把引擎给出的 4 元组规整成 x0<x1、y0<y1 的浮点 bbox。"""
    if not box or len(box) != 4:
        return None
    try:
        x0, y0, x1, y1 = (float(v) for v in box)
    except (TypeError, ValueError):
        return None
    return (min(x0, x1), min(y0, y1), max(x0, x1), max(y0, y1))


def _normalize_grid(rows) -> List[List[str]]:
    """把原始行数据规整成等列数的字符串网格；不足两行（无数据行）时返回空。"""
    if not rows or len(rows) < 2:
        return []
    normalized = [
        [str(c).strip() if c is not None else "" for c in row] for row in rows
    ]
    max_cols = max(len(r) for r in normalized)
    if max_cols == 0:
        return []
    for row in normalized:
        while len(row) < max_cols:
            row.append("")
    return normalized


def _grid_to_lines(grid: List[List[str]]) -> List[str]:
    """把规整后的网格转成 Markdown 行（首行为表头，第二行为分隔行）。"""
    if not grid:
        return []
    ncols = len(grid[0])
    lines = ["| " + " | ".join(grid[0]) + " |"]
    lines.append("| " + " | ".join(["---"] * ncols) + " |")
    lines.extend("| " + " | ".join(row) + " |" for row in grid[1:])
    return lines


def _rows_to_markdown(rows: List[List]) -> str:
    """Convert a list of rows (list of cell values) to a Markdown table."""
    return "\n".join(_grid_to_lines(_normalize_grid(rows)))


def _split_row(line: str) -> List[str]:
    """把一个 Markdown 表格行拆成单元格文本。"""
    return [c.strip() for c in line.strip().strip("|").split("|")]


def _coerce_table(value, page_index: int) -> TableResult:
    """把裸 Markdown（旧的 ``List[str]`` 结果）包成 TableResult。

    没有几何信息时按「只有行文本」降级：定位块最终只带页码与 ``approx``，
    符合 §5.3「精度不足就降级、但绝不阻断」。
    """
    if isinstance(value, TableResult):
        return value
    md = value if isinstance(value, str) else (getattr(value, "markdown", "") or "")
    lines = md.strip().splitlines()
    return TableResult(
        page_index=page_index,
        lines=lines,
        row_cells=[[TableCell(text=c) for c in _split_row(line)] for line in lines],
        bbox=None,
    )


def merge_cross_page_tables(tables_by_page: dict) -> List[MergedTable]:
    """Merge tables that span multiple pages by header similarity.

    Args:
        tables_by_page: {page_index: [TableResult | markdown str, ...]}

    Returns:
        List of :class:`MergedTable`, one per unique table. Continuation pages
        are merged into the first occurrence, and every Markdown row keeps the
        page it was originally read from (``row_pages``).
    """
    if not tables_by_page:
        return []

    merged: List[MergedTable] = []
    used_pages = set()

    sorted_pages = sorted(tables_by_page.keys())
    coerced = {
        page: [t for t in (_coerce_table(v, page) for v in (tables_by_page.get(page) or [])) if t.lines]
        for page in sorted_pages
    }

    for page_i in sorted_pages:
        if page_i in used_pages:
            continue

        tables = coerced.get(page_i) or []
        if not tables:
            continue

        for table in tables:
            header = _extract_table_header(table.markdown)
            if not header:
                merged.append(_as_merged(table, len(merged) + 1))
                continue

            # Look ahead for continuation pages with matching header
            combined = _as_merged(table, len(merged) + 1)
            for next_page in range(page_i + 1, max(sorted_pages) + 1):
                if next_page in used_pages:
                    continue
                matched = False
                for nt in coerced.get(next_page) or []:
                    next_header = _extract_table_header(nt.markdown)
                    if next_header and _header_similarity(header, next_header) > 0.6:
                        # Merge: append data rows (skip the continuation's header)
                        _merge_table_into(combined, nt)
                        used_pages.add(next_page)
                        matched = True
                        break
                if not matched:
                    break  # No continuation found, stop looking

            merged.append(combined)

    return merged


def _as_merged(table: TableResult, seq: int) -> MergedTable:
    """把单页表包装成 MergedTable；``seq`` 是该页序内的表序号（1 基）。"""
    row_numbers: List[int] = []
    number = 0
    for line in table.lines:
        if _TABLE_SEP_RE.match(line):
            row_numbers.append(0)  # 分隔行没有对应的原始行
            continue
        number += 1
        row_numbers.append(number)
    return MergedTable(
        page_index=table.page_index,
        table_id=f"pdf:{table.page_index + 1}:{seq}",
        lines=list(table.lines),
        row_pages=[table.page_index] * len(table.lines),
        row_numbers=row_numbers,
        row_cells=list(table.row_cells),
        bbox=table.bbox,
    )


def _merge_table_into(target: MergedTable, other: TableResult) -> None:
    """把续页的数据行接到 ``target`` 上，同时记下这些行的**原始页**。

    行页归属必须在拼行的同时累积：事后从合并后的 Markdown 反推不可行，重复的
    行文本无法区分它来自哪一页。
    """
    # other = 表头 + 分隔行 + 数据行；只接数据行。
    if len(other.lines) < 3:
        return
    number = max(target.row_numbers) if target.row_numbers else 0
    for index in range(2, len(other.lines)):
        number += 1
        target.lines.append(other.lines[index])
        target.row_pages.append(other.page_index)
        target.row_numbers.append(number)
        target.row_cells.append(
            other.row_cells[index] if index < len(other.row_cells) else []
        )


def _extract_table_header(md_table: str) -> Optional[List[str]]:
    """Extract the header row from a Markdown table."""
    lines = md_table.strip().splitlines()
    if len(lines) < 2:
        return None
    header_line = lines[0]
    if not _TABLE_ROW_RE.match(header_line):
        return None
    return _split_row(header_line)


def _header_similarity(h1: List[str], h2: List[str]) -> float:
    """Compute Jaccard similarity between two header rows."""
    if not h1 or not h2:
        return 0.0
    s1 = set(c.lower().strip() for c in h1 if c.strip())
    s2 = set(c.lower().strip() for c in h2 if c.strip())
    if not s1 or not s2:
        return 0.0
    intersection = s1 & s2
    union = s1 | s2
    return len(intersection) / len(union)


def is_available() -> bool:
    """Check if any table extraction backend is available."""
    return _HAS_CAMELOT or _HAS_PDFPLUMBER


def availability_info() -> dict:
    return {
        "camelot": _HAS_CAMELOT,
        "pdfplumber": _HAS_PDFPLUMBER,
    }
