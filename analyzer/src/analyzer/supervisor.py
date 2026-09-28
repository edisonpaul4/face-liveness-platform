"""Supervisor de procesos worker.

El análisis es CPU pura: decodificar JPEG, convolucionar y correr una red. Con
hilos, el GIL los serializa y añadir concurrencia no añade rendimiento. Por eso
el paralelismo son PROCESOS, cada uno con su intérprete, su detector y su
bucle de eventos.

Encaja con el resto del diseño sin esfuerzo: cada proceso se anuncia como un
worker independiente y el gateway reparte sesiones entre ellos por carga
(CLAUDE.md §4). La afinidad de sesión sigue valiendo, porque una sesión cae en
un proceso y ahí se queda.

Cada proceso, además, configura su motor de inferencia con UN hilo. Si cada
uno abriera su propio pool, N procesos × N hilos se pisarían y la latencia por
frame subiría en vez de bajar.
"""

from __future__ import annotations

import asyncio
import logging
import multiprocessing as mp
import os
import signal
import time

from analyzer.config import Config

log = logging.getLogger("analyzer.supervisor")

#: Si un proceso muere antes de esto, se considera que no arrancó bien.
_FAST_CRASH_S = 5.0
#: Muertes rápidas seguidas antes de rendirse.
_MAX_FAST_CRASHES = 3


def default_processes() -> int:
    """Procesos a levantar si no se configura: uno por CPU, menos una."""
    return max(1, (os.cpu_count() or 2) - 1)


def run_worker(config: Config) -> None:
    """Punto de entrada de un proceso worker."""
    from analyzer import metrics
    from analyzer.logging_setup import configure
    from analyzer.worker import Worker, build_detector, build_embedder

    configure(config.log_level)
    worker_log = logging.getLogger("analyzer.worker")

    # Un hilo por proceso para las librerías numéricas: el paralelismo ya lo
    # dan los procesos.
    for var in ("OMP_NUM_THREADS", "OPENBLAS_NUM_THREADS", "MKL_NUM_THREADS"):
        os.environ.setdefault(var, "1")

    try:
        detector = build_detector(config)
    except Exception as exc:  # noqa: BLE001
        worker_log.error("no se pudo preparar el detector: %s", exc)
        raise SystemExit(2) from exc

    embedder = build_embedder(config)

    try:
        metrics.serve(config.metrics_port)
    except OSError as exc:
        worker_log.warning(
            "no se pudo abrir el puerto de métricas %d: %s", config.metrics_port, exc
        )

    stop = asyncio.Event()

    async def main() -> None:
        loop = asyncio.get_running_loop()
        for sig in (signal.SIGINT, signal.SIGTERM):
            loop.add_signal_handler(sig, stop.set)
        loop.create_task(_exit_with_parent(stop))
        await Worker(config, detector, embedder).run(stop)

    asyncio.run(main())


async def _exit_with_parent(stop: asyncio.Event) -> None:
    """Se para si el supervisor desaparece.

    Hace falta de verdad. Si al supervisor lo matan con SIGKILL —o se cae— sus
    hijos no se enteran: el sistema los reasigna a init y siguen vivos,
    anunciándose al bus y aceptando sesiones. Pasó de verdad durante el
    desarrollo: workers de diecinueve horas antes, con código viejo, seguían
    analizando frames.

    Un hijo reasignado tiene ppid 1. Comprobarlo cada segundo cuesta nada y
    evita que un redespliegue deje fantasmas atendiendo con la versión
    anterior.
    """
    original = os.getppid()
    while not stop.is_set():
        await asyncio.sleep(1.0)
        current = os.getppid()
        if current != original or current == 1:
            logging.getLogger("analyzer.worker").warning(
                "el supervisor desapareció (ppid %s → %s); parando", original, current
            )
            stop.set()
            return


class Supervisor:
    """Levanta y vigila los procesos worker."""

    def __init__(self, config: Config) -> None:
        self._cfg = config
        self._count = config.processes or default_processes()
        self._ctx = mp.get_context("spawn")
        self._procs: dict[int, mp.process.BaseProcess] = {}
        self._started_at: dict[int, float] = {}
        self._fast_crashes = 0
        self._stopping = False

    def run(self) -> int:
        """Levanta los procesos y los vigila hasta que se pida parar."""
        log.info("supervisor: %d procesos worker", self._count)

        for index in range(self._count):
            self._spawn(index)

        signal.signal(signal.SIGINT, self._on_signal)
        signal.signal(signal.SIGTERM, self._on_signal)

        try:
            while not self._stopping:
                time.sleep(0.25)
                if not self._check():
                    return 1
        finally:
            self._stop_all()
        return 0

    def _spawn(self, index: int) -> None:
        config = self._cfg.for_process(index)
        proc = self._ctx.Process(
            target=run_worker, args=(config,), name=f"analyzer-{index}", daemon=False
        )
        proc.start()
        self._procs[index] = proc
        self._started_at[index] = time.monotonic()
        log.info("worker %s arrancado (pid=%s)", config.worker_id, proc.pid)

    def _check(self) -> bool:
        """Revisa los procesos y repone los caídos.

        Un proceso que muere enseguida y varias veces seguidas no es un
        accidente: es que no puede arrancar —modelo que falta, backend que
        aborta— y reponerlo sin parar sólo esconde el problema.
        """
        for index, proc in list(self._procs.items()):
            if proc.is_alive():
                continue

            lifetime = time.monotonic() - self._started_at[index]
            log.error(
                "worker %d murió con código %s tras %.1f s",
                index,
                proc.exitcode,
                lifetime,
            )

            if lifetime < _FAST_CRASH_S:
                self._fast_crashes += 1
                if self._fast_crashes >= _MAX_FAST_CRASHES:
                    log.error(
                        "%d arranques fallidos seguidos: se para. Revisa el detector y el modelo.",
                        self._fast_crashes,
                    )
                    return False
            else:
                self._fast_crashes = 0

            self._spawn(index)
        return True

    def _on_signal(self, *_: object) -> None:
        self._stopping = True

    def _stop_all(self) -> None:
        log.info("supervisor: parando los workers")
        for proc in self._procs.values():
            if proc.is_alive():
                proc.terminate()
        for proc in self._procs.values():
            proc.join(timeout=10)
            if proc.is_alive():
                log.warning("worker %s no se fue por las buenas; se mata", proc.name)
                proc.kill()
