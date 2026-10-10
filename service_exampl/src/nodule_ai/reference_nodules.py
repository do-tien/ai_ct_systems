"""LIDC reading-session matching and QA reference export (LC1-02).

The reference is QA-only: it never enters the independent-read flow, the case
API or an inference job. It records patient-space (DICOM LPS, mm) nodule
centres and contour-derived axial diameters for the single demo series.
"""

from __future__ import annotations

import argparse
import hashlib
import importlib.metadata
import json
import math
import os
import re
import sys
import urllib.request
import zipfile
from pathlib import Path
from xml.etree import ElementTree as ET

import numpy as np

from .dicom_geometry import (
    DataError,
    mean_contour_center_lps,
    read_series_metadata,
    validate_uid,
)
from .errors import NoduleAIError, ToolError

SCHEMA_VERSION = "1"
COORDINATE_SYSTEM = "DICOM_LPS_mm"
Z_TOLERANCE_MM = 0.01
MIN_READING_SESSIONS = 3
MIN_MEDIAN_DIAMETER_MM = 10.0
MAX_CLUSTER_ANNOTATIONS = 4
SESSION_PREFIX = "reading-session"

ANNOTATIONS_ARCHIVE_URL = "https://www.cancerimagingarchive.net/wp-content/uploads/LIDC-XML-only.zip"
ANNOTATIONS_ARCHIVE_NAME = "LIDC-XML-only.zip"
ANNOTATIONS_TREE_NAME = "tcia-lidc-xml"

_XMLNS = re.compile(r'\sxmlns(:\w+)?="[^"]*"')
_UIDS = ("study_instance_uid", "series_instance_uid")


# --------------------------------------------------------------------------- #
# XML parsing
# --------------------------------------------------------------------------- #


def _parse_xml(path: Path) -> ET.Element:
    try:
        text = Path(path).read_text(encoding="utf-8", errors="replace")
    except OSError as exc:
        raise ToolError(f"cannot read annotation XML {path}: {exc}") from exc
    try:
        return ET.fromstring(_XMLNS.sub("", text))
    except ET.ParseError as exc:
        raise DataError(f"annotation XML {path} is not valid XML: {exc}") from exc


def _is_large_nodule(node: ET.Element) -> bool:
    """Mirror pylidc: keep nodules with a non-trivial characteristics block."""
    characteristics = node.find("characteristics")
    if characteristics is None or len(characteristics) == 0:
        return False
    return all((child.text or "").strip() not in ("", "0") for child in characteristics)


def _roi_coords(roi: ET.Element) -> str:
    return "\n".join(
        f"{edge.findtext('xCoord')},{edge.findtext('yCoord')}" for edge in roi.findall("edgeMap")
    )


def _roi_fingerprint(roi: ET.Element) -> tuple:
    return (
        round(float(roi.findtext("imageZposition")), 4),
        (roi.findtext("inclusion") or "").strip().upper() == "TRUE",
        _roi_coords(roi),
    )


def parse_reading_sessions(xml_path: Path) -> dict:
    """Read only the fields needed to match pylidc annotations to readers."""
    root = _parse_xml(xml_path)
    header = root.find("ResponseHeader")
    if header is None:
        raise DataError(f"annotation XML {xml_path} has no ResponseHeader")
    study_uid = header.findtext("StudyInstanceUID")
    series_uid = header.findtext("SeriesInstanceUid")
    if not study_uid or not series_uid:
        raise DataError(f"annotation XML {xml_path} is missing study/series UID")

    sessions = []
    for index, session in enumerate(root.findall("readingSession")):
        nodules = []
        for nodule in session.findall("unblindedReadNodule"):
            if not _is_large_nodule(nodule):
                continue
            rois = []
            for roi in nodule.findall("roi"):
                if len(roi.findall("edgeMap")) <= 1:
                    continue
                rois.append(
                    {
                        "sop_instance_uid": (roi.findtext("imageSOP_UID") or "").strip(),
                        "z": round(float(roi.findtext("imageZposition")), 4),
                        "inclusion": _roi_fingerprint(roi)[1],
                        "coords": _roi_coords(roi),
                    }
                )
            if not rois:
                continue
            nodules.append(
                {
                    "nodule_id": (nodule.findtext("noduleID") or "").strip(),
                    "fingerprint": tuple(sorted(_roi_fingerprint(roi) for roi in nodule.findall("roi") if len(roi.findall("edgeMap")) > 1)),
                    "rois": rois,
                }
            )
        # Only keep sessions that annotate at least one large nodule.
        if nodules:
            sessions.append({"id": f"{SESSION_PREFIX}-{index}", "nodules": nodules})

    return {
        "study_instance_uid": study_uid,
        "series_instance_uid": series_uid,
        "sessions": sessions,
    }


