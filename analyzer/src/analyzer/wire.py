"""Codificación del bus: espejo exacto de `gateway/internal/bus/codec.go`.

Contrato de referencia en `/proto/nats/v1/*.proto`; los `.proto` todavía no se
generan y ambos lados hablan esta codificación escrita a mano. Los vectores
dorados de `/proto/testdata/` sujetan las dos implementaciones: cada lado
comprueba que decodifica exactamente lo que el otro escribió.

Los frames van en binario porque son el camino caliente: en JSON el payload
iría en base64 y costaría un tercio más de red por frame. Todo lo demás va en
JSON, que es de bajo caudal.
"""

from __future__ import annotations

import json
import struct
from dataclasses import dataclass
from typing import Any

#: Cabecera de FrameTask: versión, codificación, flags, seq,
#: captured_at_us, received_at_us y longitud del payload.
#: Big-endian, 32 bytes.
FRAME_TASK_HEADER = struct.Struct(">BBHQqqI")
FRAME_TASK_HEADER_SIZE = FRAME_TASK_HEADER.size
FRAME_TASK_VERSION = 1

#: El sello del cliente sirve para alinear frames entre sí.
FLAG_CLIENT_CLOCK_TRUSTED = 1 << 0

# Codificaciones de payload (FrameEncoding en common.proto).
ENCODING_UNSPECIFIED = 0
ENCODING_JPEG = 1
ENCODING_WEBP = 2
ENCODING_RGB24 = 3
ENCODING_SYNTHETIC_SCENE = 0xF0

#: Tipos de mensaje de control.
CONTROL_SESSION_CLOSE = "session_close"
#: Alias aceptado: el mismo significado con otro nombre.
CONTROL_SESSION_END = "session_end"


class WireError(ValueError):
    """Un mensaje del bus no cumple el contrato."""


@dataclass(frozen=True, slots=True)
class FrameTask:
    """Un frame camino del analizador.

    No lleva identificador de sesión: va en el subject, y el worker sabe de
    qué sesión se trata porque aceptó su lease. Tampoco lleva nada del estado
    de la sesión: el analizador mide y no decide (CLAUDE.md §3).
    """

    seq: int
    encoding: int
    payload: bytes
    #: Sello del SERVIDOR. Es el único instante que puntúa.
    received_at_us: int
    #: Sello del CLIENTE. Sólo alinea, y sólo si client_clock_trusted.
    captured_at_us: int = 0
    flags: int = 0

    @property
    def client_clock_trusted(self) -> bool:
        """Indica si el sello del cliente es utilizable para alinear."""
        return bool(self.flags & FLAG_CLIENT_CLOCK_TRUSTED)


def decode_frame_task(data: bytes) -> FrameTask:
    """Decodifica un frame del bus.

    Raises:
        WireError: si el mensaje está truncado, la versión no se soporta o la
            longitud declarada no coincide con el payload. Un frame ilegible
            se descarta; nunca se adivina.
    """
    if len(data) < FRAME_TASK_HEADER_SIZE:
        raise WireError(f"frame más corto que la cabecera: {len(data)} bytes")

    header = FRAME_TASK_HEADER.unpack_from(data)
    version, encoding, flags, seq, captured, received, declared = header
    if version != FRAME_TASK_VERSION:
        raise WireError(f"versión de frame no soportada: {version}")

    payload = data[FRAME_TASK_HEADER_SIZE:]
    if declared != len(payload):
        raise WireError(f"longitud declarada {declared}, hay {len(payload)}")

    return FrameTask(
        seq=seq,
        encoding=encoding,
        payload=payload,
        received_at_us=received,
        captured_at_us=captured,
        flags=flags,
    )


def encode_frame_task(task: FrameTask) -> bytes:
    """Serializa un frame. Sólo lo usan los tests: quien publica frames es Go."""
    header = FRAME_TASK_HEADER.pack(
        FRAME_TASK_VERSION,
        task.encoding,
        task.flags,
        task.seq,
        task.captured_at_us,
        task.received_at_us,
        len(task.payload),
    )
    return header + task.payload


