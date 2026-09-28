"""Línea base: sin ella no se mide nada."""

from __future__ import annotations

import numpy as np
import pytest

from analyzer import calibration
from analyzer.photometry import FramePhotometry


def sample(scale=1.0, noise=0.0, seed=0):
    """Fotometría sintética: cuatro regiones y un fondo."""
    rng = np.random.default_rng(seed)
    regions = np.array([[90.0, 100.0, 110.0], [95.0, 105.0, 115.0],
                        [88.0, 98.0, 108.0], [89.0, 99.0, 109.0]]) * scale
    if noise:
        regions = regions * (1 + rng.normal(0, noise, regions.shape))
    return FramePhotometry(
        regions=regions,
        background=np.array([120.0, 120.0, 120.0]) * scale,
        face_luminance=100.0 * scale,
        specular_ratio=0.0,
        luminance_ratio=0.83,
        moire_index=0.01,
        banding_index=0.01,
    )


def test_necesita_frames_suficientes():
    """Con dos frames no hay línea base que valga."""
    assert calibration.estimate([sample() for _ in range(2)]) is None
    assert calibration.estimate([]) is None
    assert calibration.estimate([sample() for _ in range(8)]) is not None


def test_la_linea_base_no_depende_del_nivel_de_luz():
    """Es lo que hace que funcione con cualquier tono de piel y cualquier luz.

    Dos sujetos que devuelven la mitad de luz que otro dan la MISMA línea base
    relativa, porque lo que se guarda es la razón contra el fondo.
    """
    bright = calibration.estimate([sample(scale=1.0) for _ in range(10)])
    dark = calibration.estimate([sample(scale=0.35) for _ in range(10)])

    # rtol holgada por el epsilon que evita dividir por cero en zonas oscuras.
    np.testing.assert_allclose(bright.region_ratio, dark.region_ratio, rtol=1e-4)


def test_el_suelo_de_ruido_sale_de_la_variacion_en_reposo():
    quiet = calibration.estimate([sample(noise=0.0, seed=i) for i in range(12)])
    restless = calibration.estimate([sample(noise=0.03, seed=i) for i in range(12)])

    assert quiet.noise_floor < restless.noise_floor
    assert restless.noise_floor > 0


def test_la_respuesta_relativa_es_cero_cuando_no_pasa_nada():
    """Sin destello, el sujeto no ha cambiado respecto a sí mismo."""
    baseline = calibration.estimate([sample() for _ in range(10)])
    response = calibration.relative_response(sample(), baseline)

    np.testing.assert_allclose(response, np.zeros_like(response), atol=1e-9)


def test_la_respuesta_relativa_capta_el_cambio():
    baseline = calibration.estimate([sample() for _ in range(10)])

    lit = sample()
    brighter = FramePhotometry(
        regions=lit.regions * 1.5, background=lit.background,
        face_luminance=lit.face_luminance, specular_ratio=0.0,
        luminance_ratio=1.2, moire_index=0.01, banding_index=0.01,
    )
    response = calibration.relative_response(brighter, baseline)

    assert np.all(response > 0.3)
    np.testing.assert_allclose(response, np.log(1.5), rtol=0.01)


def test_el_resumen_no_saca_datos_del_sujeto():
    """A las trazas van estadísticos, nunca piel ni imagen."""
    baseline = calibration.estimate([sample() for _ in range(10)])
    summary = baseline.to_dict()

    assert set(summary) == {
        "frames", "face_luminance", "specular_ratio", "luminance_ratio",
        "moire_index", "banding_index", "noise_floor",
    }
    assert all(isinstance(v, float) for v in summary.values())


@pytest.mark.parametrize("frames", [5, 20, 100])
def test_cuenta_los_frames_que_uso(frames):
    assert calibration.estimate([sample() for _ in range(frames)]).frames == frames
