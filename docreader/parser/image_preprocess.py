"""OCR 前处理（FR-13）：去噪 / 纠偏 / 对比度增强 / 补白边。

设计约束（与 PRD 一致）：

* **默认全部关闭**。任何一个步骤都要显式打开才会跑，因此升级后既有部署的
  解析结果逐字节不变。
* **只用 docreader 已有的依赖**。去噪、对比度增强、补白边全部由 Pillow 完成；
  只有「纠偏」需要 numpy 做投影轮廓统计（numpy 已随 pandas 间接安装）。
  不引入 opencv / rapidocr / onnxruntime 这类重依赖。
* **可选依赖做 import 守卫，缺失时优雅降级**。numpy 不在时纠偏标记为不可用并
  记一条日志，其余步骤照常；Pillow 不在时整个前处理不可用。
* **绝不抛异常阻断解析**。任何一步失败都返回原图，并把原因写进
  ``PreprocessResult.notes``，供上层记进 metadata 做诊断。

参数校验范式对齐上游 ``image_preprocess.resolve_threshold``：**越界拒绝并回落
默认值 + warning，不做 clamp**。悄悄把 6.0 改成 5.0 会让用户以为自己的配置
生效了，这比拒绝更糟。
"""

from __future__ import annotations

import io
import logging
import os
from dataclasses import dataclass, field
from typing import Any, Dict, Optional, Tuple

logger = logging.getLogger(__name__)

# --- 可选依赖 import 守卫 ----------------------------------------------------
try:  # pragma: no cover - 取决于运行环境
    from PIL import Image, ImageFilter, ImageOps
except Exception:  # pragma: no cover
    Image = None  # type: ignore[assignment]
    ImageFilter = None  # type: ignore[assignment]
    ImageOps = None  # type: ignore[assignment]
    _PIL_IMPORT_ERROR = "Pillow 不可用"
else:
    _PIL_IMPORT_ERROR = ""

try:  # pragma: no cover - 取决于运行环境
    import numpy as _np
except Exception:  # pragma: no cover
    _np = None  # type: ignore[assignment]
    _NUMPY_IMPORT_ERROR = "numpy 不可用，纠偏已降级为跳过"
else:
    _NUMPY_IMPORT_ERROR = ""


def is_available() -> bool:
    """前处理整体是否可用（只取决于 Pillow）。"""
    return Image is not None


def deskew_available() -> bool:
    """纠偏步骤是否可用（额外需要 numpy）。"""
    return is_available() and _np is not None


def unavailable_reason() -> str:
    """不可用原因，供日志与 metadata 记录；可用时返回空串。"""
    if not is_available():
        return _PIL_IMPORT_ERROR or "Pillow 不可用"
    return ""


# --- 参数声明：{名字: (默认值, 下界, 上界)} -----------------------------------
# 与 Go 侧 types.PreprocessFields() 一一对应，两边改一处必须同步改另一处。
_THRESHOLDS: Dict[str, Tuple[float, float, float]] = {
    "denoise.sigma": (1.0, 0.1, 5.0),
    "deskew.ang_tol": (15.0, 0.0, 90.0),
    "deskew.min_score": (0.5, 0.0, 1.0),
    "contrast.clip": (2.0, 0.0, 5.0),
    "pad": (0.015, 0.0, 0.2),
}

# 开关类参数：{名字: 默认值}。默认全关（PRD 要求）。
_SWITCHES: Dict[str, bool] = {
    "denoise.enabled": False,
    "deskew.enabled": False,
    "contrast.enabled": False,
}

# 前处理总开关的取值。off 为默认。
MODE_OFF = "off"
MODE_AUTO = "auto"
MODE_ALWAYS = "always"

# Go 侧下发 override 时统一加的前缀（types.PreprocessParamPrefix）。
_OVERRIDE_PREFIX = "preprocess."
_MODE_OVERRIDE_KEY = _OVERRIDE_PREFIX + "mode"

# 纠偏：探测用的缩略图边长与搜索步长。缩到 800px 以内是为了让几十次旋转的
# 总耗时保持在几十毫秒量级 —— 纠偏默认关闭，打开时也不该拖慢解析。
_DESKEW_PROBE_EDGE = 800
_DESKEW_ANGLE_STEP = 0.5
# 小于该角度不旋转：肉眼不可辨，旋转反而引入插值损失。
_DESKEW_MIN_ANGLE = 0.3
# 投影轮廓方差相对提升达到该比例时记为满分置信度。
_DESKEW_FULL_SCORE_GAIN = 0.5

_TRUTHY = {"1", "true", "yes", "y", "on"}
_FALSY = {"0", "false", "no", "n", "off", ""}


