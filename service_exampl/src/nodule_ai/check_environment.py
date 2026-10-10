"""LC1-03 environment verifier: package imports and trained-checkpoint loading.

This produces *environment evidence*, not the LC1-05 ``result.json``. It never
runs a forward pass, never produces candidates and never measures inference
memory. ``imports`` mode verifies package/config identity only; ``load`` mode
additionally loads the trained checkpoint on an explicitly requested device.
"""

from __future__ import annotations

import argparse
import ctypes
import hashlib
import importlib.metadata
import json
import os
import platform
import re
import sys
import time
from pathlib import Path
from types import SimpleNamespace

SCHEMA_VERSION = "1"
MODE_IMPORTS = "imports"
MODE_LOAD = "load"
EVIDENCE_NAME = "environment.json"
CHECKPOINT_PATH = "models/model.pt"
METADATA_PATH = "configs/metadata.json"
CONFIG_PATH = "configs/inference.json"

REQUIRED_PACKAGES = {
    "monai": "monai",
    "torch": "torch",
    "torchvision": "torchvision",
    "SimpleITK": "SimpleITK",
    "numpy": "numpy",
    "nibabel": "nibabel",
    "ignite": "pytorch-ignite",
    "tensorboard": "tensorboard",
}
_REVISION_RE = re.compile(r"^[0-9a-f]{40}$")


class CheckError(RuntimeError):
    """Runtime verification failure with a stable evidence error code."""

    def __init__(self, code: str, message: str) -> None:
        super().__init__(message)
        self.code = code


