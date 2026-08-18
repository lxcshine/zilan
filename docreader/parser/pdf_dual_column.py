"""Enhanced dual-column layout detection for academic papers.

Strengthens the existing _split_columns logic in pdf_parser.py with:
1. Column boundary detection via whitespace valleys (not just gutter gaps)
2. Title block detection (full-width title above columns)
3. Figure/caption spanning detection (full-width figures between columns)
4. Reading-order reconstruction respecting column flow

These enhancements are critical for:
- Two-column arXiv/IEEE/ACM papers
- Three-column newspaper layouts
- Mixed single/dual-column pages (abstract full-width, body dual-column)
"""

import logging
import re
import statistics
from typing import List, Optional, Tuple

logger = logging.getLogger(__name__)


def detect_column_count(chars: list, page_width: float) -> int:
    """Detect the number of columns on a page.

    Uses the distribution of glyph x-centers to find column gaps.
    A bimodal/multimodal distribution indicates multiple columns.

    Args:
        chars: List of char dicts with x0, x1, y0, y1 keys.
        page_width: Page width in PDF points.

    Returns:
        Number of columns (1, 2, or 3). Returns 1 for single-column.
    """
    if not chars or page_width <= 0:
        return 1

    # Compute x-center histogram
    x_centers = [(c["x0"] + c["x1"]) / 2 for c in chars]
    if not x_centers:
        return 1

    # Divide page width into bins
    n_bins = 40
    bin_width = page_width / n_bins
    histogram = [0] * n_bins
    for xc in x_centers:
        bin_idx = min(n_bins - 1, int(xc / bin_width))
        histogram[bin_idx] += 1

    # Find valleys (local minima with low counts)
    valley_threshold = max(1, statistics.median(histogram) * 0.3)
    valleys = []
    for i in range(2, n_bins - 2):
        if (
            histogram[i] <= valley_threshold
            and histogram[i] < histogram[i - 1]
            and histogram[i] < histogram[i + 1]
            and histogram[i - 1] > valley_threshold
            and histogram[i + 1] > valley_threshold
        ):
            valleys.append(i * bin_width + bin_width / 2)

    # Count columns = valleys + 1
    if not valleys:
        return 1

    # Filter: column gaps must be at least 4% of page width
    min_gap = page_width * 0.04
    filtered = []
    for v in valleys:
        if not filtered or v - filtered[-1] >= min_gap:
            filtered.append(v)

    n_cols = len(filtered) + 1

    # Cap at 3 columns (newspaper layouts); more than 3 is almost certainly noise
    return min(3, n_cols)


def split_into_columns(
    chars: list,
    page_width: float,
    n_cols: Optional[int] = None,
) -> List[List[dict]]:
    """Split glyphs into column groups in reading order.

    Args:
        chars: List of char dicts.
        page_width: Page width.
        n_cols: If known, the number of columns. If None, auto-detect.

    Returns:
        List of column glyph lists, in reading order (left-to-right, top-to-bottom).
    """
    if n_cols is None:
        n_cols = detect_column_count(chars, page_width)

    if n_cols <= 1:
        return [chars]

    # Find column boundaries
    boundaries = _find_column_boundaries(chars, page_width, n_cols)

    # Assign chars to columns
    columns: List[List[dict]] = [[] for _ in range(n_cols)]
    for c in chars:
        xc = (c["x0"] + c["x1"]) / 2
        col_idx = 0
        for i, boundary in enumerate(boundaries):
            if xc >= boundary:
                col_idx = i + 1
            else:
                break
        columns[col_idx].append(c)

    return columns


def _find_column_boundaries(
    chars: list,
    page_width: float,
    n_cols: int,
) -> List[float]:
    """Find the x-coordinates of column boundaries (gutters).

    Uses the histogram approach: find the n_cols-1 widest valleys.
    """
    n_bins = 40
    bin_width = page_width / n_bins
    histogram = [0] * n_bins
    for c in chars:
        xc = (c["x0"] + c["x1"]) / 2
        bin_idx = min(n_bins - 1, int(xc / bin_width))
        histogram[bin_idx] += 1

    # Find all valleys
    valley_threshold = max(1, statistics.median(histogram) * 0.3)
    valleys = []
    for i in range(2, n_bins - 2):
        if (
            histogram[i] <= valley_threshold
            and histogram[i] < histogram[i - 1]
            and histogram[i] < histogram[i + 1]
        ):
            valleys.append((i * bin_width + bin_width / 2, histogram[i]))

    # Sort by gap size (ascending count = wider gap) and pick top n_cols-1
    valleys.sort(key=lambda v: v[1])
    selected = sorted([v[0] for v in valleys[:n_cols - 1]])

    if len(selected) < n_cols - 1:
        # Fallback: evenly divide
        selected = [
            page_width * (i + 1) / n_cols for i in range(n_cols - 1)
        ]

    return selected


