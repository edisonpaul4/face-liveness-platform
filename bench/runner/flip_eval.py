#!/usr/bin/env python3
"""Mide un modelo FLIP exportado contra el banco de ataques.

Hace lo mismo que `pad_crop_sweep.py` pero con la pareja visión+frases que
produce `flip_export.py`: recorta a varias escalas, clasifica y mide el AUC.
El recorte se barre porque la documentación de estos modelos nunca dice cuál
esperan, y ya ha sido el parámetro que decidía el resultado dos veces.

    python3 bench/runner/flip_eval.py analyzer/models ~/datasets/liveness/axon-fas
"""

from __future__ import annotations

import argparse
import pathlib
import sys

import numpy as np

ROOT = pathlib.Path(__file__).resolve().parents[2]
for extra in ("analyzer", "analyzer/src", "bench/runner"):
    sys.path.insert(0, str(ROOT / extra))

from analyze_media import read_frames  # noqa: E402
from bench_dataset import MEDIA_SUFFIXES, classify  # noqa: E402
from pad_crop_sweep import auc  # noqa: E402

MEAN = np.array([0.485, 0.456, 0.406], dtype=np.float32)
STD = np.array([0.229, 0.224, 0.225], dtype=np.float32)
SCALES = [1.0, 1.3, 1.8, 2.4, 3.0]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("models", type=pathlib.Path)
    parser.add_argument("dataset", type=pathlib.Path)
    parser.add_argument("--name", default="flip")
    parser.add_argument("--frames", type=int, default=2)
    args = parser.parse_args()

    import cv2
    import onnxruntime as ort

    import analyzer.pad as pad
    from analyzer.config import load
    from analyzer.worker import build_detector

    text = np.load(args.models / f"{args.name}_text.npy")  # (2, 512): real, ataque
    opts = ort.SessionOptions()
    opts.intra_op_num_threads = 1
    opts.inter_op_num_threads = 1
    vision = ort.InferenceSession(
        str(args.models / f"{args.name}_vision.onnx"), opts, providers=["CPUExecutionProvider"]
    )

    def attack_margin(patch_bgr: np.ndarray) -> float:
        """Cuánto más se parece a las frases de ataque que a las de real.

        Se puntúa el MARGEN de similitud, no la probabilidad. Aplicarle la
        temperatura de CLIP (100) satura el softmax: los ataques salen todos
        clavados en 1,0000 y el AUC se hunde por empates, que es un artefacto
        de la escala y no del modelo. El margen es monótono con la
        probabilidad y conserva el orden, que es lo único que mide el AUC.
        """
        img = cv2.resize(patch_bgr, (224, 224)).astype(np.float32)[:, :, ::-1] / 255.0
        img = (img - MEAN) / STD
        tensor = np.ascontiguousarray(img.transpose(2, 0, 1)[None])
        feats = vision.run(None, {"pixels": tensor})[0][0]
        similarity = text @ feats
        # Fila 1 (ataque) menos fila 0 (real). Sólo se publica esta
        # diferencia: concluir que NO hay ataque es decidir, y decide Go.
        return float(similarity[1] - similarity[0])

    cfg = load()
    detector = build_detector(cfg)
    buckets = {s: {"live": [], "spoof": []} for s in SCALES}
    seen = {"live": 0, "spoof": 0}

    for path in sorted(args.dataset.rglob("*")):
        if not path.is_file() or path.name.startswith(".") or path.suffix.lower() not in MEDIA_SUFFIXES:
            continue
        label, _ = classify(path, args.dataset)
        if label not in ("live", "spoof"):
            continue
        try:
            frames = read_frames(path, max_frames=args.frames)
        except (ValueError, OSError):
            continue

        best: dict[float, float] = {}
        for frame in frames:
            primary = detector.detect(frame).primary
            if primary is None:
                continue
            height, width = frame.shape[:2]
            box = (
                primary.x * width,
                primary.y * height,
                primary.w * width,
                primary.h * height,
            )
            for scale in SCALES:
                patch = pad._crop(frame, box, scale)
                if patch is not None:
                    best[scale] = max(best.get(scale, -9.9), attack_margin(patch))
        if not best:
            continue
        seen[label] += 1
        for scale, value in best.items():
            buckets[scale][label].append(value)

    detector.close()

    print(f"caras reales {seen['live']}   ataques {seen['spoof']}\n")
    print(f"{'recorte':>8} {'AUC':>8} {'margen reales':>15} {'margen ataques':>16}")
    for scale in SCALES:
        b = buckets[scale]
        if not b["live"] or not b["spoof"]:
            continue
        print(
            f"{scale:8.1f} {auc(b['spoof'], b['live']):8.3f} "
            f"{np.median(b['live']):15.4f} {np.median(b['spoof']):16.4f}"
        )
    print("\nreferencia — MiniFASNet v2 con recorte 2,4:  AUC 0.970")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
