"""MediaPipe Face Landmarker.

Se usa en modo IMAGE, no VIDEO, aunque VIDEO sea más rápido: un worker atiende
varias sesiones a la vez y el modo VIDEO exige marcas de tiempo estrictamente
crecientes por instancia. Con sesiones intercaladas eso no se cumple, y el
resultado sería silenciosamente incorrecto. Una instancia por sesión saldría
carísima en memoria.

Aviso de versión: mediapipe 1.0.x aborta el proceso en macOS ARM
(TensorsToDetectionsCalculator inicializa Metal incondicionalmente y falla).
La línea 0.10.x funciona. Por eso está fijada en pyproject.toml.
"""

from __future__ import annotations

import pathlib

import numpy as np

from analyzer.detector import Detection, DetectorError, FaceBox


class MediaPipeLandmarker:
    """Detector basado en MediaPipe Face Landmarker."""

    name = "mediapipe-face-landmarker"

    def __init__(self, model_path: str, *, max_faces: int = 3) -> None:
        path = pathlib.Path(model_path)
        if not path.is_file():
            raise DetectorError(
                f"falta el modelo de MediaPipe en {path}. Descárgalo con `make models`."
            )

        try:
            import cv2
            import mediapipe as mp
            from mediapipe.tasks import python as mp_python
            from mediapipe.tasks.python import vision
        except ImportError as exc:  # pragma: no cover - dependencia ausente
            raise DetectorError(f"mediapipe no está disponible: {exc}") from exc

        self._cv2 = cv2
        self._mp = mp

        options = vision.FaceLandmarkerOptions(
            base_options=mp_python.BaseOptions(
                model_asset_path=str(path),
                # CPU explícito: el paralelismo lo damos con procesos, no
                # delegando en un acelerador compartido.
                delegate=mp_python.BaseOptions.Delegate.CPU,
            ),
            running_mode=vision.RunningMode.IMAGE,
            num_faces=max_faces,
            output_face_blendshapes=False,
            # La matriz de transformación es de donde sale la pose. Medido:
            # no cuesta latencia apreciable frente a no pedirla.
            output_facial_transformation_matrixes=True,
        )
        self._landmarker = vision.FaceLandmarker.create_from_options(options)

    def detect(self, bgr: np.ndarray) -> Detection:
        """Devuelve una caja por rostro, más landmarks y pose del principal."""
        # SIN reintento con margen, y merece quedar escrito por qué.
        #
        # Una cara pegada a los bordes del encuadre se le escapa al detector
        # aunque se vea perfectamente: medido sobre 58 frames de una sesión
        # real con la cara ocupando el 61 % del ancho y el 81 % del alto, sólo
        # 33 (57 %) se detectaban. Repitiendo sobre la imagen con un borde
        # replicado del 15 % subían a 57 (98 %).
        #
        # Y aun así hubo que quitarlo: MediaPipe devuelve landmarks DISTINTOS
        # según se le dé la imagen tal cual o ampliada. Medido sobre los mismos
        # frames, el desplazamiento de iris difería entre 0,005 y 0,028 —del
        # mismo tamaño que la señal, porque una respuesta real son 0,02-0,06—
        # y el yaw entre 1 y 3,5 grados. Mezclando los dos caminos frame a
        # frame, la serie lleva un sesgo que se enciende y se apaga: es ruido
        # de la magnitud de la señal. Se cambió "no detecta" por "detecta mal",
        # y detectar mal es peor.
        #
        # Un sesgo CONSTANTE sí se cancelaría —la mirada se mide contra el
        # reposo del propio sujeto y la fotometría contra la calibración—, así
        # que ampliar SIEMPRE sería viable. Lo que lo desaconseja es que los
        # ángulos de pose sí son absolutos y se desplazarían entre 1 y 3,5°.
        #
        # El arreglo correcto es de encuadre: que el cliente no deje al sujeto
        # tan pegado a los bordes.
        result = self._run(bgr)
        if not result.face_landmarks:
            return Detection(faces=())

        faces = tuple(_bbox_from_landmarks(lms) for lms in result.face_landmarks)

        # El principal es el más grande: el que está delante.
        primary = max(range(len(faces)), key=lambda i: faces[i].area)
        landmarks = np.array(
            [(lm.x, lm.y, lm.z) for lm in result.face_landmarks[primary]], dtype=np.float32
        )
        transform = None
        if result.facial_transformation_matrixes:
            transform = np.array(
                result.facial_transformation_matrixes[primary], dtype=np.float32
            )

        return Detection(faces=faces, landmarks=landmarks, transform=transform)

    def _run(self, bgr: np.ndarray):  # noqa: ANN202 - tipo de mediapipe
        rgb = self._cv2.cvtColor(bgr, self._cv2.COLOR_BGR2RGB)
        image = self._mp.Image(image_format=self._mp.ImageFormat.SRGB, data=rgb)
        return self._landmarker.detect(image)

    def close(self) -> None:
        self._landmarker.close()


def _bbox_from_landmarks(landmarks) -> FaceBox:  # noqa: ANN001 - tipo de mediapipe
    """Encierra los landmarks en una caja normalizada.

    El Face Landmarker no devuelve puntuación de detección, así que la
    confianza es 1.0 cuando hay rostro. No es una probabilidad: es la
    ausencia de una.
    """
    xs = [lm.x for lm in landmarks]
    ys = [lm.y for lm in landmarks]

    x0, x1 = max(0.0, min(xs)), min(1.0, max(xs))
    y0, y1 = max(0.0, min(ys)), min(1.0, max(ys))

    return FaceBox(x=x0, y=y0, w=max(0.0, x1 - x0), h=max(0.0, y1 - y0), confidence=1.0)
