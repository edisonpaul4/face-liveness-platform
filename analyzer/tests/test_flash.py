"""Piezas del analizador de destello, por separado."""

from __future__ import annotations

import numpy as np
import pytest

from analyzer import calibration, flash
from analyzer.photometry import FramePhotometry

SEQUENCE = (
    flash.FlashSegment("white", 300),
    flash.FlashSegment("red", 250),
    flash.FlashSegment("blue", 300),
)
START = 1_000_000


def spec(sequence=SEQUENCE, start=START, end=START + 2_000_000):
    return flash.FlashWindowSpec("w1", sequence, start, end)


def frame(regions, background=(120.0, 120.0, 120.0), **kwargs):
    defaults = {
        "face_luminance": 100.0, "specular_ratio": 0.0, "luminance_ratio": 0.85,
        "moire_index": 0.01, "banding_index": 0.01,
    }
    defaults.update(kwargs)
    return FramePhotometry(
        regions=np.array(regions, dtype=np.float64),
        background=np.array(background, dtype=np.float64),
        **defaults,
    )


def responsive_session(gains, *, lag_ms=0, frames=40, interval_us=50_000, noise=0.0, seed=1):
    """Sesión sintética donde cada región responde con su propia ganancia.

    Devuelve (línea base, muestras). Con `gains` iguales, superficie plana.
    """
    rng = np.random.default_rng(seed)
    base_level = np.array([[90.0, 100.0, 110.0]] * 4)

    baseline_frames = [frame(base_level * (1 + rng.normal(0, noise, base_level.shape)))
                       for _ in range(12)]
    baseline = calibration.estimate(baseline_frames)

    samples = []
    for i in range(frames):
        at = START + i * interval_us
        color = flash.stimulus_at(spec(), at, lag_ms * 1000)
        lit = base_level.copy()
        if color is not None:
            for region in range(4):
                lit[region] = base_level[region] * (1 + gains[region] * color * 0.6)
        if noise:
            lit = lit * (1 + rng.normal(0, noise, lit.shape))
        samples.append((at, frame(lit)))
    return baseline, samples


# --- estímulo y retardo ------------------------------------------------------


def test_la_linea_de_tiempo_del_estimulo():
    s = spec()
    assert flash.stimulus_at(s, START, 0).tolist() == [1, 1, 1]          # blanco
    assert flash.stimulus_at(s, START + 400_000, 0).tolist() == [0, 0, 1]  # rojo
    assert flash.stimulus_at(s, START + 700_000, 0).tolist() == [1, 0, 0]  # azul
    assert flash.stimulus_at(s, START + 900_000, 0) is None                # terminó
    assert flash.stimulus_at(s, START - 1, 0) is None                      # no empezó


def test_el_retardo_desplaza_la_linea_de_tiempo():
    s = spec()
    # Con 200 ms de retardo, lo que se ve a los 400 ms salió a los 200: blanco.
    assert flash.stimulus_at(s, START + 400_000, 200_000).tolist() == [1, 1, 1]


@pytest.mark.parametrize("lag_ms", [0, 60, 140, 240, 320])
def test_encuentra_el_retardo(lag_ms):
    """Nunca se asume sincronía: se busca."""
    baseline, samples = responsive_session([1.0, 1.4, 0.8, 0.85], lag_ms=lag_ms)
    result = flash.analyze(spec(), samples, baseline)

    assert abs(result.raw["lag_ms"] - lag_ms) <= 20
    assert result.submetrics["correlation"] > 0.9


# --- gradiente 3D ------------------------------------------------------------


def test_el_relieve_da_dispersion():
    baseline, samples = responsive_session([1.0, 1.5, 0.7, 0.75])
    result = flash.analyze(spec(), samples, baseline)

    assert result.submetrics["gradient_3d"] > 0.8
    assert result.raw["gradient_dispersion"] > 0.2
    assert result.raw["amplitude_nose"] > result.raw["amplitude_cheek_left"]


def test_una_superficie_plana_no_da_dispersion():
    """Todas las regiones reciben lo mismo: es lo que delata a una foto."""
    baseline, samples = responsive_session([1.0, 1.0, 1.0, 1.0])
    result = flash.analyze(spec(), samples, baseline)

    assert result.submetrics["gradient_3d"] < 0.15
    assert result.raw["gradient_dispersion"] < 0.03
    # Pero sigue al destello igual de bien que un rostro.
    assert result.submetrics["correlation"] > 0.9


