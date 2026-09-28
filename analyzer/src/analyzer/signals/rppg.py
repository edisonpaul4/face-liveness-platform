"""Fotopletismografía remota

Ataques objetivo:  pulso en la señal cromática..

Ataques objetivo: A1 foto impresa, A2 foto en pantalla.

Señales emitidas: rppg_snr_db, rppg_dominant_hz,
rppg_band_power_ratio.

Este módulo mide y devuelve magnitudes. No aplica umbrales, no compara contra
referencias de decisión y no concluye nada sobre la sesión.

SCAFFOLDING: sin implementación.
"""

from __future__ import annotations

from typing import Any


def compute(frame: Any, state: Any) -> dict[str, float]:
    """Calcula las señales de este detector para un frame.

    Args:
        frame: frame decodificado.
        state: estado temporal de la sesión en este worker.

    Returns:
        Mapa nombre → magnitud. Nunca contiene decisiones ni probabilidades
        de ataque.
    """
    raise NotImplementedError("scaffolding: detector no implementado")
