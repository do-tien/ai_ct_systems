"""LC1-06 location QA: nearest detector candidate against the frozen target.

Offline, standard-library only. This module compares the real detector's
published result document with the frozen LC1-02 reference and reports whether
the nearest candidate centre lies within the reference nodule's radius. It does
not run inference, transform coordinates, or decide the overall step-zero gate;
its verdict is ``location_status`` only.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import sys
from pathlib import Path

from .inference_result import COORDINATE_SYSTEM, validate_success, write_result

SCHEMA_VERSION = "1"
SCOPE = "lc1-06-location-only"
REFERENCE_SCHEMA_VERSION = "1"

REASON_WITHIN = "WITHIN_RADIUS"
REASON_OUTSIDE = "OUTSIDE_RADIUS"
REASON_EMPTY = "NO_CANDIDATES"

IDENTITY_MISMATCH_MESSAGE = "result and reference series/frame identity do not match"

EXIT_PASSED = 0
EXIT_FAILED = 1
EXIT_INVALID = 2

CODE_INVALID_INPUT = "INVALID_INPUT"
CODE_IDENTITY_MISMATCH = "INPUT_IDENTITY_MISMATCH"
CODE_OUTPUT_COLLISION = "OUTPUT_COLLISION"
CODE_IO_ERROR = "IO_ERROR"

# Fixed, safe descriptions: never source paths, UIDs, coordinates or exception
# dumps (the exception text never reaches the evidence or stderr).
SAFE_MESSAGES = {
    CODE_INVALID_INPUT: "input document or argument is invalid",
    CODE_IDENTITY_MISMATCH: "result and reference identity do not match",
    CODE_OUTPUT_COLLISION: "refusing to write the comparison over a source document",
    CODE_IO_ERROR: "operational filesystem failure",
}


def _fail(message: str) -> ValueError:
    return ValueError(message)


def _finite(value: object, name: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise _fail(f"{name} must be a number")
    number = float(value)
    if number != number or number in (float("inf"), float("-inf")):
        raise _fail(f"{name} must be finite")
    return number


def _triple(value: object, name: str) -> list[float]:
    if not isinstance(value, (list, tuple)) or len(value) != 3:
        raise _fail(f"{name} must be three numbers")
    return [_finite(item, f"{name}[{index}]") for index, item in enumerate(value)]


def _nonempty_str(value: object, name: str) -> str:
    if not isinstance(value, str) or not value:
        raise _fail(f"{name} must be a non-empty string")
    return value


def _validate_reference(reference: dict) -> dict:
    """Validate the LC1-02 reference and return the frozen target plus identity."""
    if not isinstance(reference, dict):
        raise _fail("reference must be an object")
    if reference.get("schema_version") != REFERENCE_SCHEMA_VERSION:
        raise _fail("reference.schema_version must be the string '1'")
    if reference.get("coordinate_system") != COORDINATE_SYSTEM:
        raise _fail(f"reference.coordinate_system must be {COORDINATE_SYSTEM!r}")
    series_uid = _nonempty_str(reference.get("series_instance_uid"), "reference.series_instance_uid")
    frame_uid = _nonempty_str(reference.get("frame_of_reference_uid"), "reference.frame_of_reference_uid")
    target_id = _nonempty_str(reference.get("target_reference_id"), "reference.target_reference_id")

    nodules = reference.get("nodules")
    if not isinstance(nodules, list) or not nodules:
        raise _fail("reference.nodules must be a non-empty list")
    by_id: dict[str, dict] = {}
    for nodule in nodules:
        if not isinstance(nodule, dict):
            raise _fail("reference.nodules entries must be objects")
        nodule_id = _nonempty_str(nodule.get("id"), "reference nodule id")
        if nodule_id in by_id:
            raise _fail(f"reference nodule id {nodule_id!r} is duplicated")
        center = _triple(nodule.get("center_mm"), f"reference nodule {nodule_id} center_mm")
        diameter = _finite(nodule.get("diameter_mm"), f"reference nodule {nodule_id} diameter_mm")
        if diameter <= 0:
            raise _fail(f"reference nodule {nodule_id} diameter_mm must be positive")
        by_id[nodule_id] = {"id": nodule_id, "center_mm": center, "diameter_mm": diameter}

    if target_id not in by_id:
        raise _fail(f"reference target_reference_id {target_id!r} is not one of the nodules")
    return {
        "series_instance_uid": series_uid,
        "frame_of_reference_uid": frame_uid,
        "target": by_id[target_id],
    }


def _validate_candidate_thresholds(result: dict) -> None:
    """A candidate below the result's declared threshold is inconsistent evidence."""
    threshold = float(result["model"]["score_threshold"])
    for candidate in result["candidates"]:
        if float(candidate["score"]) < threshold:
            raise _fail("candidate score is below the result's declared score_threshold")


