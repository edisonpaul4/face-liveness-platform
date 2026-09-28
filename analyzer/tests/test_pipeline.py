"""El pipeline y el contrato de las medidas."""

from __future__ import annotations

import logging

import pytest

from analyzer import frames, wire
from analyzer.detector import Detection, FaceBox
from analyzer.metrics import LatencyBudget
from analyzer.pipeline import Pipeline
from tests import contract
from tests.conftest import StubDetector, draw_scene, encode_jpeg, make_task


def test_emite_medidas_validas_sin_rostro(stub_pipeline, session_state):
    outcome = stub_pipeline.process(make_task(encode_jpeg(draw_scene(face=False))), session_state)
    features = outcome.features.to_dict()

    contract.assert_valid(features)
    assert features["signals"]["face_count"] == 0.0
    assert features["signals"]["face_present"] == 0.0
    assert features["quality"]["face_detected"] is False
    assert "face" not in features


def test_emite_la_caja_cuando_hay_rostro(session_state):
    detector = StubDetector(Detection(faces=(FaceBox(0.3, 0.25, 0.25, 0.4, 0.87),)))
    pipeline = Pipeline(detector, budget=LatencyBudget("test"))

    outcome = pipeline.process(make_task(encode_jpeg(draw_scene())), session_state)
    features = outcome.features.to_dict()

    contract.assert_valid(features)
    assert features["signals"]["face_count"] == 1.0
    assert features["signals"]["face_present"] == 1.0
    assert features["face"]["x"] == pytest.approx(0.3)
    assert features["face"]["confidence"] == pytest.approx(0.87)
    assert features["signals"]["face_box_area"] == pytest.approx(0.1)


def test_cuenta_varios_rostros(session_state):
    """Cuántos hay es un dato; qué significa que haya dos, no es cosa suya."""
    faces = (FaceBox(0.1, 0.1, 0.2, 0.2, 0.9), FaceBox(0.6, 0.2, 0.3, 0.35, 0.8))
    pipeline = Pipeline(StubDetector(Detection(faces=faces)), budget=LatencyBudget("test"))

    outcome = pipeline.process(make_task(encode_jpeg(draw_scene())), session_state)
    features = outcome.features.to_dict()

    contract.assert_valid(features)
    assert features["signals"]["face_count"] == 2.0
    assert features["quality"]["face_count"] == 2
    # El principal es el más grande.
    assert features["face"]["x"] == pytest.approx(0.6)


def test_el_seq_del_frame_viaja_en_las_medidas(stub_pipeline, session_state):
    outcome = stub_pipeline.process(make_task(encode_jpeg(draw_scene()), seq=4242), session_state)
    assert outcome.features.seq == 4242


def test_acumula_estado_entre_frames(stub_pipeline, session_state):
    payload = encode_jpeg(draw_scene())
    for seq in range(1, 6):
        outcome = stub_pipeline.process(make_task(payload, seq), session_state)

    assert outcome.features.signals["temporal_frames_seen"] == 5.0
    assert outcome.features.signals["temporal_window_frames"] == 5.0


def test_un_frame_ilegible_es_un_error_acotado(stub_pipeline, session_state):
    """No tumba la sesión: el llamante lo cuenta y sigue."""
    task = wire.FrameTask(seq=1, encoding=wire.ENCODING_JPEG, payload=b"basura", received_at_us=0)

    with pytest.raises(frames.DecodeError):
        stub_pipeline.process(task, session_state)

    assert session_state.frames == 0


def test_mide_las_etapas(stub_pipeline, session_state):
    outcome = stub_pipeline.process(make_task(encode_jpeg(draw_scene())), session_state)

    assert set(outcome.stages) == {
        "decode", "detect", "quality", "photometry", "gaze", "pad", "pose",
    }
    assert all(ms >= 0 for ms in outcome.stages.values())
    assert outcome.features.processing_ms >= 0


def test_avisa_cuando_se_pasa_del_presupuesto(session_state, caplog):
    """Pasarse no es un fallo, es un aviso. Pero tiene que verse."""
    import time

    class SlowDetector(StubDetector):
        def detect(self, bgr):  # noqa: ANN001, ANN201
            time.sleep(0.02)
            return super().detect(bgr)

    log = logging.getLogger("presupuesto")
    pipeline = Pipeline(SlowDetector(), budget=LatencyBudget("test", budget_ms=5.0, logger=log))

    with caplog.at_level(logging.WARNING, logger="presupuesto"):
        outcome = pipeline.process(make_task(encode_jpeg(draw_scene())), session_state)

    assert outcome.over_budget
    assert "fuera de presupuesto" in caplog.text
    # El desglose por etapas: sin él, "tardó 25 ms" no arregla nada.
    assert "detect" in caplog.text


def test_no_avisa_cuando_va_sobrado(stub_pipeline, session_state, caplog):
    with caplog.at_level(logging.WARNING):
        outcome = stub_pipeline.process(make_task(encode_jpeg(draw_scene())), session_state)

    assert not outcome.over_budget
    assert "fuera de presupuesto" not in caplog.text


def test_las_medidas_pasan_por_el_codec(stub_pipeline, session_state):
    """Lo que emite el pipeline tiene que poder salir por el bus."""
    outcome = stub_pipeline.process(make_task(encode_jpeg(draw_scene())), session_state)
    assert wire.encode_features(outcome.features)


def test_no_emite_ningun_juicio(mediapipe_pipeline, session_state):
    """La frontera: aquí sólo salen magnitudes (CLAUDE.md §3)."""
    outcome = mediapipe_pipeline.process(make_task(encode_jpeg(draw_scene())), session_state)

    for name in outcome.features.signals:
        assert name not in wire.FORBIDDEN_SIGNAL_NAMES
        for judgment in ("live", "spoof", "attack", "fake", "real", "verdict", "score_final"):
            assert judgment not in name, f"la señal {name} suena a conclusión, no a medida"
