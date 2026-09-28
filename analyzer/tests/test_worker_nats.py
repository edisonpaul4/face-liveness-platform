"""El worker contra un NATS de verdad.

Hace el papel del gateway: anuncia, asigna, manda frames y recoge medidas. Es
la prueba de que este worker habla el mismo protocolo que la implementación de
Go, no una versión parecida.

Se salta si no hay NATS a mano; `make up` lo levanta.
"""

from __future__ import annotations

import asyncio
import json
import os

import pytest

from analyzer import wire
from analyzer.config import Config
from analyzer.metrics import LatencyBudget
from analyzer.pipeline import Pipeline
from analyzer.worker import Worker
from tests import contract
from tests.conftest import StubDetector, draw_scene, encode_jpeg

pytestmark = pytest.mark.asyncio

NATS_URL = os.getenv("NATS_URL", "nats://localhost:4222")
SESSION_ID = "01J0PYTESTSESSION"


async def _connect():
    import nats

    try:
        return await asyncio.wait_for(nats.connect(servers=[NATS_URL]), timeout=2.0)
    except Exception:  # noqa: BLE001
        pytest.skip(f"no hay NATS en {NATS_URL}; levántalo con `make up`")


@pytest.fixture
async def gateway():
    """Conexión que hace de gateway."""
    nc = await _connect()
    yield nc
    await nc.close()


@pytest.fixture
async def worker(request):
    """Un worker corriendo, con detector de mentira para no depender del modelo."""
    from analyzer.detector import Detection, FaceBox

    config = Config(
        nats_url=NATS_URL,
        worker_id="pytest-worker",
        capacity=2,
        announce_interval_s=0.1,
        heartbeat_interval_s=0.1,
        sweep_interval_s=0.2,
        session_idle_ttl_s=getattr(request, "param", 60.0),
    )
    detector = StubDetector(Detection(faces=(FaceBox(0.3, 0.25, 0.25, 0.4, 0.9),)))
    instance = Worker(config, detector)
    # El pipeline con detector de mentira: lo que se prueba aquí es el
    # protocolo, no la visión por computador.
    instance._pipeline = Pipeline(detector, budget=LatencyBudget(config.worker_id))

    stop = asyncio.Event()
    task = asyncio.create_task(instance.run(stop))
    await asyncio.sleep(0.3)

    yield instance

    stop.set()
    await asyncio.wait_for(task, timeout=5)


async def _lease(gateway, session_id: str = SESSION_ID, deadline_us: int = 0) -> wire.LeaseReply:
    payload = json.dumps({"session_id": session_id, "deadline_us": deadline_us}).encode()
    msg = await gateway.request(wire.lease_subject("pytest-worker"), payload, timeout=2.0)
    raw = json.loads(msg.data)
    return wire.LeaseReply(
        accepted=raw.get("accepted", False),
        worker_id=raw.get("worker_id", ""),
        reason=raw.get("reason", ""),
    )


def _frame(seq: int = 1) -> bytes:
    return wire.encode_frame_task(
        wire.FrameTask(
            seq=seq,
            encoding=wire.ENCODING_JPEG,
            payload=encode_jpeg(draw_scene()),
            received_at_us=1_787_500_000_000_000 + seq * 66_000,
        )
    )


async def test_se_anuncia_con_su_capacidad(gateway, worker):
    """El descubrimiento del gateway no funciona sin esto."""
    received: asyncio.Queue = asyncio.Queue()
    await gateway.subscribe(wire.SUBJECT_ANNOUNCE, cb=received.put)
    await gateway.flush()

    # El subject de anuncios es global, así que en la misma máquina puede
    # haber otros workers hablando —uno de desarrollo, por ejemplo—. Se
    # descartan los ajenos en vez de dar por hecho que el primero es el
    # nuestro: si no, el test falla según qué esté corriendo al lado.
    announce = None
    deadline = asyncio.get_running_loop().time() + 3.0
    while announce is None:
        restante = deadline - asyncio.get_running_loop().time()
        if restante <= 0:
            raise AssertionError("el worker de prueba no se anunció")
        msg = await asyncio.wait_for(received.get(), timeout=restante)
        candidato = json.loads(msg.data)
        if candidato.get("worker_id") == "pytest-worker":
            announce = candidato

    assert announce["worker_id"] == "pytest-worker"
    assert announce["capacity"] == 2
    assert announce["sessions"] == 0
    assert announce["version"]


