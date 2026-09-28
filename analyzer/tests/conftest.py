"""Material de prueba común.

El vídeo es sintético a propósito. No se versionan rostros reales en este
repositorio (CLAUDE.md §9), y para lo que hay que medir aquí —caudal, latencia
y forma de las medidas— una cara dibujada sirve: los dos detectores la
encuentran.
"""

from __future__ import annotations

import pathlib

import cv2
import numpy as np
import pytest

from analyzer import wire
from analyzer.metrics import LatencyBudget
from analyzer.pipeline import Pipeline
from analyzer.sessions import SessionState

#: Raíz del repositorio, para llegar a /proto/testdata.
REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
MODELS_DIR = pathlib.Path(__file__).resolve().parents[1] / "models"

MEDIAPIPE_MODEL = MODELS_DIR / "face_landmarker.task"
ONNX_MODEL = MODELS_DIR / "face_detection_yunet_2023mar.onnx"
EMBEDDER_MODEL = MODELS_DIR / "face_recognition_sface_2021dec.onnx"


def draw_scene(width: int = 640, height: int = 480, *, face: bool = True,
               cx: int | None = None, cy: int | None = None, scale: float = 1.0) -> np.ndarray:
    """Dibuja una escena con o sin rostro."""
    img = np.full((height, width, 3), 200, np.uint8)
    # Fondo con algo de textura: un plano liso da nitidez cero y no se parece
    # a nada que salga de una cámara.
    noise = np.random.default_rng(7).integers(0, 18, (height, width, 3), dtype=np.uint8)
    img = cv2.subtract(img, noise)

    if not face:
        return img

    cx = width // 2 if cx is None else cx
    cy = height // 2 if cy is None else cy
    rx, ry = int(95 * scale), int(125 * scale)

    cv2.ellipse(img, (cx, cy), (rx, ry), 0, 0, 360, (185, 160, 140), -1)
    cv2.ellipse(
        img, (cx, cy - int(95 * scale)), (rx, int(60 * scale)), 0, 180, 360, (60, 45, 35), -1
    )
    for dx in (-int(32 * scale), int(32 * scale)):
        cv2.ellipse(img, (cx + dx, cy - int(20 * scale)), (int(16 * scale), int(9 * scale)),
                    0, 0, 360, (250, 250, 250), -1)
        cv2.circle(img, (cx + dx, cy - int(20 * scale)), int(6 * scale), (40, 30, 25), -1)
    cv2.ellipse(img, (cx, cy + int(10 * scale)), (int(8 * scale), int(18 * scale)),
                0, 0, 360, (165, 140, 120), -1)
    cv2.ellipse(img, (cx, cy + int(55 * scale)), (int(28 * scale), int(12 * scale)),
                0, 0, 180, (120, 70, 70), -1)
    return cv2.GaussianBlur(img, (5, 5), 0)


def encode_jpeg(image: np.ndarray, quality: int = 80) -> bytes:
    """Codifica un frame como lo haría el cliente."""
    ok, buf = cv2.imencode(".jpg", image, [cv2.IMWRITE_JPEG_QUALITY, quality])
    if not ok:
        raise RuntimeError("no se pudo codificar el frame")
    return buf.tobytes()


@pytest.fixture(scope="session")
def video_frames() -> list[bytes]:
    """Vídeo de prueba: 150 frames JPEG, que son 10 segundos a 15 fps.

    La cara se mueve despacio, como una persona delante de la cámara.
    """
    frames = []
    for i in range(150):
        # Deriva suave, sin saltos: un rostro real no teletransporta.
        cx = 320 + int(28 * np.sin(i / 18.0))
        cy = 240 + int(14 * np.cos(i / 23.0))
        scale = 1.0 + 0.06 * np.sin(i / 31.0)
        frames.append(encode_jpeg(draw_scene(cx=cx, cy=cy, scale=scale)))
    return frames


