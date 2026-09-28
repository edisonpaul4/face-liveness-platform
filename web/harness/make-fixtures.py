#!/usr/bin/env python3
"""Genera los frames sintéticos que el banco del cliente usa como cámara.

No se versionan binarios: se regeneran. Los renderizadores viven en el
analizador (analyzer/tests) porque es allí donde se validan las señales; aquí
sólo se les pide un juego de frames coherente con los retos que el servidor
puede pedir.

    python3 web/harness/make-fixtures.py

Cobertura: calibración y destello (los cinco colores), yaw en ambos sentidos y
acercamiento. El pitch se genera pero NO alcanza el umbral: una cabeza dibujada
deja de ser detectable antes de que el ángulo medido llegue a 15 grados. Es una
limitación del maniquí, no del producto; el banco lo dice en voz alta.
"""

import pathlib
import sys

import cv2

ROOT = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "analyzer"))
sys.path.insert(0, str(ROOT / "analyzer" / "src"))

from tests.flashscene import SKIN_TONES, Camera, Scene, build_geometry, render_frame  # noqa: E402
from tests.render import render_head_3d  # noqa: E402
from analyzer.config import load  # noqa: E402
from analyzer.worker import build_detector  # noqa: E402

OUT = pathlib.Path(__file__).resolve().parent / "fixtures"
#: BGR normalizado, que es lo que pinta el renderizador.
COLORS = {
    "none": None,
    "white": (1.0, 1.0, 1.0),
    "red": (0.0, 0.0, 1.0),
    "green": (0.0, 1.0, 0.0),
    "blue": (1.0, 0.0, 0.0),
}
# Calidad alta a propósito: el banco no está midiendo el códec, y comprimir de
# más borraría justo la señal cromática que se quiere ejercitar.
JPEG = [cv2.IMWRITE_JPEG_QUALITY, 92]


def write(name: str, image) -> None:  # noqa: ANN001
    OUT.mkdir(parents=True, exist_ok=True)
    ok, buf = cv2.imencode(".jpg", image, JPEG)
    if not ok:
        raise RuntimeError(f"no se pudo codificar {name}")
    (OUT / f"{name}.jpg").write_bytes(buf.tobytes())


def main() -> int:
    import numpy as np

    # El detector resuelve sus modelos en rutas relativas al analizador.
    import os

    os.chdir(ROOT / "analyzer")
    detector = build_detector(load())
    geometry = build_geometry(detector)
    # Sujeto real, tono medio, luz de habitación normal: el caso que el
    # cliente tiene que saber conducir de punta a punta.
    scene = Scene(kind="real", tone=SKIN_TONES[2], camera=Camera())

    for name, color in COLORS.items():
        rgb = None if color is None else np.array(color, dtype=float)
        write(f"flash_{name}", render_frame(scene, geometry, rgb, frame_index=0))

    for deg in range(-30, 31, 5):
        write(f"yaw_{deg:+03d}", render_head_3d(yaw_deg=float(deg)))

    # Acercamiento: la escala del render crece, y con ella la proporción del
    # bbox. Es el mismo efecto que dar un paso hacia la cámara.
    for step, scale in enumerate((1.0, 1.1, 1.2, 1.3, 1.45)):
        write(f"closer_{step}", render_head_3d(scale=scale))

    # Mejor esfuerzo. Documentado como insuficiente en el docstring.
    write("pitch_up", render_head_3d(pitch_deg=-45.0))

    detector.close()
    print(f"{len(list(OUT.glob('*.jpg')))} frames en {OUT}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
