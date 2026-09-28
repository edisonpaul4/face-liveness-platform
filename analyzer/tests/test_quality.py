"""Métricas de calidad: nitidez, luminancia y quemados."""

from __future__ import annotations

import cv2
import numpy as np
import pytest

from analyzer import quality


def checkerboard(size: int = 480, cell: int = 20) -> np.ndarray:
    img = np.zeros((size, size), np.uint8)
    for y in range(0, size, cell):
        for x in range(0, size, cell):
            if (x // cell + y // cell) % 2 == 0:
                img[y:y + cell, x:x + cell] = 255
    return img


def test_la_nitidez_distingue_enfoque_de_desenfoque():
    sharp = checkerboard()
    blurred = cv2.GaussianBlur(sharp, (31, 31), 0)

    assert quality.measure(sharp).sharpness > quality.measure(blurred).sharpness * 10


def test_la_nitidez_de_un_plano_liso_es_cero():
    """Sin bordes no hay nada que enfocar."""
    assert quality.measure(np.full((100, 100), 128, np.uint8)).sharpness == pytest.approx(0.0)


@pytest.mark.parametrize("value, expected", [(0, 0.0), (128, 0.502), (255, 1.0)])
def test_la_luminancia_es_una_fraccion(value, expected):
    got = quality.measure(np.full((64, 64), value, np.uint8)).brightness
    assert got == pytest.approx(expected, abs=0.01)


def test_los_quemados_se_miden_como_fraccion():
    img = np.full((100, 100), 100, np.uint8)
    img[:20, :] = 255  # una quinta parte del frame, quemada

    metrics = quality.measure(img)
    assert metrics.highlight_saturation == pytest.approx(0.2, abs=0.001)


def test_sin_quemados_la_fraccion_es_cero():
    assert quality.measure(np.full((50, 50), 200, np.uint8)).highlight_saturation == 0.0


def test_el_contraste_separa_lo_plano_de_lo_variado():
    flat = quality.measure(np.full((100, 100), 128, np.uint8))
    varied = quality.measure(checkerboard())

    assert flat.contrast == pytest.approx(0.0, abs=0.001)
    assert varied.contrast > 0.4


def test_exige_una_imagen_de_un_canal():
    with pytest.raises(ValueError, match="un canal"):
        quality.measure(np.zeros((10, 10, 3), np.uint8))
