"""Robustez del analizador de destello.

Dos cosas que tienen que cumplirse para que esto sirva en producción:

* La medición diferencial aguanta los automatismos de la cámara. Si no, la
  webcam compensa el destello y borra la señal.
* Cuando la luz ambiente aplasta el destello, el sistema lo DICE en vez de
  fingir una medida. Eso lleva a reintentar, no a rechazar a nadie.
"""

from __future__ import annotations

from dataclasses import replace

import numpy as np
import pytest

from analyzer import calibration, flash, photometry
from tests.flashlab import SEQUENCE
from tests.flashscene import SKIN_TONES, Camera, Scene, render_session


def _analyse(detector, geometry, scene, *, absolute: bool):
    """Analiza una escena midiendo contra el fondo, o sin hacerlo."""
    cal_frames, flash_frames, start = render_session(scene, geometry, SEQUENCE)

    def measure(image):  # noqa: ANN001, ANN202
        detection = detector.detect(image)
        if not detection.faces:
            return None
        sample = photometry.measure(image, detection.landmarks)
        if sample is None:
            return None
        if absolute:
            # Fondo neutro = medir el brillo tal cual, sin diferencial.
            return replace(sample, background=np.ones(3, dtype=np.float64))
        return sample

    baseline = calibration.estimate([p for p in (measure(i) for _, i in cal_frames) if p])
    samples = [(at, p) for at, p in ((at, measure(i)) for at, i in flash_frames) if p]
    spec = flash.FlashWindowSpec("w", SEQUENCE, start, flash_frames[-1][0])
    return flash.analyze(spec, samples, baseline)


@pytest.fixture(scope="module")
def geometry(mediapipe_detector):
    from tests.flashscene import build_geometry

    return build_geometry(mediapipe_detector)


def test_el_diferencial_aguanta_lo_que_la_medida_absoluta_pierde(mediapipe_detector, geometry):
    """El problema número uno de esta técnica, medido.

    La webcam mide ponderando el centro —donde está la cara— y corrige todo el
    frame. Cuando el destello ilumina al sujeto, baja la ganancia y el brillo
    absoluto del rostro apenas se mueve. Dividir por el fondo lo cancela,
    porque la corrección afecta a los dos por igual.
    """
    scene = Scene(
        kind="real",
        tone=SKIN_TONES[2],
        camera=Camera(exposure_strength=1.0, white_balance_strength=1.0),
    )

    differential = _analyse(mediapipe_detector, geometry, scene, absolute=False)
    absolute = _analyse(mediapipe_detector, geometry, scene, absolute=True)

    print(f"\n  amplitud   diferencial={differential.raw['modulation_amplitude']:.4f} "
          f"absoluta={absolute.raw['modulation_amplitude']:.4f}")
    print(f"  dispersión diferencial={differential.raw['gradient_dispersion']:.4f} "
          f"absoluta={absolute.raw['gradient_dispersion']:.4f}")

    # La cámara se come la mayor parte de la señal absoluta.
    assert absolute.raw["modulation_amplitude"] < differential.raw["modulation_amplitude"] * 0.4

    # Y peor todavía: al comprimir unas regiones más que otras, la medida
    # absoluta se INVENTA un relieve que no existe. El gradiente 3D dejaría de
    # medir la cara para medir el auto-exposición.
    assert absolute.raw["gradient_dispersion"] > differential.raw["gradient_dispersion"] * 3

    # El diferencial, en cambio, no se entera.
    assert differential.submetrics["correlation"] > 0.9
    assert differential.submetrics["gradient_3d"] > 0.8


@pytest.mark.parametrize(
    "exposure, balance",
    [(0.0, 0.0), (0.5, 0.3), (0.85, 0.6), (1.0, 1.0)],
    ids=["sin_automatismos", "suaves", "tipicos", "agresivos"],
)
def test_la_medida_diferencial_es_estable_con_cualquier_camara(
    mediapipe_detector, geometry, exposure, balance
):
    """La misma escena, cámaras que compensan de forma muy distinta."""
    scene = Scene(
        kind="real",
        tone=SKIN_TONES[2],
        camera=Camera(exposure_strength=exposure, white_balance_strength=balance),
    )
    result = _analyse(mediapipe_detector, geometry, scene, absolute=False)

    print(f"\n  AE={exposure} AWB={balance} → amplitud={result.raw['modulation_amplitude']:.4f} "
          f"dispersión={result.raw['gradient_dispersion']:.4f} score={result.score:.3f}")

    assert result.quality_sufficient
    assert result.submetrics["correlation"] > 0.9
    assert result.submetrics["gradient_3d"] > 0.8
    assert result.score > 0.85


def test_la_luz_ambiente_fuerte_da_calidad_insuficiente(flash_lab):
    """El requisito de robustez.

    Con la habitación muy iluminada y la pantalla dando poco, el destello no
    llega a mover la piel por encima del ruido. Eso NO es un ataque: es una
    medida que no se puede hacer, y el sistema tiene que decirlo para que se
    reintente.
    """
    crushed = flash_lab.run(
        Scene(kind="real", tone=SKIN_TONES[2], ambient=3000.0, flash_intensity=4.0)
    ).score

    print(f"\n  snr={crushed.raw['signal_to_noise']:.2f} → {crushed.quality_reason}")

    assert not crushed.quality_sufficient
    assert crushed.quality_reason == flash.AMBIENT_REASON

    # Y no se emite un juicio disfrazado: lo que no se pudo medir va como no
    # medido, no como cero.
    assert crushed.submetrics["correlation"] is None
    assert crushed.submetrics["gradient_3d"] is None


def test_la_degradacion_es_progresiva(flash_lab):
    """Entre 'se ve perfecto' y 'no se ve nada' hay una rampa, no un salto."""
    print()
    previous = None
    for ambient, intensity in ((150, 130), (600, 130), (150, 20), (600, 8), (1500, 5)):
        result = flash_lab.run(
            Scene(kind="real", tone=SKIN_TONES[2], ambient=float(ambient),
                  flash_intensity=float(intensity))
        ).score
        snr = result.raw["signal_to_noise"]
        print(f"  destello/ambiente={intensity / ambient:.4f} → snr={snr:7.2f} "
              f"{'ok' if result.quality_sufficient else 'INSUFICIENTE'}")

        if previous is not None:
            assert snr <= previous * 1.35, "el snr debería bajar con menos destello"
        previous = snr


def test_una_ventana_sin_linea_base_no_se_analiza(flash_lab):
    """Sin calibración no hay nada contra qué comparar.

    Es el motivo de que la fase de calibración sea obligatoria: sin ella sólo
    quedarían umbrales absolutos, y con umbrales absolutos el sistema
    rechazaría de más a quien tiene la piel oscura.
    """
    spec = flash.FlashWindowSpec("w", SEQUENCE, 0, 10**12)
    result = flash.analyze(spec, [], baseline=None)

    assert not result.quality_sufficient
    assert "línea base" in result.quality_reason
    assert all(v is None for v in result.submetrics.values())