def build_annotation_index(annotations_dir: Path) -> dict:
    """Map (StudyInstanceUID, SeriesInstanceUID) -> XML path for a directory tree."""
    index = {}
    for path in sorted(Path(annotations_dir).rglob("*.xml")):
        try:
            root = _parse_xml(path)
        except NoduleAIError:
            continue
        header = root.find("ResponseHeader")
        if header is None:
            continue
        study_uid = header.findtext("StudyInstanceUID")
        series_uid = header.findtext("SeriesInstanceUid")
        if study_uid and series_uid:
            index[(study_uid, series_uid)] = path
    return index


# --------------------------------------------------------------------------- #
# Session matching
# --------------------------------------------------------------------------- #


def _annotation_fingerprint(annotation) -> tuple:
    return tuple(
        sorted(
            (
                round(float(contour.image_z_position), 4),
                bool(contour.inclusion),
                contour.coords,
            )
            for contour in annotation.contours
        )
    )


def match_reading_sessions(scan, xml_path: Path) -> tuple[dict, dict]:
    """Map each pylidc annotation to exactly one local XML reading session.

    Returns ``(session_ids, roi_bindings)`` keyed by annotation id. Raises
    DataError when an annotation matches zero or more than one XML nodule.
    """
    document = parse_reading_sessions(xml_path)
    if document["study_instance_uid"] != scan.study_instance_uid:
        raise DataError("annotation XML study UID does not match the scan")
    if document["series_instance_uid"] != scan.series_instance_uid:
        raise DataError("annotation XML series UID does not match the scan")

    candidates: dict[tuple, list[dict]] = {}
    for session in document["sessions"]:
        for nodule in session["nodules"]:
            candidates.setdefault(nodule["fingerprint"], []).append(
                {"session_id": session["id"], "nodule_id": nodule["nodule_id"], "rois": nodule["rois"]}
            )

    session_ids: dict[int, str] = {}
    roi_bindings: dict[int, list[dict]] = {}
    for annotation in scan.annotations:
        fingerprint = _annotation_fingerprint(annotation)
        matches = candidates.get(fingerprint, [])
        if len(matches) != 1:
            raise DataError(
                f"annotation {annotation.id} matches {len(matches)} XML reading sessions (expected 1)"
            )
        match = matches[0]
        session_ids[annotation.id] = match["session_id"]
        roi_bindings[annotation.id] = match["rois"]
    return session_ids, roi_bindings


# --------------------------------------------------------------------------- #
# Selection (discovery)
# --------------------------------------------------------------------------- #


def _reference_id(series_uid: str, annotation_ids) -> str:
    digest = hashlib.sha256(
        (series_uid + ":" + ",".join(str(i) for i in sorted(annotation_ids))).encode("ascii")
    ).hexdigest()
    return f"nodule-{digest[:16]}"


def _idc_series_row(client, series_uid: str):
    index = client.index
    rows = index[index["SeriesInstanceUID"] == series_uid]
    if len(rows) != 1:
        raise DataError(f"IDC index has {len(rows)} rows for series {series_uid}")
    row = rows.iloc[0]
    if str(row["Modality"]) != "CT":
        raise DataError(f"IDC series {series_uid} is not CT")
    return row


def _sorted_scans(scans):
    return sorted(scans, key=lambda s: (s.study_instance_uid, s.series_instance_uid))


