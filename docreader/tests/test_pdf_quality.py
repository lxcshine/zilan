"""Tests for PDF parse quality scoring (§5.4)."""

import sys
import os

# Add project root to path
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from docreader.parser.pdf_quality import (
    PageQuality,
    ParseQualityReport,
    score_page,
    build_quality_report,
    _is_garbled_line,
    _detect_table_damage,
    GARBLE_THRESHOLD,
    MIN_ACCEPTABLE_SCORE,
)


class TestGarbleDetection:
    def test_clean_line_not_garbled(self):
        assert not _is_garbled_line("This is a normal English sentence with proper words.")
        assert not _is_garbled_line("这是一段正常的中文文本，用于测试。")

    def test_garbled_line_detected(self):
        # Line with many short tokens = broken OCR
        assert _is_garbled_line("ab cd ef gh ij kl mn op qr st uv wx yz")
        # Replacement characters
        assert _is_garbled_line("Th\ufffds \ufffdfdf \ufffdfdg \ufffdfdh \ufffdfdf")


class TestTableDamageDetection:
    def test_no_table(self):
        lines = ["Just some text", "More text here", "No table at all"]
        has_table, damaged = _detect_table_damage(lines)
        assert not has_table
        assert not damaged

    def test_good_table(self):
        lines = [
            "| Name | Age | City |",
            "| --- | --- | --- |",
            "| Alice | 30 | NYC |",
            "| Bob | 25 | LA |",
        ]
        has_table, damaged = _detect_table_damage(lines)
        assert has_table
        assert not damaged

    def test_damaged_table_missing_separator(self):
        lines = [
            "| Name | Age |",
            "| Alice | 30 |",
            "| Bob | 25 |",
            "| Charlie | 35 |",
        ]
        has_table, damaged = _detect_table_damage(lines)
        assert has_table
        assert damaged

    def test_damaged_table_column_mismatch(self):
        lines = [
            "| Name | Age | City |",
            "| --- | --- | --- |",
            "| Alice | 30 |",
            "| Bob | 25 | LA | Extra |",
        ]
        has_table, damaged = _detect_table_damage(lines)
        assert has_table
        assert damaged


class TestPageQuality:
    def test_empty_page(self):
        pq = score_page(0, "", set())
        assert pq.is_empty
        assert pq.char_count == 0

    def test_clean_page(self):
        text = "This is a well-formed paragraph.\n\nAnother paragraph with proper sentences."
        pq = score_page(0, text, set())
        assert not pq.is_empty
        assert pq.garbled_lines == 0
        assert not pq.table_damaged

    def test_page_with_garble(self):
        text = "ab cd ef gh ij kl mn op qr st uv wx yz 12 34 56 78 90"
        pq = score_page(0, text, set())
        assert pq.garbled_lines >= 1


class TestQualityReport:
    def test_perfect_quality(self):
        reports = [
            PageQuality(page_index=0, char_count=500, line_count=10, garbled_lines=0),
            PageQuality(page_index=1, char_count=600, line_count=12, garbled_lines=0),
        ]
        report = build_quality_report(reports, total_image_refs=0, extracted_image_count=0)
        assert report.overall_score >= MIN_ACCEPTABLE_SCORE
        assert not report.should_retry

    def test_garbled_document_retries(self):
        reports = []
        for i in range(10):
            reports.append(PageQuality(
                page_index=i,
                char_count=300,
                line_count=15,
                garbled_lines=10,
            ))
        report = build_quality_report(reports, total_image_refs=0, extracted_image_count=0)
        assert report.should_retry
        assert "garble" in report.retry_reason.lower()

    def test_empty_document_retries(self):
        reports = [
            PageQuality(page_index=0, char_count=0, is_empty=True),
            PageQuality(page_index=1, char_count=0, is_empty=True),
            PageQuality(page_index=2, char_count=0, is_empty=True),
        ]
        report = build_quality_report(reports, total_image_refs=0, extracted_image_count=0)
        assert report.should_retry

    def test_image_loss_triggers_retry(self):
        reports = [PageQuality(page_index=0, char_count=500, line_count=10)]
        report = build_quality_report(reports, total_image_refs=10, extracted_image_count=2)
        assert report.image_loss_rate > 0.2
        assert report.should_retry

    def test_metadata_export(self):
        report = ParseQualityReport(
            overall_score=0.85,
            garble_rate=0.05,
            empty_page_rate=0.0,
            table_damage_rate=0.0,
            image_loss_rate=0.0,
        )
        meta = report.to_metadata()
        assert meta["quality_score"] == "0.8500"
        assert meta["quality_should_retry"] == "False"
