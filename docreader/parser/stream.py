"""Common result types for incremental document parsing."""

import logging
import os
import sys
import time
from dataclasses import dataclass, field
from typing import Callable, Iterator, Optional

from docreader.config import CONFIG
from docreader.models.document import Document

logger = logging.getLogger(__name__)


@dataclass(frozen=True)
class StreamedImage:
    """One image payload emitted by a parser without retaining the full set."""

    ref_path: str
    image_data: bytes


@dataclass
class ParsedDocumentStream:
    """A document header plus images that may be produced incrementally.

    ``document.images`` contains any small images already required to build the
    document. ``images`` yields the remaining payloads lazily. ``image_count``
    is the total count across both sources and is suitable for the gRPC header.
    Call ``close`` when the consumer stops before exhausting ``images`` so
    parser resources and temporary files are released.

    FR-16 追加字段：``pages_total`` / ``pages_done`` 让调用方能把「已产出的页数」
    换算成百分比进度；``incomplete`` / ``incomplete_reason`` 表示流被时长、内存
    或取消中止，此时结果是**部分解析**，必须显式告知用户而不是伪装成完整结果。
    这些字段由 :func:`track_stream` 在消费过程中回填，因此只有在迭代结束后才可信。
    """

    document: Document
    image_count: int
    images: Iterator[StreamedImage] = field(default_factory=lambda: iter(()))
    pages_total: int = 0
    pages_done: int = 0
    incomplete: bool = False
    incomplete_reason: str = ""
    _cleanup: Callable[[], None] | None = field(default=None, repr=False)
    _closed: bool = field(default=False, init=False, repr=False)

    def close(self) -> None:
        """Close the lazy image iterator and release parser-owned resources."""
        if self._closed:
            return
        self._closed = True
        try:
            close_iterator = getattr(self.images, "close", None)
            if callable(close_iterator):
                close_iterator()
        finally:
            if self._cleanup is not None:
                self._cleanup()

    def annotate_incomplete(self) -> None:
        """把「解析未完成」写进文档 metadata（键名与 Go 侧约定一致）。

        流式路径的首帧 metadata 在中止发生前就已发出，因此这份标注主要服务于
        非流式调用方与日志排查；线上告知走 ReadStreamMeta 的 incomplete 字段。
        """
        if not self.incomplete:
            return
        metadata = self.document.metadata
        metadata["parse_incomplete"] = "true"
        metadata["parse_incomplete_reason"] = self.incomplete_reason or ""
        metadata["pages_done"] = str(self.pages_done)
        metadata["pages_total"] = str(self.pages_total)

    def __enter__(self) -> "ParsedDocumentStream":
        return self

    def __exit__(self, exc_type, exc, traceback) -> None:
        self.close()


# FR-16：incomplete_reason 的取值集合，Go 侧按同一套字符串判定。
INCOMPLETE_TIMEOUT = "timeout"
INCOMPLETE_MEMORY = "memory"
INCOMPLETE_CANCELLED = "cancelled"

# Go 侧通过 parser_engine_overrides 下发的解析截止时刻（Unix 秒）。
PARSE_DEADLINE_OVERRIDE_KEY = "parse_deadline_unix"


def resident_bytes() -> int:
    """当前进程常驻内存（RSS）字节数；平台不支持或探测失败时返回 0。

    返回 0 表示「无法判定」，调用方必须把它当作未超限 —— 内存上界是保护措施，
    绝不能因为它自己不可用而让解析失败（Windows / macOS 开发环境即属此类）。
    """
    try:
        if sys.platform.startswith("linux"):
            with open("/proc/self/statm", encoding="ascii") as stat:
                return int(stat.read().split()[1]) * os.sysconf("SC_PAGE_SIZE")
        import resource

        rss = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss
        # macOS 返回字节，其余 POSIX（BSD 等）返回 KB；Linux 走上面的 /proc。
        return int(rss) if sys.platform == "darwin" else int(rss) * 1024
    except Exception:  # pragma: no cover - 非 Linux/POSIX 平台的优雅降级
        return 0


