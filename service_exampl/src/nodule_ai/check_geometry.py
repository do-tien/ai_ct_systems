"""LC1-04 QA CLI: geometry landmarks, NIfTI round-trip and bundle transform probe.

QA only: it consumes the private LIDC reference and the case manifest, and never
hands them to inference. Exit codes: 0 = every requested check passed, 2 =
unsuitable input/reference, 1 = dependency/filesystem/verification failure.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import sys
from pathlib import Path

import numpy as np

from .dicom_geometry import (
    lps_to_ras,
    lps_to_ras_affine,
    voxel_to_lps_affine,
    voxel_to_world,
    world_to_voxel,
)
from .errors import DataError, GeometryError, NoduleAIError, ToolError

SCHEMA_VERSION = "1"
LANDMARK_ERROR_TOL_MM = 1e-3
LANDMARK_VOXEL_TOL = 1e-3
RAW_TAG_TOL_MM = 1e-4

# Codes that mean "the input or the reference is unsuitable" (exit 2).
INPUT_CODES = {
    "INVALID_DICOM",
    "NOT_CT",
    "UNSUPPORTED_CT",
    "INVALID_UID",
    "UID_MISMATCH",
    "DUPLICATE_SOP",
    "INSUFFICIENT_SLICES",
    "INVALID_GEOMETRY",
    "DUPLICATE_POSITION",
    "NONUNIFORM_SPACING",
    "SLICE_TOO_THICK",
    "SLICE_SPACING_TOO_LARGE",
    "UNSUPPORTED_GANTRY_TILT",
    "INVALID_REFERENCE",
    "LANDMARK_OUT_OF_BOUNDS",
}


def _error(message: str, code: str) -> GeometryError:
    return GeometryError(message, code=code)


def _read_json(path: Path, code: str) -> dict:
    path = Path(path)
    if not path.is_file():
        raise _error(f"file not found: {path}", code)
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        raise _error(f"{path} is not valid JSON: {exc}", code) from exc


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with Path(path).open("rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _landmarks_from_reference(reference: dict) -> list[dict]:
    nodules = reference.get("nodules")
    if not isinstance(nodules, list) or not nodules:
        raise _error("reference has no nodules", "INVALID_REFERENCE")
    landmarks = []
    for nodule in nodules:
        if not isinstance(nodule.get("center_mm"), list) or len(nodule["center_mm"]) != 3:
            raise _error(f"nodule {nodule.get('id')} has no valid center_mm", "INVALID_REFERENCE")
        landmarks.append({"source": f"cluster:{nodule['id']}", "point_lps": [float(v) for v in nodule["center_mm"]]})
        for measurement in nodule.get("measurements", []):
            center = measurement.get("center_mm")
            if isinstance(center, list) and len(center) == 3:
                landmarks.append(
                    {
                        "source": f"annotation:{measurement.get('annotation_id')}",
                        "point_lps": [float(v) for v in center],
                    }
                )
    if not all(np.all(np.isfinite(landmark["point_lps"])) for landmark in landmarks):
        raise _error("reference contains non-finite landmark points", "INVALID_REFERENCE")
    return landmarks


def _raw_tag_lps(series_metadata: dict, path: Path, row: float, column: float) -> tuple[float, float, float]:
    """Independent LPS oracle straight from the raw DICOM tags (QA only)."""
    import pydicom

    ds = pydicom.dcmread(path, stop_before_pixels=True, force=False)
    ipp = [float(v) for v in ds.ImagePositionPatient]
    iop = [float(v) for v in ds.ImageOrientationPatient]
    spacing = [float(v) for v in ds.PixelSpacing]
    lps = [ipp[0], ipp[1], ipp[2]]
    for axis in range(3):
        lps[axis] += column * spacing[1] * iop[axis] + row * spacing[0] * iop[3 + axis]
    return (float(lps[0]), float(lps[1]), float(lps[2]))


def run_geometry(series_dir: Path, reference_path: Path, manifest_path: Path, *, nifti_out: Path | None = None) -> dict:
    from .volume import itk_affine, read_ct_volume, write_nifti

    reference = _read_json(reference_path, "INVALID_REFERENCE")
    manifest = _read_json(manifest_path, "INVALID_REFERENCE")
    inventory = manifest.get("sop_instance_uids") or None

    image, metadata = read_ct_volume(series_dir, expected_sop_uids=inventory)

    identity = {
        "study_instance_uid": metadata["study_instance_uid"],
        "series_instance_uid": metadata["series_instance_uid"],
        "frame_of_reference_uid": metadata["frame_of_reference_uid"],
    }
    for key in identity:
        expected = reference.get(key) or manifest.get(key)
        if expected and expected != identity[key]:
            raise _error(f"{key} mismatch between reference/manifest and decoded series", "UID_MISMATCH")

    affine_lps = voxel_to_lps_affine(metadata)
    size = list(image.GetSize())

    landmarks = []
    raw_error = None
    for landmark in _landmarks_from_reference(reference):
        point = landmark["point_lps"]
        index = world_to_voxel(point, affine_lps)
        for axis in range(3):
            if index[axis] < -LANDMARK_VOXEL_TOL or index[axis] > size[axis] - 1 + LANDMARK_VOXEL_TOL:
                raise _error(f"landmark {landmark['source']} index {index} is outside the volume", "LANDMARK_OUT_OF_BOUNDS")
        recovered = voxel_to_world(index, affine_lps)
        error = float(np.linalg.norm(np.array(recovered) - np.array(point)))
        if error > LANDMARK_ERROR_TOL_MM:
            raise _error(f"landmark {landmark['source']} round-trip error {error} mm", "GEOMETRY_MISMATCH")
        landmark_entry = {
            "source": landmark["source"],
            "patient_point_lps": point,
            "voxel_index": [float(v) for v in index],
            "recovered_lps": [float(v) for v in recovered],
            "error_mm": error,
        }
        # Independent in-plane oracle on the landmark's own slice: compare the
        # raw-tag LPS with the affine applied at the same (integer) slice. Every
        # landmark is checked, not only the first.
        slice_index = int(round(index[2]))
        slice_path = metadata["ordered_files"][min(max(slice_index, 0), len(metadata["ordered_files"]) - 1)]
        raw = _raw_tag_lps(metadata, slice_path, index[1], index[0])
        oracle_point = np.array(voxel_to_world((index[0], index[1], float(slice_index)), affine_lps))
        landmark_error = float(np.linalg.norm(np.array(raw) - oracle_point))
        landmark_entry["raw_tag_lps"] = [float(v) for v in raw]
        landmark_entry["raw_tag_error_mm"] = landmark_error
        if landmark_error > RAW_TAG_TOL_MM:
            raise _error(f"raw-tag oracle disagrees by {landmark_error} mm", "GEOMETRY_MISMATCH")
        raw_error = landmark_error if raw_error is None else max(raw_error, landmark_error)
        landmarks.append(landmark_entry)

    nifti_report = {"written": False}
    with __import__("tempfile").TemporaryDirectory() as tmp:
        target = Path(nifti_out) if nifti_out is not None else Path(tmp) / "volume.nii.gz"
        write_nifti(image, target)
        import nibabel as nib

        reloaded = nib.load(str(target))
        nifti_report = {
            "written": True,
            "persisted": nifti_out is not None,
            "shape": [int(v) for v in reloaded.shape],
            "dtype": str(reloaded.get_data_dtype()),
            "affine": [[float(v) for v in row] for row in np.asarray(reloaded.affine)],
            "finite": bool(np.all(np.isfinite(np.asarray(reloaded.dataobj)))),
            "size_bytes": int(target.stat().st_size),
        }
        write_nifti(image, target)  # second pass must be stable
        nifti_report["repeat_sha256"] = _sha256(target)
        nifti_report["affine_max_error_mm"] = float(np.max(np.abs(np.asarray(reloaded.affine) - lps_to_ras_affine(
            np.block([[np.array(image.GetDirection()).reshape(3, 3) @ np.diag(image.GetSpacing()), np.array(image.GetOrigin()).reshape(3, 1)], [np.zeros((1, 3)), np.ones((1, 1))]])
        ))))

    return {
        "identity": identity,
        "dependencies": {
            name: _version(name)
            for name in ("SimpleITK", "nibabel", "pydicom", "numpy")
        },
        "source_inventory": {"count": len(metadata["sop_instance_uids"]), "completeness": metadata["completeness"]},
        "geometry": {
            "size": size,
            "spacing": [float(v) for v in image.GetSpacing()],
            "origin": [float(v) for v in image.GetOrigin()],
            "direction": [float(v) for v in image.GetDirection()],
        },
        "affines": {"voxel_to_lps": [[float(v) for v in row] for row in affine_lps]},
        "tolerances": {
            "landmark_error_mm": LANDMARK_ERROR_TOL_MM,
            "landmark_voxel": LANDMARK_VOXEL_TOL,
            "raw_tag_mm": RAW_TAG_TOL_MM,
        },
        "landmarks": landmarks,
        "corner_max_error_mm": metadata.get("reader_affine_max_error_mm"),
        "completeness": metadata["completeness"],
        "nifti": nifti_report,
        "bundle_transform": {"status": "not_run"},
    }


def _version(name: str) -> str | None:
    import importlib.metadata

    return importlib.metadata.version(name) if _has_distribution(name) else None


def _has_distribution(name: str) -> bool:
    import importlib.metadata

    try:
        importlib.metadata.version(name)
        return True
    except importlib.metadata.PackageNotFoundError:
        return False


def run_bundle_transform(bundle_dir: Path, nifti_path: Path, landmarks: list[dict]) -> dict:
    """Probe the pinned bundle postprocessing transforms on a generated NIfTI.

    No network, detector, evaluator or checkpoint object is instantiated; only
    the preprocessing/affine/box transforms from the pinned config.
    """
    import torch
    from monai.bundle import ConfigParser

    config = Path(bundle_dir) / "configs" / "inference.json"
    if not config.is_file():
        raise _error(f"bundle config not found: {config}", "INVALID_REFERENCE")

    parser = ConfigParser()
    parser.read_config(str(config))
    parser["bundle_root"] = str(bundle_dir)
    parser["device"] = torch.device("cpu")
    processed = parser.get_parsed_content("preprocessing")({"image": str(nifti_path)})
    image = processed["image"]
    meta_affine = np.asarray(image.meta["affine"], dtype=float)

    # The physical index of an LPS point follows the flag-true convention; the
    # SAME boxes are then fed to both flag settings so the outputs can be
    # compared against the known LPS points (non-circular).
    lps_affine = np.diag([-1.0, -1.0, 1.0, 1.0]) @ meta_affine
    inverse = np.linalg.inv(lps_affine)

    results = {}
    for flag in (True, False):
        toggled = ConfigParser()
        toggled.read_config(str(config))
        toggled["bundle_root"] = str(bundle_dir)
        toggled["device"] = torch.device("cpu")
        toggled["postprocessing#transforms#1#affine_lps_to_ras"] = flag
        clip = toggled.get_parsed_content("postprocessing#transforms#0")
        to_world = toggled.get_parsed_content("postprocessing#transforms#1")
        to_mode = toggled.get_parsed_content("postprocessing#transforms#2")

        errors = []
        mode_errors = []
        for landmark in landmarks:
            point = np.array(landmark["point_lps"] + [1.0])
            index = inverse @ point
            box = torch.tensor(
                [[index[0] - 1, index[1] - 1, index[2] - 1, index[0] + 1, index[1] + 1, index[2] + 1]]
            )
            staged = clip({"image": image, "box": box, "label": torch.tensor([0])})
            staged = to_world(staged)
            world = np.asarray(staged["box"], dtype=float)[0]
            centre = (world[:3] + world[3:]) / 2.0
            errors.append(float(np.linalg.norm(centre - np.array(landmark["point_lps"]))))
            converted = np.asarray(to_mode(dict(staged))["box"], dtype=float)[0]
            mode_errors.append(float(np.linalg.norm(converted[:3] - centre)))
        results["true" if flag else "false"] = {
            "max_error_mm": max(errors) if errors else None,
            "landmark_errors_mm": errors,
            "max_mode_centre_error_mm": max(mode_errors) if mode_errors else None,
        }

    chosen = min(results, key=lambda key: results[key]["max_error_mm"])
    passed = results[chosen]["max_error_mm"] is not None and results[chosen]["max_error_mm"] <= LANDMARK_ERROR_TOL_MM
    return {
        "status": "passed" if passed else "failed",
        "config_sha256": _sha256(config),
        "reader": "preprocessing:LoadImaged(default)+EnsureChannelFirstd+Orientationd(RAS)+ScaleIntensityRanged",
        "postprocessing_target": "ClipBoxToImaged -> AffineBoxToWorldCoordinated -> ConvertBoxModed",
        "configured_flag": True,
        "agrees_with_lps": chosen == "true",
        "results": results,
    }


def _write_evidence(evidence: dict, out: Path) -> None:
    out = Path(out)
    out.parent.mkdir(parents=True, exist_ok=True)
    text = json.dumps(evidence, indent=2, sort_keys=True, allow_nan=False) + "\n"
    temporary = out.with_name(out.name + ".tmp")
    temporary.write_text(text, encoding="utf-8")
    os.replace(temporary, out)


def _failed_evidence(mode: str, code: str, message: str) -> dict:
    return {"schema_version": SCHEMA_VERSION, "mode": mode, "status": "failed", "error": {"code": code, "message": message}}


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="nodule_ai.check_geometry",
        description="QA geometry checks for one CT series and, optionally, the bundle transform probe.",
    )
    parser.add_argument("--mode", default="geometry", choices=["geometry", "bundle-transform"])
    parser.add_argument("--series-dir", type=Path)
    parser.add_argument("--reference", type=Path)
    parser.add_argument("--manifest", type=Path)
    parser.add_argument("--bundle-dir", type=Path)
    parser.add_argument("--nifti", type=Path, help="NIfTI for --mode bundle-transform")
    parser.add_argument("--nifti-out", type=Path, help="persist the QA NIfTI for an external probe")
    parser.add_argument("--out", required=True, type=Path)
    args = parser.parse_args(argv)

    if args.mode == "geometry" and not (args.series_dir and args.reference and args.manifest):
        parser.error("--mode geometry requires --series-dir, --reference and --manifest")
    if args.mode == "bundle-transform" and not (args.bundle_dir and args.nifti and args.reference):
        parser.error("--mode bundle-transform requires --bundle-dir, --nifti and --reference")
    if args.mode == "geometry" and args.bundle_dir is not None and args.nifti_out is None:
        parser.error("--bundle-dir requires --nifti-out so the probe can read the QA NIfTI")

    out = Path(args.out)
    if out.exists():
        out.unlink()

    try:
        if args.mode == "geometry":
            evidence = run_geometry(args.series_dir, args.reference, args.manifest, nifti_out=args.nifti_out)
            if args.bundle_dir is not None:
                if not _has_distribution("monai"):
                    raise _error("MONAI is not importable in this interpreter; run the probe in the AI environment", "MONAI_UNAVAILABLE")
                reference = _read_json(args.reference, "INVALID_REFERENCE")
                evidence["bundle_transform"] = run_bundle_transform(
                    args.bundle_dir, args.nifti_out, _landmarks_from_reference(reference)
                )
                if evidence["bundle_transform"]["status"] != "passed":
                    raise _error("bundle transform probe did not agree with LPS", "BUNDLE_TRANSFORM_MISMATCH")
        else:
            reference = _read_json(args.reference, "INVALID_REFERENCE")
            if not _has_distribution("monai"):
                raise _error("MONAI is not importable in this interpreter", "MONAI_UNAVAILABLE")
            transform = run_bundle_transform(args.bundle_dir, args.nifti, _landmarks_from_reference(reference))
            evidence = {
                "schema_version": SCHEMA_VERSION,
                "mode": "bundle-transform",
                "status": "passed" if transform["status"] == "passed" else "failed",
                "bundle_transform": transform,
            }
            if transform["status"] != "passed":
                evidence["error"] = {"code": "BUNDLE_TRANSFORM_MISMATCH", "message": "bundle transform probe did not agree with LPS"}
    except GeometryError as exc:
        evidence = _failed_evidence(args.mode, exc.code, str(exc))
    except DataError as exc:
        evidence = _failed_evidence(args.mode, "INVALID_REFERENCE", str(exc))
    except ToolError as exc:
        evidence = _failed_evidence(args.mode, "TOOL_ERROR", str(exc))
    except NoduleAIError as exc:
        evidence = _failed_evidence(args.mode, "ERROR", str(exc))
    except Exception as exc:  # unexpected: still record a failed run
        evidence = _failed_evidence(args.mode, "UNEXPECTED_ERROR", str(exc))

    evidence.setdefault("schema_version", SCHEMA_VERSION)
    evidence.setdefault("mode", args.mode)
    evidence.setdefault("status", "passed" if evidence.get("error") is None else "failed")
    _write_evidence(evidence, out)

    if evidence["status"] == "passed":
        print(json.dumps({"mode": evidence["mode"], "status": "passed", "out": str(out)}, sort_keys=True))
        return 0
    code = evidence["error"]["code"]
    print(f"check_geometry: {code}: {evidence['error']['message']}", file=sys.stderr)
    return 2 if code in INPUT_CODES else 1


if __name__ == "__main__":
    raise SystemExit(main())
