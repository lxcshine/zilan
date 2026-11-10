"""FR-21 表格与公式的单元格级溯源测试。

覆盖 PRD FR-21 的五条处理规则与 §5.3 的降级原则：

1. **单元格几何提取**：camelot 的 ``table.cells`` 与 pdfplumber 的
   ``Table.rows[i].cells`` 都要被保留下来，坐标系换算到 PageGeometry 需要的
   左下原点；
2. **跨页合并保留原始页**：``row_pages`` 必须在拼行的同时累积，合并后每一行
   仍指向它被读出来的那一页；
3. **公式几何透出**：裁剪区域的行号、PDF 点 bbox 与 0-1 归一化 bbox 都进结果；
4. **偏移精确对应**：追加到正文尾部的表格，每个定位块切出来的文本必须逐字
   等于该 Markdown 行；
5. **降级不阻断**：单元格几何缺失时退化为表区域 / 页级并标 ``approx``；
   ``rebase_blocks`` 在最终正文上统一执行一次后，页块与表格块都仍然对齐。
"""

import unittest
from types import SimpleNamespace
from unittest import mock

import pandas as pd

from docreader.parser import pdf_table_extractor as table_mod
from docreader.parser.pdf_formula_extractor import (
    _normalize_bbox,
    formula_markdown,
    inject_formula_markdown,
)
from docreader.parser.pdf_parser import (
    _append_tables,
    _formula_locator,
    _formula_parts,
    _table_row_locator,
)
from docreader.parser.pdf_table_extractor import (
    MergedTable,
    TableCell,
    TableResult,
    _align_cell_boxes,
    _camelot_result,
    _coerce_table,
    _normalize_box,
    _plumber_box,
    _plumber_result,
    merge_cross_page_tables,
)
from docreader.parser.source_locator import (
    PageGeometry,
    page_blocks,
    rebase_blocks,
    union_boxes,
)

# 100 x 200 的页面几何（PDF 点、左下原点）。
_GEOMETRY = PageGeometry((0.0, 0.0, 100.0, 200.0), 0)


def _cells(row_boxes):
    """把「每行一个 bbox 列表」转成 row_cells。"""
    return [[TableCell(text=text, bbox=box) for text, box in row] for row in row_boxes]


def _table(page_index, header, rows, boxes=None, bbox=None):
    """构造一张带几何的 TableResult（boxes 为 None 时单元格没有几何）。"""
    grid = [header] + rows
    lines = table_mod._grid_to_lines(table_mod._normalize_grid(grid))
    if boxes is None:
        row_cells = [[TableCell(text=c) for c in row] for row in grid]
    else:
        row_cells = _cells(boxes)
    return TableResult(
        page_index=page_index, lines=lines, row_cells=row_cells, bbox=bbox
    )