def _torch_available() -> bool:
    import importlib.util

    return importlib.util.find_spec("torch") is not None


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with Path(path).open("rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _md5(path: Path) -> str:
    digest = hashlib.md5()
    with Path(path).open("rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def load_manifest(path: Path) -> dict:
    path = Path(path)
    if not path.is_file():
        raise CheckError("manifest_missing", f"manifest file not found: {path}")
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        raise CheckError("manifest_invalid", f"manifest is not valid JSON: {exc}") from exc


def verify_bundle(bundle_dir: Path, manifest: dict, *, require_weights: bool) -> None:
    """Verify bundle identity and content hashes before any deserialization.

    Raises ValueError for any identity/hash mismatch. A missing checkpoint is
    tolerated only when ``require_weights`` is false (imports mode).
    """
    bundle_dir = Path(bundle_dir)
    if not bundle_dir.is_dir():
        raise ValueError(f"bundle directory does not exist: {bundle_dir}")
    revision = manifest.get("source_revision")
    if not isinstance(revision, str) or not _REVISION_RE.match(revision):
        raise ValueError(f"manifest source_revision is not an immutable revision: {revision!r}")
    version = manifest.get("bundle_version")
    if not isinstance(version, str) or not version:
        raise ValueError("manifest bundle_version is missing")

    metadata_path = bundle_dir / METADATA_PATH
    if not metadata_path.is_file():
        raise ValueError(f"bundle metadata is missing: {metadata_path}")
    try:
        metadata_version = json.loads(metadata_path.read_text(encoding="utf-8")).get("version")
    except json.JSONDecodeError as exc:
        raise ValueError(f"bundle metadata is not valid JSON: {exc}") from exc
    if metadata_version != version:
        raise ValueError(
            f"bundle metadata version {metadata_version!r} does not match manifest {version!r}"
        )

    files = manifest.get("files")
    if not isinstance(files, list) or not files:
        raise ValueError("manifest files list is missing or empty")
    for entry in files:
        rel = entry.get("path")
        if not isinstance(rel, str) or not rel:
            raise ValueError(f"manifest file entry has no path: {entry!r}")
        path = bundle_dir / rel
        if not path.is_file():
            if rel == CHECKPOINT_PATH and not require_weights:
                continue
            raise ValueError(f"bundle file is missing: {rel}")
        if entry.get("algorithm") != "sha256":
            raise ValueError(f"unsupported hash algorithm for {rel}: {entry.get('algorithm')!r}")
        actual = _sha256(path)
        if actual != entry.get("digest"):
            raise ValueError(f"bundle file hash mismatch for {rel}")
        upstream = entry.get("expected_upstream")
        if upstream:
            algorithm = upstream.get("algorithm")
            if algorithm == "md5":
                if _md5(path) != upstream.get("digest"):
                    raise ValueError(f"upstream md5 mismatch for {rel}")
            else:
                raise ValueError(f"unsupported upstream hash algorithm for {rel}")

    if require_weights and not any(e.get("path") == CHECKPOINT_PATH for e in files):
        raise ValueError("manifest does not declare the checkpoint artifact")


def collect_packages() -> dict:
    """Import the AI stack and record observed versions (no CUDA, no model)."""
    packages = {}
    for module_name, distribution in REQUIRED_PACKAGES.items():
        try:
            module = __import__(module_name)
        except Exception as exc:  # import errors are the finding, not a crash
            raise CheckError("import_failed", f"cannot import {module_name}: {exc}") from exc
        packages[module_name] = {
            "version": getattr(module, "__version__", None),
            "distribution": distribution,
            "distribution_version": importlib.metadata.version(distribution),
            "file": getattr(module, "__file__", None),
        }
    return packages


def _total_ram_bytes() -> int | None:
    try:
        if sys.platform == "win32":
            class MemoryStatus(ctypes.Structure):
                _fields_ = [
                    ("dwLength", ctypes.c_ulong),
                    ("dwMemoryLoad", ctypes.c_ulong),
                    ("ullTotalPhys", ctypes.c_ulonglong),
                    ("ullAvailPhys", ctypes.c_ulonglong),
                    ("ullTotalPageFile", ctypes.c_ulonglong),
                    ("ullAvailPageFile", ctypes.c_ulonglong),
                    ("ullTotalVirtual", ctypes.c_ulonglong),
                    ("ullAvailVirtual", ctypes.c_ulonglong),
                    ("ullAvailExtendedVirtual", ctypes.c_ulonglong),
                ]

            status = MemoryStatus()
            status.dwLength = ctypes.sizeof(status)
            if ctypes.windll.kernel32.GlobalMemoryStatusEx(ctypes.byref(status)):
                return int(status.ullTotalPhys)
        else:
            with open("/proc/meminfo", encoding="ascii") as handle:
                for line in handle:
                    if line.startswith("MemTotal:"):
                        return int(line.split()[1]) * 1024
    except Exception:
        return None
    return None


def collect_hardware(device: str) -> dict:
    """Measure the real device/host for a load run; never reports zeros as data."""
    import torch

    hardware = {
        "cpu": platform.processor() or platform.machine(),
        "cpu_count": os.cpu_count(),
        "ram_bytes": _total_ram_bytes(),
        "torch_cuda_build": torch.version.cuda,
    }
    if device.startswith("cuda"):
        index = int(device.split(":")[1]) if ":" in device else 0
        properties = torch.cuda.get_device_properties(index)
        hardware["gpu"] = {
            "name": torch.cuda.get_device_name(index),
            "total_memory_bytes": int(properties.total_memory),
            "capability": list(torch.cuda.get_device_capability(index)),
        }
    else:
        hardware["gpu"] = None
    return hardware


def _verify_device(device: str) -> None:
    """Reject an unusable requested device; never fall back to CPU."""
    import torch

    if not device.startswith("cuda"):
        return
    if not torch.cuda.is_available():
        raise CheckError("device_unavailable", f"requested device {device} is not available")
    index = int(device.split(":")[1]) if ":" in device else 0
    if index >= torch.cuda.device_count():
        raise CheckError("device_unavailable", f"requested device {device} does not exist")


def _load_checkpoint(bundle_dir: Path, device: str) -> tuple[float, dict]:
    """Initialise the bundle network and load the trained checkpoint strictly."""
    import torch
    from monai.bundle import ConfigParser

    _verify_device(device)

    config_file = bundle_dir / CONFIG_PATH
    if not config_file.is_file():
        raise ValueError(f"bundle inference config is missing: {config_file}")

    parser = ConfigParser(config_file=str(config_file))
    parser["bundle_root"] = str(bundle_dir)
    parser["device"] = torch.device(device)
    try:
        network = parser.get_parsed_content("network")
        loader = parser.get_parsed_content("checkpointloader")
    except Exception as exc:
        raise CheckError("model_load_failed", f"cannot initialise the bundle network: {exc}") from exc
    # Explicit device; never the bundle's auto-device fallback.
    loader.map_location = device
    stub = SimpleNamespace(state=SimpleNamespace(max_epochs=0))
    started = time.perf_counter()
    try:
        loader(stub)
    except Exception as exc:
        raise CheckError("model_load_failed", f"checkpoint load failed: {exc}") from exc
    elapsed = time.perf_counter() - started
    parameters = sum(p.numel() for p in network.parameters())
    return elapsed, {"parameters": int(parameters), "device": device}


def check_environment(mode: str, bundle_dir: Path, manifest: dict, device: str | None) -> dict:
    """Produce the environment evidence document for one requested mode."""
    bundle_dir = Path(bundle_dir)
    if mode not in (MODE_IMPORTS, MODE_LOAD):
        raise CheckError("invalid_mode", f"unsupported mode: {mode!r}")
    require_weights = mode == MODE_LOAD
    if require_weights and not device:
        raise CheckError("invalid_mode", "load mode requires an explicit device")

    verify_bundle(bundle_dir, manifest, require_weights=require_weights)

    parameters = None
    packages = collect_packages()
    device_report = {"requested": device, "actual": "not_run"}
    hardware = "not_run"
    model_load = {"status": "not_run", "load_seconds": None}
    configuration = {"precision": None, "overrides": {"bundle_root": str(bundle_dir)}}

    if require_weights:
        _verify_device(device)
        hardware = collect_hardware(device)
        elapsed, details = _load_checkpoint(bundle_dir, device)
        parameters = details["parameters"]
        device_report["actual"] = details["device"]
        model_load = {"status": "loaded", "load_seconds": elapsed}
        configuration["precision"] = "amp=true (bundle inference config; not exercised)"

    checkpoint = next((e for e in manifest["files"] if e["path"] == CHECKPOINT_PATH), None)
    return {
        "schema_version": SCHEMA_VERSION,
        "mode": mode,
        "status": "passed",
        "python": {"version": platform.python_version(), "executable": sys.executable},
        "platform": {
            "platform": platform.platform(),
            "system": platform.system(),
            "release": platform.release(),
            "machine": platform.machine(),
        },
        "packages": packages,
        "bundle": {
            "name": manifest.get("bundle_name"),
            "version": manifest.get("bundle_version"),
            "revision": manifest.get("source_revision"),
            "license": manifest.get("license"),
            "checkpoint_sha256": checkpoint["digest"] if checkpoint else None,
            "parameter_count": parameters,
        },
        "device": device_report,
        "hardware": hardware,
        "configuration": configuration,
        "model_load": model_load,
        "error": None,
    }


def _failed_evidence(mode: str, bundle_dir: Path, manifest: dict, device: str | None, code: str, message: str) -> dict:
    return {
        "schema_version": SCHEMA_VERSION,
        "mode": mode,
        "status": "failed",
        "python": {"version": platform.python_version(), "executable": sys.executable},
        "platform": {"platform": platform.platform()},
        "packages": {},
        "bundle": {
            "name": manifest.get("bundle_name"),
            "version": manifest.get("bundle_version"),
            "revision": manifest.get("source_revision"),
        },
        "device": {"requested": device, "actual": "not_run"},
        "hardware": "not_run",
        "configuration": {"bundle_root": str(bundle_dir)},
        "model_load": {"status": "not_run", "load_seconds": None},
        "error": {"code": code, "message": message},
    }


def write_evidence(evidence: dict, out: Path) -> None:
    """Write evidence atomically, replacing any previous run's file."""
    out = Path(out)
    out.parent.mkdir(parents=True, exist_ok=True)
    text = json.dumps(evidence, indent=2, sort_keys=True, allow_nan=False) + "\n"
    temporary = out.with_name(out.name + ".tmp")
    temporary.write_text(text, encoding="utf-8")
    os.replace(temporary, out)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="nodule_ai.check_environment",
        description="Verify the LC1-03 MONAI environment and, on a suitable machine, load the checkpoint.",
    )
    parser.add_argument("--mode", required=True, choices=[MODE_IMPORTS, MODE_LOAD])
    parser.add_argument("--bundle-dir", required=True, type=Path)
    parser.add_argument("--manifest", required=True, type=Path)
    parser.add_argument("--device", default=None, help="required for --mode load: cpu or cuda:N")
    parser.add_argument("--out", required=True, type=Path, help="environment evidence JSON path")
    args = parser.parse_args(argv)
    if args.mode == MODE_LOAD and not args.device:
        parser.error("--mode load requires --device")

    # A failed run must never leave the previous run's evidence at --out.
    out = Path(args.out)
    if out.exists():
        out.unlink()

    manifest = {}
    try:
        manifest = load_manifest(args.manifest)
        evidence = check_environment(args.mode, args.bundle_dir, manifest, args.device)
    except ValueError as exc:
        evidence = _failed_evidence(args.mode, args.bundle_dir, manifest, args.device, "verification_failed", str(exc))
    except CheckError as exc:
        evidence = _failed_evidence(args.mode, args.bundle_dir, manifest, args.device, exc.code, str(exc))
    except Exception as exc:  # unexpected: still record the failed run
        evidence = _failed_evidence(args.mode, args.bundle_dir, manifest, args.device, "unexpected_error", str(exc))

    write_evidence(evidence, out)
    if evidence["status"] == "passed":
        print(json.dumps({"mode": evidence["mode"], "status": "passed", "out": str(out)}, sort_keys=True))
        return 0
    print(f"check_environment: {evidence['error']['code']}: {evidence['error']['message']}", file=sys.stderr)
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