@dataclass(frozen=True, slots=True)
class Quality:
    """Si el frame servía para medir."""

    face_detected: bool
    face_count: int
    sharpness: float
    brightness: float

    def to_dict(self) -> dict[str, Any]:
        return {
            "face_detected": self.face_detected,
            "face_count": self.face_count,
            "sharpness": self.sharpness,
            "brightness": self.brightness,
        }


@dataclass(frozen=True, slots=True)
class FrameFeatures:
    """Medidas de un frame.

    Magnitudes con nombre, nunca decisiones. Aquí no cabe un `is_live` ni un
    `spoof_score`: quien concluye es Go (CLAUDE.md §3).
    """

    seq: int
    signals: dict[str, float]
    quality: Quality
    processing_ms: int
    version: str
    analyzed_at_us: int
    #: Caja del rostro normalizada [0,1]. Ausente si no se detectó ninguno.
    face: dict[str, float] | None = None

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {
            "seq": self.seq,
            "signals": self.signals,
            "quality": self.quality.to_dict(),
            "processing_ms": self.processing_ms,
            "version": self.version,
            "analyzed_at_us": self.analyzed_at_us,
        }
        if self.face is not None:
            out["face"] = self.face
        return out


#: Nombres de señal prohibidos: son decisiones, no medidas.
FORBIDDEN_SIGNAL_NAMES = frozenset(
    {
        "is_live",
        "liveness",
        "spoof_score",
        "attack_detected",
        "passed_challenge",
        "decision",
        "verdict",
    }
)


def encode_features(features: FrameFeatures) -> bytes:
    """Serializa las medidas de un frame.

    Raises:
        WireError: si alguna señal lleva nombre de decisión. Es una barrera
            deliberada: la frontera Go/Python se rompería sin hacer ruido.
    """
    for name in features.signals:
        if name in FORBIDDEN_SIGNAL_NAMES:
            raise WireError(
                f"la señal {name!r} es un juicio, no una medida: "
                "el analizador no decide (CLAUDE.md §3)"
            )
    return json.dumps(features.to_dict(), separators=(",", ":")).encode()


@dataclass(frozen=True, slots=True)
class Announce:
    """Anuncio periódico de capacidad. Es todo el descubrimiento que hay."""

    worker_id: str
    capacity: int
    sessions: int
    in_flight: int
    version: str
    emitted_at_us: int

    def encode(self) -> bytes:
        return json.dumps(
            {
                "worker_id": self.worker_id,
                "capacity": self.capacity,
                "sessions": self.sessions,
                "in_flight": self.in_flight,
                "version": self.version,
                "emitted_at_us": self.emitted_at_us,
            },
            separators=(",", ":"),
        ).encode()


@dataclass(frozen=True, slots=True)
class LeaseRequest:
    """Petición de hacerse cargo de una sesión."""

    session_id: str
    deadline_us: int

    @staticmethod
    def decode(data: bytes) -> LeaseRequest:
        try:
            raw = json.loads(data)
        except json.JSONDecodeError as exc:
            raise WireError(f"lease ilegible: {exc}") from exc
        session_id = raw.get("session_id")
        if not isinstance(session_id, str) or not session_id:
            raise WireError("lease sin session_id")
        return LeaseRequest(session_id=session_id, deadline_us=int(raw.get("deadline_us", 0)))


@dataclass(frozen=True, slots=True)
class LeaseReply:
    """Respuesta al lease."""

    accepted: bool
    worker_id: str
    reason: str = ""

    def encode(self) -> bytes:
        out: dict[str, Any] = {"accepted": self.accepted, "worker_id": self.worker_id}
        if self.reason:
            out["reason"] = self.reason
        return json.dumps(out, separators=(",", ":")).encode()


@dataclass(frozen=True, slots=True)
class SessionHeartbeat:
    """Renovación del lease.

    Mientras llegue, el gateway da al worker por vivo. Si calla más que el
    TTL, la sesión se ABORTA: no se intenta recuperar (CLAUDE.md §4).
    """

    worker_id: str
    session_id: str
    in_flight: int
    emitted_at_us: int

    def encode(self) -> bytes:
        return json.dumps(
            {
                "worker_id": self.worker_id,
                "session_id": self.session_id,
                "in_flight": self.in_flight,
                "emitted_at_us": self.emitted_at_us,
            },
            separators=(",", ":"),
        ).encode()