def _env_name(key: str) -> str:
    """``denoise.sigma`` -> ``DOCREADER_PREPROCESS_DENOISE_SIGMA``。"""
    return "DOCREADER_PREPROCESS_" + key.replace(".", "_").upper()


def _to_float(raw: Any) -> Optional[float]:
    if isinstance(raw, bool):
        return 1.0 if raw else 0.0
    if isinstance(raw, (int, float)):
        return float(raw)
    if isinstance(raw, str):
        text = raw.strip()
        if not text:
            return None
        try:
            return float(text)
        except ValueError:
            return None
    return None


def _to_bool(raw: Any) -> Optional[bool]:
    if isinstance(raw, bool):
        return raw
    if isinstance(raw, (int, float)):
        return bool(raw)
    if isinstance(raw, str):
        text = raw.strip().lower()
        if text in _TRUTHY:
            return True
        if text in _FALSY:
            return False
    return None


def resolve_threshold(name: str, raw: Any = None) -> float:
    """解析一个数值参数：显式值 > 环境变量 > 默认值。

    越界或无法解析时**拒绝并回落默认值**，同时打一条 warning —— 不 clamp。
    """
    default, low, high = _THRESHOLDS[name]
    if raw is None:
        raw = os.environ.get(_env_name(name))
    value = _to_float(raw)
    if value is None:
        if raw is not None and str(raw).strip() != "":
            logger.warning(
                "preprocess: %s=%r 无法解析为数值，回落默认值 %.4g", name, raw, default
            )
        return default
    if value < low or value > high:
        logger.warning(
            "preprocess: %s=%.4g 超出取值范围 [%.4g, %.4g]，拒绝并回落默认值 %.4g",
            name, value, low, high, default,
        )
        return default
    return value


def resolve_switch(name: str, raw: Any = None) -> bool:
    """解析一个开关参数：显式值 > 环境变量 > 默认值（默认全关）。"""
    default = _SWITCHES[name]
    if raw is None:
        raw = os.environ.get(_env_name(name))
    value = _to_bool(raw)
    if value is None:
        if raw is not None and str(raw).strip() != "":
            logger.warning(
                "preprocess: %s=%r 不是合法布尔值，回落默认值 %s", name, raw, default
            )
        return default
    return value


def resolve_mode(raw: Any = None) -> str:
    """解析前处理模式，未知值回落 off（默认关闭）。"""
    if raw is None:
        raw = os.environ.get("DOCREADER_PREPROCESS_MODE")
    text = str(raw or "").strip().lower()
    if text in (MODE_AUTO, MODE_ALWAYS):
        return text
    if text and text != MODE_OFF:
        logger.warning("preprocess: mode=%r 未知，回落 off", raw)
    return MODE_OFF


def effective_thresholds(overrides: Optional[Dict[str, Any]] = None) -> Dict[str, Any]:
    """返回一份完整生效的前处理参数（含开关与模式），供启动日志与实际执行使用。"""
    overrides = overrides or {}
    out: Dict[str, Any] = {"mode": resolve_mode(overrides.get("mode"))}
    for name in _SWITCHES:
        out[name] = resolve_switch(name, overrides.get(name))
    for name in _THRESHOLDS:
        out[name] = resolve_threshold(name, overrides.get(name))
    return out


def parse_overrides(kwargs: Dict[str, Any]) -> Dict[str, Any]:
    """从 parser overrides（``**kwargs``）里摘出 ``preprocess.*`` 键。

    Go 侧经 ``parser_engine_overrides`` 下发的键形如
    ``preprocess.denoise.sigma``、``preprocess.mode``，值一律是字符串。
    """
    out: Dict[str, Any] = {}
    for key, value in (kwargs or {}).items():
        if not isinstance(key, str) or not key.startswith(_OVERRIDE_PREFIX):
            continue
        bare = key[len(_OVERRIDE_PREFIX):]
        if bare == "mode":
            out["mode"] = value
        elif bare in _SWITCHES or bare in _THRESHOLDS:
            out[bare] = value
    return out


def any_step_enabled(params: Dict[str, Any]) -> bool:
    """是否至少启用了一个前处理步骤。"""
    return any(params.get(name) for name in _SWITCHES) or float(params.get("pad", 0.0) or 0.0) > 0


