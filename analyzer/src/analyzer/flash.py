"""Análisis de una ventana de destello.

Mide tres cosas sobre la respuesta del sujeto a la secuencia de colores que
emitió la pantalla, y una cuarta sobre si lo que hay delante es una superficie
emisiva.

Nada de esto decide: aquí no hay veredicto ni umbral de aprobado. Salen
medidas y un resumen 0-1, y Go decide (CLAUDE.md §3).

Los cuatro pilares:

1. **Diferencial**: cada región facial se mide contra el FONDO del mismo
   frame. La cámara aplica su ganancia a todo el frame por igual, así que la
   división la cancela. Sin esto, el auto-exposición compensa el destello y
   borra la señal.
2. **Relativo a la línea base**: nunca umbrales absolutos. El tono de piel se
   cancela en la división, y con él el sesgo que un umbral absoluto
   introduciría contra las pieles oscuras.
3. **Correlación con búsqueda de retardo**: entre pintar un color y verlo en
   un frame pasan de 100 a 300 ms, variables. Asumir sincronía exacta es
   perder la señal.
4. **Gradiente 3D**: frente, nariz y pómulos están a distinta distancia y
   orientación respecto a la pantalla, así que responden con intensidades
   distintas. Una superficie plana responde igual en todas partes.
"""

from __future__ import annotations

from dataclasses import dataclass, field

import numpy as np

from analyzer import rois
from analyzer.calibration import Baseline, relative_response
from analyzer.photometry import ROW_BANDS, FramePhotometry

#: Colores de la paleta, en BGR normalizado, en el orden de los canales de
#: OpenCV. El blanco sube los tres; cada primario, el suyo.
PALETTE: dict[str, tuple[float, float, float]] = {
    "white": (1.0, 1.0, 1.0),
    "blue": (1.0, 0.0, 0.0),
    "green": (0.0, 1.0, 0.0),
    "red": (0.0, 0.0, 1.0),
}


@dataclass(frozen=True, slots=True)
class FlashSegment:
    """Un color mantenido durante una duración."""

    color: str
    duration_ms: int


@dataclass(frozen=True, slots=True)
class FlashWindowSpec:
    """Lo que Go pide medir.

    Trae la secuencia emitida porque sin ella no hay con qué correlacionar, y
    el intervalo en el reloj del servidor. Nada más: ni reto, ni posición en
    el guion, ni umbral de aprobado.
    """

    window_id: str
    sequence: tuple[FlashSegment, ...]
    started_at_us: int
    ended_at_us: int


