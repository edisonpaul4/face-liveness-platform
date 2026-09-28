"""Banco de pruebas sintético para el analizador de destello.

Genera las tres escenas del modelo de amenaza ante la misma secuencia de
colores, **con una cámara que se comporta como una webcam de verdad**: ajusta
la exposición y el balance de blancos a cada frame, entrega con retardo y añade
ruido de sensor.

Esa cámara no es un adorno. El auto-exposición es el problema número uno de
esta técnica: cuando la pantalla destella, la webcam baja la ganancia y el
brillo del rostro apenas cambia en términos absolutos. Si el banco no lo
simulara, la medición diferencial no estaría probada.

Física de cada escena, que es la verdad del terreno contra la que se mide:

* **Rostro real** — cada región recibe luz distinta según su distancia y
  orientación respecto a la pantalla: la nariz sobresale y recibe más que los
  pómulos. El fondo está mucho más lejos y apenas recibe.
* **Foto impresa** — superficie plana: todas las regiones reciben lo mismo.
  Responde al destello, pero sin gradiente.
* **Replay en pantalla** — fabrica su propia luz: mucha luminancia, apenas
  modulación con el destello, más reflejo concentrado en el cristal, moiré de
  su rejilla y bandeo por desfase de refresco.

Los valores de irradiancia y albedo son un modelo, no medidas de laboratorio.
Lo que el banco valida es que la CADENA DE MEDIDA recupere una verdad conocida
a pesar de la cámara, el retardo y el ruido. La calibración con material real
es cosa de /bench.
"""

from __future__ import annotations

from dataclasses import dataclass, field

import cv2
import numpy as np

from analyzer import rois
from tests.render import render_head_3d  # noqa: TID252


@dataclass(frozen=True, slots=True)
class SkinTone:
    """Tono de piel, como reflectancia BGR."""

    name: str
    albedo: tuple[float, float, float]


#: Escala de tonos. El sistema tiene que funcionar igual con todos; si no,
#: no es un problema de precisión, es un producto defectuoso.
SKIN_TONES: tuple[SkinTone, ...] = (
    SkinTone("muy_clara", (0.78, 0.80, 0.82)),
    SkinTone("clara", (0.58, 0.62, 0.68)),
    SkinTone("media", (0.36, 0.42, 0.50)),
    SkinTone("oscura", (0.17, 0.20, 0.26)),
    SkinTone("muy_oscura", (0.09, 0.11, 0.14)),
)

#: Irradiancia relativa que recibe cada región desde la pantalla, en un rostro
#: con volumen. La nariz está más cerca y más de frente.
REAL_IRRADIANCE: dict[str, float] = {
    "forehead": 0.72,
    "nose": 1.00,
    "cheek_left": 0.55,
    "cheek_right": 0.58,
}
#: En una superficie plana, todas las regiones reciben lo mismo.
FLAT_IRRADIANCE: dict[str, float] = dict.fromkeys(REAL_IRRADIANCE, 0.78)

#: El fondo está mucho más lejos de la pantalla: le llega poco.
BACKGROUND_IRRADIANCE = 0.10


@dataclass(frozen=True, slots=True)
class Camera:
    """Webcam con automatismos.

    `exposure_strength` y `white_balance_strength` son cuánto compensa: a 1.0
    la cámara cancela por completo el cambio de luz, que es el peor caso para
    quien mida en absoluto.
    """

    target_mean: float = 118.0
    exposure_strength: float = 0.85
    white_balance_strength: float = 0.6
    noise_sigma: float = 1.5
    seed: int = 20260824

    def capture(self, radiance: np.ndarray, frame_index: int) -> np.ndarray:
        """Convierte irradiancia en un frame de 8 bits, con automatismos.

        La medición es PONDERADA AL CENTRO, que es como miden las webcams, y
        la corrección se aplica a todo el frame. Ahí está el problema: el
        centro es la cara, así que cuando el destello la ilumina, la cámara
        baja la ganancia de TODO y el brillo absoluto del rostro apenas se
        mueve. Quien mida en absoluto pierde la señal; quien mida contra el
        fondo, no, porque la corrección afecta a los dos por igual.
        """
        metered = self._metered_region(radiance)

        mean = float(metered.mean())
        gain = 1.0
        if mean > 1e-6:
            gain = 1.0 + self.exposure_strength * (self.target_mean / mean - 1.0)
        exposed = radiance * gain

        channel_means = self._metered_region(exposed).reshape(-1, 3).mean(axis=0)
        overall = float(channel_means.mean())
        if overall > 1e-6:
            balance = 1.0 + self.white_balance_strength * (
                overall / np.maximum(channel_means, 1e-6) - 1.0
            )
            exposed = exposed * balance

        rng = np.random.default_rng(self.seed + frame_index)
        noisy = exposed + rng.normal(0.0, self.noise_sigma, exposed.shape)
        return np.clip(noisy, 0, 255).astype(np.uint8)

    def _metered_region(self, frame: np.ndarray) -> np.ndarray:
        """Zona central que la cámara usa para medir: donde está la cara."""
        height, width = frame.shape[:2]
        y0, y1 = int(height * 0.25), int(height * 0.75)
        x0, x1 = int(width * 0.30), int(width * 0.70)
        return frame[y0:y1, x0:x1]


