"""LC1-05 inference CLI: run the pinned bundle on one validated CT series.

The CLI owns the temporary workspace, error mapping, timing and resource
measurement. It never writes the result to stdout, never falls back to another
device and never leaves a previous success file behind after a failed attempt.
"""

from __future__ import annotations

import argparse
import importlib.metadata
import os
import re
import sys
import tempfile
import threading
import time
from pathlib import Path

from .inference_result import candidates_from_boxes, error_result, validate_success, write_result

DEFAULT_SCORE_THRESHOLD = 0.5
DEFAULT_DEVICE = "cuda"
RSS_SAMPLE_INTERVAL_S = 0.01
_DEVICE_SYNTAX = re.compile(r"^(cpu|cuda|cuda:[0-9]+)$")

# Fixed, safe failure descriptions: never source paths, UIDs, DICOM tags or
# exception dumps (the exception text goes to stderr as a short code only).
SAFE_MESSAGES = {
    "INVALID_ARGUMENT": "invalid command line argument",
    "INVALID_OUTPUT_PATH": "refusing to write the result over input or bundle data",
    "TOOL_ERROR": "operational dependency or filesystem failure",
    "IO_ERROR": "filesystem failure while writing the result",
    "RESOURCE_MEASUREMENT_FAILED": "resource measurement failed",
    "INTERNAL_ERROR": "unexpected internal failure",
    "DEVICE_UNAVAILABLE": "requested device is not available",
    "BUNDLE_INVALID": "bundle configuration is invalid or incomplete",
    "MODEL_LOAD_FAILED": "model checkpoint could not be loaded",
    "MODEL_OOM": "model ran out of memory",
    "INFERENCE_FAILED": "inference failed",
    "INVALID_MODEL_OUTPUT": "model returned malformed output",
    "INVALID_DICOM": "series is not readable DICOM",
    "NOT_CT": "series is not a CT series",
    "UNSUPPORTED_CT": "series is not a supported single-frame CT series",
    "INVALID_UID": "series contains an invalid UID",
    "UID_MISMATCH": "series UIDs are inconsistent",
    "DUPLICATE_SOP": "series contains duplicate instances",
    "INSUFFICIENT_SLICES": "series has too few slices",
    "INVALID_GEOMETRY": "series geometry is invalid",
    "DUPLICATE_POSITION": "series contains duplicate slice positions",
    "NONUNIFORM_SPACING": "slice spacing is not uniform",
    "SLICE_TOO_THICK": "slice thickness exceeds the accepted limit",
    "SLICE_SPACING_TOO_LARGE": "slice spacing exceeds the accepted limit",
    "UNSUPPORTED_GANTRY_TILT": "series has gantry tilt or a moving origin",
    "INCOMPLETE_SERIES": "series is incomplete compared with its inventory",
    "PIXEL_DECODE_FAILED": "pixel data could not be decoded",
    "INVALID_PIXEL_DATA": "decoded pixels are not a usable scalar volume",
    "GEOMETRY_MISMATCH": "decoded geometry does not match the source metadata",
    "NIFTI_AFFINE_MISMATCH": "converted volume affine is inconsistent",
    "NIFTI_WRITE_FAILED": "temporary volume could not be written",
    "LANDMARK_OUT_OF_BOUNDS": "reference landmark is outside the volume",
    "INVALID_REFERENCE": "reference data is invalid",
}

EXIT_INPUT = 2
EXIT_MODEL = 3
EXIT_OPERATIONAL = 1


class MeasurementError(RuntimeError):
    """Resource measurement was impossible; the CLI must fail, not substitute."""