def select_case(client, scans, annotations_dir: Path) -> dict:
    """Freeze the first eligible LIDC case from a shortlist of pylidc scans.

    A cluster is eligible when 3-4 annotations map to 3-4 distinct reading
    sessions and their median contour diameter is >= 10 mm.
    """
    index = build_annotation_index(annotations_dir)
    for scan in _sorted_scans(scans):
        xml_path = index.get((scan.study_instance_uid, scan.series_instance_uid))
        if xml_path is None:
            continue
        try:
            session_ids, _ = match_reading_sessions(scan, xml_path)
            clusters = scan.cluster_annotations(
                metric="min",
                tol=scan.pixel_spacing,
                factor=0.9,
                min_tol=0.1,
                verbose=False,
            )
        except DataError:
            continue
        if any(len(cluster) > MAX_CLUSTER_ANNOTATIONS for cluster in clusters):
            continue

        eligible = []
        for cluster in clusters:
            if not (MIN_READING_SESSIONS <= len(cluster) <= MAX_CLUSTER_ANNOTATIONS):
                continue
            session_set = sorted({session_ids[ann.id] for ann in cluster})
            if len(session_set) < MIN_READING_SESSIONS:
                continue
            diameters = [float(ann.diameter) for ann in cluster]
            if any(not math.isfinite(d) or d <= 0 for d in diameters):
                continue
            median = float(np.median(diameters))
            if median < MIN_MEDIAN_DIAMETER_MM:
                continue
            annotation_ids = sorted(ann.id for ann in cluster)
            eligible.append(
                {
                    "id": _reference_id(scan.series_instance_uid, annotation_ids),
                    "annotation_count": len(annotation_ids),
                    "annotation_ids": annotation_ids,
                    "session_ids": session_set,
                    "median_diameter_mm": median,
                    "measurements": [
                        {
                            "annotation_id": ann.id,
                            "session_id": session_ids[ann.id],
                            "nodule_id": getattr(ann, "_nodule_id", None),
                            "diameter_mm": float(ann.diameter),
                        }
                        for ann in sorted(cluster, key=lambda a: a.id)
                    ],
                }
            )
        if not eligible:
            continue

        row = _idc_series_row(client, scan.series_instance_uid)
        target = max(eligible, key=lambda c: (c["median_diameter_mm"], c["id"]))
        return {
            "schema_version": SCHEMA_VERSION,
            "source": {
                "collection_id": str(row["collection_id"]),
                "collection_doi": str(row["source_DOI"]),
                "license": str(row["license_short_name"]),
                "idc_version": client.get_idc_version(),
                "idc_index_version": importlib.metadata.version("idc-index"),
                "idc_index_data_version": importlib.metadata.version("idc-index-data"),
            },
            "study_instance_uid": scan.study_instance_uid,
            "series_instance_uid": scan.series_instance_uid,
            "expected_instance_count": int(row["instanceCount"]),
            "slice_thickness_mm": float(scan.slice_thickness),
            "target_reference_id": target["id"],
            "clusters": eligible,
            "artifacts": {
                "annotations": {
                    "name": Path(xml_path).name,
                    "sha256": _sha256(Path(xml_path)),
                }
            },
        }
    raise DataError("no eligible LIDC case found in the shortlist")


# --------------------------------------------------------------------------- #
# Reference build / export
# --------------------------------------------------------------------------- #


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with Path(path).open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _slice_lookup(series_metadata: dict) -> dict:
    lookup = {}
    for sop, position, projection in zip(
        series_metadata["sop_instance_uids"],
        series_metadata["image_position_patient"],
        series_metadata["slice_positions_mm"],
    ):
        lookup[sop] = (list(position), float(projection))
    return lookup


def _dependency_lock_hash() -> str | None:
    candidate = Path(__file__).resolve().parents[2] / "requirements-data.txt"
    if candidate.is_file():
        return _sha256(candidate)
    return None