class CellGeometryTest(unittest.TestCase):
    """规则 1：单元格几何必须被保留，且坐标系与 PageGeometry 一致。"""

    def test_normalize_box_orders_corners(self):
        # camelot 的 cells 用 (x0, y_bottom, x1, y_top) 命名，顺序可能反着给。
        self.assertEqual(_normalize_box((30.0, 40.0, 10.0, 20.0)), (10.0, 20.0, 30.0, 40.0))
        self.assertIsNone(_normalize_box(None))
        self.assertIsNone(_normalize_box((1.0, 2.0, 3.0)))
        self.assertIsNone(_normalize_box((1.0, None, 3.0, 4.0)))

    def test_plumber_box_flips_to_bottom_left_origin(self):
        # pdfplumber 用左上原点：top=20、bottom=40、页高 200 →
        # 左下原点里下边是 160、上边是 180。
        self.assertEqual(_plumber_box((10.0, 20.0, 30.0, 40.0), 200.0), (10.0, 160.0, 30.0, 180.0))
        # 页高未知时不能猜几何，直接放弃（调用方降级）。
        self.assertIsNone(_plumber_box((10.0, 20.0, 30.0, 40.0), 0.0))

    def test_camelot_cells_align_with_markdown_rows(self):
        df = pd.DataFrame([["bolt", "10"]], columns=["Name", "Qty"])
        fake = SimpleNamespace(
            df=df,
            cells=[(0.0, 0.0, 10.0, 10.0), (10.0, 0.0, 20.0, 10.0),
                   (0.0, 10.0, 10.0, 20.0), (10.0, 10.0, 20.0, 20.0)],
            _bbox=(0.0, 0.0, 20.0, 20.0),
        )
        result = _camelot_result(fake, 3)
        self.assertEqual(result.page_index, 3)
        self.assertEqual(result.lines[0], "| Name | Qty |")
        self.assertEqual(result.bbox, (0.0, 0.0, 20.0, 20.0))
        # 表头行与数据行都拿到了自己的单元格框。
        self.assertEqual(result.row_cells[0][0].bbox, (0.0, 0.0, 10.0, 10.0))
        self.assertEqual(result.row_cells[1][1].bbox, (10.0, 10.0, 20.0, 20.0))

    def test_camelot_cells_without_header_row_still_align(self):
        # 首行被 pandas 吃成列名时，cells 只覆盖数据行 —— 按 offset=1 对齐，
        # 表头行退化为「没有几何」，而不是整体错位。
        df = pd.DataFrame([["bolt", "10"]], columns=["Name", "Qty"])
        fake = SimpleNamespace(
            df=df, cells=[(0.0, 0.0, 10.0, 10.0), (10.0, 0.0, 20.0, 10.0)], _bbox=None
        )
        result = _camelot_result(fake, 0)
        self.assertIsNone(result.row_cells[0][0].bbox)
        self.assertEqual(result.row_cells[1][0].bbox, (0.0, 0.0, 10.0, 10.0))
        # 表级 bbox 缺失时用单元格框的并集兜底。
        self.assertEqual(result.bbox, (0.0, 0.0, 20.0, 10.0))

    def test_align_cell_boxes_rejects_mismatched_counts(self):
        boxes = [_normalize_box((0.0, 0.0, 1.0, 1.0))] * 5
        self.assertEqual(_align_cell_boxes(boxes, 2, 2), {})
        self.assertEqual(_align_cell_boxes([], 2, 2), {})
        self.assertEqual(_align_cell_boxes(boxes, 3, 0), {})

    def test_plumber_rows_provide_per_cell_boxes(self):
        fake = SimpleNamespace(
            extract=lambda: [["Name", "Qty"], ["bolt", "10"]],
            rows=[
                SimpleNamespace(cells=[(0.0, 180.0, 40.0, 200.0), (40.0, 180.0, 80.0, 200.0)]),
                SimpleNamespace(cells=[(0.0, 160.0, 40.0, 180.0), (40.0, 160.0, 80.0, 180.0)]),
            ],
            bbox=(0.0, 160.0, 80.0, 200.0),
        )
        result = _plumber_result(fake, 1, 200.0)
        self.assertEqual(result.lines[0], "| Name | Qty |")
        self.assertEqual(result.row_cells[1][0].bbox, (0.0, 20.0, 40.0, 40.0))
        self.assertEqual(result.bbox, (0.0, 0.0, 80.0, 40.0))

    def test_plumber_missing_rows_degrades_to_text_only(self):
        fake = SimpleNamespace(
            extract=lambda: [["Name", "Qty"], ["bolt", "10"]], rows=None, bbox=None
        )
        result = _plumber_result(fake, 0, 200.0)
        self.assertIsNotNone(result)
        self.assertIsNone(result.bbox)
        self.assertTrue(all(c.bbox is None for row in result.row_cells for c in row))

    def test_union_boxes_ignores_missing(self):
        self.assertEqual(
            union_boxes([(0.0, 0.0, 1.0, 1.0), None, (2.0, 3.0, 4.0, 5.0)]),
            (0.0, 0.0, 4.0, 5.0),
        )
        self.assertIsNone(union_boxes([None, None]))


