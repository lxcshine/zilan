"""Tests for PDF auto-router (§5.1)."""

import sys
import os

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from docreader.parser.pdf_auto_router import (
    PDFProfile,
    RouteDecision,
    profile_pdf,
    select_engine,
    should_retry_with_heavier_engine,
    route_decision_to_metadata,
)
from docreader.parser.pdf_quality import ParseQualityReport


class TestProfilePDF:
    def test_small_text_pdf(self):
        profile = profile_pdf(
            page_count=10,
            page_image_ratios=[0.05] * 10,
            page_text_lengths=[2000] * 10,
        )
        assert profile.page_count == 10
        assert profile.scanned_page_ratio == 0.0
        assert profile.avg_image_area_ratio < 0.1

    def test_scanned_pdf(self):
        profile = profile_pdf(
            page_count=5,
            page_image_ratios=[0.95] * 5,
            page_text_lengths=[3] * 5,
        )
        assert profile.scanned_page_ratio == 1.0

    def test_mixed_pdf(self):
        ratios = [0.05, 0.9, 0.05, 0.9, 0.05]
        lengths = [2000, 2, 1500, 1, 1800]
        profile = profile_pdf(5, ratios, lengths)
        assert 0.3 < profile.scanned_page_ratio < 0.6


class TestSelectEngine:
    def test_light_pdf_uses_builtin(self):
        profile = PDFProfile(page_count=10, avg_image_area_ratio=0.05, scanned_page_ratio=0.0)
        decision = select_engine(profile)
        assert decision.engine == "builtin"
        assert "pages=10" in decision.reason

    def test_scanned_pdf_uses_paddleocr(self):
        profile = PDFProfile(page_count=30, scanned_page_ratio=0.6)
        decision = select_engine(profile)
        assert decision.engine == "paddleocr_vl"
        assert "mineru" in decision.fallback_chain

    def test_formula_dense_uses_mineru(self):
        profile = PDFProfile(
            page_count=20,
            scanned_page_ratio=0.0,
            estimated_formula_count=8,
            is_dual_column=True,
        )
        decision = select_engine(profile)
        assert decision.engine == "mineru"

    def test_table_dense_large_uses_mineru(self):
        profile = PDFProfile(
            page_count=60,
            scanned_page_ratio=0.0,
            estimated_table_count=5,
        )
        decision = select_engine(profile)
        assert decision.engine == "mineru"

    def test_default_builtin_with_fallback(self):
        profile = PDFProfile(
            page_count=30,
            avg_image_area_ratio=0.4,
            scanned_page_ratio=0.3,
            estimated_table_count=1,
            estimated_formula_count=0,
        )
        decision = select_engine(profile)
        assert decision.engine == "builtin"
        assert "mineru" in decision.fallback_chain


class TestShouldRetry:
    def test_no_retry_when_quality_ok(self):
        quality = ParseQualityReport(overall_score=0.85, should_retry=False)
        result = should_retry_with_heavier_engine(quality, "builtin", ["mineru"])
        assert result is None

    def test_retry_with_next_engine(self):
        quality = ParseQualityReport(overall_score=0.45, should_retry=True)
        result = should_retry_with_heavier_engine(quality, "builtin", ["builtin", "mineru"])
        assert result == "mineru"

    def test_no_more_fallback(self):
        quality = ParseQualityReport(overall_score=0.30, should_retry=True)
        result = should_retry_with_heavier_engine(quality, "mineru", ["builtin", "mineru"])
        assert result is None


class TestRouteMetadata:
    def test_metadata_export(self):
        decision = RouteDecision(
            engine="mineru",
            reason="formula dense",
            fallback_chain=["builtin"],
        )
        meta = route_decision_to_metadata(decision)
        assert meta["route_engine"] == "mineru"
        assert meta["route_fallback_chain"] == "builtin"
