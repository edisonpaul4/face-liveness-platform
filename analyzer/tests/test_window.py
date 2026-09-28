"""Las cuatro sub-métricas de una ventana de pose."""

from __future__ import annotations

import numpy as np
import pytest

from analyzer import window
from analyzer.pose import HeadPose
from analyzer.sessions import PoseSample

FRAME_US = 66_000  # 15 fps


def samples(yaws, *, embeddings=None, landmarks=None, interval_us=FRAME_US, start_us=0):
    """Construye una serie de muestras con los ángulos dados."""
    out = []
    for i, yaw in enumerate(yaws):
        out.append(
            PoseSample(
                seq=i + 1,
                at_us=start_us + i * interval_us,
                pose=HeadPose(yaw=float(yaw), pitch=0.0, roll=0.0),
                scale=0.4,
                landmarks=None if landmarks is None else landmarks[i],
                embedding=None if embeddings is None else embeddings[i],
            )
        )
    return out


def spec(target=25.0, tolerance=5.0, axis="yaw"):
    return window.WindowSpec("w1", axis, target, tolerance, 0, 10**12)


def smooth_turn(peak, n=16):
    """Giro suave de 0 al pico, como el de un cuello."""
    return list(np.linspace(0, peak, n))


# --- 1. cumplimiento ---------------------------------------------------------


def test_llegar_al_objetivo_cumple():
    score = window.analyze(spec(target=25.0), samples(smooth_turn(26)))
    assert score.submetrics["compliance"] == pytest.approx(1.0)
    assert score.raw["peak_deg"] == pytest.approx(26.0, abs=0.01)


def test_la_tolerancia_cuenta_como_llegar():
    """Con objetivo 25 y tolerancia 5, llegar a 20 ya cumple."""
    score = window.analyze(spec(25.0, 5.0), samples(smooth_turn(20)))
    assert score.submetrics["compliance"] == pytest.approx(1.0)


def test_quedarse_corto_cumple_a_medias():
    score = window.analyze(spec(25.0, 5.0), samples(smooth_turn(10)))
    assert 0.0 < score.submetrics["compliance"] < 0.7


def test_girar_al_lado_contrario_no_cumple():
    """Girar mucho, pero al revés, no es girar."""
    score = window.analyze(spec(target=25.0), samples(smooth_turn(-40)))
    assert score.submetrics["compliance"] == pytest.approx(0.0)


def test_objetivo_negativo_mide_hacia_su_lado():
    score = window.analyze(spec(target=-25.0), samples(smooth_turn(-27)))
    assert score.submetrics["compliance"] == pytest.approx(1.0)
    assert score.raw["peak_deg"] == pytest.approx(-27.0, abs=0.01)


def test_sin_muestras_no_hay_cumplimiento():
    """None, no cero: no es que fallara, es que no hubo con qué medir."""
    assert window.analyze(spec(), []).submetrics["compliance"] is None


@pytest.mark.parametrize("axis", ["yaw", "pitch", "roll"])
def test_mide_el_eje_que_le_piden(axis):
    series = [
        PoseSample(seq=i, at_us=i * FRAME_US, scale=0.4,
                   pose=HeadPose(yaw=float(i), pitch=float(2 * i), roll=float(3 * i)))
        for i in range(1, 11)
    ]
    peaks = {"yaw": 10.0, "pitch": 20.0, "roll": 30.0}
    score = window.analyze(spec(axis=axis, target=40.0), series)
    assert score.raw["peak_deg"] == pytest.approx(peaks[axis])


# --- 2. continuidad de trayectoria -------------------------------------------


def test_un_giro_suave_es_continuo():
    score = window.analyze(spec(), samples(smooth_turn(30)))
    assert score.submetrics["continuity"] == pytest.approx(1.0)
    assert score.raw["discontinuity_count"] == 0.0


def test_un_salto_delata_el_corte():
    """Un corte de vídeo aparece como un ángulo que ningún cuello alcanza."""
    yaws = smooth_turn(10, 8) + smooth_turn(30, 8)[4:]  # salto brusco a mitad
    yaws = [0, 2, 4, 6, 8, 45, 47, 49]                  # más claro todavía
    score = window.analyze(spec(), samples(yaws))

    assert score.submetrics["continuity"] < 0.9
    assert score.raw["discontinuity_count"] >= 1
    assert score.raw["max_jump_deg"] > 30