class CrossPageMergeTest(unittest.TestCase):
    """规则 2：跨页合并后每行仍保留其原始页。"""

    def test_continuation_rows_keep_their_own_page(self):
        boxes_p1 = [[("Name", (0.0, 10.0, 40.0, 20.0)), ("Qty", (40.0, 10.0, 80.0, 20.0))],
                    [("", None), ("", None)],
                    [("bolt", (0.0, 0.0, 40.0, 10.0)), ("10", (40.0, 0.0, 80.0, 10.0))]]
        boxes_p2 = [[("Name", (0.0, 190.0, 40.0, 200.0)), ("Qty", (40.0, 190.0, 80.0, 200.0))],
                    [("", None), ("", None)],
                    [("nut", (0.0, 180.0, 40.0, 190.0)), ("20", (40.0, 180.0, 80.0, 190.0))]]
        merged = merge_cross_page_tables({
            0: [_table(0, ["Name", "Qty"], [["bolt", "10"]], boxes_p1)],
            1: [_table(1, ["Name", "Qty"], [["nut", "20"]], boxes_p2)],
        })
        self.assertEqual(len(merged), 1)
        table = merged[0]
        self.assertEqual(table.table_id, "pdf:1:1")
        self.assertEqual(table.page_index, 0)
        self.assertEqual(len(table.lines), 4)
        # 续页的数据行归属**第 2 页**，没有被合并到首页上。
        self.assertEqual(table.row_pages, [0, 0, 0, 1])
        self.assertEqual(table.row_numbers, [1, 0, 2, 3])
        self.assertEqual(table.ncols, 2)
        self.assertIn("nut", table.markdown)
        # 续页行的单元格几何仍然是它自己那一页的坐标。
        self.assertEqual(table.row_cells[3][0].bbox, (0.0, 180.0, 40.0, 190.0))

    def test_unrelated_headers_stay_separate(self):
        merged = merge_cross_page_tables({
            0: [_table(0, ["Name", "Qty"], [["bolt", "10"]])],
            1: [_table(1, ["Date", "Amount"], [["2026-01-01", "5"]])],
        })
        self.assertEqual(len(merged), 2)
        self.assertEqual([t.table_id for t in merged], ["pdf:1:1", "pdf:2:2"])
        self.assertEqual([t.page_index for t in merged], [0, 1])

    def test_legacy_markdown_strings_are_coerced(self):
        # 旧调用方（以及 FR-13 的路由测试桩）给的是裸 Markdown：结构照旧可用，
        # 只是没有几何，最终降级为页级定位。
        md = "| Name | Qty |\n| --- | --- |\n| bolt | 10 |"
        merged = merge_cross_page_tables({2: [md]})
        self.assertEqual(len(merged), 1)
        self.assertEqual(merged[0].markdown, md)
        self.assertEqual(merged[0].row_pages, [2, 2, 2])
        self.assertEqual(merged[0].ncols, 2)
        self.assertTrue(all(c.bbox is None for row in merged[0].row_cells for c in row))

    def test_coerce_keeps_table_result_untouched(self):
        original = _table(0, ["A"], [["1"]])
        self.assertIs(_coerce_table(original, 5), original)

    def test_empty_input(self):
        self.assertEqual(merge_cross_page_tables({}), [])
        self.assertEqual(merge_cross_page_tables({0: [""]}), [])

    def test_row_numbers_skip_the_separator(self):
        merged = merge_cross_page_tables({0: [_table(0, ["A", "B"], [["1", "2"]])]})
        self.assertIsInstance(merged[0], MergedTable)
        self.assertEqual(merged[0].row_numbers, [1, 0, 2])


