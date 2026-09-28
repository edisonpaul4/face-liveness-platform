"""Regiones de interés para fotometría, recortadas desde los landmarks.

Cuatro regiones faciales a distinta profundidad y orientación respecto a la
pantalla, más una de FONDO fuera del rostro.

El fondo no es un extra: es la referencia. La respuesta de cada región facial
se mide siempre contra la del fondo EN EL MISMO FRAME, y eso es lo que cancela
el auto-exposición y el balance de blancos automático de la webcam. Sin esa
división, la cámara compensa el destello y borra justo la señal que se quiere
medir. Es el problema número uno de esta técnica.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

#: Anclas de MediaPipe para cada región facial.
#:
#: Se toma un cuadrado alrededor del ancla, con lado proporcional al tamaño
#: del rostro: así la región vale igual de cerca que de lejos.
ANCHORS: dict[str, int] = {
    "forehead": 151,    # centro de la frente, por debajo del nacimiento del pelo
    "nose": 1,          # punta de la nariz, lo más cercano a la pantalla
    "cheek_left": 50,   # pómulo izquierdo del sujeto
    "cheek_right": 280,  # pómulo derecho del sujeto
}

#: Nombres de las regiones faciales, en orden estable.
FACE_REGIONS = tuple(ANCHORS)

#: Lado del cuadrado de muestreo, como fracción del ancho del rostro.
PATCH_RATIO = 0.16
#: Lado mínimo en píxeles: por debajo, la media es ruido.
MIN_PATCH_PX = 6


@dataclass(frozen=True, slots=True)
class Patch:
    """Un rectángulo de muestreo en píxeles."""

    x0: int
    y0: int
    x1: int
    y1: int

    @property
    def area(self) -> int:
        return max(0, self.x1 - self.x0) * max(0, self.y1 - self.y0)

    def mean_bgr(self, image: np.ndarray) -> np.ndarray | None:
        """Color medio del rectángulo, en BGR float."""
        if self.area <= 0:
            return None
        region = image[self.y0 : self.y1, self.x0 : self.x1]
        if region.size == 0:
            return None
        return region.reshape(-1, region.shape[-1]).mean(axis=0).astype(np.float64)


def _clip(value: int, low: int, high: int) -> int:
    return max(low, min(high, value))


def _square(cx: float, cy: float, half: float, width: int, height: int) -> Patch:
    return Patch(
        x0=_clip(int(cx - half), 0, width),
        y0=_clip(int(cy - half), 0, height),
        x1=_clip(int(cx + half), 0, width),
        y1=_clip(int(cy + half), 0, height),
    )


def face_patches(landmarks: np.ndarray, width: int, height: int) -> dict[str, Patch]:
    """Rectángulos de muestreo de las cuatro regiones faciales.

    Devuelve sólo las que caben en el encuadre y tienen tamaño suficiente.
    """
    if landmarks is None or len(landmarks) <= max(ANCHORS.values()):
        return {}

    xs = landmarks[:, 0] * width
    face_width = float(xs.max() - xs.min())
    half = max(MIN_PATCH_PX / 2.0, face_width * PATCH_RATIO / 2.0)

    patches: dict[str, Patch] = {}
    for name, index in ANCHORS.items():
        patch = _square(float(landmarks[index][0] * width), float(landmarks[index][1] * height),
                        half, width, height)
        if min(patch.x1 - patch.x0, patch.y1 - patch.y0) >= MIN_PATCH_PX:
            patches[name] = patch
    return patches


def background_patches(landmarks: np.ndarray, width: int, height: int) -> list[Patch]:
    """Rectángulos de fondo, a los lados del rostro y fuera de él.

    Se toman a la misma altura que la cara para que les llegue una luz
    ambiente parecida, y con margen respecto al contorno para no colar piel.
    """
    if landmarks is None or len(landmarks) == 0:
        return []

    xs = landmarks[:, 0] * width
    ys = landmarks[:, 1] * height
    left, right = float(xs.min()), float(xs.max())
    top, bottom = float(ys.min()), float(ys.max())

    margin = max(8.0, (right - left) * 0.18)
    band_height = max(MIN_PATCH_PX, int((bottom - top) * 0.5))
    cy = (top + bottom) / 2.0
    y0 = _clip(int(cy - band_height / 2), 0, height)
    y1 = _clip(int(cy + band_height / 2), 0, height)

    band_width = max(MIN_PATCH_PX, int(width * 0.12))

    candidates = [
        Patch(_clip(int(left - margin - band_width), 0, width), y0,
              _clip(int(left - margin), 0, width), y1),
        Patch(_clip(int(right + margin), 0, width), y0,
              _clip(int(right + margin + band_width), 0, width), y1),
    ]
    return [p for p in candidates if min(p.x1 - p.x0, p.y1 - p.y0) >= MIN_PATCH_PX]


def background_mean(image: np.ndarray, patches: list[Patch]) -> np.ndarray | None:
    """Color medio del fondo, ponderado por área."""
    total = np.zeros(3, dtype=np.float64)
    weight = 0.0
    for patch in patches:
        mean = patch.mean_bgr(image)
        if mean is None:
            continue
        total += mean * patch.area
        weight += patch.area
    if weight <= 0:
        return None
    return total / weight
