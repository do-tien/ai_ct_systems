"""Download exactly one frozen LIDC-IDRI CT series and validate its geometry.

The CLI implements only the frozen-selection fetch mode: it never selects a
case, never downloads a whole collection, and never writes a success manifest
for an incomplete or inconsistent download. Discovery (``--select``) is added
separately once the XML reading-session matcher exists.
"""

from __future__ import annotations

import argparse
import hashlib
import importlib.metadata
import json
import os
import sys
from pathlib import Path

from .dicom_geometry import read_series_metadata, validate_uid
from .errors import DataError, NoduleAIError, ToolError

SCHEMA_VERSION = "1"
MANIFEST_NAME = "case-manifest.json"
DICOM_DIRNAME = "dicom"
DEFAULT_DIR_TEMPLATE = "%SeriesInstanceUID"
REQUIRED_SOURCE_KEYS = ("collection_id", "idc_version", "idc_index_data_version")


def validate_selection(selection: object) -> dict:
    """Validate the frozen selection document shared by fetch and reference."""
    if not isinstance(selection, dict):
        raise DataError("selection must be a JSON object")
    if selection.get("schema_version") != SCHEMA_VERSION:
        raise DataError(f"selection.schema_version must be {SCHEMA_VERSION!r}")
    source = selection.get("source")
    if not isinstance(source, dict):
        raise DataError("selection.source must be an object")
    for key in REQUIRED_SOURCE_KEYS:
        if not isinstance(source.get(key), str) or not source[key]:
            raise DataError(f"selection.source.{key} is required")
    validate_uid(selection.get("study_instance_uid"), "study_instance_uid")
    validate_uid(selection.get("series_instance_uid"), "series_instance_uid")
    if selection.get("frame_of_reference_uid") is not None:
        validate_uid(selection["frame_of_reference_uid"], "frame_of_reference_uid")
    count = selection.get("expected_instance_count")
    if not isinstance(count, int) or isinstance(count, bool) or count <= 0:
        raise DataError("selection.expected_instance_count must be a positive integer")
    if not isinstance(selection.get("target_reference_id"), str) or not selection["target_reference_id"]:
        raise DataError("selection.target_reference_id is required")
    if not isinstance(selection.get("clusters"), list):
        raise DataError("selection.clusters must be a list")
    return selection


def _load_selection(path: Path) -> dict:
    path = Path(path)
    if not path.is_file():
        raise DataError(f"selection file not found: {path}")
    try:
        text = path.read_text(encoding="utf-8")
    except OSError as exc:
        raise ToolError(f"cannot read selection file {path}: {exc}") from exc
    try:
        return json.loads(text)
    except json.JSONDecodeError as exc:
        raise DataError(f"selection file is not valid JSON: {exc}") from exc


def _new_idc_client():
    from idc_index import IDCClient

    return IDCClient()


def _build_client(selection: dict):
    """Create the pinned-version IDC client after checking frozen index versions."""
    source = selection["source"]
    installed = importlib.metadata.version("idc-index-data")
    if installed != source["idc_index_data_version"]:
        raise DataError(
            "installed idc-index-data version "
            f"{installed} does not match frozen selection {source['idc_index_data_version']}"
        )
    client = _new_idc_client()
    actual = client.get_idc_version()
    if actual != source["idc_version"]:
        raise DataError(
            f"IDC index version {actual} does not match frozen selection {source['idc_version']}"
        )
    return client


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _has_dicom_files(directory: Path) -> bool:
    return directory.is_dir() and any(p.is_file() for p in directory.iterdir())


def _download(client, series_uid: str, dicom_root: Path, dir_template: str) -> None:
    dicom_root.mkdir(parents=True, exist_ok=True)
    try:
        client.download_dicom_series(
            seriesInstanceUID=series_uid,
            downloadDir=str(dicom_root),
            dirTemplate=dir_template,
        )
    except NoduleAIError:
        raise
    except Exception as exc:
        raise ToolError(f"IDC download failed for series {series_uid}: {exc}") from exc


def _cross_check(selection: dict, metadata: dict) -> None:
    for key in ("study_instance_uid", "series_instance_uid"):
        if selection[key] != metadata[key]:
            raise DataError(
                f"downloaded {key} {metadata[key]} does not match selection {selection[key]}"
            )
    expected_frame = selection.get("frame_of_reference_uid")
    if expected_frame is not None and expected_frame != metadata["frame_of_reference_uid"]:
        raise DataError(
            "downloaded FrameOfReferenceUID "
            f"{metadata['frame_of_reference_uid']} does not match selection {expected_frame}"
        )
    if metadata["num_slices"] != selection["expected_instance_count"]:
        raise DataError(
            f"downloaded {metadata['num_slices']} slice(s) but selection expects "
            f"{selection['expected_instance_count']}"
        )


