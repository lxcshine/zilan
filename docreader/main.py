import logging
import os
import re
import sys
import traceback
import uuid
from concurrent import futures
from typing import Optional

import grpc
from grpc_health.v1 import health_pb2_grpc
from grpc_health.v1.health import HealthServicer

from docreader.auth import AuthInterceptor, TLSConfigError, load_tls_credentials
from docreader import config
from docreader.config import CONFIG
from docreader.parser import Parser
from docreader.parser.stream import ParsedDocumentStream, ParseBudget
from docreader.proto import docreader_pb2_grpc
from docreader.parser.registry import registry
from docreader.proto.docreader_pb2 import (
    ReadRequest,
    ReadResponse,
    ImageRef,
    ReadStreamMeta,
    ReadStreamResponse,
    ListEnginesResponse,
    ParserEngineInfo,
)
from docreader.source_wire import source_blocks_to_proto
from docreader.utils.request import init_logging_request_id, request_id_context

_SURROGATE_RE = re.compile(r"[\ud800-\udfff]")


def to_valid_utf8_text(s: Optional[str]) -> str:
    if not s:
        return ""
    s = _SURROGATE_RE.sub("\ufffd", s)
    return s.encode("utf-8", errors="replace").decode("utf-8")


for handler in logging.root.handlers[:]:
    logging.root.removeHandler(handler)

handler = logging.StreamHandler(sys.stdout)
logging.root.addHandler(handler)

_level_name = (os.environ.get("LOG_LEVEL") or "INFO").upper()
_level = getattr(logging, _level_name, logging.INFO)
logging.root.setLevel(_level)

logger = logging.getLogger(__name__)
logger.info("Initializing server logging, level=%s", _level_name)

init_logging_request_id()


def _resolve_images(
    images: dict, request_id: str, storage_map: dict | None = None
) -> tuple[str, list]:
    """Resolve document images into inline bytes for the Go App to persist.

    ``images`` is a dict of {relative_path: raw_data} where raw_data is
    base64-encoded string or raw bytes.

    The Go App is solely responsible for persisting images to the configured
    storage backend (local/minio/cos/tos). This function only decodes images
    and returns them as inline bytes via ImageRef.

    Returns ("", list[ImageRef]).  image_dir_path is always empty.
    """
    import base64

    if not images:
        return "", []

    mime_map = {
        ".png": "image/png",
        ".jpg": "image/jpeg",
        ".jpeg": "image/jpeg",
        ".gif": "image/gif",
        ".webp": "image/webp",
        ".bmp": "image/bmp",
    }

    refs = []
    for ref_path, b64data in images.items():
        try:
            img_bytes = base64.b64decode(b64data)
        except Exception:
            img_bytes = b64data.encode("utf-8") if isinstance(b64data, str) else b64data

        fname = os.path.basename(ref_path) or f"{uuid.uuid4().hex}.png"
        ext = os.path.splitext(fname)[1].lower()
        mime = mime_map.get(ext, "application/octet-stream")

        refs.append(
            ImageRef(
                filename=fname,
                original_ref=ref_path,
                mime_type=mime,
                image_data=img_bytes,
            )
        )

    logger.info("Resolved %d images (mode=inline)", len(refs))
    return "", refs


def _mime_for_ref(ref_path: str) -> tuple[str, str]:
    """Return (filename, mime_type) for an image reference path."""
    mime_map = {
        ".png": "image/png",
        ".jpg": "image/jpeg",
        ".jpeg": "image/jpeg",
        ".gif": "image/gif",
        ".webp": "image/webp",
        ".bmp": "image/bmp",
    }
    fname = os.path.basename(ref_path) or f"{uuid.uuid4().hex}.png"
    ext = os.path.splitext(fname)[1].lower()
    return fname, mime_map.get(ext, "application/octet-stream")


def _iter_image_refs(images: dict):
    """Yield ImageRef one at a time, freeing each source entry as we go.

    Used by the streaming RPC so we never hold every decoded image plus its
    base64 source in memory simultaneously (the inline path's peak-memory and
    message-size problem for large scanned PDFs).
    """
    import base64

    for ref_path in list(images.keys()):
        b64data = images.pop(ref_path)
        try:
            img_bytes = base64.b64decode(b64data)
        except Exception:
            img_bytes = b64data.encode("utf-8") if isinstance(b64data, str) else b64data
        del b64data
        fname, mime = _mime_for_ref(ref_path)
        yield ImageRef(
            filename=fname,
            original_ref=ref_path,
            mime_type=mime,
            image_data=img_bytes,
        )


def _image_ref_from_streamed_data(ref_path: str, image_data: bytes) -> ImageRef:
    """Build one image reference from raw bytes yielded lazily by a parser."""
    fname, mime = _mime_for_ref(ref_path)
    return ImageRef(
        filename=fname,
        original_ref=ref_path,
        mime_type=mime,
        image_data=image_data,
    )


