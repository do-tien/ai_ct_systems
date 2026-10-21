"""Run the pinned MONAI lung-nodule bundle on a single image (LC1-05).

Only the pinned configuration is used: the bundle is provisioned and hash-checked
by the LC1-03 verifier before inference, so this module never accepts an arbitrary
config, never auto-selects a device, never falls back to CPU and never fabricates
model output.
"""

from __future__ import annotations

import json
import re
import time
from pathlib import Path
from types import SimpleNamespace

# Configuration of the pinned bundle. The score threshold is a research
# configuration value, never a malignancy probability.
EXPECTED_BUNDLE_VERSION = "0.6.11"
CONFIG_PATH = "configs/inference.json"
METADATA_PATH = "configs/metadata.json"
CHECKPOINT_PATH = "models/model.pt"
DEFAULT_SCORE_THRESHOLD = 0.5

# The pinned config disables Spacingd because it expects pre-resampled LUNA16
# input. LC1-05 enables it for fresh DICOM-derived NIfTI; the resampled path is
# proved transform-only in tests before any real inference.
RESAMPLING_OVERRIDE_KEY = "preprocessing#transforms#4#_disabled_"

# The LC1-04 probe proved that the pinned `affine_lps_to_ras=true` yields LPS
# world coordinates for this reader/resampling path, so no further flip is made.
AFFINE_LPS_TO_RAS = True

_DEVICE_PATTERN = re.compile(r"^(cpu|cuda|cuda:[0-9]+)$")
_SELECTOR_PATTERN = re.compile(r"set_box_selector_parameters")


class BundleError(RuntimeError):
    """Bundle/inference failure with a stable code; the CLI maps these to exit 3."""

    def __init__(self, message: str, *, code: str) -> None:
        super().__init__(message)
        self.code = code


def engine_stub() -> SimpleNamespace:
    """Minimal engine proxy for MONAI's CheckpointLoader.

    MONAI's handler reads ``state.max_epochs`` and, when it is not ``None``,
    ``state.epoch``; both must exist for the load to complete.
    """
    return SimpleNamespace(state=SimpleNamespace(max_epochs=None, epoch=0))


def normalize_device(device: str) -> str:
    """Map ``cuda`` to ``cuda:0``; reject anything else (no auto-selection)."""
    if not isinstance(device, str) or not _DEVICE_PATTERN.match(device):
        raise BundleError(f"unsupported device {device!r}; use cpu, cuda or cuda:N", code="DEVICE_UNAVAILABLE")
    return "cuda:0" if device == "cuda" else device


def validate_device(device: str) -> str:
    """Validate an explicitly requested device; never fall back to CPU."""
    normalized = normalize_device(device)
    if not normalized.startswith("cuda"):
        return normalized
    import torch

    if not torch.cuda.is_available():
        raise BundleError(f"requested device {normalized} is not available", code="DEVICE_UNAVAILABLE")
    index = int(normalized.split(":")[1])
    if index >= torch.cuda.device_count():
        raise BundleError(f"requested device {normalized} does not exist", code="DEVICE_UNAVAILABLE")
    return normalized


def _validate_threshold(score_threshold: float) -> float:
    try:
        value = float(score_threshold)
    except (TypeError, ValueError) as exc:
        raise BundleError("score threshold must be a number in [0, 1]", code="BUNDLE_INVALID") from exc
    if not 0.0 <= value <= 1.0 or value != value:
        raise BundleError("score threshold must be a number in [0, 1]", code="BUNDLE_INVALID")
    return value


def check_bundle(bundle_dir: Path, *, require_weights: bool = True) -> dict:
    """Check the provisioned bundle's expected version and checkpoint presence."""
    bundle_dir = Path(bundle_dir)
    config = bundle_dir / CONFIG_PATH
    metadata_path = bundle_dir / METADATA_PATH
    if not config.is_file() or not metadata_path.is_file():
        raise BundleError("bundle configuration is missing", code="BUNDLE_INVALID")
    try:
        metadata = json.loads(metadata_path.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, OSError, UnicodeDecodeError) as exc:
        raise BundleError("bundle metadata is not valid JSON", code="BUNDLE_INVALID") from exc
    if not isinstance(metadata, dict):
        raise BundleError("bundle metadata is not an object", code="BUNDLE_INVALID")
    version = metadata.get("version")
    if version != EXPECTED_BUNDLE_VERSION:
        raise BundleError(f"unexpected bundle version {version!r}", code="BUNDLE_INVALID")
    if require_weights and not (bundle_dir / CHECKPOINT_PATH).is_file():
        raise BundleError("bundle checkpoint is missing", code="BUNDLE_INVALID")
    try:
        return json.loads(config.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, OSError, UnicodeDecodeError) as exc:
        raise BundleError("bundle configuration is not valid JSON", code="BUNDLE_INVALID") from exc


