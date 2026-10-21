"""Decode an ordered CT series into a SimpleITK image and write a validated NIfTI.

Policy (thickness/spacing/direction/drift) lives in ``dicom_geometry``; this
module decodes pixels, cross-checks what the reader actually produced against
the validated metadata, and serialises a temporary NIfTI. It never resamples,
never runs inference and never writes into the series directory.
"""

from __future__ import annotations

from pathlib import Path
from typing import Sequence

import numpy as np
import SimpleITK as sitk

from .dicom_geometry import (
    DIRECTION_ATOL,
    SPACING_ABS_TOL_MM,
    lps_to_ras_affine,
    read_series_metadata,
    voxel_to_lps_affine,
    voxel_to_world,
)
from .errors import GeometryError, ToolError

# Reader-vs-metadata tolerances. The metadata policy already bounds spacing and
# in-plane drift; this bounds the cumulative affine error the reader introduces.
SPACING_READER_ATOL_MM = SPACING_ABS_TOL_MM
SLICE_ORIGIN_ATOL_MM = 0.01
CORNER_ATOL_MM = 0.05
NIFTI_AFFINE_ATOL_MM = 1e-3
NIFTI_CORNER_ATOL_MM = 1e-3


def _error(message: str, code: str) -> GeometryError:
    return GeometryError(message, code=code)


def _check_inventory(metadata: dict, expected_sop_uids: Sequence[str] | None) -> None:
    actual = list(metadata["sop_instance_uids"])
    if expected_sop_uids is None:
        # No trusted inventory: completeness is asserted, not assumed.
        metadata["completeness"] = "not_verified"
        return
    expected = list(expected_sop_uids)
    actual_set, expected_set = set(actual), set(expected)
    if actual_set != expected_set or len(actual) != len(expected):
        missing = sorted(expected_set - actual_set)
        unexpected = sorted(actual_set - expected_set)
        raise _error(
            f"series SOP inventory mismatch (missing={len(missing)}, unexpected={len(unexpected)})",
            "INCOMPLETE_SERIES",
        )
    metadata["completeness"] = "verified"


def validate_decoded_pixels(image: sitk.Image) -> None:
    """Reject anything that is not a finite single-component 3D scalar volume."""
    if image.GetDimension() != 3:
        raise _error(f"decoded volume must be 3D, got {image.GetDimension()}D", "INVALID_PIXEL_DATA")
    if image.GetNumberOfComponentsPerPixel() != 1:
        raise _error(
            f"decoded volume must be scalar, got {image.GetNumberOfComponentsPerPixel()} components",
            "INVALID_PIXEL_DATA",
        )
    array = sitk.GetArrayViewFromImage(image)  # view: no whole-volume copy
    if array.size == 0:
        raise _error("decoded volume is empty", "INVALID_PIXEL_DATA")
    if not bool(np.all(np.isfinite(array))):
        raise _error("decoded volume contains non-finite values", "INVALID_PIXEL_DATA")


def _expected_direction(metadata: dict) -> np.ndarray:
    iop = metadata["image_orientation_patient"]
    matrix = np.eye(3)
    matrix[:, 0] = iop[:3]
    matrix[:, 1] = iop[3:]
    matrix[:, 2] = metadata["slice_normal_lps"]
    return matrix


def itk_affine(image: sitk.Image) -> np.ndarray:
    affine = np.eye(4)
    # ITK keeps direction (unit) and spacing apart; the index transform is
    # origin + direction @ diag(spacing) @ index.
    direction = np.array(image.GetDirection()).reshape(3, 3)
    affine[:3, :3] = direction @ np.diag([float(v) for v in image.GetSpacing()])
    affine[:3, 3] = image.GetOrigin()
    return affine


def _cross_check(image: sitk.Image, metadata: dict) -> None:
    size = list(image.GetSize())
    expected_size = [int(metadata["columns"]), int(metadata["rows"]), int(metadata["num_slices"])]
    if size != expected_size:
        raise _error(f"decoded size {size} does not match metadata {expected_size}", "GEOMETRY_MISMATCH")

    spacing = list(image.GetSpacing())
    pixel_spacing = metadata["pixel_spacing_mm"]
    expected_spacing = [pixel_spacing[1], pixel_spacing[0], metadata["median_slice_spacing_mm"]]
    if any(abs(a - b) > SPACING_READER_ATOL_MM for a, b in zip(spacing, expected_spacing)):
        raise _error(f"decoded spacing {spacing} does not match metadata {expected_spacing}", "GEOMETRY_MISMATCH")

    origin = list(image.GetOrigin())
    expected_origin = metadata["image_position_patient"][0]
    if any(abs(a - b) > SPACING_READER_ATOL_MM for a, b in zip(origin, expected_origin)):
        raise _error(f"decoded origin {origin} does not match metadata {expected_origin}", "GEOMETRY_MISMATCH")

    direction = np.array(image.GetDirection()).reshape(3, 3)
    if float(np.max(np.abs(direction - _expected_direction(metadata)))) > DIRECTION_ATOL:
        raise _error("decoded direction does not match ImageOrientationPatient", "GEOMETRY_MISMATCH")

    for index, expected in enumerate(metadata["image_position_patient"]):
        actual = image.TransformIndexToPhysicalPoint((0, 0, index))
        if any(abs(a - b) > SLICE_ORIGIN_ATOL_MM for a, b in zip(actual, expected)):
            raise _error(f"decoded slice {index} origin {actual} does not match {expected}", "GEOMETRY_MISMATCH")

    affine = voxel_to_lps_affine(metadata)
    worst = 0.0
    for i in (0, size[0] - 1):
        for j in (0, size[1] - 1):
            for k in (0, size[2] - 1):
                reader_point = np.array(image.TransformIndexToPhysicalPoint((i, j, k)))
                affine_point = np.array(voxel_to_world((i, j, k), affine))
                worst = max(worst, float(np.linalg.norm(reader_point - affine_point)))
    if worst > CORNER_ATOL_MM:
        raise _error(
            f"reader/metadata affine disagreement {worst:.6f} mm exceeds {CORNER_ATOL_MM} mm",
            "GEOMETRY_MISMATCH",
        )
    metadata["reader_affine_max_error_mm"] = worst