def compare_detection(result: dict, reference: dict) -> dict:
    """Compare the nearest candidate against the frozen target (LC1-06).

    Raises ``ValueError`` on invalid or incompatible inputs. The returned
    document is private QA evidence carrying ``location_status``; it is not an
    overall gate authorization.
    """
    validate_success(result)
    _validate_candidate_thresholds(result)
    validated_reference = _validate_reference(reference)

    if (
        result["series_instance_uid"] != validated_reference["series_instance_uid"]
        or result["frame_of_reference_uid"] != validated_reference["frame_of_reference_uid"]
    ):
        raise _fail(IDENTITY_MISMATCH_MESSAGE)

    target = validated_reference["target"]
    radius = target["diameter_mm"] / 2.0

    best_key: tuple[float, str] | None = None
    best_candidate: dict | None = None
    best_distance: float | None = None
    for candidate in result["candidates"]:
        distance = math.dist(candidate["center_mm"], target["center_mm"])
        if not math.isfinite(distance):
            raise _fail("computed distance is not finite")
        key = (distance, candidate["id"])
        if best_key is None or key < best_key:
            best_key = key
            best_candidate = candidate
            best_distance = distance

    if best_candidate is None:
        location_status = "failed"
        reason = REASON_EMPTY
    elif best_distance <= radius:
        location_status = "passed"
        reason = REASON_WITHIN
    else:
        location_status = "failed"
        reason = REASON_OUTSIDE

    return {
        "schema_version": SCHEMA_VERSION,
        "scope": SCOPE,
        "location_status": location_status,
        "reason": reason,
        "series_instance_uid": result["series_instance_uid"],
        "frame_of_reference_uid": result["frame_of_reference_uid"],
        "coordinate_system": COORDINATE_SYSTEM,
        "target": {
            "id": target["id"],
            "center_mm": list(target["center_mm"]),
            "diameter_mm": target["diameter_mm"],
            "radius_mm": radius,
        },
        "nearest_candidate": best_candidate,
        "distance_mm": best_distance,
        "model": result["model"],
        "timing_s": result["timing_s"],
        "resources": result["resources"],
    }


# --------------------------------------------------------------------------- #
# CLI
# --------------------------------------------------------------------------- #


def _prescan(argv: list[str], option: str) -> list[str]:
    values = []
    for index, token in enumerate(argv):
        if token == option and index + 1 < len(argv):
            values.append(argv[index + 1])
        elif token.startswith(f"{option}="):
            values.append(token.split("=", 1)[1])
    return values


def _resolve_single(argv: list[str], option: str) -> tuple[Path | None, str | None]:
    """Return (path, error); ambiguous or missing options are never guessed."""
    values = sorted(set(_prescan(argv, option)))
    if not values:
        return None, f"missing {option}"
    if len(values) > 1:
        return None, f"ambiguous {option}"
    return Path(values[0]), None


def _resolved(path: Path) -> Path:
    try:
        return path.resolve()
    except OSError:  # pragma: no cover - resolution failure
        return path


def _collides(out: Path | None, *sources: Path | None) -> bool:
    if out is None:
        return False
    resolved_out = _resolved(out)
    return any(source is not None and _resolved(source) == resolved_out for source in sources)


_KNOWN_OPTIONS = {"--result", "--reference", "--out", "--help", "-h"}


def _has_unknown_option(argv: list[str]) -> bool:
    """True when argv carries an option spelling argparse would reject.

    Used only on the parser-failure path: an unrecognised (e.g. abbreviated)
    option means the arguments cannot be trusted, so the dedicated output must
    not be touched.
    """
    for token in argv:
        if token == "--":
            continue
        if token.startswith("--"):
            if token.split("=", 1)[0] not in _KNOWN_OPTIONS:
                return True
        elif token.startswith("-") and token != "-":
            return True
    return False


