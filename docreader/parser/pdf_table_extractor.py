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
"""

import io
import logging
import re
from typing import List, Optional, Tuple

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


def extract_tables_from_page(
    pdf_content: bytes,
    page_index: int,
    flavor: str = "auto",
) -> List[str]:
    """Extract tables from a single PDF page as Markdown strings.

    Args:
        pdf_content: Raw PDF file bytes.
        page_index: 0-based page index.
        flavor: "lattice" (ruled tables), "stream" (borderless), or "auto".

    Returns:
        List of Markdown table strings. Empty if no tables found or
        no extraction backend is available.
    """
    if not _HAS_CAMELOT and not _HAS_PDFPLUMBER:
        return []

    tables_md: List[str] = []

    # --- Camelot path (preferred for structured tables) ---
    if _HAS_CAMELOT:
        try:
            tables_md.extend(_extract_camelot(pdf_content, page_index, flavor))
        except Exception as e:
            logger.debug("Camelot extraction failed on page %d: %s", page_index, e)

    # --- pdfplumber path (fallback for borderless tables) ---
    if not tables_md and _HAS_PDFPLUMBER:
        try:
            tables_md.extend(_extract_pdfplumber(pdf_content, page_index))
        except Exception as e:
            logger.debug("pdfplumber extraction failed on page %d: %s", page_index, e)

    return tables_md


def _extract_camelot(pdf_content: bytes, page_index: int, flavor: str) -> List[str]:
    """Extract tables using Camelot (lattice + stream modes)."""
    import tempfile

    results: List[str] = []

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
                    md = _dataframe_to_markdown(table.df)
                    if md:
                        results.append(md)
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


def _extract_pdfplumber(pdf_content: bytes, page_index: int) -> List[str]:
    """Extract tables using pdfplumber (borderless table support)."""
    results: List[str] = []
    with pdfplumber.open(io.BytesIO(pdf_content)) as pdf:
        if page_index >= len(pdf.pages):
            return []
        page = pdf.pages[page_index]
        tables = page.extract_tables()
        for table_data in tables:
            if not table_data or len(table_data) < 2:
                continue
            md = _rows_to_markdown(table_data)
            if md:
                results.append(md)
    return results


def _dataframe_to_markdown(df) -> str:
    """Convert a pandas DataFrame to a Markdown table string."""
    if df is None or df.empty:
        return ""
    rows = df.values.tolist()
    headers = [str(h) if h is not None else "" for h in df.columns]
    return _rows_to_markdown([headers] + rows)


def _rows_to_markdown(rows: List[List]) -> str:
    """Convert a list of rows (list of cell values) to a Markdown table."""
    if not rows or len(rows) < 2:
        return ""

    # Normalize: convert None to empty string, strip whitespace
    normalized = []
    for row in rows:
        normalized.append([str(c).strip() if c is not None else "" for c in row])

    # Pad rows to equal column count
    max_cols = max(len(r) for r in normalized)
    for row in normalized:
        while len(row) < max_cols:
            row.append("")

    # Build Markdown
    lines = []
    header = normalized[0]
    lines.append("| " + " | ".join(header) + " |")
    lines.append("| " + " | ".join(["---"] * max_cols) + " |")
    for row in normalized[1:]:
        lines.append("| " + " | ".join(row) + " |")

    return "\n".join(lines)


def merge_cross_page_tables(tables_by_page: dict) -> List[Tuple[int, str]]:
    """Merge tables that span multiple pages by header similarity.

    Args:
        tables_by_page: {page_index: [markdown_table, ...]}

    Returns:
        List of (start_page, merged_markdown) for each unique table.
        Continuation pages are merged into the first occurrence.
    """
    if not tables_by_page:
        return []

    merged: List[Tuple[int, str]] = []
    used_pages = set()

    sorted_pages = sorted(tables_by_page.keys())

    for page_i in sorted_pages:
        if page_i in used_pages:
            continue

        tables = tables_by_page.get(page_i, [])
        if not tables:
            continue

        for table_md in tables:
            header = _extract_table_header(table_md)
            if not header:
                merged.append((page_i, table_md))
                continue

            # Look ahead for continuation pages with matching header
            combined = table_md
            for next_page in range(page_i + 1, max(sorted_pages) + 1):
                if next_page in used_pages:
                    continue
                next_tables = tables_by_page.get(next_page, [])
                matched = False
                for nt in next_tables:
                    next_header = _extract_table_header(nt)
                    if next_header and _header_similarity(header, next_header) > 0.6:
                        # Merge: append data rows (skip the continuation's header)
                        combined = _merge_table_rows(combined, nt)
                        used_pages.add(next_page)
                        matched = True
                        break
                if not matched:
                    break  # No continuation found, stop looking

            merged.append((page_i, combined))

    return merged


def _extract_table_header(md_table: str) -> Optional[List[str]]:
    """Extract the header row from a Markdown table."""
    lines = md_table.strip().splitlines()
    if len(lines) < 2:
        return None
    header_line = lines[0]
    if not _TABLE_ROW_RE.match(header_line):
        return None
    cells = [c.strip() for c in header_line.strip("|").split("|")]
    return cells


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


def _merge_table_rows(table1_md: str, table2_md: str) -> str:
    """Merge two Markdown tables: keep t1's header, append t2's data rows."""
    lines1 = table1_md.strip().splitlines()
    lines2 = table2_md.strip().splitlines()

    # t1 = header + separator + data rows
    # t2 = header + separator + data rows
    # Result = t1 + t2's data rows (skip header + separator)

    if len(lines2) < 3:
        return table1_md

    data_rows = lines2[2:]  # Skip header and separator
    return "\n".join(lines1 + data_rows)


def is_available() -> bool:
    """Check if any table extraction backend is available."""
    return _HAS_CAMELOT or _HAS_PDFPLUMBER


def availability_info() -> dict:
    return {
        "camelot": _HAS_CAMELOT,
        "pdfplumber": _HAS_PDFPLUMBER,
    }
