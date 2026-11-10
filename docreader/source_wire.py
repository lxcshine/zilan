"""source blocks 的上线（wire）形态，见 docreader.parser.source_locator。

单独成模块是为了让 docreader.main 之外也能引用，且不必加载解析器注册表。
"""

import json

from docreader.proto.docreader_pb2 import SourceBlock


def source_blocks_to_proto(document) -> list:
    """把文档的 source blocks 转换为 proto 形态。

    畸形条目直接跳过：定位信息是可选的附加能力，绝不能因为它而让解析失败。
    """
    out = []
    for block in getattr(document, "source_blocks", None) or []:
        try:
            start, end = int(block["start"]), int(block["end"])
            locator = block["locator"]
            if end <= start or start < 0 or not locator.get("type"):
                continue
            out.append(
                SourceBlock(
                    start=start,
                    end=end,
                    locator_json=json.dumps(locator, ensure_ascii=False),
                )
            )
        except (KeyError, TypeError, ValueError, AttributeError):
            continue
    return out