@dataclass(frozen=True, slots=True)
class FlashConfig:
    """Parámetros del análisis.

    SIN CALIBRAR con material real. Los valores salen de medidas sobre el
    banco sintético; los definitivos tienen que salir de /bench.
    """

    weights: dict[str, float] = field(
        default_factory=lambda: {
            # El gradiente 3D es lo único que separa un rostro de una foto
            # impresa: las dos responden al destello, pero sólo una tiene
            # relieve. Por eso pesa más.
            "correlation": 0.35,
            "gradient_3d": 0.45,
            "screen_absence": 0.20,
        }
    )

    #: Rango de retardo que se explora entre emitir y observar.
    #
    # 600 y no 400. Los mejores retardos medidos sobre sesiones reales caían en
    # 360, 380 y 400 ms —el último, justo en el borde del rango, que es la
    # señal de que el techo se quedaba corto—. Con 600 uno de ellos se resolvió
    # en 460 ms y su correlación subió de 0,6944 a 0,7542.
    #
    # Ampliar el rango no fabrica correlaciones: una sesión sin respuesta se
    # quedó en −0,046 con techo 400 y −0,022 con techo 600. Y a 800 no cambia
    # nada, así que 600 cubre el caso.
    #
    # El retardo es grande porque se acumula todo el camino: red hasta el
    # cliente, decisión, composición del navegador, exposición de la cámara y
    # vuelta. Por un túnel se va a estas cifras con facilidad.
    min_lag_ms: int = 0
    #: Retardo máximo que se busca entre emitir un color y verlo en la cámara.
    #:
    #: Entre revelar y ver la respuesta hay red de bajada, decisión y pintado
    #: del cliente, reacción de la piel, captura y red de subida. Medido con la
    #: alineación anclada en el cliente, la parte que no es red vale 160-300 ms.
    #:
    #: **Este rango puede llegar a durar un tramo entero, y por eso la defensa
    #: contra el aliasing NO vive aquí sino en el guion.** Si la búsqueda puede
    #: desplazar el estímulo medio periodo, encaja una secuencia sobre su
    #: contraria; lo que lo impide es que las dos duraciones de una secuencia
    #: difieran al menos 100 ms (`FlashDurationSpread`, medido: por debajo de
    #: 30 ms la inversa llegaba a 0,85).
    #:
    #: Alargar los tramos por encima de este rango se probó y salió MAL: la
    #: correlación de caras reales se hundió de 0,92-0,99 a 0,05, porque a esa
    #: duración la cámara ya ha compensado el color con su balance de blancos.
    max_lag_ms: int = 600
    lag_step_ms: int = 20

    #: Correlación a partir de la cual la respuesta se considera seguir al
    #: estímulo.
    correlation_reference: float = 0.6
    #: Dispersión entre regiones que se considera propia de un rostro con
    #: volumen. Medido sobre el banco sintético.
    gradient_reference: float = 0.18

    #: Cuánta señal hace falta por encima del ruido de la línea base para que
    #: la medida signifique algo. Por debajo, la luz ambiente está aplastando
    #: el destello: eso es CALIDAD INSUFICIENTE, no un ataque.
    #:
    #: **Sólo se pregunta cuando la correlación es débil.** Ver
    #: `min_correlation_to_trust`.
    min_signal_to_noise: float = 2.0

    #: Correlación a partir de la cual la señal se da por medida sin mirar su
    #: amplitud.
    #:
    #: Una correlación alta contra una secuencia que el atacante no conoce
    #: demuestra por sí sola que había señal que medir: el ruido no se
    #: correlaciona con una secuencia aleatoria, por poco que subiera el
    #: brillo. Ésa es justo la razón de correlacionar en vez de clasificar
    #: cada tramo.
    #:
    #: Preguntarlo al revés —exigir amplitud ANTES de mirar la correlación—
    #: declaraba no medibles sesiones reales seguidas con correlaciones de
    #: 0,815 y 0,703, mientras dejaba pasar como medida una con correlación
    #: 0,090 que sólo tenía la cara más iluminada. Es el mismo error que el
    #: gateway ya cometió una vez y que §4 dejó anotado; aquí estaba repetido.
    min_correlation_to_trust: float = 0.35

    # Referencias de las pistas de superficie emisiva.
    #
    # Se aplican sobre las pistas ESPACIALES —rostro frente a fondo en el
    # mismo frame—, no sobre su crecimiento respecto a la línea base. Tiene
    # que ser así: si el replay ya estaba delante durante la calibración, sus
    # artefactos están en la línea base y compararse contra ella no delata
    # nada. Lo que delata es que el rostro los tenga y la habitación no.
    #
    #: Luminancia del rostro frente al fondo. Sólo cuenta acompañada de
    #: correlación baja: alguien con la piel muy clara y la habitación a
    #: oscuras también brilla, y no por eso es una pantalla.
    luminance_ratio_reference: float = 1.8
    #: Fracción de píxeles quemados dentro del rostro.
    #:
    #: Medido en /bench: no separa. Deja pasar el 61 % de los ataques incluso
    #: en su mejor punto de corte. Se mantiene porque cuesta cero calcularlo y
    #: puede aportar en combinación, pero no se le puede pedir que decida.
    specular_reference: float = 0.02

    #: Exceso de bandeo y de moiré del rostro sobre el fondo, normalizados.
    #:
    #: Son, con diferencia, los mejores discriminantes medidos hasta ahora.
    #: Sobre 29 rostros reales y 120 ataques de ocho tipos:
    #:
    #:   moiré    reales mediana 0,009 (máx 0,270) · ataques mediana 4,55
    #:   bandeo   reales mediana 0,046 (máx 1,533) · ataques mediana 9,88
    #:
    #: Treinta veces de diferencia en la mediana. Las referencias estaban en
    #: 0,15, o sea POR DEBAJO del máximo de los rostros auténticos: un rostro
    #: real con algo de textura saturaba el indicador y se leía como pantalla.
    #: Subirlas por encima de ese máximo no debilita nada contra los ataques,
    #: que están un orden de magnitud más arriba.
    #:
    #: Medido sobre una MUESTRA de 149 archivos. Es un punto de partida con
    #: datos, no una calibración: para eso hace falta partición aparte y más
    #: sujetos.
    banding_index_reference: float = 1.8
    moire_index_reference: float = 0.35

    #: Frames mínimos dentro de la ventana.
    min_frames: int = 8

    #: Frames mínimos POR TRAMO de color para que la correlación signifique
    #: algo.
    #:
    #: El total no basta, y costó un rechazo real. Una sesión legítima por una
    #: red mala llegó con nueve frames para dos tramos: la correlación salió
    #: 0,22, por debajo del suelo de 0,30, y la persona fue **acusada de
    #: ataque** con un score global de 0,85. El 40 % de sus frames se había
    #: perdido en la red.
    #:
    #: Una correlación calculada sobre cuatro muestras por tramo no es una
    #: correlación baja: es una correlación que no se ha podido calcular. Y
    #: confundir las dos convierte una conexión lenta en una acusación de
    #: fraude, que es exactamente lo que §4 prohíbe.
    min_frames_per_segment: int = 5


@dataclass(frozen=True, slots=True)
class FlashScore:
    """Resultado del análisis de una ventana de destello."""

    window_id: str
    score: float
    submetrics: dict[str, float | None]
    raw: dict[str, float]
    frames_used: int
    #: False cuando la señal no llega al suelo de ruido. Lleva a REINTENTAR,
    #: nunca a rechazar a la persona.
    quality_sufficient: bool
    quality_reason: str

    def to_dict(self) -> dict:
        return {
            "window_id": self.window_id,
            "score": round(self.score, 4),
            "submetrics": {
                k: (None if v is None else round(v, 4)) for k, v in self.submetrics.items()
            },
            "raw": {k: round(float(v), 6) for k, v in self.raw.items()},
            "frames_used": self.frames_used,
            "quality_sufficient": self.quality_sufficient,
            "quality_reason": self.quality_reason,
        }


