"""Punto de entrada del analyzer.

    python -m analyzer                 supervisor con N procesos worker
    python -m analyzer worker          un único worker en primer plano
    python -m analyzer preflight       comprueba el detector y sale
"""

from __future__ import annotations

import logging
import sys

from analyzer import config as config_module
from analyzer.logging_setup import configure


def main(argv: list[str] | None = None) -> int:
    """Arranca el analyzer. Devuelve el código de salida del proceso."""
    argv = list(sys.argv[1:] if argv is None else argv)
    command = argv[0] if argv else "supervise"

    cfg = config_module.load()
    configure(cfg.log_level)
    log = logging.getLogger("analyzer")

    if command == "preflight":
        return _preflight(cfg, log)

    if command == "worker":
        from analyzer.supervisor import run_worker

        run_worker(cfg)
        return 0

    if command in ("supervise", "run"):
        from analyzer.supervisor import Supervisor

        return Supervisor(cfg).run()

    log.error("comando desconocido: %s", command)
    return 2


def _preflight(cfg: config_module.Config, log: logging.Logger) -> int:
    """Comprueba que el detector arranca de verdad.

    Vale la pena tenerlo aparte: mediapipe 1.0.x no lanza una excepción cuando
    no puede arrancar, ABORTA el proceso. Un fallo así dentro del supervisor se
    ve como un ciclo de arranques fallidos; aquí se ve directamente.
    """
    from analyzer.worker import build_detector

    try:
        detector = build_detector(cfg)
    except Exception as exc:  # noqa: BLE001
        log.error("preflight FALLIDO: %s", exc)
        return 1

    log.info(
        "preflight OK: detector=%s backend=%s modelo=%s",
        detector.name,
        cfg.detector_backend,
        cfg.detector_model,
    )
    detector.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