@dataclass(frozen=True, slots=True)
class Scene:
    """Qué hay delante de la cámara."""

    kind: str  # "real" | "photo" | "screen"
    tone: SkinTone = SKIN_TONES[2]
    #: Luz de la habitación. Subirla aplasta el destello: es el caso de
    #: calidad insuficiente.
    ambient: float = 150.0
    #: Cuánta luz mete la pantalla al destellar.
    flash_intensity: float = 130.0
    #: Retardo entre pintar el color y verlo, en milisegundos.
    lag_ms: int = 180
    #: Micro-movimiento del sujeto, en píxeles. Nadie se queda quieto del
    #: todo, y ese temblor es lo que de verdad fija el suelo de ruido: hace
    #: que cada frame muestree piel ligeramente distinta. Sin él, promediar
    #: parches de mil píxeles daría un ruido irrealmente bajo.
    micro_motion_px: float = 1.2
    camera: Camera = field(default_factory=Camera)


@dataclass(frozen=True, slots=True)
class _Geometry:
    """Máscaras espaciales del rostro, calculadas una vez."""

    base: np.ndarray
    face_mask: np.ndarray
    region_fields: dict[str, np.ndarray]
    landmarks: np.ndarray


def build_geometry(detector) -> _Geometry:  # noqa: ANN001
    """Prepara la geometría del sujeto a partir de un rostro renderizado."""
    base = render_head_3d(0.0)
    detection = detector.detect(base)
    if not detection.faces or detection.landmarks is None:
        raise RuntimeError("el renderizador base no produjo un rostro detectable")

    landmarks = detection.landmarks
    height, width = base.shape[:2]

    points = np.stack([landmarks[:, 0] * width, landmarks[:, 1] * height], axis=1).astype(np.int32)
    hull = cv2.convexHull(points)
    face_mask = np.zeros((height, width), np.float32)
    cv2.fillConvexPoly(face_mask, hull, 1.0)
    face_mask = cv2.GaussianBlur(face_mask, (31, 31), 0)

    # Campo de influencia de cada región: una campana centrada en su ancla.
    # Ancho generoso para que la irradiancia varíe de forma suave, como la luz.
    face_width = float(points[:, 0].max() - points[:, 0].min())
    sigma = max(8.0, face_width * 0.13)
    yy, xx = np.mgrid[0:height, 0:width].astype(np.float32)

    region_fields = {}
    for name, index in rois.ANCHORS.items():
        cx, cy = landmarks[index][0] * width, landmarks[index][1] * height
        region_fields[name] = np.exp(-((xx - cx) ** 2 + (yy - cy) ** 2) / (2 * sigma**2))

    return _Geometry(base=base, face_mask=face_mask, region_fields=region_fields,
                     landmarks=landmarks)


def _irradiance_map(geometry: _Geometry, weights: dict[str, float]) -> np.ndarray:
    """Mapa de cuánta luz de pantalla recibe cada píxel."""
    total = np.zeros_like(geometry.face_mask)
    norm = np.zeros_like(geometry.face_mask)
    for name, field_map in geometry.region_fields.items():
        total += field_map * weights[name]
        norm += field_map

    face = np.divide(total, np.maximum(norm, 1e-6))
    return face * geometry.face_mask + BACKGROUND_IRRADIANCE * (1.0 - geometry.face_mask)


def _albedo_map(geometry: _Geometry, tone: SkinTone) -> np.ndarray:
    """Reflectancia por píxel: piel dentro del rostro, fondo fuera."""
    mask = geometry.face_mask[..., None]
    skin = np.array(tone.albedo, dtype=np.float32)
    background = np.array((0.55, 0.55, 0.55), dtype=np.float32)
    return skin * mask + background * (1.0 - mask)