def detect_full_width_block(
    chars: list,
    page_width: float,
    n_cols: int,
) -> List[Tuple[float, float, float, float, str]]:
    """Detect full-width blocks (title, abstract, figures) in a multi-column page.

    Returns:
        List of (x0, y0, x1, y1, block_type) tuples for full-width regions.
        block_type: "title", "abstract", "figure", "caption"
    """
    if n_cols <= 1 or not chars:
        return []

    # Find chars that span across column boundaries
    boundaries = _find_column_boundaries(chars, page_width, n_cols)
    if not boundaries:
        return []

    # Group chars into lines
    heights = [c["y1"] - c["y0"] for c in chars if c["y1"] > c["y0"]]
    if not heights:
        return []
    med_h = statistics.median(heights)

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

    # Find lines that span across the first boundary (full-width)
    full_width_lines = []
    for line_chars in lines:
        if not line_chars:
            continue
        x0 = min(c["x0"] for c in line_chars)
        x1 = max(c["x1"] for c in line_chars)
        # Full-width if it crosses all boundaries
        crosses_all = all(x0 < b < x1 for b in boundaries)
        if crosses_all:
            y0 = min(c["y0"] for c in line_chars)
            y1 = max(c["y1"] for c in line_chars)
            text = "".join(c["ch"] for c in sorted(line_chars, key=lambda c: c["x0"]))
            block_type = _classify_full_width_block(text, y0, page_width)
            full_width_lines.append((x0, y0, x1, y1, block_type))

    # Merge adjacent full-width lines of the same type
    merged = _merge_adjacent_blocks(full_width_lines)

    return merged


def _classify_full_width_block(text: str, y_pos: float, page_width: float) -> str:
    """Classify a full-width line as title, abstract, figure, or caption."""
    stripped = text.strip()
    if not stripped:
        return "unknown"

    # Title: top of page, short, no terminal punctuation
    if y_pos > page_width * 0.75:  # upper part of page (PDF coords)
        if len(stripped) < 200 and not stripped[-1:] in ".。!？":
            return "title"

    # Abstract
    if re.match(r"^Abstract\b", stripped, re.I):
        return "abstract"
    if re.match(r"^摘要\b", stripped):
        return "abstract"

    # Figure caption
    if re.match(r"^Figure\s+\d+", stripped, re.I):
        return "caption"

    # Table caption
    if re.match(r"^Table\s+\d+", stripped, re.I):
        return "caption"

    return "text"


def _merge_adjacent_blocks(
    blocks: List[Tuple[float, float, float, float, str]],
) -> List[Tuple[float, float, float, float, str]]:
    """Merge vertically adjacent full-width blocks of the same type."""
    if not blocks:
        return []

    # Sort by y position (top to bottom = high to low in PDF coords)
    blocks.sort(key=lambda b: -b[3])

    merged = [blocks[0]]
    for block in blocks[1:]:
        last = merged[-1]
        # Merge if same type and y-adjacent
        if (
            block[4] == last[4]
            and abs(block[3] - last[1]) < 20  # y gap threshold
        ):
            merged[-1] = (
                min(last[0], block[0]),
                max(last[1], block[1]),
                max(last[2], block[2]),
                min(last[3], block[3]),
                last[4],
            )
        else:
            merged.append(block)

    return merged


def reconstruct_reading_order(
    chars: list,
    page_width: float,
    full_width_blocks: List[Tuple[float, float, float, float, str]],
) -> List[list]:
    """Reconstruct reading order: full-width blocks first, then columns.

    Reading order for a dual-column academic paper:
    1. Title (full-width, top)
    2. Abstract (full-width, below title)
    3. Left column (top to bottom)
    4. Right column (top to bottom)
    5. Full-width figure/caption blocks (interspersed)

    Returns:
        List of char groups in reading order.
    """
    n_cols = detect_column_count(chars, page_width)
    if n_cols <= 1:
        return [chars]

    # Split into columns
    columns = split_into_columns(chars, page_width, n_cols)

    # Build output: interleave full-width blocks and columns
    result: List[list] = []

    # Sort full-width blocks by y position (top to bottom)
    sorted_blocks = sorted(full_width_blocks, key=lambda b: -b[3])

    # Chars not in any full-width block go to columns
    for block_x0, block_y0, block_x1, block_y1, block_type in sorted_blocks:
        # Extract chars in this block
        block_chars = [
            c for c in chars
            if (
                (c["x0"] + c["x1"]) / 2 >= block_x0
                and (c["x0"] + c["x1"]) / 2 <= block_x1
                and (c["y0"] + c["y1"]) / 2 >= block_y0
                and (c["y0"] + c["y1"]) / 2 <= block_y1
            )
        ]
        if block_chars:
            result.append(block_chars)

    # Append column chars (excluding those already in full-width blocks)
    consumed = set()
    for group in result:
        for c in group:
            consumed.add(id(c))

    for col_chars in columns:
        remaining = [c for c in col_chars if id(c) not in consumed]
        if remaining:
            result.append(remaining)

    return result
