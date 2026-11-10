"""FR-13 OCR 流水线可配置化的单元测试。

覆盖三件事：

1. **配置解析**：override > env > 默认值的优先级，未知流水线回落手动，
   越界参数被拒绝并回落默认（不 clamp）；
2. **四分类路由**：文本页/扫描页/图片页/混合页在每条下游路径（文本层、
   整页渲染、表格、公式、内嵌图、质量评分、页眉页脚）上的归属，确保
   任何一个类别都不会被静默丢页；
3. **可选前处理**：默认关闭、开启后真的改写了图片、依赖缺失时优雅降级。
"""

import base64
import io
import unittest
from unittest import mock

import pypdfium2 as pdfium
from PIL import Image

import docreader.parser.pdf_parser as pdf_mod
from docreader.parser import image_preprocess
from docreader.parser.pdf_parser import (
    OCR_PIPELINE_MANUAL,
    OCR_PIPELINE_SMART,
    PDFParser,
    _apply_preprocess,
    _extract_embedded_images,
    _extract_and_inject_formulas,
    _extract_tables_per_page,
    _override_bool,
    _override_number,
    _preprocess_metadata,
    _strip_repeating_lines,
    resolve_ocr_settings,
)


def _make_image_pdf(num_pages: int = 2, edge: int = 64) -> bytes:
    """整页图片、无文本层的 PDF（每一页都会被判成图片页）。"""
    buf = io.BytesIO()
    pages = [
        Image.new("RGB", (edge, edge), color)
        for color in (("white", "black") * ((num_pages // 2) + 1))[:num_pages]
    ]
    pages[0].save(buf, format="PDF", save_all=True, append_images=pages[1:])
    return buf.getvalue()


def _make_jpeg(edge: int = 120) -> bytes:
    buf = io.BytesIO()
    Image.new("RGB", (edge, edge), (200, 200, 200)).save(buf, format="JPEG", quality=90)
    return buf.getvalue()


class ResolveOcrSettingsTest(unittest.TestCase):
    def test_absent_overrides_mean_manual_pipeline(self):
        settings = resolve_ocr_settings(None)
        self.assertEqual(settings.pipeline, OCR_PIPELINE_MANUAL)
        self.assertFalse(settings.is_smart)
        self.assertFalse(settings.force_scanned)
        # 0 表示「知识库没配，用服务默认」，不是「配成了 0」。
        self.assertEqual(settings.render_dpi, 0)
        self.assertEqual(settings.jpeg_quality, 0)
        self.assertIsNone(settings.scan_image_ratio)
        # 手动流水线只有文本页取文本层 —— 与 FR-13 之前一致。
        self.assertEqual(settings.text_layer_classes, ("text",))
        self.assertEqual(settings.preprocess_mode, "off")

    def test_unknown_pipeline_falls_back_to_manual(self):
        settings = resolve_ocr_settings({"ocr_pipeline": "SmartOCR-v9"})
        self.assertEqual(settings.pipeline, OCR_PIPELINE_MANUAL)

    def test_smart_pipeline_keeps_mixed_text_layer_by_default(self):
        settings = resolve_ocr_settings({"ocr_pipeline": "smart"})
        self.assertTrue(settings.is_smart)
        self.assertEqual(settings.text_layer_classes, ("text", "mixed"))

    def test_smart_pipeline_can_drop_mixed_text_layer(self):
        settings = resolve_ocr_settings(
            {"ocr_pipeline": "smart", "smart.mixed_use_text_layer": "false"}
        )
        self.assertEqual(settings.text_layer_classes, ("text",))

    def test_manual_force_scanned_flag(self):
        self.assertTrue(
            resolve_ocr_settings(
                {"ocr_pipeline": "manual", "manual.force_scanned": "true"}
            ).force_scanned
        )
        # 别的流水线的参数不会被误读。
        self.assertFalse(
            resolve_ocr_settings(
                {"ocr_pipeline": "smart", "manual.force_scanned": "true"}
            ).force_scanned
        )

    def test_out_of_range_params_are_rejected_not_clamped(self):
        settings = resolve_ocr_settings(
            {
                "ocr_pipeline": "manual",
                "manual.render_dpi": "999",  # 上界 600
                "manual.jpeg_quality": "0",  # 下界 1
            }
        )
        # 被拒绝 -> 回落「用服务默认」，而不是被夹到 600/1。
        self.assertEqual(settings.render_dpi, 0)
        self.assertEqual(settings.jpeg_quality, 0)

        smart = resolve_ocr_settings(
            {
                "ocr_pipeline": "smart",
                "smart.scan_image_ratio": "5",  # 上界 1
                "smart.min_chars_per_page": "not-a-number",
                "smart.heavy_render_dpi": "70",  # 下界 72
            }
        )
        self.assertIsNone(smart.scan_image_ratio)
        self.assertIsNone(smart.min_chars_per_page)
        self.assertEqual(smart.heavy_render_dpi, 300)

    def test_valid_params_are_accepted(self):
        settings = resolve_ocr_settings(
            {
                "ocr_pipeline": "smart",
                "smart.scan_image_ratio": "0.35",
                "smart.min_chars_per_page": "25",
                "smart.heavy_render_dpi": "400",
                "smart.scan_dense_ratio": "0.8",
            }
        )
        self.assertAlmostEqual(settings.scan_image_ratio, 0.35)
        self.assertEqual(settings.min_chars_per_page, 25)
        self.assertEqual(settings.heavy_render_dpi, 400)
        self.assertAlmostEqual(settings.scan_dense_ratio, 0.8)

    def test_preprocess_params_are_collected_and_unprefixed(self):
        settings = resolve_ocr_settings(
            {
                "ocr_pipeline": "manual",
                "preprocess.mode": "always",
                "preprocess.denoise.enabled": "true",
                "preprocess.denoise.sigma": "2.5",
                "manual.render_dpi": "200",
            }
        )
        self.assertEqual(settings.preprocess_mode, "always")
        self.assertEqual(settings.preprocess_params["denoise.enabled"], "true")
        self.assertEqual(settings.preprocess_params["denoise.sigma"], "2.5")
        # 非前处理键不会被塞进前处理参数表。
        self.assertNotIn("render_dpi", settings.preprocess_params)

    def test_preprocess_mode_from_pipeline_slot(self):
        # 面板上 preprocess.mode 属于流水线字段，因此也可能带流水线前缀下发。
        settings = resolve_ocr_settings(
            {"ocr_pipeline": "smart", "smart.preprocess.mode": "auto"}
        )
        self.assertEqual(settings.preprocess_mode, "auto")


class OverrideScalarTest(unittest.TestCase):
    def test_bool_parsing(self):
        self.assertTrue(_override_bool({"k": "on"}, "k", False))
        self.assertFalse(_override_bool({"k": "off"}, "k", True))
        # 非法值回落默认，不抛异常。
        self.assertTrue(_override_bool({"k": "maybe"}, "k", True))
        self.assertFalse(_override_bool({}, "k", False))

    def test_number_parsing(self):
        self.assertEqual(_override_number({"k": "300"}, "k", 0, 72, 600, integer=True), 300)
        # 越界拒绝并回落默认。
        self.assertEqual(_override_number({"k": "71"}, "k", 200, 72, 600, integer=True), 200)
        self.assertEqual(_override_number({"k": "x"}, "k", 1.0, 0.0, 5.0), 1.0)
        self.assertEqual(_override_number({}, "k", None, 0.0, 1.0), None)


class PreprocessMetadataTest(unittest.TestCase):
    def test_off_produces_no_metadata(self):
        self.assertEqual(_preprocess_metadata({"mode": "off", "status": "skipped"}), {})
        self.assertEqual(_preprocess_metadata(None), {})

    def test_enabled_produces_diagnostic_metadata(self):
        meta = _preprocess_metadata(
            {"mode": "always", "status": "ok", "steps": "denoise,pad", "pages": 3}
        )
        self.assertEqual(meta["preprocess_mode"], "always")
        self.assertEqual(meta["preprocess_status"], "ok")
        self.assertEqual(meta["preprocess_steps"], "denoise,pad")
        self.assertEqual(meta["preprocess_pages"], 3)


class ApplyPreprocessTest(unittest.TestCase):
    def _settings(self, params):
        return resolve_ocr_settings({"ocr_pipeline": "manual", **params})

    def test_default_off_returns_input_untouched(self):
        data = _make_jpeg()
        out, summary = _apply_preprocess({0: data}, self._settings({}))
        self.assertIs(out[0], data)
        self.assertEqual(summary["status"], "skipped")

    def test_empty_render_set_is_a_noop(self):
        out, summary = _apply_preprocess({}, self._settings({"preprocess.mode": "always"}))
        self.assertEqual(out, {})
        self.assertEqual(summary["mode"], "always")

    def test_mode_on_without_any_step_is_skipped(self):
        data = _make_jpeg()
        out, summary = _apply_preprocess(
            {0: data},
            self._settings({"preprocess.mode": "always", "preprocess.pad": "0"}),
        )
        self.assertIs(out[0], data)
        self.assertEqual(summary["status"], "skipped")

    def test_default_pad_applies_once_preprocess_is_enabled(self):
        # pad 的默认值是 0.015（补白边帮助 OCR 处理贴边内容），因此只要打开
        # 前处理就会生效；显式设成 0 才不补。
        data = _make_jpeg()
        out, summary = _apply_preprocess(
            {0: data}, self._settings({"preprocess.mode": "always"})
        )
        self.assertEqual(summary["status"], "ok")
        self.assertEqual(summary["steps"], "pad")
        self.assertNotEqual(out[0], data)

    def test_denoise_and_pad_rewrite_the_page_image(self):
        data = _make_jpeg()
        out, summary = _apply_preprocess(
            {0: data},
            self._settings(
                {
                    "preprocess.mode": "always",
                    "preprocess.denoise.enabled": "true",
                    "preprocess.pad": "0.05",
                }
            ),
        )
        self.assertEqual(summary["status"], "ok")
        self.assertEqual(summary["pages"], 1)
        self.assertIn("denoise", summary["steps"])
        self.assertIn("pad", summary["steps"])
        self.assertNotEqual(out[0], data)
        # 补白边后图像确实变大了。
        before = Image.open(io.BytesIO(data)).size
        after = Image.open(io.BytesIO(out[0])).size
        self.assertGreater(after[0], before[0])

    def test_unavailable_dependency_degrades_gracefully(self):
        data = _make_jpeg()
        with mock.patch.object(image_preprocess, "is_available", return_value=False):
            out, summary = _apply_preprocess(
                {0: data},
                self._settings(
                    {
                        "preprocess.mode": "always",
                        "preprocess.denoise.enabled": "true",
                    }
                ),
            )
        self.assertIs(out[0], data)
        self.assertEqual(summary["status"], "unavailable")

    def test_corrupt_jpeg_does_not_break_parsing(self):
        out, summary = _apply_preprocess(
            {0: b"not a jpeg"},
            self._settings(
                {
                    "preprocess.mode": "always",
                    "preprocess.contrast.enabled": "true",
                }
            ),
        )
        self.assertEqual(out[0], b"not a jpeg")
        self.assertNotEqual(summary["status"], "ok")


class ImagePreprocessThresholdTest(unittest.TestCase):
    """前处理参数校验范式：越界拒绝并回落默认，不 clamp。"""

    def test_out_of_range_falls_back_to_default(self):
        self.assertEqual(image_preprocess.resolve_threshold("denoise.sigma", "9"), 1.0)
        self.assertEqual(image_preprocess.resolve_threshold("denoise.sigma", "0"), 1.0)
        self.assertEqual(image_preprocess.resolve_threshold("pad", "0.9"), 0.015)

    def test_in_range_is_kept(self):
        self.assertEqual(image_preprocess.resolve_threshold("denoise.sigma", "2.5"), 2.5)
        self.assertEqual(image_preprocess.resolve_threshold("deskew.ang_tol", "30"), 30.0)

    def test_switches_default_to_off(self):
        for name in ("denoise.enabled", "deskew.enabled", "contrast.enabled"):
            self.assertFalse(image_preprocess.resolve_switch(name, None), name)

    def test_mode_defaults_to_off(self):
        self.assertEqual(image_preprocess.resolve_mode(None), "off")
        self.assertEqual(image_preprocess.resolve_mode("nonsense"), "off")
        self.assertEqual(image_preprocess.resolve_mode("ALWAYS"), "always")


class StripRepeatingLinesRoutingTest(unittest.TestCase):
    """页眉页脚清理只应作用在「保留文本层」的页上。"""

    def _texts(self):
        return [f"ACME REPORT\nbody page {i}\nACME REPORT" for i in range(6)]

    def test_manual_ignores_mixed_pages(self):
        classes = ["text", "mixed"] * 3
        cleaned = _strip_repeating_lines(self._texts(), classes)
        # 手动流水线下混合页的文本层被丢弃，因此不该参与统计：
        # 只剩 3 个文本页，达不到 4 页门槛，全部原样保留。
        self.assertEqual(cleaned, self._texts())

    def test_smart_includes_mixed_pages(self):
        classes = ["text", "mixed"] * 3
        cleaned = _strip_repeating_lines(
            self._texts(), classes, ("text", "mixed")
        )
        for page in cleaned:
            self.assertNotIn("ACME REPORT", page)
            self.assertIn("body page", page)


class TableRoutingTest(unittest.TestCase):
    """表格抽取必须覆盖所有保留文本层的页（混合页不漏）。"""

    def _run(self, classes, text_classes):
        seen = []

        def fake_extract(_content, page_index):
            seen.append(page_index)
            return [f"| table on page {page_index + 1} |"]

        with mock.patch(
            "docreader.parser.pdf_table_extractor.extract_tables_detailed",
            side_effect=fake_extract,
        ):
            tables = _extract_tables_per_page(b"%PDF-1.4", classes, text_classes=text_classes)
        return seen, tables

    def test_manual_only_text_pages(self):
        seen, tables = self._run(
            ["text", "scanned", "image", "mixed"], ("text",)
        )
        self.assertEqual(seen, [0])
        self.assertEqual(list(tables), [0])

    def test_smart_covers_mixed_pages_too(self):
        seen, tables = self._run(
            ["text", "scanned", "image", "mixed"], ("text", "mixed")
        )
        self.assertEqual(seen, [0, 3])
        self.assertEqual(sorted(tables), [0, 3])

    def test_no_page_class_is_silently_dropped(self):
        # 每个类别要么被表格抽取覆盖，要么被整页渲染覆盖（后者由
        # RENDER_PAGE_CLASSES 保证），不存在两头都不管的类别。
        classes = ["text", "scanned", "image", "mixed"]
        seen_manual, _ = self._run(classes, ("text",))
        rendered = [i for i, c in enumerate(classes) if c in pdf_mod.RENDER_PAGE_CLASSES]
        self.assertEqual(
            sorted(set(seen_manual) | set(rendered)), [0, 1, 2, 3]
        )


class FormulaRoutingTest(unittest.TestCase):
    """公式抽取必须覆盖所有保留文本层的页（混合页不漏）。"""

    class _FakePdf:
        def __getitem__(self, index):
            return object()

    def _run(self, classes, text_classes):
        seen = []

        def fake_extract(_page, _raw, page_index, *_args, **_kwargs):
            seen.append(page_index)
            return []

        texts = [""] * len(classes)
        with mock.patch(
            "docreader.parser.pdf_formula_extractor.extract_formulas_from_page",
            side_effect=fake_extract,
        ), mock.patch(
            "docreader.parser.pdf_formula_extractor.is_available", return_value=False
        ):
            _extract_and_inject_formulas(
                self._FakePdf(),
                None,
                classes,
                texts,
                "doc",
                1.0,
                85,
                0,
                {},
                text_classes=text_classes,
            )
        return seen

    def test_manual_only_text_pages(self):
        self.assertEqual(
            self._run(["text", "scanned", "image", "mixed"], ("text",)), [0]
        )

    def test_smart_covers_mixed_pages_too(self):
        self.assertEqual(
            self._run(["text", "scanned", "image", "mixed"], ("text", "mixed")),
            [0, 3],
        )


class EmbeddedImageRoutingTest(unittest.TestCase):
    """内嵌图只从文本页抽取：整页渲染的页其图已在整页图里，抽了就是重复。"""

    def _extract(self, classes):
        pdf = pdfium.PdfDocument(_make_image_pdf(2, edge=200))
        try:
            import pypdfium2.raw as raw

            return _extract_embedded_images(pdf, classes, raw, "doc", 85)
        finally:
            pdf.close()

    def test_text_pages_are_processed(self):
        self.assertTrue(self._extract(["text", "text"]))

    def test_rendered_page_classes_are_skipped(self):
        for classes in (["image", "image"], ["mixed", "mixed"], ["scanned", "scanned"]):
            self.assertEqual(self._extract(classes), {}, classes)


class ManualPipelineIntegrationTest(unittest.TestCase):
    """手动流水线（默认）：混合页只渲染、不信任其文本层 —— 行为与 FR-13 之前一致。"""

    def _parse(self, overrides, body_text="Alpha body text of the page."):
        pdf_bytes = _make_image_pdf(2)
        # 图片页没有文本层；这里伪造文本层把它们变成「混合页」，
        # 从而覆盖 ratio>=阈值 且 text_len>=阈值 的分支。
        with mock.patch.object(
            pdf_mod, "_extract_page_text", return_value=body_text
        ):
            return PDFParser(file_name="mixed.pdf", file_type="pdf", **overrides).parse_into_text(
                pdf_bytes
            ), body_text

    def test_mixed_page_is_rendered_only(self):
        doc, body = self._parse({})
        self.assertEqual(doc.metadata["page_classes"], "mixed,mixed")
        self.assertEqual(doc.metadata["mixed_page_count"], 2)
        self.assertEqual(doc.metadata["image_source_type"], "scanned_pdf")
        self.assertEqual(doc.metadata["scanned_page_count"], 2)
        self.assertEqual(doc.metadata["text_page_count"], 0)
        self.assertEqual(doc.metadata["ocr_pipeline"], "manual")
        self.assertNotIn(body, doc.content)
        self.assertIn("![mixed_page_1.jpg](images/mixed_page_1.jpg)", doc.content)
        self.assertEqual(len(doc.images), 2)

    def test_smart_mixed_page_keeps_text_and_render(self):
        doc, body = self._parse({"ocr_pipeline": "smart"})
        self.assertEqual(doc.metadata["page_classes"], "mixed,mixed")
        self.assertEqual(doc.metadata["ocr_pipeline"], "smart")
        # 混合页两者兼取：文本层进正文，整页图也进正文。
        self.assertIn(body, doc.content)
        self.assertIn("![mixed_page_1.jpg](images/mixed_page_1.jpg)", doc.content)
        self.assertEqual(doc.metadata["text_page_count"], 2)
        self.assertEqual(doc.metadata["scanned_page_count"], 2)
        self.assertIn("mixed_use_text_layer=true", doc.metadata["ocr_route"])
        # 混合页的图形已经在整页图里，不该再抽一份矢量图/内嵌图造成重复。
        self.assertEqual(len(doc.images), 2)
        self.assertEqual(doc.metadata["embedded_image_count"], 0)
        self.assertEqual(doc.metadata["vector_figure_count"], 0)

    def test_smart_mixed_page_can_drop_text_layer(self):
        doc, body = self._parse(
            {"ocr_pipeline": "smart", "smart.mixed_use_text_layer": "false"}
        )
        self.assertNotIn(body, doc.content)
        self.assertEqual(doc.metadata["text_page_count"], 0)
        self.assertEqual(doc.metadata["scanned_page_count"], 2)

    def test_source_blocks_cover_every_page(self):
        # 四分类下溯源定位不能漏页：每页至少有一个定位块。
        for overrides in ({}, {"ocr_pipeline": "smart"}):
            doc, _ = self._parse(overrides)
            pages = {b["locator"]["page"] for b in doc.source_blocks}
            self.assertEqual(pages, {1, 2}, overrides)


class SmartPipelineRenderTest(unittest.TestCase):
    def test_image_only_pdf_is_classified_image(self):
        doc = PDFParser(file_name="img.pdf", file_type="pdf").parse_into_text(
            _make_image_pdf(2)
        )
        self.assertEqual(doc.metadata["page_classes"], "image,image")
        self.assertEqual(doc.metadata["image_page_count"], 2)
        self.assertEqual(doc.metadata["mixed_page_count"], 0)
        self.assertEqual(doc.metadata["render_dpi"], 200)

    def test_scan_dense_document_raises_render_dpi(self):
        doc = PDFParser(
            file_name="img.pdf", file_type="pdf", **{"ocr_pipeline": "smart"}
        ).parse_into_text(_make_image_pdf(2))
        self.assertEqual(doc.metadata["render_dpi"], 300)
        self.assertIn("scan_dense", doc.metadata["ocr_route"])
        size = Image.open(
            io.BytesIO(base64.b64decode(doc.images["images/img_page_1.jpg"]))
        ).size
        manual = PDFParser(file_name="img.pdf", file_type="pdf").parse_into_text(
            _make_image_pdf(2)
        )
        manual_size = Image.open(
            io.BytesIO(base64.b64decode(manual.images["images/img_page_1.jpg"]))
        ).size
        self.assertGreater(size[0], manual_size[0])

    def test_manual_render_dpi_override_is_honoured(self):
        doc = PDFParser(
            file_name="img.pdf",
            file_type="pdf",
            **{"ocr_pipeline": "manual", "manual.render_dpi": "100"},
        ).parse_into_text(_make_image_pdf(1))
        self.assertEqual(doc.metadata["render_dpi"], 100)

    def test_smart_thresholds_change_classification(self):
        # 把图像占比阈值调到 0 以上、字符阈值调高：原本的图片页变成扫描页
        # （ratio 仍 >= 0，但 text_len < min_chars -> image；这里用 min_chars=0
        # 让「无文本层」不再成立，于是落到 mixed 分支）。
        doc = PDFParser(
            file_name="img.pdf",
            file_type="pdf",
            **{"ocr_pipeline": "smart", "smart.min_chars_per_page": "0"},
        ).parse_into_text(_make_image_pdf(2))
        self.assertEqual(doc.metadata["page_classes"], "mixed,mixed")


class PreprocessIntegrationTest(unittest.TestCase):
    def test_preprocess_runs_on_rendered_pages(self):
        doc = PDFParser(
            file_name="img.pdf",
            file_type="pdf",
            **{
                "ocr_pipeline": "manual",
                "preprocess.mode": "always",
                "preprocess.pad": "0.1",
            },
        ).parse_into_text(_make_image_pdf(1))
        self.assertEqual(doc.metadata["preprocess_mode"], "always")
        self.assertEqual(doc.metadata["preprocess_status"], "ok")
        self.assertIn("pad", doc.metadata["preprocess_steps"])
        self.assertEqual(doc.metadata["preprocess_pages"], 1)

    def test_preprocess_off_by_default(self):
        doc = PDFParser(file_name="img.pdf", file_type="pdf").parse_into_text(
            _make_image_pdf(1)
        )
        self.assertNotIn("preprocess_mode", doc.metadata)
        self.assertNotIn("preprocess_status", doc.metadata)

    def test_force_scanned_path_also_preprocesses(self):
        doc = PDFParser(
            file_name="forced.pdf",
            file_type="pdf",
            **{
                "manual.force_scanned": "true",
                "preprocess.mode": "always",
                "preprocess.contrast.enabled": "true",
            },
        ).parse_into_text(_make_image_pdf(1))
        self.assertEqual(doc.metadata["image_source_type"], "scanned_pdf")
        self.assertEqual(doc.metadata["scanned_page_count"], 1)
        self.assertEqual(doc.metadata["text_page_count"], 0)
        self.assertEqual(doc.metadata["preprocess_mode"], "always")
        self.assertIn("contrast", doc.metadata["preprocess_steps"])


if __name__ == "__main__":
    unittest.main()