def test_un_salto_enorme_hunde_la_continuidad():
    score = window.analyze(spec(), samples([0, 1, 2, 90, 91, 92]))
    assert score.submetrics["continuity"] < 0.3


def test_los_frames_descartados_no_cuentan_como_salto():
    """Si entre dos frames analizados pasó el triple de tiempo, un salto
    mayor es lo esperable: se descartaron frames por caudal, no hubo corte."""
    # 25° por paso: por encima del umbral (15°) al ritmo normal, dentro de lo
    # esperable si entre frame y frame pasó el triple de tiempo.
    yaws = [0, 25, 50, 75]

    apretado = window.analyze(spec(), samples(yaws, interval_us=FRAME_US))
    espaciado = window.analyze(spec(), samples(yaws, interval_us=FRAME_US * 3))

    assert espaciado.submetrics["continuity"] > apretado.submetrics["continuity"]
    assert espaciado.submetrics["continuity"] == pytest.approx(1.0)


def test_una_sola_muestra_no_tiene_trayectoria():
    assert window.analyze(spec(), samples([10])).submetrics["continuity"] is None


# --- 3. paralaje -------------------------------------------------------------


def test_sin_giro_no_se_puede_medir_paralaje():
    """Quien no gira no es una foto: es alguien que no giró."""
    rng = np.random.default_rng(5)
    marks = [rng.uniform(0.3, 0.7, (60, 2)).astype(np.float32) for _ in range(6)]
    score = window.analyze(spec(), samples([0, 0.5, 1.0, 0.8, 0.3, 0.1], landmarks=marks))

    assert score.submetrics["parallax"] is None
    assert score.raw["angle_span_deg"] < 6.0


def test_sin_landmarks_no_se_puede_medir_paralaje():
    assert window.analyze(spec(), samples(smooth_turn(30))).submetrics["parallax"] is None


# --- 4. consistencia de identidad --------------------------------------------


def unit(vector):
    v = np.array(vector, dtype=np.float32)
    return v / np.linalg.norm(v)


def test_una_identidad_estable_puntua_alto():
    base = unit(np.ones(8))
    drift = [unit(np.ones(8) + np.linspace(0, 0.15, 8) * i) for i in range(6)]
    score = window.analyze(spec(), samples(smooth_turn(30, 6), embeddings=drift))

    assert score.submetrics["identity"] > 0.9
    assert score.raw["embedding_samples"] == 6.0
    assert base is not None


def test_un_cambio_de_cara_hunde_la_identidad():
    """Sustituir la cara a mitad del reto deja un salto en el embedding."""
    same = unit([1, 0, 0, 0, 0, 0, 0, 0])
    other = unit([0, 0, 0, 0, 0, 0, 0, 1])
    score = window.analyze(spec(), samples(smooth_turn(30, 6),
                                            embeddings=[same, same, same, other, other, other]))

    assert score.submetrics["identity"] == pytest.approx(0.0)
    assert score.raw["embedding_min_similarity"] < 0.1


def test_un_frame_borroso_del_que_la_cara_vuelve_no_acusa():
    """Un fallo del tracker no es una sustitución, y confundirlos acusa.

    Girar la cabeza deprisa —que es lo que esta ventana pide— produce
    desenfoque de movimiento, y con él un embedding disparatado durante uno o
    dos frames. Eso deja el MISMO salto suelto que una sustitución, así que
    mirar sólo el peor par consecutivo no puede distinguirlos.

    Lo que los separa es que aquí la cara VUELVE. Medido sobre una webcam real,
    la señal daba 0 · 0,1178 · 0,3335 · 0,7499 en cuatro ventanas de un sujeto
    legítimo, y con el suelo de 0,35 acabó acusándole de `attack_identity_change`
    en una sesión con las dos miradas aceptadas y las dos poses con paralaje 1.
    """
    same = unit([1, 0, 0, 0, 0, 0, 0, 0])
    borroso = unit([0, 1, 0, 0, 0, 0, 0, 0])
    score = window.analyze(spec(), samples(smooth_turn(30, 6),
                                            embeddings=[same, same, borroso, same, same, same]))

    assert score.raw["embedding_min_similarity"] < 0.1, "el salto suelto sigue ahí"
    assert score.raw["embedding_across_jump"] > 0.8, "pero la cara volvió"
    assert score.submetrics["identity"] > 0.8


