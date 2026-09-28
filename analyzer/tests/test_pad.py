"""PAD pasivo por textura y contexto.

Lo que se fija aquí es la frontera y el contrato, no la precisión del modelo:
cuánto acierta sólo se puede saber con ataques reales, y eso es trabajo de
/bench.
"""

from __future__ import annotations

import pathlib

import numpy as np
import pytest

from analyzer import pad

MODELS = pathlib.Path(__file__).resolve().parents[1] / "models"
pytestmark = pytest.mark.skipif(
    not (MODELS / "MiniFASNetV2.onnx").exists(),
    reason="faltan los modelos de PAD (make models)",
)


@pytest.fixture(scope="module")
def model() -> pad.PadModel:
    return pad.PadModel(MODELS)


def _frame(width: int = 640, height: int = 480) -> np.ndarray:
    rng = np.random.default_rng(11)
    return rng.integers(0, 255, (height, width, 3), dtype=np.uint8)


def test_emite_las_cuatro_probabilidades(model: pad.PadModel) -> None:
    scores = model.measure(_frame(), (200.0, 150.0, 200.0, 200.0))
    assert scores is not None
    for value in (
        scores.v2_attack_a,
        scores.v2_attack_b,
        scores.v1se_attack_a,
        scores.v1se_attack_b,
    ):
        assert 0.0 <= value <= 1.0


def test_una_caja_degenerada_no_se_mide(model: pad.PadModel) -> None:
    """No medir es una respuesta legítima, y mejor que un número inventado."""
    assert model.measure(_frame(), (10.0, 10.0, 0.0, 0.0)) is None
    assert model.measure(_frame(), (10.0, 10.0, -5.0, 30.0)) is None


def test_el_recorte_respeta_el_encuadre(model: pad.PadModel) -> None:
    """Una caja pegada al borde no puede salirse de la imagen."""
    frame = _frame()
    patch = pad._crop(frame, (600.0, 440.0, 100.0, 100.0), 4.0)
    assert patch is not None
    assert patch.shape[0] <= frame.shape[0]
    assert patch.shape[1] <= frame.shape[1]


def test_el_recorte_se_desplaza_en_vez_de_encogerse() -> None:
    """Una cara pegada al borde produce el mismo tamaño de recorte.

    La referencia (minivision) desplaza el recorte hacia dentro cuando se sale
    del encuadre; encogerlo cambiaría la fracción del 80x80 que ocupa el
    rostro, y ese tamaño relativo es justo lo que fija el modelo entrenado.
    Una selfie tiene la cara pegada al borde, o sea que este es el caso
    normal, no el raro.
    """
    frame = _frame()
    centrado = pad._crop(frame, (300.0, 200.0, 100.0, 100.0), 2.0)
    en_borde = pad._crop(frame, (0.0, 0.0, 100.0, 100.0), 2.0)
    assert centrado is not None
    assert en_borde is not None
    assert en_borde.shape[:2] == centrado.shape[:2]


def test_las_dos_variantes_ven_el_mismo_recorte() -> None:
    """Y hay que saberlo, porque contradice lo que dicen los pesos.

    Los ficheros de MiniFASNet declaran escalas distintas en su nombre (2.7 y
    4), pero ninguna se alcanza: el recorte se limita por el encuadre y con la
    cara cerca —que es lo que exige el gate— manda el límite. Medido sobre el
    banco, 0 de 147 archivos llegan a 2.7.

    Este test existe para que nadie vuelva a repartir su peso a partes iguales
    creyendo que son dos vistas independientes. Son dos modelos sobre una
    entrada, y el perfil de decisión lo refleja.
    """
    assert pad.SCALES["v1se"] == pad.SCALES["v2"]


def test_no_se_publica_la_clase_real() -> None:
    """Concluir que NO hay ataque es decidir, y decide Go (CLAUDE.md §3)."""
    campos = pad.PadScores.__slots__
    assert all("genuine" not in c and "real" not in c for c in campos)