class ParseBudget:
    """流式解析的时长 / 内存 / 取消上界（FR-16，与 FR-07 资源边界协同）。

    ``check()`` 返回 ``None`` 表示可以继续产出下一帧；返回非空字符串表示应当
    立即中止，并把该字符串作为 ``incomplete_reason``。中止**不抛异常**：已经
    产出的部分是有效结果，调用方保留它并打上「解析未完成」标记。
    """

    def __init__(
        self,
        *,
        deadline_unix: Optional[float] = None,
        time_limit_seconds: float = 0,
        memory_limit_bytes: int = 0,
        is_cancelled: Optional[Callable[[], bool]] = None,
    ):
        self._deadline: Optional[float] = None
        remain: Optional[float] = None
        if deadline_unix:
            try:
                # wall-clock deadline 换算到单调时钟：系统时间被调整也不会误判。
                remain = float(deadline_unix) - time.time()
            except (TypeError, ValueError):
                logger.warning("忽略无法解析的 %s=%r", PARSE_DEADLINE_OVERRIDE_KEY, deadline_unix)
        elif time_limit_seconds and time_limit_seconds > 0:
            remain = float(time_limit_seconds)
        if remain is not None:
            self._deadline = time.monotonic() + max(0.0, remain)
        self._memory_limit = int(memory_limit_bytes or 0)
        self._is_cancelled = is_cancelled

    @classmethod
    def from_overrides(
        cls,
        engine_overrides: Optional[dict] = None,
        *,
        is_cancelled: Optional[Callable[[], bool]] = None,
    ) -> "ParseBudget":
        """按 Go 侧下发的 overrides + 服务默认配置构造上界。"""
        overrides = engine_overrides or {}
        return cls(
            deadline_unix=overrides.get(PARSE_DEADLINE_OVERRIDE_KEY),
            time_limit_seconds=CONFIG.parse_time_limit_seconds,
            memory_limit_bytes=CONFIG.parse_memory_limit_mb * 1024 * 1024,
            is_cancelled=is_cancelled,
        )

    @property
    def enabled(self) -> bool:
        """是否配置了任何一项上界（未配置时 check() 只做取消探测）。"""
        return bool(self._deadline is not None or self._memory_limit > 0)

    def check(self) -> Optional[str]:
        """返回应当中止的原因，或 ``None`` 表示可以继续。"""
        if self._is_cancelled is not None:
            try:
                if self._is_cancelled():
                    return INCOMPLETE_CANCELLED
            except Exception:  # pragma: no cover - gRPC context 异常不应中断解析
                logger.debug("取消探测失败，按未取消处理", exc_info=True)
        if self._deadline is not None and time.monotonic() >= self._deadline:
            return INCOMPLETE_TIMEOUT
        if self._memory_limit > 0:
            rss = resident_bytes()
            if rss and rss > self._memory_limit:
                logger.warning(
                    "解析内存上界触发：rss=%dMB limit=%dMB",
                    rss // (1024 * 1024),
                    self._memory_limit // (1024 * 1024),
                )
                return INCOMPLETE_MEMORY
        return None


def track_stream(
    stream: ParsedDocumentStream,
    images: Iterator[StreamedImage],
    budget: Optional[ParseBudget] = None,
) -> Iterator[StreamedImage]:
    """包装懒渲染迭代器：逐帧回填进度，触及资源上界时中止并保留已产出部分。

    上界检查发生在**拉取下一帧之前**，因此被中止时不会白渲染一页；已经 yield
    出去的帧全部有效，``stream.pages_done`` 与实际发出的图像数严格一致。

    无论是正常结束、触及上界中止，还是消费方提前 ``close()``，内层渲染迭代器都
    会被显式关闭 —— 否则 PDFium 文档句柄要等到 GC 才释放，临时文件也可能因为
    仍被占用而删不掉（Windows 上尤其明显）。
    """
    iterator = iter(images)
    try:
        while True:
            if budget is not None:
                reason = budget.check()
                if reason is not None:
                    stream.incomplete = True
                    stream.incomplete_reason = reason
                    logger.warning(
                        "流式解析中止（reason=%s）：已产出 %d/%d 页，保留已解析部分",
                        reason,
                        stream.pages_done,
                        stream.pages_total,
                    )
                    return
            try:
                image = next(iterator)
            except StopIteration:
                return
            # 计数在 yield 之前：帧一旦交出去就算「已产出」，消费方看到的进度
            # 与实际收到的图像数严格一致（不会出现「收到 3 帧却报告 2 页」）。
            stream.pages_done += 1
            yield image
    finally:
        close_iterator = getattr(iterator, "close", None)
        if callable(close_iterator):
            close_iterator()