def _incomplete_from_metadata(metadata: dict) -> tuple[bool, str]:
    """从解析 metadata 里取出「解析未完成」标注（FR-16）。

    流式路径走 ReadStreamMeta 的 incomplete 字段；这里只服务于 unary Read ——
    解析器若已自行标注（例如整体解析路径复用了同一套 metadata 键），同样透传，
    避免降级被静默吞掉。
    """
    if str(metadata.get("parse_incomplete", "")).lower() not in {"1", "true", "yes"}:
        return False, ""
    return True, str(metadata.get("parse_incomplete_reason", "") or "")


class DocReaderServicer(docreader_pb2_grpc.DocReaderServicer):
    def __init__(self):
        super().__init__()
        self.parser = Parser()

    def _parse_request(self, request: ReadRequest):
        """Run the parser for a ReadRequest, returning (result, source_desc).

        Shared by the unary Read and streaming ReadStream RPCs.
        """
        cfg = request.config
        parser_engine = cfg.parser_engine if cfg else ""
        engine_overrides = dict(cfg.parser_engine_overrides) if cfg else {}

        if request.url:
            logger.info("Read(URL): url=%s", request.url)
            result = self.parser.parse_url(
                request.url,
                request.title,
                parser_engine=parser_engine,
                engine_overrides=engine_overrides,
            )
            return result, request.url

        file_type = request.file_type or os.path.splitext(request.file_name)[1][1:]
        logger.info(
            "Read(File): file=%s, type=%s, size=%d bytes",
            request.file_name,
            file_type,
            len(request.file_content),
        )
        result = self.parser.parse_file(
            request.file_name,
            file_type,
            request.file_content,
            parser_engine=parser_engine,
            engine_overrides=engine_overrides,
        )
        return result, request.file_name

    def Read(self, request: ReadRequest, context):
        """Unified read: file mode (file_content set) or URL mode (url set)."""
        request_id = request.request_id or str(uuid.uuid4())

        with request_id_context(request_id):
            try:
                result, source_desc = self._parse_request(request)

                if not result or not result.content:
                    error_msg = f"Failed to parse: {source_desc}"
                    logger.error(error_msg)
                    return ReadResponse(error=error_msg)

                _c = to_valid_utf8_text
                image_dir, image_refs = _resolve_images(result.images, request_id)

                metadata = (
                    {k: _c(str(v)) for k, v in result.metadata.items()}
                    if result.metadata
                    else {}
                )
                incomplete, incomplete_reason = _incomplete_from_metadata(metadata)
                response = ReadResponse(
                    markdown_content=_c(result.content),
                    image_refs=image_refs,
                    image_dir_path=image_dir,
                    metadata=metadata,
                    source_blocks=source_blocks_to_proto(result),
                    incomplete=incomplete,
                    incomplete_reason=incomplete_reason,
                )
                logger.info(
                    "Read response: content_len=%d, images=%d, incomplete=%s",
                    len(result.content),
                    len(image_refs),
                    incomplete_reason or "no",
                )
                return response

            except Exception as e:
                error_msg = f"Error reading document: {e}"
                logger.error(error_msg)
                logger.info("Traceback: %s", traceback.format_exc())
                return ReadResponse(error=str(e))

    def ReadStream(self, request: ReadRequest, context):
        """Streaming read: yields one meta frame, then one frame per image.

        Each frame is a small, independent gRPC message, so documents with many
        page images (large scanned PDFs) are returned without hitting the unary
        message-size cap, and neither side has to hold the whole payload at once.

        FR-16：解析过程本身也是流式的 —— 解析器实现了 ``parse_stream`` 时，文本
        与元数据（含 FR-05 的 source_blocks）先出，整页图由解析器**按需渲染**后
        逐帧发出，帧序即页序、ref_path 里带页码，因此顺序与来源关联都保持。
        触及时长 / 内存 / 取消上界时停止产出后续页，已解析部分照常返回，并在最后
        补一个只带进度与 ``incomplete`` 标记的 meta 帧 —— 绝不产出看似完整实则
        残缺的结果（PRD §5.3 规则 3）。
        """
        request_id = request.request_id or str(uuid.uuid4())

        with request_id_context(request_id):
            _c = to_valid_utf8_text
            parsed_stream = None
            source_desc = request.file_name or request.url
            try:
                if request.url or not callable(
                    getattr(self.parser, "parse_file_stream", None)
                ):
                    # URL 与不支持流式的解析器：整体解析后包装成单帧流（行为不变）。
                    result, source_desc = self._parse_request(request)
                    parsed_stream = ParsedDocumentStream(
                        document=result,
                        image_count=len(result.images or {}),
                    )
                else:
                    cfg = request.config
                    parser_engine = cfg.parser_engine if cfg else ""
                    engine_overrides = dict(cfg.parser_engine_overrides) if cfg else {}
                    file_type = request.file_type or os.path.splitext(
                        request.file_name
                    )[1][1:]
                    logger.info(
                        "Read(File stream): file=%s, type=%s, size=%d bytes",
                        request.file_name,
                        file_type,
                        len(request.file_content),
                    )
                    parsed_stream = self.parser.parse_file_stream(
                        request.file_name,
                        file_type,
                        request.file_content,
                        parser_engine=parser_engine,
                        engine_overrides=engine_overrides,
                        # 时长上界由 Go 侧按 DocReaderCallTimeout 下发，内存上界取
                        # 服务配置；客户端断开（取消）同样中止，避免白烧资源。
                        budget=ParseBudget.from_overrides(
                            engine_overrides,
                            is_cancelled=lambda: not context.is_active(),
                        ),
                    )
                    result = parsed_stream.document
            except Exception as e:
                logger.error("Error reading document: %s", e)
                logger.info("Traceback: %s", traceback.format_exc())
                if parsed_stream is not None:
                    parsed_stream.close()
                del request
                yield ReadStreamResponse(meta=ReadStreamMeta(error=str(e)))
                return

            # PDF 流式解析已经持有自己的临时源文件；客户端排空页图期间把请求的
            # 完整字节字段丢掉，避免同一份内容在内存里存两份。
            del request

            if not result or not result.content:
                error_msg = f"Failed to parse: {source_desc}"
                logger.error(error_msg)
                parsed_stream.close()
                yield ReadStreamResponse(meta=ReadStreamMeta(error=error_msg))
                return

            images = result.images or {}
            content_len = len(result.content)
            sent = 0
            try:
                yield ReadStreamResponse(
                    meta=ReadStreamMeta(
                        markdown_content=_c(result.content),
                        image_dir_path="",
                        metadata={k: _c(str(v)) for k, v in result.metadata.items()}
                        if result.metadata
                        else {},
                        image_count=parsed_stream.image_count,
                        source_blocks=source_blocks_to_proto(result),
                        pages_total=parsed_stream.pages_total,
                    )
                )

                for ref in _iter_image_refs(images):
                    yield ReadStreamResponse(image=ref)
                    sent += 1

                for image in parsed_stream.images:
                    yield ReadStreamResponse(
                        image=_image_ref_from_streamed_data(
                            image.ref_path, image.image_data
                        )
                    )
                    sent += 1
            except Exception:
                logger.exception("Error streaming images for %s", source_desc)
                raise
            finally:
                parsed_stream.close()

            if parsed_stream.incomplete:
                # 中止时补发一个只承载进度与「解析未完成」标记的 meta 帧：首帧的
                # metadata 早已发出，客户端据此把已收到的部分结果标注为不完整。
                parsed_stream.annotate_incomplete()
                yield ReadStreamResponse(
                    meta=ReadStreamMeta(
                        pages_done=parsed_stream.pages_done,
                        pages_total=parsed_stream.pages_total,
                        incomplete=True,
                        incomplete_reason=parsed_stream.incomplete_reason,
                    )
                )

            logger.info(
                "ReadStream response: content_len=%d, images=%d/%d, incomplete=%s",
                content_len,
                sent,
                parsed_stream.image_count,
                parsed_stream.incomplete_reason or "no",
            )

    def ListEngines(self, request, context):
        overrides = dict(getattr(request, "config_overrides", None) or {})
        engines_data = registry.list_engines(overrides=overrides or None)
        engines = [
            ParserEngineInfo(
                name=e["name"],
                description=e["description"],
                file_types=e["file_types"],
                available=e.get("available", True),
                unavailable_reason=e.get("unavailable_reason", ""),
            )
            for e in engines_data
        ]
        return ListEnginesResponse(engines=engines)