def stimulus_at(spec: FlashWindowSpec, at_us: int, lag_us: int) -> np.ndarray | None:
    """Color que la pantalla estaba emitiendo para lo que se ve en `at_us`.

    Se descuenta el retardo: lo que la cámara capta ahora salió de la pantalla
    hace `lag_us`.
    """
    elapsed_ms = (at_us - lag_us - spec.started_at_us) / 1000.0
    if elapsed_ms < 0:
        return None

    accumulated = 0.0
    for segment in spec.sequence:
        accumulated += segment.duration_ms
        if elapsed_ms < accumulated:
            return np.array(PALETTE.get(segment.color, (0.0, 0.0, 0.0)), dtype=np.float64)
    return None


#: Pesos de luminancia Rec. 601 en orden BGR, que es como vienen los píxeles.
#: Motivo de calidad cuando la luz de la habitación aplasta el destello.
#:
#: Es una CADENA ESTABLE, no prosa: el gateway la reconoce para traducirla al
#: motivo público `too_much_ambient_light`, que es la única pista de captura
#: que se le da a la persona. Si se cambia aquí sin cambiarla allí, el usuario
#: se queda reintentando a ciegas y nada lo delata.
#: Fracción de la ventana que un retardo tiene que seguir cubriendo para que
#: se le considere. Ver la nota en `_best_lag`.
MIN_LAG_OVERLAP = 0.6

AMBIENT_REASON = "flash_ambient_light"

#: Marca una ventana cuya correlación no llegó pero SÍ hubo amplitud.
#:
#: Es distinta de la ambiente porque la pista para la persona es distinta: la
#: habitación no tapó el destello, es que lo que se movió no fue su piel
#: siguiendo los colores. La causa medida es el lazo de exposición de la cámara
#: acomodándose —`sensor_agc_response` valía 0,93 en la sesión que destapó
#: esto— y contra eso el usuario no puede hacer nada salvo repetir.
UNTRUSTED_REASON = "flash_untrusted_correlation"

LUMA_BGR = np.array([0.114, 0.587, 0.299])


def _luma(bgr: np.ndarray) -> float:
    """Luminancia perceptual de un color BGR."""
    return float(np.dot(np.asarray(bgr, dtype=np.float64), LUMA_BGR))


def transition_times(spec: FlashWindowSpec) -> list[tuple[int, np.ndarray, np.ndarray]]:
    """Instantes en que la pantalla cambió de color, con el color de antes y el de después."""
    out = []
    accumulated = 0.0
    for previous, current in zip(spec.sequence, spec.sequence[1:], strict=False):
        accumulated += previous.duration_ms
        at = spec.started_at_us + int(accumulated * 1000)
        before = np.array(PALETTE.get(previous.color, (0.0, 0.0, 0.0)), dtype=np.float64)
        after = np.array(PALETTE.get(current.color, (0.0, 0.0, 0.0)), dtype=np.float64)
        out.append((at, before, after))
    return out


def row_gradient(
    spec: FlashWindowSpec,
    samples: list[tuple[int, FramePhotometry]],
    lag_us: int,
) -> float | None:
    """Cuánto del cambio de color cabe DENTRO de un solo frame.

    Mide el obturador rodante. Un sensor de webcam no captura el frame de
    golpe: lo barre fila por fila durante los ~33 ms que dura. Si la pantalla
    cambia de color a mitad de barrido, las filas de arriba quedan expuestas
    con el color viejo y las de abajo con el nuevo, y ese frame contiene una
    frontera horizontal.

    Por qué es distinto de todo lo demás del sistema: la fila donde cae la
    frontera depende del instante EXACTO en que el servidor decidió cambiar el
    color. No se puede pregrabar, y hereda toda la impredecibilidad del guion.
    Un flujo inyectado compone frames enteros —cambia todo de golpe— así que
    no tiene fila de frontera: el frame de transición está entero en el color
    viejo o entero en el nuevo.

    Devuelve cuánto varía el avance del cambio de color ENTRE las bandas del
    frame de transición, de 0 a 1: cerca de 1 si las de arriba llevan el color
    viejo y las de abajo el nuevo, cerca de 0 si todas van juntas. None si no
    hubo con qué medir.

    Ojo con lo que NO prueba: separa un sensor real de un flujo compuesto. Un
    replay en pantalla delante de una cámara real tiene obturador rodante
    igual, porque hay un sensor de verdad barriendo. Es un eje distinto del
    resto de señales, y ahí está su valor.
    """
    with_rows = [(at, p) for at, p in samples if p.row_profile is not None]
    if len(with_rows) < 4:
        return None

    times = np.array([at for at, _ in with_rows], dtype=np.float64)
    fractions: list[float] = []

    for at, before, after in transition_times(spec):
        # El cambio se ve en la cámara `lag_us` después de emitirse.
        seen_at = at + lag_us
        direction = after - before
        norm = float(np.linalg.norm(direction))
        if norm < 1e-6:
            continue
        direction = direction / norm

        index = int(np.argmin(np.abs(times - seen_at)))
        if index == 0 or index >= len(with_rows) - 1:
            continue

        # Proyección de cada banda sobre la dirección del cambio de color.
        def project(sample: FramePhotometry, axis: np.ndarray = direction) -> np.ndarray:
            assert sample.row_profile is not None
            return np.asarray(sample.row_profile @ axis)

        rows_before = project(with_rows[index - 1][1])
        rows_at = project(with_rows[index][1])
        rows_after = project(with_rows[index + 1][1])

        # Cuánto ha avanzado CADA banda del color viejo al nuevo, entre 0 y 1.
        #
        # Por banda, y no en agregado, porque así se cancela la sombra propia
        # del rostro: una cara nunca es uniforme de arriba abajo —la frente
        # recibe más luz que la barbilla— y esa sombra MULTIPLICA el color, así
        # que aparece igual en el numerador y en el denominador y desaparece
        # sola.
        #
        # Restarla en agregado no funciona, y se probó: la sombra escala con el
        # color que ilumina, así que el gradiente bajo rojo y bajo verde no son
        # el mismo número. Dividir sí.
        span = rows_after - rows_before

        # El denominador tiene que SIGNIFICAR algo, y ésta es la tercera vez
        # que esta señal falla por no exigírselo.
        #
        # Dividir por un salto minúsculo convierte ruido en un progreso
        # enorme, y al recortarlo a [0,1] unas bandas caen en 0 y otras en 1:
        # el recorrido sale 1,000 —el tope— sin que haya barrido ninguno. Se
        # vio en tres grabaciones seguidas.
        #
        # Se exige que el salto del frame sea comparable al de la transición
        # completa, y que ésta destaque sobre el reposo de la propia serie.
        escala = float(np.median(np.abs(span)))
        reposo = float(np.median(np.abs(np.diff(rows_before))))
        if escala < max(4.0 * reposo, 1e-3):
            continue

        usable = np.abs(span) > 0.3 * escala
        if usable.sum() < ROW_BANDS // 2:
            continue
        progress = (rows_at[usable] - rows_before[usable]) / span[usable]

        # Con barrido por filas, las bandas de arriba están casi en el color
        # viejo (progreso ~0) y las de abajo en el nuevo (~1): el recorrido es
        # amplio. Un frame compuesto cambia entero de golpe, así que todas las
        # bandas llevan el MISMO progreso y el recorrido es cero.
        fractions.append(float(min(1.0, np.ptp(np.clip(progress, 0.0, 1.0)))))

    if not fractions:
        return None
    # El MAYOR de las transiciones: basta con haber pillado una a mitad de
    # barrido. Que otra caiga justo entre dos frames no dice nada.
    return max(fractions)


