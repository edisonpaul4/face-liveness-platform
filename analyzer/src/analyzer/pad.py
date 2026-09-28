"""PAD pasivo por textura y contexto (MiniFASNet).

Sólo mide. El modelo fue entrenado como clasificador —real contra dos
familias de ataque— y aquí se publican sus probabilidades de ATAQUE como dos
magnitudes más. La de "real" no se emite a propósito: concluir que no hay
ataque es una decisión, y las decisiones son de Go (CLAUDE.md §3).

Complementa lo que ya hay, que es todo reto-respuesta: un solo frame, sin
información temporal. Lo pasivo pilla lo que lo activo no —una impresión
excelente que se queda quieta— y lo activo pilla lo que lo pasivo no —un
replay bueno, que sí responde a los colores del destello... salvo que no
puede—.

Dos variantes con recortes de distinta amplitud. La V2 apenas se sale de la
cara; la V1SE abarca bastante entorno, que es donde se ven los bordes de una
foto o el bisel de una pantalla. Justo por eso la V1SE es la que más se
debilita ante un replay a pantalla completa sin marco visible.

SIN VALIDAR contra ataques reales. Ver el aviso de calibración más abajo.
"""

from __future__ import annotations

import pathlib
from dataclasses import dataclass

import cv2
import numpy as np
import onnxruntime as ort

#: Lado de la entrada del modelo, en píxeles.
INPUT_SIZE = 80

#: Cuánto se expande el recorte alrededor de la caja del rostro.
#:
#: Los pesos originales declaran 2.7 (V2) y 4 (V1SE) en su propio nombre de
#: fichero, y aquí estaban esos números. **No se alcanzaban nunca.** El
#: recorte se limita por el encuadre —no se puede pedir más imagen de la que
#: hay— y con la cara cerca, que es lo que exige el gate de encuadre, el
#: límite manda siempre. Medido sobre el banco: 0 de 147 archivos llegan a
#: 2.7, y el factor efectivo tiene mediana 1.7.
#:
#: Consecuencia que hay que tener presente: las dos variantes ven el MISMO
#: recorte. No son dos vistas del rostro, son dos modelos sobre una entrada.
#: El perfil de decisión reparte su peso en consecuencia.
#:
#: 2.4 sale de barrer 1.0-5.0 con `bench/runner/pad_crop_sweep.py`: por debajo
#: cae en picado (AUC 0.73 a escala 1.0) y por encima la curva está plana
#: porque ya no se alcanza. Rellenar el borde por reflexión para forzar las
#: escalas altas se probó y empeora: fabrica justo los bordes rectos que el
#: modelo busca para detectar papel y biseles.
SCALES = {"v2": 2.4, "v1se": 2.4}

MODEL_FILES = {"v2": "MiniFASNetV2.onnx", "v1se": "MiniFASNetV1SE.onnx"}

#: Orden de clases del modelo: 0 y 2 son familias de ataque, 1 es rostro real.
CLASS_ATTACK_A = 0
CLASS_GENUINE = 1
CLASS_ATTACK_B = 2


@dataclass(frozen=True, slots=True)
class PadScores:
    """Probabilidades de ataque de cada variante, 0-1.

    Se guardan las dos familias por separado y no su suma: son medidas
    distintas y agregarlas aquí sería empezar a decidir.
    """

    v2_attack_a: float
    v2_attack_b: float
    v1se_attack_a: float
    v1se_attack_b: float


class PadModel:
    """Envoltorio de las dos variantes de MiniFASNet."""

    def __init__(self, model_dir: pathlib.Path) -> None:
        self._sessions: dict[str, ort.InferenceSession] = {}
        for key, filename in MODEL_FILES.items():
            path = model_dir / filename
            if not path.exists():
                raise FileNotFoundError(
                    f"falta {path}. Descárgalo con `make models`."
                )
            # Un hilo por sesión: el paralelismo es por procesos, y varios
            # hilos por proceso empeoran la latencia en vez de mejorarla.
            options = ort.SessionOptions()
            options.intra_op_num_threads = 1
            options.inter_op_num_threads = 1
            self._sessions[key] = ort.InferenceSession(
                str(path), options, providers=["CPUExecutionProvider"]
            )

    def measure(
        self, image: np.ndarray, box: tuple[float, float, float, float]
    ) -> PadScores | None:
        """Mide el frame. None si el recorte no da para nada.

        `box` es (x, y, w, h) en píxeles.
        """
        probs = {}
        for key, session in self._sessions.items():
            patch = _crop(image, box, SCALES[key])
            if patch is None:
                return None
            probs[key] = _infer(session, patch)

        return PadScores(
            v2_attack_a=float(probs["v2"][CLASS_ATTACK_A]),
            v2_attack_b=float(probs["v2"][CLASS_ATTACK_B]),
            v1se_attack_a=float(probs["v1se"][CLASS_ATTACK_A]),
            v1se_attack_b=float(probs["v1se"][CLASS_ATTACK_B]),
        )


def _crop(
    image: np.ndarray, box: tuple[float, float, float, float], scale: float
) -> np.ndarray | None:
    """Recorte expandido alrededor del rostro, como en el entrenamiento.

    Cuando el recorte pedido se sale del encuadre se **desplaza** hacia dentro,
    no se recorta. La diferencia parece cosmética y no lo es: recortando, el
    rostro ocupa una fracción distinta del 80x80 que ve el modelo, y ese
    tamaño relativo es justo lo que el nombre del fichero de pesos fija
    (`2.7_80x80`, `4_80x80`). Recortar convierte una cara pegada al borde
    —una selfie, o sea el caso normal— en una entrada a una escala que el
    modelo no vio entrenando.
    """
    height, width = image.shape[:2]
    x, y, box_w, box_h = box
    if box_w <= 0 or box_h <= 0:
        return None

    # El factor se limita por el propio encuadre: pedir más contexto del que
    # hay deformaría la escala respecto a la que vio el modelo.
    factor = min((height - 1) / box_h, (width - 1) / box_w, scale)
    new_w, new_h = box_w * factor, box_h * factor
    cx, cy = x + box_w / 2, y + box_h / 2

    left, top = cx - new_w / 2, cy - new_h / 2
    right, bottom = cx + new_w / 2, cy + new_h / 2

    # Desplazar, nunca encoger.
    if left < 0:
        right -= left
        left = 0
    if top < 0:
        bottom -= top
        top = 0
    if right > width - 1:
        left -= right - (width - 1)
        right = width - 1
    if bottom > height - 1:
        top -= bottom - (height - 1)
        bottom = height - 1

    left, top = int(max(0, left)), int(max(0, top))
    right, bottom = int(min(width, right)), int(min(height, bottom))
    if right - left < 2 or bottom - top < 2:
        return None
    return image[top:bottom, left:right]


def _infer(session: ort.InferenceSession, patch: np.ndarray) -> np.ndarray:
    """Una pasada. BGR crudo, sin normalizar: así se entrenó."""
    resized = cv2.resize(patch, (INPUT_SIZE, INPUT_SIZE))
    tensor = resized.astype(np.float32).transpose(2, 0, 1)[None]
    logits = session.run(None, {"input": tensor})[0]
    shifted = np.exp(logits - logits.max(axis=1, keepdims=True))
    return (shifted / shifted.sum(axis=1, keepdims=True))[0]
