"""Banco de ensayo: monta una sesión de destello y la analiza de punta a punta.

Lo usan el criterio de aceptación y los tests de robustez. Cachea por
configuración de escena porque renderizar y detectar cuesta.
"""

from __future__ import annotations

from dataclasses import dataclass

from analyzer import calibration, flash, photometry
from tests.flashscene import Scene, build_geometry, render_session

#: Secuencia de colores del reto. Empieza en blanco, que es la referencia
#: cromática, y sigue con primarios.
SEQUENCE = (
    flash.FlashSegment("white", 400),
    flash.FlashSegment("red", 350),
    flash.FlashSegment("blue", 350),
    flash.FlashSegment("green", 350),
)


@dataclass(frozen=True, slots=True)
class Run:
    """Resultado de pasar una escena entera por la cadena de medida."""

    score: flash.FlashScore
    baseline: calibration.Baseline | None
    calibration_frames: int
    flash_frames: int


class Lab:
    """Ejecuta escenas contra el analizador real."""

    def __init__(self, detector) -> None:  # noqa: ANN001
        self._detector = detector
        self._geometry = build_geometry(detector)
        self._cache: dict[tuple, Run] = {}

    def run(self, scene: Scene, sequence=SEQUENCE, config: flash.FlashConfig | None = None) -> Run:
        key = (
            scene.kind, scene.tone.name, scene.ambient, scene.flash_intensity,
            scene.lag_ms, id(sequence), id(config),
        )
        if key in self._cache:
            return self._cache[key]

        cal_frames, flash_frames, start = render_session(scene, self._geometry, sequence)

        cal_photometry = [p for p in (self._measure(img) for _, img in cal_frames) if p]
        baseline = calibration.estimate(cal_photometry)

        flash_photometry = [
            (at, p) for at, p in ((at, self._measure(img)) for at, img in flash_frames) if p
        ]

        spec = flash.FlashWindowSpec(
            window_id="w1", sequence=tuple(sequence),
            started_at_us=start, ended_at_us=flash_frames[-1][0],
        )
        result = Run(
            score=flash.analyze(spec, flash_photometry, baseline, config),
            baseline=baseline,
            calibration_frames=len(cal_photometry),
            flash_frames=len(flash_photometry),
        )
        self._cache[key] = result
        return result

    def _measure(self, image):  # noqa: ANN001, ANN202
        detection = self._detector.detect(image)
        if not detection.faces:
            return None
        return photometry.measure(image, detection.landmarks)
