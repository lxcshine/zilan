"""引用溯源定位：把解析产出的 markdown 区间映射回原文件位置（PRD FR-05）。

一个 source block 形如 ``{"start": int, "end": int, "locator": {...}}``，其中
``start``/``end`` 是 Document.content 的码点偏移（end 为开区间），locator 描述
这段文本在原文件中的位置。PDF 的 locator 为 ``{"type": "pdf", "page": n}``，
命中精确区域时再附 ``"bbox": [x0, y0, x1, y1]``——bbox 是相对**显示页面**的
0-1 归一化分数、原点取左上角，因此前端在任意缩放级别下都能直接绘制高亮框。

映射鲁棒性（FR-05 规则 2）在本模块中逐条落实：
* 归一化匹配：投影只保留字母/数字/组合符（数字语义符号例外），因此空白折叠、
  Unicode 组合形式差异都不会破坏定位；
* 数字安全匹配：``_numeric_safe_matches`` 拒绝把数字片段匹配到更长的数字内部，
  避免误配页码、脚注编号；
* 多栏感知：``_column_cursor`` 优先从同栏上一行的游标继续搜索，按栏内阅读顺序
  而不是物理行顺序定位；
* 段落边界对齐：``page_blocks`` 依据几何与列表符把行聚合成段落块，定位结果落在
  段落/块级区域上。
定位不确定时（命中数与出现数不一致、区域无任何行命中）一律放弃精确 bbox，
只保留页级 provenance，绝不猜测。
"""

from __future__ import annotations

import re
import statistics
import unicodedata
from collections import Counter
from typing import Iterable, List, Optional, Sequence, Tuple

# 从游标往后搜索一行文本的最大跨度（归一化字符数）。
_SEARCH_WINDOW = 6000
# 归一化后短于该长度的行歧义太大，不定位。
_MIN_KEY_CHARS = 2
# 归一化权重低于该值的行只有在页面内唯一时才定位：图内标签（"LLM"、"EBAE"）
# 否则会抢走正文的偏移。宽字符（CJK）按 4 计，因为每个宽字符约等于一个词。
_SHORT_KEY_WEIGHT = 16

_LIST_START_RE = re.compile(
    r"^\s*(?:"
    r"\d{1,3}(?:\.\d{1,3}){0,4}[\.、．)）]?\s"  # 1  1.  8.14.11  2)
    r"|[（(]\d{1,3}[)）]"  # (1) （1）
    r"|[一二三四五六七八九十百]+[、．.]"  # 一、
    r"|[•·●○■□◆◇▪\-–*]\s"  # 项目符号
    r"|第[一二三四五六七八九十百零\d]+[章节条款部分篇]"  # 第三章
    r")"
)
_SENTENCE_END = tuple("。．.!?！？：:；;」』”)）")


def _fold(ch: str) -> str:
    """投影为分解形式下的字母、数字与组合符。"""
    if not ch.isalnum() and not unicodedata.category(ch).startswith("M"):
        return ""
    return unicodedata.normalize("NFKD", ch).lower()


def normalize_with_positions(text: str) -> Tuple[str, List[int]]:
    """把文本投影为分解形式的字母、数字与组合符。

    返回投影串，以及投影串每个字符对应到 ``text`` 中的偏移数组。
    数字语义符号（``.,:/+-%<>=`` 等）在数字之间时例外保留，以免 1.5 与 15、
    -5 与 5、5% 与 5 归一化后无法区分。
    """
    out: List[str] = []
    pos: List[int] = []
    for i, ch in enumerate(text):
        value = unicodedata.normalize("NFKC", ch)
        folded = _fold(ch)
        if value in ".,:/+−-%‰<>=≤≥≠":
            before, after = i - 1, i + 1
            while before >= 0 and text[before] in " \t":
                before -= 1
            while after < len(text) and text[after] in " \t":
                after += 1
            prev = before >= 0 and text[before].isdigit()
            next_digit = after < len(text) and text[after].isdigit()
            semantic = ((value in ".,/:" and prev and next_digit)
                        or (value in "+-−" and next_digit)
                        or (value in "%‰" and prev)
                        or value in "<>=≤≥≠")
            if semantic:
                folded = value.replace("−", "-")
        for f in folded:
            out.append(f)
            pos.append(i)
    # 逐字符分解保留了原始偏移，但相邻字符产生的组合符仍需按规范的
    # combining class 顺序排列。
    start = 0
    while start < len(out):
        if not unicodedata.combining(out[start]):
            start += 1
            continue
        stop = start + 1
        while stop < len(out) and unicodedata.combining(out[stop]):
            stop += 1
        ordered = sorted(
            zip(out[start:stop], pos[start:stop]),
            key=lambda item: unicodedata.combining(item[0]),
        )
        out[start:stop] = [item[0] for item in ordered]
        pos[start:stop] = [item[1] for item in ordered]
        start = stop
    return "".join(out), pos