@dataclass(frozen=True, slots=True)
class SessionControl:
    """Higiene de recursos. No es un canal de negocio."""

    kind: str
    session_id: str = ""

    @property
    def closes_session(self) -> bool:
        """Indica si pide soltar el estado caliente."""
        return self.kind in (CONTROL_SESSION_CLOSE, CONTROL_SESSION_END)

    @staticmethod
    def decode(data: bytes) -> SessionControl:
        try:
            raw = json.loads(data)
        except json.JSONDecodeError as exc:
            raise WireError(f"control ilegible: {exc}") from exc
        return SessionControl(
            kind=str(raw.get("kind", "")), session_id=str(raw.get("session_id", ""))
        )


@dataclass(frozen=True, slots=True)
class PoseWindowRequest:
    """Go → Python: mide esta ventana de pose.

    Es lo mínimo que hace falta para medir, y ni un campo más. NO trae
    identificador de reto, ni posición en el guion, ni umbral de aprobado, ni
    qué consecuencia tiene el resultado. Python mide geometría en un intervalo
    de tiempo; qué signifique eso es de Go (CLAUDE.md §3).
    """

    window_id: str
    axis: str
    target_deg: float
    tolerance_deg: float
    started_at_us: int
    ended_at_us: int

    @staticmethod
    def decode(data: bytes) -> PoseWindowRequest:
        try:
            raw = json.loads(data)
        except json.JSONDecodeError as exc:
            raise WireError(f"ventana ilegible: {exc}") from exc

        window_id = raw.get("window_id")
        if not isinstance(window_id, str) or not window_id:
            raise WireError("ventana sin window_id")

        axis = str(raw.get("axis", "yaw"))
        if axis not in ("yaw", "pitch", "roll"):
            raise WireError(f"eje desconocido: {axis!r}")

        return PoseWindowRequest(
            window_id=window_id,
            axis=axis,
            target_deg=float(raw.get("target_deg", 0.0)),
            tolerance_deg=float(raw.get("tolerance_deg", 0.0)),
            started_at_us=int(raw.get("started_at_us", 0)),
            ended_at_us=int(raw.get("ended_at_us", 0)),
        )


@dataclass(frozen=True, slots=True)
class CalibrationWindowRequest:
    """Go → Python: toma la línea base de este intervalo.

    Es la fase obligatoria previa a cualquier destello. Sin línea base no se
    analiza nada, porque todo lo posterior se mide RELATIVO a ella y no contra
    umbrales absolutos.
    """

    window_id: str
    started_at_us: int
    ended_at_us: int


@dataclass(frozen=True, slots=True)
class FlashWindowRequest:
    """Go → Python: mide la respuesta a esta secuencia de colores.

    Trae la secuencia emitida porque sin ella no hay con qué correlacionar. No
    trae ni reto, ni posición en el guion, ni umbral de aprobado.
    """

    window_id: str
    #: Pares (color, duración en ms) en el orden en que se pintaron.
    sequence: tuple[tuple[str, int], ...]
    started_at_us: int
    ended_at_us: int


@dataclass(frozen=True, slots=True)
class PulseWindowRequest:
    """Go → Python: mide el pulso en este intervalo.

    Es la petición más escueta de todas —un identificador y dos instantes— y
    eso es exactamente lo que debe ser. El pulso no depende de qué reto se
    pidió ni de qué se espera de él; sólo del color de la piel a lo largo del
    tiempo. Si algún día esta estructura necesita saber algo más, la frontera
    de §3 se estará moviendo.

    Qué intervalo elegir es decisión de Go: tiene que ser largo, sin destellos
    y sin la cabeza girando. Python mide lo que le den y dice si no llegaba.
    """

    window_id: str
    started_at_us: int
    ended_at_us: int


#: Tipos de ventana que Go puede pedir.
WINDOW_POSE = "pose"
WINDOW_CALIBRATION = "calibration"
WINDOW_FLASH = "flash"
WINDOW_PULSE = "rppg"


