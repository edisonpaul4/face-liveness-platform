"""Dirección de la mirada por frame, a partir de los landmarks de iris.

Sólo mide. Dónde estaba el objetivo y si el sujeto lo miró lo decide Go
(CLAUDE.md §3): aquí no se sabe que existan los retos.

Lo que se emite es un desplazamiento del iris DENTRO de su órbita,
normalizado por el tamaño del propio ojo. Normalizar es lo que hace que el
número signifique lo mismo con una cara cerca y una lejos, con ojos grandes y
pequeños: la escala del sujeto se cancela en la división, igual que el albedo
se cancela en la respuesta cromática del destello.

No se convierte a grados de mirada. Hacerlo exigiría conocer la distancia al
sujeto, el tamaño de su globo ocular y la posición de la cámara respecto a la
pantalla — tres cosas que no tenemos y que habría que fingir. Un
desplazamiento relativo medido honestamente vale más que un ángulo inventado.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

#: Centros de iris en la malla de atención de MediaPipe (478 puntos).
IRIS_LEFT_CENTER = 468
IRIS_RIGHT_CENTER = 473
#: Contorno del iris: cinco puntos por ojo, centro incluido.
IRIS_LEFT = slice(468, 473)
IRIS_RIGHT = slice(473, 478)

#: Comisuras de cada ojo, para acotar la órbita en horizontal.
LEFT_EYE_CORNERS = (33, 133)
RIGHT_EYE_CORNERS = (362, 263)
#: Párpados superior e inferior, para acotarla en vertical.
LEFT_EYE_LIDS = (159, 145)
RIGHT_EYE_LIDS = (386, 374)

#: Por debajo de este ancho de ojo en píxeles la medida no se sostiene: el
#: iris ocupa tan pocos píxeles que el ruido del landmark domina.
MIN_EYE_WIDTH_PX = 12.0

#: Apertura por debajo de la cual no hay ojo que medir: párpado cerrado.
#:
#: Es un suelo de EXISTENCIA, no un umbral de calidad, y por eso es tan bajo.
#: Estaba en 0,15 con este motivo: "un ojo entrecerrado da un desplazamiento
#: vertical que es del párpado, no de la mirada". Pero el eje vertical se
#: abandonó (§4) y `gaze_offset_y` ya no interviene en ninguna decisión, así
#: que esa puerta protegía un canal que no existe — y a cambio tiraba la medida
#: horizontal entera.
#:
#: Costó dos sesiones seguidas de un usuario legítimo. Medido con la puerta
#: desactivada sobre una de ellas: su apertura iba de 0,045 a 0,139, SIEMPRE
#: por debajo de 0,15, así que el reto se cerró con avance CERO. Y sin embargo
#: la mirada estaba ahí: el mundo pasó de ~1,5° antes del objetivo a 10-15°
#: después, con el acuerdo entre ojos entre 0,67 y 0,92 todo el tramo.
#:
#: Un umbral ABSOLUTO de apertura es además lo que §4 prohíbe en todo lo demás:
#: la apertura depende de la anatomía y del ángulo de la cámara, así que
#: convierte la cara de cada uno en motivo de rechazo. Quien decide si una
#: apertura es suficiente es Go, contra el reposo del propio sujeto (§3: Go
#: decide, Python mide — y descartar una medida es decidir).
MIN_EYE_OPENNESS = 0.02


@dataclass(frozen=True, slots=True)
class Gaze:
    """Desplazamiento del iris dentro de la órbita, por ojo y promediado.

    Origen en el centro de la órbita. Signos, en coordenadas de IMAGEN y por
    tanto vistos desde la cámara, que mira al sujeto de frente:

    * x > 0: el iris se desplaza hacia la derecha de la imagen, que es la
      izquierda del sujeto.
    * y > 0: el iris se desplaza hacia abajo de la imagen.

    El mismo convenio que la pose, y por el mismo motivo: los ángulos se miden
    desde la cámara aunque los retos se enuncien desde el sujeto (CLAUDE.md §9).
    """

    #: Desplazamiento medio de los dos ojos, en unidades de ancho de ojo.
    offset_x: float
    offset_y: float
    #: Cuánto se parecen los dos ojos entre sí, 0-1. Los dos ojos de una
    #: persona miran al mismo sitio; un desacuerdo grande es un iris mal
    #: detectado, no una mirada rara.
    agreement: float
    #: Apertura media de los ojos, alto/ancho. Sirve para saber si la medida
    #: vertical es creíble y para detectar parpadeo.
    openness: float


def measure(landmarks: np.ndarray | None, width: int, height: int) -> Gaze | None:
    """Mide el desplazamiento del iris. None si no se puede medir.

    Devolver None es una respuesta legítima y frecuente: gafas con reflejo,
    ojos cerrados, cara demasiado lejos. Una señal que no se pudo medir NO
    entra en la fusión, y por eso no se rellena con un cero (CLAUDE.md §4).
    """
    if landmarks is None or len(landmarks) < 478:
        return None

    left = _eye_offset(
        landmarks, IRIS_LEFT_CENTER, LEFT_EYE_CORNERS, LEFT_EYE_LIDS, width, height
    )
    right = _eye_offset(
        landmarks, IRIS_RIGHT_CENTER, RIGHT_EYE_CORNERS, RIGHT_EYE_LIDS, width, height
    )
    if left is None or right is None:
        return None

    (lx, ly, lopen), (rx, ry, ropen) = left, right

    # Desacuerdo entre ojos, en las mismas unidades que el desplazamiento. Un
    # ojo bien detectado y otro no dan números muy distintos.
    disagreement = float(np.hypot(lx - rx, ly - ry))
    agreement = float(np.clip(1.0 - disagreement / 0.5, 0.0, 1.0))

    return Gaze(
        offset_x=float((lx + rx) / 2.0),
        offset_y=float((ly + ry) / 2.0),
        agreement=agreement,
        openness=float((lopen + ropen) / 2.0),
    )


def _eye_offset(
    landmarks: np.ndarray,
    iris_center: int,
    corners: tuple[int, int],
    lids: tuple[int, int],
    width: int,
    height: int,
) -> tuple[float, float, float] | None:
    """Desplazamiento del iris de un ojo, normalizado por el ancho del ojo.

    La referencia es la línea que une las dos comisuras, no el punto medio
    entre párpados. Es la diferencia entre medir algo y no medir nada: los
    párpados SIGUEN a la mirada —al mirar abajo bajan los dos—, así que el
    iris se queda siempre centrado entre ellos y la componente vertical se
    cancela sola. Medido con una cara real: 0,002 de desvío vertical donde
    debería haber habido diez veces más.

    Las comisuras están fijas al cráneo. Y usar su eje trae de regalo la
    compensación del balanceo de cabeza: si la persona inclina la cabeza, el
    eje se inclina con ella.
    """
    inner, outer = landmarks[corners[0]], landmarks[corners[1]]
    upper, lower = landmarks[lids[0]], landmarks[lids[1]]
    iris = landmarks[iris_center]

    # A píxeles: normalizar en coordenadas relativas mezclaría la relación de
    # aspecto del frame con la del ojo.
    corner_a = np.array([inner[0] * width, inner[1] * height], dtype=np.float64)
    corner_b = np.array([outer[0] * width, outer[1] * height], dtype=np.float64)

    axis = corner_b - corner_a
    eye_width = float(np.hypot(axis[0], axis[1]))
    if eye_width < MIN_EYE_WIDTH_PX:
        return None

    eye_height = float(abs(upper[1] - lower[1]) * height)
    openness = eye_height / eye_width
    if openness < MIN_EYE_OPENNESS:
        return None

    # El eje apunta siempre hacia la derecha de la IMAGEN, para que los signos
    # signifiquen lo mismo en los dos ojos y en cualquier orden de landmarks.
    if axis[0] < 0:
        axis = -axis
    along = axis / eye_width
    # Perpendicular: con el eje hacia la derecha, apunta hacia abajo, que es
    # el sentido en que crece la Y de una imagen.
    down = np.array([-along[1], along[0]])

    center = (corner_a + corner_b) / 2.0
    offset = np.array([float(iris[0]) * width, float(iris[1]) * height]) - center

    return (
        float(offset @ along) / eye_width,
        float(offset @ down) / eye_width,
        openness,
    )