def normalize(text: str) -> str:
    return normalize_with_positions(text)[0]


def _key_weight(key: str) -> int:
    key = unicodedata.normalize("NFC", key)
    return sum(4 if unicodedata.east_asian_width(ch) in ("W", "F") else 1 for ch in key)


def _column_cursor(boxes, idx: int, ends: List[Optional[int]]) -> Optional[int]:
    """``boxes[idx]`` 所在栏中、位于其上方最近一个已定位行的结束偏移。"""
    x0, y0, x1, _ = boxes[idx]
    for j in range(idx - 1, max(-1, idx - 80), -1):
        if ends[j] is None:
            continue
        px0, py0, px1, _ = boxes[j]
        if px0 < x1 and x0 < px1 and py0 > y0:
            return ends[j]
    return None


def _numeric_safe_matches(text: str, key: str) -> List[int]:
    """数字安全匹配：跳过嵌在更长数字/编号内部的命中（页码、脚注编号）。"""
    out = []
    if not key:
        return out
    start = 0
    while (at := text.find(key, start)) >= 0:
        before = text[at - 1] if at else ""
        end = at + len(key)
        after = text[end] if end < len(text) else ""
        left = key[0].isdigit() and bool(before) and (before.isdigit() or before in ".,:/+−-")
        right = key[-1].isdigit() and bool(after) and (after.isdigit() or after in ".,:/%‰")
        if not left and not right:
            out.append(at)
        start = at + 1
    return out


def locate_lines(
    page_text: str,
    line_texts: Sequence[str],
    boxes: Optional[Sequence[Tuple[float, float, float, float]]] = None,
) -> List[Optional[int]]:
    """定位每个视觉行在页文本中的起始偏移，逐行返回偏移或 None。

    行按顺序在上一处命中之后搜索，使重复短语落到正确的那一次出现。传入
    ``boxes``（PDF 点、左下原点）时，先从同栏上一行的游标继续搜索——并排双栏
    的字形行是交错到达的。两个游标都没找到时，再从页首搜索一次，以容忍文本
    抽取器的栏顺序与字形分组顺序不一致的情况。

    歧义即放弃：某行归一化文本在页内的命中数与它在待定位行中的出现数不一致
    （说明页里有未被解释的重复文本，例如目录），或短行在页内出现多次时，
    一律返回 None，由调用方降级为页级定位。
    """
    norm, pos = normalize_with_positions(page_text)
    hits: List[Optional[int]] = []
    ends: List[Optional[int]] = []
    cursor = 0
    counts = Counter(normalize(line) for line in line_texts)
    occurrences = {key: _numeric_safe_matches(norm, key) for key in counts}
    for idx, line in enumerate(line_texts):
        key = normalize(line)
        if len(unicodedata.normalize("NFC", key)) < _MIN_KEY_CHARS or len(occurrences[key]) != counts[key] or (
            _key_weight(key) < _SHORT_KEY_WEIGHT and len(occurrences[key]) > 1
        ):
            hits.append(None)
            ends.append(None)
            continue
        cursors = [cursor]
        if boxes is not None:
            column = _column_cursor(boxes, idx, ends)
            if column is not None:
                cursors.insert(0, column)
        at = -1
        for start_at in cursors:
            at = next((hit for hit in occurrences[key] if start_at <= hit <= start_at + _SEARCH_WINDOW), -1)
            if at >= 0:
                break
        if at < 0:
            at = occurrences[key][0] if len(occurrences[key]) == 1 else -1
            if at < 0:
                hits.append(None)
                ends.append(None)
                continue
        else:
            cursor = at + len(key)
        ends.append(at + len(key))
        start = pos[at]
        # 把归一化跳过、但同属该文本行的前导标点/标记（括号、"## "、列表符）
        # 一并纳入定位起点。
        while start > 0 and page_text[start - 1] != "\n" and not page_text[start - 1].isalnum():
            start -= 1
        hits.append(start)
    return hits


