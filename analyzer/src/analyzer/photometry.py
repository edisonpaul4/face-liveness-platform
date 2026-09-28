"""Fotometría por frame: respuesta de cada región y pistas de superficie emisiva.

Todo lo que sale de aquí es DIFERENCIAL: la respuesta de cada región facial
dividida por la del fondo en el MISMO frame. Esa división es lo que cancela el
auto-exposición y el balance de blancos de la webcam, que aplican una ganancia
global y, si se midiera en absoluto, compensarían el destello hasta borrarlo.
"""

from __future__ import annotations

from dataclasses import dataclass

import cv2
import numpy as np

from analyzer import quality, rois

#: Evita dividir por cero en zonas muy oscuras.
EPS = 1e-3

#: Lado al que se reduce el recorte facial para los análisis de frecuencia.
#: Suficiente para ver moiré y bandeo, y barato.
SPECTRUM_SIZE = 128

#: Bandas horizontales en que se divide el rostro para el perfil por filas.
#:
#: Es lo que permite ver el obturador rodante: un sensor de webcam no captura
#: el frame de golpe, lo barre fila por fila durante los ~33 ms que dura. Si la
#: pantalla cambia de color a mitad de barrido, las filas de arriba quedan
#: expuestas con el color viejo y las de abajo con el nuevo.
#:
#: Dieciséis bandas localizan esa frontera dentro de un 6 % de la altura del
#: rostro, que sobra: a 1/30 s la transición no es un escalón limpio sino un
#: degradado, y lo que se mide es la rampa.
ROW_BANDS = 16


@dataclass(frozen=True, slots=True)
class FramePhotometry:
    """Lo medido en un frame."""

    #: Medias BGR de cada región facial, en el orden de rois.FACE_REGIONS.
    regions: np.ndarray
    #: Media BGR del fondo.
    background: np.ndarray
    #: Luminancia media del rostro, 0-255.
    face_luminance: float
    #: Fracción de píxeles casi quemados dentro del rostro. Una pantalla deja
    #: un reflejo concentrado; la luz difusa, no.
    specular_ratio: float

    # Las tres siguientes son ESPACIALMENTE DIFERENCIALES: rostro partido por
    # fondo, en el mismo frame.
    #
    # Tienen que serlo. Si un replay está delante durante la calibración —y lo
    # está, porque la calibración es parte de la misma sesión—, su moiré y su
    # bandeo ya están en la línea base y compararse contra ella no delataría
    # nada. Lo que sí delata es que el rostro los tenga y la habitación no.

    #: Luminancia del rostro frente a la del fondo. Una pantalla fabrica su
    #: propia luz y brilla mucho más que la habitación.
    luminance_ratio: float
    #: Exceso de moiré del rostro sobre el fondo, normalizado. La rejilla de
    #: píxeles del panel deja un patrón periódico que la pared no tiene.
    moire_index: float
    #: Exceso de bandeo del rostro sobre el fondo, normalizado: desfase entre
    #: el refresco del panel y la exposición de la cámara.
    banding_index: float

    #: Fracción de píxeles quemados dentro del ROSTRO, al umbral de calidad.
    #:
    #: Se mide aquí y no sobre el encuadre entero porque una ventana detrás
    #: del sujeto quema medio frame sin afectar a su cara. Medido con una
    #: cámara real: 10,4 % del encuadre quemado y 0,53 % del rostro, veinte
    #: veces menos. Con la medida del encuadre, una sesión con TODOS los retos
    #: superados se resolvía en reintentar por calidad.
    #:
    #: Con valor por defecto para no romper los constructores de prueba que
    #: sólo se ocupan de la fotometría del destello.
    face_highlight: float = 0.0
    #: Media BGR de cada banda horizontal del rostro, (ROW_BANDS, 3).
    #:
    #: Sirve para una sola cosa: ver si el color cambió a mitad de barrido del
    #: sensor. Se guarda en crudo; dividirlo por el fondo es cosa de quien lo
    #: analice, que es donde se sabe contra qué comparar.
    row_profile: np.ndarray | None = None

    def region_ratio(self) -> np.ndarray:
        """Respuesta diferencial: cada región partida por el fondo.

        Es la medida base de todo lo demás. Una ganancia global de la cámara
        multiplica numerador y denominador por igual y desaparece.
        """
        return (self.regions + EPS) / (self.background + EPS)