def _build_parser(bundle_dir: Path, device: str, score_threshold: float, *, resample: bool):
    import torch
    from monai.bundle import ConfigParser

    config = bundle_dir / CONFIG_PATH
    parser = ConfigParser()
    parser.read_config(str(config))
    parser["bundle_root"] = str(bundle_dir)
    parser["device"] = torch.device(device)
    # The bundle's fp16 autocast path yields NaN box extents on this detector
    # (pred_whd_axis is NaN or Inf); fp32 fits comfortably in GPU memory.
    parser["amp"] = False
    if resample:
        # Enable the existing Spacingd instead of flipping the reader branch.
        parser[RESAMPLING_OVERRIDE_KEY] = False
        # Set the selector floor from the CLI threshold so values below the
        # upstream 0.02 are not silently truncated; other selector parameters keep
        # their upstream values.
        raw_ops = json.loads(config.read_text(encoding="utf-8"))["detector_ops"]
        for index, entry in enumerate(raw_ops):
            if isinstance(entry, str) and _SELECTOR_PATTERN.search(entry):
                parser[f"detector_ops#{index}"] = re.sub(
                    r"score_thresh=[^,)]+", f"score_thresh={score_threshold}", entry
                )
    return parser


def load_bundle(
    bundle_dir: Path,
    device: str,
    *,
    score_threshold: float = DEFAULT_SCORE_THRESHOLD,
    resample: bool = True,
    weights_required: bool = True,
) -> tuple[object, object, float]:
    """Resolve the pinned configuration and strictly load its checkpoint.

    Returns ``(parser, network, checkpoint_load_seconds)``. No forward pass runs
    here. ``resample``/``score_threshold`` overrides are part of the inference
    path; the LC1-03 verifier keeps the untouched pinned configuration.
    """
    threshold = _validate_threshold(score_threshold)
    normalized = validate_device(device)
    check_bundle(bundle_dir, require_weights=weights_required)

    import torch

    parser = _build_parser(Path(bundle_dir), normalized, threshold, resample=resample)
    try:
        network = parser.get_parsed_content("network")
        loader = parser.get_parsed_content("checkpointloader")
    except Exception as exc:
        raise BundleError("cannot initialise the bundle network", code="BUNDLE_INVALID") from exc

    if not weights_required:
        return parser, network, 0.0

    loader.map_location = normalized
    started = time.perf_counter()
    try:
        loader(engine_stub())
    except torch.cuda.OutOfMemoryError as exc:
        raise BundleError("checkpoint load ran out of memory", code="MODEL_OOM") from exc
    except MemoryError as exc:
        raise BundleError("checkpoint load ran out of memory", code="MODEL_OOM") from exc
    except Exception as exc:
        raise BundleError("checkpoint load failed", code="MODEL_LOAD_FAILED") from exc
    return parser, network, time.perf_counter() - started


def _validate_prediction(prediction: object) -> dict:
    """Reject malformed detector output before any filtering happens."""
    import torch

    if not isinstance(prediction, dict):
        raise BundleError("detector output is not a dictionary", code="INVALID_MODEL_OUTPUT")
    box = prediction.get("box")
    scores = prediction.get("label_scores")
    labels = prediction.get("label")
    if not all(isinstance(value, torch.Tensor) for value in (box, scores, labels)):
        raise BundleError("detector output is missing tensors", code="INVALID_MODEL_OUTPUT")
    if box.dim() != 2 or box.shape[1] != 6:
        raise BundleError("detector boxes must be (N, 6)", code="INVALID_MODEL_OUTPUT")
    if scores.dim() != 1 or labels.dim() != 1:
        raise BundleError("detector scores/labels must be (N,)", code="INVALID_MODEL_OUTPUT")
    if not (box.shape[0] == scores.shape[0] == labels.shape[0]):
        raise BundleError("detector box/score/label counts differ", code="INVALID_MODEL_OUTPUT")
    if not bool(torch.isfinite(box).all()) or not bool(torch.isfinite(scores).all()):
        raise BundleError("detector output contains non-finite values", code="INVALID_MODEL_OUTPUT")
    if box.shape[0] and float(scores.min()) < 0.0:
        raise BundleError("detector scores must be within [0, 1]", code="INVALID_MODEL_OUTPUT")
    if box.shape[0] and float(scores.max()) > 1.0:
        raise BundleError("detector scores must be within [0, 1]", code="INVALID_MODEL_OUTPUT")
    if labels.numel() and not all(int(value) == 0 for value in labels.tolist()):
        raise BundleError("detector returned an unexpected class label", code="INVALID_MODEL_OUTPUT")
    return prediction