def _clear_stale_output(out: Path | None) -> None:
    """Invalidate only the dedicated output; never a source document."""
    if out is None:
        return
    try:
        if out.is_file():
            out.unlink()
    except OSError:  # pragma: no cover - best effort
        pass


def _error_document(code: str) -> dict:
    return {
        "schema_version": SCHEMA_VERSION,
        "scope": SCOPE,
        "location_status": "invalid",
        "error": {"code": code, "message": SAFE_MESSAGES[code]},
    }


def _publish(out: Path | None, document: dict) -> bool:
    if out is None:
        return False
    try:
        write_result(document, out)
        return True
    except Exception:
        return False


def _publish_error(out: Path | None, code: str) -> bool:
    return _publish(out, _error_document(code))


def main(argv: list[str] | None = None) -> int:
    argv = list(sys.argv[1:] if argv is None else argv)
    result_path, _ = _resolve_single(argv, "--result")
    reference_path, _ = _resolve_single(argv, "--reference")
    out, out_error = _resolve_single(argv, "--out")

    parser = argparse.ArgumentParser(
        prog="nodule_ai.check_detection",
        description="Compare the nearest detector candidate against the frozen LC1-02 target (location QA only).",
        allow_abbrev=False,
    )
    parser.add_argument("--result", required=True, type=Path)
    parser.add_argument("--reference", required=True, type=Path)
    parser.add_argument("--out", required=True, type=Path)
    try:
        args = parser.parse_args(argv)
    except SystemExit as exc:
        code = int(exc.code or 0)
        if code == 0:
            return EXIT_PASSED
        # Only invalidate a stale pass when the output is unambiguously known
        # and cannot be a source: no abbreviated/unknown option, no resolved
        # collision. Otherwise leave every file untouched.
        if (
            out_error is not None
            or out is None
            or _has_unknown_option(argv)
            or _collides(out, result_path, reference_path)
        ):
            print(f"check_detection: {out_error or CODE_INVALID_INPUT}", file=sys.stderr)
            return EXIT_INVALID
        _clear_stale_output(out)
        _publish_error(out, CODE_INVALID_INPUT)
        return EXIT_INVALID

    if out_error is not None:
        print(f"check_detection: {out_error}", file=sys.stderr)
        return EXIT_INVALID

    # Safety: never write over a source document, and only then invalidate any
    # previous evidence at the dedicated output.
    if _collides(args.out, args.result, args.reference):
        print(f"check_detection: {CODE_OUTPUT_COLLISION}", file=sys.stderr)
        return EXIT_INVALID
    _clear_stale_output(args.out)

    try:
        result_bytes = args.result.read_bytes()
        reference_bytes = args.reference.read_bytes()
    except OSError:
        _publish_error(args.out, CODE_IO_ERROR)
        print(f"check_detection: {CODE_IO_ERROR}", file=sys.stderr)
        return EXIT_FAILED

    try:
        result = json.loads(result_bytes)
        reference = json.loads(reference_bytes)
    except (ValueError, UnicodeDecodeError):
        _publish_error(args.out, CODE_INVALID_INPUT)
        print(f"check_detection: {CODE_INVALID_INPUT}", file=sys.stderr)
        return EXIT_INVALID

    try:
        comparison = compare_detection(result, reference)
    except ValueError as exc:
        code = CODE_IDENTITY_MISMATCH if str(exc) == IDENTITY_MISMATCH_MESSAGE else CODE_INVALID_INPUT
        _publish_error(args.out, code)
        print(f"check_detection: {code}", file=sys.stderr)
        return EXIT_INVALID

    comparison["source_sha256"] = {
        "result": hashlib.sha256(result_bytes).hexdigest(),
        "reference": hashlib.sha256(reference_bytes).hexdigest(),
    }
    if not _publish(args.out, comparison):
        print(f"check_detection: {CODE_IO_ERROR}", file=sys.stderr)
        return EXIT_FAILED

    print(
        json.dumps(
            {
                "scope": SCOPE,
                "location_status": comparison["location_status"],
                "reason": comparison["reason"],
                "out": str(args.out),
            },
            sort_keys=True,
        )
    )
    return EXIT_PASSED if comparison["location_status"] == "passed" else EXIT_FAILED


if __name__ == "__main__":
    raise SystemExit(main())