def measure(image: np.ndarray, landmarks: np.ndarray | None) -> FramePhotometry | None:
    """Mide las regiones y las pistas de superficie emisiva de un frame."""
    if landmarks is None or len(landmarks) == 0:
        return None

    height, width = image.shape[:2]
    patches = rois.face_patches(landmarks, width, height)
    if len(patches) < len(rois.FACE_REGIONS):
        return None

    background_boxes = rois.background_patches(landmarks, width, height)
    background = rois.background_mean(image, background_boxes)
    if background is None:
        return None

    means = []
    for name in rois.FACE_REGIONS:
        mean = patches[name].mean_bgr(image)
        if mean is None:
            return None
        means.append(mean)

    face_crop = _face_crop(image, landmarks)
    face_lum, specular, face_moire, face_banding = _surface_cues(face_crop)
    face_highlight = _burnt_fraction(face_crop)
    rows = _row_profile(face_crop)

    background_crop = _background_crop(image, background_boxes)
    if background_crop is None:
        bg_lum, bg_moire, bg_banding = face_lum, face_moire, face_banding
    else:
        bg_lum, _, bg_moire, bg_banding = _surface_cues(background_crop)

    # Exceso del rostro sobre el fondo, normalizado por la luminancia del
    # fondo. Así la ganancia de la cámara se cancela —multiplica las tres
    # cosas por igual— y el índice no depende de lo oscura que sea la piel.
    reference = max(bg_lum, 1.0)

    return FramePhotometry(
        regions=np.array(means, dtype=np.float64),
        background=background,
        face_luminance=face_lum,
        specular_ratio=specular,
        face_highlight=face_highlight,
        luminance_ratio=face_lum / max(bg_lum, 1e-3),
        moire_index=max(0.0, face_moire - bg_moire) / reference,
        banding_index=max(0.0, face_banding - bg_banding) / reference,
        row_profile=rows,
    )


def _row_profile(face_crop: np.ndarray) -> np.ndarray | None:
    """Media BGR de cada banda horizontal del recorte facial.

    El recorte ya trae sustituido lo de fuera del contorno por la media de
    dentro, así que las bandas no mezclan piel con fondo — que es justo lo que
    arruinaría la medida, porque el fondo NO recibe el destello.
    """
    height = face_crop.shape[0]
    if height < ROW_BANDS * 2:
        return None
    edges = np.linspace(0, height, ROW_BANDS + 1).astype(int)
    bands = [
        face_crop[a:b].reshape(-1, 3).mean(axis=0)
        for a, b in zip(edges, edges[1:], strict=False)
    ]
    return np.asarray(bands, dtype=np.float64)


def _background_crop(image: np.ndarray, patches: list[rois.Patch]) -> np.ndarray | None:
    """El mayor rectángulo de fondo, para medirle las mismas pistas."""
    usable = [p for p in patches if p.area > 0]
    if not usable:
        return None
    patch = max(usable, key=lambda p: p.area)
    crop = image[patch.y0 : patch.y1, patch.x0 : patch.x1]
    return crop if crop.size else None


def _face_crop(image: np.ndarray, landmarks: np.ndarray) -> np.ndarray:
    """Recorte del rostro, con lo de fuera del contorno sustituido.

    La caja que encierra los landmarks incluye las esquinas, que son fondo. Si
    se dejaran, contarían como parte del rostro, y eso trajo un sesgo real: con
    piel muy oscura la cámara sube tanto la ganancia que ese fondo se quema, y
    los píxeles quemados se leían como el reflejo concentrado de una pantalla.
    Resultado: sospechar de un replay por tener la piel oscura.

    Los píxeles de fuera del contorno se sustituyen por la media de dentro —no
    se ponen a cero— para no crear un borde artificial que ensuciaría el
    análisis de frecuencias.
    """
    height, width = image.shape[:2]
    xs, ys = landmarks[:, 0] * width, landmarks[:, 1] * height
    x0, x1 = max(0, int(xs.min())), min(width, int(xs.max()))
    y0, y1 = max(0, int(ys.min())), min(height, int(ys.max()))
    if x1 - x0 < 8 or y1 - y0 < 8:
        return image

    crop = image[y0:y1, x0:x1]

    points = np.stack([xs - x0, ys - y0], axis=1).astype(np.int32)
    mask = np.zeros(crop.shape[:2], np.uint8)
    cv2.fillConvexPoly(mask, cv2.convexHull(points), 1)
    if int(mask.sum()) < 16:
        return crop

    inside = mask.astype(bool)
    filled = crop.copy()
    filled[~inside] = crop[inside].mean(axis=0).astype(crop.dtype)
    return filled


