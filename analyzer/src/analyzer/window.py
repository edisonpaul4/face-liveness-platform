"""Análisis de una ventana de pose.

Go delimita una ventana temporal y dice contra qué ángulo medirla; este módulo
devuelve cuatro sub-métricas y un resumen 0-1. **Aquí no se decide nada**: no
se aplica ningún umbral de aprobado, no se emite pass/fail y no se sabe qué
consecuencia tiene el resultado. Eso es de Go (CLAUDE.md §3).

Las cuatro sub-métricas:

1. **Cumplimiento** — hasta dónde llegó el ángulo respecto al objetivo.
2. **Continuidad de trayectoria** — que la pose recorra los ángulos
   intermedios. Un salto discreto delata un corte de vídeo o un frame
   sustituido.
3. **Paralaje / no rigidez** — la señal fuerte. Al girar una cabeza de verdad,
   los landmarks se desplazan unos respecto a otros: la nariz tapa, la oreja
   aparece. Una foto o una pantalla son un plano, y el movimiento de un plano
   lo explica exactamente una homografía. Se mide cuánto residuo deja el mejor
   modelo plano: **si lo explica demasiado bien, es una superficie plana**.
4. **Consistencia de identidad** — que el embedding no pegue saltos.

Una sub-métrica que no se puede medir vale `None`, no cero. La diferencia
importa: cero significa "medido y malo", y `None` significa "no había con qué
medirlo". Confundirlos convertiría a un usuario que no llegó a girar en un
atacante. Los pesos se renormalizan sobre lo que sí se midió.
"""

from __future__ import annotations

from dataclasses import dataclass, field

import cv2
import numpy as np

from analyzer.identity import cosine_similarity
from analyzer.sessions import PoseSample


@dataclass(frozen=True, slots=True)
class WindowSpec:
    """Lo que Go pide medir.

    Nótese lo que NO trae: ni identificador de reto, ni posición en el guion,
    ni umbral de aprobado, ni qué pasa si sale mal. Sólo geometría y tiempo.
    """

    window_id: str
    #: Eje a medir: yaw, pitch o roll.
    axis: str
    #: Ángulo objetivo en grados, con signo.
    target_deg: float
    #: Margen admisible alrededor del objetivo.
    tolerance_deg: float
    started_at_us: int
    ended_at_us: int


@dataclass(frozen=True, slots=True)
class ScoreConfig:
    """Parámetros del análisis.

    SIN CALIBRAR. Los números salen de medidas sobre material sintético; los
    definitivos tienen que salir de /bench con material real. Van aquí, en
    configuración, y no repartidos por el código.
    """

    #: Peso de cada sub-métrica en el resumen. El paralaje se lleva el mayor
    #: porque es el que de verdad separa un rostro de una superficie plana.
    weights: dict[str, float] = field(
        default_factory=lambda: {
            "parallax": 0.45,
            "compliance": 0.25,
            "continuity": 0.20,
            "identity": 0.10,
        }
    )

    #: Salto máximo admisible entre frames consecutivos, en grados. Por
    #: encima, discontinuidad.
    continuity_threshold_deg: float = 15.0
    #: Ritmo nominal de captura. Sirve para ajustar el umbral cuando entre dos
    #: frames analizados ha pasado más tiempo del normal: con frames
    #: descartados, un salto mayor es esperable y no es sospechoso.
    nominal_fps: float = 15.0

    #: Giro mínimo para que el paralaje sea medible. Por debajo, no hay
    #: base suficiente y la sub-métrica sale None.
    min_angle_span_deg: float = 6.0
    #: Residuo que deja el propio ruido de detección de landmarks, aunque la
    #: superficie sea perfectamente plana. Se resta antes de normalizar: sin
    #: esto, un recorrido corto infla la tasa y una foto quieta parecería
    #: tener volumen. Medido sobre foto girada: se queda en ~0.004 haga el
    #: ángulo que haga.
    parallax_noise_floor: float = 0.004
    #: Residuo por grado de giro, ya descontado el ruido, que se considera
    #: "rostro con volumen". Medido con el renderizador sintético: un rostro
    #: 3D da ~0.0011/grado y una foto girada 0.
    parallax_reference_rate: float = 0.0008

    #: Similitud entre embeddings consecutivos por debajo de la cual se
    #: considera que la cara cambió.
    identity_floor: float = 0.30
    #: Similitud a partir de la cual la identidad se considera estable.
    identity_ceiling: float = 0.80


