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


class ToolError(NoduleAIError):
    """Network, filesystem or dependency failure while running a tool."""

    exit_code = 1
