"""Decodificación de frames. Un payload roto no tumba una sesión."""

from __future__ import annotations

import numpy as np
import pytest

from analyzer import frames, wire
from tests.conftest import draw_scene, encode_jpeg


def test_decodifica_jpeg():
    image = frames.decode(encode_jpeg(draw_scene()), wire.ENCODING_JPEG)
    assert image.shape == (480, 640, 3)
    assert image.dtype == np.uint8


def test_decodifica_rgb24_con_dimensiones():
    raw = np.full((4, 5, 3), 7, np.uint8).tobytes()
    image = frames.decode(raw, wire.ENCODING_RGB24, width=5, height=4)
    assert image.shape == (4, 5, 3)


@pytest.mark.parametrize(
    "payload, encoding, kwargs, match",
    [
        (b"", wire.ENCODING_JPEG, {}, "vacío"),
        (b"esto no es una imagen", wire.ENCODING_JPEG, {}, "no se pudo decodificar"),
        (b"123", wire.ENCODING_RGB24, {}, "dimensiones"),
        (b"123", wire.ENCODING_RGB24, {"width": 5, "height": 4}, "bytes"),
        (b"abc", 99, {}, "no soportada"),
    ],
)
def test_rechaza_lo_que_no_puede_decodificar(payload, encoding, kwargs, match):
    with pytest.raises(frames.DecodeError, match=match):
        frames.decode(payload, encoding, **kwargs)


def test_la_escena_sintetica_no_es_una_imagen():
    """El payload sintético del banco de pruebas es JSON, no píxeles.

    Este worker analiza píxeles: la escena sintética la entiende el stub de Go.
    """
    with pytest.raises(frames.DecodeError):
        frames.decode(b'{"face":true}', wire.ENCODING_SYNTHETIC_SCENE)