class PageGeometry:
    """把 PDF 用户空间点坐标换算为显示页面的 0-1 归一化分数。"""

    def __init__(self, cropbox: Tuple[float, float, float, float], rotation: int = 0):
        left, bottom, right, top = cropbox
        self.left, self.bottom = min(left, right), min(bottom, top)
        self.width = abs(right - left) or 1.0
        self.height = abs(top - bottom) or 1.0
        self.rotation = int(rotation or 0) % 360

    def bbox(self, box: Tuple[float, float, float, float]) -> List[float]:
        x0, y0, x1, y1 = box
        # 先转成未旋转、左上原点的归一化坐标。
        ux0 = (x0 - self.left) / self.width
        ux1 = (x1 - self.left) / self.width
        uy0 = 1.0 - (y1 - self.bottom) / self.height
        uy1 = 1.0 - (y0 - self.bottom) / self.height
        corners = [self._rotate(ux0, uy0), self._rotate(ux1, uy1)]
        xs = [c[0] for c in corners]
        ys = [c[1] for c in corners]
        # 越界坐标收敛到页面边界内（对应 FR-05 异常表：区域超出页几何时
        # 定位到最近有效区域），并保留 4 位小数以控制体积。
        return [
            round(_clamp(min(xs)), 4),
            round(_clamp(min(ys)), 4),
            round(_clamp(max(xs)), 4),
            round(_clamp(max(ys)), 4),
        ]

    def _rotate(self, x: float, y: float) -> Tuple[float, float]:
        # 阅读器按 /Rotate 顺时针显示页面。
        if self.rotation == 90:
            return 1.0 - y, x
        if self.rotation == 180:
            return 1.0 - x, 1.0 - y
        if self.rotation == 270:
            return y, 1.0 - x
        return x, y


def _clamp(v: float) -> float:
    return 0.0 if v < 0 else 1.0 if v > 1 else v


def _union(boxes: Iterable[Tuple[float, float, float, float]]):
    boxes = list(boxes)
    return (
        min(b[0] for b in boxes),
        min(b[1] for b in boxes),
        max(b[2] for b in boxes),
        max(b[3] for b in boxes),
    )


def union_boxes(boxes: Iterable[Optional[Tuple[float, float, float, float]]]):
    """多个 PDF 点 bbox 的并集，空项忽略；一个都没有时返回 None。

    FR-21 用它把一行里各单元格的几何区域合成整行的区域，因此缺项不能让整个
    并集失败——调用方据此判断该退化到哪一级精度。
    """
    present = [b for b in boxes if b is not None]
    if not present:
        return None
    return _union(present)


def _line_gap(prev: dict, cur: dict) -> Optional[float]:
    """``prev`` 到 ``cur`` 的垂直间距，仅当两行同栏时有值。"""
    px0, py0, px1, _ = prev["bbox"]
    cx0, _, cx1, cy1 = cur["bbox"]
    if cx1 < px0 or cx0 > px1 or cy1 > py0:
        return None
    return py0 - cy1


def _starts_paragraph(
    prev: dict, cur: dict, line_h: float, usual_gap: float, col_right: float
) -> bool:
    """判断 ``cur`` 是否紧接 ``prev`` 开启了一个新的视觉段落。

    bbox 是左下原点的 PDF 点坐标，因此向下阅读意味着 y 递减。``usual_gap``
    是该页段内相邻行的典型间距，用来吸收双倍行距等排版。
    """
    px0, py0, px1, _ = prev["bbox"]
    cx0, _, cx1, cy1 = cur["bbox"]
    if cx1 < px0 or cx0 > px1:
        return True  # 换到了另一栏
    if cy1 > py0 + line_h * 0.5:
        return True  # 往上回去了：新的一栏或新的区域
    if py0 - cy1 > usual_gap * 1.5 + line_h * 0.3:
        return True  # 垂直间距明显大于常规行距
    if _LIST_START_RE.match(cur["text"]):
        return True
    ends_sentence = prev["text"].rstrip().endswith(_SENTENCE_END)
    short = col_right - px1 > line_h * 2
    return ends_sentence and short


