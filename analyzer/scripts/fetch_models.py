"""Descarga los modelos de detección.

No se versionan: son binarios grandes y con su propia licencia. Se bajan una
vez y se quedan en `analyzer/models/`, que está en .gitignore.
"""

from __future__ import annotations

import hashlib
import pathlib
import sys
import urllib.request

MODELS = {
    "face_landmarker.task": (
        "https://storage.googleapis.com/mediapipe-models/face_landmarker/"
        "face_landmarker/float16/1/face_landmarker.task"
    ),
    "face_detection_yunet_2023mar.onnx": (
        "https://media.githubusercontent.com/media/opencv/opencv_zoo/main/"
        "models/face_detection_yunet/face_detection_yunet_2023mar.onnx"
    ),
    # Embeddings para medir continuidad de identidad dentro de una sesión.
    # No es reconocimiento facial: no se compara contra ninguna base.
    "face_recognition_sface_2021dec.onnx": (
        "https://media.githubusercontent.com/media/opencv/opencv_zoo/main/"
        "models/face_recognition_sface/face_recognition_sface_2021dec.onnx"
    ),
    # PAD pasivo por textura y contexto (MiniFASNet, Apache-2.0). Dos
    # variantes con recortes de distinta amplitud: la V2 mira poco más que la
    # cara y la V1SE abarca el entorno, que es donde se ven los bordes de una
    # foto o el bisel de una pantalla.
    #
    # Emite MEDIDAS, no veredictos: el analizador publica sus probabilidades
    # de ataque y quien concluye es Go (CLAUDE.md §3).
    "MiniFASNetV2.onnx": (
        "https://github.com/yakhyo/face-anti-spoofing/releases/download/"
        "weights/MiniFASNetV2.onnx"
    ),
    "MiniFASNetV1SE.onnx": (
        "https://github.com/yakhyo/face-anti-spoofing/releases/download/"
        "weights/MiniFASNetV1SE.onnx"
    ),
}

#: Por debajo de esto es una página de error o un puntero de Git LFS, no un
#: modelo. Pasó de verdad: la URL cruda de GitHub devuelve 131 bytes.
MIN_BYTES = 100_000


def main() -> int:
    target = pathlib.Path(__file__).resolve().parents[1] / "models"
    target.mkdir(exist_ok=True)

    for name, url in MODELS.items():
        path = target / name
        if path.is_file() and path.stat().st_size >= MIN_BYTES:
            print(f"ya está: {name} ({path.stat().st_size} bytes)")  # noqa: T201
            continue

        print(f"descargando {name}…")  # noqa: T201
        try:
            with urllib.request.urlopen(url, timeout=120) as response:  # noqa: S310
                data = response.read()
        except Exception as exc:  # noqa: BLE001
            print(f"  FALLO: {exc}", file=sys.stderr)  # noqa: T201
            return 1

        if len(data) < MIN_BYTES:
            msg = f"  FALLO: sólo {len(data)} bytes; ¿un puntero de Git LFS?"
            print(msg, file=sys.stderr)  # noqa: T201
            return 1

        path.write_bytes(data)
        digest = hashlib.sha256(data).hexdigest()[:16]
        print(f"  {name}: {len(data)} bytes, sha256:{digest}…")  # noqa: T201

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
