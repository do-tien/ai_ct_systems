"""Result/error contract for the LC1-05 inference CLI.

The wire schema is fixed by LC1-01 §6: no extra fields for revision, quality,
geometry evidence or telemetry are added here. Publication is atomic and a
failed write never leaves a truncated document behind.
"""

from __future__ import annotations

import json
import os
import re
from pathlib import Path
from typing import Sequence

SCHEMA_VERSION = "1"
COORDINATE_SYSTEM = "DICOM_LPS_mm"
TIMING_TOLERANCE_S = 1e-6

_UID_PATTERN = re.compile(r"^[0-9]+(?:\.[0-9]+)*$")
MAX_UID_LENGTH = 64
_MODEL_KEYS = ("bundle", "bundle_version", "monai_version", "torch_version")


def _fail(message: str) -> ValueError:
    return ValueError(message)


def _finite(value: object, name: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise _fail(f"{name} must be a number")
    number = float(value)
    if number != number or number in (float("inf"), float("-inf")):
        raise _fail(f"{name} must be finite")
    return number


def _triple(value: object, name: str, *, positive: bool) -> list[float]:
    if not isinstance(value, (list, tuple)) or len(value) != 3:
        raise _fail(f"{name} must be three numbers")
    numbers = [_finite(item, f"{name}[{index}]") for index, item in enumerate(value)]
    if positive and any(number <= 0 for number in numbers):
        raise _fail(f"{name} must be positive")
    return numbers


def _uid(value: object, name: str) -> str:
    if not isinstance(value, str) or not value or len(value) > MAX_UID_LENGTH or not _UID_PATTERN.match(value):
        raise _fail(f"{name} is not a valid DICOM UID")
    return value


def _threshold(value: object, name: str = "score_threshold") -> float:
    number = _finite(value, name)
    if not 0.0 <= number <= 1.0:
        raise _fail(f"{name} must be within [0, 1]")
    return number


def candidates_from_boxes(
    boxes_cccwhd: Sequence[Sequence[float]],
    scores: Sequence[float],
    threshold: float,
) -> list[dict]:
    """Validate every prediction, then retain those scoring at/above threshold.

    Boxes are already patient-space LPS ``cccwhd`` (center + extent); no
    coordinate conversion happens here. Validation covers all rows, including
    rows whose score is below the threshold, and never repairs or clamps values.
    """
    threshold = _threshold(threshold)
    if not isinstance(boxes_cccwhd, (list, tuple)) or not isinstance(scores, (list, tuple)):
        raise _fail("boxes and scores must be sequences")
    if len(boxes_cccwhd) != len(scores):
        raise _fail("boxes and scores must have the same length")

    validated = []
    for index, (box, score) in enumerate(zip(boxes_cccwhd, scores)):
        if not isinstance(box, (list, tuple)) or len(box) != 6:
            raise _fail(f"boxes[{index}] must have six values")
        center = _triple(box[:3], f"boxes[{index}].center", positive=False)
        extent = _triple(box[3:], f"boxes[{index}].extent", positive=True)
        validated.append((center, extent, _threshold(score, f"scores[{index}]")))

    candidates = []
    for center, extent, score in validated:
        if score >= threshold:
            candidates.append(
                {
                    "id": f"c{len(candidates) + 1}",
                    "center_mm": [float(v) for v in center],
                    "size_mm": [float(v) for v in extent],
                    "score": float(score),
                }
            )
    return candidates


def validate_success(result: dict) -> None:
    """Reject a malformed settled success document before it is published."""
    if not isinstance(result, dict):
        raise _fail("result must be an object")
    if result.get("schema_version") != SCHEMA_VERSION:
        raise _fail("schema_version must be the string '1'")
    _uid(result.get("series_instance_uid"), "series_instance_uid")
    _uid(result.get("frame_of_reference_uid"), "frame_of_reference_uid")
    if result.get("coordinate_system") != COORDINATE_SYSTEM:
        raise _fail(f"coordinate_system must be {COORDINATE_SYSTEM!r}")

    model = result.get("model")
    if not isinstance(model, dict):
        raise _fail("model must be an object")
    for key in _MODEL_KEYS:
        if not isinstance(model.get(key), str) or not model[key]:
            raise _fail(f"model.{key} is required")
    if not isinstance(model.get("affine_lps_to_ras"), bool):
        raise _fail("model.affine_lps_to_ras must be a boolean")
    _threshold(model.get("score_threshold"), "model.score_threshold")

    data_input = result.get("input")
    if not isinstance(data_input, dict):
        raise _fail("input must be an object")
    num_slices = data_input.get("num_slices")
    if isinstance(num_slices, bool) or not isinstance(num_slices, int) or num_slices <= 0:
        raise _fail("input.num_slices must be a positive integer")
    _triple(data_input.get("spacing_mm"), "input.spacing_mm", positive=True)
    size = _triple(data_input.get("size_voxels"), "input.size_voxels", positive=False)
    if any(value <= 0 for value in size):
        raise _fail("input.size_voxels must be positive")

    candidates = result.get("candidates")
    if not isinstance(candidates, list):
        raise _fail("candidates must be a list")
    seen = set()
    for candidate in candidates:
        if not isinstance(candidate, dict):
            raise _fail("candidate must be an object")
        candidate_id = candidate.get("id")
        if not isinstance(candidate_id, str) or not candidate_id:
            raise _fail("candidate.id is required")
        if candidate_id in seen:
            raise _fail("candidate.id must be unique")
        seen.add(candidate_id)
        _triple(candidate.get("center_mm"), "candidate.center_mm", positive=False)
        _triple(candidate.get("size_mm"), "candidate.size_mm", positive=True)
        _threshold(candidate.get("score"), "candidate.score")

    timing = result.get("timing_s")
    if not isinstance(timing, dict):
        raise _fail("timing_s must be an object")
    load = _finite(timing.get("load"), "timing_s.load")
    inference = _finite(timing.get("inference"), "timing_s.inference")
    total = _finite(timing.get("total"), "timing_s.total")
    if min(load, inference, total) < 0:
        raise _fail("timing_s values must be non-negative")
    if total + TIMING_TOLERANCE_S < load + inference:
        raise _fail("timing_s.total must cover load plus inference")

    resources = result.get("resources")
    if not isinstance(resources, dict):
        raise _fail("resources must be an object")
    if not isinstance(resources.get("device"), str) or not resources["device"]:
        raise _fail("resources.device is required")
    if _finite(resources.get("peak_gpu_mem_mb"), "resources.peak_gpu_mem_mb") < 0:
        raise _fail("resources.peak_gpu_mem_mb must be non-negative")
    if _finite(resources.get("peak_rss_mb"), "resources.peak_rss_mb") < 0:
        raise _fail("resources.peak_rss_mb must be non-negative")


def error_result(code: str, message: str) -> dict:
    """The fixed failure envelope: schema_version plus an error object."""
    return {"schema_version": SCHEMA_VERSION, "error": {"code": str(code), "message": str(message)}}


def write_result(result: dict, out: Path) -> None:
    """Serialise strictly and atomically; clean only this call's own temporary."""
    out = Path(out)
    out.parent.mkdir(parents=True, exist_ok=True)
    payload = json.dumps(result, indent=2, sort_keys=True, allow_nan=False) + "\n"
    temporary = out.with_name(f".{out.name}.{os.getpid()}.tmp")
    try:
        temporary.write_text(payload, encoding="utf-8")
        os.replace(temporary, out)
    except Exception:
        if temporary.exists():
            temporary.unlink()
        raise