def _build_manifest(selection: dict, metadata: dict, series_dir: Path, out_dir: Path) -> dict:
    files = sorted(p for p in series_dir.iterdir() if p.is_file())
    checksums = [{"name": path.name, "sha256": _sha256(path)} for path in files]
    combined = hashlib.sha256(
        "".join(entry["sha256"] for entry in checksums).encode("ascii")
    ).hexdigest()
    geometry_keys = (
        "rows",
        "columns",
        "pixel_spacing_mm",
        "image_orientation_patient",
        "slice_normal_lps",
        "median_slice_spacing_mm",
        "slice_spacing_mm",
        "slice_thickness_mm",
        "slice_positions_mm",
        "image_position_patient",
    )
    return {
        "schema_version": SCHEMA_VERSION,
        "study_instance_uid": metadata["study_instance_uid"],
        "series_instance_uid": metadata["series_instance_uid"],
        "frame_of_reference_uid": metadata["frame_of_reference_uid"],
        "source": selection["source"],
        "num_slices": metadata["num_slices"],
        "dicom_directory": Path(os.path.relpath(series_dir, out_dir)).as_posix(),
        "files": checksums,
        "series_checksum": combined,
        "sop_instance_uids": metadata["sop_instance_uids"],
        "geometry": {key: metadata[key] for key in geometry_keys},
    }


def _write_json_atomic(path: Path, payload: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    text = json.dumps(payload, indent=2, sort_keys=True, allow_nan=False) + "\n"
    temporary.write_text(text, encoding="utf-8")
    os.replace(temporary, path)


def fetch_case(selection: dict, out_dir: Path, *, client=None, dir_template: str = DEFAULT_DIR_TEMPLATE) -> dict:
    """Download and validate exactly the series named by a frozen selection.

    Returns the validated case manifest and writes it to
    ``out_dir/case-manifest.json`` only after every check passes.
    """
    validate_selection(selection)
    out_dir = Path(out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    # A failed run must never leave a prior success manifest that a consumer
    # could mistake for this run's result.
    manifest_path = out_dir / MANIFEST_NAME
    if manifest_path.exists():
        manifest_path.unlink()
    dicom_root = out_dir / DICOM_DIRNAME
    series_uid = selection["series_instance_uid"]
    series_dir = dicom_root / dir_template.replace("%SeriesInstanceUID", series_uid)

    if not _has_dicom_files(series_dir):
        if client is None:
            client = _build_client(selection)
        _download(client, series_uid, dicom_root, dir_template)

    metadata = read_series_metadata(series_dir)
    _cross_check(selection, metadata)
    manifest = _build_manifest(selection, metadata, series_dir, out_dir)
    _write_json_atomic(out_dir / MANIFEST_NAME, manifest)
    return manifest


def select_case_document(annotations_dir: Path, out: Path) -> dict:
    """Freeze a selection from the pylidc shortlist; XML comes before any CT download."""
    import pylidc as pl

    from .dicom_geometry import MAX_SLICE_THICKNESS_MM
    from .reference_nodules import ensure_annotations, select_case

    annotations_dir = Path(annotations_dir)
    _, archive = ensure_annotations(annotations_dir)
    scans = [
        scan
        for scan in pl.query(pl.Scan).all()
        if scan.slice_thickness and float(scan.slice_thickness) <= MAX_SLICE_THICKNESS_MM
    ]
    selection = select_case(_new_idc_client(), scans, annotations_dir)
    selection["artifacts"]["annotations_archive"] = archive
    _write_json_atomic(Path(out), selection)
    return selection


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="nodule_ai.fetch_case",
        description="Freeze a selection or download one frozen LIDC-IDRI CT series (LC1-02).",
    )
    parser.add_argument("--select", action="store_true", help="freeze a selection instead of fetching")
    parser.add_argument("--selection", type=Path, help="frozen selection JSON (fetch mode)")
    parser.add_argument("--annotations-dir", type=Path, help="TCIA XML annotations directory (select mode)")
    parser.add_argument("--out", required=True, type=Path, help="output file (select) or directory (fetch)")
    args = parser.parse_args(argv)

    if args.select and not args.annotations_dir:
        parser.error("--select requires --annotations-dir")
    if not args.select and not args.selection:
        parser.error("--selection is required unless --select is used")

    try:
        if args.select:
            selection = select_case_document(args.annotations_dir, args.out)
            summary = {
                "series_instance_uid": selection["series_instance_uid"],
                "target_reference_id": selection["target_reference_id"],
                "clusters": len(selection["clusters"]),
                "selection": str(args.out),
            }
        else:
            selection = _load_selection(args.selection)
            manifest = fetch_case(selection, args.out)
            summary = {
                "series_instance_uid": manifest["series_instance_uid"],
                "num_slices": manifest["num_slices"],
                "manifest": str(Path(args.out) / MANIFEST_NAME),
            }
    except NoduleAIError as exc:
        print(f"fetch_case: {exc}", file=sys.stderr)
        return exc.exit_code

    print(json.dumps(summary, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