class TableRowLocatorTest(unittest.TestCase):
    """规则 5 与 §5.3：精度按几何可得性逐级降级，绝不阻断。"""

    def _merged(self, boxes=None, bbox=None, pages=None):
        table = _table(0, ["Name", "Qty"], [["bolt", "10"]], boxes, bbox)
        merged = merge_cross_page_tables({0: [table]})[0]
        if pages:
            merged.row_pages = pages
        return merged

    def test_separator_row_produces_no_block(self):
        merged = self._merged()
        self.assertIsNone(_table_row_locator(merged, 1, {0: _GEOMETRY}))

    def test_full_cell_geometry_is_exact_cell_level(self):
        boxes = [[("Name", (0.0, 10.0, 40.0, 20.0)), ("Qty", (40.0, 10.0, 80.0, 20.0))],
                 [("", None), ("", None)],
                 [("bolt", (0.0, 0.0, 40.0, 10.0)), ("10", (40.0, 0.0, 80.0, 10.0))]]
        merged = self._merged(boxes)
        locator = _table_row_locator(merged, 2, {0: _GEOMETRY})
        self.assertEqual(locator["type"], "table")
        self.assertEqual(locator["mapping"], "exact")
        self.assertEqual(locator["page"], 1)
        self.assertEqual(locator["row_start"], 2)
        self.assertEqual(locator["row_end"], 2)
        self.assertEqual(locator["col"], 1)
        self.assertEqual(locator["col_end"], 2)
        self.assertEqual(locator["table_id"], "pdf:1:1")
        self.assertEqual(locator["source_id"], "table:pdf:1:1:2:1")
        self.assertEqual(locator["quote"], "| bolt | 10 |")
        # 行区域 = 该行两个单元格的并集，归一化到 0-1、左上原点。
        self.assertEqual(locator["bbox"], [0.0, 0.95, 0.8, 1.0])

    def test_missing_cell_geometry_degrades_to_table_region(self):
        merged = self._merged(bbox=(0.0, 0.0, 80.0, 40.0))
        locator = _table_row_locator(merged, 2, {0: _GEOMETRY})
        self.assertEqual(locator["mapping"], "approx")
        self.assertEqual(locator["bbox"], [0.0, 0.8, 0.8, 1.0])
        self.assertEqual(locator["row_start"], 2)

    def test_partial_cell_geometry_degrades_to_table_region(self):
        boxes = [[("Name", (0.0, 10.0, 40.0, 20.0)), ("Qty", (40.0, 10.0, 80.0, 20.0))],
                 [("", None), ("", None)],
                 [("bolt", (0.0, 0.0, 40.0, 10.0)), ("10", None)]]
        merged = self._merged(boxes, bbox=(0.0, 0.0, 80.0, 40.0))
        self.assertEqual(_table_row_locator(merged, 2, {0: _GEOMETRY})["mapping"], "approx")

    def test_continuation_row_never_borrows_the_first_page_bbox(self):
        # 续页行只有页码可信：把首页的表区域套到第 2 页上会是错误定位。
        merged = self._merged(bbox=(0.0, 0.0, 80.0, 40.0), pages=[0, 0, 1])
        locator = _table_row_locator(merged, 2, {0: _GEOMETRY, 1: _GEOMETRY})
        self.assertEqual(locator["page"], 2)
        self.assertEqual(locator["mapping"], "approx")
        self.assertNotIn("bbox", locator)

    def test_missing_geometry_degrades_to_page_level(self):
        merged = self._merged()
        locator = _table_row_locator(merged, 2, {})
        self.assertEqual(locator["mapping"], "approx")
        self.assertNotIn("bbox", locator)
        self.assertEqual(locator["page"], 1)
        self.assertEqual(locator["row_start"], 2)

    def test_wide_row_quote_is_capped(self):
        # 宽表格行不能把响应体撑大：quote 与 Go 侧 SourceLocatorQuoteMax 对齐。
        wide = [f"c{i}" for i in range(200)]
        merged = merge_cross_page_tables({0: [_table(0, wide, wide)]})[0]
        locator = _table_row_locator(merged, 2, {0: _GEOMETRY})
        self.assertEqual(locator["col_end"], 200)
        self.assertLessEqual(len(locator["quote"]), 300)


