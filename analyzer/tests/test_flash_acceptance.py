"""Criterio de aceptación del analizador de destello.

Con el banco sintético, separa rostro real de foto impresa y de replay en
pantalla, y lo hace igual con cualquier tono de piel.

La cámara del banco ajusta exposición y balance de blancos en cada frame, como
una webcam de verdad. Eso importa: es lo que borraría la señal si se midiera
en absoluto en vez de contra el fondo.
"""

from __future__ import annotations

import pytest

from tests.flashscene import SKIN_TONES, Scene


@pytest.fixture(scope="module")
def three_scenes(flash_lab):
    """Las tres escenas del modelo de amenaza, con el mismo sujeto."""
    return {
        kind: flash_lab.run(Scene(kind=kind, tone=SKIN_TONES[2]))
        for kind in ("real", "photo", "screen")
    }


def test_separa_las_tres_escenas(three_scenes):
    """El criterio de aceptación."""
    real = three_scenes["real"].score
    photo = three_scenes["photo"].score
    screen = three_scenes["screen"].score

    print("\n" + "-" * 78)
    print(f"{'escena':10} {'score':>7} {'corr':>7} {'grad3d':>7} {'sin_pant':>9} {'calidad':>9}")
    for name, result in (("real", real), ("foto", photo), ("pantalla", screen)):
        sub = result.submetrics
        fmt = lambda v: "  n/d" if v is None else f"{v:7.3f}"  # noqa: E731
        print(f"{name:10} {result.score:7.3f} {fmt(sub['correlation'])} {fmt(sub['gradient_3d'])} "
              f"{fmt(sub['screen_absence'])} {'ok' if result.quality_sufficient else 'INSUF':>9}")
    print("-" * 78)

    # Las tres, medibles: nadie se cae por falta de señal.
    for name, result in (("real", real), ("foto", photo), ("pantalla", screen)):
        assert result.quality_sufficient, f"{name}: {result.quality_reason}"

    # Y separadas con holgura.
    assert real.score > 0.85, f"un rostro real debería puntuar alto: {real.score:.3f}"
    assert photo.score < 0.70, f"una foto impresa no debería colarse: {photo.score:.3f}"
    assert screen.score < 0.30, f"un replay no debería colarse: {screen.score:.3f}"
    assert real.score > photo.score + 0.25
    assert photo.score > screen.score + 0.25


def test_el_gradiente_3d_es_lo_que_delata_a_la_foto(three_scenes):
    """Una foto responde al destello igual de bien que una cara.

    Lo que no puede es responder con RELIEVE: la nariz de una persona está más
    cerca de la pantalla que sus pómulos y recibe más luz. Una superficie
    plana recibe lo mismo en todas partes.
    """
    real = three_scenes["real"].score
    photo = three_scenes["photo"].score

    # Las dos siguen al destello igual de bien.
    assert real.submetrics["correlation"] > 0.9
    assert photo.submetrics["correlation"] > 0.9

    # La diferencia está en la dispersión entre regiones.
    real_dispersion = real.raw["gradient_dispersion"]
    photo_dispersion = photo.raw["gradient_dispersion"]
    print(f"\ndispersión entre regiones: rostro={real_dispersion:.4f} foto={photo_dispersion:.4f} "
          f"({real_dispersion / max(photo_dispersion, 1e-9):.1f}x)")

    assert real_dispersion > photo_dispersion * 4.0
    assert real.submetrics["gradient_3d"] > 0.8
    assert photo.submetrics["gradient_3d"] < 0.3


def test_la_nariz_responde_mas_que_los_pomulos(three_scenes):
    """El relieve, región por región. Es geometría, no estadística."""
    raw = three_scenes["real"].score.raw
    nose = raw["amplitude_nose"]
    cheeks = (raw["amplitude_cheek_left"] + raw["amplitude_cheek_right"]) / 2

    print(f"\namplitudes: frente={raw['amplitude_forehead']:.4f} nariz={nose:.4f} "
          f"pómulos={cheeks:.4f}")
    assert nose > cheeks * 1.15, "la nariz debería recibir más luz que los pómulos"

    plana = three_scenes["photo"].score.raw
    plana_nose = plana["amplitude_nose"]
    plana_cheeks = (plana["amplitude_cheek_left"] + plana["amplitude_cheek_right"]) / 2
    assert abs(plana_nose - plana_cheeks) < plana_cheeks * 0.1, "una foto no tiene relieve"


