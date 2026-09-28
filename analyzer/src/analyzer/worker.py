"""Worker de análisis: consumidor NATS asíncrono.

Ciclo de vida (contrato completo en /proto/subjects.md):

1. Se anuncia en `analyzer.announce` con capacidad y carga.
2. Atiende `analyzer.lease.<worker_id>`: acepta o rechaza hacerse cargo de una
   sesión. Al aceptar, crea su estado caliente EN MEMORIA.
3. Se suscribe a `session.<id>.frames`, analiza y publica en
   `session.<id>.features`.
4. Renueva el lease con `session.<id>.heartbeat` mientras la sesión viva.
5. Suelta el estado al recibir `session_close`, o por TTL.

Lo que este servicio NO hace, y no es un olvido:
    * No sabe qué es un WebSocket.
    * No sabe qué reto está activo, ni que existen los retos.
    * No emite veredictos ni probabilidades de ataque.

Sobre el bucle de eventos: el análisis es CPU y bloquea el bucle unos
milisegundos por frame. Es deliberado. Sacarlo a otro proceso obligaría a
mover el estado caliente con cada frame, que es justo lo que la afinidad de
sesión evita. El paralelismo se consigue con VARIOS PROCESOS worker, cada uno
con su bucle (ver `analyzer.supervisor`).
"""

from __future__ import annotations

import asyncio
import contextlib
import functools
import logging
import time

import nats
from nats.aio.msg import Msg

from analyzer import calibration, flash, rppg, window, wire
from analyzer import frames as frame_codec
from analyzer import metrics as m
from analyzer.config import Config
from analyzer.detector import DetectorError, FaceDetector
from analyzer.metrics import LatencyBudget
from analyzer.pad import PadModel
from analyzer.pipeline import Pipeline
from analyzer.sessions import SessionStore

log = logging.getLogger("analyzer.worker")