async def test_acepta_el_lease_y_devuelve_medidas(gateway, worker):
    """El recorrido completo: asignar, mandar un frame, recoger medidas."""
    features: asyncio.Queue = asyncio.Queue()
    await gateway.subscribe(wire.features_subject(SESSION_ID), cb=features.put)
    await gateway.flush()

    reply = await _lease(gateway)
    assert reply.accepted, reply.reason
    assert reply.worker_id == "pytest-worker"

    await gateway.publish(wire.frames_subject(SESSION_ID), _frame(1))
    await gateway.flush()

    msg = await asyncio.wait_for(features.get(), timeout=3.0)
    payload = json.loads(msg.data)

    contract.assert_valid(payload)
    assert payload["seq"] == 1
    assert payload["quality"]["face_detected"] is True
    assert payload["face"]["x"] == pytest.approx(0.3)


async def test_manda_heartbeats_mientras_la_sesion_vive(gateway, worker):
    """Si esto deja de llegar, el gateway aborta la sesión."""
    beats: asyncio.Queue = asyncio.Queue()
    await gateway.subscribe(wire.heartbeat_subject(SESSION_ID), cb=beats.put)
    await gateway.flush()

    assert (await _lease(gateway)).accepted

    msg = await asyncio.wait_for(beats.get(), timeout=3.0)
    beat = json.loads(msg.data)
    assert beat["worker_id"] == "pytest-worker"
    assert beat["session_id"] == SESSION_ID

    # Y siguen llegando, no es uno solo.
    await asyncio.wait_for(beats.get(), timeout=3.0)


async def test_el_estado_caliente_se_acumula(gateway, worker):
    """Los frames llegan pelados: lo que se sabe de la sesión está aquí."""
    features: asyncio.Queue = asyncio.Queue()
    await gateway.subscribe(wire.features_subject(SESSION_ID), cb=features.put)
    await gateway.flush()

    assert (await _lease(gateway)).accepted

    last = None
    for seq in range(1, 6):
        await gateway.publish(wire.frames_subject(SESSION_ID), _frame(seq))
        await gateway.flush()
        msg = await asyncio.wait_for(features.get(), timeout=3.0)
        last = json.loads(msg.data)

    assert last["signals"]["temporal_frames_seen"] == 5.0
    assert worker._sessions.get(SESSION_ID).frames == 5


async def test_session_close_suelta_el_estado(gateway, worker):
    assert (await _lease(gateway)).accepted
    assert SESSION_ID in worker._sessions

    await gateway.publish(
        wire.control_subject(SESSION_ID),
        json.dumps({"kind": "session_close", "session_id": SESSION_ID}).encode(),
    )
    await gateway.flush()

    for _ in range(30):
        if SESSION_ID not in worker._sessions:
            break
        await asyncio.sleep(0.05)

    assert SESSION_ID not in worker._sessions


@pytest.mark.parametrize("worker", [0.3], indirect=True)
async def test_el_ttl_suelta_las_sesiones_abandonadas(gateway, worker):
    """Cubre que el aviso de cierre se pierda: con un bus efímero, pasa."""
    assert (await _lease(gateway)).accepted
    assert SESSION_ID in worker._sessions

    for _ in range(40):
        if SESSION_ID not in worker._sessions:
            break
        await asyncio.sleep(0.05)

    assert SESSION_ID not in worker._sessions, "el estado caliente se quedó para siempre"


