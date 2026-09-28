"""Métricas de calidad de captura.

Miden si un frame SIRVE para medir, no si la sesión es real. La diferencia
importa: con ellas Go puede declarar una sesión no concluyente en vez de
acusar a nadie (CLAUDE.md §3).
"""

from __future__ import annotations

from dataclasses import dataclass

import cv2
import numpy as np

#: Un píxel se considera quemado a partir de aquí.
HIGHLIGHT_THRESHOLD = 250


@dataclass(frozen=True, slots=True)
class QualityMetrics:
    """Calidad de un frame."""

    #: Varianza del laplaciano. Sin techo: una escena con mucho detalle da
    #: cientos, una imagen desenfocada da unidades.
    sharpness: float
    #: Luminancia media en [0,1].
    brightness: float
    #: Fracción de píxeles quemados en [0,1]. Un valor alto delata un foco
    #: directo o una pantalla apuntando a la cámara.
    highlight_saturation: float
    #: Desviación típica de la luminancia en [0,1].
    contrast: float


def measure(gray: np.ndarray) -> QualityMetrics:
    """Mide la calidad sobre la luminancia del frame.

    Args:
        gray: imagen de un canal, uint8.
    """
    if gray.ndim != 2:
        raise ValueError(f"se esperaba una imagen de un canal, llegó {gray.ndim}D")

    # Varianza del laplaciano: el estimador de nitidez de toda la vida. Barato
    # y suficiente para descartar frames movidos o desenfocados.
    laplacian = cv2.Laplacian(gray, cv2.CV_64F)
    sharpness = float(laplacian.var())

    mean, stddev = cv2.meanStdDev(gray)
    brightness = float(mean[0][0]) / 255.0
    contrast = float(stddev[0][0]) / 255.0

    burnt = int(np.count_nonzero(gray >= HIGHLIGHT_THRESHOLD))
    highlight_saturation = burnt / float(gray.size)

    return QualityMetrics(
        sharpness=sharpness,
        brightness=brightness,
        highlight_saturation=highlight_saturation,
        contrast=contrast,
    )
