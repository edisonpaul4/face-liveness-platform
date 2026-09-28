"""Estado caliente por sesión, en memoria de ESTE proceso.

Existe porque las señales temporales —pulso, flujo óptico, tracking— necesitan
continuidad entre frames. Es la razón de ser de la afinidad de sesión: el
gateway ata la sesión a un worker y todos sus frames caen aquí
(CLAUDE.md §4).

Es caché, no verdad: si este proceso se cae, el gateway aborta la sesión y el
usuario reintenta. No hay nada que persistir.
"""

from __future__ import annotations

import time
from collections.abc import Callable
from dataclasses import dataclass, field

import numpy as np

from analyzer.calibration import Baseline
from analyzer.photometry import FramePhotometry
from analyzer.pose import HeadPose


@dataclass(frozen=True, slots=True)
class PoseSample:
    """Lo medido en un frame, guardado para analizar ventanas después.

    Es el estado caliente en su forma más concreta: sin esta serie, una
    ventana de pose no se puede analizar, porque paralaje y continuidad son
    propiedades de la SECUENCIA, no de un frame suelto.
    """

    seq: int
    #: Sello del SERVIDOR. Es el que delimita las ventanas.
    at_us: int
    pose: HeadPose | None
    scale: float
    #: Landmarks reducidos, (N, 2). None si el detector no los da.
    landmarks: np.ndarray | None = None
    #: Embedding normalizado. None si no tocaba calcularlo en este frame.
    embedding: np.ndarray | None = None
    #: Fotometría del frame: respuesta por región contra el fondo, y pistas
    #: de superficie emisiva.
    photometry: FramePhotometry | None = None


@dataclass(slots=True)
class SessionState:
    """Lo que el worker recuerda de una sesión."""

    session_id: str
    #: Fin de vida de la sesión según el lease, en microsegundos epoch.
    deadline_us: int
    created_at: float
    last_seen: float

    frames: int = 0
    last_seq: int = 0
    #: Ventana reciente de luminancia. Aquí irá el buffer de rPPG.
    brightness_window: list[float] = field(default_factory=list)

    #: Serie de pose para analizar ventanas. Acotada: una sesión dura menos de
    #: un minuto, así que con 40 s a 15 fps sobra.
    samples: list[PoseSample] = field(default_factory=list)

    #: Línea base de la calibración. Sin ella no se analiza ningún destello:
    #: no habría contra qué comparar (CLAUDE.md §5).
    baseline: Baseline | None = None

    #: Cuántas muestras conserva la ventana temporal.
    window_size: int = 64
    #: Cuántas muestras de pose se conservan.
    #:
    #: 2000, no 600, y el motivo es el pulso. La ventana de quietud son once
    #: segundos que a 30 fps son 330 muestras, pero lo que hay que garantizar
    #: no es que quepan: es que **sigan ahí al final de la sesión**, porque el
    #: pulso se pide al resolver, no al cerrar el paso.
    #:
    #: Con el tope en 600 y una sesión de 30 fps, el recorte se comía las
    #: muestras del tramo antes de llegar a medirlo, y la señal salía como no
    #: medible sin que nada lo delatara. El presupuesto de sesión son 60 s, y
    #: 60 x 30 = 1800.
    sample_limit: int = 2000

    def observe(self, seq: int, brightness: float, now: float) -> None:
        """Incorpora un frame al estado."""
        self.frames += 1
        self.last_seq = seq
        self.last_seen = now

        self.brightness_window.append(brightness)
        if len(self.brightness_window) > self.window_size:
            del self.brightness_window[0]

    def record(self, sample: PoseSample) -> None:
        """Guarda la medida de pose de un frame."""
        self.samples.append(sample)
        # Recorta todo el exceso, no un elemento: si el límite baja en
        # caliente, quitar uno por llamada nunca alcanzaría.
        excess = len(self.samples) - self.sample_limit
        if excess > 0:
            del self.samples[:excess]

    def samples_between(self, start_us: int, end_us: int) -> list[PoseSample]:
        """Muestras dentro de una ventana temporal, extremos incluidos."""
        return [s for s in self.samples if start_us <= s.at_us <= end_us]

    def photometry_between(self, start_us: int, end_us: int) -> list[tuple[int, FramePhotometry]]:
        """Fotometría de la ventana, con su instante."""
        return [
            (s.at_us, s.photometry)
            for s in self.samples
            if start_us <= s.at_us <= end_us and s.photometry is not None
        ]

    @property
    def window_frames(self) -> int:
        """Muestras acumuladas en la ventana temporal."""
        return len(self.brightness_window)


class SessionStore:
    """Sesiones vivas de este worker.

    No es seguro para uso concurrente entre hilos: lo usa una única tarea
    asíncrona. El paralelismo de este servicio son procesos.
    """

    def __init__(
        self, *, idle_ttl_s: float = 60.0, clock: Callable[[], float] = time.monotonic
    ) -> None:
        self._sessions: dict[str, SessionState] = {}
        self._idle_ttl_s = idle_ttl_s
        self._clock = clock

    def __len__(self) -> int:
        return len(self._sessions)

    def __contains__(self, session_id: str) -> bool:
        return session_id in self._sessions

    def open(self, session_id: str, deadline_us: int) -> SessionState:
        """Crea el estado de una sesión al aceptar su lease.

        Raises:
            KeyError: si la sesión ya estaba abierta. Aceptar dos veces la
                misma sesión mezclaría dos flujos en un solo estado.
        """
        if session_id in self._sessions:
            raise KeyError(f"la sesión {session_id} ya está abierta")

        now = self._clock()
        state = SessionState(
            session_id=session_id, deadline_us=deadline_us, created_at=now, last_seen=now
        )
        self._sessions[session_id] = state
        return state

    def get(self, session_id: str) -> SessionState | None:
        """Devuelve el estado de la sesión, o None si no está abierta."""
        return self._sessions.get(session_id)

    def close(self, session_id: str) -> bool:
        """Suelta el estado de una sesión. Devuelve si existía."""
        return self._sessions.pop(session_id, None) is not None

    def sweep(self, *, now_us: int | None = None) -> list[str]:
        """Cierra las sesiones caducadas y devuelve sus identificadores.

        Se cierran por dos motivos, y hacen falta los dos:

        * Inactividad: nadie manda frames desde hace `idle_ttl_s`. Cubre al
          cliente que desaparece sin avisar.
        * Vencimiento del lease: pasó el deadline que el gateway fijó al
          asignar la sesión. Cubre el caso de que el aviso de cierre se pierda,
          que con un bus efímero pasa.
        """
        now = self._clock()
        if now_us is None:
            now_us = int(time.time() * 1_000_000)

        expired = [
            sid
            for sid, state in self._sessions.items()
            if now - state.last_seen > self._idle_ttl_s
            or (state.deadline_us > 0 and now_us > state.deadline_us)
        ]
        for sid in expired:
            del self._sessions[sid]
        return expired

    def ids(self) -> list[str]:
        """Sesiones abiertas."""
        return list(self._sessions)