def sensor_response(
    spec: FlashWindowSpec,
    samples: list[tuple[int, FramePhotometry]],
    lag_us: int,
) -> float | None:
    """Cuánto reaccionó el control automático de la cámara al destello.

    Se mide en el FONDO, y ésa es toda la idea. El fondo está lejos de la
    pantalla y no recibe el destello, así que en teoría no debería moverse.
    Se mueve igualmente, y sólo puede ser por una razón: los lazos automáticos
    de la cámara —exposición, ganancia y balance de blancos— reaccionando a
    que de pronto entra mucha más luz por el centro del encuadre.

    Eso es una propiedad de un sensor físico con realimentación. Un plano de
    píxeles inyectado no tiene lazo de control y no puede producirla.

    Ya estaba medido sin saberlo y archivado como deuda: el MISMO blanco daba
    0,550 de respuesta como primer tramo y 0,129 como tercero, con el fondo
    quieto. Esa caída es la curva de convergencia del control automático.

    Devuelve la correlación absoluta entre la luminancia del fondo y la del
    estímulo emitido, de 0 a 1. El signo se descarta a propósito: la cámara
    puede compensar hacia arriba o hacia abajo según de dónde venga, y lo que
    importa es que responda, no en qué dirección.

    Se usa LUMINANCIA perceptual, no la media de los tres canales. La media es
    idéntica para los tres primarios saturados —rojo, verde y azul valen 85 los
    tres— así que con ella el estímulo sería una constante y no habría nada que
    correlacionar. Un sensor no reacciona al promedio de los canales: reacciona
    a la luz que le entra, donde el verde pesa el doble que el rojo y seis
    veces el azul.
    """
    if len(samples) < 6:
        return None

    stimulus, observed = [], []
    for at, photo in samples:
        colour = stimulus_at(spec, at, lag_us)
        if colour is None:
            continue
        stimulus.append(_luma(colour))
        observed.append(_luma(photo.background))

    if len(observed) < 6:
        return None

    a = np.asarray(stimulus, dtype=np.float64)
    b = np.asarray(observed, dtype=np.float64)
    if a.std() < 1e-9 or b.std() < 1e-9:
        return None
    return float(abs(np.corrcoef(a, b)[0, 1]))


def subsurface_red(
    spec: FlashWindowSpec,
    samples: list[tuple[int, FramePhotometry]],
    baseline: Baseline,
    lag_us: int,
) -> float | None:
    """Cuánto rojo devuelve la piel cuando se la ilumina con OTRO color.

    Mide dispersión subsuperficial. La luz no rebota en la piel viva: entra
    unos milímetros, se dispersa dentro del tejido y sale por otro sitio. Y ese
    recorrido filtra por longitud de onda — la hemoglobina y la melanina se
    comen el azul y el verde en el primer milímetro, mientras el rojo penetra
    varios y vuelve a salir. Es lo que hace que una oreja a contraluz se vea
    roja, y lo que hace difícil renderizar piel creíble.

    Consecuencia medible: si la pantalla emite AZUL saturado, una cara viva
    devuelve una componente roja que el azul emitido no explica. Un papel
    impreso o una máscara de silicona reflejan en superficie y no la tienen.

    Sólo se mide en tramos que NO son rojos: con un destello rojo el rojo
    devuelto viene del propio estímulo y no dice nada. Si la secuencia no trae
    ningún tramo no-rojo, devuelve None — no medible, que es distinto de cero.

    Devuelve la fracción de la respuesta que cae en el canal rojo sin que el
    color emitido la justifique, entre 0 y 1.
    """
    aportes: list[float] = []
    for at, photo in samples:
        colour = stimulus_at(spec, at, lag_us)
        if colour is None:
            continue
        # BGR: el canal 2 es el rojo. Un tramo rojo no sirve de sonda.
        if float(np.argmax(colour)) == 2:
            continue

        response = relative_response(photo, baseline).mean(axis=0)
        magnitude = float(np.linalg.norm(response))
        if magnitude < 1e-6:
            continue

        # Lo que el color emitido explica, y lo que sobra.
        direction = np.asarray(colour, dtype=np.float64)
        direction = direction / max(float(np.linalg.norm(direction)), 1e-9)
        explained = float(response @ direction) * direction
        excess = response - explained

        # De ese exceso, cuánto es rojo. Positivo = la piel devolvió rojo que
        # nadie le mandó.
        aportes.append(float(excess[2]) / magnitude)

    if len(aportes) < 4:
        return None
    return float(np.clip(np.median(aportes), 0.0, 1.0))