class PeakRssSampler:
    """Sampled peak process RSS.

    This is a sampled maximum, not an exact OS high-water mark. The background
    thread is always stopped and joined, including when the body raises.
    """

    def __init__(self, interval: float = RSS_SAMPLE_INTERVAL_S, rss_reader=None) -> None:
        self._interval = interval
        self._reader = rss_reader or _default_rss_reader
        self._peak_bytes = 0
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None

    def _sample(self) -> None:
        try:
            value = int(self._reader())
        except Exception:
            # Measurement unavailable (e.g. psutil missing). The run is failed
            # later with RESOURCE_MEASUREMENT_FAILED rather than reporting a
            # placeholder value.
            return
        if value > self._peak_bytes:
            self._peak_bytes = value

    def _loop(self) -> None:
        while not self._stop.wait(self._interval):
            self._sample()

    @property
    def peak_bytes(self) -> int:
        return self._peak_bytes

    @property
    def peak_mb(self) -> float:
        return self._peak_bytes / (1024**2)

    def __enter__(self) -> "PeakRssSampler":
        self._sample()
        self._thread = threading.Thread(target=self._loop, name="peak-rss-sampler", daemon=True)
        self._thread.start()
        return self

    def __exit__(self, exc_type, exc, tb) -> bool:
        self._stop.set()
        if self._thread is not None:
            self._thread.join(timeout=5.0)
        self._sample()
        return False


def _default_rss_reader() -> int:
    import psutil

    return int(psutil.Process().memory_info().rss)


def _xz_spacing(metadata: dict) -> list[float]:
    """Original XYZ spacing (x = column spacing), not the resampled model grid."""
    pixel_spacing = metadata["pixel_spacing_mm"]
    return [float(pixel_spacing[1]), float(pixel_spacing[0]), float(metadata["median_slice_spacing_mm"])]


def _xz_size(metadata: dict) -> list[int]:
    return [int(metadata["columns"]), int(metadata["rows"]), int(metadata["num_slices"])]


def _validate_threshold(value: float) -> float:
    try:
        threshold = float(value)
    except (TypeError, ValueError):
        raise ValueError("score threshold must be a number in [0, 1]") from None
    if threshold != threshold or threshold in (float("inf"), float("-inf")) or not 0.0 <= threshold <= 1.0:
        raise ValueError("score threshold must be a number in [0, 1]")
    return threshold


def run_inference(series_dir: Path, bundle_dir: Path, *, score_threshold: float, device: str) -> dict:
    """Run the full pipeline and return a validated success document.

    Raises GeometryError for unsuitable series, BundleError for bundle/device/
    model failures and ToolError/OSError for operational failures.
    """
    from .bundle_runtime import (
        AFFINE_LPS_TO_RAS,
        EXPECTED_BUNDLE_VERSION,
        BundleError,
        load_bundle,
        normalize_device,
        predict_nifti,
    )
    from .volume import read_ct_volume, write_nifti

    threshold = _validate_threshold(score_threshold)
    normalized = normalize_device(device)
    series_dir = Path(series_dir)
    total_started = time.perf_counter()

    with PeakRssSampler() as sampler:
        with tempfile.TemporaryDirectory(prefix="nodule-ai-job-") as workspace:
            load_started = time.perf_counter()
            # Geometry first: unsuitable input is rejected before torch exists.
            image, metadata = read_ct_volume(series_dir)
            nifti_path = write_nifti(image, Path(workspace) / "volume.nii.gz")
            parser, network, _checkpoint_seconds = load_bundle(
                Path(bundle_dir), normalized, score_threshold=threshold
            )
            load_seconds = time.perf_counter() - load_started

            import torch

            gpu_peak_mb = 0.0
            if normalized.startswith("cuda"):
                torch.cuda.reset_peak_memory_stats(normalized)
            inference_started = time.perf_counter()
            prediction = predict_nifti(parser, network, nifti_path)
            if normalized.startswith("cuda"):
                torch.cuda.synchronize(normalized)
                gpu_peak_mb = float(torch.cuda.max_memory_allocated(normalized)) / (1024**2)
            inference_seconds = time.perf_counter() - inference_started

    if sampler.peak_bytes <= 0:
        raise MeasurementError("peak RSS could not be measured")

    candidates = candidates_from_boxes(
        prediction["box"].tolist(), prediction["label_scores"].tolist(), threshold
    )
    result = {
        "schema_version": "1",
        "series_instance_uid": metadata["series_instance_uid"],
        "frame_of_reference_uid": metadata["frame_of_reference_uid"],
        "coordinate_system": "DICOM_LPS_mm",
        "model": {
            "bundle": "lung_nodule_ct_detection",
            "bundle_version": EXPECTED_BUNDLE_VERSION,
            "monai_version": importlib.metadata.version("monai"),
            "torch_version": importlib.metadata.version("torch"),
            "affine_lps_to_ras": AFFINE_LPS_TO_RAS,
            "score_threshold": threshold,
        },
        "input": {
            "num_slices": int(metadata["num_slices"]),
            "spacing_mm": _xz_spacing(metadata),
            "size_voxels": _xz_size(metadata),
        },
        "candidates": candidates,
        "timing_s": {
            "load": load_seconds,
            "inference": inference_seconds,
            "total": 0.0,
        },
        "resources": {
            "device": normalized,
            "peak_gpu_mem_mb": gpu_peak_mb,
            "peak_rss_mb": sampler.peak_mb,
        },
    }
    result["timing_s"]["total"] = time.perf_counter() - total_started
    validate_success(result)
    return result