@dataclass(frozen=True, slots=True)
class WindowScore:
    """Resultado del análisis de una ventana."""

    window_id: str
    #: Resumen 0-1 de las sub-métricas medidas.
    score: float
    #: Sub-métricas normalizadas 0-1. `None` = no se pudo medir.
    submetrics: dict[str, float | None]
    #: Magnitudes crudas, para que Go pueda decidir con sus propios umbrales.
    raw: dict[str, float]
    frames_used: int
    axis: str

    def to_dict(self) -> dict:
        return {
            "window_id": self.window_id,
            "axis": self.axis,
            "score": round(self.score, 4),
            "submetrics": {
                k: (None if v is None else round(v, 4)) for k, v in self.submetrics.items()
            },
            "raw": {k: round(v, 6) for k, v in self.raw.items()},
            "frames_used": self.frames_used,
        }


def analyze(
    spec: WindowSpec, samples: list[PoseSample], config: ScoreConfig | None = None
) -> WindowScore:
    """Mide una ventana de pose y devuelve sus sub-métricas."""
    config = config or ScoreConfig()
    window = [s for s in samples if spec.started_at_us <= s.at_us <= spec.ended_at_us]

    raw: dict[str, float] = {"frames_in_window": float(len(window))}
    submetrics: dict[str, float | None] = {
        "compliance": _compliance(spec, window, raw),
        "continuity": _continuity(spec, window, config, raw),
        "parallax": _parallax(spec, window, config, raw),
        "identity": _identity(window, config, raw),
    }

    return WindowScore(
        window_id=spec.window_id,
        axis=spec.axis,
        score=_combine(submetrics, config.weights),
        submetrics=submetrics,
        raw=raw,
        frames_used=len(window),
    )


def _angles(spec: WindowSpec, window: list[PoseSample]) -> list[float]:
    return [s.pose.angle(spec.axis) for s in window if s.pose is not None]


def _usable(window: list[PoseSample]) -> list[PoseSample]:
    """Muestras con pose y landmarks: las únicas que sirven para el paralaje."""
    return [s for s in window if s.pose is not None and s.landmarks is not None]


# --- 1. cumplimiento ---------------------------------------------------------


def _compliance(spec: WindowSpec, window: list[PoseSample], raw: dict[str, float]) -> float | None:
    """Hasta dónde llegó el ángulo respecto al objetivo.

    Se mide en la dirección del objetivo: girar al lado contrario no cuenta,
    por mucho que se gire.
    """
    angles = _angles(spec, window)
    if not angles:
        return None

    direction = 1.0 if spec.target_deg >= 0 else -1.0
    peak = max(angles) if direction > 0 else min(angles)

    raw["peak_deg"] = float(peak)
    raw["target_deg"] = float(spec.target_deg)
    raw["start_deg"] = float(angles[0])

    target = abs(spec.target_deg)
    if target < 1e-6:
        return None

    # Llegar al objetivo menos la tolerancia ya cuenta como llegar.
    reached = max(0.0, peak * direction)
    effective_target = max(1e-6, target - spec.tolerance_deg)
    return float(np.clip(reached / effective_target, 0.0, 1.0))


# --- 2. continuidad de trayectoria -------------------------------------------


def _continuity(
    spec: WindowSpec, window: list[PoseSample], config: ScoreConfig, raw: dict[str, float]
) -> float | None:
    """Penaliza los saltos de ángulo entre frames consecutivos.

    Un giro humano recorre los ángulos intermedios. Un corte de vídeo o un
    frame sustituido aparece como un salto que ningún cuello puede hacer.
    """
    pairs = [
        (a.pose, b.pose, a.at_us, b.at_us)
        for a, b in zip(window, window[1:], strict=False)
        if a.pose is not None and b.pose is not None
    ]
    if not pairs:
        return None

    nominal_dt_us = 1_000_000.0 / max(1e-6, config.nominal_fps)
    excess_total = 0.0
    max_jump = 0.0
    discontinuities = 0

    for previous, current, previous_at, current_at in pairs:
        jump = abs(current.angle(spec.axis) - previous.angle(spec.axis))
        max_jump = max(max_jump, jump)

        # Si entre los dos frames analizados pasó más tiempo del normal
        # —porque se descartaron frames por caudal—, un salto mayor es
        # esperable y no es sospechoso.
        dt_us = max(1.0, float(current_at - previous_at))
        allowance = config.continuity_threshold_deg * max(1.0, dt_us / nominal_dt_us)

        excess = max(0.0, jump - allowance)
        if excess > 0:
            discontinuities += 1
        excess_total += excess

    raw["max_jump_deg"] = float(max_jump)
    raw["discontinuity_count"] = float(discontinuities)
    raw["mean_abs_delta_deg"] = float(
        np.mean([abs(b.angle(spec.axis) - a.angle(spec.axis)) for a, b, _, _ in pairs])
    )

    budget = config.continuity_threshold_deg * len(pairs)
    return float(np.clip(1.0 - excess_total / budget, 0.0, 1.0))


