"""Configuración del worker de análisis, leída del entorno.

Aquí no hay valores de negocio: ni umbrales de decisión, ni pesos, ni
catálogo de retos. Eso vive en el orchestrator (Go).
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field, replace

#: Backends de detección disponibles.
BACKEND_MEDIAPIPE = "mediapipe"
BACKEND_ONNX = "onnx"


def _env(name: str, default: str) -> str:
    value = os.getenv(name)
    return value if value else default


def _env_int(name: str, default: int) -> int:
    try:
        return int(os.getenv(name, ""))
    except ValueError:
        return default


def _env_float(name: str, default: float) -> float:
    try:
        return float(os.getenv(name, ""))
    except ValueError:
        return default


@dataclass(frozen=True, slots=True)
class Config:
    """Configuración de proceso del analyzer."""

    nats_url: str = field(default_factory=lambda: _env("NATS_URL", "nats://localhost:4222"))

    #: Identificador del worker. Va en `analyzer.lease.<worker_id>`, así que no
    #: admite puntos ni comodines.
    worker_id: str = field(default_factory=lambda: _env("ANALYZER_WORKER_ID", "analyzer-1"))

    #: Procesos worker que levanta el supervisor. 0 = uno por CPU disponible,
    #: menos una para el resto del sistema.
    processes: int = field(default_factory=lambda: _env_int("ANALYZER_PROCESSES", 0))

    #: Sesiones simultáneas por proceso.
    capacity: int = field(default_factory=lambda: _env_int("ANALYZER_CAPACITY", 8))

    # --- detección ---
    detector_backend: str = field(
        default_factory=lambda: _env("ANALYZER_DETECTOR", BACKEND_MEDIAPIPE)
    )
    detector_model: str = field(
        default_factory=lambda: _env("ANALYZER_MODEL", "models/face_landmarker.task")
    )
    #: Directorio de modelos. Vacío desactiva el PAD pasivo.
    model_dir: str = field(default_factory=lambda: _env("ANALYZER_MODEL_DIR", "models"))
    max_faces: int = field(default_factory=lambda: _env_int("ANALYZER_MAX_FACES", 3))
    min_face_confidence: float = field(
        default_factory=lambda: _env_float("ANALYZER_MIN_FACE_CONFIDENCE", 0.6)
    )

    # --- continuidad de identidad ---
    #: Modelo de embedding facial. Vacío = sin embeddings: la continuidad de
    #: identidad sale como no medida y las otras tres sub-métricas siguen
    #: valiendo.
    embedder_model: str = field(
        default_factory=lambda: _env("ANALYZER_EMBEDDER_MODEL",
                                     "models/face_recognition_sface_2021dec.onnx")
    )
    #: Cada cuántos frames se calcula el embedding. La continuidad se mide
    #: entre muestras, no hace falta una por frame.
    embedding_stride: int = field(
        default_factory=lambda: _env_int("ANALYZER_EMBEDDING_STRIDE", 3)
    )

    # --- análisis de ventana ---
    #: Salto de ángulo entre frames por encima del cual hay discontinuidad.
    continuity_threshold_deg: float = field(
        default_factory=lambda: _env_float("ANALYZER_CONTINUITY_THRESHOLD_DEG", 15.0)
    )
    #: Residuo que deja el ruido de detección sobre una superficie plana.
    parallax_noise_floor: float = field(
        default_factory=lambda: _env_float("ANALYZER_PARALLAX_NOISE_FLOOR", 0.004)
    )
    #: Residuo por grado que se considera "rostro con volumen".
    parallax_reference_rate: float = field(
        default_factory=lambda: _env_float("ANALYZER_PARALLAX_REFERENCE_RATE", 0.0008)
    )

    # --- ritmo ---
    announce_interval_s: float = field(
        default_factory=lambda: _env_float("ANALYZER_ANNOUNCE_INTERVAL_S", 1.0)
    )
    #: Tiene que ser bastante menor que el TTL del lease en el gateway
    #: (1,5 s por defecto): si el worker calla más de eso, la sesión se aborta.
    heartbeat_interval_s: float = field(
        default_factory=lambda: _env_float("ANALYZER_HEARTBEAT_INTERVAL_S", 0.3)
    )
    sweep_interval_s: float = field(
        default_factory=lambda: _env_float("ANALYZER_SWEEP_INTERVAL_S", 5.0)
    )
    #: Sin frames durante este tiempo, se suelta el estado caliente.
    session_idle_ttl_s: float = field(
        default_factory=lambda: _env_float("ANALYZER_SESSION_IDLE_TTL_S", 60.0)
    )

    # --- límites ---
    #: Presupuesto de latencia por frame. Pasarse no es un fallo, es un aviso.
    latency_budget_ms: float = field(
        default_factory=lambda: _env_float("ANALYZER_LATENCY_BUDGET_MS", 30.0)
    )
    #: Frames que caben esperando análisis. Uno, y el resto se tira: encolar
    #: frames viejos sólo sirve para analizar el pasado.
    frame_queue: int = field(default_factory=lambda: _env_int("ANALYZER_FRAME_QUEUE", 2))
    max_frame_bytes: int = field(
        default_factory=lambda: _env_int("ANALYZER_MAX_FRAME_BYTES", 512 * 1024)
    )

    metrics_port: int = field(default_factory=lambda: _env_int("ANALYZER_METRICS_PORT", 9102))
    log_level: str = field(default_factory=lambda: _env("LOG_LEVEL", "INFO"))

    def score_config(self):  # noqa: ANN201 - evita importar window en config
        """Parámetros del análisis de ventana, desde el entorno."""
        from analyzer.window import ScoreConfig

        return ScoreConfig(
            continuity_threshold_deg=self.continuity_threshold_deg,
            nominal_fps=15.0,
            parallax_noise_floor=self.parallax_noise_floor,
            parallax_reference_rate=self.parallax_reference_rate,
        )

    def for_process(self, index: int) -> Config:
        """Deriva la configuración del proceso worker número `index`.

        Cada proceso es un worker independiente con su propio identificador y
        su propio puerto de métricas: para el gateway son workers distintos, y
        el reparto de sesiones lo hace su descubrimiento por carga.
        """
        if index == 0 and self.processes in (0, 1):
            return self
        return replace(
            self,
            worker_id=f"{self.worker_id}-{index}",
            metrics_port=self.metrics_port + index,
        )


def load() -> Config:
    """Devuelve la configuración del proceso."""
    return Config()