class Worker:
    """Atiende sesiones de análisis sobre NATS."""

    def __init__(self, config: Config, detector: FaceDetector, embedder=None) -> None:  # noqa: ANN001
        self._cfg = config
        self._detector = detector
        self._sessions = SessionStore(idle_ttl_s=config.session_idle_ttl_s)
        self._pipeline = Pipeline(
            detector,
            budget=LatencyBudget(config.worker_id, config.latency_budget_ms, log),
            embedder=embedder,
            embedding_stride=config.embedding_stride,
            pad=build_pad(config, log),
        )
        self._score_config = config.score_config()
        self._flash_config = flash.FlashConfig()
        self._nc: nats.NATS | None = None
        self._session_subs: dict[str, list] = {}
        self._tasks: list[asyncio.Task] = []
        self._in_flight = 0

    async def run(self, stop: asyncio.Event) -> None:
        """Conecta, sirve y se retira limpiamente."""
        self._nc = await nats.connect(
            servers=[self._cfg.nats_url],
            name=f"liveness-analyzer-{self._cfg.worker_id}",
            max_reconnect_attempts=-1,
            reconnect_time_wait=0.5,
            error_cb=self._on_error,
        )
        log.info(
            "worker %s conectado a %s (detector=%s, capacidad=%d)",
            self._cfg.worker_id,
            self._cfg.nats_url,
            self._detector.name,
            self._cfg.capacity,
        )

        await self._nc.subscribe(wire.lease_subject(self._cfg.worker_id), cb=self._on_lease)
        await self._announce()
        await self._nc.flush()

        self._tasks = [
            asyncio.create_task(self._announce_loop(stop), name="announce"),
            asyncio.create_task(self._sweep_loop(stop), name="sweep"),
        ]

        try:
            await stop.wait()
        finally:
            await self._shutdown()

    async def _shutdown(self) -> None:
        for task in self._tasks:
            task.cancel()
        for task in self._tasks:
            with contextlib.suppress(asyncio.CancelledError):
                await task

        for session_id in list(self._session_subs):
            await self._release(session_id, reason="shutdown")

        if self._nc is not None:
            with contextlib.suppress(Exception):
                await self._nc.drain()
        self._detector.close()
        log.info("worker %s retirado", self._cfg.worker_id)

    # --- descubrimiento ------------------------------------------------------

    async def _announce_loop(self, stop: asyncio.Event) -> None:
        """Se anuncia periódicamente. Un worker que calla deja de existir."""
        while not stop.is_set():
            await asyncio.sleep(self._cfg.announce_interval_s)
            with contextlib.suppress(Exception):
                await self._announce()

    async def _announce(self) -> None:
        assert self._nc is not None
        payload = wire.Announce(
            worker_id=self._cfg.worker_id,
            capacity=self._cfg.capacity,
            sessions=len(self._sessions),
            in_flight=self._in_flight,
            version=self._pipeline.version,
            emitted_at_us=_now_us(),
        ).encode()
        await self._nc.publish(wire.SUBJECT_ANNOUNCE, payload)

    # --- lease ---------------------------------------------------------------

    async def _on_lease(self, msg: Msg) -> None:
        """Acepta o rechaza hacerse cargo de una sesión."""
        assert self._nc is not None
        worker_id = self._cfg.worker_id

        try:
            request = wire.LeaseRequest.decode(msg.data)
        except wire.WireError as exc:
            log.warning("lease ilegible: %s", exc)
            m.LEASES_REJECTED.labels(worker_id, "malformed").inc()
            await self._reply(msg, wire.LeaseReply(False, worker_id, "petición ilegible"))
            return

        if not wire.valid_token(request.session_id):
            m.LEASES_REJECTED.labels(worker_id, "invalid_id").inc()
            await self._reply(msg, wire.LeaseReply(False, worker_id, "sesión inválida"))
            return

        if len(self._sessions) >= self._cfg.capacity:
            m.LEASES_REJECTED.labels(worker_id, "at_capacity").inc()
            await self._reply(msg, wire.LeaseReply(False, worker_id, "sin capacidad"))
            return

        try:
            self._sessions.open(request.session_id, request.deadline_us)
        except KeyError:
            m.LEASES_REJECTED.labels(worker_id, "duplicate").inc()
            await self._reply(msg, wire.LeaseReply(False, worker_id, "sesión ya asignada"))
            return

        try:
            await self._attach(request.session_id)
        except Exception as exc:  # noqa: BLE001 - hay que contestar igualmente
            log.error("no se pudo atender la sesión %s: %s", request.session_id, exc)
            self._sessions.close(request.session_id)
            m.LEASES_REJECTED.labels(worker_id, "attach_failed").inc()
            await self._reply(msg, wire.LeaseReply(False, worker_id, "no se pudo suscribir"))
            return

        m.SESSIONS_TOTAL.labels(worker_id).inc()
        m.SESSIONS_OPEN.labels(worker_id).set(len(self._sessions))
        log.info("sesión %s aceptada", request.session_id)
        await self._reply(msg, wire.LeaseReply(True, worker_id))

    async def _reply(self, msg: Msg, reply: wire.LeaseReply) -> None:
        if msg.reply:
            with contextlib.suppress(Exception):
                await msg.respond(reply.encode())

    async def _attach(self, session_id: str) -> None:
        """Abre las suscripciones de la sesión y su heartbeat."""
        assert self._nc is not None

        frames_sub = await self._nc.subscribe(
            wire.frames_subject(session_id),
            cb=functools.partial(self._on_frame, session_id),
            # Cola diminuta: si el análisis va por detrás, NATS descarta los
            # frames viejos en vez de acumularlos. Un frame viejo ya no
            # describe lo que está pasando.
            pending_msgs_limit=self._cfg.frame_queue,
            pending_bytes_limit=self._cfg.frame_queue * self._cfg.max_frame_bytes,
        )
        control_sub = await self._nc.subscribe(
            wire.control_subject(session_id),
            cb=functools.partial(self._on_control, session_id),
        )
        window_sub = await self._nc.subscribe(
            wire.window_subject(session_id),
            cb=functools.partial(self._on_window, session_id),
        )
        self._session_subs[session_id] = [frames_sub, control_sub, window_sub]

        task = asyncio.create_task(self._heartbeat_loop(session_id), name=f"hb-{session_id}")
        self._tasks.append(task)
        await self._nc.flush()

    async def _heartbeat_loop(self, session_id: str) -> None:
        """Renueva el lease mientras la sesión siga viva.

        Si esto deja de llegar, el gateway ABORTA la sesión. No se intenta
        recuperar nada: dura segundos y se repite desde cero (CLAUDE.md §4).
        """
        assert self._nc is not None
        while session_id in self._session_subs:
            await asyncio.sleep(self._cfg.heartbeat_interval_s)
            state = self._sessions.get(session_id)
            if state is None:
                return
            with contextlib.suppress(Exception):
                await self._nc.publish(
                    wire.heartbeat_subject(session_id),
                    wire.SessionHeartbeat(
                        worker_id=self._cfg.worker_id,
                        session_id=session_id,
                        in_flight=self._in_flight,
                        emitted_at_us=_now_us(),
                    ).encode(),
                )

    # --- frames --------------------------------------------------------------

    async def _on_frame(self, session_id: str, msg: Msg) -> None:
        """Analiza un frame y publica sus medidas.

        El frame llega pelado: ni sesión, ni reto, ni estado. Lo que se sabe de
        la sesión está en memoria desde que se aceptó el lease.
        """
        worker_id = self._cfg.worker_id
        m.FRAMES_TOTAL.labels(worker_id).inc()

        state = self._sessions.get(session_id)
        if state is None:
            # Frame de una sesión ya cerrada: se descarta sin ruido.
            m.FRAMES_FAILED.labels(worker_id, "unknown_session").inc()
            return

        try:
            task = wire.decode_frame_task(msg.data)
        except wire.WireError as exc:
            log.warning("frame ilegible en %s: %s", session_id, exc)
            m.FRAMES_FAILED.labels(worker_id, "bad_frame").inc()
            return

        self._in_flight += 1
        try:
            outcome = self._pipeline.process(task, state)
        except frame_codec.DecodeError as exc:
            log.warning("no se pudo decodificar la imagen en %s: %s", session_id, exc)
            m.FRAMES_FAILED.labels(worker_id, "undecodable").inc()
            return
        except Exception as exc:  # noqa: BLE001 - un frame malo no tumba la sesión
            log.exception("fallo analizando un frame de %s: %s", session_id, exc)
            m.FRAMES_FAILED.labels(worker_id, "error").inc()
            return
        finally:
            self._in_flight -= 1

        if outcome.detection.face_detected:
            m.FACES_DETECTED.labels(worker_id).inc()

        assert self._nc is not None
        with contextlib.suppress(Exception):
            await self._nc.publish(
                wire.features_subject(session_id), wire.encode_features(outcome.features)
            )

    async def _on_window(self, session_id: str, msg: Msg) -> None:
        """Mide una ventana de pose y devuelve sus sub-métricas.

        Go delimita el intervalo y dice contra qué ángulo medirlo; aquí sólo
        se mide lo que pasó en ese trozo de la serie que ya está en memoria.
        """
        worker_id = self._cfg.worker_id

        state = self._sessions.get(session_id)
        if state is None:
            m.WINDOWS_FAILED.labels(worker_id, "unknown_session").inc()
            return

        try:
            kind = wire.window_kind(msg.data)
            if kind == wire.WINDOW_CALIBRATION:
                await self._on_calibration_window(session_id, state, msg.data)
                return
            if kind == wire.WINDOW_FLASH:
                await self._on_flash_window(session_id, state, msg.data)
                return
            if kind == wire.WINDOW_PULSE:
                await self._on_pulse_window(session_id, state, msg.data)
                return
            request = wire.PoseWindowRequest.decode(msg.data)
        except wire.WireError as exc:
            log.warning("ventana ilegible en %s: %s", session_id, exc)
            m.WINDOWS_FAILED.labels(worker_id, "malformed").inc()
            return

        started = time.perf_counter()
        try:
            score = window.analyze(
                window.WindowSpec(
                    window_id=request.window_id,
                    axis=request.axis,
                    target_deg=request.target_deg,
                    tolerance_deg=request.tolerance_deg,
                    started_at_us=request.started_at_us,
                    ended_at_us=request.ended_at_us,
                ),
                state.samples,
                self._score_config,
            )
        except Exception as exc:  # noqa: BLE001 - una ventana mala no tumba la sesión
            log.exception("fallo analizando la ventana %s: %s", request.window_id, exc)
            m.WINDOWS_FAILED.labels(worker_id, "error").inc()
            return

        elapsed_ms = (time.perf_counter() - started) * 1000.0
        m.WINDOWS_TOTAL.labels(worker_id).inc()
        m.WINDOW_LATENCY.labels(worker_id).observe(elapsed_ms)
        log.info(
            "ventana %s: score=%.3f sub=%s frames=%d (%.1f ms)",
            request.window_id,
            score.score,
            {k: (None if v is None else round(v, 3)) for k, v in score.submetrics.items()},
            score.frames_used,
            elapsed_ms,
        )

        assert self._nc is not None
        with contextlib.suppress(Exception):
            await self._nc.publish(
                wire.challenge_score_subject(session_id),
                wire.encode_challenge_score(score.to_dict()),
            )

    async def _on_calibration_window(self, session_id: str, state, data: bytes) -> None:  # noqa: ANN001
        """Toma la línea base del sujeto con pantalla neutra.

        Es la fase obligatoria previa a cualquier destello: todo lo posterior
        se mide relativo a esto, nunca contra umbrales absolutos. Sin ella, el
        sistema rechazaría de más a quien tiene la piel oscura, y eso sería un
        defecto de producto.
        """
        worker_id = self._cfg.worker_id
        request = wire.decode_calibration_window(data)

        measured = state.photometry_between(request.started_at_us, request.ended_at_us)
        photometry = [p for _, p in measured]
        baseline = calibration.estimate(photometry)
        state.baseline = baseline

        if baseline is None:
            m.WINDOWS_FAILED.labels(worker_id, "calibration_insufficient").inc()
            log.warning(
                "calibración %s sin material suficiente: %d frames",
                request.window_id,
                len(photometry),
            )
        else:
            m.CALIBRATIONS_TOTAL.labels(worker_id).inc()
            log.info("calibración %s: %s", request.window_id, baseline.to_dict())

        assert self._nc is not None
        payload = {
            "window_id": request.window_id,
            "kind": wire.WINDOW_CALIBRATION,
            "score": 1.0 if baseline is not None else 0.0,
            "submetrics": {},
            "raw": (
                baseline.to_dict()
                if baseline is not None
                else {"frames": float(len(photometry))}
            ),
            "frames_used": len(photometry),
            "quality_sufficient": baseline is not None,
            "quality_reason": (
                "" if baseline is not None else "frames insuficientes para la línea base"
            ),
        }
        with contextlib.suppress(Exception):
            await self._nc.publish(
                wire.challenge_score_subject(session_id), wire.encode_challenge_score(payload)
            )

    async def _on_pulse_window(self, session_id: str, state, data: bytes) -> None:  # noqa: ANN001
        """Mide el pulso sanguíneo en el intervalo que Go señale.

        Publica el resumen sólo si se pudo medir. Cuando no —intervalo corto,
        rostro perdido, deriva mandando sobre el latido— se dice, y la fusión
        deja la señal fuera en vez de contarla como cero. Meterla como cero
        acusaría de máscara a quien simplemente se movió.
        """
        worker_id = self._cfg.worker_id
        request = wire.decode_pulse_window(data)

        started = time.perf_counter()
        samples = state.samples_between(request.started_at_us, request.ended_at_us)
        usable = [s for s in samples if s.photometry is not None]

        # El pulso se mide CONTRA EL FONDO, y un frame con el fondo a oscuras
        # no tiene divisor válido: se cae, no se rellena.
        series = [(s.at_us, rppg.skin_series(s.photometry)) for s in usable]
        measurable = [(at, v) for at, v in series if v is not None]

        pulse = None
        if measurable:
            try:
                pulse = rppg.measure(
                    [at for at, _ in measurable],
                    [v for _, v in measurable],
                )
            except Exception as exc:  # noqa: BLE001 - una ventana mala no tumba la sesión
                log.exception("fallo midiendo el pulso %s: %s", request.window_id, exc)
                m.WINDOWS_FAILED.labels(worker_id, "error").inc()
                return

        elapsed_ms = (time.perf_counter() - started) * 1000.0
        m.WINDOWS_TOTAL.labels(worker_id).inc()
        m.WINDOW_LATENCY.labels(worker_id).observe(elapsed_ms)

        if pulse is None:
            if not usable:
                reason = "sin fotometría en el intervalo"
            elif not measurable:
                # Sin fondo iluminado no hay medida diferencial posible, y sin
                # ella la deriva de balance de blancos de la cámara se lee como
                # latido. Es calidad de captura, no ausencia de pulso.
                reason = "fondo demasiado oscuro para medir contra él"
            else:
                reason = f"señal insuficiente ({len(measurable)} frames)"
            log.info("pulso %s: no medible — %s (%.1f ms)", request.window_id, reason, elapsed_ms)
            payload = {
                "window_id": request.window_id,
                "session_id": session_id,
                "kind": wire.WINDOW_PULSE,
                "score": 0.0,
                "submetrics": {},
                "raw": {"frames": float(len(measurable))},
                "frames_used": len(measurable),
                "quality_sufficient": False,
                "quality_reason": reason,
            }
        else:
            log.info(
                "pulso %s: snr=%.2f dB %.1f lpm sobre %.1f s (%.1f ms)",
                request.window_id,
                pulse.snr_db,
                pulse.bpm,
                pulse.seconds,
                elapsed_ms,
            )
            payload = {
                "window_id": request.window_id,
                "session_id": session_id,
                "kind": wire.WINDOW_PULSE,
                # El resumen 0-1 que pide el contrato. La escala la fija Go al
                # fusionar; aquí sólo se normaliza el decibelio a algo acotado.
                "score": rppg.summary(pulse.snr_db),
                "submetrics": {"snr": rppg.summary(pulse.snr_db)},
                "raw": {
                    "rppg_snr_db": pulse.snr_db,
                    "rppg_bpm": pulse.bpm,
                    "rppg_seconds": pulse.seconds,
                },
                "frames_used": len(measurable),
                "quality_sufficient": True,
                "quality_reason": "",
            }

        nc = self._nc
        if nc is None:
            return
        with contextlib.suppress(Exception):
            await nc.publish(
                wire.challenge_score_subject(session_id), wire.encode_challenge_score(payload)
            )

    async def _on_flash_window(self, session_id: str, state, data: bytes) -> None:  # noqa: ANN001
        """Mide la respuesta del sujeto a una secuencia de destellos."""
        worker_id = self._cfg.worker_id
        request = wire.decode_flash_window(data)

        started = time.perf_counter()
        spec = flash.FlashWindowSpec(
            window_id=request.window_id,
            sequence=tuple(
                flash.FlashSegment(color=color, duration_ms=duration)
                for color, duration in request.sequence
            ),
            started_at_us=request.started_at_us,
            ended_at_us=request.ended_at_us,
        )
        try:
            # La ventana se alarga por el RETARDO MÁXIMO que se va a buscar.
            #
            # Entre que la pantalla pinta un color y la cámara lo entrega pasan
            # de 100 a 400 ms, así que la respuesta al ÚLTIMO tramo llega
            # después de que la secuencia haya terminado. Entregando sólo los
            # frames de dentro de la ventana, los retardos grandes se quedan
            # sin muestras suficientes y `_best_lag` los descarta por
            # `min_frames`: se acaba eligiendo un retardo peor y se tira la
            # señal.
            #
            # Medido sobre una sesión real por túnel: con la ventana justa, el
            # analizador daba correlación 0,3212 con retardo 220 ms y declaraba
            # la ventana no medible; con los frames de después disponibles, el
            # mejor retardo era 380 ms y la correlación **0,6931**.
            #
            # No le cuenta a Python nada nuevo del guion (§3): sigue siendo su
            # propio buffer, sólo que sin recortarlo antes de tiempo.
            tail_us = self._flash_config.max_lag_ms * 1000
            score = flash.analyze(
                spec,
                state.photometry_between(
                    request.started_at_us, request.ended_at_us + tail_us
                ),
                state.baseline,
                self._flash_config,
            )
        except Exception as exc:  # noqa: BLE001 - una ventana mala no tumba la sesión
            log.exception("fallo analizando el destello %s: %s", request.window_id, exc)
            m.WINDOWS_FAILED.labels(worker_id, "error").inc()
            return

        elapsed_ms = (time.perf_counter() - started) * 1000.0
        m.WINDOWS_TOTAL.labels(worker_id).inc()
        m.WINDOW_LATENCY.labels(worker_id).observe(elapsed_ms)
        if not score.quality_sufficient:
            m.FLASH_INSUFFICIENT.labels(worker_id).inc()

        log.info(
            "destello %s: score=%.3f sub=%s calidad=%s (%.1f ms)",
            request.window_id,
            score.score,
            {k: (None if v is None else round(v, 3)) for k, v in score.submetrics.items()},
            "ok" if score.quality_sufficient else score.quality_reason,
            elapsed_ms,
        )

        payload = score.to_dict()
        payload["kind"] = wire.WINDOW_FLASH

        assert self._nc is not None
        with contextlib.suppress(Exception):
            await self._nc.publish(
                wire.challenge_score_subject(session_id), wire.encode_challenge_score(payload)
            )

    # --- control y limpieza --------------------------------------------------

    async def _on_control(self, session_id: str, msg: Msg) -> None:
        """Higiene de recursos. No mueve nada de negocio."""
        try:
            control = wire.SessionControl.decode(msg.data)
        except wire.WireError as exc:
            log.warning("control ilegible en %s: %s", session_id, exc)
            return
        if control.closes_session:
            await self._release(session_id, reason="closed")

    async def _sweep_loop(self, stop: asyncio.Event) -> None:
        """Cierra las sesiones que caducaron.

        Hace falta aunque exista el aviso de cierre: el bus es efímero y ese
        aviso se puede perder. Sin esto, el estado caliente de una sesión
        abandonada se quedaría en memoria para siempre.
        """
        while not stop.is_set():
            await asyncio.sleep(self._cfg.sweep_interval_s)
            for session_id in self._sessions.sweep():
                log.info("sesión %s caducada, soltando estado", session_id)
                await self._release(session_id, reason="expired", already_closed=True)

    async def _release(self, session_id: str, *, reason: str, already_closed: bool = False) -> None:
        """Suelta el estado caliente y las suscripciones de una sesión."""
        if not already_closed:
            self._sessions.close(session_id)

        for sub in self._session_subs.pop(session_id, []):
            with contextlib.suppress(Exception):
                await sub.unsubscribe()

        m.SESSIONS_CLOSED.labels(self._cfg.worker_id, reason).inc()
        m.SESSIONS_OPEN.labels(self._cfg.worker_id).set(len(self._sessions))

    async def _on_error(self, exc: Exception) -> None:
        # El desbordamiento de la cola de un consumidor es información, no
        # avería: significa que llegan frames más rápido de lo que se analizan
        # y NATS está tirando los viejos, que es lo que queremos.
        log.warning("error de NATS: %s", exc)


