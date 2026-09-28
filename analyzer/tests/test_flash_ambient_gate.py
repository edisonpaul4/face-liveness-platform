"""La puerta de luz ambiente sólo se pregunta si la correlación no llegó.

Una correlación alta contra una secuencia que el atacante no conoce demuestra
por sí sola que había señal que medir: el ruido no se correlaciona con una
secuencia aleatoria. Exigir además amplitud tiraba medidas buenas.

Salió de datos reales. Sesiones legítimas con correlaciones de 0,815 y 0,703
se declaraban no medibles y acababan en «demasiada luz ambiente», mientras una
con correlación 0,090 —sin respuesta ninguna, sólo la cara más iluminada— se
daba por medida. Estaba al revés en los dos sentidos.
"""

from __future__ import annotations

import numpy as np

from analyzer import flash, rois
from analyzer.calibration import Baseline
from analyzer.photometry import FramePhotometry

SEQUENCE = (
    flash.FlashSegment(color="red", duration_ms=500),
    flash.FlashSegment(color="green", duration_ms=500),
)

#: Respuesta en reposo de cada región contra el fondo, por canal.
REST = np.full((len(rois.FACE_REGIONS), 3), 0.8)


def baseline(noise: float) -> Baseline:
    return Baseline(
        region_ratio=REST.copy(),
        region_noise=np.full_like(REST, noise * 0.8),
        face_luminance=110.0,
        specular_ratio=0.0,
        luminance_ratio=0.7,
        moire_index=0.0,
        banding_index=0.0,
        frames=24,
    )


def frame(gain: np.ndarray) -> FramePhotometry:
    """Un frame cuyo rostro responde `gain` veces su reposo, por canal."""
    background = np.array([100.0, 100.0, 100.0])
    return FramePhotometry(
        regions=REST * gain * background,
        background=background,
        face_luminance=110.0,
        specular_ratio=0.0,
        luminance_ratio=0.7,
        moire_index=0.0,
        banding_index=0.0,
    )


def window(amplitude: float, *, follows: bool) -> list[tuple[int, FramePhotometry]]:
    """Ventana de destello.

    `follows` decide si la piel sigue a los colores o responde a los
    contrarios. Lo segundo produce amplitud de sobra y correlación mala, que
    es el caso que hay que distinguir: se vio responder, y respondió a otra
    cosa.
    """
    samples = []
    start, step_us = 0, 33_000
    total_ms = sum(s.duration_ms for s in SEQUENCE)
    for i in range(int(total_ms * 1000 / step_us)):
        at = start + i * step_us
        elapsed = at / 1000.0
        color = SEQUENCE[0].color if elapsed < SEQUENCE[0].duration_ms else SEQUENCE[1].color
        # BGR: el rojo enciende el canal 2, el verde el 1.
        expected = 2 if color == "red" else 1
        gain = np.ones(3)
        gain[expected if follows else 3 - expected] += amplitude
        samples.append((at, frame(gain)))
    return samples


def analyse(samples, noise: float) -> flash.FlashScore:
    spec = flash.FlashWindowSpec(
        window_id="w",
        sequence=SEQUENCE,
        started_at_us=0,
        ended_at_us=sum(s.duration_ms for s in SEQUENCE) * 1000,
    )
    return flash.analyze(spec, samples, baseline(noise), None)


def test_una_correlacion_fuerte_se_da_por_medida_aunque_suba_poco_el_brillo() -> None:
    """El caso que estaba roto: correlación alta, amplitud modesta."""
    score = analyse(window(0.05, follows=True), noise=0.04)

    assert score.raw["correlation"] > 0.5, "la piel sigue a la secuencia"
    assert score.raw["signal_to_noise"] < flash.FlashConfig().min_signal_to_noise, (
        "el escenario tiene que caer por debajo de la puerta de amplitud, "
        "o el test no comprueba nada"
    )
    assert score.quality_sufficient, (
        f"correlación {score.raw['correlation']:.3f} declarada no medible: {score.quality_reason}"
    )


def test_sin_correlacion_y_sin_amplitud_sigue_siendo_no_medible() -> None:
    """La puerta no se ha quitado, sólo se ha puesto detrás de la correlación."""
    score = analyse(window(0.03, follows=False), noise=0.06)

    assert score.raw["correlation"] < flash.FlashConfig().min_correlation_to_trust
    assert not score.quality_sufficient
    assert score.quality_reason == flash.AMBIENT_REASON


