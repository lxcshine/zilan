import logging
from typing import Any, Optional

from docreader.models.document import Document
from docreader.parser.registry import registry
from docreader.parser.stream import (
    PARSE_DEADLINE_OVERRIDE_KEY,
    ParsedDocumentStream,
    ParseBudget,
)
from docreader.parser.web_parser import WebParser

logger = logging.getLogger(__name__)


# OLE Compound File magic used by legacy binary Microsoft Office files.
# Some WPS/Word documents keep this payload while being renamed to .docx.
_OLE_COMPOUND_FILE_MAGIC = b"\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1"

# FR-16：只承载控制信息（资源上界）的 override 键。它们在 Parser 层被消费，
# 不能透传进解析器构造函数 —— 否则会被当成引擎参数，也会污染 metadata。
_CONTROL_OVERRIDE_KEYS = (PARSE_DEADLINE_OVERRIDE_KEY,)


def split_control_overrides(
    engine_overrides: Optional[dict[str, Any]],
) -> tuple[dict[str, Any], dict[str, Any]]:
    """拆出控制类 override，返回 ``(解析器参数, 控制参数)``。"""
    overrides = dict(engine_overrides or {})
    control: dict[str, Any] = {}
    for key in _CONTROL_OVERRIDE_KEYS:
        if key in overrides:
            control[key] = overrides.pop(key)
    return overrides, control


def detect_effective_file_type(file_type: str, content: bytes) -> str:
    """Return the parser file type after checking trustworthy file magic.

    OOXML ``.docx`` files are ZIP containers, while legacy ``.doc`` files
    use the OLE Compound File format.  Word and WPS tolerate a legacy file
    renamed to ``.docx``, so route that well-known mismatch through the DOC
    parser instead of feeding binary OLE data to the DOCX parser.
    """
    normalized = file_type.lower().lstrip(".")
    if normalized == "docx" and content.startswith(_OLE_COMPOUND_FILE_MAGIC):
        logger.warning(
            "Detected legacy DOC content with a DOCX extension; using DOC parser"
        )
        return "doc"
    return normalized


class Parser:
    """Document parser facade (lightweight version).

    Converts files/URLs to markdown + image references.
    No chunking, no storage, no OCR, no VLM.
    """

    def __init__(self):
        self.registry = registry
        logger.info(
            "Parser initialized with engines: %s",
            ", ".join(self.registry.get_engine_names()),
        )

    def parse_file(
        self,
        file_name: str,
        file_type: str,
        content: bytes,
        parser_engine: Optional[str] = None,
        engine_overrides: Optional[dict[str, Any]] = None,
    ) -> Document:
        """Parse file content to markdown."""
        engine = parser_engine or ""
        overrides, _control = split_control_overrides(engine_overrides)
        logger.info(
            "Parsing file: %s, type: %s, engine: %s",
            file_name,
            file_type,
            engine or "builtin",
        )

        effective_file_type = detect_effective_file_type(file_type, content)
        cls = self.registry.get_parser_class(engine, effective_file_type)
        logger.info(
            "Creating %s parser instance for %s file",
            cls.__name__,
            effective_file_type,
        )
        parser = cls(
            file_name=file_name,
            file_type=effective_file_type,
            **overrides,
        )

        logger.info("Starting to parse file content, size: %d bytes", len(content))
        result = parser.parse(content)

        if not result.content:
            logger.warning("Parser returned empty content for file: %s", file_name)
        logger.info("Parsed file %s, content length=%d", file_name, len(result.content))
        return result

    def parse_file_stream(
        self,
        file_name: str,
        file_type: str,
        content: bytes,
        parser_engine: Optional[str] = None,
        engine_overrides: Optional[dict[str, Any]] = None,
        budget: Optional[ParseBudget] = None,
    ) -> ParsedDocumentStream:
        """为 ReadStream 解析文件，优先走解析器的懒渲染图像路径（FR-16）。

        探测方式与上游一致：解析器实现了 ``parse_stream`` 就用它（文本与元数据
        先出、页图按需渲染），否则整体解析后包装成「无懒渲染帧」的流 —— 图像仍
        由 ReadStream 逐帧发送，只是解析阶段会把它们全部物化，行为与之前一致。

        ``budget`` 是时长 / 内存 / 取消上界；触及上界时流会提前结束并带上
        ``incomplete`` 标记，已产出的部分照常返回。
        """
        engine = parser_engine or ""
        overrides, control = split_control_overrides(engine_overrides)
        effective_file_type = detect_effective_file_type(file_type, content)
        cls = self.registry.get_parser_class(engine, effective_file_type)
        logger.info(
            "Creating %s parser instance for %s file (stream)",
            cls.__name__,
            effective_file_type,
        )
        parser = cls(
            file_name=file_name,
            file_type=effective_file_type,
            **overrides,
        )

        parse_stream = getattr(parser, "parse_stream", None)
        if callable(parse_stream):
            logger.info(
                "Streaming parse for %s, size: %d bytes, deadline_override=%s",
                file_name,
                len(content),
                control.get(PARSE_DEADLINE_OVERRIDE_KEY) or "none",
            )
            return parse_stream(content, budget=budget)

        # Non-streaming parsers retain their established behavior. Their images
        # are still sent one at a time by ReadStream, but parsing itself may
        # materialize them before the first frame.
        result = parser.parse(content)
        if not result.content:
            logger.warning("Parser returned empty content for file: %s", file_name)
        return ParsedDocumentStream(
            document=result,
            image_count=len(result.images or {}),
        )

    def parse_url(
        self,
        url: str,
        title: str,
        parser_engine: Optional[str] = None,
        engine_overrides: Optional[dict[str, Any]] = None,
    ) -> Document:
        """Parse content from a URL to markdown."""
        logger.info("Parsing URL: %s, title: %s", url, title)

        parser = WebParser(title=title)
        logger.info("Starting to parse URL content")
        result = parser.parse(url.encode())

        if not result.content:
            logger.warning("Parser returned empty content for url: %s", url)
        logger.info("Parsed url %s, content length=%d", url, len(result.content))
        return result
