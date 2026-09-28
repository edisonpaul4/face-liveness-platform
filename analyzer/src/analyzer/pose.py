"""Pose de la cabeza y geometría del rostro por frame.

Sólo mide. Que un ángulo sea el que se pidió, o que un movimiento sea
sospechoso, lo decide Go (CLAUDE.md §3).
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

#: Landmarks de MediaPipe que corresponden a los cinco puntos canónicos de
#: alineación facial: iris izquierdo, iris derecho, punta de nariz y las dos
#: comisuras de la boca.
CANONICAL_LANDMARKS = (468, 473, 1, 61, 291)

#: De cuántos en cuántos se guardan los landmarks en el estado caliente.
#: 478 puntos por frame durante una sesión entera es mucha memoria para lo que
#: aporta: uno de cada cuatro cubre la cara igual de bien para ajustar un
#: modelo 2D.
LANDMARK_STRIDE = 4


@dataclass(frozen=True, slots=True)
class HeadPose:
    """Orientación de la cabeza en grados.

    Convención, verificada contra verdad conocida con el renderizador
    sintético de los tests:

    * yaw   > 0 mira a la derecha de la imagen.
    * pitch > 0 levanta la barbilla.
    * roll  > 0 inclina la cabeza hacia su hombro derecho.
    """

    yaw: float
    pitch: float
    roll: float

    def angle(self, axis: str) -> float:
        """Devuelve el ángulo del eje pedido."""
        if axis == "yaw":
            return self.yaw
        if axis == "pitch":
            return self.pitch
        if axis == "roll":
            return self.roll
        raise ValueError(f"eje desconocido: {axis!r}")


def from_transform(matrix: np.ndarray | None) -> HeadPose | None:
    """Extrae la pose de la matriz de transformación facial 4x4.

    Devuelve None si no hay matriz: sin ella no se inventa una pose.
    """
    if matrix is None:
        return None

    rotation = np.asarray(matrix, dtype=np.float64)[:3, :3]

    # Descomposición ZYX. El caso degenerado (cabeza mirando al cenit) no se
    # da en una sesión de liveness, pero se contempla igualmente.
    sy = float(np.hypot(rotation[0, 0], rotation[1, 0]))
    if sy > 1e-6:
        pitch = np.arctan2(rotation[2, 1], rotation[2, 2])
        yaw = np.arctan2(-rotation[2, 0], sy)
        roll = np.arctan2(rotation[1, 0], rotation[0, 0])
    else:
        pitch = np.arctan2(-rotation[1, 2], rotation[1, 1])
        yaw = np.arctan2(-rotation[2, 0], sy)
        roll = 0.0

    return HeadPose(
        yaw=float(np.degrees(yaw)),
        pitch=float(np.degrees(pitch)),
        roll=float(np.degrees(roll)),
    )


def face_scale(width: float, height: float) -> float:
    """Escala del rostro: proporción lineal del bbox respecto al encuadre.

    Se usa la media geométrica de ancho y alto, no el área, para que crezca
    linealmente al acercarse: al doble de cerca, el doble de escala.
    """
    return float(np.sqrt(max(0.0, width) * max(0.0, height)))


def compact(landmarks: np.ndarray | None) -> np.ndarray | None:
    """Reduce los landmarks a lo que hace falta guardar: (N, 2) en float32.

    La coordenada z de MediaPipe es relativa y sin escala métrica fiable; el
    análisis de paralaje trabaja sobre lo que se ve en la imagen, que es
    justamente lo que un atacante tiene que falsificar.
    """
    if landmarks is None or len(landmarks) == 0:
        return None
    return np.ascontiguousarray(landmarks[::LANDMARK_STRIDE, :2], dtype=np.float32)


def canonical_points(landmarks: np.ndarray | None) -> np.ndarray | None:
    """Los cinco puntos de alineación, en coordenadas normalizadas."""
    if landmarks is None or len(landmarks) <= max(CANONICAL_LANDMARKS):
        return None
    return np.array([landmarks[i][:2] for i in CANONICAL_LANDMARKS], dtype=np.float32)