def _safe_message(code: str) -> str:
    return SAFE_MESSAGES.get(code, "inference failed")


def _prescan(argv: list[str], option: str) -> list[str]:
    values = []
    for index, token in enumerate(argv):
        if token == option and index + 1 < len(argv):
            values.append(argv[index + 1])
        elif token.startswith(f"{option}="):
            values.append(token.split("=", 1)[1])
    return values


def _resolve_output(argv: list[str]) -> tuple[Path | None, str | None]:
    """Return (unique out, error). Ambiguous or missing output is never guessed."""
    values = _prescan(argv, "--out")
    unique = sorted(set(values))
    if not unique:
        return None, "missing --out"
    if len(unique) > 1:
        return None, "ambiguous --out"
    return Path(unique[0]), None


def _is_inside(path: Path, directory: Path | None) -> bool:
    if directory is None:
        return False
    try:
        return path.resolve().is_relative_to(directory.resolve())
    except OSError:  # pragma: no cover - resolution failure
        return False


def _clear_stale_output(out: Path | None, series_dir: Path | None, bundle_dir: Path | None) -> None:
    """Invalidate only the dedicated output path, and never a source file."""
    if out is None:
        return
    if _is_inside(out, series_dir) or _is_inside(out, bundle_dir):
        return
    try:
        if out.is_file():
            out.unlink()
    except OSError:  # pragma: no cover - best effort
        pass


def _write_error(out: Path | None, code: str, message: str) -> bool:
    if out is None:
        return False
    try:
        write_result(error_result(code, message), out)
        return True
    except Exception:
        return False