def build_embedder(config: Config):  # noqa: ANN201
    """Prepara el extractor de embeddings, si hay modelo.

    Sin él, la continuidad de identidad sale como no medida y las otras tres
    sub-métricas siguen valiendo. Es preferible eso a inventarse un número.
    """
    if not config.embedder_model:
        return None

    from analyzer.identity import EmbedderError, SFaceEmbedder

    try:
        return SFaceEmbedder(config.embedder_model)
    except EmbedderError as exc:
        log.warning("sin continuidad de identidad: %s", exc)
        return None


def build_detector(config: Config) -> FaceDetector:
    """Construye el detector configurado."""
    from analyzer.detector import create

    try:
        return create(
            config.detector_backend,
            config.detector_model,
            max_faces=config.max_faces,
            min_confidence=config.min_face_confidence,
        )
    except DetectorError:
        raise
    except Exception as exc:  # noqa: BLE001
        raise DetectorError(f"no se pudo preparar el detector: {exc}") from exc


def _now_us() -> int:
    return int(time.time() * 1_000_000)


def build_pad(config: Config, logger) -> PadModel | None:  # noqa: ANN001
    """Carga el PAD pasivo. Sin modelos, se sigue sin él.

    Que falte no puede tumbar al analizador: sus señales salen como no
    medidas, y una señal que no se pudo medir no entra en la fusión.
    """
    import pathlib

    if not config.model_dir:
        return None
    try:
        return PadModel(pathlib.Path(config.model_dir))
    except Exception as exc:  # noqa: BLE001
        logger.warning("PAD pasivo no disponible: %s", exc)
        return None
