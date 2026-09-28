"""Criterio de aceptación: 15 fps sostenidos con medidas válidas.

Se procesa un vídeo entero midiendo cada frame. No basta con que la media
salga: lo que importa es el percentil alto, porque un frame lento tarde o
temprano se come el hueco del siguiente.
"""

from __future__ import annotations

import statistics
import time

import pytest

from tests import contract
from tests.conftest import make_task

#: Ritmo que hay que sostener.
TARGET_FPS = 15.0
#: Presupuesto por frame.
BUDGET_MS = 30.0


@pytest.mark.parametrize("pipeline_name", ["mediapipe_pipeline", "full_pipeline"])
def test_procesa_el_video_a_15fps_sostenidos(request, pipeline_name, video_frames, session_state):
    """Procesa el vídeo de prueba y comprueba caudal, latencia y contrato."""
    pipeline = request.getfixturevalue(pipeline_name)

    latencies: list[float] = []
    faces_found = 0

    started = time.perf_counter()
    for seq, payload in enumerate(video_frames, start=1):
        frame_started = time.perf_counter()
        outcome = pipeline.process(make_task(payload, seq), session_state)
        latencies.append((time.perf_counter() - frame_started) * 1000.0)

        # Cada frame emite medidas válidas. Todos, no una muestra.
        contract.assert_valid(outcome.features.to_dict())
        if outcome.detection.face_detected:
            faces_found += 1

    elapsed = time.perf_counter() - started
    fps = len(video_frames) / elapsed
    p50 = statistics.median(latencies)
    p95 = sorted(latencies)[int(len(latencies) * 0.95)]
    worst = max(latencies)

    print(
        f"\n{len(video_frames)} frames en {elapsed:.2f} s → {fps:.1f} fps"
        f" | latencia p50={p50:.1f} ms p95={p95:.1f} ms máx={worst:.1f} ms"
        f" | rostros detectados en {faces_found}/{len(video_frames)} frames"
    )

    assert fps >= TARGET_FPS, f"sólo {fps:.1f} fps, hacen falta {TARGET_FPS}"
    assert p95 <= BUDGET_MS, f"el p95 es {p95:.1f} ms y el presupuesto {BUDGET_MS} ms"

    # Un detector que no encuentra nada también daría 15 fps: hay que
    # comprobar que además está detectando.
    assert faces_found >= len(video_frames) * 0.9, (
        f"sólo se detectó rostro en {faces_found} de {len(video_frames)} frames"
    )

    # Y que el estado caliente se acumuló: sin continuidad, cada frame sería
    # una foto suelta.
    assert session_state.frames == len(video_frames)


def test_sostiene_el_ritmo_en_tiempo_real(mediapipe_pipeline, video_frames, session_state):
    """El mismo vídeo, pero a ritmo de cámara.

    Se alimenta a 15 fps de verdad y se comprueba que ningún frame se sale de
    su hueco de 66 ms. Es la prueba que se parece a producción: no vale
    procesar rápido en ráfaga si luego un frame se atasca.
    """
    slot_s = 1.0 / TARGET_FPS
    overruns = 0
    next_deadline = time.perf_counter() + slot_s

    for seq, payload in enumerate(video_frames[:75], start=1):
        outcome = mediapipe_pipeline.process(make_task(payload, seq), session_state)
        contract.assert_valid(outcome.features.to_dict())

        now = time.perf_counter()
        if now > next_deadline:
            overruns += 1
        else:
            time.sleep(next_deadline - now)
        next_deadline += slot_s

    assert overruns == 0, f"{overruns} frames se salieron de su hueco de {slot_s * 1000:.0f} ms"
