"""Pulso sanguíneo a distancia (rPPG) a partir del color de la piel.

Mide algo que ninguna superficie tiene: **latido**. Cada sístole empuja sangre
a los capilares del rostro y cambia su absorción de luz verde unas décimas de
punto. Es invisible al ojo y lo capta una webcam corriente.

Por qué importa aquí: es la única señal de este repositorio que una máscara no
puede fingir. Una foto y una pantalla las delatan el moiré, el bandeo y el
paralaje; una máscara de silicona bien hecha tiene volumen, no hace moiré y se
mueve con la cabeza —medido en /bench, se cuela en el 55-73 % de los casos—.
Lo que no tiene es corazón.

Método: POS (Plane-Orthogonal-to-Skin, Wang et al. 2017). Se eligió sobre el
canal verde a secas y sobre CHROM porque es el que mejor aguanta el
movimiento, y aquí el sujeto se mueve: gira la cabeza cuando se le pide.

Sólo mide. Que un SNR bajo signifique máscara, mala luz o que el sujeto se
movió demasiado lo decide Go (CLAUDE.md §3): aquí no se emite ningún juicio.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from analyzer.photometry import FramePhotometry

#: Banda fisiológica del pulso humano, en Hz. 42 a 240 latidos por minuto.
#:
#: Generosa por los extremos a propósito: un atleta en reposo baja de 50 y
#: alguien nervioso ante una verificación de identidad pasa de 100. Recortarla
#: a lo "normal" convertiría una taquicardia en un rechazo.
MIN_HZ = 0.7
MAX_HZ = 4.0

#: Ventana de POS, en segundos. La del artículo original.
POS_WINDOW_S = 1.6

#: Muestras mínimas para intentarlo. Por debajo de esto la resolución en
#: frecuencia no distingue un pulso de un armónico del movimiento.
MIN_SAMPLES = 64

#: Segundos mínimos de señal continua.
#:
#: 10 s es el suelo de VALIDEZ, no el de utilidad, y la diferencia importa.
#:
#: Medido (150 series sintéticas por punto), el error en la frecuencia estimada
#: cae en picado justo aquí:
#:
#:     ventana    4s     6s     8s    10s    12s    20s
#:     error    51 lpm 31 lpm 21 lpm  3 lpm  4 lpm  0 lpm
#:
#: Por debajo de 10 s lo que sale no es un pulso mal medido: es un pico del
#: ruido. La resolución de una FFT es 1/T, y con 8 s los bins miden 7,5 lpm.
#:
#: Lo que 10 s NO garantizan es discriminar. Con una modulación fuerte (~0,9 %)
#: la separación entre rostro y superficie es casi perfecta ya a 4 s
#: (AUC 0,955). Con una modulación DÉBIL —0,25 %, que es la realista con una
#: webcam— hace falta mucho más:
#:
#:     ventana   10s    15s    20s    30s
#:     AUC      0,62   0,68   0,75   0,80
#:
#: O sea: 10 s bastan para que el número signifique algo; para que esta señal
#: pese de verdad contra una máscara hacen falta 20-30 s de tramo tranquilo.
#: Eso es una decisión de producto sobre cuánto puede durar una sesión.
#:
#: Cuando no llega, se dice. Un SNR bajo aquí significa "la sesión no me dio
#: señal suficiente", no "esta cara no tiene pulso", y puntuarlo rechazaría a
#: personas vivas.
MIN_SECONDS = 10.0

#: Separación máxima entre frames consecutivos, en segundos. Un hueco mayor
#: parte la serie: interpolar sobre un segundo perdido es inventar latidos.
MAX_GAP_S = 0.5


#: Regiones faciales que se promedian para el pulso.
#:
#: La nariz queda FUERA. Está peor irrigada que frente y mejillas, y encima es
#: donde más pega el reflejo especular y donde más se nota un giro de cabeza.
#: La literatura la señala como la peor región facial para rPPG; medido sobre
#: escena sintética, quitarla gana en torno a 0,9 dB.
PULSE_REGIONS = ("forehead", "cheek_left", "cheek_right")

#: Luminancia mínima del fondo, 0-255, para que la medida diferencial valga.
#:
#: El pulso se mide como rostro PARTIDO POR fondo, y con el fondo a oscuras el
#: divisor es ruido: la división amplifica ese ruido en vez de cancelar la
#: deriva de la cámara.
MIN_BACKGROUND_LUMA = 12.0


def skin_series(photometry: FramePhotometry) -> np.ndarray | None:
    """Serie de color de la piel de un frame, medida CONTRA EL FONDO.

    Éste era el fallo. El pulso se medía sobre la media cruda de las regiones
    —valor absoluto— y era el único camino del analizador que no usaba
    `region_ratio()`, cuyo propio docstring dice que es «la medida base de
    todo lo demás».

    Por qué importa, y no es lo que parece: POS normaliza cada ventana en el
    tiempo, así que ya cancela la deriva COMÚN a los tres canales, que es lo
    que hace la auto-exposición. Lo que no cancela es el **balance de
    blancos**, que mueve cada canal por su lado. Esa oscilación cromática cae
    dentro de la banda del pulso y se lee como latido.

    Medido con este mismo POS sobre escena sintética, AUC de vivo contra
    superficie plana:

        deriva de balance de blancos   absoluto   diferencial
                                 0 %      0,713         0,617
                               0,5 %      0,415         0,625
                                 2 %      0,424         0,625

    Por debajo de 0,5 significa que la máscara puntúa MÁS que el rostro vivo.
    El diferencial cuesta algo con la cámara perfectamente quieta —el fondo
    aporta su propio ruido— y a cambio no se desploma, que es lo que hace una
    webcam de verdad con el balance automático puesto.

    Es la tercera vez que este repositorio tropieza con lo mismo: el destello
    ya lo aprendió y la puerta de quemados también. CLAUDE.md lo dejó escrito:
    «que dos partes del sistema tropiecen con la misma piedra sugiere mirar
    con lupa cualquier medida que promedie el encuadre entero».
    """
    from analyzer import rois

    if float(np.mean(photometry.background)) < MIN_BACKGROUND_LUMA:
        return None

    ratio = photometry.region_ratio()
    keep = [i for i, name in enumerate(rois.FACE_REGIONS) if name in PULSE_REGIONS]
    if not keep:
        return None
    return np.asarray(ratio[keep].mean(axis=0), dtype=np.float64)


@dataclass(frozen=True, slots=True)
class Pulse:
    """Lo que se pudo medir del pulso."""

    #: Relación señal/ruido en decibelios: potencia en torno al pico y su
    #: primer armónico, contra el resto de la banda fisiológica.
    #:
    #: Es la magnitud que discrimina. Un rostro vivo concentra energía en una
    #: frecuencia; una máscara reparte ruido por toda la banda.
    snr_db: float
    #: Frecuencia dominante, en latidos por minuto. Informativa: el sistema no
    #: decide por el ritmo, sino por si HAY ritmo.
    bpm: float
    #: Segundos de señal utilizable.
    seconds: float
    #: Muestras usadas tras remuestrear.
    samples: int


def measure(
    timestamps_us: list[int], channels: list[np.ndarray], fps: float | None = None
) -> Pulse | None:
    """Mide el pulso de una serie de medias de color del rostro.

    `channels` son medias BGR por frame, en el orden de `timestamps_us`.

    `fps` es la rejilla de remuestreo. Por omisión se deduce de los propios
    sellos, que es mejor dato que la cadencia nominal: los frames llegan
    cuando llegan, y remuestrear a 15 Hz una serie que de verdad va a 12
    desplaza el pico.

    Devuelve None cuando no hay con qué medir: pocas muestras, huecos grandes
    o señal plana. No medir es una respuesta legítima y no cuenta ni a favor
    ni en contra (CLAUDE.md §4).
    """
    if len(timestamps_us) < MIN_SAMPLES or len(timestamps_us) != len(channels):
        return None

    times = np.asarray(timestamps_us, dtype=np.float64) / 1e6
    rgb = np.asarray(channels, dtype=np.float64)
    if rgb.ndim != 2 or rgb.shape[1] != 3:
        return None

    # De BGR a RGB: POS está definido sobre RGB y el orden importa, porque el
    # plano de proyección no es simétrico.
    rgb = rgb[:, ::-1]

    if fps is None:
        step = float(np.median(np.diff(times)))
        if not np.isfinite(step) or step <= 0:
            return None
        fps = 1.0 / step

    segment = _longest_run(times)
    if segment is None:
        return None
    start, end = segment
    times, rgb = times[start:end], rgb[start:end]
    if len(times) < MIN_SAMPLES:
        return None

    # Remuestreo a rejilla uniforme: los frames no llegan a intervalos
    # exactos, y una FFT sobre muestras desiguales reparte la energía del pico
    # por toda la banda — justo lo que se está midiendo.
    uniform_t = np.arange(times[0], times[-1], 1.0 / fps)
    if len(uniform_t) < MIN_SAMPLES:
        return None
    resampled = np.column_stack([np.interp(uniform_t, times, rgb[:, c]) for c in range(3)])

    signal = _pos(resampled, fps)
    if signal is None:
        return None

    if uniform_t[-1] - uniform_t[0] < MIN_SECONDS:
        return None

    snr_db, bpm = _band_snr(signal, fps)
    if snr_db is None:
        return None

    return Pulse(
        snr_db=float(snr_db),
        bpm=float(bpm),
        seconds=float(uniform_t[-1] - uniform_t[0]),
        samples=int(len(uniform_t)),
    )


def _longest_run(times: np.ndarray) -> tuple[int, int] | None:
    """Tramo más largo sin huecos. Un hueco parte la serie, no se rellena."""
    if len(times) < 2:
        return None
    gaps = np.diff(times)
    breaks = [0, *(np.flatnonzero(gaps > MAX_GAP_S) + 1).tolist(), len(times)]

    best, best_len = None, 0
    for i in range(len(breaks) - 1):
        start, end = breaks[i], breaks[i + 1]
        if end - start > best_len:
            best, best_len = (start, end), end - start
    return best


def _pos(rgb: np.ndarray, fps: float) -> np.ndarray | None:
    """POS: proyecta el color sobre el plano ortogonal al tono de la piel.

    La idea: el tono de piel de cada persona ocupa una dirección distinta en
    el espacio RGB, y el pulso la modula en una dirección que NO depende del
    tono. Proyectando sobre el plano ortogonal se queda la modulación y se va
    el color de base — que es lo que hace que esto funcione igual con
    cualquier piel, por el mismo motivo que la calibración del destello.
    """
    n = len(rgb)
    window = int(POS_WINDOW_S * fps)
    if window < 8 or n < window:
        return None

    output = np.zeros(n, dtype=np.float64)
    # Cuántas ventanas aportan a cada muestra. Sin esto, las muestras de los
    # extremos reciben menos aportaciones que las del centro y la serie queda
    # con una envolvente en forma de meseta. Esa envolvente ES una componente
    # de baja frecuencia, y se cuela en la banda del pulso por el borde de
    # abajo disfrazada de bradicardia. Medido: producía picos clavados en
    # 42 bpm —justo MIN_HZ— en todas las grabaciones.
    weight = np.zeros(n, dtype=np.float64)
    # Matriz de proyección del artículo.
    projection = np.array([[0.0, 1.0, -1.0], [-2.0, 1.0, 1.0]])

    for start in range(0, n - window + 1):
        chunk = rgb[start : start + window]
        mean = chunk.mean(axis=0)
        if np.any(mean <= 0):
            continue
        # Normalización temporal: quita el nivel de iluminación, deja la
        # modulación relativa.
        normalized = chunk / mean

        s = projection @ normalized.T
        std1 = s[1].std()
        alpha = (s[0].std() / std1) if std1 > 1e-12 else 0.0
        h = s[0] + alpha * s[1]

        # Suma solapada: cada muestra recibe la aportación de todas las
        # ventanas que la contienen, lo que promedia el ruido de cada una.
        std = h.std()
        if std <= 1e-12:
            continue
        # Cada ventana se estandariza antes de sumarse: si no, la que pilla
        # más luz pesa más, y lo que se busca es la periodicidad, no la
        # amplitud.
        output[start : start + window] += (h - h.mean()) / std
        weight[start : start + window] += 1.0

    covered = weight > 0
    if not np.any(covered):
        return None
    output[covered] /= weight[covered]
    output[~covered] = 0.0

    return _detrend(output)


def _detrend(signal: np.ndarray) -> np.ndarray:
    """Quita la tendencia lineal.

    Lo que queda de deriva tras la normalización sigue siendo suficiente para
    ensuciar el borde inferior de la banda: una rampa no es periódica, así que
    su energía se reparte por todo el espectro y cae sobre todo cerca de cero.
    """
    n = len(signal)
    x = np.arange(n, dtype=np.float64)
    slope, intercept = np.polyfit(x, signal, 1)
    return signal - (slope * x + intercept)


def _band_snr(signal: np.ndarray, fps: float) -> tuple[float | None, float]:
    """Relación señal/ruido dentro de la banda del pulso.

    Se compara la energía del pico y su primer armónico contra el resto de la
    banda. Un rostro vivo concentra; una superficie reparte.
    """
    n = len(signal)
    windowed = signal * np.hanning(n)
    spectrum = np.abs(np.fft.rfft(windowed)) ** 2
    freqs = np.fft.rfftfreq(n, d=1.0 / fps)

    band = (freqs >= MIN_HZ) & (freqs <= MAX_HZ)
    if not np.any(band) or spectrum[band].sum() <= 0:
        return None, 0.0

    band_freqs = freqs[band]
    band_power = spectrum[band]
    peak_index = int(np.argmax(band_power))
    peak_hz = float(band_freqs[peak_index])

    # Un máximo en el primer bin de la banda no es un pico: es el faldón de
    # algo que está por debajo —deriva de iluminación, el sujeto
    # acomodándose— asomando por el borde. Un pulso de verdad tiene bins más
    # bajos a ambos lados. Sin esta guarda, las grabaciones reales daban
    # sistemáticamente 42 lpm, que es exactamente MIN_HZ.
    if peak_index == 0:
        return None, peak_hz * 60.0

    # Anchura de la ventana del pico: lo bastante ancha para no castigar una
    # frecuencia cardiaca que varía durante la sesión, que es lo normal.
    half_width = 0.2
    mask = np.abs(band_freqs - peak_hz) <= half_width
    # Y el primer armónico, que un pulso real produce y el ruido no.
    mask |= np.abs(band_freqs - 2 * peak_hz) <= half_width

    signal_power = float(band_power[mask].sum())
    noise_power = float(band_power[~mask].sum())
    if noise_power <= 0 or signal_power <= 0:
        return None, peak_hz * 60.0

    return 10.0 * np.log10(signal_power / noise_power), peak_hz * 60.0


#: Extremos de la normalización a 0-1. **Sin calibrar**: salen de la
#: separación medida sobre señal sintética (vivo fuerte +6,7 dB, plano
#: -6,8 dB) y de los márgenes que da la literatura, no de un conjunto de
#: ataques reales, que todavía no existe para esta señal.
#:
#: Por eso `rppg_snr_db` viaja crudo en `raw`: el día que haya datos, se
#: recalibra sin tocar lo que se midió.
SNR_FLOOR_DB = -6.0
SNR_CEILING_DB = 6.0


def summary(snr_db: float) -> float:
    """Normaliza el decibelio a 0-1. No es un veredicto.

    Es la misma transformación monótona que aplican las demás ventanas para
    cumplir el contrato: comprime un rango físico a uno acotado y nada más.
    Dónde está la frontera entre vivo y máscara lo decide el perfil de
    decisión en Go, no esta función.
    """
    span = SNR_CEILING_DB - SNR_FLOOR_DB
    return float(np.clip((snr_db - SNR_FLOOR_DB) / span, 0.0, 1.0))