async def test_rechaza_cuando_no_le_caben_mas_sesiones(gateway, worker):
    """Capacidad 2: la tercera sobra."""
    assert (await _lease(gateway, "01J0CAP1")).accepted
    assert (await _lease(gateway, "01J0CAP2")).accepted

    third = await _lease(gateway, "01J0CAP3")
    assert not third.accepted
    assert "capacidad" in third.reason


async def test_rechaza_la_misma_sesion_dos_veces(gateway, worker):
    """Aceptarla dos veces mezclaría dos flujos en un solo estado."""
    assert (await _lease(gateway)).accepted

    again = await _lease(gateway)
    assert not again.accepted
    assert "asignada" in again.reason


async def test_rechaza_identificadores_que_rompen_el_ruteo(gateway, worker):
    reply = await _lease(gateway, "sesion.con.puntos")
    assert not reply.accepted
    assert "inválida" in reply.reason


async def test_un_frame_ilegible_no_tumba_la_sesion(gateway, worker):
    """El cliente es hostil por definición: puede mandar cualquier cosa."""
    features: asyncio.Queue = asyncio.Queue()
    await gateway.subscribe(wire.features_subject(SESSION_ID), cb=features.put)
    await gateway.flush()

    assert (await _lease(gateway)).accepted

    await gateway.publish(wire.frames_subject(SESSION_ID), b"esto no es un frame")
    await gateway.publish(wire.frames_subject(SESSION_ID), _frame(7))
    await gateway.flush()

    msg = await asyncio.wait_for(features.get(), timeout=3.0)
    assert json.loads(msg.data)["seq"] == 7
    assert SESSION_ID in worker._sessions


async def test_ignora_frames_de_sesiones_que_no_lleva(gateway, worker):
    features: asyncio.Queue = asyncio.Queue()
    await gateway.subscribe(wire.features_subject("01J0AJENA"), cb=features.put)
    await gateway.flush()

    await gateway.publish(wire.frames_subject("01J0AJENA"), _frame(1))
    await gateway.flush()

    with pytest.raises(asyncio.TimeoutError):
        await asyncio.wait_for(features.get(), timeout=0.5)


async def test_una_rafaga_se_descarta_en_vez_de_encolarse(gateway, worker):
    """Backpressure en el worker.

    Si llegan frames más rápido de lo que se analizan, NATS descarta los que
    no caben en la cola diminuta de la suscripción. Encolarlos sólo serviría
    para analizar el pasado: cuando le tocara el turno a un frame viejo, ya no
    describiría lo que está pasando.
    """
    import time as _time

    class SlowDetector(StubDetector):
        def detect(self, bgr):  # noqa: ANN001, ANN201
            _time.sleep(0.02)
            return super().detect(bgr)

    worker._pipeline = Pipeline(SlowDetector(), budget=LatencyBudget("test"))

    features: asyncio.Queue = asyncio.Queue()
    await gateway.subscribe(wire.features_subject(SESSION_ID), cb=features.put)
    await gateway.flush()
    assert (await _lease(gateway)).accepted

    sent = 30
    for seq in range(1, sent + 1):
        await gateway.publish(wire.frames_subject(SESSION_ID), _frame(seq))
    await gateway.flush()
    await asyncio.sleep(1.0)

    received = features.qsize()
    assert received < sent, f"se analizaron los {sent} frames de la ráfaga: no hubo descarte"
    assert received > 0, "no se analizó ninguno"

    # Y la sesión sigue viva: descartar frames no la rompe.
    assert SESSION_ID in worker._sessions


