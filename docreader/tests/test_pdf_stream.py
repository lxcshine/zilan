"""FR-16 流式解析与大文件边界。

覆盖三件事：①按页流式产出（懒渲染，图像不提前全驻内存）；②产出顺序与来源
关联（ref_path 带页码、source_blocks 先于图像发出）；③时长 / 内存 / 取消上界
触发后**中止并保留已解析部分**，同时标注「解析未完成」。
"""

import io
import os
import time
import unittest
from dataclasses import replace
from unittest.mock import patch

from PIL import Image

from docreader.models.document import Document
from docreader.parser import pdf_parser
from docreader.parser.base_parser import BaseParser
from docreader.parser.parser import Parser, split_control_overrides
from docreader.parser.pdf_parser import PDFParser
from docreader.parser.stream import (
    INCOMPLETE_CANCELLED,
    INCOMPLETE_MEMORY,
    INCOMPLETE_TIMEOUT,
    PARSE_DEADLINE_OVERRIDE_KEY,
    ParsedDocumentStream,
    ParseBudget,
    StreamedImage,
    resident_bytes,
    track_stream,
)


def _make_image_only_pdf(num_pages: int = 2) -> bytes:
    buf = io.BytesIO()
    pages = [Image.new("RGB", (64, 64), color) for color in ("white", "black")]
    pages = (pages * ((num_pages // 2) + 1))[:num_pages]
    pages[0].save(buf, format="PDF", save_all=True, append_images=pages[1:])
    return buf.getvalue()


def _serial_config():
    """关掉文档内并行渲染，让懒渲染的调用次数可预测（Windows 上本来也串不了）。"""
    return replace(pdf_parser.CONFIG, pdf_render_parallelism=1)


class _RecordingParser(BaseParser):
    def parse_into_text(self, content: bytes) -> Document:
        return Document(content="parsed", images={"images/page.jpg": "anBlZw=="})


class _StreamingParser(BaseParser):
    def parse_into_text(self, content: bytes) -> Document:  # pragma: no cover
        raise AssertionError("parse_into_text must not be used for streaming parsers")

    def parse_stream(self, content: bytes, budget=None) -> ParsedDocumentStream:
        return ParsedDocumentStream(
            document=Document(content="streamed"),
            image_count=1,
            images=iter([StreamedImage("images/page.jpg", b"jpeg")]),
        )


class _RecordingRegistry:
    def __init__(self, parser_class):
        self.parser_class = parser_class

    def get_parser_class(self, engine: str, file_type: str):
        return self.parser_class


class ParseFileStreamRoutingTest(unittest.TestCase):
    def test_streaming_parser_is_used_when_available(self):
        parser = Parser()
        parser.registry = _RecordingRegistry(_StreamingParser)

        stream = parser.parse_file_stream("scan.pdf", "pdf", b"%PDF-data")

        self.assertEqual(stream.document.content, "streamed")
        self.assertEqual(stream.image_count, 1)
        self.assertEqual(next(stream.images).ref_path, "images/page.jpg")

    def test_non_streaming_parser_falls_back_to_single_frame_stream(self):
        parser = Parser()
        parser.registry = _RecordingRegistry(_RecordingParser)

        stream = parser.parse_file_stream("page.png", "png", b"image-data")

        self.assertEqual(stream.document.content, "parsed")
        self.assertEqual(stream.image_count, 1)
        self.assertEqual(list(stream.images), [])
        self.assertEqual(stream.document.images, {"images/page.jpg": "anBlZw=="})

    def test_deadline_override_is_not_forwarded_to_parser_kwargs(self):
        overrides = {PARSE_DEADLINE_OVERRIDE_KEY: "123", "ocr_pipeline": "smart"}

        parser_overrides, control = split_control_overrides(overrides)

        self.assertEqual(control, {PARSE_DEADLINE_OVERRIDE_KEY: "123"})
        self.assertEqual(parser_overrides, {"ocr_pipeline": "smart"})
        # 入参不能被就地改写：Go 侧的 map 会被复用。
        self.assertIn(PARSE_DEADLINE_OVERRIDE_KEY, overrides)


class PDFLazyRenderTest(unittest.TestCase):
    def test_page_images_are_rendered_only_when_consumed(self):
        content = _make_image_only_pdf(2)
        real_render = pdf_parser._render_page_to_jpeg

        with patch.object(pdf_parser, "CONFIG", _serial_config()):
            with patch.object(
                pdf_parser, "_render_page_to_jpeg", wraps=real_render
            ) as render:
                stream = PDFParser(file_name="scan.pdf").parse_stream(content)
                try:
                    # 首帧之前：文本与元数据齐备，一张页图都还没渲染。
                    self.assertEqual(stream.image_count, 2)
                    self.assertEqual(stream.document.images, {})
                    self.assertEqual(stream.document.metadata["scanned_page_count"], 2)
                    self.assertEqual(stream.pages_total, 2)
                    self.assertEqual(render.call_count, 0)
                    # FR-05：溯源块随首帧一起就绪，正文已写好页图占位。
                    self.assertTrue(stream.document.source_blocks)
                    self.assertIn("![scan_page_1.jpg](images/scan_page_1.jpg)",
                                  stream.document.content)

                    # 顺序与来源关联：第 n 帧就是第 n 页，页码编码在 ref_path 里。
                    first = next(stream.images)
                    self.assertEqual(first.ref_path, "images/scan_page_1.jpg")
                    self.assertTrue(first.image_data.startswith(b"\xff\xd8"))
                    self.assertEqual(render.call_count, 1)
                    self.assertEqual(stream.pages_done, 1)

                    second = next(stream.images)
                    self.assertEqual(second.ref_path, "images/scan_page_2.jpg")
                    self.assertEqual(stream.pages_done, 2)
                    self.assertFalse(stream.incomplete)
                finally:
                    stream.close()

    def test_scanned_pages_are_rendered_in_bounded_batches(self):
        batch = pdf_parser.PDF_STREAM_RENDER_BATCH_PAGES
        pages = batch + 5
        content = _make_image_only_pdf(pages)
        real_render = pdf_parser._render_page_to_jpeg
        real_serial = pdf_parser._iter_scanned_pages_serial_batch
        batches = []

        def tracking_serial(pdf_path, indices, *args, **kwargs):
            # 每批都会重新打开一次 PdfDocument，因此批数即渲染器重启次数。
            batches.append(list(indices))
            return real_serial(pdf_path, indices, *args, **kwargs)

        with patch.object(pdf_parser, "CONFIG", _serial_config()):
            with patch.object(
                pdf_parser, "_iter_scanned_pages_serial_batch", tracking_serial
            ):
                with patch.object(
                    pdf_parser, "_render_page_to_jpeg", wraps=real_render
                ) as render:
                    stream = PDFParser(
                        file_name="scan.pdf", pdf_force_scanned="true"
                    ).parse_stream(content)
                    try:
                        self.assertEqual(stream.image_count, pages)
                        self.assertEqual(stream.document.images, {})
                        self.assertEqual(stream.document.metadata["page_count"], pages)
                        # 首帧之前一张页图都没渲染，一批也还没开。
                        self.assertEqual(render.call_count, 0)
                        self.assertEqual(batches, [])

                        refs = [img.ref_path for img in stream.images]
                        # 页序严格递增，没有乱序或重复（FR-16 ②）。
                        self.assertEqual(
                            refs,
                            [f"images/scan_page_{i + 1}.jpg" for i in range(pages)],
                        )
                        self.assertEqual(render.call_count, pages)
                        self.assertEqual(stream.pages_done, pages)
                        self.assertFalse(stream.incomplete)
                        # 分批上界：批数 = ceil(pages / batch)，每批不超过 batch 页。
                        self.assertEqual(len(batches), 2)
                        self.assertEqual(len(batches[0]), batch)
                        self.assertEqual(len(batches[1]), pages - batch)
                    finally:
                        stream.close()

    def test_close_removes_temporary_pdf(self):
        content = _make_image_only_pdf(2)
        written = []
        real_write = pdf_parser._write_pdf_temp_file

        def tracking_write(payload):
            path = real_write(payload)
            written.append(path)
            return path

        with patch.object(pdf_parser, "CONFIG", _serial_config()):
            with patch.object(pdf_parser, "_write_pdf_temp_file", tracking_write):
                stream = PDFParser(file_name="scan.pdf").parse_stream(content)
                self.assertEqual(len(written), 1)
                self.assertTrue(os.path.isfile(written[0]))
                next(stream.images)
                stream.close()
                self.assertFalse(os.path.exists(written[0]))
                # close() 幂等：重复调用不抛异常。
                stream.close()

    def test_close_before_consuming_still_cleans_up(self):
        content = _make_image_only_pdf(2)
        written = []
        real_write = pdf_parser._write_pdf_temp_file

        def tracking_write(payload):
            path = real_write(payload)
            written.append(path)
            return path

        with patch.object(pdf_parser, "CONFIG", _serial_config()):
            with patch.object(pdf_parser, "_write_pdf_temp_file", tracking_write):
                stream = PDFParser(file_name="scan.pdf").parse_stream(content)
                stream.close()
                self.assertFalse(os.path.exists(written[0]))


class ParseBudgetTest(unittest.TestCase):
    def test_disabled_budget_never_aborts(self):
        budget = ParseBudget()

        self.assertFalse(budget.enabled)
        self.assertIsNone(budget.check())

    def test_expired_deadline_reports_timeout(self):
        budget = ParseBudget(deadline_unix=time.time() - 5)

        self.assertTrue(budget.enabled)
        self.assertEqual(budget.check(), INCOMPLETE_TIMEOUT)

    def test_future_deadline_allows_progress(self):
        budget = ParseBudget(deadline_unix=time.time() + 600)

        self.assertIsNone(budget.check())

    def test_invalid_deadline_is_ignored_not_fatal(self):
        budget = ParseBudget(deadline_unix="not-a-number")

        self.assertIsNone(budget.check())

    def test_memory_limit_triggers_abort(self):
        budget = ParseBudget(memory_limit_bytes=1024)

        with patch("docreader.parser.stream.resident_bytes", return_value=4096):
            self.assertEqual(budget.check(), INCOMPLETE_MEMORY)

    def test_memory_check_is_skipped_when_unavailable(self):
        """非 Linux（或探测失败）时 resident_bytes 返回 0，必须按未超限处理。"""
        budget = ParseBudget(memory_limit_bytes=1)

        with patch("docreader.parser.stream.resident_bytes", return_value=0):
            self.assertIsNone(budget.check())

    def test_resident_bytes_never_raises(self):
        self.assertGreaterEqual(resident_bytes(), 0)

    def test_cancellation_probe_wins_over_other_bounds(self):
        budget = ParseBudget(
            deadline_unix=time.time() + 600, is_cancelled=lambda: True
        )

        self.assertEqual(budget.check(), INCOMPLETE_CANCELLED)

    def test_broken_cancellation_probe_does_not_break_parsing(self):
        def boom():
            raise RuntimeError("context gone")

        budget = ParseBudget(is_cancelled=boom)

        self.assertIsNone(budget.check())

    def test_from_overrides_uses_deadline_and_config_memory(self):
        overrides = {PARSE_DEADLINE_OVERRIDE_KEY: str(time.time() - 1)}

        budget = ParseBudget.from_overrides(overrides)

        self.assertTrue(budget.enabled)
        self.assertEqual(budget.check(), INCOMPLETE_TIMEOUT)


class TrackStreamAbortTest(unittest.TestCase):
    def _stream(self, pages=5):
        document = Document(content="text", metadata={})
        return ParsedDocumentStream(
            document=document, image_count=pages, pages_total=pages
        )

    def test_abort_keeps_partial_results_and_marks_incomplete(self):
        stream = self._stream(5)
        rendered = []

        def pages():
            for i in range(5):
                rendered.append(i)
                yield StreamedImage(f"images/p_page_{i + 1}.jpg", b"jpeg")

        calls = {"n": 0}

        def cancel_after_three():
            calls["n"] += 1
            return calls["n"] > 3

        budget = ParseBudget(is_cancelled=cancel_after_three)
        got = list(track_stream(stream, pages(), budget))

        # 已产出的 3 页保留，后 2 页连渲染都没发生（不白烧资源）。
        self.assertEqual([img.ref_path for img in got], [
            "images/p_page_1.jpg",
            "images/p_page_2.jpg",
            "images/p_page_3.jpg",
        ])
        self.assertEqual(rendered, [0, 1, 2])
        self.assertTrue(stream.incomplete)
        self.assertEqual(stream.incomplete_reason, INCOMPLETE_CANCELLED)
        self.assertEqual(stream.pages_done, 3)
        self.assertEqual(stream.pages_total, 5)

        stream.annotate_incomplete()
        self.assertEqual(stream.document.metadata["parse_incomplete"], "true")
        self.assertEqual(
            stream.document.metadata["parse_incomplete_reason"], INCOMPLETE_CANCELLED
        )
        self.assertEqual(stream.document.metadata["pages_done"], "3")
        self.assertEqual(stream.document.metadata["pages_total"], "5")

    def test_timeout_abort_before_first_page_yields_nothing(self):
        stream = self._stream(3)
        budget = ParseBudget(deadline_unix=time.time() - 1)

        got = list(track_stream(stream, iter([StreamedImage("a", b"b")]), budget))

        self.assertEqual(got, [])
        self.assertTrue(stream.incomplete)
        self.assertEqual(stream.incomplete_reason, INCOMPLETE_TIMEOUT)
        self.assertEqual(stream.pages_done, 0)

    def test_completed_stream_is_not_marked_incomplete(self):
        stream = self._stream(2)
        budget = ParseBudget(deadline_unix=time.time() + 600, memory_limit_bytes=0)

        got = list(
            track_stream(
                stream,
                iter([StreamedImage("a", b"1"), StreamedImage("b", b"2")]),
                budget,
            )
        )

        self.assertEqual(len(got), 2)
        self.assertFalse(stream.incomplete)
        self.assertEqual(stream.pages_done, 2)
        stream.annotate_incomplete()
        self.assertNotIn("parse_incomplete", stream.document.metadata)

    def test_pdf_stream_abort_keeps_parsed_text(self):
        """端到端：超限时正文与元数据完整保留，只有页图被截断。"""
        content = _make_image_only_pdf(4)

        with patch.object(pdf_parser, "CONFIG", _serial_config()):
            budget_state = {"stop": False}
            budget = ParseBudget(is_cancelled=lambda: budget_state["stop"])
            stream = PDFParser(file_name="scan.pdf").parse_stream(content, budget=budget)
            try:
                first = next(stream.images)
                budget_state["stop"] = True
                rest = list(stream.images)

                self.assertEqual(first.ref_path, "images/scan_page_1.jpg")
                self.assertEqual(rest, [])
                self.assertTrue(stream.incomplete)
                self.assertEqual(stream.incomplete_reason, INCOMPLETE_CANCELLED)
                self.assertEqual(stream.pages_done, 1)
                self.assertEqual(stream.pages_total, 4)
                # 文本 / 溯源 / 元数据不受影响：这就是「保留已解析部分」。
                self.assertIn("![scan_page_4.jpg]", stream.document.content)
                self.assertEqual(len(stream.document.source_blocks), 4)
                self.assertEqual(stream.document.metadata["page_count"], 4)
            finally:
                stream.close()


class ReadStreamEnvelopeTest(unittest.TestCase):
    """main.ReadStream 的帧序：meta → 逐帧图像 →（中止时）尾部 incomplete meta。"""

    def _servicer(self, parser):
        from docreader.main import DocReaderServicer

        servicer = DocReaderServicer.__new__(DocReaderServicer)
        servicer.parser = parser
        return servicer

    def _request(self, deadline=None):
        from docreader.proto.docreader_pb2 import ReadConfig, ReadRequest

        overrides = {}
        if deadline is not None:
            overrides[PARSE_DEADLINE_OVERRIDE_KEY] = str(deadline)
        return ReadRequest(
            file_name="scan.pdf",
            file_type="pdf",
            file_content=b"%PDF-fake",
            config=ReadConfig(parser_engine="", parser_engine_overrides=overrides),
        )

    def _frames(self, servicer, request, context):
        return list(servicer.ReadStream(request, context))

    def _stream(self, pages=3, budget=None):
        document = Document(
            content="".join(f"![p_page_{i+1}.jpg](images/p_page_{i+1}.jpg)\n\n"
                            for i in range(pages)),
            metadata={"page_count": pages},
            source_blocks=[
                {"start": 0, "end": 4, "locator": {"type": "pdf", "page": 1,
                                                   "mapping": "exact"}}
            ],
        )
        stream = ParsedDocumentStream(
            document=document,
            image_count=pages,
            pages_total=pages,
        )

        def lazy():
            for i in range(pages):
                yield StreamedImage(f"images/p_page_{i+1}.jpg", b"jpeg-bytes")

        stream.images = track_stream(stream, lazy(), budget)
        return stream

    class _StubParser:
        def __init__(self, stream):
            self.stream = stream
            self.seen_budget = "unset"

        def parse_file_stream(self, file_name, file_type, content,
                              parser_engine=None, engine_overrides=None, budget=None):
            self.seen_budget = budget
            return self.stream

    class _Context:
        def is_active(self):
            return True

    def test_frames_keep_order_and_source_blocks(self):
        parser = self._StubParser(self._stream(3))
        servicer = self._servicer(parser)

        frames = self._frames(servicer, self._request(), self._Context())

        self.assertTrue(frames[0].HasField("meta"))
        meta = frames[0].meta
        self.assertEqual(meta.image_count, 3)
        self.assertEqual(meta.pages_total, 3)
        self.assertFalse(meta.incomplete)
        # FR-05：溯源块随首帧一起到达，早于任何图像帧。
        self.assertEqual(len(meta.source_blocks), 1)
        self.assertEqual(meta.metadata["page_count"], "3")

        self.assertEqual(
            [f.image.original_ref for f in frames[1:]],
            ["images/p_page_1.jpg", "images/p_page_2.jpg", "images/p_page_3.jpg"],
        )
        self.assertIsInstance(parser.seen_budget, ParseBudget)

    def test_aborted_stream_appends_incomplete_meta_frame(self):
        budget = ParseBudget(deadline_unix=time.time() - 1)
        parser = self._StubParser(self._stream(3, budget=budget))
        servicer = self._servicer(parser)

        frames = self._frames(
            servicer, self._request(deadline=time.time() - 1), self._Context()
        )

        self.assertTrue(frames[0].HasField("meta"))
        self.assertFalse(frames[0].meta.incomplete)
        # 超限：一张页图都没发出，但正文与元数据完整保留。
        self.assertEqual(len(frames), 2)
        tail = frames[-1].meta
        self.assertTrue(tail.incomplete)
        self.assertEqual(tail.incomplete_reason, INCOMPLETE_TIMEOUT)
        self.assertEqual(tail.pages_done, 0)
        self.assertEqual(tail.pages_total, 3)

    def test_partial_stream_reports_pages_done(self):
        state = {"stop": False}
        budget = ParseBudget(is_cancelled=lambda: state["stop"])
        stream = self._stream(4, budget=budget)
        parser = self._StubParser(stream)
        servicer = self._servicer(parser)

        frames = []
        for frame in servicer.ReadStream(self._request(), self._Context()):
            frames.append(frame)
            if frame.HasField("image"):
                state["stop"] = True

        image_frames = [f for f in frames if f.HasField("image")]
        tail = frames[-1].meta
        self.assertEqual(len(image_frames), 1)
        self.assertTrue(tail.incomplete)
        self.assertEqual(tail.incomplete_reason, INCOMPLETE_CANCELLED)
        self.assertEqual(tail.pages_done, 1)
        self.assertEqual(tail.pages_total, 4)
        # 已解析部分保留，并被打上「解析未完成」标注。
        self.assertEqual(stream.document.metadata["parse_incomplete"], "true")


if __name__ == "__main__":
    unittest.main()