def read_ct_volume(series_dir: Path, *, expected_sop_uids: Sequence[str] | None = None) -> tuple[sitk.Image, dict]:
    """Decode the validated, ordered CT series into an LPS SimpleITK image.

    ``expected_sop_uids`` is an optional trusted inventory; when supplied, the
    actual SOP set must match it exactly (otherwise ``INCOMPLETE_SERIES``).
    """
    metadata = read_series_metadata(series_dir)
    _check_inventory(metadata, expected_sop_uids)

    reader = sitk.ImageSeriesReader()
    reader.SetFileNames([str(path) for path in metadata["ordered_files"]])
    try:
        image = reader.Execute()
    except RuntimeError as exc:
        raise _error(f"pixel decode failed: {exc}", "PIXEL_DECODE_FAILED") from exc
    except OSError as exc:
        raise ToolError(f"cannot read DICOM pixel data: {exc}") from exc

    try:
        image = sitk.Cast(image, sitk.sitkFloat32)
    except RuntimeError as exc:  # pragma: no cover - defensive
        raise _error(f"cannot convert decoded volume to float: {exc}", "INVALID_PIXEL_DATA") from exc

    validate_decoded_pixels(image)
    _cross_check(image, metadata)
    return image, metadata


def write_nifti(
    image: sitk.Image,
    out_path: Path,
    *,
    expected_shape: Sequence[int] | None = None,
) -> Path:
    """Write ``image`` as .nii.gz and verify the affine/pixels read back.

    The caller owns the destination's lifetime (a TemporaryDirectory); on
    failure only this call's own output file is removed.
    """
    import nibabel as nib

    out = Path(out_path)
    out.parent.mkdir(parents=True, exist_ok=True)
    array = sitk.GetArrayFromImage(image)  # (z, y, x)
    data = np.transpose(array, (2, 1, 0))  # nibabel is (x, y, z)
    affine_ras = lps_to_ras_affine(itk_affine(image))

    created = False
    try:
        nib.save(nib.Nifti1Image(data, affine_ras), str(out))
        created = True
        reloaded = nib.load(str(out))
        shape = list(reloaded.shape)
        # nibabel shape is (x, y, z) = (columns, rows, slices) = SimpleITK size.
        expected = list(expected_shape) if expected_shape is not None else list(image.GetSize())
        if shape != expected:
            raise _error(f"reopened NIfTI shape {shape} does not match {expected}", "GEOMETRY_MISMATCH")
        read_affine = np.asarray(reloaded.affine, dtype=float)
        if not np.all(np.isfinite(read_affine)) or abs(float(np.linalg.det(read_affine[:3, :3]))) <= 1e-12:
            raise _error("reopened NIfTI affine is not finite and invertible", "NIFTI_AFFINE_MISMATCH")
        if float(np.max(np.abs(read_affine - affine_ras))) > NIFTI_AFFINE_ATOL_MM:
            raise _error("reopened NIfTI affine differs from the LPS->RAS affine", "NIFTI_AFFINE_MISMATCH")

        size = image.GetSize()
        for i in (0, size[0] - 1):
            for j in (0, size[1] - 1):
                for k in (0, size[2] - 1):
                    lps = np.array(image.TransformIndexToPhysicalPoint((i, j, k)))
                    ras = np.array([-lps[0], -lps[1], lps[2], 1.0])
                    recovered = read_affine @ np.array([i, j, k, 1.0])
                    if float(np.linalg.norm(ras - recovered)) > NIFTI_CORNER_ATOL_MM:
                        raise _error("reopened NIfTI corner world coordinate mismatch", "NIFTI_AFFINE_MISMATCH")
    except GeometryError:
        if created and out.exists():
            out.unlink()
        raise
    except Exception as exc:
        if created and out.exists():
            out.unlink()
        raise _error(f"cannot write NIfTI: {exc}", "NIFTI_WRITE_FAILED") from exc
    return out
