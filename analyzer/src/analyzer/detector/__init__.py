"""Detección de rostro.

El detector es intercambiable a propósito. Hay dos implementaciones reales:

* `mediapipe`  — MediaPipe Face Landmarker. Es la que pide el diseño.
* `onnx`       — ONNX Runtime en CPU con YuNet.

Nota sobre las dos: MediaPipe Tasks trae su propio motor de inferencia
(TFLite con XNNPACK) y NO se puede enrutar por ONNX Runtime. Son dos caminos
alternativos, no uno encima del otro. El de ONNX Runtime existe porque es
donde entrarán los modelos propios de PAD, que sí se exportarán a ONNX.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Protocol

import numpy as np


@dataclass(frozen=True, slots=True)
class FaceBox:
    """Caja de rostro normalizada [0,1] respecto al frame."""

    x: float
    y: float
    w: float
    h: float
    confidence: float

    @property
    def area(self) -> float:
        """Fracción del encuadre que ocupa el rostro."""
        return max(0.0, self.w) * max(0.0, self.h)

    def to_dict(self) -> dict[str, float]:
        return {
            "x": round(self.x, 6),
            "y": round(self.y, 6),
            "w": round(self.w, 6),
            "h": round(self.h, 6),
            "confidence": round(self.confidence, 4),
        }


@dataclass(frozen=True, slots=True)
class Detection:
    """Lo que el detector vio en un frame.

    `landmarks` y `transform` son del rostro PRINCIPAL, y sólo los rellenan
    los detectores que los dan (MediaPipe). El análisis de pose y de paralaje
    va sobre la persona que está delante; si hay más caras en el encuadre, eso
    ya es un dato de calidad, no un sujeto a medir.
    """

    faces: tuple[FaceBox, ...]
    #: Landmarks normalizados del rostro principal, (N, 3) en float32.
    landmarks: np.ndarray | None = None
    #: Matriz 4x4 de transformación facial del rostro principal.
    transform: np.ndarray | None = None

    @property
    def has_landmarks(self) -> bool:
        return self.landmarks is not None and len(self.landmarks) > 0

    @property
    def face_detected(self) -> bool:
        return len(self.faces) > 0

    @property
    def primary(self) -> FaceBox | None:
        """El rostro más grande: si hay varios, el que está delante."""
        if not self.faces:
            return None
        return max(self.faces, key=lambda f: f.area)


EMPTY = Detection(faces=())


class FaceDetector(Protocol):
    """Detecta rostros en un frame BGR."""

    name: str

    def detect(self, bgr: np.ndarray) -> Detection:
        """Devuelve los rostros del frame."""
        ...

    def close(self) -> None:
        """Libera los recursos del detector."""
        ...


class DetectorError(RuntimeError):
    """No se pudo preparar el detector."""


def create(
    backend: str, model_path: str, *, max_faces: int = 3, min_confidence: float = 0.6
) -> FaceDetector:
    """Construye el detector indicado.

    Args:
        backend: "mediapipe" u "onnx".
        model_path: fichero del modelo (.task para MediaPipe, .onnx para ORT).
    """
    if backend == "mediapipe":
        from analyzer.detector.mediapipe_backend import MediaPipeLandmarker

        return MediaPipeLandmarker(model_path, max_faces=max_faces)
    if backend == "onnx":
        from analyzer.detector.onnx_backend import OnnxYuNetDetector

        return OnnxYuNetDetector(model_path, max_faces=max_faces, min_confidence=min_confidence)
    raise DetectorError(f"backend de detección desconocido: {backend!r}")