def window_kind(data: bytes) -> str:
    """Tipo de ventana pedida. Por omisión, pose."""
    try:
        raw = json.loads(data)
    except json.JSONDecodeError as exc:
        raise WireError(f"ventana ilegible: {exc}") from exc
    return str(raw.get("kind", WINDOW_POSE))


def decode_calibration_window(data: bytes) -> CalibrationWindowRequest:
    raw = _window_common(data)
    return CalibrationWindowRequest(
        window_id=raw["window_id"],
        started_at_us=raw["started_at_us"],
        ended_at_us=raw["ended_at_us"],
    )


def decode_pulse_window(data: bytes) -> PulseWindowRequest:
    raw = _window_common(data)
    return PulseWindowRequest(
        window_id=raw["window_id"],
        started_at_us=raw["started_at_us"],
        ended_at_us=raw["ended_at_us"],
    )


def decode_flash_window(data: bytes) -> FlashWindowRequest:
    raw = _window_common(data)
    sequence = raw["payload"].get("sequence")
    if not isinstance(sequence, list) or not sequence:
        raise WireError("ventana de destello sin secuencia de colores")

    segments = []
    for item in sequence:
        color = str(item.get("color", ""))
        duration = int(item.get("duration_ms", 0))
        if not color or duration <= 0:
            raise WireError(f"tramo de destello inválido: {item!r}")
        segments.append((color, duration))

    return FlashWindowRequest(
        window_id=raw["window_id"],
        sequence=tuple(segments),
        started_at_us=raw["started_at_us"],
        ended_at_us=raw["ended_at_us"],
    )


def _window_common(data: bytes) -> dict[str, Any]:
    """Campos comunes a toda petición de ventana."""
    try:
        raw = json.loads(data)
    except json.JSONDecodeError as exc:
        raise WireError(f"ventana ilegible: {exc}") from exc

    window_id = raw.get("window_id")
    if not isinstance(window_id, str) or not window_id:
        raise WireError("ventana sin window_id")

    return {
        "window_id": window_id,
        "started_at_us": int(raw.get("started_at_us", 0)),
        "ended_at_us": int(raw.get("ended_at_us", 0)),
        "payload": raw,
    }


def encode_challenge_score(payload: dict[str, Any]) -> bytes:
    """Serializa el resultado de una ventana.

    Sale el resumen 0-1, las cuatro sub-métricas y las magnitudes crudas. No
    sale ningún veredicto: Go decide qué hacer con todo esto.
    """
    for key in ("window_id", "score", "submetrics", "raw"):
        if key not in payload:
            raise WireError(f"falta {key!r} en el resultado de la ventana")
    return json.dumps(payload, separators=(",", ":")).encode()


# --- subjects ----------------------------------------------------------------

SUBJECT_ANNOUNCE = "analyzer.announce"
_LEASE_PREFIX = "analyzer.lease."
_SESSION_PREFIX = "session."

_FORBIDDEN_TOKEN_CHARS = frozenset(". *>\t\r\n")


def valid_token(token: str) -> bool:
    """Indica si el identificador puede ir en un subject.

    Un `>` o un `*` colado ahí es una suscripción a sesiones ajenas.
    """
    if not token or len(token) > 64:
        return False
    return not any(c in _FORBIDDEN_TOKEN_CHARS for c in token)


def lease_subject(worker_id: str) -> str:
    return _LEASE_PREFIX + worker_id


def frames_subject(session_id: str) -> str:
    return f"{_SESSION_PREFIX}{session_id}.frames"


def features_subject(session_id: str) -> str:
    return f"{_SESSION_PREFIX}{session_id}.features"


def heartbeat_subject(session_id: str) -> str:
    return f"{_SESSION_PREFIX}{session_id}.heartbeat"


def control_subject(session_id: str) -> str:
    return f"{_SESSION_PREFIX}{session_id}.control"


def window_subject(session_id: str) -> str:
    """Por donde Go pide medir una ventana de pose."""
    return f"{_SESSION_PREFIX}{session_id}.window"


def challenge_score_subject(session_id: str) -> str:
    """Por donde vuelve el resultado de la ventana."""
    return f"{_SESSION_PREFIX}{session_id}.challenge_score"