def build_reference(selection: dict, series_metadata: dict, scan, session_ids: dict, roi_bindings: dict) -> dict:
    """Build the QA reference document from the frozen selection and real geometry."""
    for key in _UIDS:
        if selection[key] != series_metadata[key]:
            raise DataError(f"selection {key} does not match the series metadata")
    orientation = series_metadata["image_orientation_patient"]
    spacing = series_metadata["pixel_spacing_mm"]
    slice_lookup = _slice_lookup(series_metadata)
    annotations = {ann.id: ann for ann in scan.annotations}

    nodules = []
    for cluster in selection["clusters"]:
        annotation_ids = sorted(cluster["annotation_ids"])
        measurements = []
        centers = []
        diameters = []
        for annotation_id in annotation_ids:
            annotation = annotations.get(annotation_id)
            if annotation is None:
                raise DataError(f"annotation {annotation_id} is not present in the scan")
            bindings = roi_bindings.get(annotation_id)
            if not bindings:
                raise DataError(f"no ROI binding for annotation {annotation_id}")
            by_z = {round(float(binding["z"]), 4): binding for binding in bindings}

            contour_points = []
            contour_positions = []
            for contour in annotation.contours:
                binding = by_z.get(round(float(contour.image_z_position), 4))
                if binding is None:
                    raise DataError(f"contour z={contour.image_z_position} has no XML ROI binding")
                sop = binding["sop_instance_uid"]
                if sop not in slice_lookup:
                    raise DataError(f"ROI SOPInstanceUID {sop} is not present in the CT series")
                position, projection = slice_lookup[sop]
                if abs(projection - float(contour.image_z_position)) > Z_TOLERANCE_MM:
                    raise DataError(
                        f"contour z={contour.image_z_position} disagrees with slice projection {projection}"
                    )
                contour_points.append(contour.to_matrix(include_k=False))
                contour_positions.append(position)

            center = mean_contour_center_lps(contour_points, contour_positions, orientation, spacing)
            diameter = float(annotation.diameter)
            if not all(math.isfinite(v) for v in center) or not math.isfinite(diameter) or diameter <= 0:
                raise DataError(f"annotation {annotation_id} has a non-finite or non-positive measurement")
            measurements.append(
                {
                    "annotation_id": annotation_id,
                    "session_id": session_ids.get(annotation_id),
                    "nodule_id": getattr(annotation, "_nodule_id", None),
                    "center_mm": list(center),
                    "diameter_mm": diameter,
                }
            )
            centers.append(center)
            diameters.append(diameter)

        cluster_center = np.mean(np.array(centers, dtype=float), axis=0)
        cluster_diameter = float(np.median(diameters))
        if not all(math.isfinite(float(v)) for v in cluster_center) or cluster_diameter <= 0:
            raise DataError(f"cluster {cluster['id']} has a non-finite or non-positive aggregate")
        nodules.append(
            {
                "id": cluster["id"],
                "center_mm": [float(v) for v in cluster_center],
                "diameter_mm": cluster_diameter,
                "annotation_count": len(annotation_ids),
                "session_ids": sorted({m["session_id"] for m in measurements}),
                "annotation_ids": annotation_ids,
                "measurements": measurements,
            }
        )
    nodules.sort(key=lambda n: n["id"])

    geometry_keys = (
        "num_slices",
        "rows",
        "columns",
        "pixel_spacing_mm",
        "image_orientation_patient",
        "slice_normal_lps",
        "median_slice_spacing_mm",
        "slice_spacing_mm",
        "slice_thickness_mm",
        "slice_positions_mm",
        "sop_instance_uids",
    )
    provenance = {
        "collection_id": selection["source"].get("collection_id"),
        "collection_doi": selection["source"].get("collection_doi"),
        "license": selection["source"].get("license"),
        "idc_version": selection["source"].get("idc_version"),
        "idc_index_version": selection["source"].get("idc_index_version"),
        "idc_index_data_version": selection["source"].get("idc_index_data_version"),
        "pylidc_version": importlib.metadata.version("pylidc"),
        "pylidc_sqlite_sha256": _pylidc_sqlite_hash(),
        "annotations_artifact": selection.get("artifacts", {}).get("annotations"),
        "dependency_lock_sha256": _dependency_lock_hash(),
    }
    return {
        "schema_version": SCHEMA_VERSION,
        "study_instance_uid": series_metadata["study_instance_uid"],
        "series_instance_uid": series_metadata["series_instance_uid"],
        "frame_of_reference_uid": series_metadata["frame_of_reference_uid"],
        "coordinate_system": COORDINATE_SYSTEM,
        "target_reference_id": selection["target_reference_id"],
        "nodules": nodules,
        "geometry": {key: series_metadata[key] for key in geometry_keys},
        "methods": {
            "center": "arithmetic mean of contour points mapped to LPS via ImagePositionPatient/ImageOrientationPatient/PixelSpacing",
            "diameter": "median of per-annotation greatest axial contour diameter (pylidc Annotation.diameter)",
            "cluster_center": "arithmetic mean of per-annotation centres",
            "eligibility": "3-4 annotations from 3-4 distinct TCIA reading sessions, median diameter >= 10 mm",
            "caveats": [
                "contour-derived centre and axial diameter, not volumetric or clinical measurements",
                "reference is QA-only and is not shown in the independent-read flow",
            ],
        },
        "provenance": provenance,
    }


def _pylidc_sqlite_hash() -> str | None:
    try:
        import pkg_resources

        path = pkg_resources.resource_filename("pylidc", "pylidc.sqlite")
    except Exception:  # pragma: no cover - packaging dependent
        return None
    return _sha256(Path(path)) if Path(path).is_file() else None


