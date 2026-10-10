"""DICOM series metadata validation and patient-space coordinate conversion.

This module is the single place that defines slice ordering, the voxel->patient
affine, and the LPS<->RAS flip. It decodes no pixels, reconstructs no volume and
runs no inference; ``nodule_ai.volume`` consumes it for that.
"""

from __future__ import annotations

import math
import re
from pathlib import Path
from typing import Sequence

import numpy as np
import pydicom
from pydicom.uid import CTImageStorage

from .errors import DataError, GeometryError

# LC1-04 demo-case policy. Recorded in output provenance; never used to rewrite
# data into compliance.
MAX_SLICE_THICKNESS_MM = 2.5
MAX_SLICE_SPACING_MM = 2.5
SPACING_ABS_TOL_MM = 0.01
SPACING_REL_TOL = 0.01
DIRECTION_ATOL = 1e-5
IN_PLANE_ORIGIN_TOL_MM = 0.01

_UID_PATTERN = re.compile(r"^[0-9]+(?:\.[0-9]+)*$")
MAX_UID_LENGTH = 64

_RAS_FLIP = np.diag([-1.0, -1.0, 1.0, 1.0])


def _geometry(message: str, code: str) -> GeometryError:
    return GeometryError(message, code=code)


# --------------------------------------------------------------------------- #
# Pixel / patient conversion
# --------------------------------------------------------------------------- #


def pixel_to_lps(
    row: float,
    column: float,
    position_mm: Sequence[float],
    orientation: Sequence[float],
    pixel_spacing_mm: Sequence[float],
) -> tuple[float, float, float]:
    """Convert a pixel index to patient-space (DICOM LPS, mm).

    ``column`` runs along the first ImageOrientationPatient vector with
    PixelSpacing[1]; ``row`` runs along the second vector with PixelSpacing[0].
    """
    ipp = np.array([float(v) for v in position_mm], dtype=float)
    iop = np.array([float(v) for v in orientation], dtype=float)
    spacing = np.array([float(v) for v in pixel_spacing_mm], dtype=float)
    lps = ipp + column * spacing[1] * iop[:3] + row * spacing[0] * iop[3:]
    return (float(lps[0]), float(lps[1]), float(lps[2]))


def mean_contour_center_lps(
    contour_rows_columns: Sequence[Sequence[Sequence[float]]],
    contour_positions_mm: Sequence[Sequence[float]],
    orientation: Sequence[float],
    pixel_spacing_mm: Sequence[float],
) -> tuple[float, float, float]:
    """Mean LPS position of every contour point across all supplied slices."""
    points = []
    for rows_columns, position in zip(contour_rows_columns, contour_positions_mm):
        for point in rows_columns:
            points.append(pixel_to_lps(point[0], point[1], position, orientation, pixel_spacing_mm))
    if not points:
        raise DataError("cannot compute a centre from an empty contour set")
    mean = np.mean(np.array(points, dtype=float), axis=0)
    return (float(mean[0]), float(mean[1]), float(mean[2]))


def voxel_to_lps_affine(metadata: dict) -> np.ndarray:
    """Homogeneous 4x4 affine from index (column, row, slice) to LPS mm.

    Column 0 is IOP[:3]*PixelSpacing[1], column 1 is IOP[3:]*PixelSpacing[0],
    column 2 is the slice normal times the median slice spacing; translation is
    the first (lowest) slice ImagePositionPatient.
    """
    iop = _finite_vector(metadata.get("image_orientation_patient"), 6, "image_orientation_patient")
    spacing = _finite_vector(metadata.get("pixel_spacing_mm"), 2, "pixel_spacing_mm")
    step = metadata.get("median_slice_spacing_mm")
    normal = metadata.get("slice_normal_lps")
    origin = metadata.get("image_position_patient")
    if not isinstance(step, (int, float)) or not math.isfinite(float(step)) or float(step) <= 0:
        raise _geometry("median_slice_spacing_mm must be a positive finite number", "INVALID_GEOMETRY")
    normal = _finite_vector(normal, 3, "slice_normal_lps")
    if not isinstance(origin, list) or not origin:
        raise _geometry("image_position_patient must be a non-empty list", "INVALID_GEOMETRY")
    translation = _finite_vector(origin[0], 3, "image_position_patient[0]")

    affine = np.eye(4, dtype=float)
    affine[:3, 0] = np.array(iop[:3]) * spacing[1]
    affine[:3, 1] = np.array(iop[3:]) * spacing[0]
    affine[:3, 2] = np.array(normal) * float(step)
    affine[:3, 3] = translation
    return affine


