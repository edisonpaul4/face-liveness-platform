"""Fotometría: regiones, medición diferencial y pistas de superficie."""

from __future__ import annotations

import cv2
import numpy as np
import pytest

from analyzer import photometry, rois
from tests.conftest import draw_scene


@pytest.fixture(scope="module")
def landmarks(mediapipe_detector):
    detection = mediapipe_detector.detect(draw_scene())
    assert detection.landmarks is not None
    return detection.landmarks


def test_las_regiones_caen_donde_deben(landmarks):
    patches = rois.face_patches(landmarks, 640, 480)

    assert set(patches) == set(rois.FACE_REGIONS)
    forehead, nose = patches["forehead"], patches["nose"]
    left, right = patches["cheek_left"], patches["cheek_right"]

    assert forehead.y0 < nose.y0, "la frente va por encima de la nariz"
    assert left.x0 < nose.x0 < right.x0, "los pómulos van a los lados de la nariz"
    for patch in patches.values():
        assert patch.area > 0


def test_el_fondo_queda_fuera_del_rostro(landmarks):
    """Si el fondo tocara piel, la división dejaría de cancelar la ganancia."""
    width, height = 640, 480
    face = rois.face_patches(landmarks, width, height)
    background = rois.background_patches(landmarks, width, height)

    assert len(background) >= 1
    face_left = min(p.x0 for p in face.values())
    face_right = max(p.x1 for p in face.values())
    for patch in background:
        assert patch.x1 <= face_left or patch.x0 >= face_right


def test_la_division_por_el_fondo_cancela_la_ganancia(landmarks):
    """El corazón de la medición diferencial.

    Multiplicar el frame entero —que es lo que hace el auto-exposición— no
    puede cambiar la respuesta medida.
    """
    image = draw_scene()
    original = photometry.measure(image, landmarks)

    darker = np.clip(image.astype(np.float32) * 0.55, 0, 255).astype(np.uint8)
    gained = photometry.measure(darker, landmarks)

    assert original is not None and gained is not None
    np.testing.assert_allclose(original.region_ratio(), gained.region_ratio(), rtol=0.05)


def test_sin_landmarks_no_hay_fotometria():
    assert photometry.measure(draw_scene(), None) is None


def test_el_indice_de_bandeo_no_depende_de_lo_oscura_que_sea_la_piel(landmarks):
    """Regresión de un sesgo real que tuvo este módulo.

    La primera versión medía el bandeo como razón pico/mediana del espectro.
    En un rostro de piel oscura el recorte es oscuro y de poco contraste, su
    espectro es casi todo ruido, y esa razón se disparaba: 24,7 frente a 5,4
    con el mismo sujeto. El resultado era sospechar de pantalla por tener la
    piel oscura.

    Medido en amplitud, un rostro sin bandeo no tiene bandeo, sea claro u
    oscuro.
    """
    bright = draw_scene()
    indices = []
    for factor in (1.0, 0.6, 0.35, 0.18):
        darker = np.clip(bright.astype(np.float32) * factor, 0, 255).astype(np.uint8)
        sample = photometry.measure(darker, landmarks)
        assert sample is not None
        indices.append(sample.banding_index)

    print("\n  índice de bandeo por oscuridad:", [round(v, 5) for v in indices])
    assert max(indices) < 0.05, f"el bandeo se dispara al oscurecer: {indices}"
    assert max(indices) - min(indices) < 0.03


def test_detecta_bandas_solo_en_el_rostro(landmarks):
    """Y con bandeo de verdad, sí lo ve.

    Las bandas se pintan SÓLO sobre el rostro, que es lo que hace un replay:
    la pantalla bandea, la pared de detrás no. Si bandeara todo el frame
    —parpadeo de la luz de la habitación— el índice daría cero, y así debe
    ser: eso no es una pantalla.
    """
    image = draw_scene().astype(np.float32)
    height, width = image.shape[:2]
    rows = np.arange(height, dtype=np.float32)
    stripes = 16.0 * np.sin(2 * np.pi * rows / 26.0)

    xs, ys = landmarks[:, 0] * width, landmarks[:, 1] * height
    x0, x1 = int(xs.min()), int(xs.max())
    y0, y1 = int(ys.min()), int(ys.max())

    banded = image.copy()
    banded[y0:y1, x0:x1] += stripes[y0:y1, None, None]
    banded = np.clip(banded, 0, 255).astype(np.uint8)

    clean = photometry.measure(draw_scene(), landmarks)
    striped = photometry.measure(banded, landmarks)

    print(f"\n  sin bandas={clean.banding_index:.5f} con bandas={striped.banding_index:.5f}")
    assert striped.banding_index > clean.banding_index + 0.02


def test_el_parpadeo_de_toda_la_escena_no_es_una_pantalla(landmarks):
    """Si bandea el frame entero, no es un panel: es la luz de la habitación."""
    image = draw_scene().astype(np.float32)
    rows = np.arange(image.shape[0], dtype=np.float32)
    stripes = 16.0 * np.sin(2 * np.pi * rows / 26.0)
    flickering = np.clip(image + stripes[:, None, None], 0, 255).astype(np.uint8)

    clean = photometry.measure(draw_scene(), landmarks)
    global_flicker = photometry.measure(flickering, landmarks)

    assert global_flicker.banding_index < clean.banding_index + 0.02


def test_detecta_el_reflejo_concentrado(landmarks):
    """Una pantalla deja un reflejo pequeño y muy brillante en el cristal."""
    image = draw_scene()
    clean = photometry.measure(image, landmarks)

    glared = image.copy()
    cv2.circle(glared, (330, 220), 22, (255, 255, 255), -1)
    glare = photometry.measure(glared, landmarks)

    assert glare.specular_ratio > clean.specular_ratio + 0.005


def test_el_fondo_quemado_no_cuenta_como_reflejo_del_rostro(landmarks):
    """Regresión de un sesgo real que tuvo este módulo.

    La caja que encierra los landmarks incluye las esquinas, que son fondo.
    Contándolas, con piel muy oscura la cámara subía tanto la ganancia que ese
    fondo se quemaba, y los píxeles quemados se leían como el reflejo
    concentrado de una pantalla: sospechar de replay por tener la piel oscura.

    Medido antes del arreglo: fracción especular 0,0449 con piel muy oscura y
    0,0000 con piel clara, siendo el mismo sujeto real.
    """
    image = draw_scene()
    height, width = image.shape[:2]
    xs, ys = landmarks[:, 0] * width, landmarks[:, 1] * height

    # Fondo quemado a tope, rostro intacto.
    blown = image.copy()
    mask = np.zeros((height, width), np.uint8)
    cv2.fillConvexPoly(mask, cv2.convexHull(np.stack([xs, ys], axis=1).astype(np.int32)), 1)
    blown[mask == 0] = 255

    clean = photometry.measure(image, landmarks)
    with_blown_background = photometry.measure(blown, landmarks)

    print(f"\n  especular: fondo normal={clean.specular_ratio:.5f} "
          f"fondo quemado={with_blown_background.specular_ratio:.5f}")
    assert with_blown_background.specular_ratio < clean.specular_ratio + 0.005, (
        "el fondo quemado se está contando como reflejo del rostro"
    )