def page_blocks(
    page_text: str,
    offset: int,
    page_number: int,
    lines: Sequence[dict],
    geometry: Optional[PageGeometry],
) -> List[dict]:
    """为位于文档 ``offset`` 处的一个文本页生成 source blocks。

    ``lines`` 是视觉行 ``{"text", "bbox"}``（bbox 为 PDF 点）。行先在
    ``page_text`` 中定位，再按几何聚合成段落，每个段落产出一个只覆盖其已命中
    行的块，bbox 取这些行的并集。未命中的间隙只保留页级 provenance，绝不
    继承上一段的几何区域。
    """
    end = offset + len(page_text)
    page_only = {"type": "pdf", "page": page_number, "mapping": "exact"}
    if not page_text:
        return []
    placed = []
    if lines and geometry is not None:
        starts = locate_lines(
            page_text, [ln["text"] for ln in lines], [ln["bbox"] for ln in lines]
        )
        placed = sorted(
            (
                (at, ln)
                for at, ln in zip(starts, lines)
                if at is not None
            ),
            key=lambda item: item[0],
        )
    if not placed:
        # 整页没有任何行命中：只产出页级定位（前端仍可跳转到该页）。
        return [{"start": offset, "end": end, "locator": dict(page_only)}]

    heights = [ln["bbox"][3] - ln["bbox"][1] for _, ln in placed if ln["bbox"][3] > ln["bbox"][1]]
    line_h = statistics.median(heights) if heights else 10.0
    gaps = [
        g
        for g in (_line_gap(a[1], b[1]) for a, b in zip(placed, placed[1:]))
        if g is not None and g >= 0
    ]
    usual_gap = min(statistics.median(gaps), line_h * 1.5) if gaps else line_h * 0.5

    norm, positions = normalize_with_positions(page_text)

    def line_end(at, line):
        key = normalize(line["text"])
        norm_start = next((i for i, p in enumerate(positions) if p >= at), len(norm))
        hit = norm.find(key, norm_start)
        if hit < 0:
            return at
        end_at = max(positions[hit:hit + len(key)]) + 1
        while end_at < len(page_text) and not page_text[end_at].isalnum():
            if page_text[end_at] == "\n":
                return end_at + 1
            end_at += 1
        return end_at

    paragraphs: List[Tuple[int, List[dict]]] = []
    for at, ln in placed:
        ln = {**ln, "_source_end": line_end(at, ln)}
        if paragraphs:
            prev = paragraphs[-1][1][-1]
            col_right = max(p["bbox"][2] for p in paragraphs[-1][1])
            col_right = max(col_right, ln["bbox"][2])
            previous_end = prev["_source_end"]
            gap_is_empty = not normalize(page_text[previous_end:at])
            if gap_is_empty and not _starts_paragraph(prev, ln, line_h, usual_gap, col_right):
                paragraphs[-1][1].append(ln)
                continue
        paragraphs.append((at, [ln]))

    blocks: List[dict] = []
    cursor = 0
    for at, members in paragraphs:
        stop = members[-1]["_source_end"]
        if stop <= at:
            continue
        if at > cursor and page_text[cursor:at].strip():
            # 段落之前的未匹配文本单独成块，只有页级 provenance。
            blocks.append({"start": offset + cursor, "end": offset + at, "locator": dict(page_only)})
        locator = dict(page_only)
        locator["bbox"] = geometry.bbox(_union(m["bbox"] for m in members))
        locator["source_id"] = f"pdf:{page_number}:{at}:{stop}"
        blocks.append({"start": offset + at, "end": offset + stop, "locator": locator})
        cursor = stop
    if cursor < len(page_text) and page_text[cursor:].strip():
        blocks.append({"start": offset + cursor, "end": end, "locator": dict(page_only)})
    return blocks


def rebase_blocks(
    blocks: List[dict],
    base_content: str,
    content: str,
) -> List[dict]:
    """解析后处理改写正文时，把 source blocks 重新锚定到最终正文。

    Zilan 的 PDF 流水线在组装 markdown 之后还可能追加跨页表格、按文档类型
    改写标题层级（见 ``pdf_type_strategies``）。这些改写会让记录好的偏移失效，
    而错误的偏移比没有偏移更糟——它会把引用高亮到完全无关的位置。因此：
    * 正文未变：原样返回；
    * 仅在尾部追加：偏移依旧有效，原样返回；
    * 其它改写：按块内原文在最终正文中顺序重新查找，找得到就平移，找不到就
      丢弃该块（丢弃只会损失精度，不会产生错误定位）。
    """
    if not blocks or base_content == content:
        return blocks
    if content.startswith(base_content):
        return blocks
    out: List[dict] = []
    cursor = 0
    for block in blocks:
        start, end = block.get("start"), block.get("end")
        if not isinstance(start, int) or not isinstance(end, int) or end <= start:
            continue
        if start > len(base_content) or end > len(base_content):
            continue
        snippet = base_content[start:end]
        if not snippet.strip():
            continue
        at = content.find(snippet, cursor)
        if at < 0:
            continue
        out.append({"start": at, "end": at + len(snippet), "locator": block["locator"]})
        cursor = at + len(snippet)
    return out
