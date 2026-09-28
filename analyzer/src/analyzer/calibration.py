"""Línea base del sujeto, capturada con pantalla neutra.

**Es obligatoria y va antes de cualquier destello.** Todo lo que se mide
después se mide RELATIVO a esta línea base, nunca contra umbrales absolutos.

Por qué importa, y no es un detalle de implementación: la respuesta absoluta de
la piel al destello depende del tono de piel, de la luz de la habitación y de
cómo esa cámara concreta expone y balancea. Un umbral absoluto convertiría esas
tres cosas en motivo de rechazo, y quien más lo pagaría serían las personas de
piel oscura, que devuelven menos luz. Eso no sería un fallo de precisión: sería
un producto defectuoso.

Midiendo relativo, lo que se compara es cuánto CAMBIA cada sujeto respecto a sí
mismo. El albedo de la piel se cancela en la división.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from analyzer import rois
from analyzer.photometry import FramePhotometry

#: Frames mínimos con rostro para que la línea base signifique algo.
#:
#: Acoplado con `minCalibrationHits` del gateway, que no cierra el paso de
#: calibración hasta tener al menos tantos. El acoplamiento está anotado en los
#: dos lados a propósito: si el gateway cierra antes, entrega una ventana que
#: aquí se rechaza, y sin línea base ningún destello posterior se puede medir
#: — sin que nada señale que la causa estaba en el primer paso.
MIN_CALIBRATION_FRAMES = 5


@dataclass(frozen=True, slots=True)
class Baseline:
    """Cómo se comporta este sujeto, esta luz y esta cámara en reposo."""

    #: Respuesta diferencial en reposo: región/fondo, por canal. (4, 3)
    region_ratio: np.ndarray
    #: Variación de esa respuesta cuando no pasa nada. Es el suelo de ruido
    #: contra el que se juzga si un destello ha producido señal o no.
    region_noise: np.ndarray

    #: Luminancia del rostro en reposo, y pistas de superficie ya
    #: diferenciales (rostro frente a fondo).
    face_luminance: float
    specular_ratio: float
    luminance_ratio: float
    moire_index: float
    banding_index: float

    frames: int

    @property
    def noise_floor(self) -> float:
        """Ruido típico de la respuesta diferencial, en escala logarítmica."""
        with np.errstate(divide="ignore", invalid="ignore"):
            relative = self.region_noise / np.maximum(self.region_ratio, 1e-6)
        return float(np.median(relative[np.isfinite(relative)])) if relative.size else 0.0

    def to_dict(self) -> dict[str, float]:
        """Resumen para trazas. No sale piel ni imagen: sólo estadísticos."""
        return {
            "frames": float(self.frames),
            "face_luminance": round(self.face_luminance, 3),
            "specular_ratio": round(self.specular_ratio, 6),
            "luminance_ratio": round(self.luminance_ratio, 3),
            "moire_index": round(self.moire_index, 5),
            "banding_index": round(self.banding_index, 5),
            "noise_floor": round(self.noise_floor, 6),
        }


def estimate(samples: list[FramePhotometry]) -> Baseline | None:
    """Calcula la línea base a partir de los frames de pantalla neutra.

    Devuelve None si no hubo material suficiente: sin línea base no se analiza
    ningún destello, porque no habría contra qué comparar.
    """
    usable = [s for s in samples if s is not None]
    if len(usable) < MIN_CALIBRATION_FRAMES:
        return None

    ratios = np.stack([s.region_ratio() for s in usable])  # (n, 4, 3)

    return Baseline(
        region_ratio=ratios.mean(axis=0),
        region_noise=ratios.std(axis=0),
        face_luminance=float(np.mean([s.face_luminance for s in usable])),
        specular_ratio=float(np.mean([s.specular_ratio for s in usable])),
        luminance_ratio=float(np.mean([s.luminance_ratio for s in usable])),
        moire_index=float(np.mean([s.moire_index for s in usable])),
        banding_index=float(np.mean([s.banding_index for s in usable])),
        frames=len(usable),
    )


def relative_response(sample: FramePhotometry, baseline: Baseline) -> np.ndarray:
    """Respuesta de un frame relativa a la línea base, en logaritmo.

    El logaritmo convierte en sumas lo que la iluminación hace multiplicando,
    y deja la medida en una escala donde el tono de piel ya no aparece: lo que
    queda es cuánto ha cambiado el sujeto respecto a sí mismo.
    """
    ratio = sample.region_ratio()
    with np.errstate(divide="ignore", invalid="ignore"):
        response = np.log(np.maximum(ratio, 1e-6) / np.maximum(baseline.region_ratio, 1e-6))
    return np.nan_to_num(response, nan=0.0, posinf=0.0, neginf=0.0)


#: Orden estable de las regiones, para que los índices signifiquen lo mismo
#: en todo el módulo.
REGION_ORDER = rois.FACE_REGIONS
