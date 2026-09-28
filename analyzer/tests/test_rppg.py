"""El pulso se mide, no se juzga.

Estos tests fijan tres cosas: que la frecuencia recuperada es la que se metió,
que una superficie sin latido no produce un pico, y —lo que más importa— que
cuando no hay señal suficiente el módulo lo dice en vez de devolver un número
bajo. Un SNR bajo por sesión corta es indistinguible de un SNR bajo por
máscara, y confundirlos rechaza a gente viva.
"""

from __future__ import annotations

import numpy as np
import pytest

from analyzer.rppg import MIN_SECONDS, measure


def series(
    *,
    bpm: float = 69.0,
    amplitude: float = 1.2,
    noise: float = 0.3,
    seconds: float = 20.0,
    fps: float = 15.0,
    drift: float = 6.0,
    seed: int = 0,
) -> tuple[list[int], list[np.ndarray]]:
    """Medias BGR sintéticas de un rostro con —o sin— latido."""
    rng = np.random.default_rng(seed)
    t = np.arange(int(seconds * fps)) / fps
    beat = np.sin(2 * np.pi * (bpm / 60.0) * t)
    # Deriva lenta de iluminación: lo que hace una habitación real.
    slow = drift * np.sin(2 * np.pi * 0.05 * t)
    # El verde es el que más absorbe la hemoglobina; de ahí los pesos.
    base, weights = (120.0, 140.0, 175.0), (0.3, 1.0, 0.55)
    channels = np.stack(
        [
            base[i] + amplitude * weights[i] * beat + slow + rng.normal(0, noise, len(t))
            for i in range(3)
        ],
        axis=1,
    )
    return [int(x * 1e6) for x in t], [np.asarray(row) for row in channels]


def test_recovers_the_heart_rate_it_was_given() -> None:
    for bpm in (55.0, 69.0, 92.0):
        pulse = measure(*series(bpm=bpm), 15.0)
        assert pulse is not None, f"{bpm} lpm debería medirse"
        # Tolerancia = una resolución de FFT sobre 20 s, que son 3 lpm.
        assert abs(pulse.bpm - bpm) <= 4.0, f"{bpm} lpm salió como {pulse.bpm}"


def test_a_surface_without_pulse_scores_below_a_live_face() -> None:
    live = measure(*series(amplitude=1.2), 15.0)
    flat = measure(*series(amplitude=0.0), 15.0)
    assert live is not None
    assert flat is not None
    assert live.snr_db > flat.snr_db + 6.0, (
        f"separación insuficiente: vivo {live.snr_db:.2f} dB, plano {flat.snr_db:.2f} dB"
    )


def test_too_short_is_unmeasurable_not_a_low_score() -> None:
    """La regla del repositorio: lo que no se pudo medir no entra en la fusión.

    Devolver un SNR bajo aquí sería fabricar evidencia contra alguien cuya
    sesión simplemente fue corta.
    """
    short = measure(*series(seconds=MIN_SECONDS - 2.0), 15.0)
    assert short is None


def test_a_gap_splits_the_series_instead_of_being_interpolated() -> None:
    """Interpolar sobre un hueco de un segundo es inventar latidos."""
    timestamps, channels = series(seconds=24.0)
    half = len(timestamps) // 2
    # Cinco segundos de agujero en mitad de la serie.
    timestamps = timestamps[:half] + [t + 5_000_000 for t in timestamps[half:]]

    pulse = measure(timestamps, channels, 15.0)
    assert pulse is not None
    # Se queda con un solo tramo, nunca con los dos cosidos.
    assert pulse.seconds < 24.0 * 0.6


def test_illumination_drift_does_not_masquerade_as_bradycardia() -> None:
    """Sin latido pero con deriva fuerte, no se devuelve un pulso del borde.

    La deriva asoma por el borde inferior de la banda y se lee como 42 lpm si
    nadie lo impide. Pasó con datos reales.
    """
    for seed in range(6):
        pulse = measure(*series(amplitude=0.0, drift=40.0, noise=0.2, seed=seed), 15.0)
        if pulse is not None:
            assert pulse.bpm > 45.0, f"deriva leída como {pulse.bpm:.1f} lpm"


@pytest.mark.parametrize("bad", [[], [1, 2, 3]])
def test_degenerate_input_is_unmeasurable(bad: list[int]) -> None:
    assert measure(bad, [np.zeros(3) for _ in bad], 15.0) is None


def test_el_tramo_de_quietud_sobrevive_a_la_sesion_entera() -> None:
    """Las muestras del tramo tienen que seguir ahí cuando se pida el pulso.

    Es un fallo que no se ve: el pulso se pide al RESOLVER, no al cerrar el
    paso, así que entre medias entran los frames de todos los retos que vengan
    después. Si el recorte se lleva las muestras del tramo, la señal sale como
    no medible y nada lo delata — parece que no había pulso cuando lo que no
    había era memoria.

    Pasó al subir la captura a 30 fps: el tope estaba en 600 muestras y una
    sesión de 60 s produce 1800.
    """
    from analyzer.sessions import PoseSample, SessionState

    fps, budget_s = 30, 60
    state = SessionState("s", 0, 0.0, 0.0)

    hold_start_us, hold_end_us = 2_000_000, 13_000_000
    for i in range(fps * budget_s):
        state.record(PoseSample(seq=i, at_us=int(i * 1e6 / fps), pose=None, scale=1.0))

    kept = state.samples_between(hold_start_us, hold_end_us)
    esperadas = fps * (hold_end_us - hold_start_us) // 1_000_000
    assert len(kept) >= esperadas * 0.9, (
        f"del tramo de quietud sobrevivieron {len(kept)} muestras de ~{esperadas}: "
        "el recorte se las llevó antes de poder medir el pulso"
    )
