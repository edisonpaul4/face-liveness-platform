"""Embedding facial para medir continuidad de identidad.

Sirve para una sola cosa: detectar que la cara que aparece a mitad de un reto
no es la misma que había al principio. NO es reconocimiento facial: aquí no se
compara contra ninguna base de datos ni se identifica a nadie. Sólo se mide si
el vector se mantiene o pega un salto.

Los embeddings viven en la memoria del worker mientras dura la sesión y se
tiran con ella. No se persisten ni se emiten: lo que sale al bus es la
similitud entre frames consecutivos, un número.
"""

from __future__ import annotations

import pathlib

import cv2
import numpy as np

#: Plantilla canónica de 5 puntos de ArcFace para recortes de 112x112.
_TEMPLATE = np.array(
    [
        [38.2946, 51.6963],  # iris izquierdo
        [73.5318, 51.5014],  # iris derecho
        [56.0252, 71.7366],  # punta de nariz
        [41.5493, 92.3655],  # comisura izquierda
        [70.7299, 92.2041],  # comisura derecha
    ],
    dtype=np.float32,
)
_CROP_SIZE = 112


class EmbedderError(RuntimeError):
    """No se pudo preparar el extractor de embeddings."""


class SFaceEmbedder:
    """Embeddings de 128 dimensiones con ONNX Runtime en CPU."""

    name = "onnx-sface"
    dimensions = 128

    def __init__(self, model_path: str) -> None:
        path = pathlib.Path(model_path)
        if not path.is_file():
            raise EmbedderError(f"falta el modelo de embeddings en {path}. Ejecuta `make models`.")

        try:
            import onnxruntime as ort
        except ImportError as exc:  # pragma: no cover - dependencia ausente
            raise EmbedderError(f"onnxruntime no está disponible: {exc}") from exc

        options = ort.SessionOptions()
        # Un hilo: el paralelismo son procesos.
        options.intra_op_num_threads = 1
        options.inter_op_num_threads = 1
        # El modelo trae inicializadores en las entradas y ORT avisa de ello
        # en cada carga; es ruido conocido del fichero, no un problema.
        options.log_severity_level = 3

        self._session = ort.InferenceSession(
            str(path), sess_options=options, providers=["CPUExecutionProvider"]
        )
        self._input = self._session.get_inputs()[0].name

    @property
    def providers(self) -> list[str]:
        return self._session.get_providers()

    def embed(self, bgr: np.ndarray, canonical: np.ndarray) -> np.ndarray | None:
        """Devuelve el embedding normalizado del rostro alineado.

        Args:
            bgr: frame completo.
            canonical: los cinco puntos de alineación en coordenadas
                normalizadas [0,1].
        """
        crop = self._align(bgr, canonical)
        if crop is None:
            return None

        blob = crop.transpose(2, 0, 1)[None].astype(np.float32)
        vector = self._session.run(None, {self._input: blob})[0][0]

        norm = float(np.linalg.norm(vector))
        if norm < 1e-6:
            return None
        return (vector / norm).astype(np.float32)

    def _align(self, bgr: np.ndarray, canonical: np.ndarray) -> np.ndarray | None:
        """Recorta y endereza el rostro a la plantilla canónica.

        Sin alinear, el embedding cambia con cada giro de cabeza y la
        continuidad de identidad se confundiría con el propio reto de pose.
        """
        height, width = bgr.shape[:2]
        if height == 0 or width == 0 or canonical is None or len(canonical) != 5:
            return None

        points = canonical.astype(np.float32) * np.array([width, height], dtype=np.float32)
        matrix, _ = cv2.estimateAffinePartial2D(points, _TEMPLATE, method=cv2.LMEDS)
        if matrix is None:
            return None

        return cv2.warpAffine(bgr, matrix, (_CROP_SIZE, _CROP_SIZE), flags=cv2.INTER_LINEAR)


def cosine_similarity(a: np.ndarray, b: np.ndarray) -> float:
    """Similitud coseno entre dos embeddings ya normalizados."""
    return float(np.clip(np.dot(a, b), -1.0, 1.0))