async def test_devuelve_el_score_de_una_ventana_de_pose(gateway, worker):
    """Go delimita la ventana, el worker devuelve las sub-métricas.

    Lo que va de ida no lleva reto, ni posición en el guion, ni umbral: sólo
    un eje, un ángulo y un intervalo de tiempo.
    """
    import numpy as np

    from analyzer.pose import HeadPose
    from analyzer.sessions import PoseSample

    scores: asyncio.Queue = asyncio.Queue()
    await gateway.subscribe(wire.challenge_score_subject(SESSION_ID), cb=scores.put)
    await gateway.flush()

    assert (await _lease(gateway)).accepted

    # Se inyecta una serie de pose como la que dejarían 16 frames de un giro.
    state = worker._sessions.get(SESSION_ID)
    for i, yaw in enumerate(np.linspace(0, 28, 16)):
        state.record(
            PoseSample(
                seq=i + 1,
                at_us=1_000_000 + i * 66_000,
                pose=HeadPose(yaw=float(yaw), pitch=0.0, roll=0.0),
                scale=0.4,
            )
        )

    await gateway.publish(
        wire.window_subject(SESSION_ID),
        json.dumps(
            {
                "window_id": "win-1",
                "axis": "yaw",
                "target_deg": 25.0,
                "tolerance_deg": 5.0,
                "started_at_us": 1_000_000,
                "ended_at_us": 1_000_000 + 16 * 66_000,
            }
        ).encode(),
    )
    await gateway.flush()

    msg = await asyncio.wait_for(scores.get(), timeout=3.0)
    payload = json.loads(msg.data)

    assert payload["window_id"] == "win-1"
    assert payload["axis"] == "yaw"
    assert 0.0 <= payload["score"] <= 1.0
    assert set(payload["submetrics"]) == {"compliance", "continuity", "parallax", "identity"}
    assert payload["submetrics"]["compliance"] == pytest.approx(1.0)
    assert payload["submetrics"]["continuity"] == pytest.approx(1.0)
    # Sin landmarks ni embeddings inyectados, esas dos no se pueden medir.
    assert payload["submetrics"]["parallax"] is None
    assert payload["submetrics"]["identity"] is None
    assert payload["frames_used"] == 16
    assert payload["raw"]["peak_deg"] == pytest.approx(28.0, abs=0.1)

    # Y no sale ningún veredicto: eso lo decide Go.
    for forbidden in ("passed", "verdict", "decision", "live", "spoof"):
        assert forbidden not in json.dumps(payload).lower()


async def test_una_ventana_ilegible_no_tumba_la_sesion(gateway, worker):
    scores: asyncio.Queue = asyncio.Queue()
    await gateway.subscribe(wire.challenge_score_subject(SESSION_ID), cb=scores.put)
    await gateway.flush()
    assert (await _lease(gateway)).accepted

    await gateway.publish(wire.window_subject(SESSION_ID), b"esto no es una ventana")
    await gateway.flush()

    with pytest.raises(asyncio.TimeoutError):
        await asyncio.wait_for(scores.get(), timeout=0.5)
    assert SESSION_ID in worker._sessions