def analyze(
    spec: FlashWindowSpec,
    samples: list[tuple[int, FramePhotometry]],
    baseline: Baseline | None,
    config: FlashConfig | None = None,
) -> FlashScore:
    """Mide una ventana de destello.

    Args:
        samples: pares (instante en microsegundos del reloj del SERVIDOR,
            fotometría del frame), ya recortados a la ventana o no.
        baseline: línea base de la calibración. Sin ella no se analiza nada.
    """
    config = config or FlashConfig()
    # La ventana llega hasta el final de la secuencia MÁS el retardo máximo
    # que se va a buscar.
    #
    # Entre que la pantalla pinta un color y la cámara lo entrega pasan de 100
    # a 400 ms, así que la respuesta al ÚLTIMO tramo cae fuera de la secuencia.
    # Recortando en `ended_at_us`, los retardos grandes se quedan con muy pocas
    # muestras dentro del rango del estímulo y `_best_lag` los descarta por
    # `min_frames`: se elige un retardo peor y se tira la señal.
    #
    # Medido sobre una sesión real por túnel, destello rojo→verde: recortando
    # en el final daba correlación 0,3212 con retardo 220 ms y la ventana salía
    # NO MEDIBLE; con la cola disponible, el mejor retardo era 380 ms y la
    # correlación 0,6931.
    #
    # No cambia lo que Python sabe del guion (§3): es su propio buffer, sólo
    # que sin recortarlo antes de tiempo.
    tail_us = config.max_lag_ms * 1000
    window = [
        (at, p)
        for at, p in samples
        if spec.started_at_us <= at <= spec.ended_at_us + tail_us
    ]
    raw: dict[str, float] = {"frames_in_window": float(len(window))}

    if baseline is None:
        return _insufficient(spec, raw, 0, "sin línea base de calibración")
    raw.update({f"baseline_{k}": v for k, v in baseline.to_dict().items()})

    if len(window) < config.min_frames:
        return _insufficient(spec, raw, len(window), "frames insuficientes en la ventana")

    # Y suficientes POR TRAMO, no sólo en total: con dos tramos y nueve frames
    # la correlación se calcula sobre cuatro muestras y no significa nada.
    needed = config.min_frames_per_segment * max(1, len(spec.sequence))
    if len(window) < needed:
        return _insufficient(
            spec,
            raw,
            len(window),
            f"{len(window)} frames para {len(spec.sequence)} tramos: "
            f"hacen falta {needed} para correlacionar",
        )

    times = np.array([at for at, _ in window], dtype=np.int64)
    # (n, 4, 3): respuesta relativa a la línea base, en logaritmo.
    response = np.stack([relative_response(p, baseline) for _, p in window])

    lag_us, correlation, per_region = _best_lag(spec, times, response, config)
    raw["lag_ms"] = lag_us / 1000.0
    raw["correlation"] = correlation

    amplitudes = _amplitudes(spec, times, response, lag_us)
    signal = float(np.mean(np.abs(amplitudes))) if amplitudes.size else 0.0
    noise = max(baseline.noise_floor, 1e-4)
    snr = signal / noise
    raw["modulation_amplitude"] = signal
    raw["noise_floor"] = noise
    raw["signal_to_noise"] = snr

    for index, name in enumerate(rois.FACE_REGIONS):
        raw[f"amplitude_{name}"] = float(amplitudes[index]) if index < amplitudes.size else 0.0
        raw[f"correlation_{name}"] = float(per_region[index]) if index < per_region.size else 0.0

    screen, screen_raw = _screen_suspicion(window, baseline, correlation, config)
    raw.update(screen_raw)

    # Las dos medidas de SENSOR. Van en `raw` y NO en las sub-métricas: miden
    # otra cosa que el resto de la ventana —si hay un sensor físico detrás, no
    # si hay una cara viva delante— y están sin calibrar contra ataques reales.
    # Meterlas en el score les daría voto antes de saber qué valen.
    rolling = row_gradient(spec, window, lag_us)
    if rolling is not None:
        raw["sensor_rolling_shutter"] = rolling
    agc = sensor_response(spec, window, lag_us)
    if agc is not None:
        raw["sensor_agc_response"] = agc
    sss = subsurface_red(spec, window, baseline, lag_us)
    if sss is not None:
        raw["skin_subsurface_red"] = sss

    # La luz ambiente aplastando el destello no es un ataque: es una medida
    # que no se puede hacer. Se dice, y Go manda repetir.
    #
    # Pero sólo se pregunta si la correlación NO llegó. Con una correlación
    # fuerte ya está demostrado que había señal, y exigirle además amplitud
    # tira medidas buenas: el destello aporta un porcentaje pequeño del brillo
    # de una cara y aun así la sigue.
    if correlation < config.min_correlation_to_trust:
        # Una correlación que no llega es una ventana que NO SE PUDO MEDIR, y
        # nunca una acusación. La amplitud no puede desbloquear ese camino.
        #
        # Antes hacía falta además `snr < min_signal_to_noise`, y ese `and` era
        # el agujero: `snr` se calcula sobre `mean(|amplitud|)` —valor
        # ABSOLUTO—, así que dice "algo se movió", no "se movió como pedía la
        # secuencia". Y lo que se mueve es el lazo de exposición de la cámara
        # acomodándose al destello, que ya está medido aparte
        # (`sensor_agc_response`). Con amplitud alta y correlación nula, la
        # puerta no saltaba y el suelo de 0,30 acusaba de fraude.
        #
        # Costó un rechazo real: una sesión con las dos miradas aceptadas, las
        # dos poses con paralaje 1 y scores de 0,93 y 0,98, resuelta en
        # `attack_no_color_response` con `sensor_agc_response` en 0,9349. Es la
        # misma lección que §4 ya había aprendido dos veces: distinguir "hubo
        # movimiento" de "hubo señal medible".
        #
        # La correlación sobre caras reales de esta cámara ha dado 0 · 0,090 ·
        # 0,1356 · 0,4934 · 0,703 · 0,815 · 1,0. Una señal que barre todo el
        # rango en sujetos auténticos no puede acusar por sí sola.
        if snr < config.min_signal_to_noise:
            # No se vio NADA: ni amplitud ni correlación. La habitación tapó el
            # destello y no hay ninguna medida que salvar.
            result = _insufficient(spec, raw, len(window), AMBIENT_REASON)
            return FlashScore(
                window_id=result.window_id,
                score=result.score,
                submetrics={**result.submetrics, "screen_absence": 1.0 - screen},
                raw=raw,
                frames_used=len(window),
                quality_sufficient=False,
                quality_reason=result.quality_reason,
            )

        # Hubo amplitud pero no correlación. Se invalida LA CORRELACIÓN, no la
        # ventana entera, y la diferencia decide si un replay se caza o se
        # escapa.
        #
        # `correlation` y `gradient_3d` se caen juntos porque comparten las
        # amplitudes contaminadas. `screen_absence` no: mide si la superficie
        # es emisora, y eso no depende de que la piel siguiera a los colores.
        #
        # Tirar la ventana entera habría costado la defensa: la fusión no toma
        # NINGUNA señal de una ventana con calidad insuficiente, así que un
        # replay con `screen_absence` de 0,215 —muy por debajo de su suelo de
        # 0,35— habría escapado a `retry` en vez de `reject`. Comprobado en
        # tests/test_flash_acceptance.py con la escena de pantalla.
        raw["correlation_untrusted"] = 1.0
        # Se recalcula SIN la correlación, y se borra el término viejo: dejarlo
        # en `raw` sería publicar un número calculado con una correlación que
        # acabamos de declarar no medible.
        raw.pop("screen_bright_and_deaf", None)
        screen, screen_raw = _screen_suspicion(window, baseline, None, config)
        raw.update(screen_raw)
        return FlashScore(
            window_id=spec.window_id,
            score=0.0,
            submetrics={
                "correlation": None,
                "gradient_3d": None,
                "screen_absence": 1.0 - screen,
            },
            raw=raw,
            frames_used=len(window),
            quality_sufficient=True,
            quality_reason=UNTRUSTED_REASON,
        )

    gradient, gradient_raw = _gradient_3d(amplitudes, config)
    raw.update(gradient_raw)

    submetrics: dict[str, float | None] = {
        "correlation": float(np.clip(correlation / config.correlation_reference, 0.0, 1.0)),
        "gradient_3d": gradient,
        "screen_absence": 1.0 - screen,
    }

    return FlashScore(
        window_id=spec.window_id,
        score=_combine(submetrics, config.weights),
        submetrics=submetrics,
        raw=raw,
        frames_used=len(window),
        quality_sufficient=True,
        quality_reason="",
    )