class TableAppendTest(unittest.TestCase):
    """规则 4：追加块的偏移必须与正文逐字对应。"""

    def _merged(self):
        boxes = [[("Name", (0.0, 10.0, 40.0, 20.0)), ("Qty", (40.0, 10.0, 80.0, 20.0))],
                 [("", None), ("", None)],
                 [("bolt", (0.0, 0.0, 40.0, 10.0)), ("10", (40.0, 0.0, 80.0, 10.0))]]
        return merge_cross_page_tables({0: [_table(0, ["Name", "Qty"], [["bolt", "10"]], boxes)]})

    def test_content_shape_is_unchanged(self):
        merged = self._merged()
        expected = "body\n\n" + "\n\n".join(t.markdown for t in merged)
        content, blocks = _append_tables("body", merged, {0: _GEOMETRY})
        self.assertEqual(content, expected)
        self.assertEqual(len(blocks), 2)  # 表头行 + 数据行；分隔行不产块

    def test_every_block_slices_its_own_markdown_row(self):
        merged = self._merged()
        content, blocks = _append_tables("正文第一段", merged, {0: _GEOMETRY})
        rows = [line for line in merged[0].lines if not line.startswith("| ---")]
        self.assertEqual(len(blocks), len(rows))
        for block, row in zip(blocks, rows):
            self.assertEqual(content[block["start"]:block["end"]], row)
            self.assertGreater(block["end"], block["start"])
        # 块按正文顺序排列，互不重叠。
        starts = [b["start"] for b in blocks]
        self.assertEqual(starts, sorted(starts))
        self.assertLess(blocks[0]["end"], blocks[1]["start"])

    def test_multiple_tables_offsets_stay_exact(self):
        tables = merge_cross_page_tables({
            0: [_table(0, ["A"], [["1"]]), _table(0, ["B", "C"], [["2", "3"]])],
        })
        content, blocks = _append_tables("head", tables, {0: _GEOMETRY})
        self.assertEqual(content.count("\n\n| "), 2)
        for block in blocks:
            self.assertTrue(content[block["start"]:block["end"]].startswith("|"))
        self.assertEqual({b["locator"]["table_id"] for b in blocks}, {"pdf:1:1", "pdf:1:2"})

    def test_cell_text_with_newline_keeps_offsets(self):
        # 单元格里带换行时，按 lines 累加的偏移仍然精确（重新 split 就会错位）。
        table = _table(0, ["A", "B"], [["x\ny", "1"]])
        merged = merge_cross_page_tables({0: [table]})
        content, blocks = _append_tables("", merged, {0: _GEOMETRY})
        self.assertIn("\n", content)
        rows = [line for line in merged[0].lines if not line.startswith("| ---")]
        for block, row in zip(blocks, rows):
            self.assertEqual(content[block["start"]:block["end"]], row)
        self.assertEqual(content[blocks[-1]["end"]:], "")

    def test_rebase_once_keeps_page_and_table_blocks_aligned(self):
        # 表格追加 + apply_type_strategy 改写同时发生：统一 rebase 一次之后，
        # 页块与表格块都还能在最终正文里切出原文。
        merged = self._merged()
        base = "第一章 引言\n\n正文内容"
        content, table_blocks = _append_tables(base, merged, {0: _GEOMETRY})
        page_blocks = [
            {"start": 0, "end": len("第一章 引言"),
             "locator": {"type": "pdf", "page": 1, "mapping": "exact"}},
            {"start": len("第一章 引言") + 2, "end": len(base),
             "locator": {"type": "pdf", "page": 1, "mapping": "exact",
                         "bbox": [0.1, 0.1, 0.9, 0.2], "source_id": "pdf:1:7:13"}},
        ]
        blocks = page_blocks + table_blocks
        snippets = [content[b["start"]:b["end"]] for b in blocks]
        self.assertTrue(all(snippets))

        # 模拟类型策略改写标题层级。
        final = content.replace("第一章 引言", "# 第一章 引言")
        rebased = rebase_blocks(blocks, content, final)
        self.assertEqual(len(rebased), len(blocks))
        for block, snippet in zip(rebased, snippets):
            self.assertEqual(final[block["start"]:block["end"]], snippet)
        # 表格行的原始页归属在 rebase 之后保持不变。
        table_locator = [b for b in rebased if b["locator"]["type"] == "table"][-1]
        self.assertEqual(table_locator["locator"]["page"], 1)
        self.assertEqual(table_locator["locator"]["row_start"], 2)

    def test_rebase_drops_only_what_it_cannot_find(self):
        merged = self._merged()
        content, table_blocks = _append_tables("body", merged, {0: _GEOMETRY})
        stale = table_blocks + [
            {"start": 0, "end": 4, "locator": {"type": "pdf", "page": 9, "mapping": "exact"}}
        ]
        # 正文被整段换掉：找不到的块被丢弃（损失精度），而不是留下错误偏移。
        self.assertEqual(rebase_blocks(stale, content, "完全不同的正文"), [])


