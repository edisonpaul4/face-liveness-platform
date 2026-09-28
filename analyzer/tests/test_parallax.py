"""Criterio de aceptación: el paralaje separa rostro real de foto girada.

El control es el estímulo: **el mismo movimiento, la misma cámara, distinto
objeto delante**. Una cabeza con volumen girando 30° y una foto impresa de esa
misma cara girando 30° a la misma distancia.
"""

from __future__ import annotations

import cv2
import pytest

from analyzer import pose, window
from tests.render import render_head_3d, render_photo_rotated, yaw_sweep

#: Fotogramas por barrido. A 15 fps son 1,6 s, lo que dura un reto de pose.
SWEEP_FRAMES = 24
#: Giro del objeto delante de la cámara.
SWEEP_DEGREES = 30.0


def _trace(detector, frames):
    """Pose y landmarks de cada frame."""
    samples = []
    for image in frames:
        detection = detector.detect(image)
        if not detection.faces or detection.transform is None:
            continue
        head = pose.from_transform(detection.transform)
        samples.append((head.yaw, pose.compact(detection.landmarks)))
    return samples


def _extremes(samples):
    """Par de frames con mayor separación angular, y esa separación."""
    yaws = [s[0] for s in samples]
    low = samples[yaws.index(min(yaws))]
    high = samples[yaws.index(max(yaws))]
    return low[1], high[1], max(yaws) - min(yaws)


@pytest.fixture(scope="module")
def sequences(mediapipe_detector):
    """Las dos secuencias con el MISMO estímulo: giro de 30°."""
    angles = yaw_sweep(SWEEP_DEGREES, SWEEP_FRAMES)
    frontal = render_head_3d(0.0)

    return {
        "real": _trace(mediapipe_detector, [render_head_3d(a) for a in angles]),
        "photo": _trace(mediapipe_detector, [render_photo_rotated(frontal, a) for a in angles]),
    }


def test_el_residuo_plano_separa_rostro_de_foto(sequences):
    """El corazón del asunto.

    Una foto es un plano, y el movimiento de un plano lo explica una
    homografía. Un rostro con volumen no: la nariz y las orejas están a
    profundidades distintas y se desplazan unas respecto a otras.
    """
    real_a, real_b, _ = _extremes(sequences["real"])
    photo_a, photo_b, _ = _extremes(sequences["photo"])

    real_residual = window.planar_residual(real_a, real_b)
    photo_residual = window.planar_residual(photo_a, photo_b)

    assert real_residual is not None and photo_residual is not None
    print(f"\nresiduo plano: rostro={real_residual:.5f} foto={photo_residual:.5f} "
          f"({real_residual / photo_residual:.1f}x)")

    # Separación holgada: el rostro deja al menos el triple de residuo.
    assert real_residual > photo_residual * 3.0, (
        f"el residuo no separa: rostro={real_residual:.5f} foto={photo_residual:.5f}"
    )
    # Y en términos absolutos, cada uno a su lado de la frontera.
    assert real_residual > 0.02, "el rostro con volumen debería dejar residuo claro"
    assert photo_residual < 0.015, "la foto debería quedar cerca del suelo de ruido"


def test_la_foto_no_consigue_fingir_el_giro(sequences):
    """La otra mitad de la defensa, y no menos importante.

    Girar una cartulina no produce la firma de pose de una cabeza girando: el
    ajuste 3D del detector apenas ve unos grados. Una foto impresa se cae ya
    en el cumplimiento, antes de llegar al paralaje.
    """
    _, _, real_span = _extremes(sequences["real"])
    _, _, photo_span = _extremes(sequences["photo"])

    print(f"\nrecorrido aparente: rostro={real_span:.1f}° foto={photo_span:.1f}°")

    assert real_span > 25.0, "el rostro 3D debería recorrer casi todo el giro real"
    assert photo_span < 8.0, "la foto no debería aparentar un giro de cabeza"
    assert real_span > photo_span * 3.0


