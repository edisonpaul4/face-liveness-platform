"""El contrato del bus, sujeto por los vectores dorados que escribió Go."""

from __future__ import annotations

import base64
import json

import pytest

from analyzer import wire
from tests.conftest import REPO_ROOT

TESTDATA = REPO_ROOT / "proto" / "testdata"


def test_decodifica_el_frame_que_escribio_go():
    """El vector dorado viene de `gateway/internal/bus/codec.go`."""
    raw = (TESTDATA / "frame_task.bin").read_bytes()
    expected = json.loads((TESTDATA / "frame_task.expected.json").read_text())

    task = wire.decode_frame_task(raw)

    assert task.seq == expected["seq"]
    assert task.encoding == expected["encoding"]
    assert task.flags == expected["flags"]
    assert task.captured_at_us == expected["captured_at_us"]
    assert task.received_at_us == expected["received_at_us"]
    assert task.client_clock_trusted is expected["client_clock_trusted"]
    assert base64.b64encode(task.payload).decode() == expected["payload_base64"]


def test_reescribe_el_frame_byte_a_byte():
    """Si Python volviera a codificarlo, saldría lo mismo que escribió Go."""
    raw = (TESTDATA / "frame_task.bin").read_bytes()
    assert wire.encode_frame_task(wire.decode_frame_task(raw)) == raw


@pytest.mark.parametrize(
    "fixture, decoder, checks",
    [
        ("lease_request.json", wire.LeaseRequest.decode,
         lambda m: (m.session_id == "01J0TESTSESSION0000000000", m.deadline_us > 0)),
        ("session_control.json", wire.SessionControl.decode,
         lambda m: (m.closes_session, m.kind == "session_close")),
    ],
)
def test_decodifica_los_mensajes_de_go(fixture, decoder, checks):
    message = decoder((TESTDATA / fixture).read_bytes())
    assert all(checks(message))


def test_las_medidas_encajan_con_el_ejemplo_de_go():
    """Lo que Python emite tiene la forma que Go espera leer."""
    canonical = json.loads((TESTDATA / "frame_features.json").read_text())

    mine = wire.FrameFeatures(
        seq=canonical["seq"],
        signals=canonical["signals"],
        quality=wire.Quality(
            face_detected=canonical["quality"]["face_detected"],
            face_count=canonical["quality"]["face_count"],
            sharpness=canonical["quality"]["sharpness"],
            brightness=canonical["quality"]["brightness"],
        ),
        processing_ms=canonical["processing_ms"],
        version=canonical["version"],
        analyzed_at_us=canonical["analyzed_at_us"],
    )

    emitted = json.loads(wire.encode_features(mine))
    for key in ("seq", "signals", "quality", "processing_ms", "version", "analyzed_at_us"):
        assert emitted[key] == canonical[key], f"el campo {key} no coincide con lo que espera Go"


def test_rechaza_frames_rotos():
    valid = (TESTDATA / "frame_task.bin").read_bytes()

    with pytest.raises(wire.WireError, match="más corto"):
        wire.decode_frame_task(valid[:10])

    bad_version = bytearray(valid)
    bad_version[0] = 9
    with pytest.raises(wire.WireError, match="versión"):
        wire.decode_frame_task(bytes(bad_version))

    bad_length = bytearray(valid)
    bad_length[31] = 200
    with pytest.raises(wire.WireError, match="longitud"):
        wire.decode_frame_task(bytes(bad_length))


def test_no_deja_emitir_juicios():
    """La frontera Go/Python, defendida por el codec.

    Es fácil que a alguien se le escape un `is_live` en las señales. Aquí se
    rompe con estruendo en vez de cruzar la frontera sin hacer ruido.
    """
    features = wire.FrameFeatures(
        seq=1,
        signals={"is_live": 1.0},
        quality=wire.Quality(True, 1, 10.0, 0.5),
        processing_ms=5,
        version="test",
        analyzed_at_us=1,
    )
    with pytest.raises(wire.WireError, match="no decide"):
        wire.encode_features(features)


def test_los_subjects_atan_cada_sesion_a_los_suyos():
    assert wire.frames_subject("01ABC") == "session.01ABC.frames"
    assert wire.features_subject("01ABC") == "session.01ABC.features"
    assert wire.heartbeat_subject("01ABC") == "session.01ABC.heartbeat"
    assert wire.control_subject("01ABC") == "session.01ABC.control"
    assert wire.lease_subject("w1") == "analyzer.lease.w1"


@pytest.mark.parametrize("token", ["", "a.b", "*", ">", "ses ion", "a\tb", "x" * 65])
def test_rechaza_identificadores_que_rompen_el_ruteo(token):
    """Un `>` colado en un subject es una suscripción a sesiones ajenas."""
    assert not wire.valid_token(token)


@pytest.mark.parametrize("token", ["01J0ABC", "analyzer-1", "w_2"])
def test_acepta_identificadores_validos(token):
    assert wire.valid_token(token)


def test_session_end_es_alias_de_session_close():
    """El contrato dice session_close; se acepta también session_end."""
    assert wire.SessionControl(kind="session_close").closes_session
    assert wire.SessionControl(kind="session_end").closes_session
    assert not wire.SessionControl(kind="otra_cosa").closes_session