def _insufficient(
    spec: FlashWindowSpec, raw: dict[str, float], frames: int, reason: str
) -> FlashScore:
    """Resultado cuando no se puede medir.

    Todas las sub-métricas a None y calidad insuficiente. Un score bajo aquí
    se leería como ataque, y lo que pasa es que no hubo con qué medir.
    """
    return FlashScore(
        window_id=spec.window_id,
        score=0.0,
        submetrics={"correlation": None, "gradient_3d": None, "screen_absence": None},
        raw=raw,
        frames_used=frames,
        quality_sufficient=False,
        quality_reason=reason,
    )


def _best_lag(
    spec: FlashWindowSpec, times: np.ndarray, response: np.ndarray, config: FlashConfig
) -> tuple[int, float, np.ndarray]:
    """Busca el retardo que mejor alinea estímulo y respuesta.

    Entre que la pantalla pinta un color y la cámara lo entrega pasan de 100 a
    300 ms, y no siempre los mismos: render, captura, codificación y red. Dar
    la sincronía por supuesta es tirar la señal.
    """
    best = (0, -1.0, np.zeros(len(rois.FACE_REGIONS)))

    # El solape se mide contra las muestras que caen dentro de la SECUENCIA sin
    # retardo, que es el máximo que cualquier retardo puede aspirar a cubrir.
    #
    # Ni contra la ventana entera —lleva una cola de hasta 600 ms para que los
    # retardos grandes tengan datos, y esa cola nunca solapa con el estímulo—
    # ni contra el intervalo declarado, que puede ser más largo que la suma de
    # los tramos: medido en un caso de test, 40 muestras en la ventana y sólo
    # 17 dentro de la secuencia.
    dentro = sum(1 for at in times if stimulus_at(spec, int(at), 0) is not None)
    minimo_solape = int(max(1, dentro) * MIN_LAG_OVERLAP)

    for lag_ms in range(config.min_lag_ms, config.max_lag_ms + 1, config.lag_step_ms):
        lag_us = lag_ms * 1000
        stimulus = []
        keep = []
        for index, at in enumerate(times):
            value = stimulus_at(spec, int(at), lag_us)
            if value is None:
                continue
            stimulus.append(value)
            keep.append(index)

        # Además de un mínimo absoluto, un mínimo de SOLAPE.
        #
        # Con el rango de retardo ampliado a 600 ms, un retardo grande puede
        # dejar fuera del estímulo a media ventana y aun así superar
        # `min_frames`. Entonces gana por tener menos muestras, no por alinear
        # mejor: se elegía un retardo donde los tramos apenas rozaban los datos
        # y la amplitud salía cero, o sea ventana no medible por culpa de la
        # propia búsqueda.
        if len(keep) < config.min_frames or len(keep) < minimo_solape:
            continue

        stim = np.stack(stimulus)                 # (m, 3)
        obs = response[np.array(keep)]            # (m, 4, 3)

        per_region = np.array(
            [_correlate(stim, obs[:, r, :]) for r in range(obs.shape[1])], dtype=np.float64
        )
        # Ponderada por AMPLITUD, no media a secas.
        #
        # La pantalla no ilumina la cara por igual: es una fuente de área en
        # una posición concreta, así que unas regiones la miran de frente y
        # otras quedan lavadas por la luz de la habitación. Medido en una
        # sesión real, en los DOS destellos la mejilla izquierda correlacionó
        # 0,66 y 0,58 mientras frente y mejilla derecha daban 0,06 · 0,09 ·
        # 0,07 · −0,02 — y era la región con el doble de amplitud que las
        # demás, o sea justo donde llegaba la luz.
        #
        # Con la media, esa respuesta real se diluía a 0,32 y 0,30 y la ventana
        # se declaraba no medible por 0,03. Dar más voz a las regiones donde SÍ
        # llegó modulación devuelve 0,41.
        #
        # Ponderar y no quedarse con el máximo: con cuatro regiones y quince
        # muestras, el máximo de cuatro correlaciones de ruido ronda ya el 0,35
        # del umbral. La media ponderada sigue dando ~0 con ruido puro porque
        # sigue siendo una media.
        weights = np.abs(_amplitudes_per_region(obs))
        total = float(weights.sum())
        if total > 1e-9:
            aggregate = float(np.nansum(per_region * weights) / total)
        else:
            aggregate = float(np.nanmean(per_region))
        if aggregate > best[1]:
            best = (lag_us, aggregate, per_region)

    return best