def _screen_artifacts(geometry: _Geometry, frame_index: int) -> tuple[np.ndarray, np.ndarray]:
    """Moiré y bandeo de un panel visto por una cámara."""
    height, width = geometry.face_mask.shape
    yy, xx = np.mgrid[0:height, 0:width].astype(np.float32)

    # Rejilla de píxeles muestreada por otra rejilla: patrón periódico fino.
    moire = 9.0 * np.sin(2 * np.pi * (xx * 0.31 + yy * 0.11))
    # Desfase entre refresco y exposición: franjas que se desplazan.
    banding = 7.0 * np.sin(2 * np.pi * (yy / 46.0 + frame_index * 0.17))
    return moire * geometry.face_mask, banding * geometry.face_mask


def render_frame(scene: Scene, geometry: _Geometry, color: np.ndarray | None,
                 frame_index: int) -> np.ndarray:
    """Compone un frame para el color emitido que corresponda.

    `color` es el BGR normalizado que la pantalla está emitiendo tal y como se
    ve en ESTE frame (el retardo ya está aplicado por el llamante). None si no
    hay destello.
    """
    emitted = np.zeros(3, dtype=np.float32) if color is None else color.astype(np.float32)

    if scene.kind == "screen":
        # Una pantalla fabrica su propia luz: el rostro no depende del
        # destello, sólo un reflejo pequeño en el cristal.
        weights = dict.fromkeys(REAL_IRRADIANCE, 0.06)
        irradiance = _irradiance_map(geometry, weights)
        emissive = 165.0 * geometry.face_mask[..., None]
        radiance = (
            emissive
            + _albedo_map(geometry, scene.tone)
            * (scene.ambient + irradiance[..., None] * scene.flash_intensity * emitted)
        )
        moire, banding = _screen_artifacts(geometry, frame_index)
        radiance = radiance + (moire + banding)[..., None]
    else:
        weights = REAL_IRRADIANCE if scene.kind == "real" else FLAT_IRRADIANCE
        irradiance = _irradiance_map(geometry, weights)
        radiance = _albedo_map(geometry, scene.tone) * (
            scene.ambient + irradiance[..., None] * scene.flash_intensity * emitted
        )

    # La textura del rostro base modula la radiancia, para que el detector
    # siga viendo una cara y no una mancha.
    texture = geometry.base.astype(np.float32) / 190.0
    composed = np.clip(radiance * texture, 0, None)

    if scene.micro_motion_px > 0:
        rng = np.random.default_rng(scene.camera.seed * 7919 + frame_index)
        dx, dy = rng.normal(0.0, scene.micro_motion_px, 2)
        shift = np.float32([[1, 0, dx], [0, 1, dy]])
        composed = cv2.warpAffine(
            composed, shift, (composed.shape[1], composed.shape[0]),
            flags=cv2.INTER_LINEAR, borderMode=cv2.BORDER_REPLICATE,
        )

    return scene.camera.capture(composed, frame_index)


def render_session(scene: Scene, geometry: _Geometry, sequence, *, fps: float = 15.0,
                   calibration_ms: int = 1200, start_us: int = 1_000_000):
    """Genera calibración (pantalla neutra) y destello.

    Returns:
        (frames_calibracion, frames_destello, inicio_destello_us), donde cada
        frame es (instante_us, imagen).
    """
    interval_us = int(1_000_000 / fps)
    lag_us = scene.lag_ms * 1000

    calibration = []
    at = start_us
    index = 0
    while at < start_us + calibration_ms * 1000:
        calibration.append((at, render_frame(scene, geometry, None, index)))
        at += interval_us
        index += 1

    flash_start = at
    total_ms = sum(segment.duration_ms for segment in sequence)
    # Se graba más allá del final para que el retardo quepa dentro.
    end = flash_start + (total_ms + scene.lag_ms + 400) * 1000

    flash = []
    while at < end:
        elapsed_ms = (at - lag_us - flash_start) / 1000.0
        color = None
        if elapsed_ms >= 0:
            accumulated = 0.0
            for segment in sequence:
                accumulated += segment.duration_ms
                if elapsed_ms < accumulated:
                    color = np.array(_PALETTE[segment.color], dtype=np.float32)
                    break
        flash.append((at, render_frame(scene, geometry, color, index)))
        at += interval_us
        index += 1

    return calibration, flash, flash_start


_PALETTE = {
    "white": (1.0, 1.0, 1.0),
    "blue": (1.0, 0.0, 0.0),
    "green": (0.0, 1.0, 0.0),
    "red": (0.0, 0.0, 1.0),
}
