"""El trabajo de un frame: decodificar, detectar rostro y medir calidad.

Es lógica pura y síncrona. No sabe de NATS, ni de sesiones de negocio, ni de
retos. Se le da un frame y devuelve medidas.

Lo que NO hace, y no es un olvido (CLAUDE.md §3):
    * No decide si hay una persona real delante.
    * No aplica umbrales de negocio.
    * No emite probabilidades de ataque.
"""

from __future__ import annotations

import time
from dataclasses import dataclass

import numpy as np

from analyzer import calibration as calibration_module
from analyzer import frames, pose, quality, wire
from analyzer import gaze as gaze_module
from analyzer import pad as pad_module
from analyzer import photometry as photometry_module
from analyzer.detector import Detection, FaceDetector
from analyzer.identity import SFaceEmbedder
from analyzer.metrics import LatencyBudget
from analyzer.sessions import PoseSample, SessionState

#: Versión del pipeline. Viaja en cada mensaje de medidas para poder atribuir
#: un resultado a una versión concreta del análisis.
PIPELINE_VERSION = "analyzer-py-6"

#: Cuánto se amplifica la respuesta logarítmica para llevarla a [0,1]. Con un
#: destello normal, el canal iluminado sube ~0,3 en logaritmo.
COLOR_RESPONSE_GAIN = 2.0


@dataclass(slots=True)
class FrameOutcome:
    """Resultado de analizar un frame."""

    features: wire.FrameFeatures
    detection: Detection
    over_budget: bool
    stages: dict[str, float]