def test_el_submetrico_de_paralaje_distingue_los_dos_casos(sequences, session_from_samples):
    """El mismo asunto, ya como sub-métrica 0-1."""
    real = window.analyze(
        window.WindowSpec("w-real", "yaw", 25.0, 5.0, 0, 10**12),
        session_from_samples(sequences["real"]),
    )
    photo = window.analyze(
        window.WindowSpec("w-photo", "yaw", 25.0, 5.0, 0, 10**12),
        session_from_samples(sequences["photo"]),
    )

    print(f"\nparalaje: rostro={real.submetrics['parallax']} foto={photo.submetrics['parallax']}")
    print(f"score:    rostro={real.score:.3f} foto={photo.score:.3f}")

    assert real.submetrics["parallax"] is not None
    assert real.submetrics["parallax"] > 0.9, "el rostro real debería puntuar alto en paralaje"

    # La foto no llega ni a tener recorrido con el que medir paralaje. No es
    # cero: es que no hay giro que analizar, y eso también lo dice la métrica.
    assert photo.submetrics["parallax"] is None or photo.submetrics["parallax"] < 0.4

    # Y el resumen las separa de todas formas, por el cumplimiento.
    assert real.score > photo.score + 0.3, (
        f"los resúmenes no separan: rostro={real.score:.3f} foto={photo.score:.3f}"
    )


def test_el_residuo_crece_con_la_base_angular(mediapipe_detector):
    """El paralaje es geometría: a más giro, más se desplazan entre sí.

    Si no creciera, no sería paralaje: sería ruido.
    """
    residuals = {}
    for peak in (10.0, 20.0, 30.0):
        samples = _trace(mediapipe_detector, [render_head_3d(a) for a in yaw_sweep(peak, 16)])
        a, b, span = _extremes(samples)
        residuals[peak] = (span, window.planar_residual(a, b))

    print("\n" + "\n".join(f"  giro {k:4.0f}° → base {v[0]:5.1f}° residuo {v[1]:.5f}"
                           for k, v in residuals.items()))

    assert residuals[10.0][1] < residuals[20.0][1] < residuals[30.0][1]


def test_un_plano_perfecto_no_deja_residuo():
    """Comprobación de la propia métrica, sin detector de por medio.

    Una homografía aplicada a unos puntos es, por definición, explicable por
    una homografía. Si esto no diera ~0, el residuo mediría otra cosa.
    """
    import numpy as np

    rng = np.random.default_rng(11)
    points = rng.uniform(0.2, 0.8, size=(120, 2)).astype(np.float32)

    homography = np.array([[1.08, 0.05, -0.03], [0.02, 1.02, -0.01], [0.15, 0.05, 1.0]],
                          dtype=np.float32)
    warped = cv2.perspectiveTransform(points.reshape(-1, 1, 2), homography).reshape(-1, 2)

    residual = window.planar_residual(points, warped)
    assert residual is not None and residual < 1e-4, f"residuo {residual} sobre un plano exacto"


def test_una_nube_con_profundidad_deja_residuo():
    """Y el contrario: puntos a distinta profundidad, girados, no lo explica."""
    import numpy as np

    rng = np.random.default_rng(3)
    points3d = rng.uniform(-0.5, 0.5, size=(120, 3)).astype(np.float32)

    def project(pts, yaw_deg):
        a = np.radians(yaw_deg)
        rot = np.array([[np.cos(a), 0, np.sin(a)], [0, 1, 0], [-np.sin(a), 0, np.cos(a)]],
                       dtype=np.float32)
        rotated = pts @ rot.T
        depth = 4.0 - rotated[:, 2]
        return np.stack([rotated[:, 0] / depth, rotated[:, 1] / depth], axis=1).astype(np.float32)

    residual = window.planar_residual(project(points3d, 0.0), project(points3d, 25.0))
    assert residual is not None and residual > 0.01, f"residuo {residual} sobre volumen real"
