"""Auto-routing engine selector for PDF parsing (§5.1).

Analyzes a PDF's characteristics (page count, image ratio, table/formula
density, scan ratio) and selects the optimal parsing engine:

  - Pages < 20 + image ratio < 30% → builtin (pypdfium2, fast)
  - Pages > 50 / table-dense / formula-dense → MinerU/Marker
  - Scanned (image ratio > 50%) → PaddleOCR-VL or advanced engine
  - Quality below threshold → retry with next heavier engine

The router is called by the Go side via a new gRPC metadata field or
by the Python parser itself when auto-routing is enabled.
"""

import logging
import statistics
from dataclasses import dataclass, field
from typing import List, Optional

from docreader.parser.pdf_quality import ParseQualityReport

logger = logging.getLogger(__name__)


@dataclass
class PDFProfile:
    """Pre-analysis profile of a PDF document."""
    page_count: int = 0
    avg_image_area_ratio: float = 0.0
    scanned_page_ratio: float = 0.0
    text_page_ratio: float = 1.0
    has_tables: bool = False
    has_formulas: bool = False
    is_dual_column: bool = False
    estimated_table_count: int = 0
    estimated_formula_count: int = 0


@dataclass
class RouteDecision:
    """Engine routing decision for a PDF."""
    engine: str  # builtin, mineru, paddleocr_vl, markitdown
    reason: str
    fallback_chain: List[str] = field(default_factory=list)
    profile: Optional[PDFProfile] = None


# Routing thresholds (tunable)
LIGHT_ENGINE_MAX_PAGES = 20
HEAVY_ENGINE_MIN_PAGES = 50
SCANNED_THRESHOLD = 0.50
IMAGE_RATIO_LIGHT = 0.30
TABLE_DENSE_THRESHOLD = 3
FORMULA_DENSE_THRESHOLD = 5


def profile_pdf(
    page_count: int,
    page_image_ratios: List[float],
    page_text_lengths: List[int],
    has_tables: bool = False,
    has_formulas: bool = False,
    is_dual_column: bool = False,
    table_count: int = 0,
    formula_count: int = 0,
) -> PDFProfile:
    """Build a pre-analysis profile of a PDF.

    Called during Pass 1 of PDFParser._route_locked, after the cheap
    text extraction + image-area classification pass.

    Args:
        page_count: Total number of pages.
        page_image_ratios: Per-page image area coverage ratios.
        page_text_lengths: Per-page text character counts.
        has_tables: Whether tables were detected.
        has_formulas: Whether formulas were detected.
        is_dual_column: Whether dual-column layout was detected.
        table_count: Estimated number of tables.
        formula_count: Estimated number of formulas.
    """
    scanned_pages = sum(
        1
        for i, ratio in enumerate(page_image_ratios)
        if ratio >= SCANNED_THRESHOLD
        or (page_text_lengths[i] < 10 and ratio >= 0.1)
    )

    avg_ratio = (
        statistics.mean(page_image_ratios) if page_image_ratios else 0.0
    )

    return PDFProfile(
        page_count=page_count,
        avg_image_area_ratio=avg_ratio,
        scanned_page_ratio=scanned_pages / page_count if page_count else 0.0,
        text_page_ratio=1.0 - (scanned_pages / page_count if page_count else 0.0),
        has_tables=has_tables,
        has_formulas=has_formulas,
        is_dual_column=is_dual_column,
        estimated_table_count=table_count,
        estimated_formula_count=formula_count,
    )


def select_engine(profile: PDFProfile) -> RouteDecision:
    """Select the optimal parsing engine based on the PDF profile.

    Routing rules (evaluated in priority order):
    1. Scanned-dominant (>50% scanned pages) → PaddleOCR-VL (OCR specialist)
    2. Formula-dense academic paper → MinerU (formula/table specialist)
    3. Table-dense + pages > 50 → MinerU (complex layout)
    4. Pages < 20 + low image ratio → Builtin (fast pypdfium2)
    5. Default → Builtin with fallback to MinerU
    """
    # Rule 1: Scanned-dominant
    if profile.scanned_page_ratio >= SCANNED_THRESHOLD:
        return RouteDecision(
            engine="paddleocr_vl",
            reason=f"scanned_page_ratio={profile.scanned_page_ratio:.2f} >= {SCANNED_THRESHOLD}",
            fallback_chain=["mineru", "builtin"],
            profile=profile,
        )

    # Rule 2: Formula-dense
    if profile.estimated_formula_count >= FORMULA_DENSE_THRESHOLD or (
        profile.has_formulas and profile.is_dual_column
    ):
        return RouteDecision(
            engine="mineru",
            reason=f"formula_count={profile.estimated_formula_count}, dual_column={profile.is_dual_column}",
            fallback_chain=["builtin"],
            profile=profile,
        )

    # Rule 3: Table-dense + large document
    if (
        profile.estimated_table_count >= TABLE_DENSE_THRESHOLD
        and profile.page_count >= HEAVY_ENGINE_MIN_PAGES
    ):
        return RouteDecision(
            engine="mineru",
            reason=f"table_count={profile.estimated_table_count}, pages={profile.page_count}",
            fallback_chain=["builtin"],
            profile=profile,
        )

    # Rule 4: Light document
    if (
        profile.page_count <= LIGHT_ENGINE_MAX_PAGES
        and profile.avg_image_area_ratio < IMAGE_RATIO_LIGHT
    ):
        return RouteDecision(
            engine="builtin",
            reason=f"pages={profile.page_count} <= {LIGHT_ENGINE_MAX_PAGES}, image_ratio={profile.avg_image_area_ratio:.2f}",
            fallback_chain=[],
            profile=profile,
        )

    # Rule 5: Default — builtin with fallback
    return RouteDecision(
        engine="builtin",
        reason=f"default routing (pages={profile.page_count}, image_ratio={profile.avg_image_area_ratio:.2f})",
        fallback_chain=["mineru"],
        profile=profile,
    )


def should_retry_with_heavier_engine(
    quality: ParseQualityReport,
    current_engine: str,
    fallback_chain: List[str],
) -> Optional[str]:
    """Decide whether to retry parsing with a heavier engine.

    Returns the next engine name to try, or None if no retry needed.
    """
    if not quality.should_retry:
        return None

    # Find the next engine in the fallback chain after the current one
    try:
        idx = fallback_chain.index(current_engine)
        if idx + 1 < len(fallback_chain):
            return fallback_chain[idx + 1]
    except ValueError:
        # Current engine not in chain — try first fallback
        if fallback_chain:
            return fallback_chain[0]

    return None


def route_decision_to_metadata(decision: RouteDecision) -> dict:
    """Convert routing decision to metadata for logging/debugging."""
    return {
        "route_engine": decision.engine,
        "route_reason": decision.reason,
        "route_fallback_chain": ",".join(decision.fallback_chain),
    }