def test_la_pantalla_brilla_y_no_sigue_al_destello(three_scenes):
    """Los INDICIOS siguen ahí; lo que ya no hay es acusación.

    La pantalla sigue delatándose en los datos: brilla mucho más que una cara
    y no sigue a la secuencia. Lo que se ha caído es poder acusar con eso,
    porque las dos mitades del indicio están rotas.

    "No sigue al destello" es la correlación, y sobre caras REALES de webcam ha
    dado 0 · 0,090 · 0,1356 · 0,4934 · 0,703 · 0,815 · 1,0: no separa nada.
    "Brilla mucho" tampoco vale a solas, y la razón ya estaba escrita en el
    propio detector: alguien de piel clara en una habitación a oscuras también
    brilla. Multiplicarlas era `screen_bright_and_deaf`, y al invalidar la
    correlación ese término convertía «no pudimos medir» en
    `attack_emissive_surface` — la misma acusación con otro nombre. Medido: una
    cara real dio `screen_absence` de 0,2194 y 0,2198 contra un suelo de 0,35,
    donde esta escena sintética de pantalla da 0,215.

    Y sin ese término la sospecha de esta escena cae a 0,09, lo que dice algo
    incómodo y que conviene tener escrito: la detección de pantalla del
    destello se sostenía ENTERA sobre la correlación. El reflejo especular, el
    moiré y el bandeo no aportan nada aquí — aunque esta escena es sintética y
    no los simula, así que su valor contra una pantalla de verdad está sin
    medir.

    **Deuda.** Hoy el destello no puede acusar a nadie: sus tres vías
    (`flash_correlation`, `flash_screen_absence` y `flash_gradient_3d`) se
    remontan a la misma correlación. Sigue pesando en la media. Recuperar la
    acusación pasa por hacer medible la correlación —ver la deuda del §4 sobre
    el suelo del 3-4 % de modulación— o por medir moiré y especular contra
    pantallas reales.
    """
    screen = three_scenes["screen"].score
    real = three_scenes["real"].score

    # Los indicios crudos, intactos.
    assert screen.raw["correlation"] < 0.3, "un replay no debería seguir al destello"
    assert screen.raw["face_bg_luminance_ratio"] > real.raw["face_bg_luminance_ratio"] * 1.5

    # Pero ninguno acusa: la correlación no se pudo medir en NINGUNA de las dos
    # escenas, así que el término que las multiplicaba no existe.
    assert "screen_bright_and_deaf" not in screen.raw
    assert real.raw["screen_suspicion"] < 0.3


def test_recupera_el_retardo(flash_lab):
    """Entre pintar un color y verlo pasan de 100 a 300 ms, variables.

    Darlos por sincronizados sería tirar la señal.
    """
    print()
    for real_lag in (0, 100, 180, 260, 340):
        result = flash_lab.run(Scene(kind="real", tone=SKIN_TONES[2], lag_ms=real_lag))
        estimated = result.score.raw["lag_ms"]
        print(f"  retardo real {real_lag:3d} ms → estimado {estimated:5.0f} ms "
              f"(correlación {result.score.raw['correlation']:.3f})")

        assert abs(estimated - real_lag) <= 40, f"retardo mal estimado: {estimated} vs {real_lag}"
        assert result.score.submetrics["correlation"] > 0.9


@pytest.mark.parametrize("tone", SKIN_TONES, ids=lambda t: t.name)
def test_funciona_igual_con_cualquier_tono_de_piel(flash_lab, tone):
    """El requisito de producto, no un detalle de precisión.

    Todo se mide relativo a la línea base del propio sujeto, así que el albedo
    de la piel se cancela en la división. Si en vez de eso se usaran umbrales
    absolutos, el sistema rechazaría de más a las personas de piel oscura.
    """
    real = flash_lab.run(Scene(kind="real", tone=tone)).score
    photo = flash_lab.run(Scene(kind="photo", tone=tone)).score

    print(f"\n  {tone.name:12} albedo={tone.albedo} → real={real.score:.3f} foto={photo.score:.3f} "
          f"(snr={real.raw['signal_to_noise']:.0f})")

    assert real.quality_sufficient
    assert real.submetrics["correlation"] > 0.9, "la correlación no puede depender del tono de piel"
    assert real.submetrics["gradient_3d"] > 0.8, "el relieve no puede depender del tono de piel"
    assert real.score > 0.85
    assert real.score > photo.score + 0.25


def test_ningun_tono_de_piel_queda_por_debajo(flash_lab):
    """Visto en conjunto: la dispersión entre tonos tiene que ser pequeña.

    Un tono que puntúe sistemáticamente peor sería un sesgo, aunque cada caso
    por separado pasara el umbral.
    """
    scores = {t.name: flash_lab.run(Scene(kind="real", tone=t)).score.score for t in SKIN_TONES}
    worst, best = min(scores.values()), max(scores.values())

    print("\n  " + "  ".join(f"{k}={v:.3f}" for k, v in scores.items()))
    print(f"  peor={worst:.3f} mejor={best:.3f} diferencia={best - worst:.3f}")

    assert best - worst < 0.10, f"el tono de piel cambia el score demasiado: {scores}"