def _amplitudes_per_region(obs: np.ndarray) -> np.ndarray:
    """Cuánta modulación vio cada región, en la ventana entera.

    Es el peso de la correlación: una región cuya señal no se movió no tiene
    nada que decir sobre si la piel siguió a los colores.
    """
    # Desviación típica por canal, promediada: recorrido sin que un solo frame
    # raro lo decida.
    return np.asarray(obs.std(axis=0).mean(axis=1), dtype=np.float64)


def _correlate(stimulus: np.ndarray, observed: np.ndarray) -> float:
    """Correlación media por canal entre lo emitido y lo observado."""
    values = []
    for channel in range(stimulus.shape[1]):
        s = stimulus[:, channel] - stimulus[:, channel].mean()
        o = observed[:, channel] - observed[:, channel].mean()
        denominator = float(np.linalg.norm(s) * np.linalg.norm(o))
        if denominator < 1e-9:
            continue
        values.append(float(np.dot(s, o) / denominator))
    return float(np.mean(values)) if values else 0.0


def _amplitudes(
    spec: FlashWindowSpec, times: np.ndarray, response: np.ndarray, lag_us: int
) -> np.ndarray:
    """Cuánto modula cada región, en el retardo elegido.

    Es la pendiente de la respuesta frente al estímulo: cuánto sube la región
    por cada unidad de color emitido.
    """
    stimulus, keep = [], []
    for index, at in enumerate(times):
        value = stimulus_at(spec, int(at), lag_us)
        if value is None:
            continue
        stimulus.append(value)
        keep.append(index)

    if len(keep) < 2:
        return np.zeros(len(rois.FACE_REGIONS))

    stim = np.stack(stimulus)
    obs = response[np.array(keep)]

    amplitudes = []
    for region in range(obs.shape[1]):
        slopes = []
        for channel in range(stim.shape[1]):
            s = stim[:, channel] - stim[:, channel].mean()
            variance = float(np.dot(s, s))
            if variance < 1e-9:
                continue
            o = obs[:, region, channel] - obs[:, region, channel].mean()
            slopes.append(abs(float(np.dot(s, o) / variance)))
        amplitudes.append(float(np.mean(slopes)) if slopes else 0.0)
    return np.array(amplitudes, dtype=np.float64)