def _surface_cues(crop: np.ndarray) -> tuple[float, float, float, float]:
    """Luminancia, reflejo concentrado, moiré y bandeo de un recorte.

    Moiré y bandeo se devuelven como AMPLITUD en cuentas de gris, no como
    relación pico/mediana del espectro. La razón importa: en un rostro de piel
    oscura el recorte es oscuro y de poco contraste, su espectro es casi todo
    ruido, y una razón pico/mediana se dispara. Medido: 24,7 frente a 5,4 en
    piel clara, con el mismo sujeto real. Eso haría sospechar de pantalla a
    quien tiene la piel oscura, que es exactamente el defecto que este módulo
    no puede tener.

    Una amplitud en cuentas no se dispara: si no hay modulación periódica, no
    hay amplitud, sea el recorte claro u oscuro.
    """
    gray = cv2.cvtColor(crop, cv2.COLOR_BGR2GRAY) if crop.ndim == 3 else crop
    luminance = float(gray.mean())

    # Reflejo especular: no cuánta luz hay, sino cuánta se concentra en unos
    # pocos píxeles. La luz difusa sube la media; un reflejo sube la cola.
    saturated = float(np.count_nonzero(gray >= 245)) / max(1, gray.size)

    small = cv2.resize(gray, (SPECTRUM_SIZE, SPECTRUM_SIZE), interpolation=cv2.INTER_AREA)
    small = small.astype(np.float32) - float(small.mean())

    return luminance, saturated, _moire_amplitude(small), _banding_amplitude(small)


def _moire_amplitude(small: np.ndarray) -> float:
    """Amplitud de la componente periódica 2D más fuerte, en cuentas.

    Una textura natural es de banda ancha y no concentra energía en ninguna
    frecuencia. La rejilla de píxeles de un panel, muestreada por la rejilla
    del sensor, deja una componente periódica clara.
    """
    window = np.outer(np.hanning(small.shape[0]), np.hanning(small.shape[1]))
    spectrum = np.abs(np.fft.fftshift(np.fft.fft2(small * window)))

    size = small.shape[0]
    center = size // 2
    yy, xx = np.ogrid[:size, :size]
    radius = np.sqrt((yy - center) ** 2 + (xx - center) ** 2)

    # Anillo de frecuencias medias-altas: donde cae el aliasing de un panel y
    # donde la piel aporta poco.
    ring = (radius > size * 0.12) & (radius < size * 0.45)
    values = spectrum[ring]
    if values.size == 0:
        return 0.0

    # La ventana de Hanning se lleva algo más de la mitad de la energía.
    return float(values.max()) * 4.0 / (size * size) / 0.25


def _banding_amplitude(small: np.ndarray) -> float:
    """Amplitud de las franjas horizontales, en cuentas.

    El desfase entre el refresco del panel y la exposición de la cámara deja
    bandas que recorren la imagen. Se miran sólo periodos de entre 4 y 40
    filas: por debajo es ruido y por encima es la propia estructura del
    rostro (pelo, ojos, boca), que no es bandeo.
    """
    rows = small.mean(axis=1)
    rows = rows - rows.mean()
    if rows.size < 8:
        return 0.0

    spectrum = np.abs(np.fft.rfft(rows))
    low = max(1, rows.size // 40)
    high = min(len(spectrum), rows.size // 4)
    if high <= low:
        return 0.0

    return float(spectrum[low:high].max()) * 2.0 / rows.size


def _burnt_fraction(crop: np.ndarray) -> float:
    """Fracción de píxeles quemados de un recorte, al umbral de calidad."""
    gray = cv2.cvtColor(crop, cv2.COLOR_BGR2GRAY) if crop.ndim == 3 else crop
    if gray.size == 0:
        return 0.0
    return float(np.count_nonzero(gray >= quality.HIGHLIGHT_THRESHOLD)) / gray.size