async def test_calibra_y_analiza_un_destello(gateway, worker):
    """El recorrido del destello por el bus: calibrar y luego medir.

    Go delimita las dos ventanas; el worker guarda la línea base de la
    primera y mide la segunda contra ella.
    """
    import numpy as np

    from analyzer.photometry import FramePhotometry
    from analyzer.pose import HeadPose
    from analyzer.sessions import PoseSample

    scores: asyncio.Queue = asyncio.Queue()
    await gateway.subscribe(wire.challenge_score_subject(SESSION_ID), cb=scores.put)
    await gateway.flush()
    assert (await _lease(gateway)).accepted

    state = worker._sessions.get(SESSION_ID)
    base_level = np.array([[90.0, 100.0, 110.0]] * 4)
    background = np.array([120.0, 120.0, 120.0])

    def photometry(regions):
        return FramePhotometry(
            regions=regions, background=background, face_luminance=100.0,
            specular_ratio=0.0, luminance_ratio=0.85, moire_index=0.01, banding_index=0.01,
        )

    # 12 frames de pantalla neutra: la calibración.
    calibration_start = 1_000_000
    for i in range(12):
        state.record(PoseSample(
            seq=i + 1, at_us=calibration_start + i * 50_000,
            pose=HeadPose(0.0, 0.0, 0.0), scale=0.4, photometry=photometry(base_level),
        ))
    calibration_end = calibration_start + 12 * 50_000

    await gateway.publish(wire.window_subject(SESSION_ID), json.dumps({
        "kind": "calibration", "window_id": "cal-1",
        "started_at_us": calibration_start, "ended_at_us": calibration_end,
    }).encode())
    await gateway.flush()

    msg = await asyncio.wait_for(scores.get(), timeout=3.0)
    payload = json.loads(msg.data)
    assert payload["kind"] == "calibration"
    assert payload["quality_sufficient"] is True
    assert payload["raw"]["frames"] == 12.0
    assert state.baseline is not None

    # Y ahora el destello: cada región responde con su propia intensidad.
    sequence = [{"color": "white", "duration_ms": 300}, {"color": "red", "duration_ms": 250},
                {"color": "blue", "duration_ms": 300}]
    gains = np.array([1.0, 1.5, 0.7, 0.75])
    flash_start = calibration_end + 100_000

    from analyzer import flash as flash_module

    spec = flash_module.FlashWindowSpec(
        "flash-1",
        tuple(flash_module.FlashSegment(s["color"], s["duration_ms"]) for s in sequence),
        flash_start, flash_start + 2_000_000,
    )
    for i in range(30):
        at = flash_start + i * 50_000
        color = flash_module.stimulus_at(spec, at, 0)
        lit = base_level.copy()
        if color is not None:
            for region in range(4):
                lit[region] = base_level[region] * (1 + gains[region] * color * 0.6)
        state.record(PoseSample(
            seq=100 + i, at_us=at, pose=HeadPose(0.0, 0.0, 0.0), scale=0.4,
            photometry=photometry(lit),
        ))

    await gateway.publish(wire.window_subject(SESSION_ID), json.dumps({
        "kind": "flash", "window_id": "flash-1", "sequence": sequence,
        "started_at_us": flash_start, "ended_at_us": flash_start + 30 * 50_000,
    }).encode())
    await gateway.flush()

    msg = await asyncio.wait_for(scores.get(), timeout=3.0)
    payload = json.loads(msg.data)

    assert payload["kind"] == "flash"
    assert payload["window_id"] == "flash-1"
    assert payload["quality_sufficient"] is True
    assert set(payload["submetrics"]) == {"correlation", "gradient_3d", "screen_absence"}
    assert payload["submetrics"]["correlation"] > 0.9
    assert payload["submetrics"]["gradient_3d"] > 0.8
    assert 0.0 <= payload["score"] <= 1.0

    # Y ningún veredicto: eso lo decide Go.
    for forbidden in ("passed", "verdict", "decision", "live", "spoof"):
        assert forbidden not in json.dumps(payload).lower()


async def test_un_destello_sin_calibracion_previa_da_calidad_insuficiente(gateway, worker):
    """La calibración es obligatoria: sin ella no hay contra qué comparar."""
    scores: asyncio.Queue = asyncio.Queue()
    await gateway.subscribe(wire.challenge_score_subject(SESSION_ID), cb=scores.put)
    await gateway.flush()
    assert (await _lease(gateway)).accepted

    await gateway.publish(wire.window_subject(SESSION_ID), json.dumps({
        "kind": "flash", "window_id": "flash-sin-base",
        "sequence": [{"color": "white", "duration_ms": 300}],
        "started_at_us": 1_000_000, "ended_at_us": 3_000_000,
    }).encode())
    await gateway.flush()

    msg = await asyncio.wait_for(scores.get(), timeout=3.0)
    payload = json.loads(msg.data)

    assert payload["quality_sufficient"] is False
    assert "línea base" in payload["quality_reason"]
    assert all(v is None for v in payload["submetrics"].values())
