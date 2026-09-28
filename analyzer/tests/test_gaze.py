"""Medida de la mirada.

Lo que se comprueba aquí es que la señal se mueve en la dirección correcta y
que se calla cuando no puede medir. Cuánto vale un desplazamiento y si eso
cuenta como mirar a un sitio es de Go.
"""

from __future__ import annotations

import numpy as np
import pytest

from analyzer import gaze

WIDTH, HEIGHT = 640, 480


def _landmarks() -> np.ndarray:
    """Una cara sintética con los puntos que la mirada necesita.

    Coordenadas relativas, como las de MediaPipe. Los ojos quedan a un tamaño
    parecido al de una cara real en una webcam.
    """
    lm = np.zeros((478, 3), dtype=np.float64)
    # Ojo izquierdo de la imagen: comisuras, párpados e iris centrado.
    lm[33] = (0.38, 0.45, 0)
    lm[133] = (0.44, 0.45, 0)
    lm[159] = (0.41, 0.43, 0)
    lm[145] = (0.41, 0.47, 0)
    lm[468:473] = (0.41, 0.45, 0)
    # Ojo derecho.
    lm[362] = (0.56, 0.45, 0)
    lm[263] = (0.62, 0.45, 0)
    lm[386] = (0.59, 0.43, 0)
    lm[374] = (0.59, 0.47, 0)
    lm[473:478] = (0.59, 0.45, 0)
    return lm


def _shift(lm: np.ndarray, dx: float, dy: float) -> np.ndarray:
    """Mueve los dos iris, como haría una mirada."""
    out = lm.copy()
    for s in (gaze.IRIS_LEFT, gaze.IRIS_RIGHT):
        out[s, 0] += dx
        out[s, 1] += dy
    return out


def test_iris_centrado_da_desplazamiento_nulo() -> None:
    g = gaze.measure(_landmarks(), WIDTH, HEIGHT)
    assert g is not None
    assert g.offset_x == pytest.approx(0.0, abs=1e-6)
    assert g.offset_y == pytest.approx(0.0, abs=1e-6)
    assert g.agreement == pytest.approx(1.0)


def test_signos_en_coordenadas_de_imagen() -> None:
    """El convenio es el de la pose: se mide desde la cámara.

    Mirar hacia la propia izquierda desplaza el iris a la DERECHA de la
    imagen, porque la cámara está enfrente. Confundir esto es el mismo error
    que se coló dos veces con el yaw y el pitch.
    """
    lm = _landmarks()
    derecha_imagen = gaze.measure(_shift(lm, +0.01, 0), WIDTH, HEIGHT)
    abajo_imagen = gaze.measure(_shift(lm, 0, +0.01), WIDTH, HEIGHT)
    assert derecha_imagen is not None and abajo_imagen is not None
    assert derecha_imagen.offset_x > 0
    assert abajo_imagen.offset_y > 0


def test_desplazamiento_normalizado_por_el_ojo() -> None:
    """La misma mirada mide lo mismo con la cara cerca y lejos.

    Es lo que hace que el número signifique algo sin saber a qué distancia
    está el sujeto: la escala se cancela en la división.
    """
    lm = _landmarks()
    cerca = gaze.measure(_shift(lm, 0.01, 0), WIDTH, HEIGHT)

    # La misma cara al doble de distancia: todo la mitad de grande respecto al
    # centro del encuadre, iris incluido.
    lejos_lm = (lm - 0.5) * 0.5 + 0.5
    lejos = gaze.measure(_shift(lejos_lm, 0.005, 0), WIDTH, HEIGHT)

    assert cerca is not None and lejos is not None
    assert lejos.offset_x == pytest.approx(cerca.offset_x, rel=0.02)


def test_ojo_demasiado_pequeno_no_se_mide() -> None:
    """Sin píxeles suficientes la medida es ruido, y se dice que no se puede.

    Rellenarlo con un cero sería fabricar evidencia contra alguien que
    simplemente está lejos de la cámara (CLAUDE.md §4).
    """
    lm = (_landmarks() - 0.5) * 0.05 + 0.5
    assert gaze.measure(lm, WIDTH, HEIGHT) is None


def test_ojo_cerrado_no_se_mide() -> None:
    """Un ojo entrecerrado mueve el iris sin que la mirada cambie."""
    lm = _landmarks()
    lm[159] = (0.41, 0.4496, 0)
    lm[145] = (0.41, 0.4504, 0)
    lm[386] = (0.59, 0.4496, 0)
    lm[374] = (0.59, 0.4504, 0)
    assert gaze.measure(lm, WIDTH, HEIGHT) is None


def test_sin_iris_no_hay_medida() -> None:
    """Con la malla de 468 puntos no hay iris que medir."""
    assert gaze.measure(np.zeros((468, 3)), WIDTH, HEIGHT) is None
    assert gaze.measure(None, WIDTH, HEIGHT) is None


def test_ojos_en_desacuerdo_bajan_el_acuerdo() -> None:
    """Los dos ojos de una persona miran al mismo sitio.

    Un desacuerdo grande es un iris mal detectado —gafas, reflejo—, no una
    mirada rara. Go decide qué hacer con eso; aquí sólo se reporta.
    """
    lm = _landmarks()
    torcido = lm.copy()
    torcido[gaze.IRIS_LEFT, 0] += 0.012
    torcido[gaze.IRIS_RIGHT, 0] -= 0.012
    g = gaze.measure(torcido, WIDTH, HEIGHT)
    assert g is not None
    assert g.agreement < 0.5