def main():
    config.print_config()

    interceptors = [AuthInterceptor()]

    server = grpc.server(
        futures.ThreadPoolExecutor(max_workers=CONFIG.grpc_max_workers),
        options=[
            ("grpc.max_send_message_length", CONFIG.grpc_max_file_size_mb),
            ("grpc.max_receive_message_length", CONFIG.grpc_max_file_size_mb),
        ],
        interceptors=interceptors,
    )

    docreader_pb2_grpc.add_DocReaderServicer_to_server(DocReaderServicer(), server)

    health_servicer = HealthServicer()
    health_pb2_grpc.add_HealthServicer_to_server(health_servicer, server)

    try:
        tls_credentials = load_tls_credentials()
    except TLSConfigError as e:
        logger.error("Refusing to start: %s", e)
        sys.exit(1)

    if tls_credentials:
        server.add_secure_port(f"[::]:{CONFIG.grpc_port}", tls_credentials)
        logger.info("Server starting on port %d with TLS", CONFIG.grpc_port)
    else:
        server.add_insecure_port(f"[::]:{CONFIG.grpc_port}")
        logger.warning(
            "Server starting on port %d WITHOUT TLS (insecure mode)", CONFIG.grpc_port
        )

    server.start()

    logger.info("Server started on port %d", CONFIG.grpc_port)
    logger.info("Server is ready to accept connections")

    try:
        server.wait_for_termination()
    except KeyboardInterrupt:
        logger.info("Received termination signal, shutting down server")
        server.stop(0)


if __name__ == "__main__":
    main()
