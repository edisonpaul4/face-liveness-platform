"""Decodificación de frames.

Un frame ilegible no es un error de sesión: se descarta y se cuenta. El
cliente es hostil por definición y puede mandar cualquier cosa.
"""

from __future__ import annotations

import cv2
import numpy as np

from analyzer import wire


class DecodeError(ValueError):
    """El payload no es una imagen que se pueda decodificar."""


def decode(payload: bytes, encoding: int, *, width: int = 0, height: int = 0) -> np.ndarray:
    """Decodifica el payload de un frame a BGR.

    Args:
        payload: bytes tal y como llegaron por el bus.
        encoding: código de FrameEncoding.
        width, height: sólo para RGB24, que no lleva dimensiones dentro.

    Returns:
        Array HxWx3 en BGR, uint8.
    """
    if not payload:
        raise DecodeError("payload vacío")

    if encoding in (wire.ENCODING_JPEG, wire.ENCODING_WEBP, wire.ENCODING_UNSPECIFIED):
        buf = np.frombuffer(payload, dtype=np.uint8)
        img = cv2.imdecode(buf, cv2.IMREAD_COLOR)
        if img is None:
            raise DecodeError(f"no se pudo decodificar {len(payload)} bytes de imagen")
        return img

    if encoding == wire.ENCODING_RGB24:
        if width <= 0 or height <= 0:
            raise DecodeError("rgb24 exige dimensiones conocidas")
        expected = width * height * 3
        if len(payload) != expected:
            raise DecodeError(f"rgb24: {len(payload)} bytes, se esperaban {expected}")
        rgb = np.frombuffer(payload, dtype=np.uint8).reshape((height, width, 3))
        return cv2.cvtColor(rgb, cv2.COLOR_RGB2BGR)

    raise DecodeError(f"codificación no soportada: {encoding}")


def to_gray(bgr: np.ndarray) -> np.ndarray:
    """Devuelve la luminancia del frame."""
    return cv2.cvtColor(bgr, cv2.COLOR_BGR2GRAY)