def write_reference(reference: dict, out: Path) -> None:
    """Remove any prior generated reference, then write atomically."""
    out = Path(out)
    out.parent.mkdir(parents=True, exist_ok=True)
    if out.exists():
        out.unlink()
    text = json.dumps(reference, indent=2, sort_keys=True, allow_nan=False) + "\n"
    temporary = out.with_name(out.name + ".tmp")
    temporary.write_text(text, encoding="utf-8")
    os.replace(temporary, out)


def export_reference(selection: dict, series_metadata: dict, scan, session_ids: dict, roi_bindings: dict, out: Path) -> dict:
    """Remove prior output first so a failed export never leaves a stale reference."""
    out = Path(out)
    if out.exists():
        out.unlink()
    reference = build_reference(selection, series_metadata, scan, session_ids, roi_bindings)
    write_reference(reference, out)
    return reference


# --------------------------------------------------------------------------- #
# TCIA annotation archive
# --------------------------------------------------------------------------- #


def ensure_annotations(annotations_dir: Path) -> tuple[Path, dict]:
    """Ensure the official TCIA XML archive is extracted; return (root, provenance)."""
    annotations_dir = Path(annotations_dir)
    annotations_dir.mkdir(parents=True, exist_ok=True)
    tree_root = annotations_dir / ANNOTATIONS_TREE_NAME
    archive = annotations_dir / ANNOTATIONS_ARCHIVE_NAME
    if not archive.is_file():
        try:
            urllib.request.urlretrieve(ANNOTATIONS_ARCHIVE_URL, archive)
        except Exception as exc:
            raise ToolError(f"cannot download LIDC XML archive: {exc}") from exc
    archive_sha = _sha256(archive)
    if not tree_root.is_dir():
        try:
            with zipfile.ZipFile(archive) as bundle:
                for member in bundle.namelist():
                    target = (annotations_dir / member).resolve()
                    if not str(target).startswith(str(annotations_dir.resolve())):
                        raise DataError(f"archive member escapes target directory: {member}")
                bundle.extractall(annotations_dir)
        except (zipfile.BadZipFile, OSError) as exc:
            raise ToolError(f"cannot extract LIDC XML archive: {exc}") from exc
    return tree_root, {
        "url": ANNOTATIONS_ARCHIVE_URL,
        "name": ANNOTATIONS_ARCHIVE_NAME,
        "sha256": archive_sha,
    }


# --------------------------------------------------------------------------- #
# CLI
# --------------------------------------------------------------------------- #


def _find_scan(series_uid: str):
    import pylidc as pl

    scans = pl.query(pl.Scan).filter(pl.Scan.series_instance_uid == series_uid).all()
    for scan in scans:
        if scan.series_instance_uid == series_uid:
            return scan
    raise DataError(f"pylidc has no scan with series UID {series_uid}")


def _load_selection(path: Path) -> dict:
    from .fetch_case import _load_selection as load

    return load(path)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="nodule_ai.reference_nodules",
        description="Export a QA reference for one frozen LIDC-IDRI series (LC1-02).",
    )
    parser.add_argument("--series-dir", required=True, type=Path, help="downloaded CT series directory")
    parser.add_argument("--selection", required=True, type=Path, help="frozen selection JSON")
    parser.add_argument("--annotations-dir", required=True, type=Path, help="TCIA XML annotations directory")
    parser.add_argument("--out", required=True, type=Path, help="reference.json output path")
    args = parser.parse_args(argv)

    try:
        selection = _load_selection(args.selection)
        series_metadata = read_series_metadata(args.series_dir)
        index = build_annotation_index(args.annotations_dir)
        xml_path = index.get((selection["study_instance_uid"], selection["series_instance_uid"]))
        if xml_path is None:
            raise DataError("no annotation XML found for the selected series")
        expected = selection.get("artifacts", {}).get("annotations", {}).get("sha256")
        actual = _sha256(xml_path)
        if expected and expected != actual:
            raise DataError("annotation XML hash does not match the frozen selection")
        scan = _find_scan(selection["series_instance_uid"])
        session_ids, roi_bindings = match_reading_sessions(scan, xml_path)
        reference = export_reference(selection, series_metadata, scan, session_ids, roi_bindings, args.out)
    except NoduleAIError as exc:
        print(f"reference_nodules: {exc}", file=sys.stderr)
        return exc.exit_code

    print(
        json.dumps(
            {
                "series_instance_uid": reference["series_instance_uid"],
                "nodules": len(reference["nodules"]),
                "target_reference_id": reference["target_reference_id"],
                "out": str(args.out),
            },
            sort_keys=True,
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
