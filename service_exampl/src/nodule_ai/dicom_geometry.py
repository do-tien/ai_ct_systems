"""DICOM series metadata reading and validation for the LC1-02 data tools.

The module is deliberately narrow: it validates a single-frame CT series and
returns the geometry needed to order slices and map pixel indices to patient
space. It does not reconstruct a volume, write NIfTI, or run inference.
"""

from __future__ import annotations

import math
import re
from pathlib import Path
from typing import Sequence

import numpy as np
import pydicom
from pydicom.uid import CTImageStorage

from .errors import DataError

# LC1-02 demo-case policy. Recorded in output provenance; never used to rewrite
# data into compliance.
MAX_SLICE_THICKNESS_MM = 2.5
MAX_SLICE_SPACING_MM = 2.5
SPACING_ABS_TOL_MM = 0.01
SPACING_REL_TOL = 0.01
DIRECTION_ATOL = 1e-5
IN_PLANE_ORIGIN_TOL_MM = 0.01

_UID_PATTERN = re.compile(r"^[0-9]+(?:\.[0-9]+)*$")
MAX_UID_LENGTH = 64


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


def validate_uid(value: object, name: str) -> str:
    """Return a syntactically valid DICOM UID (<=64 chars) or raise DataError."""
    if not isinstance(value, str) or not value:
        raise DataError(f"{name} is missing or not a string")
    if len(value) > MAX_UID_LENGTH or not _UID_PATTERN.match(value):
        raise DataError(f"{name} is not a valid DICOM UID: {value!r}")
    return value


def _vector(ds, keyword: str, length: int) -> list[float]:
    value = ds.get(keyword)
    if value is None:
        raise DataError(f"{keyword} is missing")
    try:
        numbers = [float(v) for v in value]
    except (TypeError, ValueError) as exc:
        raise DataError(f"{keyword} is not numeric: {value!r}") from exc
    if len(numbers) != length or not all(math.isfinite(n) for n in numbers):
        raise DataError(f"{keyword} must be {length} finite numbers, got {value!r}")
    return numbers


def _single(values: list[str], name: str) -> str:
    unique = set(values)
    if len(unique) != 1:
        raise DataError(f"{name} is not unique across the series: {sorted(unique)}")
    return unique.pop()