def test_amplitud_alta_con_correlacion_nula_no_acusa() -> None:
    """Hoy no se puede distinguir «respondió a otra cosa» de «la cámara se
    estaba acomodando», así que no se acusa.

    La distinción es buena y sigue en pie como objetivo: «no pudimos verte
    responder» y «te vimos responder a otra cosa» no son lo mismo, y sólo la
    segunda debería acusar. Lo que no hay es forma de medirla.

    El discriminante era `snr`, calculado sobre `mean(|amplitud|)` —valor
    ABSOLUTO—, que dice "algo se movió" y no "se movió como pedía la
    secuencia". Y lo que se mueve en todo destello es el lazo de exposición de
    la cámara acomodándose: medido, `sensor_agc_response` valía 0,9349 en la
    ventana que destapó esto. Con amplitud alta y correlación nula la puerta no
    saltaba y el suelo acusaba de fraude.

    Costó un rechazo real: sesión con las dos miradas aceptadas, las dos poses
    con paralaje 1 y scores 0,93 y 0,98, resuelta en `attack_no_color_response`.
    Y la correlación sobre caras auténticas de esa cámara ha dado 0 · 0,090 ·
    0,1356 · 0,4934 · 0,703 · 0,815 · 1,0: una señal que barre todo el rango en
    sujetos legítimos no puede vetar sola.

    Lo que se pierde es una ACUSACIÓN, no una defensa. A quien no correlaciona
    no se le autentica igual: se le manda repetir, `flash_gradient_3d` y
    `flash_screen_absence` conservan sus suelos acusadores, y la política de
    reintentos escala a revisión manual. Negar el acceso es la propiedad de
    seguridad; llamarlo fraude no lo es.

    **Deuda:** cuando `sensor_agc_response` esté calibrado contra ataques
    reales, el camino de acusación puede volver, condicionado a que la amplitud
    NO la explique la cámara. Es el mismo criterio que el perfil ya aplica a
    los clasificadores pasivos.
    """
    score = analyse(window(0.5, follows=False), noise=0.02)

    assert score.raw["signal_to_noise"] > flash.FlashConfig().min_signal_to_noise
    assert score.raw["correlation"] < flash.FlashConfig().min_correlation_to_trust
    assert score.quality_reason == flash.UNTRUSTED_REASON
    # La correlación y el relieve se caen: comparten las amplitudes
    # contaminadas. Ninguna puede llegar a la fusión como un número bajo, que
    # sería fabricar evidencia contra quien no se pudo medir.
    assert score.submetrics["correlation"] is None
    assert score.submetrics["gradient_3d"] is None
    # Pero la ventana SIGUE VALIENDO, y esto es lo que decide si un replay se
    # caza: la fusión no toma ninguna señal de una ventana con calidad
    # insuficiente, así que tirarla entera dejaría escapar `screen_absence`,
    # que no depende de la correlación y conserva su suelo acusador.
    assert score.quality_sufficient
    assert score.submetrics["screen_absence"] is not None


def test_el_motivo_de_luz_ambiente_es_un_codigo_estable() -> None:
    """El gateway lo reconoce por su cadena exacta para traducirlo.

    `too_much_ambient_light` es la única pista de captura que se le da a la
    persona: habla de su habitación, no del guion ni de qué detector la pilló.
    Si esta cadena cambia aquí sin cambiarla en el gateway, el aviso se pierde
    y el usuario se queda reintentando a ciegas — sin que nada lo delate.

    El acoplamiento vive en `gateway/internal/conn/conn.go`,
    `analyzerAmbientReason`.
    """
    assert flash.AMBIENT_REASON == "flash_ambient_light"


def test_pocos_frames_por_tramo_no_es_una_correlacion_baja() -> None:
    """Una red mala no puede convertirse en una acusación de fraude.

    Caso real y el motivo de que exista esta regla: una sesión legítima por una
    conexión lenta llegó con NUEVE frames para dos tramos de color. La
    correlación salió 0,22, por debajo del suelo de 0,30, y el suelo vetó — la
    persona fue **rechazada por ataque** con un score global de 0,85. El 40 %
    de sus frames se había perdido en la red.

    Cuatro muestras por tramo no dan una correlación baja: dan una correlación
    que no se ha podido calcular. La diferencia entre las dos es la diferencia
    entre reintentar y acusar.
    """
    completa = window(0.05, follows=True)
    escasa = completa[:9]

    con_datos = analyse(completa, noise=0.04)
    sin_datos = analyse(escasa, noise=0.04)

    assert con_datos.quality_sufficient, "el caso de control debería medirse"
    assert not sin_datos.quality_sufficient, (
        "nueve frames para dos tramos se dieron por medibles: eso acaba en acusación"
    )
    assert sin_datos.submetrics["correlation"] is None, (
        "no se puede emitir correlación calculada sobre cuatro muestras por tramo"
    )


def test_un_retardo_grande_necesita_la_cola_de_la_ventana() -> None:
    """La respuesta al último tramo llega DESPUÉS de que la secuencia acabe.

    Entre que la pantalla pinta y la cámara entrega pasan de 100 a 600 ms, así
    que recortar la ventana en el final deja los retardos grandes sin muestras
    dentro del estímulo: `_best_lag` los descarta y elige uno peor.

    Costó el bloqueo de una sesión real por túnel: con la ventana recortada, el
    destello rojo→verde daba correlación 0,3212 con retardo 220 ms y salía NO
    MEDIBLE; con la cola disponible el mejor retardo era 460 ms y la
    correlación 0,7542.
    """
    retardo_us = 300_000
    muestras = [(at + retardo_us, ph) for at, ph in window(0.25, follows=True)]

    spec = flash.FlashWindowSpec(
        window_id="w",
        sequence=SEQUENCE,
        started_at_us=0,
        ended_at_us=sum(s.duration_ms for s in SEQUENCE) * 1000,
    )
    score = flash.analyze(spec, muestras, baseline(0.02), None)

    assert score.quality_sufficient, score.quality_reason
    assert score.raw["lag_ms"] >= 200, "no encontró el retardo"
    assert score.raw["correlation"] > 0.5
