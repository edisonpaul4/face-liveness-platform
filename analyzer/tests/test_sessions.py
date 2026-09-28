"""Estado caliente por sesión: acumulación y limpieza."""

from __future__ import annotations

import pytest

from analyzer.sessions import SessionStore


class FakeClock:
    """Reloj manual: el tiempo no se espera, se adelanta."""

    def __init__(self) -> None:
        self.now = 1000.0

    def __call__(self) -> float:
        return self.now

    def advance(self, seconds: float) -> None:
        self.now += seconds


def test_el_estado_se_acumula_entre_frames():
    """Es la razón de ser de la afinidad de sesión."""
    store = SessionStore()
    state = store.open("S1", deadline_us=0)

    for seq in range(1, 11):
        state.observe(seq, brightness=0.5, now=1000.0)

    assert state.frames == 10
    assert state.last_seq == 10
    assert state.window_frames == 10


def test_la_ventana_temporal_no_crece_sin_freno():
    store = SessionStore()
    state = store.open("S1", deadline_us=0)
    state.window_size = 8

    for seq in range(1, 51):
        state.observe(seq, brightness=0.5, now=1000.0)

    assert state.frames == 50
    assert state.window_frames == 8


def test_no_admite_dos_veces_la_misma_sesion():
    """Aceptarla dos veces mezclaría dos flujos en un solo estado."""
    store = SessionStore()
    store.open("S1", deadline_us=0)

    with pytest.raises(KeyError, match="ya está abierta"):
        store.open("S1", deadline_us=0)


def test_cerrar_suelta_el_estado():
    store = SessionStore()
    store.open("S1", deadline_us=0)

    assert store.close("S1") is True
    assert len(store) == 0
    assert store.get("S1") is None
    assert store.close("S1") is False


def test_caduca_por_inactividad():
    """Cubre al cliente que desaparece sin avisar."""
    clock = FakeClock()
    store = SessionStore(idle_ttl_s=30.0, clock=clock)
    state = store.open("S1", deadline_us=0)

    clock.advance(20)
    state.observe(1, 0.5, clock())
    clock.advance(20)
    assert store.sweep() == []  # se le habló hace 20 s

    clock.advance(15)
    assert store.sweep() == ["S1"]
    assert len(store) == 0


def test_caduca_al_vencer_el_lease():
    """Cubre que el aviso de cierre se pierda: con un bus efímero, pasa."""
    clock = FakeClock()
    store = SessionStore(idle_ttl_s=3600.0, clock=clock)
    store.open("S1", deadline_us=2_000_000)

    assert store.sweep(now_us=1_000_000) == []
    assert store.sweep(now_us=2_000_001) == ["S1"]


def test_una_sesion_sin_deadline_no_caduca_por_el():
    clock = FakeClock()
    store = SessionStore(idle_ttl_s=3600.0, clock=clock)
    store.open("S1", deadline_us=0)

    assert store.sweep(now_us=9_999_999_999) == []