# --- 3. paralaje / no rigidez ------------------------------------------------


def _parallax(
    spec: WindowSpec, window: list[PoseSample], config: ScoreConfig, raw: dict[str, float]
) -> float | None:
    """Cuánto residuo deja el mejor modelo plano.

    Se comparan los landmarks del par de frames con mayor separación angular:
    a más base, más paralaje, igual que en estereoscopía.

    El modelo nulo es una homografía, que es exactamente lo que describe el
    movimiento de un plano visto por una cámara. Si la homografía explica el
    movimiento casi sin residuo, lo que se está moviendo es un plano: una foto
    o una pantalla. Un rostro con volumen deja residuo porque la nariz y las
    orejas están a profundidades distintas.

    El residuo se divide entre los grados girados: el paralaje crece con la
    base, así que sin normalizar, quien gire poco parecería una foto.
    """
    usable = _usable(window)
    if len(usable) < 2:
        return None

    angles = [s.pose.angle(spec.axis) for s in usable if s.pose is not None]
    low_landmarks = usable[int(np.argmin(angles))].landmarks
    high_landmarks = usable[int(np.argmax(angles))].landmarks

    span = abs(max(angles) - min(angles))
    raw["angle_span_deg"] = float(span)
    if span < config.min_angle_span_deg:
        # Sin giro no hay paralaje que medir. No es un cero: es que no se
        # puede saber.
        return None

    if low_landmarks is None or high_landmarks is None:
        return None
    residual = planar_residual(low_landmarks, high_landmarks)
    if residual is None:
        return None

    # Se descuenta el ruido del detector antes de normalizar: una superficie
    # plana deja siempre algo de residuo, y sin restarlo un recorrido corto lo
    # convertiría en una tasa alta.
    rate = max(0.0, residual - config.parallax_noise_floor) / span
    raw["rigid_residual"] = float(residual)
    raw["nonrigidity_rate"] = float(rate)

    return float(np.clip(rate / max(1e-9, config.parallax_reference_rate), 0.0, 1.0))


def planar_residual(source: np.ndarray, target: np.ndarray) -> float | None:
    """Residuo del mejor modelo plano entre dos nubes de landmarks.

    Devuelve la raíz del error cuadrático medio dividida entre la diagonal del
    rostro, para que no dependa de lo cerca que esté la cara de la cámara.
    """
    if source is None or target is None:
        return None
    if len(source) != len(target) or len(source) < 4:
        return None

    a = np.ascontiguousarray(source[:, :2], dtype=np.float32)
    b = np.ascontiguousarray(target[:, :2], dtype=np.float32)

    # Mínimos cuadrados sobre todos los puntos, sin RANSAC: aquí no interesa
    # descartar los puntos que no encajan, ¡son justo la señal!
    matrix, _ = cv2.findHomography(a, b, 0)
    if matrix is None:
        return None

    projected = cv2.perspectiveTransform(a.reshape(-1, 1, 2), matrix).reshape(-1, 2)
    error = np.linalg.norm(projected - b, axis=1)

    diagonal = float(np.linalg.norm(a.max(axis=0) - a.min(axis=0)))
    if diagonal < 1e-6:
        return None

    return float(np.sqrt(np.mean(error**2)) / diagonal)


# --- 4. consistencia de identidad --------------------------------------------