@pytest.fixture
def session_state() -> SessionState:
    """Estado caliente de una sesión de prueba."""
    import time

    now = time.monotonic()
    return SessionState(session_id="01J0TESTSESSION", deadline_us=0, created_at=now, last_seen=now)


def make_task(payload: bytes, seq: int = 1) -> wire.FrameTask:
    """Construye el mensaje que el gateway publicaría."""
    return wire.FrameTask(
        seq=seq,
        encoding=wire.ENCODING_JPEG,
        payload=payload,
        received_at_us=1_787_500_000_000_000 + seq * 66_000,
        captured_at_us=1_787_499_999_990_000 + seq * 66_000,
        flags=wire.FLAG_CLIENT_CLOCK_TRUSTED,
    )


class StubDetector:
    """Detector de mentira: devuelve lo que se le diga.

    Permite probar el pipeline y el contrato sin depender de un modelo.
    """

    name = "stub"

    def __init__(self, detection=None) -> None:
        from analyzer.detector import EMPTY

        self.detection = EMPTY if detection is None else detection
        self.calls = 0

    def detect(self, bgr):  # noqa: ANN001, ANN201
        self.calls += 1
        return self.detection

    def close(self) -> None:
        return None


@pytest.fixture
def stub_pipeline() -> Pipeline:
    """Pipeline con detector de mentira, para probar el contrato."""
    return Pipeline(StubDetector(), budget=LatencyBudget("test"))


@pytest.fixture(scope="session")
def mediapipe_pipeline() -> Pipeline:
    """Pipeline con MediaPipe Face Landmarker."""
    if not MEDIAPIPE_MODEL.is_file():
        pytest.skip(f"falta {MEDIAPIPE_MODEL}; ejecuta `make models`")

    from analyzer.detector import create

    return Pipeline(create("mediapipe", str(MEDIAPIPE_MODEL)), budget=LatencyBudget("test"))


@pytest.fixture(scope="session")
def mediapipe_detector():
    """Detector MediaPipe compartido: cargar el modelo cuesta."""
    if not MEDIAPIPE_MODEL.is_file():
        pytest.skip(f"falta {MEDIAPIPE_MODEL}; ejecuta `make models`")

    from analyzer.detector import create

    detector = create("mediapipe", str(MEDIAPIPE_MODEL))
    yield detector
    detector.close()


@pytest.fixture
def session_from_samples():
    """Convierte trazas (yaw, landmarks) en muestras de sesión."""
    from analyzer.pose import HeadPose
    from analyzer.sessions import PoseSample

    def build(trace, *, start_us: int = 0, interval_us: int = 66_000):
        return [
            PoseSample(
                seq=i,
                at_us=start_us + i * interval_us,
                pose=HeadPose(yaw=yaw, pitch=0.0, roll=0.0),
                scale=0.4,
                landmarks=landmarks,
            )
            for i, (yaw, landmarks) in enumerate(trace, start=1)
        ]

    return build


@pytest.fixture(scope="session")
def full_pipeline():
    """El pipeline tal y como corre en producción: detección + embeddings."""
    if not MEDIAPIPE_MODEL.is_file():
        pytest.skip(f"falta {MEDIAPIPE_MODEL}; ejecuta `make models`")

    from analyzer.detector import create

    embedder = None
    if EMBEDDER_MODEL.is_file():
        from analyzer.identity import SFaceEmbedder

        embedder = SFaceEmbedder(str(EMBEDDER_MODEL))

    return Pipeline(
        create("mediapipe", str(MEDIAPIPE_MODEL)),
        budget=LatencyBudget("test"),
        embedder=embedder,
        embedding_stride=3,
    )


@pytest.fixture(scope="session")
def flash_lab(mediapipe_detector):
    """Banco de ensayo de destello, compartido: renderizar cuesta."""
    from tests.flashlab import Lab

    return Lab(mediapipe_detector)
