#!/usr/bin/env python3
"""Busca a qué escala de recorte rinde mejor el clasificador pasivo.

Los pesos de MiniFASNet llevan la escala en el nombre —`2.7_80x80`,
`4_80x80`— pero esa escala está referida a las cajas del detector con el que
se entrenó, no a las de MediaPipe, que son más ceñidas. Si el recorte que se
le pasa no es el que vio entrenando, el modelo mide ruido y todo lo que se
decida encima de él da igual.

Esto lo comprueba con datos en vez de con fe: recorta cada imagen a muchas
escalas, clasifica, y mide la separación entre caras reales y ataques con el
área bajo la curva ROC. El AUC se elige porque no depende de dónde se ponga
el umbral: la pregunta es si la señal ordena bien, no dónde cortar.

    python3 bench/runner/pad_crop_sweep.py ~/datasets/liveness/axon-fas

Sobre el dato: sale del dataset, no del repositorio (CLAUDE.md §9, regla 6).
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

#: Escalas a probar. Se abre por debajo de 2.7 y por encima de 4.0 a propósito:
#: si el óptimo cae en un extremo del barrido, el barrido es el que está mal.
SCALES = [1.5, 2.0, 2.4, 2.7, 3.0, 3.5, 4.0, 4.5, 5.0]


def auc(positive: list[float], negative: list[float]) -> float:
    """Área bajo la ROC, por el estadístico de Mann-Whitney.

    Es la probabilidad de que un ataque tomado al azar puntúe más alto que una
    cara real tomada al azar. 0,5 es no distinguir nada; por debajo de 0,5 la
    señal está invertida, que es peor que inútil porque parece que funciona.
    """
    if not positive or not negative:
        return float("nan")
    values = np.concatenate([positive, negative])
    ranks = values.argsort().argsort().astype(np.float64) + 1
    # Empates: rango medio, o el AUC se infla con señales que saturan.
    for value in np.unique(values):
        same = values == value
        if same.sum() > 1:
            ranks[same] = ranks[same].mean()
    n_pos, n_neg = len(positive), len(negative)
    rank_sum = ranks[:n_pos].sum()
    return float((rank_sum - n_pos * (n_pos + 1) / 2) / (n_pos * n_neg))


def _with_border(image, box, scale: float):
    """Amplía el lienzo por reflexión para que quepa el recorte pedido.

    La reflexión se elige sobre el negro o el gris porque un borde plano crea
    un gradiente artificial justo donde el modelo busca bordes de papel y
    biseles de pantalla: sería fabricar la pista que se quiere detectar.
    """
    import cv2

    x, y, box_w, box_h = box
    pad_x = int(max(0, (box_w * scale - image.shape[1]) / 2 + box_w))
    pad_y = int(max(0, (box_h * scale - image.shape[0]) / 2 + box_h))
    if pad_x == 0 and pad_y == 0:
        return image, box
    bordered = cv2.copyMakeBorder(image, pad_y, pad_y, pad_x, pad_x, cv2.BORDER_REFLECT_101)
    return bordered, (x + pad_x, y + pad_y, box_w, box_h)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("dataset", type=pathlib.Path)
    parser.add_argument("--limit", type=int, default=0)
    parser.add_argument(
        "--scales",
        default="",
        help="escalas a barrer, separadas por coma. Por omisión, SCALES.",
    )
    parser.add_argument(
        "--border",
        action="store_true",
        help=(
            "rellena el borde por reflexión para que la escala pedida se "
            "alcance siempre. Sin esto, una cara cercana satura el recorte y "
            "la escala nominal no se aplica."
        ),
    )
    parser.add_argument(
        "--frames",
        type=int,
        default=4,
        help="frames por vídeo. Se agregan por el máximo, como en producción.",
    )
    args = parser.parse_args()

    global SCALES
    if args.scales:
        SCALES = [float(x) for x in args.scales.split(",")]

    import cv2

    import analyzer.pad as pad
    from analyzer.config import load
    from analyzer.worker import build_detector

    cfg = load()
    detector = build_detector(cfg)
    model = pad.PadModel(pathlib.Path(cfg.model_dir))

    files = sorted(
        p
        for p in args.dataset.rglob("*")
        if p.is_file() and p.suffix.lower() in MEDIA_SUFFIXES and not p.name.startswith(".")
    )
    if args.limit:
        files = files[: args.limit]

    # {modelo: {escala: {"live": [...], "spoof": [...]}}}
    scores: dict[str, dict[float, dict[str, list[float]]]] = {}
    seen = {"live": 0, "spoof": 0}
    skipped = 0

    for path in files:
        label, _ = classify(path, args.dataset)
        if label not in ("live", "spoof"):
            continue

        try:
            frames = read_frames(path, max_frames=args.frames)
        except (ValueError, OSError):
            skipped += 1
            continue

        # Un archivo da UNA puntuación por modelo y escala: el máximo de sus
        # frames. Es la agregación que usa el gateway en producción, medida y
        # elegida en bench/RESULTS.md. Cambiarla aquí mediría otro sistema.
        per_scale: dict[float, dict[str, float]] = {}
        for frame in frames:
            primary = detector.detect(frame).primary
            if primary is None:
                continue
            # La caja viene normalizada; el recorte trabaja en píxeles.
            height, width = frame.shape[:2]
            box = (
                primary.x * width,
                primary.y * height,
                primary.w * width,
                primary.h * height,
            )
            source, source_box = frame, box
            if args.border:
                source, source_box = _with_border(frame, box, max(SCALES))
            for scale in SCALES:
                patch = pad._crop(source, source_box, scale)
                if patch is None:
                    continue
                for key, session in model._sessions.items():
                    probs = pad._infer(session, patch)
                    # La clase 1 es "genuino"; se mide su complemento para que
                    # un valor alto signifique ataque, como los demás.
                    value = float(1.0 - probs[pad.CLASS_GENUINE])
                    bucket = per_scale.setdefault(scale, {})
                    bucket[key] = max(bucket.get(key, 0.0), value)

        if not per_scale:
            skipped += 1
            continue

        seen[label] += 1
        for scale, per_model in per_scale.items():
            for key, value in per_model.items():
                scores.setdefault(key, {}).setdefault(scale, {"live": [], "spoof": []})
                scores[key][scale][label].append(value)

    detector.close()

    print(f"caras reales {seen['live']}  ataques {seen['spoof']}  sin rostro {skipped}\n")
    print(f"{'escala':>7}  " + "  ".join(f"{k:>12}" for k in sorted(scores)))
    print(f"{'':>7}  " + "  ".join(f"{'AUC':>12}" for _ in scores))
    for scale in SCALES:
        cells = []
        for key in sorted(scores):
            bucket = scores[key].get(scale)
            cells.append("           —" if not bucket else f"{auc(bucket['spoof'], bucket['live']):12.3f}")
        marks = "".join(
            " ←" if pad.SCALES.get(key) == scale else "" for key in sorted(scores)
        )
        print(f"{scale:7.1f}  " + "  ".join(cells) + marks)

    print("\nmejor por modelo:")
    for key in sorted(scores):
        best = max(
            scores[key],
            key=lambda s: auc(scores[key][s]["spoof"], scores[key][s]["live"]),
        )
        value = auc(scores[key][best]["spoof"], scores[key][best]["live"])
        current = pad.SCALES.get(key)
        if current in scores[key]:
            now = auc(scores[key][current]["spoof"], scores[key][current]["live"])
            print(f"  {key:>6}: {best} (AUC {value:.3f})   en uso {current} (AUC {now:.3f})")
        else:
            print(f"  {key:>6}: {best} (AUC {value:.3f})   en uso {current} (fuera del barrido)")

    # Los dos modelos ven el MISMO recorte —la saturación se lo impone—, así
    # que no son dos vistas independientes sino dos votos sobre la misma
    # entrada. Esto mide si combinarlos aporta algo sobre el mejor de ellos.
    common = sorted(set.intersection(*(set(v) for v in scores.values()))) if len(scores) > 1 else []
    if common:
        print("\ncombinando los dos modelos:")
        print(f"{'escala':>7}  {'máximo':>8}  {'media':>8}  {'mejor solo':>11}")
        for scale in common:
            per = {k: scores[k][scale] for k in scores}
            keys = sorted(per)
            combos = {}
            for how, fn in (("max", max), ("mean", lambda a, b: (a + b) / 2)):
                combos[how] = {
                    label: [
                        fn(a, b)
                        for a, b in zip(per[keys[0]][label], per[keys[1]][label], strict=True)
                    ]
                    for label in ("live", "spoof")
                }
            solo = max(auc(per[k]["spoof"], per[k]["live"]) for k in keys)
            print(
                f"{scale:7.1f}  "
                f"{auc(combos['max']['spoof'], combos['max']['live']):8.3f}  "
                f"{auc(combos['mean']['spoof'], combos['mean']['live']):8.3f}  "
                f"{solo:11.3f}"
            )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