def lps_to_ras_affine(affine_lps: np.ndarray) -> np.ndarray:
    """Flip the world x/y axes of an LPS affine (not a right-multiplication)."""
    _require_invertible_affine(affine_lps)
    return _RAS_FLIP @ np.asarray(affine_lps, dtype=float)


def voxel_to_world(index_xyz: Sequence[float], affine: np.ndarray) -> tuple[float, float, float]:
    """Map an index (column, row, slice) triple through a homogeneous affine."""
    result = _apply_affine(_finite_vector(index_xyz, 3, "index_xyz"), affine)
    return (float(result[0]), float(result[1]), float(result[2]))


def world_to_voxel(point_mm: Sequence[float], affine: np.ndarray) -> tuple[float, float, float]:
    """Inverse of :func:`voxel_to_world` for a patient-space point (mm)."""
    _require_invertible_affine(affine)
    matrix = np.asarray(affine, dtype=float)
    point = _finite_vector(point_mm, 3, "point_mm")
    index = np.linalg.solve(matrix[:3, :3], point - matrix[:3, 3])
    return (float(index[0]), float(index[1]), float(index[2]))


def lps_to_ras(point_mm: Sequence[float]) -> tuple[float, float, float]:
    """Convert a DICOM LPS point to NIfTI RAS by flipping x and y."""
    point = _finite_vector(point_mm, 3, "point_mm")
    return (-point[0], -point[1], point[2])


def ras_to_lps(point_mm: Sequence[float]) -> tuple[float, float, float]:
    """Convert a NIfTI RAS point to DICOM LPS (the same sign flip)."""
    return lps_to_ras(point_mm)


def _finite_vector(value: object, length: int, name: str) -> np.ndarray:
    if value is None:
        raise _geometry(f"{name} is missing", "INVALID_GEOMETRY")
    try:
        numbers = np.array([float(v) for v in value], dtype=float)
    except (TypeError, ValueError) as exc:
        raise _geometry(f"{name} is not numeric: {value!r}", "INVALID_GEOMETRY") from exc
    if numbers.shape != (length,) or not np.all(np.isfinite(numbers)):
        raise _geometry(f"{name} must be {length} finite numbers, got {value!r}", "INVALID_GEOMETRY")
    return numbers


def _require_invertible_affine(affine: object) -> None:
    matrix = np.asarray(affine, dtype=float) if affine is not None else None
    if matrix is None or matrix.shape != (4, 4) or not np.all(np.isfinite(matrix)):
        raise _geometry("affine must be a finite 4x4 matrix", "INVALID_GEOMETRY")
    if abs(float(np.linalg.det(matrix[:3, :3]))) <= 1e-12:
        raise _geometry("affine is not invertible", "INVALID_GEOMETRY")


def _apply_affine(vector: np.ndarray, affine: object) -> np.ndarray:
    _require_invertible_affine(affine)
    matrix = np.asarray(affine, dtype=float)
    return matrix[:3, :3] @ vector + matrix[:3, 3]


# --------------------------------------------------------------------------- #
# Series validation
# --------------------------------------------------------------------------- #


def validate_uid(value: object, name: str) -> str:
    """Return a syntactically valid DICOM UID (<=64 chars) or raise."""
    if not isinstance(value, str) or not value:
        raise _geometry(f"{name} is missing or not a string", "INVALID_UID")
    if len(value) > MAX_UID_LENGTH or not _UID_PATTERN.match(value):
        raise _geometry(f"{name} is not a valid DICOM UID: {value!r}", "INVALID_UID")
    return value


def _vector(ds, keyword: str, length: int) -> list[float]:
    value = ds.get(keyword)
    if value is None:
        raise _geometry(f"{keyword} is missing", "INVALID_GEOMETRY")
    try:
        numbers = [float(v) for v in value]
    except (TypeError, ValueError) as exc:
        raise _geometry(f"{keyword} is not numeric: {value!r}", "INVALID_GEOMETRY") from exc
    if len(numbers) != length or not all(math.isfinite(n) for n in numbers):
        raise _geometry(f"{keyword} must be {length} finite numbers, got {value!r}", "INVALID_GEOMETRY")
    return numbers