def _validate_postprocessed_box(box, expected_count: int) -> None:
    """Reject impossible world boxes before they reach candidate filtering."""
    import torch

    if not isinstance(box, torch.Tensor) or box.dim() != 2 or box.shape[1] != 6:
        raise BundleError("postprocessed boxes are malformed", code="INVALID_MODEL_OUTPUT")
    if int(box.shape[0]) != expected_count:
        raise BundleError("postprocessed box count differs from the scores", code="INVALID_MODEL_OUTPUT")
    if not bool(torch.isfinite(box).all()):
        raise BundleError("postprocessed boxes contain non-finite values", code="INVALID_MODEL_OUTPUT")
    if box.shape[0]:
        if not bool((box[:, 3:] > 0).all()):
            raise BundleError("postprocessed box extents must be positive", code="INVALID_MODEL_OUTPUT")


def predict_nifti(parser, network, nifti_path: Path) -> dict:
    """Run one image through preprocessing, the detector and postprocessing.

    Returns the postprocessed ``box`` (patient-space LPS ``cccwhd``),
    ``label_scores`` and ``label``. The runtime affine flag is not re-applied
    here: the LC1-04 probe proved the pinned flag already yields LPS.
    """
    import torch

    path = Path(nifti_path)
    if not path.is_file():
        raise BundleError("prepared NIfTI is missing", code="INFERENCE_FAILED")

    parser.get_parsed_content("detector_ops")  # target keys, selector, sliding window
    pre = parser.get_parsed_content("preprocessing")
    post = parser.get_parsed_content("postprocessing")
    inferer = parser.get_parsed_content("inferer")

    device = str(parser["device"])
    try:
        amp = bool(parser["amp"])
    except Exception:
        amp = False
    try:
        data = pre({"image": str(path)})
    except Exception as exc:
        raise BundleError("preprocessing failed", code="INFERENCE_FAILED") from exc
    image = data["image"]

    network.eval()
    # Mirror the bundle evaluator's prepare_batch: the network lives on
    # `device`, so the input must too (postprocessing keeps the CPU copy).
    model_input = image.to(device=device)
    try:
        with torch.inference_mode():
            if device.startswith("cuda") and amp:
                with torch.autocast("cuda"):
                    predictions = inferer([model_input], network)
            else:
                # CPU (and any non-AMP path) never enables CUDA autocast.
                predictions = inferer([model_input], network)
    except torch.cuda.OutOfMemoryError as exc:
        raise BundleError("inference ran out of memory", code="MODEL_OOM") from exc
    except MemoryError as exc:
        raise BundleError("inference ran out of memory", code="MODEL_OOM") from exc
    except BundleError:
        raise
    except Exception as exc:
        raise BundleError("inference failed", code="INFERENCE_FAILED") from exc

    if not isinstance(predictions, list) or len(predictions) != 1:
        raise BundleError("expected exactly one prediction", code="INVALID_MODEL_OUTPUT")
    prediction = _validate_prediction(predictions[0])

    try:
        postprocessed = post({"image": image, "box": prediction["box"], "label": prediction["label"]})
    except Exception as exc:
        raise BundleError("postprocessing failed", code="INFERENCE_FAILED") from exc
    box = postprocessed["box"]
    _validate_postprocessed_box(box, int(prediction["label_scores"].shape[0]))
    return {"box": box, "label_scores": prediction["label_scores"], "label": prediction["label"]}
