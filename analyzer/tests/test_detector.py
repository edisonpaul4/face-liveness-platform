"""Los dos detectores: MediaPipe y ONNX Runtime."""

from __future__ import annotations

import cv2
import numpy as np
import pytest

from analyzer.detector import DetectorError, FaceBox, create
from tests.conftest import MEDIAPIPE_MODEL, ONNX_MODEL, draw_scene


@pytest.fixture(scope="module")
def mediapipe_detector():
    if not MEDIAPIPE_MODEL.is_file():
        pytest.skip(f"falta {MEDIAPIPE_MODEL}; ejecuta `make models`")
    detector = create("mediapipe", str(MEDIAPIPE_MODEL))
    yield detector
    detector.close()


@pytest.fixture(scope="module")
def onnx_detector():
    if not ONNX_MODEL.is_file():
        pytest.skip(f"falta {ONNX_MODEL}; ejecuta `make models`")
    detector = create("onnx", str(ONNX_MODEL))
    yield detector
    detector.close()


@pytest.mark.parametrize("name", ["mediapipe_detector", "onnx_detector"])
def test_encuentra_el_rostro(request, name):
    detection = request.getfixturevalue(name).detect(draw_scene())

    assert detection.face_detected
    assert len(detection.faces) == 1

    box = detection.primary
    # La cara está centrada y ocupa aproximadamente un cuarto de ancho.
    assert 0.25 < box.x < 0.45
    assert 0.20 < box.y < 0.45
    assert 0.15 < box.w < 0.40
    assert 0.20 < box.h < 0.60
    assert 0.0 <= box.confidence <= 1.0


@pytest.mark.parametrize("name", ["mediapipe_detector", "onnx_detector"])
def test_no_inventa_rostros(request, name):
    """Sin cara no hay caja. Es el caso que más importa: un falso positivo
    aquí se convierte en una sesión que avanza sin nadie delante."""
    detection = request.getfixturevalue(name).detect(draw_scene(face=False))

    assert not detection.face_detected
    assert detection.faces == ()
    assert detection.primary is None


@pytest.mark.parametrize("name", ["mediapipe_detector", "onnx_detector"])
def test_las_cajas_estan_normalizadas(request, name):
    box = request.getfixturevalue(name).detect(draw_scene()).primary

    assert 0.0 <= box.x <= 1.0 and 0.0 <= box.y <= 1.0
    assert 0.0 < box.w <= 1.0 and 0.0 < box.h <= 1.0
    assert box.x + box.w <= 1.001 and box.y + box.h <= 1.001


def test_los_dos_detectores_coinciden(mediapipe_detector, onnx_detector):
    """Dos modelos independientes sobre el mismo frame.

    No tienen por qué dar la misma caja al milímetro, pero si difieren mucho
    es que uno de los dos está mal.
    """
    scene = draw_scene()
    a = mediapipe_detector.detect(scene).primary
    b = onnx_detector.detect(scene).primary

    assert abs(a.x - b.x) < 0.08, f"x: {a.x:.3f} vs {b.x:.3f}"
    assert abs(a.y - b.y) < 0.08, f"y: {a.y:.3f} vs {b.y:.3f}"
    assert abs(a.w - b.w) < 0.10, f"w: {a.w:.3f} vs {b.w:.3f}"


def test_el_onnx_coincide_con_el_decodificador_de_opencv(onnx_detector):
    """Mismo modelo, dos decodificaciones: la mía y la de OpenCV.

    Es la comprobación que hace fiable la vía ONNX: la salida cruda de YuNet
    hay que decodificarla a mano (rejilla sin anclas, tamaño en logaritmo) y
    un error ahí daría cajas plausibles pero mal puestas.
    """
    scene = draw_scene()
    mine = onnx_detector.detect(scene).primary

    reference = cv2.FaceDetectorYN.create(str(ONNX_MODEL), "", (640, 480), 0.6)
    count, faces = reference.detect(scene)
    assert faces is not None and len(faces) >= 1

    x, y, w, h = (float(v) for v in faces[0][:4])
    assert mine.x == pytest.approx(x / 640, abs=0.02)
    assert mine.y == pytest.approx(y / 480, abs=0.02)
    assert mine.w == pytest.approx(w / 640, abs=0.02)
    assert mine.h == pytest.approx(h / 480, abs=0.02)


def test_el_onnx_corre_en_cpu_con_un_hilo(onnx_detector):
    """El paralelismo son procesos.

    Si además cada proceso abriera su propio pool de hilos, se pisarían y la
    latencia por frame subiría en vez de bajar.
    """
    assert onnx_detector.providers == ["CPUExecutionProvider"]


def test_la_caja_calcula_su_area():
    assert FaceBox(0.1, 0.2, 0.3, 0.4, 0.9).area == pytest.approx(0.12)


def test_el_rostro_principal_es_el_mas_grande():
    from analyzer.detector import Detection

    small = FaceBox(0.0, 0.0, 0.1, 0.1, 0.9)
    big = FaceBox(0.5, 0.5, 0.3, 0.3, 0.8)
    assert Detection(faces=(small, big)).primary is big


def test_backend_desconocido():
    with pytest.raises(DetectorError, match="desconocido"):
        create("telepatia", "modelo")


@pytest.mark.parametrize("backend", ["mediapipe", "onnx"])
def test_avisa_si_falta_el_modelo(backend):
    with pytest.raises(DetectorError, match="make models"):
        create(backend, "/no/existe/modelo.bin")


def test_aguanta_un_frame_degenerado(onnx_detector):
    assert onnx_detector.detect(np.zeros((0, 0, 3), np.uint8)).faces == ()