def _single(values: list[str], name: str, code: str) -> str:
    unique = set(values)
    if len(unique) != 1:
        if len(unique) > 1 and all(value for value in unique):
            raise _geometry(f"{name} is not unique across the series: {sorted(unique)}", "UID_MISMATCH")
        raise _geometry(f"{name} is missing or not unique across the series: {sorted(unique)}", code)
    return unique.pop()


def read_series_metadata(series_dir: Path) -> dict:
    """Validate a single-frame CT series and return its validated geometry.

    Raises GeometryError (a DataError) with a stable ``code``. Slices are ordered
    by ascending projection of ImagePositionPatient onto the slice normal, never
    by filename or InstanceNumber. ``ordered_files`` follows that same order.
    """
    series_dir = Path(series_dir)
    if not series_dir.is_dir():
        raise _geometry(f"series directory does not exist: {series_dir}", "INVALID_DICOM")
    files = sorted(p for p in series_dir.iterdir() if p.is_file())
    if len(files) < 2:
        raise _geometry(f"expected multiple CT slices, found {len(files)} file(s) in {series_dir}", "INSUFFICIENT_SLICES")

    datasets = []
    for path in files:
        try:
            datasets.append(pydicom.dcmread(path, stop_before_pixels=True, force=False))
        except Exception as exc:  # pydicom raises several unrelated error types
            raise _geometry(f"cannot read DICOM header {path.name}: {exc}", "INVALID_DICOM") from exc

    modality = _single([str(ds.get("Modality", "")) for ds in datasets], "Modality", "NOT_CT")
    if modality != "CT":
        raise _geometry(f"Modality must be CT, got {modality!r}", "NOT_CT")
    for ds in datasets:
        sop_class = str(ds.get("SOPClassUID", ""))
        if sop_class != str(CTImageStorage):
            raise _geometry(f"only single-frame CT Image Storage is supported, got {sop_class!r}", "UNSUPPORTED_CT")
        frames = ds.get("NumberOfFrames")
        if frames is not None and int(frames) != 1:
            raise _geometry(f"multi-frame DICOM is not supported (NumberOfFrames={frames})", "UNSUPPORTED_CT")

    study_uid = validate_uid(_single([str(ds.get("StudyInstanceUID", "")) for ds in datasets], "StudyInstanceUID", "INVALID_UID"), "StudyInstanceUID")
    series_uid = validate_uid(_single([str(ds.get("SeriesInstanceUID", "")) for ds in datasets], "SeriesInstanceUID", "INVALID_UID"), "SeriesInstanceUID")
    frame_uid = validate_uid(
        _single([str(ds.get("FrameOfReferenceUID", "")) for ds in datasets], "FrameOfReferenceUID", "INVALID_UID"),
        "FrameOfReferenceUID",
    )

    sop_uids = [validate_uid(str(ds.get("SOPInstanceUID", "")), "SOPInstanceUID") for ds in datasets]
    if len(set(sop_uids)) != len(sop_uids):
        duplicates = sorted({uid for uid in sop_uids if sop_uids.count(uid) > 1})
        raise _geometry(f"duplicate SOPInstanceUID(s): {duplicates}", "DUPLICATE_SOP")

    rows = _single([str(int(ds.get("Rows", 0))) for ds in datasets], "Rows", "INVALID_GEOMETRY")
    columns = _single([str(int(ds.get("Columns", 0))) for ds in datasets], "Columns", "INVALID_GEOMETRY")
    if int(rows) <= 0 or int(columns) <= 0:
        raise _geometry(f"Rows/Columns must be positive, got {rows}x{columns}", "INVALID_GEOMETRY")

    pixel_spacing = _vector(datasets[0], "PixelSpacing", 2)
    if pixel_spacing[0] <= 0 or pixel_spacing[1] <= 0:
        raise _geometry(f"PixelSpacing must be positive, got {pixel_spacing}", "INVALID_GEOMETRY")
    for ds in datasets[1:]:
        if _vector(ds, "PixelSpacing", 2) != pixel_spacing:
            raise _geometry("PixelSpacing is not consistent across the series", "INVALID_GEOMETRY")

    orientation = _vector(datasets[0], "ImageOrientationPatient", 6)
    for ds in datasets[1:]:
        if _vector(ds, "ImageOrientationPatient", 6) != orientation:
            raise _geometry("ImageOrientationPatient is not consistent across the series", "INVALID_GEOMETRY")
    row_dir = np.array(orientation[:3])
    column_dir = np.array(orientation[3:])
    if abs(np.linalg.norm(row_dir) - 1.0) > DIRECTION_ATOL or abs(np.linalg.norm(column_dir) - 1.0) > DIRECTION_ATOL:
        raise _geometry("ImageOrientationPatient vectors must be unit length", "INVALID_GEOMETRY")
    if abs(float(np.dot(row_dir, column_dir))) > DIRECTION_ATOL:
        raise _geometry("ImageOrientationPatient vectors must be orthogonal", "INVALID_GEOMETRY")

    normal = np.cross(row_dir, column_dir)
    normal_norm = float(np.linalg.norm(normal))
    if normal_norm <= DIRECTION_ATOL:
        raise _geometry("ImageOrientationPatient produces a degenerate slice normal", "INVALID_GEOMETRY")
    normal = normal / normal_norm

    thicknesses = []
    positions = []
    for ds, sop_uid in zip(datasets, sop_uids):
        raw_thickness = ds.get("SliceThickness", 0)
        try:
            thickness = float(raw_thickness or 0)
        except (TypeError, ValueError) as exc:
            raise _geometry(f"SliceThickness is not numeric: {raw_thickness!r}", "SLICE_TOO_THICK") from exc
        if not math.isfinite(thickness) or thickness <= 0 or thickness > MAX_SLICE_THICKNESS_MM:
            raise _geometry(
                f"SliceThickness must be in (0, {MAX_SLICE_THICKNESS_MM}] mm, got {thickness} for {sop_uid}",
                "SLICE_TOO_THICK",
            )
        thicknesses.append(thickness)
        positions.append(np.array(_vector(ds, "ImagePositionPatient", 3)))

    projections = [float(np.dot(position, normal)) for position in positions]
    if len(set(projections)) != len(projections):
        raise _geometry("two slices share the same position along the slice normal", "DUPLICATE_POSITION")
    order = sorted(range(len(datasets)), key=lambda i: projections[i])

    ordered = {
        "sop_instance_uids": [sop_uids[i] for i in order],
        "ordered_files": [files[i] for i in order],
        "slice_positions_mm": [projections[i] for i in order],
        "image_position_patient": [positions[i].tolist() for i in order],
        "slice_thickness_mm": [thicknesses[i] for i in order],
    }

    origin = positions[order[0]]
    for index in order[1:]:
        offset = positions[index] - origin
        in_plane = offset - float(np.dot(offset, normal)) * normal
        if float(np.linalg.norm(in_plane)) > IN_PLANE_ORIGIN_TOL_MM:
            raise _geometry(
                "ImagePositionPatient drifts in the slice plane (gantry tilt or moving origin)",
                "UNSUPPORTED_GANTRY_TILT",
            )

    steps = [
        ordered["slice_positions_mm"][i + 1] - ordered["slice_positions_mm"][i]
        for i in range(len(order) - 1)
    ]
    if any(step <= 0 for step in steps):
        raise _geometry("slice spacing must be positive along the slice normal", "NONUNIFORM_SPACING")
    if any(step > MAX_SLICE_SPACING_MM for step in steps):
        raise _geometry(f"slice spacing must be <= {MAX_SLICE_SPACING_MM} mm, got {steps}", "SLICE_SPACING_TOO_LARGE")
    median_spacing = float(np.median(steps))
    tolerance = max(SPACING_ABS_TOL_MM, SPACING_REL_TOL * median_spacing)
    if any(abs(step - median_spacing) > tolerance for step in steps):
        raise _geometry(f"slice spacing is not uniform: {steps} (median {median_spacing})", "NONUNIFORM_SPACING")

    return {
        "study_instance_uid": study_uid,
        "series_instance_uid": series_uid,
        "frame_of_reference_uid": frame_uid,
        "modality": modality,
        "num_slices": len(datasets),
        "rows": int(rows),
        "columns": int(columns),
        "pixel_spacing_mm": pixel_spacing,
        "image_orientation_patient": orientation,
        "slice_normal_lps": normal.tolist(),
        "median_slice_spacing_mm": median_spacing,
        "slice_spacing_mm": steps,
        **ordered,
    }
