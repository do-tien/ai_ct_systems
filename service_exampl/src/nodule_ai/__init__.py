"""Packaging for the nodule-reader AI worker.

This distribution ships metadata only for now: volume reconstruction, nodule
detection, and model integration are not implemented yet.
"""

from importlib.metadata import PackageNotFoundError, version

try:
    __version__ = version("nodule-ai")
except PackageNotFoundError:
    __version__ = "not-installed"