def read_series_metadata(series_dir: Path) -> dict:
    """Validate a single-frame CT series directory and return its geometry.

    Raises DataError for any inconsistency. Slices are ordered by the inverse
    projection of ImagePositionPatient onto the slice normal, never by filename
    or InstanceNumber.
    """
    series_dir = Path(series_dir)
    if not series_dir.is_dir():
        raise DataError(f"series directory does not exist: {series_dir}")
    files = sorted(p for p in series_dir.iterdir() if p.is_file())
    if len(files) < 2:
        raise DataError(f"expected multiple CT slices, found {len(files)} file(s) in {series_dir}")

    datasets = []
    for path in files:
        try:
            datasets.append(pydicom.dcmread(path, stop_before_pixels=True, force=False))
        except Exception as exc:  # pydicom raises several unrelated error types
            raise DataError(f"cannot read DICOM header {path.name}: {exc}") from exc

    modality = _single([str(ds.get("Modality", "")) for ds in datasets], "Modality")
    if modality != "CT":
        raise DataError(f"Modality must be CT, got {modality!r}")
    for ds in datasets:
        sop_class = str(ds.get("SOPClassUID", ""))
        if sop_class != str(CTImageStorage):
            raise DataError(f"only single-frame CT Image Storage is supported, got {sop_class!r}")
        frames = ds.get("NumberOfFrames")
        if frames is not None and int(frames) != 1:
            raise DataError(f"multi-frame DICOM is not supported (NumberOfFrames={frames})")

    study_uid = validate_uid(_single([str(ds.get("StudyInstanceUID", "")) for ds in datasets], "StudyInstanceUID"), "StudyInstanceUID")
    series_uid = validate_uid(_single([str(ds.get("SeriesInstanceUID", "")) for ds in datasets], "SeriesInstanceUID"), "SeriesInstanceUID")
    frame_uid = validate_uid(
        _single([str(ds.get("FrameOfReferenceUID", "")) for ds in datasets], "FrameOfReferenceUID"),
        "FrameOfReferenceUID",
    )

    sop_uids = [validate_uid(str(ds.get("SOPInstanceUID", "")), "SOPInstanceUID") for ds in datasets]
    if len(set(sop_uids)) != len(sop_uids):
        duplicates = sorted({uid for uid in sop_uids if sop_uids.count(uid) > 1})
        raise DataError(f"duplicate SOPInstanceUID(s): {duplicates}")

    rows = _single([str(int(ds.get("Rows", 0))) for ds in datasets], "Rows")
    columns = _single([str(int(ds.get("Columns", 0))) for ds in datasets], "Columns")
    if int(rows) <= 0 or int(columns) <= 0:
        raise DataError(f"Rows/Columns must be positive, got {rows}x{columns}")

    pixel_spacing = _vector(datasets[0], "PixelSpacing", 2)
    if pixel_spacing[0] <= 0 or pixel_spacing[1] <= 0:
        raise DataError(f"PixelSpacing must be positive, got {pixel_spacing}")
    for ds in datasets[1:]:
        if _vector(ds, "PixelSpacing", 2) != pixel_spacing:
            raise DataError("PixelSpacing is not consistent across the series")

    orientation = _vector(datasets[0], "ImageOrientationPatient", 6)
    for ds in datasets[1:]:
        if _vector(ds, "ImageOrientationPatient", 6) != orientation:
            raise DataError("ImageOrientationPatient is not consistent across the series")
    row_dir = np.array(orientation[:3])
    column_dir = np.array(orientation[3:])
    if abs(np.linalg.norm(row_dir) - 1.0) > DIRECTION_ATOL or abs(np.linalg.norm(column_dir) - 1.0) > DIRECTION_ATOL:
        raise DataError("ImageOrientationPatient vectors must be unit length")
    if abs(float(np.dot(row_dir, column_dir))) > DIRECTION_ATOL:
        raise DataError("ImageOrientationPatient vectors must be orthogonal")

    normal = np.cross(row_dir, column_dir)
    normal_norm = float(np.linalg.norm(normal))
    if normal_norm <= DIRECTION_ATOL:
        raise DataError("ImageOrientationPatient produces a degenerate slice normal")
    normal = normal / normal_norm

    thicknesses = []
    positions = []
    for ds, sop_uid in zip(datasets, sop_uids):
        thickness = float(ds.get("SliceThickness", 0) or 0)
        if not math.isfinite(thickness) or thickness <= 0 or thickness > MAX_SLICE_THICKNESS_MM:
            raise DataError(
                f"SliceThickness must be in (0, {MAX_SLICE_THICKNESS_MM}] mm, got {thickness} for {sop_uid}"
            )
        thicknesses.append(thickness)
        positions.append(np.array(_vector(ds, "ImagePositionPatient", 3)))

    projections = [float(np.dot(position, normal)) for position in positions]
    if len(set(projections)) != len(projections):
        raise DataError("two slices share the same position along the slice normal")
    order = sorted(range(len(datasets)), key=lambda i: projections[i])

    ordered = {
        "sop_instance_uids": [sop_uids[i] for i in order],
        "slice_positions_mm": [projections[i] for i in order],
        "image_position_patient": [positions[i].tolist() for i in order],
        "slice_thickness_mm": [thicknesses[i] for i in order],
    }

    origin = positions[order[0]]
    for index in order[1:]:
        offset = positions[index] - origin
        in_plane = offset - float(np.dot(offset, normal)) * normal
        if float(np.linalg.norm(in_plane)) > IN_PLANE_ORIGIN_TOL_MM:
            raise DataError("ImagePositionPatient drifts in the slice plane (gantry tilt or moving origin)")

    steps = [
        ordered["slice_positions_mm"][i + 1] - ordered["slice_positions_mm"][i]
        for i in range(len(order) - 1)
    ]
    if any(step <= 0 for step in steps):
        raise DataError("slice spacing must be positive along the slice normal")
    if any(step > MAX_SLICE_SPACING_MM for step in steps):
        raise DataError(f"slice spacing must be <= {MAX_SLICE_SPACING_MM} mm, got {steps}")
    median_spacing = float(np.median(steps))
    tolerance = max(SPACING_ABS_TOL_MM, SPACING_REL_TOL * median_spacing)
    if any(abs(step - median_spacing) > tolerance for step in steps):
        raise DataError(f"slice spacing is not uniform: {steps} (median {median_spacing})")

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
