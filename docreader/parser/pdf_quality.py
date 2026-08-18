"""Parse quality scoring and auto-retry decision engine.

Implements the quality metrics described in §5.4:
  - garble_rate:  share of lines that look like broken OCR
  - empty_page_rate: share of pages with no extractable text
  - table_damage_rate: detected tables with misaligned rows/cols
  - image_loss_rate:  images referenced but not extracted

The overall score feeds back into the auto-router (§5.1) to decide
whether to retry with a heavier engine (MinerU / PaddleOCR-VL).
"""

import re
import statistics
from dataclasses import dataclass, field
from typing import List, Optional


# Thresholds (env-overridable via module assignment in tests)
GARBLE_THRESHOLD = 0.15
EMPTY_PAGE_THRESHOLD = 0.30
TABLE_DAMAGE_THRESHOLD = 0.25
IMAGE_LOSS_THRESHOLD = 0.20
MIN_ACCEPTABLE_SCORE = 0.70


@dataclass
class PageQuality:
    """Per-page quality metrics."""

    page_index: int
    char_count: int = 0
    line_count: int = 0
    garbled_lines: int = 0
    has_table: bool = False
    table_damaged: bool = False
    has_image_ref: bool = False
    image_extracted: bool = False
    is_empty: bool = False


@dataclass
class ParseQualityReport:
    """Aggregate quality report for a parsed document."""

    page_count: int = 0
    garble_rate: float = 0.0
    empty_page_rate: float = 0.0
    table_damage_rate: float = 0.0
    image_loss_rate: float = 0.0
    overall_score: float = 1.0
    page_reports: List[PageQuality] = field(default_factory=list)
    should_retry: bool = False
    retry_reason: str = ""

    def to_metadata(self) -> dict:
        return {
            "quality_garble_rate": f"{self.garble_rate:.4f}",
            "quality_empty_page_rate": f"{self.empty_page_rate:.4f}",
            "quality_table_damage_rate": f"{self.table_damage_rate:.4f}",
            "quality_image_loss_rate": f"{self.image_loss_rate:.4f}",
            "quality_score": f"{self.overall_score:.4f}",
            "quality_should_retry": str(self.should_retry),
            "quality_retry_reason": self.retry_reason,
        }


# ---------------------------------------------------------------------------
# Line-level heuristics
# ---------------------------------------------------------------------------

# Lines that are mostly 1-2 character tokens indicate broken OCR / glued glyphs.
_GARBLE_WORD_RE = re.compile(r"\S+")

# Lines with replacement chars or control sequences
_REPLACEMENT_RE = re.compile(r"[\ufffd\u0000-\u0008\u000b\u000c\u000e-\u001f]")


def _is_garbled_line(line: str) -> bool:
    """True if a line looks like broken OCR output.

    Two independent signals:
      - many short (1-2 char) tokens → glued/dropped glyphs
      - replacement / control characters → undecodable bytes
    """
    t = line.strip()
    if not t or len(t) < 6:
        return False
    # Replacement / control characters are a strong garble signal on their own.
    if _REPLACEMENT_RE.search(t):
        return True
    words = _GARBLE_WORD_RE.findall(t)
    if len(words) < 6:
        return False
    short = sum(1 for w in words if len(w) <= 2)
    return short / len(words) > 0.45


def _has_replacement_chars(text: str) -> bool:
    return bool(_REPLACEMENT_RE.search(text))


# ---------------------------------------------------------------------------
# Table damage detection
# ---------------------------------------------------------------------------

# A markdown table row: | col1 | col2 |
_TABLE_ROW_RE = re.compile(r"^\|.*\|\s*$")
_TABLE_SEP_RE = re.compile(r"^\|[\s:|-]+\|\s*$")


def _detect_table_damage(lines: List[str]) -> tuple:
    """Return (has_table, is_damaged) for a set of lines.

    Damage signals:
      - separator row missing or malformed
      - column count varies across rows
      - single-column "table" that is really just text
    """
    table_rows = [(i, ln.strip()) for i, ln in enumerate(lines) if _TABLE_ROW_RE.match(ln.strip())]
    if len(table_rows) < 2:
        return False, False

    has_table = True

    # Check for separator row (|---|---|)
    has_sep = any(_TABLE_SEP_RE.match(row) for _, row in table_rows)
    if not has_sep and len(table_rows) >= 3:
        return True, True

    # Column count consistency
    col_counts = []
    for _, row in table_rows:
        if _TABLE_SEP_RE.match(row):
            continue
        cols = [c.strip() for c in row.strip("|").split("|")]
        col_counts.append(len(cols))

    if not col_counts:
        return has_table, False

    median_cols = statistics.median(col_counts)
    if median_cols < 2:
        # Single-column "table" — likely a parsing artefact
        return has_table, True

    # Any row whose column count differs from the median is a misalignment
    # signal (a dropped/merged cell or an extra spurious column). Count how
    # many rows deviate; if too many, the table is damaged.
    mismatches = sum(1 for c in col_counts if c != median_cols)
    if mismatches / len(col_counts) > 0.25:
        return has_table, True

    return has_table, False


