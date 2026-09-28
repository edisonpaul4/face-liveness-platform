"""Instrumentación del worker.

El presupuesto por frame es de 30 ms. Se mide siempre, se expone siempre y se
registra cuando se pasa: un analizador que tarda de más no falla, se retrasa,
y el gateway acaba descartando frames sin que nadie sepa por qué.
"""

from __future__ import annotations

import logging
from dataclasses import dataclass

from prometheus_client import Counter, Gauge, Histogram, start_http_server

#: Presupuesto de latencia por frame.
DEFAULT_BUDGET_MS = 30.0

FRAMES_TOTAL = Counter(
    "liveness_analyzer_frames_total", "Frames recibidos", ["worker_id"]
)
FRAMES_FAILED = Counter(
    "liveness_analyzer_frames_failed_total",
    "Frames ilegibles o descartados",
    ["worker_id", "reason"],
)
FRAMES_OVER_BUDGET = Counter(
    "liveness_analyzer_frames_over_budget_total",
    "Frames que se pasaron del presupuesto de latencia",
    ["worker_id"],
)
FACES_DETECTED = Counter(
    "liveness_analyzer_faces_detected_total", "Frames con al menos un rostro", ["worker_id"]
)
# Los cubos llegan justo hasta el presupuesto y lo pasan: así se ve de un
# vistazo qué fracción cae del lado malo.
FRAME_LATENCY = Histogram(
    "liveness_analyzer_frame_latency_ms",
    "Latencia de análisis por frame, en milisegundos",
    ["worker_id"],
    buckets=(1, 2, 5, 10, 15, 20, 25, 30, 40, 60, 100, 250),
)
STAGE_LATENCY = Histogram(
    "liveness_analyzer_stage_latency_ms",
    "Latencia por etapa del pipeline, en milisegundos",
    ["worker_id", "stage"],
    buckets=(0.5, 1, 2, 5, 10, 15, 20, 30, 60),
)
SESSIONS_OPEN = Gauge(
    "liveness_analyzer_sessions_open", "Sesiones con estado caliente vivo", ["worker_id"]
)
SESSIONS_TOTAL = Counter(
    "liveness_analyzer_sessions_total", "Sesiones aceptadas", ["worker_id"]
)
SESSIONS_CLOSED = Counter(
    "liveness_analyzer_sessions_closed_total", "Sesiones cerradas", ["worker_id", "reason"]
)
WINDOWS_TOTAL = Counter(
    "liveness_analyzer_windows_total", "Ventanas de pose analizadas", ["worker_id"]
)
WINDOWS_FAILED = Counter(
    "liveness_analyzer_windows_failed_total",
    "Ventanas que no se pudieron analizar",
    ["worker_id", "reason"],
)
WINDOW_LATENCY = Histogram(
    "liveness_analyzer_window_latency_ms",
    "Latencia del análisis de una ventana de pose, en milisegundos",
    ["worker_id"],
    buckets=(1, 2, 5, 10, 20, 50, 100, 250),
)
CALIBRATIONS_TOTAL = Counter(
    "liveness_analyzer_calibrations_total", "Líneas base tomadas", ["worker_id"]
)
FLASH_INSUFFICIENT = Counter(
    "liveness_analyzer_flash_insufficient_total",
    "Ventanas de destello con señal insuficiente. Llevan a REINTENTAR, no a rechazar.",
    ["worker_id"],
)
LEASES_REJECTED = Counter(
    "liveness_analyzer_leases_rejected_total", "Leases rechazados", ["worker_id", "reason"]
)


@dataclass(slots=True)
class LatencyBudget:
    """Vigila el presupuesto de latencia por frame."""

    worker_id: str
    budget_ms: float = DEFAULT_BUDGET_MS
    logger: logging.Logger | None = None

    def observe(
        self, total_ms: float, stages: dict[str, float], *, session_id: str, seq: int
    ) -> bool:
        """Registra la latencia de un frame. Devuelve si se pasó del presupuesto."""
        FRAME_LATENCY.labels(self.worker_id).observe(total_ms)
        for stage, ms in stages.items():
            STAGE_LATENCY.labels(self.worker_id, stage).observe(ms)

        if total_ms <= self.budget_ms:
            return False

        FRAMES_OVER_BUDGET.labels(self.worker_id).inc()
        if self.logger is not None:
            # Con el desglose por etapas: sin él, saber que "tardó 45 ms" no
            # sirve para arreglar nada.
            self.logger.warning(
                "frame fuera de presupuesto: %.1f ms > %.0f ms (sesión=%s seq=%d, etapas=%s)",
                total_ms,
                self.budget_ms,
                session_id,
                seq,
                {k: round(v, 1) for k, v in stages.items()},
            )
        return True


def serve(port: int) -> None:
    """Expone las métricas por HTTP.

    Un puerto por proceso: el paralelismo son procesos y cada uno lleva sus
    propios contadores.
    """
    start_http_server(port)