def main(argv: list[str] | None = None) -> int:
    argv = list(sys.argv[1:] if argv is None else argv)

    series_values = _prescan(argv, "--series-dir")
    bundle_values = _prescan(argv, "--bundle-dir")
    series_dir = Path(series_values[-1]) if series_values else None
    bundle_dir = Path(bundle_values[-1]) if bundle_values else None
    out, out_error = _resolve_output(argv)

    # Safety first: never publish anything inside input/bundle data, even when
    # other arguments are invalid (an argparse failure must not be able to write
    # an error document over a source file).
    if out is not None and (_is_inside(out, series_dir) or _is_inside(out, bundle_dir)):
        print("infer: INVALID_OUTPUT_PATH", file=sys.stderr)
        return EXIT_OPERATIONAL

    # A parser failure must not leave a previous success at the dedicated --out.
    _clear_stale_output(out, series_dir, bundle_dir)

    parser = argparse.ArgumentParser(
        prog="nodule_ai.infer",
        description="Run the pinned lung-nodule detector on one validated CT series (LC1-05).",
    )
    parser.add_argument("--series-dir", required=True, type=Path)
    parser.add_argument("--out", required=True, type=Path)
    parser.add_argument("--bundle-dir", required=True, type=Path)
    parser.add_argument("--score-threshold", default=DEFAULT_SCORE_THRESHOLD, type=float)
    parser.add_argument("--device", default=DEFAULT_DEVICE)
    try:
        args = parser.parse_args(argv)
    except SystemExit as exc:
        code = int(exc.code or 0)
        if code == 0:
            return 0
        if out_error is None:
            _write_error(out, "INVALID_ARGUMENT", _safe_message("INVALID_ARGUMENT"))
        else:
            print(f"infer: {out_error}", file=sys.stderr)
        return EXIT_INPUT

    if out_error is not None:
        print(f"infer: {out_error}", file=sys.stderr)
        return EXIT_INPUT

    try:
        threshold = _validate_threshold(args.score_threshold)
    except ValueError:
        _write_error(out, "INVALID_ARGUMENT", _safe_message("INVALID_ARGUMENT"))
        print("infer: INVALID_ARGUMENT", file=sys.stderr)
        return EXIT_INPUT
    if not isinstance(args.device, str) or not _DEVICE_SYNTAX.match(args.device):
        _write_error(out, "INVALID_ARGUMENT", _safe_message("INVALID_ARGUMENT"))
        print("infer: INVALID_ARGUMENT", file=sys.stderr)
        return EXIT_INPUT

    if _is_inside(args.out, args.series_dir) or _is_inside(args.out, args.bundle_dir):
        print("infer: INVALID_OUTPUT_PATH", file=sys.stderr)
        return EXIT_OPERATIONAL
    if args.out.exists() and args.out.is_file() and _is_inside(args.out, args.series_dir):
        print("infer: INVALID_OUTPUT_PATH", file=sys.stderr)
        return EXIT_OPERATIONAL

    # Invalidate the dedicated output before the expensive work starts so a
    # termination cannot leave a previous success behind.
    _clear_stale_output(args.out, args.series_dir, args.bundle_dir)

    try:
        result = run_inference(args.series_dir, args.bundle_dir, score_threshold=threshold, device=args.device)
    except Exception as exc:  # mapped below; never a raw exception dump in the file
        code, exit_code = _map_error(exc)
        published = _write_error(out, code, _safe_message(code))
        print(f"infer: {code}", file=sys.stderr)
        return exit_code if published else max(exit_code, EXIT_OPERATIONAL)

    try:
        write_result(result, out)
    except OSError:
        print("infer: IO_ERROR", file=sys.stderr)
        return EXIT_OPERATIONAL
    except Exception:
        print("infer: INTERNAL_ERROR", file=sys.stderr)
        return EXIT_OPERATIONAL
    return 0


def _map_error(exc: Exception) -> tuple[str, int]:
    from .bundle_runtime import BundleError
    from .errors import GeometryError

    if isinstance(exc, MeasurementError):
        return "RESOURCE_MEASUREMENT_FAILED", EXIT_OPERATIONAL
    if isinstance(exc, ValueError):
        # Malformed prediction/result values: a model-output failure, not a
        # generic internal error.
        return "INVALID_MODEL_OUTPUT", EXIT_MODEL

    if isinstance(exc, GeometryError):
        code = exc.code
        if code in SAFE_MESSAGES:
            return code, EXIT_INPUT
        return "GEOMETRY_MISMATCH", EXIT_INPUT
    if isinstance(exc, BundleError):
        return exc.code if exc.code in SAFE_MESSAGES else "INFERENCE_FAILED", EXIT_MODEL
    if isinstance(exc, MemoryError):
        return "MODEL_OOM", EXIT_MODEL
    try:
        import torch

        if isinstance(exc, torch.cuda.OutOfMemoryError):
            return "MODEL_OOM", EXIT_MODEL
    except Exception:  # pragma: no cover - torch absent
        pass
    from .errors import ToolError

    if isinstance(exc, (ToolError, ImportError)):
        # Operational dependency failures (including a missing torch in the
        # chosen interpreter) are exit 1, never a model error.
        return "TOOL_ERROR", EXIT_OPERATIONAL
    if isinstance(exc, OSError):
        return "IO_ERROR", EXIT_OPERATIONAL
    return "INTERNAL_ERROR", EXIT_OPERATIONAL


if __name__ == "__main__":
    raise SystemExit(main())