@dataclass
class PreprocessResult:
    """一次前处理的结果。``status`` 是**判决**而不是异常：

    ``ok``          至少执行了一个步骤，``image`` 是处理后的图；
    ``no_change``   参数有效但没有任何步骤需要执行（或都不满足触发条件）；
    ``skipped``     模式为 off，或没有任何步骤被启用；
    ``unavailable`` 依赖缺失或图片无法解码，原样返回。
    """

    status: str = "skipped"
    image: Any = None
    applied: list = field(default_factory=list)
    angle: float = 0.0
    score: float = 0.0
    notes: str = ""

    def to_metadata(self) -> Dict[str, str]:
        """转成可写进 Document.metadata 的字符串字典（可解释、可诊断）。"""
        return {
            "preprocess_status": self.status,
            "preprocess_applied": ",".join(self.applied),
            "preprocess_angle": f"{self.angle:.2f}",
            "preprocess_notes": self.notes,
        }


def _normalize_mode(img):
    """把图片规范化到中值滤波支持的模式（L / RGB）。"""
    if img.mode in ("L", "RGB"):
        return img
    return img.convert("RGB")


def _denoise(img, sigma: float):
    """中值滤波去噪，用 blend 控制强度：sigma 越小越接近原图。

    中值滤波对扫描噪点与 JPEG 压缩块有效，且不像高斯模糊那样糊掉笔画边缘。
    核大小由 sigma 分档得到（Pillow 的 RankFilter 只接受奇数核）。
    """
    if sigma <= 0:
        return img, False
    if sigma >= 2.5:
        size = 7
    elif sigma >= 1.5:
        size = 5
    else:
        size = 3
    # 核不能超过图像短边（Pillow 要求 size <= 图像尺寸）。
    size = max(3, min(size, (min(img.size) // 2) * 2 + 1))
    alpha = min(1.0, max(0.0, sigma / 3.0))
    if alpha <= 0:
        return img, False
    filtered = img.filter(ImageFilter.MedianFilter(size))
    return Image.blend(img, filtered, alpha), True


def _row_variance(arr) -> float:
    """水平投影轮廓的方差：文字行与空白行交替越明显，方差越大。"""
    sums = arr.sum(axis=1, dtype="float64")
    if sums.size == 0:
        return 0.0
    return float(sums.var())


def _estimate_skew(img, ang_tol: float) -> Tuple[float, float]:
    """用投影轮廓法估计纠偏角，返回 (correction_angle_deg, confidence)。

    把缩略图按候选角度旋转，取「行灰度和」方差最大的角度：文字行水平时，
    行与行之间的空白让方差最大。confidence 由相对 0 度的方差提升归一化得到。
    """
    probe = img.convert("L")
    probe.thumbnail((_DESKEW_PROBE_EDGE, _DESKEW_PROBE_EDGE))
    arr = _np.asarray(probe, dtype="float32")
    inverted = 255.0 - arr
    # 自适应阈值：只保留明显比背景暗的像素（文字/线条），抑制渐变背景。
    threshold = float(inverted.mean() + inverted.std())
    mask = _np.clip(inverted - threshold, 0, 255).astype("uint8")
    if not mask.any():
        return 0.0, 0.0

    base = Image.fromarray(mask, "L")
    base_var = _row_variance(_np.asarray(base, dtype="float32"))
    best_angle, best_var = 0.0, base_var

    limit = max(0.0, float(ang_tol))
    angle = _DESKEW_ANGLE_STEP
    while angle <= limit + 1e-9:
        for candidate in (angle, -angle):
            rotated = base.rotate(
                candidate, resample=Image.BILINEAR, fillcolor=0, expand=False
            )
            variance = _row_variance(_np.asarray(rotated, dtype="float32"))
            if variance > best_var:
                best_var, best_angle = variance, candidate
        angle += _DESKEW_ANGLE_STEP

    if base_var <= 0:
        return best_angle, 0.0
    gain = (best_var - base_var) / base_var
    score = min(1.0, max(0.0, gain / _DESKEW_FULL_SCORE_GAIN))
    return best_angle, score


def _deskew(img, ang_tol: float, min_score: float):
    """纠偏。numpy 缺失、置信度不足或角度过小时原样返回。"""
    if not deskew_available():
        return img, 0.0, 0.0, False, _NUMPY_IMPORT_ERROR
    angle, score = _estimate_skew(img, ang_tol)
    if abs(angle) < _DESKEW_MIN_ANGLE:
        return img, angle, score, False, "倾斜角过小，跳过"
    if score < min_score:
        return img, angle, score, False, f"置信度 {score:.2f} < {min_score:.2f}，跳过"
    fill = 255 if img.mode == "L" else (255, 255, 255)
    rotated = img.rotate(angle, resample=Image.BICUBIC, expand=False, fillcolor=fill)
    return rotated, angle, score, True, ""


def _contrast(img, clip: float):
    """自动色阶拉伸，两端各裁掉 clip% 的离群像素。"""
    cutoff = max(0.0, min(clip, 49.0))
    return ImageOps.autocontrast(img, cutoff=cutoff), True


def _pad(img, ratio: float):
    """四周补白边，帮助 OCR 处理贴边内容。"""
    if ratio <= 0:
        return img, False
    border = int(round(ratio * min(img.size)))
    if border <= 0:
        return img, False
    fill = 255 if img.mode == "L" else (255, 255, 255)
    return ImageOps.expand(img, border=border, fill=fill), True


def preprocess_image(img, params: Optional[Dict[str, Any]] = None) -> PreprocessResult:
    """对一张 PIL 图片执行已启用的前处理步骤，顺序：去噪 → 纠偏 → 对比度 → 补白边。

    任何异常都被吞掉并返回原图（``status="unavailable"``），因为前处理是可选
    增强，绝不能因为它失败而让整份文档解析失败。
    """
    if not is_available() or img is None:
        return PreprocessResult(status="unavailable", image=img, notes=unavailable_reason() or "无图片")

    try:
        effective = effective_thresholds(params)
    except Exception as exc:  # pragma: no cover - 防御性
        logger.warning("preprocess: 参数解析失败，跳过前处理: %s", exc)
        return PreprocessResult(status="unavailable", image=img, notes=f"参数解析失败: {exc}")

    if effective.get("mode") == MODE_OFF:
        return PreprocessResult(status="skipped", image=img, notes="mode=off")
    if not any_step_enabled(effective):
        return PreprocessResult(status="skipped", image=img, notes="未启用任何前处理步骤")

    result = PreprocessResult(status="no_change", image=img)
    notes: list = []
    try:
        work = _normalize_mode(img)
        original_mode = work.mode

        if effective.get("denoise.enabled"):
            work, changed = _denoise(work, float(effective["denoise.sigma"]))
            if changed:
                result.applied.append("denoise")

        if effective.get("deskew.enabled"):
            work, angle, score, changed, note = _deskew(
                work, float(effective["deskew.ang_tol"]), float(effective["deskew.min_score"])
            )
            result.angle = float(angle)
            result.score = float(score)
            if changed:
                result.applied.append("deskew")
            if note:
                notes.append(note)

        if effective.get("contrast.enabled"):
            work, changed = _contrast(work, float(effective["contrast.clip"]))
            if changed:
                result.applied.append("contrast")

        pad_ratio = float(effective.get("pad", 0.0) or 0.0)
        if pad_ratio > 0:
            work, changed = _pad(work, pad_ratio)
            if changed:
                result.applied.append("pad")

        if work.mode != original_mode:
            work = work.convert(original_mode)
        result.image = work
        result.status = "ok" if result.applied else "no_change"
    except Exception as exc:
        logger.warning("preprocess: 执行失败，返回原图: %s", exc)
        return PreprocessResult(status="unavailable", image=img, notes=f"执行失败: {exc}")

    result.notes = "; ".join(notes)
    return result


def preprocess_jpeg(
    data: bytes,
    params: Optional[Dict[str, Any]] = None,
    quality: int = 85,
) -> Tuple[bytes, PreprocessResult]:
    """对 JPEG 字节流做前处理，返回 (处理后的字节流, 结果)。

    解码失败、依赖缺失或未做任何改动时**原样返回入参字节流**，调用方无需
    区分这些情况，也不会因此丢图。
    """
    if not is_available() or not data:
        return data, PreprocessResult(
            status="unavailable", notes=unavailable_reason() or "空数据"
        )
    try:
        with Image.open(io.BytesIO(data)) as opened:
            opened.load()
            img = opened.convert("RGB") if opened.mode not in ("L", "RGB") else opened.copy()
    except Exception as exc:
        logger.debug("preprocess: JPEG 解码失败，跳过前处理: %s", exc)
        return data, PreprocessResult(status="unavailable", notes=f"解码失败: {exc}")

    result = preprocess_image(img, params)
    if result.status != "ok" or not result.applied or result.image is None:
        return data, result
    try:
        buf = io.BytesIO()
        result.image.save(buf, format="JPEG", quality=max(1, min(95, int(quality))), optimize=True)
        return buf.getvalue(), result
    except Exception as exc:
        logger.warning("preprocess: JPEG 重编码失败，返回原图: %s", exc)
        return data, PreprocessResult(status="unavailable", image=img, notes=f"重编码失败: {exc}")


def summarize(params: Dict[str, Any]) -> str:
    """把生效参数拼成一行日志文本。"""
    parts = [f"mode={params.get('mode', MODE_OFF)}"]
    for name in sorted(_SWITCHES):
        parts.append(f"{name}={bool(params.get(name))}")
    for name in sorted(_THRESHOLDS):
        parts.append(f"{name}={float(params.get(name, _THRESHOLDS[name][0])):.4g}")
    return " ".join(parts)