def test_una_ventana_larga_con_deriva_y_un_borron_no_acusa():
    """El caso que costó el segundo rechazo por identidad.

    Una pose larga —59 frames girando 25°— tiene deriva REAL del embedding, y
    si además cae un frame borroso a mitad, comparar todo-lo-de-antes contra
    todo-lo-de-después mide la deriva y no la sustitución. Medido: 0,2239 en
    esa pose contra un suelo de 0,35, mientras otra del mismo sujeto con 23
    frames daba 0,6582.

    La comparación tiene que ser local al salto: tres frames a cada lado, donde
    la deriva de un giro es despreciable.
    """
    n = 30
    deriva = [unit(np.array([1.0, 0.03 * i, 0, 0, 0, 0, 0, 0])) for i in range(n)]
    borroso = unit([0, 0, 0, 0, 0, 0, 0, 1])
    emb = list(deriva)
    emb[15] = borroso

    score = window.analyze(spec(), samples(smooth_turn(30, n), embeddings=emb))

    assert score.raw["embedding_min_similarity"] < 0.2, "el borrón sigue dejando su salto"
    assert score.raw["embedding_across_jump"] > 0.9, "pero la cara está a los dos lados"
    assert score.submetrics["identity"] > 0.8


def test_la_deriva_lenta_no_es_una_sustitucion():
    """Girar la cabeza cambia el embedding poco a poco. Eso no es un ataque:
    es hacer justo lo que se pidió."""
    vectors = [unit(np.concatenate([[1.0], np.full(7, 0.05 * i)])) for i in range(8)]
    score = window.analyze(spec(), samples(smooth_turn(30, 8), embeddings=vectors))

    assert score.submetrics["identity"] > 0.8


def test_con_un_solo_embedding_no_hay_continuidad_que_medir():
    series = samples(smooth_turn(30, 4), embeddings=[unit(np.ones(4))] + [None] * 3)
    score = window.analyze(spec(), series)
    assert score.submetrics["identity"] is None


# --- resumen -----------------------------------------------------------------


def test_el_resumen_reparte_los_pesos_entre_lo_medido():
    """Lo que no se pudo medir no arrastra el resumen hacia abajo."""
    score = window.analyze(spec(target=25.0), samples(smooth_turn(26)))

    # Sin landmarks ni embeddings: sólo cumplimiento y continuidad, ambos a 1.
    assert score.submetrics["parallax"] is None
    assert score.submetrics["identity"] is None
    assert score.score == pytest.approx(1.0)


def test_el_resumen_esta_acotado_entre_cero_y_uno():
    for yaws in ([0, 90, 0, 90], smooth_turn(60), [0] * 5, [-50, 50, -50]):
        score = window.analyze(spec(), samples(yaws))
        assert 0.0 <= score.score <= 1.0


def test_sin_nada_medible_el_resumen_es_cero():
    assert window.analyze(spec(), []).score == 0.0


def test_el_paralaje_pesa_mas_que_los_demas():
    """Es la señal que de verdad separa volumen de superficie plana."""
    weights = window.ScoreConfig().weights
    others = (weights["compliance"], weights["continuity"], weights["identity"])
    assert weights["parallax"] > max(others)
    assert sum(weights.values()) == pytest.approx(1.0)


def test_el_resultado_se_serializa_entero():
    score = window.analyze(spec(), samples(smooth_turn(30)))
    payload = score.to_dict()

    assert payload["window_id"] == "w1"
    assert payload["axis"] == "yaw"
    assert set(payload["submetrics"]) == {"compliance", "continuity", "parallax", "identity"}
    assert payload["frames_used"] == 16
    assert isinstance(payload["raw"], dict)


def test_solo_se_miran_las_muestras_de_la_ventana():
    """Lo que pasó antes o después del intervalo no cuenta."""
    series = samples(smooth_turn(30, 20))
    narrow = window.WindowSpec("w1", "yaw", 25.0, 5.0, series[5].at_us, series[10].at_us)

    score = window.analyze(narrow, series)
    assert score.frames_used == 6
    assert score.raw["frames_in_window"] == 6.0