def _gradient_3d(
    amplitudes: np.ndarray, config: FlashConfig
) -> tuple[float | None, dict[str, float]]:
    """Dispersión de la respuesta entre regiones.

    En un rostro, la nariz está más cerca de la pantalla que los pómulos y
    orientada de otra forma, así que responde más. En una superficie plana
    todas las regiones reciben y devuelven lo mismo.

    Se mide con el coeficiente de variación, que es adimensional: así no
    depende de cuánta luz llegue ni del tono de piel.
    """
    if amplitudes.size < 2:
        return None, {}

    mean = float(np.mean(amplitudes))
    if mean <= 1e-9:
        return None, {"gradient_dispersion": 0.0}

    dispersion = float(np.std(amplitudes) / mean)
    raw = {
        "gradient_dispersion": dispersion,
        "amplitude_mean": mean,
        "amplitude_spread": float(np.max(amplitudes) - np.min(amplitudes)),
    }
    return float(np.clip(dispersion / config.gradient_reference, 0.0, 1.0)), raw


def _screen_suspicion(
    window: list[tuple[int, FramePhotometry]],
    baseline: Baseline,
    correlation: float | None,
    config: FlashConfig,
) -> tuple[float, dict[str, float]]:
    """Indicios de que lo que hay delante es una superficie emisiva.

    `correlation` es None cuando no se pudo medir. Entonces el término
    "brilla y es sordo" NO se calcula: preguntar si fue sordo exige haber
    podido oír, y sin eso el término convierte una correlación que no llegó en
    una acusación con otro nombre. Ver la nota en el propio término.

    Una pantalla fabrica su propia luz: brilla mucho y apenas modula con el
    destello. Además deja huellas propias —reflejo concentrado en el cristal,
    moiré de su rejilla de píxeles y bandeo por desfase de refresco— que se
    miden como crecimiento respecto a la línea base, nunca en absoluto.
    """
    luminance_ratio = float(np.mean([p.luminance_ratio for _, p in window]))
    specular = float(np.mean([p.specular_ratio for _, p in window]))
    moire_index = float(np.mean([p.moire_index for _, p in window]))
    banding_index = float(np.mean([p.banding_index for _, p in window]))

    # Brillar mucho no es sospechoso por sí solo: alguien con la piel clara en
    # una habitación a oscuras también brilla. Lo sospechoso es brillar mucho
    # SIN seguir al destello, que es lo que hace una superficie que fabrica su
    # propia luz.
    terms = {
        "screen_specular": _ramp(specular, 0.002, config.specular_reference),
        "screen_moire": _ramp(moire_index, 0.02, config.moire_index_reference),
        "screen_banding": _ramp(banding_index, 0.02, config.banding_index_reference),
    }

    # "Brilla y es sordo" sólo existe si se pudo oír.
    #
    # Este término multiplica el brillo por lo que le FALTA a la correlación,
    # así que una correlación que no llegó lo dispara sola. Con la correlación
    # invalidada eso convertía `attack_no_color_response` en
    # `attack_emissive_surface`: la misma acusación con otro nombre.
    #
    # Medido: una cara REAL de webcam dio `screen_absence` de 0,2194 y 0,2198
    # en dos destellos seguidos —contra un suelo de 0,35— mientras la escena
    # sintética de pantalla daba 0,215. Indistinguibles. Que la sesión acabara
    # en reintentar y no en acusación fue suerte: el segundo destello salió no
    # medible por otro motivo.
    #
    # Lo que queda —reflejo especular, moiré y bandeo— son huellas propias de
    # una pantalla y no dependen de que la piel siguiera a los colores.
    if correlation is not None:
        terms["screen_bright_and_deaf"] = _ramp(
            luminance_ratio, 1.0, config.luminance_ratio_reference
        ) * _ramp(
            config.correlation_reference - correlation, 0.0, config.correlation_reference
        )
    # El máximo manda —basta una huella clara para sospechar— pero la media
    # aporta: varias pistas flojas a la vez también dicen algo.
    suspicion = float(
        np.clip(max(terms.values()) * 0.7 + float(np.mean(list(terms.values()))) * 0.3, 0.0, 1.0)
    )

    raw = {
        "screen_suspicion": suspicion,
        "face_bg_luminance_ratio": luminance_ratio,
        "face_specular_fraction": specular,
        "face_moire_index": moire_index,
        "face_banding_index": banding_index,
        **terms,
    }
    return suspicion, raw


def _ramp(value: float, low: float, high: float) -> float:
    """Rampa lineal de 0 en `low` a 1 en `high`."""
    if high <= low:
        return 0.0
    return float(np.clip((value - low) / (high - low), 0.0, 1.0))


def _combine(submetrics: dict[str, float | None], weights: dict[str, float]) -> float:
    """Media ponderada de lo que se pudo medir."""
    total = weight_sum = 0.0
    for name, value in submetrics.items():
        if value is None:
            continue
        weight = weights.get(name, 0.0)
        total += weight * value
        weight_sum += weight
    if weight_sum <= 0.0:
        return 0.0
    return float(np.clip(total / weight_sum, 0.0, 1.0))
