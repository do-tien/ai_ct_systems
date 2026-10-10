"""CLI-facing error taxonomy for the nodule_ai data tools.

The exit codes are the tools contract for LC1-02 and are distinct from the
inference CLI contract in LC1-05 (0 success, 2 unsuitable input, 3 model/OOM).
"""

from __future__ import annotations


class NoduleAIError(Exception):
    """Base class for errors a CLI should report instead of a traceback."""

    exit_code = 1


class DataError(NoduleAIError):
    """Invalid selection, metadata, geometry, annotation or CLI argument."""

    exit_code = 2


class GeometryError(DataError):
    """Geometry/volume failure carrying a stable machine-readable code.

    ``code`` is part of the LC1-04 contract consumed by LC1-05/06 evidence; the
    exit code stays 2 so existing callers keep working.
    """

    def __init__(self, message: str, *, code: str) -> None:
        super().__init__(message)
        self.code = code


class ToolError(NoduleAIError):
    """Network, filesystem or dependency failure while running a tool."""

    exit_code = 1