class FormulaProvenanceTest(unittest.TestCase):
    """规则 3：公式转 LaTeX 后保留裁剪区域的页码与几何。"""

    def test_normalize_bbox_uses_display_coordinates(self):
        # PDF 点 (10, 100)-(50, 120) 在 100x200 的页面上：左上原点 y 取 1 - y/200。
        self.assertEqual(
            _normalize_bbox(_GEOMETRY, (10.0, 100.0, 50.0, 120.0)), [0.1, 0.4, 0.5, 0.5]
        )
        self.assertIsNone(_normalize_bbox(None, (10.0, 100.0, 50.0, 120.0)))
        self.assertIsNone(_normalize_bbox(_GEOMETRY, None))

    def test_formula_markdown_variants(self):
        self.assertEqual(
            formula_markdown({"latex": "a^2", "is_display": True}), "$$ a^2 $$"
        )
        self.assertEqual(formula_markdown({"latex": "a^2", "is_display": False}), "$a^2$")
        self.assertEqual(
            formula_markdown({"latex": None, "ref_path": "images/x_eq1.jpg"}),
            "![formula](images/x_eq1.jpg)",
        )
        self.assertEqual(formula_markdown({"latex": None, "ref_path": ""}), "")

    def test_locator_is_exact_with_geometry(self):
        locator = _formula_locator(
            {"bbox_norm": [0.1, 0.4, 0.5, 0.5], "line_index": 6, "latex": "a^2"}, 2
        )
        self.assertEqual(locator, {
            "type": "formula",
            "page": 3,
            "mapping": "exact",
            "source_id": "formula:3:7",
            "bbox": [0.1, 0.4, 0.5, 0.5],
            "quote": "a^2",
        })

    def test_locator_degrades_to_page_level(self):
        locator = _formula_locator({"bbox_norm": None, "line_index": 0}, 0)
        self.assertEqual(locator["mapping"], "approx")
        self.assertNotIn("bbox", locator)
        self.assertEqual(locator["page"], 1)
        self.assertEqual(locator["source_id"], "formula:1:1")
        self.assertNotIn("quote", locator)

    def test_locator_tolerates_missing_line_index(self):
        self.assertEqual(_formula_locator({}, 4)["source_id"], "formula:5:1")

    def test_extract_formulas_from_page_exposes_bbox(self):
        from docreader.parser import pdf_formula_extractor as formula_mod

        chars = []
        for ch in "Intro line":
            chars.append((ch, (10.0, 300.0, 18.0, 310.0)))
        chars.append(("\n", (0.0, 0.0, 0.0, 0.0)))
        for i, ch in enumerate("∑ x_i ≥ 0"):
            chars.append((ch, (10.0 + 8.0 * i, 200.0, 18.0 + 8.0 * i, 210.0)))

        class _FakeTextPage:
            def get_text_range(self, index=0, count=None):
                text = "".join(c for c, _ in chars)
                return text if count is None else text[index:index + count]

            def count_chars(self):
                return len(chars)

            def get_charbox(self, index):
                return chars[index][1]

            def close(self):
                pass

        class _FakePage:
            def get_textpage(self):
                return _FakeTextPage()

            def get_cropbox(self):
                return (0.0, 0.0, 200.0, 400.0)

            def get_rotation(self):
                return 0

        with mock.patch.object(
            formula_mod, "_render_clip", return_value=b"jpeg-bytes"
        ), mock.patch.object(
            formula_mod, "_ocr_formula", return_value="\\sum x_i \\geq 0"
        ):
            found = formula_mod.extract_formulas_from_page(
                _FakePage(), None, 4, "doc", 2.0, 85, 1024
            )

        self.assertEqual(len(found), 1)
        formula = found[0]
        self.assertEqual(formula["page"], 4)
        self.assertEqual(formula["line_index"], 1)
        self.assertEqual(formula["latex"], "\\sum x_i \\geq 0")
        # 裁剪区域既给出 PDF 点，也给出可直接绘制的归一化 bbox。
        self.assertEqual(len(formula["bbox"]), 4)
        self.assertEqual(len(formula["bbox_norm"]), 4)
        self.assertTrue(all(0.0 <= v <= 1.0 for v in formula["bbox_norm"]))
        self.assertLess(formula["bbox_norm"][1], formula["bbox_norm"][3])
        self.assertEqual(_formula_locator(formula, 4)["mapping"], "exact")