def _unit(v: np.ndarray) -> np.ndarray:
    """Vector unitario. Un vector nulo se devuelve tal cual: no hay dirección
    que normalizar y fabricarle una sería inventar una identidad."""
    norm = float(np.linalg.norm(v))
    return v if norm <= 1e-12 else v / norm


def _identity(
    window: list[PoseSample], config: ScoreConfig, raw: dict[str, float]
) -> float | None:
    """Estabilidad del embedding a lo largo de la ventana.

    Se comparan frames CONSECUTIVOS, no todos contra el primero: girar la
    cabeza cambia el embedding poco a poco y compararlo contra el inicio
    penalizaría al usuario legítimo por hacer justo lo que se le pidió. Lo que
    delata una sustitución es el salto, no la deriva.
    """
    vectors = [s.embedding for s in window if s.embedding is not None]
    raw["embedding_samples"] = float(len(vectors))
    if len(vectors) < 2:
        return None

    similarities = [
        cosine_similarity(a, b) for a, b in zip(vectors, vectors[1:], strict=False)
    ]

    # Un salto suelto no basta: hay que preguntar si la identidad VOLVIÓ.
    #
    # Una sustitución y un frame con desenfoque producen lo mismo —un único
    # par consecutivo con similitud baja—, así que el mínimo suelto no los
    # distingue. Lo que los separa es lo que pasa DESPUÉS: tras un fallo del
    # tracker el embedding vuelve a la cara de antes, y tras una sustitución
    # se queda en la nueva. Por eso se compara el promedio anterior al salto
    # con el posterior.
    #
    # El desenfoque es inevitable justo en esta ventana, porque existe mientras
    # el sujeto gira la cabeza deprisa porque se lo hemos pedido. Con el mínimo
    # suelto, la señal sobre una cara REAL de una webcam dio 0 · 0,1178 ·
    # 0,3335 · 0,7499 en cuatro ventanas seguidas, y con el suelo de 0,35 acusó
    # de `attack_identity_change` a un usuario legítimo cuya sesión traía las
    # dos miradas aceptadas y las dos poses con paralaje 1.
    #
    # Es la misma lección que la regla de la mirada: preguntar si NINGÚN frame
    # baja del umbral tira respuestas correctas; lo que hay que preguntar es si
    # lo que bajó se quedó abajo.
    jump = int(np.argmin(similarities))
    worst = float(similarities[jump])

    # ¿La cara de ANTES del hueco es la misma que la de DESPUÉS?
    #
    # Se cruza el hueco por los dos lados porque el frame corrupto puede ser
    # cualquiera de los dos del par: `argmin` sólo dice que la similitud entre
    # v[j] y v[j+1] es baja. Usar v[j] como referencia cuando el malo es v[j]
    # daba 0,575 en una ventana de QUIETUD de once segundos.
    #
    # Y es LOCAL —tres muestras a cada lado— porque comparar la media de la
    # primera mitad contra la de la segunda mide deriva acumulada y no
    # sustitución: medido, eso daba 0,588 en esa misma ventana de quietud.
    span = 3
    antes = [i for i in range(jump - span, jump) if i >= 0]
    despues = [i for i in range(jump + 2, jump + 2 + span) if i < len(vectors)]
    across = 1.0
    if antes and despues:
        across = max(
            float(cosine_similarity(vectors[a], vectors[b])) for a in antes for b in despues
        )

    raw["embedding_min_similarity"] = worst
    raw["embedding_across_jump"] = across
    raw["embedding_mean_similarity"] = float(np.mean(similarities))

    # El salto sólo cuenta si la cara no volvió.
    worst = max(worst, across)

    spread = max(1e-6, config.identity_ceiling - config.identity_floor)
    return float(np.clip((worst - config.identity_floor) / spread, 0.0, 1.0))


# --- resumen -----------------------------------------------------------------


def _combine(submetrics: dict[str, float | None], weights: dict[str, float]) -> float:
    """Media ponderada de lo que se pudo medir.

    Los pesos se renormalizan sobre las sub-métricas disponibles: lo que no se
    midió no arrastra el resumen hacia abajo.
    """
    total = 0.0
    weight_sum = 0.0
    for name, value in submetrics.items():
        if value is None:
            continue
        weight = weights.get(name, 0.0)
        total += weight * value
        weight_sum += weight

    if weight_sum <= 0.0:
        return 0.0
    return float(np.clip(total / weight_sum, 0.0, 1.0))