class Pipeline:
    """Analiza frames con un detector dado."""

    def __init__(
        self,
        detector: FaceDetector,
        *,
        budget: LatencyBudget,
        version: str = PIPELINE_VERSION,
        embedder: SFaceEmbedder | None = None,
        pad: pad_module.PadModel | None = None,
        embedding_stride: int = 3,
        auto_baseline_frames: int = 10,
    ) -> None:
        self._detector = detector
        self._budget = budget
        self._version = version
        # El embedding es opcional: sin modelo, la continuidad de identidad
        # sale como no medida y las demás sub-métricas siguen valiendo.
        self._embedder = embedder
        # PAD pasivo. Opcional: sin modelo, sus señales salen como no medidas
        # y las demás siguen valiendo igual.
        self._pad = pad
        # No hace falta en cada frame: la continuidad se mide entre muestras,
        # y calcularlo siempre gasta un tercio del presupuesto para nada.
        self._embedding_stride = max(1, embedding_stride)
        # Línea base automática: si nadie ha pedido una calibración explícita,
        # el analizador se la toma de los primeros frames de la sesión.
        #
        # No es adivinar el guion: es normalizar contra el propio sujeto, que
        # es lo que hace falta para que la respuesta cromática signifique algo
        # con cualquier tono de piel. Una ventana de calibración explícita la
        # sustituye en cuanto llega.
        self._auto_baseline_frames = max(0, auto_baseline_frames)

    @property
    def version(self) -> str:
        return self._version

    def process(self, task: wire.FrameTask, state: SessionState) -> FrameOutcome:
        """Analiza un frame y devuelve sus medidas.

        Raises:
            frames.DecodeError: si el payload no es una imagen. El llamante lo
                cuenta y sigue: un frame ilegible no tumba una sesión.
        """
        started = time.perf_counter()
        stages: dict[str, float] = {}

        image = frames.decode(task.payload, task.encoding)
        gray = frames.to_gray(image)
        stages["decode"] = (time.perf_counter() - started) * 1000.0

        mark = time.perf_counter()
        detection = self._detector.detect(image)
        stages["detect"] = (time.perf_counter() - mark) * 1000.0

        mark = time.perf_counter()
        metrics = quality.measure(gray)
        stages["quality"] = (time.perf_counter() - mark) * 1000.0

        mark = time.perf_counter()
        photo = photometry_module.measure(image, detection.landmarks)
        stages["photometry"] = (time.perf_counter() - mark) * 1000.0

        mark = time.perf_counter()
        look = gaze_module.measure(detection.landmarks, image.shape[1], image.shape[0])
        stages["gaze"] = (time.perf_counter() - mark) * 1000.0

        mark = time.perf_counter()
        pad = self._measure_pad(image, detection)
        stages["pad"] = (time.perf_counter() - mark) * 1000.0

        mark = time.perf_counter()
        head = pose.from_transform(detection.transform)
        primary = detection.primary
        scale = pose.face_scale(primary.w, primary.h) if primary is not None else 0.0
        embedding = self._maybe_embed(image, detection, task.seq)
        stages["pose"] = (time.perf_counter() - mark) * 1000.0

        state.observe(task.seq, metrics.brightness, time.monotonic())
        self._maybe_autocalibrate(state, photo)
        state.record(
            PoseSample(
                seq=task.seq,
                # El sello del SERVIDOR: es el que delimita las ventanas que
                # Go pide medir después.
                at_us=task.received_at_us,
                pose=head,
                scale=scale,
                landmarks=pose.compact(detection.landmarks),
                embedding=embedding,
                photometry=photo,
            )
        )

        total_ms = (time.perf_counter() - started) * 1000.0
        over_budget = self._budget.observe(
            total_ms, stages, session_id=state.session_id, seq=task.seq
        )

        return FrameOutcome(
            features=self._build_features(
                task, detection, metrics, state, total_ms, head, scale, photo, look, pad
            ),
            detection=detection,
            over_budget=over_budget,
            stages=stages,
        )

    def _maybe_autocalibrate(self, state: SessionState, photo) -> None:  # noqa: ANN001
        """Toma una línea base de los primeros frames si no hay ninguna."""
        if state.baseline is not None or self._auto_baseline_frames <= 0 or photo is None:
            return
        opening = [
            s.photometry for s in state.samples[: self._auto_baseline_frames] if s.photometry
        ]
        if len(opening) < self._auto_baseline_frames:
            return
        state.baseline = calibration_module.estimate(opening)

    def _maybe_embed(
        self, image: np.ndarray, detection: Detection, seq: int
    ) -> np.ndarray | None:
        """Calcula el embedding si toca en este frame y hay con qué."""
        if self._embedder is None or seq % self._embedding_stride != 0:
            return None
        canonical = pose.canonical_points(detection.landmarks)
        if canonical is None:
            return None
        return self._embedder.embed(image, canonical)

    def _build_features(
        self,
        task: wire.FrameTask,
        detection: Detection,
        metrics: quality.QualityMetrics,
        state: SessionState,
        total_ms: float,
        head: pose.HeadPose | None,
        scale: float,
        photo: photometry_module.FramePhotometry | None,
        look: gaze_module.Gaze | None,
        pad: pad_module.PadScores | None,
    ) -> wire.FrameFeatures:
        """Arma el mensaje de medidas.

        Nombres en snake_case, con familia como prefijo y sin juicio de valor.
        """
        primary = detection.primary

        signals: dict[str, float] = {
            # Presencia y recuento. Números, no conclusiones: que haya un
            # rostro no dice que sea real.
            "face_count": float(len(detection.faces)),
            "face_present": 1.0 if detection.face_detected else 0.0,
            # Calidad de captura.
            "quality_sharpness": round(metrics.sharpness, 4),
            "quality_brightness": round(metrics.brightness, 6),
            "quality_highlight_saturation": round(metrics.highlight_saturation, 6),
            "quality_contrast": round(metrics.contrast, 6),
            # Continuidad: sólo existen porque el estado vive en este worker.
            "temporal_frames_seen": float(state.frames),
            "temporal_window_frames": float(state.window_frames),
        }

        if primary is not None:
            signals["face_box_area"] = round(primary.area, 6)
            # Mismo número con el nombre que usa el consumidor de Go. Duplicar
            # un nombre es feo; que dos servicios llamen distinto a lo mismo y
            # no se entiendan, peor.
            signals["face_area_ratio"] = round(primary.area, 6)
            signals["face_box_confidence"] = round(primary.confidence, 4)
            # Escala: proporción lineal del bbox respecto al encuadre. Crece
            # linealmente al acercarse.
            signals["face_scale"] = round(scale, 6)

        if photo is not None and state.baseline is not None:
            # Respuesta cromática del rostro al color que emite la pantalla,
            # relativa a la línea base del propio sujeto. Es la medida que
            # hace que esto funcione con cualquier tono de piel: el albedo se
            # cancela en la división.
            #
            # 0,45 es el reposo, no 0,5, para que un frame sin destello no
            # parezca responder a todos los colores a la vez.
            response = calibration_module.relative_response(photo, state.baseline)
            channels = response.mean(axis=0)  # media de las cuatro regiones
            for index, channel in enumerate(("b", "g", "r")):
                value = float(np.clip(0.45 + channels[index] * COLOR_RESPONSE_GAIN, 0.0, 1.0))
                signals[f"color_response_{channel}"] = round(value, 5)

        if look is not None:
            # Desplazamiento del iris dentro de su órbita, en unidades de
            # ancho de ojo. Es relativo al propio ojo del sujeto, así que
            # significa lo mismo con cualquier cara y a cualquier distancia.
            #
            # Dónde estaba el objetivo, y si mirarlo cuenta como cumplir algo,
            # es de Go. Aquí no se sabe que existan los retos.
            signals["gaze_offset_x"] = round(look.offset_x, 5)
            signals["gaze_offset_y"] = round(look.offset_y, 5)
            signals["gaze_agreement"] = round(look.agreement, 4)
            signals["gaze_openness"] = round(look.openness, 4)

        if pad is not None:
            # Probabilidades de ataque de un clasificador de textura y
            # contexto. Se publican las dos familias de cada variante y NO la
            # de "real": concluir que no hay ataque es decidir, y decide Go.
            #
            # Las dos variantes miran recortes de distinta amplitud, así que
            # no son la misma medida repetida: la ancha ve bordes de foto y
            # biseles de pantalla, la estrecha se queda en la piel.
            signals["texture_pad_v2_a"] = round(pad.v1se_attack_a, 5)
            signals["texture_pad_v2_b"] = round(pad.v1se_attack_b, 5)
            signals["texture_pad_v1se_a"] = round(pad.v2_attack_a, 5)
            signals["texture_pad_v1se_b"] = round(pad.v2_attack_b, 5)

        if photo is not None:
            # Pistas de superficie, ya diferenciales contra el fondo del mismo
            # frame. Son medidas: qué signifiquen es de Go.
            signals["surface_face_bg_luminance_ratio"] = round(photo.luminance_ratio, 5)
            signals["surface_specular_fraction"] = round(photo.specular_ratio, 6)
            # Quemado DENTRO del rostro. La versión sobre el encuadre entero
            # —quality_highlight_saturation— cuenta también la ventana del
            # fondo, y eso no dice nada de si la cara se puede medir.
            signals["quality_face_highlight"] = round(photo.face_highlight, 6)

            # Relación CRUDA rostro/fondo por canal, sin línea base y sin
            # recortar. Es lo único que permite comparar un tramo del destello
            # contra el anterior en vez de contra la calibración, y por tanto
            # lo único que aguanta la adaptación de la cámara a lo largo de la
            # secuencia. Medido con una cámara real: el mismo blanco dio 0,550
            # de respuesta como primer tramo y 0,129 como tercero.
            ratio = photo.region_ratio().mean(axis=0)
            for index, channel in enumerate(("b", "g", "r")):
                signals[f"surface_face_bg_{channel}"] = round(float(ratio[index]), 6)
            signals["surface_moire_index"] = round(photo.moire_index, 6)
            signals["surface_banding_index"] = round(photo.banding_index, 6)

        if head is not None:
            # Orientación de la cabeza. Son grados medidos, no "la pose que
            # se pidió": el reto no se conoce aquí.
            signals["pose_yaw_deg"] = round(head.yaw, 3)
            signals["pose_pitch_deg"] = round(head.pitch, 3)
            signals["pose_roll_deg"] = round(head.roll, 3)

        return wire.FrameFeatures(
            seq=task.seq,
            signals=signals,
            quality=wire.Quality(
                face_detected=detection.face_detected,
                face_count=len(detection.faces),
                sharpness=round(metrics.sharpness, 4),
                brightness=round(metrics.brightness, 6),
            ),
            face=primary.to_dict() if primary is not None else None,
            processing_ms=int(round(total_ms)),
            version=self._version,
            analyzed_at_us=int(time.time() * 1_000_000),
        )


    def _measure_pad(self, image, detection):  # noqa: ANN001, ANN202
        """PAD pasivo sobre la caja del rostro. None si no se puede medir."""
        if self._pad is None or detection.primary is None:
            return None
        height, width = image.shape[:2]
        p = detection.primary
        return self._pad.measure(
            image, (p.x * width, p.y * height, p.w * width, p.h * height)
        )
