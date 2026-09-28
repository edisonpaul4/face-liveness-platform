"""Logging estructurado del analyzer.

Nunca registra payloads de frames ni datos biométricos: sólo identificadores
de sesión, contadores y latencias.
"""

from __future__ import annotations

import logging
import os
import sys


def configure(level: str = "INFO") -> None:
    """Configura el logging del proceso."""
    logging.basicConfig(
        level=getattr(logging, level.upper(), logging.INFO),
        stream=sys.stdout,
        format=f"%(asctime)s %(levelname)s [pid {os.getpid()}] %(name)s %(message)s",
        force=True,
    )
    # MediaPipe y absl son ruidosos por debajo de WARNING.
    logging.getLogger("absl").setLevel(logging.WARNING)