def test_la_dispersion_no_depende_de_cuanta_luz_llegue():
    """Es un coeficiente de variación: adimensional a propósito.

    Si dependiera del nivel, dependería del tono de piel.
    """
    baseline_a, samples_a = responsive_session([1.0, 1.5, 0.7, 0.75])
    baseline_b, samples_b = responsive_session([0.3, 0.45, 0.21, 0.225])

    a = flash.analyze(spec(), samples_a, baseline_a)
    b = flash.analyze(spec(), samples_b, baseline_b)

    # No sale idéntico: la respuesta se mide en logaritmo y el logaritmo
    # comprime más las respuestas grandes. Con un factor 3,3 entre las dos
    # sesiones, la dispersión se mueve un 16 %, no un orden de magnitud.
    assert abs(a.raw["gradient_dispersion"] - b.raw["gradient_dispersion"]) < 0.06


# --- sospecha de pantalla ----------------------------------------------------


def test_brillar_sin_seguir_al_destello_es_sospechoso():
    baseline, samples = responsive_session([0.02, 0.02, 0.02, 0.02])
    bright = [
        (at, frame(p.regions, luminance_ratio=2.4, banding_index=0.09, specular_ratio=0.03))
        for at, p in samples
    ]
    result = flash.analyze(spec(), bright, baseline)

    assert result.raw["screen_suspicion"] > 0.7
    assert result.submetrics["screen_absence"] < 0.3


def test_brillar_siguiendo_al_destello_no_lo_es():
    """Alguien con la piel clara en una habitación a oscuras también brilla."""
    baseline, samples = responsive_session([1.0, 1.4, 0.8, 0.85])
    bright = [(at, frame(p.regions, luminance_ratio=2.4)) for at, p in samples]
    result = flash.analyze(spec(), bright, baseline)

    assert result.raw["screen_bright_and_deaf"] == pytest.approx(0.0)
    assert result.submetrics["screen_absence"] > 0.8


# --- calidad -----------------------------------------------------------------


def test_sin_linea_base_no_se_analiza():
    _, samples = responsive_session([1.0, 1.4, 0.8, 0.85])
    result = flash.analyze(spec(), samples, None)

    assert not result.quality_sufficient
    assert "línea base" in result.quality_reason
    assert all(v is None for v in result.submetrics.values())


def test_con_pocos_frames_no_se_analiza():
    baseline, samples = responsive_session([1.0, 1.4, 0.8, 0.85], frames=4)
    result = flash.analyze(spec(), samples, baseline)

    assert not result.quality_sufficient
    assert "frames" in result.quality_reason


def test_la_senal_por_debajo_del_ruido_es_calidad_insuficiente():
    """Y no un ataque: las sub-métricas van a None, no a cero."""
    baseline, samples = responsive_session([0.004, 0.004, 0.004, 0.004], noise=0.02, seed=5)
    result = flash.analyze(spec(), samples, baseline)

    assert not result.quality_sufficient
    assert result.quality_reason == flash.AMBIENT_REASON
    assert result.submetrics["correlation"] is None
    assert result.submetrics["gradient_3d"] is None


def test_el_resultado_se_serializa_entero():
    baseline, samples = responsive_session([1.0, 1.4, 0.8, 0.85])
    payload = flash.analyze(spec(), samples, baseline).to_dict()

    assert payload["window_id"] == "w1"
    assert set(payload["submetrics"]) == {"correlation", "gradient_3d", "screen_absence"}
    assert payload["quality_sufficient"] is True
    assert isinstance(payload["raw"], dict)
    assert 0.0 <= payload["score"] <= 1.0


def test_no_sale_ningun_veredicto():
    baseline, samples = responsive_session([1.0, 1.4, 0.8, 0.85])
    payload = flash.analyze(spec(), samples, baseline).to_dict()

    texto = str(payload).lower()
    for juicio in ("live", "spoof", "attack", "passed", "verdict", "decision"):
        assert juicio not in texto


def test_el_gradiente_pesa_mas_que_lo_demas():
    """Es lo único que separa un rostro de una foto impresa."""
    weights = flash.FlashConfig().weights
    assert weights["gradient_3d"] > weights["correlation"] > weights["screen_absence"]
    assert sum(weights.values()) == pytest.approx(1.0)
