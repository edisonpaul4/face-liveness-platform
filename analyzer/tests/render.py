"""Renderizador de material de prueba: una cabeza con volumen y una foto plana.

Todo sintético. En este repositorio no se versionan rostros reales
(CLAUDE.md §9), y para lo que hay que separar aquí —volumen contra plano— un
modelo geométrico es más útil que un vídeo: la verdad del terreno es exacta.

Las dos escenas están hechas para ser indistinguibles salvo en lo único que
importa:

* `render_head_3d(yaw)` proyecta cada rasgo según SU profundidad. La nariz
  sobresale y las orejas quedan detrás, así que al girar se desplazan unos
  respecto a otros. Eso es el paralaje.
* `render_photo_rotated(frontal, yaw)` toma el MISMO rostro frontal y lo gira
  como una cartulina. Todo el movimiento lo explica una homografía, porque es
  literalmente un plano.
"""

from __future__ import annotations

import cv2
import numpy as np

#: Puntos del rostro en 3D, en unidades de media anchura facial. La coordenada
#: z es la profundidad: positivo hacia la cámara.
FACE_3D: dict[str, tuple[float, float, float]] = {
    "nose_tip": (0.00, 0.05, 0.62),
    "eye_l": (-0.33, -0.15, 0.30),
    "eye_r": (0.33, -0.15, 0.30),
    "brow_l": (-0.35, -0.35, 0.28),
    "brow_r": (0.35, -0.35, 0.28),
    "mouth": (0.00, 0.42, 0.35),
    "chin": (0.00, 0.95, 0.10),
    "ear_l": (-0.95, -0.05, -0.35),
    "ear_r": (0.95, -0.05, -0.35),
    "crown": (0.00, -1.05, 0.15),
}

#: Distancia de la cámara, en unidades faciales, y distancia focal.
CAMERA_DISTANCE = 6.0
FOCAL = 900.0

_BACKGROUND = 200
_SKIN = (185, 160, 140)
_HAIR = (60, 45, 35)


def _rotation_y(yaw_deg: float) -> np.ndarray:
    a = np.radians(yaw_deg)
    return np.array([[np.cos(a), 0, np.sin(a)], [0, 1, 0], [-np.sin(a), 0, np.cos(a)]])


def _rotation_x(pitch_deg: float) -> np.ndarray:
    a = np.radians(pitch_deg)
    return np.array([[1, 0, 0], [0, np.cos(a), -np.sin(a)], [0, np.sin(a), np.cos(a)]])


def _project(point: tuple[float, float, float], yaw_deg: float, cx: int, cy: int,
             scale: float, distance: float = CAMERA_DISTANCE,
             pitch_deg: float = 0.0) -> tuple[int, int]:
    """Proyecta un punto 3D girado, con perspectiva."""
    rotation = _rotation_x(pitch_deg) @ _rotation_y(yaw_deg)
    x, y, z = rotation @ np.array(point, dtype=np.float64)
    depth = distance - z
    return (
        int(round(cx + FOCAL * scale * x / depth)),
        int(round(cy + FOCAL * scale * y / depth)),
    )


def _textured_background(width: int, height: int) -> np.ndarray:
    """Fondo con algo de grano: un plano liso no se parece a nada real."""
    img = np.full((height, width, 3), _BACKGROUND, np.uint8)
    noise = np.random.default_rng(7).integers(0, 18, (height, width, 3), dtype=np.uint8)
    return cv2.subtract(img, noise)


def render_head_3d(yaw_deg: float = 0.0, *, width: int = 640, height: int = 480,
                   scale: float = 1.0, pitch_deg: float = 0.0,
                   gaze_px: tuple[int, int] = (0, 0)) -> np.ndarray:
    """Cabeza con volumen girada `yaw_deg` en horizontal y `pitch_deg` en vertical.

    `gaze_px` desplaza las pupilas dentro del ojo, en píxeles de imagen: es una
    mirada, no un giro de cabeza.
    """
    img = _textured_background(width, height)
    cx, cy = width // 2, height // 2
    p = {name: _project(point, yaw_deg, cx, cy, scale, pitch_deg=pitch_deg)
         for name, point in FACE_3D.items()}

    left, right = min(p["ear_l"][0], p["ear_r"][0]), max(p["ear_l"][0], p["ear_r"][0])
    top, bottom = p["crown"][1], p["chin"][1]
    ecx, ecy = (left + right) // 2, (top + bottom) // 2
    rx, ry = max(8, (right - left) // 2), max(8, (bottom - top) // 2)

    cv2.ellipse(img, (ecx, ecy), (rx, ry), 0, 0, 360, _SKIN, -1)
    cv2.ellipse(img, (ecx, top + (bottom - top) // 6), (rx, max(6, (bottom - top) // 5)),
                0, 180, 360, _HAIR, -1)

    for eye, brow in (("eye_l", "brow_l"), ("eye_r", "brow_r")):
        # Ojo con sitio de sobra para que la pupila se mueva dentro: con la
        # esclerótica justa, una mirada hacia abajo deja la pupila sobre la
        # piel y el detector deja de encontrar iris.
        cv2.ellipse(img, p[eye], (18, 14), 0, 0, 360, (250, 250, 250), -1)
        pupil = (p[eye][0] + gaze_px[0], p[eye][1] + gaze_px[1])
        cv2.circle(img, pupil, 6, (40, 30, 25), -1)
        cv2.line(img, (p[brow][0] - 14, p[brow][1]), (p[brow][0] + 14, p[brow][1]), (70, 55, 45), 3)

    cv2.ellipse(img, p["nose_tip"], (9, 18), 0, 0, 360, (165, 140, 120), -1)
    cv2.ellipse(img, p["mouth"], (28, 12), 0, 0, 180, (120, 70, 70), -1)
    return cv2.GaussianBlur(img, (5, 5), 0)


def render_photo_rotated(frontal: np.ndarray, yaw_deg: float, *,
                         distance: float = CAMERA_DISTANCE) -> np.ndarray:
    """El mismo rostro frontal, girado como una foto impresa.

    Es una homografía pura: exactamente lo que ve una cámara cuando alguien
    gira una cartulina delante de ella.

    `distance` es lo cerca que se sostiene la foto. Cuanto más cerca, más
    perspectiva y más se parece a un giro de cabeza de verdad: es el ataque
    en su versión difícil.
    """
    height, width = frontal.shape[:2]
    half_w, half_h = width / 2.0, height / 2.0
    margin = 0.9

    source, destination = [], []
    for sx, sy in ((-1, -1), (1, -1), (1, 1), (-1, 1)):
        source.append([half_w + sx * half_w * margin, half_h + sy * half_h * margin])

        x, z = sx * np.cos(np.radians(yaw_deg)), -sx * np.sin(np.radians(yaw_deg))
        depth = distance - z
        destination.append(
            [
                half_w + x * half_w * margin * distance / depth,
                half_h + sy * half_h * margin * distance / depth,
            ]
        )

    matrix = cv2.getPerspectiveTransform(np.float32(source), np.float32(destination))
    return cv2.warpPerspective(
        frontal, matrix, (width, height), borderValue=(_BACKGROUND,) * 3
    )


def yaw_sweep(peak_deg: float, frames: int) -> list[float]:
    """Trayectoria de giro suave: acelera, llega al máximo y vuelve.

    Sin saltos, que es lo que hace un cuello.
    """
    return [
        float(peak_deg * np.sin(np.pi * i / max(1, frames - 1))) for i in range(frames)
    ]