# ---------------------------------------------------------------------------
# Image reference / extraction tracking
# ---------------------------------------------------------------------------

_IMAGE_REF_RE = re.compile(r"!\[.*?\]\((.*?)\)")


def _extract_image_refs(text: str) -> List[str]:
    return _IMAGE_REF_RE.findall(text)


# ---------------------------------------------------------------------------
# Per-page quality scoring
# ---------------------------------------------------------------------------

def score_page(page_index: int, text: str, extracted_image_paths: set) -> PageQuality:
    """Score a single page's parse quality."""
    lines = [ln for ln in text.splitlines() if ln.strip()]
    char_count = sum(len(ln) for ln in lines)

    pq = PageQuality(
        page_index=page_index,
        char_count=char_count,
        line_count=len(lines),
        is_empty=char_count == 0,
    )

    if char_count == 0:
        return pq

    # Garble detection
    pq.garbled_lines = sum(1 for ln in lines if _is_garbled_line(ln) or _has_replacement_chars(ln))

    # Table detection
    has_table, table_damaged = _detect_table_damage(lines)
    pq.has_table = has_table
    pq.table_damaged = table_damaged

    # Image tracking
    refs = _extract_image_refs(text)
    pq.has_image_ref = len(refs) > 0
    pq.image_extracted = all(r in extracted_image_paths for r in refs) if refs else True

    return pq


# ---------------------------------------------------------------------------
# Aggregate report
# ---------------------------------------------------------------------------

def build_quality_report(
    page_reports: List[PageQuality],
    total_image_refs: int,
    extracted_image_count: int,
) -> ParseQualityReport:
    """Build aggregate quality report and decide whether to retry."""
    if not page_reports:
        return ParseQualityReport(should_retry=True, retry_reason="No pages parsed")

    page_count = len(page_reports)

    # Garble rate: weighted by page text volume
    total_chars = sum(pq.char_count for pq in page_reports) or 1
    garbled_chars = sum(pq.garbled_lines * 40 for pq in page_reports)  # est. 40 chars/garbled line
    garble_rate = min(1.0, garbled_chars / total_chars)

    # Empty page rate
    empty_pages = sum(1 for pq in page_reports if pq.is_empty)
    empty_page_rate = empty_pages / page_count

    # Table damage rate
    table_pages = [pq for pq in page_reports if pq.has_table]
    damaged_tables = sum(1 for pq in table_pages if pq.table_damaged)
    table_damage_rate = damaged_tables / len(table_pages) if table_pages else 0.0

    # Image loss rate
    if total_image_refs > 0:
        image_loss_rate = 1.0 - min(1.0, extracted_image_count / total_image_refs)
    else:
        image_loss_rate = 0.0

    # Overall score: weighted geometric mean (each component can veto)
    import math

    def component(actual: float, threshold: float) -> float:
        """Map actual rate to [0,1] where 0=threshold, 1=perfect."""
        if actual <= 0:
            return 1.0
        if actual >= threshold:
            return 0.0
        return 1.0 - actual / threshold

    components = [
        component(garble_rate, GARBLE_THRESHOLD),
        component(empty_page_rate, EMPTY_PAGE_THRESHOLD),
        component(table_damage_rate, TABLE_DAMAGE_THRESHOLD),
        component(image_loss_rate, IMAGE_LOSS_THRESHOLD),
    ]

    # Geometric mean: any near-zero component drags the score down
    score = math.exp(sum(math.log(max(c, 0.01)) for c in components) / len(components))

    report = ParseQualityReport(
        page_count=page_count,
        garble_rate=garble_rate,
        empty_page_rate=empty_page_rate,
        table_damage_rate=table_damage_rate,
        image_loss_rate=image_loss_rate,
        overall_score=score,
        page_reports=page_reports,
    )

    if score < MIN_ACCEPTABLE_SCORE:
        report.should_retry = True
        reasons = []
        if garble_rate > GARBLE_THRESHOLD:
            reasons.append(f"garble_rate={garble_rate:.2f}")
        if empty_page_rate > EMPTY_PAGE_THRESHOLD:
            reasons.append(f"empty_page_rate={empty_page_rate:.2f}")
        if table_damage_rate > TABLE_DAMAGE_THRESHOLD:
            reasons.append(f"table_damage_rate={table_damage_rate:.2f}")
        if image_loss_rate > IMAGE_LOSS_THRESHOLD:
            reasons.append(f"image_loss_rate={image_loss_rate:.2f}")
        report.retry_reason = "; ".join(reasons) if reasons else f"score={score:.2f}"

    return report