class FormulaInjectionOrderTest(unittest.TestCase):
    """公式块必须在 page_blocks 之后 extend，否则公式密集页会整页退化。"""

    # 三行都是公式：注入 $$…$$ 后每一行在页内都出现两次。
    _PAGE = "E = mc^2\n∑ x_i ≥ 0\na^2 + b^2 = c^2"

    def _lines(self):
        return [
            {"text": text, "bbox": (10.0, 180.0 - 20.0 * i, 90.0, 190.0 - 20.0 * i)}
            for i, text in enumerate(self._PAGE.splitlines())
        ]

    def test_page_text_alone_is_located_precisely(self):
        blocks = page_blocks(self._PAGE, 0, 1, self._lines(), _GEOMETRY)
        self.assertEqual(len(blocks), 1)
        self.assertIn("bbox", blocks[0]["locator"])

    def test_injecting_formulas_degrades_the_whole_page(self):
        formulas = [{"latex": line, "is_display": True} for line in self._PAGE.splitlines()]
        injected = inject_formula_markdown(self._PAGE, formulas)
        blocks = page_blocks(injected, 0, 1, self._lines(), _GEOMETRY)
        # 命中数与出现数不一致 → 一行都放不下 → 整页只剩页级定位。
        self.assertEqual(len(blocks), 1)
        self.assertEqual(blocks[0]["start"], 0)
        self.assertEqual(blocks[0]["end"], len(injected))
        self.assertNotIn("bbox", blocks[0]["locator"])

    def test_formula_parts_are_prepared_per_page(self):
        formulas = [
            {"latex": "a^2", "is_display": True, "line_index": 3},
            {"latex": None, "ref_path": "images/d_p1_eq4.jpg", "line_index": 4},
            {"latex": None, "ref_path": "", "line_index": 5},  # 既无 LaTeX 也无图
        ]
        parts = _formula_parts({0: formulas})
        self.assertEqual(list(parts), [0])
        self.assertEqual([part for part, _ in parts[0]],
                         ["$$ a^2 $$", "![formula](images/d_p1_eq4.jpg)"])
        # 片段与 formula 一一对应，定位块拿到的仍是原始 formula。
        self.assertEqual(parts[0][0][1]["line_index"], 3)
        self.assertEqual(_formula_parts({}), {})


if __name__ == "__main__":
    unittest.main()
