"""Detección de rostro con ONNX Runtime en CPU (modelo YuNet).

Es el camino por el que entrarán los modelos propios de PAD, que se exportarán
a ONNX. MediaPipe no puede pasar por aquí: trae su propio motor.

Detalle que importa: la sesión de ORT se configura con UN hilo. El
paralelismo de este servicio son procesos, y si además cada proceso abriera su
propio pool de hilos se pisarían entre ellos y la latencia por frame subiría
en vez de bajar.
"""

from __future__ import annotations

import pathlib

import cv2
import numpy as np

from analyzer.detector import Detection, DetectorError, FaceBox

#: YuNet tiene la entrada fija en 640x640 y cabezas en tres escalas.
_INPUT_SIZE = 640
_STRIDES = (8, 16, 32)


class OnnxYuNetDetector:
    """Detector YuNet ejecutado con ONNX Runtime sobre CPU."""

    name = "onnx-yunet"

    def __init__(self, model_path: str, *, max_faces: int = 3, min_confidence: float = 0.6,
                 nms_threshold: float = 0.3) -> None:
        path = pathlib.Path(model_path)
        if not path.is_file():
            raise DetectorError(f"falta el modelo ONNX en {path}. Descárgalo con `make models`.")

        try:
            import onnxruntime as ort
        except ImportError as exc:  # pragma: no cover - dependencia ausente
            raise DetectorError(f"onnxruntime no está disponible: {exc}") from exc

        options = ort.SessionOptions()
        # Un hilo por proceso: el paralelismo es de procesos (ver módulo).
        options.intra_op_num_threads = 1
        options.inter_op_num_threads = 1
        options.graph_optimization_level = ort.GraphOptimizationLevel.ORT_ENABLE_ALL

        self._session = ort.InferenceSession(
            str(path), sess_options=options, providers=["CPUExecutionProvider"]
        )
        self._input_name = self._session.get_inputs()[0].name
        self._output_names = [o.name for o in self._session.get_outputs()]
        self._max_faces = max_faces
        self._min_confidence = min_confidence
        self._nms_threshold = nms_threshold

    @property
    def providers(self) -> list[str]:
        """Proveedores activos. Debe ser sólo CPU."""
        return self._session.get_providers()

    def detect(self, bgr: np.ndarray) -> Detection:
        height, width = bgr.shape[:2]
        if height == 0 or width == 0:
            return Detection(faces=())

        # Letterbox, no reescalado a lo bruto: encajar 640x480 en un cuadrado
        # deforma las caras un 33 % y el detector pierde puntuación. Medido
        # sobre el mismo frame: 0.52 aplastando, 0.80 respetando la
        # proporción.
        scale = _INPUT_SIZE / max(height, width)
        new_w, new_h = int(round(width * scale)), int(round(height * scale))
        canvas = np.zeros((_INPUT_SIZE, _INPUT_SIZE, 3), dtype=np.uint8)
        canvas[:new_h, :new_w] = cv2.resize(bgr, (new_w, new_h), interpolation=cv2.INTER_LINEAR)
        blob = canvas.transpose(2, 0, 1)[None].astype(np.float32)

        outputs = self._session.run(self._output_names, {self._input_name: blob})
        named = dict(zip(self._output_names, outputs, strict=True))

        boxes, scores = _decode(named, self._min_confidence)
        if len(boxes) == 0:
            return Detection(faces=())

        selected = cv2.dnn.NMSBoxes(
            boxes.tolist(), scores.tolist(), self._min_confidence, self._nms_threshold
        )
        if len(selected) == 0:
            return Detection(faces=())
        keep = np.array(selected).flatten()[: self._max_faces]

        faces = []
        for i in keep:
            x, y, w, h = boxes[i]
            faces.append(
                FaceBox(
                    # Del lienzo de 640x640 a fracción del frame original: la
                    # imagen ocupa new_w x new_h en la esquina superior
                    # izquierda, así que se normaliza contra eso.
                    x=float(np.clip(x / new_w, 0.0, 1.0)),
                    y=float(np.clip(y / new_h, 0.0, 1.0)),
                    w=float(np.clip(w / new_w, 0.0, 1.0)),
                    h=float(np.clip(h / new_h, 0.0, 1.0)),
                    confidence=float(scores[i]),
                )
            )
        return Detection(faces=tuple(faces))

    def close(self) -> None:
        # ORT libera la sesión al recolectarla; no hay nada que cerrar a mano.
        return None


def _decode(outputs: dict[str, np.ndarray], min_confidence: float) -> tuple[np.ndarray, np.ndarray]:
    """Traduce las salidas de YuNet a cajas en píxeles de la entrada.

    YuNet es sin anclas: cada celda de la rejilla predice un desplazamiento
    respecto a su esquina y un tamaño en logaritmo. La puntuación es la media
    geométrica de las dos cabezas, clasificación y objetividad.
    """
    boxes: list[list[float]] = []
    scores: list[float] = []

    for stride in _STRIDES:
        cls = outputs[f"cls_{stride}"].reshape(-1)
        obj = outputs[f"obj_{stride}"].reshape(-1)
        bbox = outputs[f"bbox_{stride}"].reshape(-1, 4)

        grid = _INPUT_SIZE // stride
        score = np.sqrt(np.clip(cls, 0.0, 1.0) * np.clip(obj, 0.0, 1.0))

        candidates = np.nonzero(score >= min_confidence)[0]
        for i in candidates:
            col = int(i % grid)
            row = int(i // grid)

            cx = (col + bbox[i, 0]) * stride
            cy = (row + bbox[i, 1]) * stride
            w = float(np.exp(bbox[i, 2]) * stride)
            h = float(np.exp(bbox[i, 3]) * stride)

            boxes.append([float(cx - w / 2.0), float(cy - h / 2.0), w, h])
            scores.append(float(score[i]))

    return np.array(boxes, dtype=np.float32), np.array(scores, dtype=np.float32)
