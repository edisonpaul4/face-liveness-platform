#!/usr/bin/env python3
"""Analiza una foto o un vídeo con los detectores PASIVOS.

Existe porque un ataque grabado no puede responder a un guion aleatorio: no
mira al punto, no gira cuando se le pide y no reacciona al destello. Eso hace
imposible juzgarlo con el recorrido normal, y a la vez deja fuera del banco
justo el material que hay que medir — fotos impresas, replays, máscaras.

Lo que sí se puede juzgar de un archivo es todo lo que NO necesita
colaboración: textura, contexto, moiré, bandeo, reflejo especular, y el
paralaje si hay movimiento de cabeza.

    python3 bench/runner/analyze_media.py foto.jpg
    python3 bench/runner/analyze_media.py replay.mp4 --json

⚠ Esto NO es una prueba de vida. Es la mitad pasiva. Un archivo que salga
limpio aquí seguiría sin superar una sesión real, porque no puede responder a
nada. Sirve para lo contrario: para ver qué detectores pillan un ataque y
cuáles se lo tragan.
"""

from __future__ import annotations

import argparse
import json
import logging
import pathlib
import statistics
import sys
import time

ROOT = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "analyzer"))
sys.path.insert(0, str(ROOT / "analyzer" / "src"))

#: Señales pasivas: las que no dependen de que el sujeto colabore.
PASSIVE_SIGNALS = (
    "texture_pad_v2_a",
    "texture_pad_v2_b",
    "texture_pad_v1se_a",
    "texture_pad_v1se_b",
    "surface_moire_index",
    "surface_banding_index",
    "surface_specular_fraction",
    "surface_face_bg_luminance_ratio",
    "quality_sharpness",
    "quality_brightness",
    "quality_face_highlight",
    "face_area_ratio",
    "pose_yaw_deg",
    "pose_pitch_deg",
)

#: Máximo de frames a analizar. Un vídeo largo no aporta más y sí tarda.
MAX_FRAMES = 150

#: Resolución a la que se lleva todo antes de analizar.
#:
#: Es la de captura de una sesión real (640x480, §7). No es un ahorro: los
#: detectores ven en producción frames de webcam, y darles un vídeo en 4K los
#: pone ante una distribución de entrada que nunca van a encontrar. Un moiré
#: se ve distinto a 4K que a 480p, y el modelo PAD también.
ANALYSIS_WIDTH = 640
ANALYSIS_HEIGHT = 480

#: Cómo se lleva un frame de otra proporción a la de análisis.
#:
#: NO es un detalle: la elección cambia qué detector parece bueno, y por eso
#: es un parámetro explícito y no una decisión enterrada.
#:
#:   "crop"      recorta al centro. Conserva la escala del rostro, pierde
#:               periferia. Favorece a los modelos PAD, que miran textura.
#:   "letterbox" encaja el frame entero con bandas negras. Conserva todo el
#:               campo, encoge el rostro y mete un contexto —las barras— que
#:               ningún modelo vio al entrenarse. Favorece a moiré y bandeo,
#:               que necesitan el patrón periódico completo.
#:
#: Medido sobre el mismo dataset, el mejor APCER de cada uno:
#:
#:   letterbox   moiré 5,8 %   ·  PAD 12,5 %
#:   crop        moiré 23,5 %  ·  PAD  1,7 %
#:
#: O sea que el mismo dato sostiene dos conclusiones contrarias según cómo se
#: prepare. Lo honesto es medir en las dos y decirlo, no elegir la que
#: confirme lo que uno esperaba.
FIT_MODES = ("crop", "letterbox")
DEFAULT_FIT = "crop"


def read_frames(path: pathlib.Path, max_frames: int = MAX_FRAMES, fit: str = DEFAULT_FIT):
    """Devuelve los frames del archivo, sea imagen o vídeo."""
    import cv2

    image = cv2.imread(str(path))
    if image is not None:
        return [_to_analysis_size(image, fit)]

    capture = cv2.VideoCapture(str(path))
    if not capture.isOpened():
        raise ValueError(f"no se pudo abrir {path.name} ni como imagen ni como vídeo")

    total = int(capture.get(cv2.CAP_PROP_FRAME_COUNT) or 0)
    # Muestreo uniforme: de un vídeo largo interesa todo el recorrido, no su
    # primer segundo.
    stride = max(1, total // max_frames) if total > max_frames else 1

    frames, index = [], 0
    while len(frames) < max_frames:
        ok, frame = capture.read()
        if not ok:
            break
        if index % stride == 0:
            frames.append(_to_analysis_size(frame, fit))
        index += 1
    capture.release()

    if not frames:
        raise ValueError(f"{path.name} no contiene frames legibles")
    return frames


def _percentile(ordenados: list[float], q: float) -> float:
    """Percentil sobre una lista YA ordenada, con interpolación lineal."""
    if not ordenados:
        return 0.0
    if len(ordenados) == 1:
        return ordenados[0]
    pos = q * (len(ordenados) - 1)
    bajo = int(pos)
    alto = min(bajo + 1, len(ordenados) - 1)
    peso = pos - bajo
    return ordenados[bajo] * (1 - peso) + ordenados[alto] * peso


def _to_analysis_size(frame, fit=DEFAULT_FIT):
    """Lleva el frame a la resolución de captura, recortando al centro.

    Recorte central, NO letterbox. Las bandas negras parecen inofensivas y no
    lo son: el modelo PAD de recorte ancho vive del contexto alrededor del
    rostro, y unas barras negras son un contexto que nunca vio al entrenarse.

    Medido sobre los mismos replays: con letterbox el modelo daba 0,0006 de
    probabilidad de ataque; con recorte central, 0,18. Trescientas veces. La
    primera versión de este banco concluyó que los modelos PAD no servían, y
    la mitad de esa conclusión era este apaño.

    Tampoco se deforma la proporción: aplastarla cambia la geometría facial, y
    eso ya rompió una vez la detección con YuNet.
    """
    import cv2

    height, width = frame.shape[:2]
    if (width, height) == (ANALYSIS_WIDTH, ANALYSIS_HEIGHT):
        return frame

    if fit == "letterbox":
        import numpy as np

        scale = min(ANALYSIS_WIDTH / width, ANALYSIS_HEIGHT / height)
        new_w, new_h = max(1, int(width * scale)), max(1, int(height * scale))
        resized = cv2.resize(frame, (new_w, new_h), interpolation=cv2.INTER_AREA)
        canvas = np.zeros((ANALYSIS_HEIGHT, ANALYSIS_WIDTH, 3), dtype=frame.dtype)
        top, left = (ANALYSIS_HEIGHT - new_h) // 2, (ANALYSIS_WIDTH - new_w) // 2
        canvas[top : top + new_h, left : left + new_w] = resized
        return canvas

    target = ANALYSIS_WIDTH / ANALYSIS_HEIGHT
    if width / height > target:
        keep = int(height * target)
        left = (width - keep) // 2
        frame = frame[:, left : left + keep]
    else:
        keep = int(width / target)
        top = (height - keep) // 2
        frame = frame[top : top + keep]

    return cv2.resize(frame, (ANALYSIS_WIDTH, ANALYSIS_HEIGHT), interpolation=cv2.INTER_AREA)


def analyze(path: pathlib.Path, fit: str = DEFAULT_FIT) -> dict:
    """Analiza el archivo y devuelve el informe."""
    import os

    # A absoluta ANTES de cambiar de directorio: el detector resuelve sus
    # modelos en rutas relativas al analizador, y el argumento del usuario es
    # relativo a donde estaba.
    path = path.resolve()
    os.chdir(ROOT / "analyzer")

    import cv2
    from analyzer.sessions import SessionState

    from analyzer import window, wire

    config, pipeline = _pipeline()
    state = SessionState("media", 0, time.monotonic(), time.monotonic())

    frames = read_frames(path, fit=fit)
    per_signal: dict[str, list[float]] = {}
    with_face = 0

    for i, image in enumerate(frames, start=1):
        ok, buf = cv2.imencode(".jpg", image, [cv2.IMWRITE_JPEG_QUALITY, 92])
        if not ok:
            continue
        task = wire.FrameTask(
            seq=i, encoding=1, payload=buf.tobytes(), received_at_us=i * 66_000
        )
        try:
            outcome = pipeline.process(task, state)
        except Exception as exc:  # noqa: BLE001 - un frame malo no para el análisis
            logging.getLogger("media").debug("frame %d ilegible: %s", i, exc)
            continue
        if outcome.features.quality.face_detected:
            with_face += 1
        for name in PASSIVE_SIGNALS:
            value = outcome.features.signals.get(name)
            if value is not None:
                per_signal.setdefault(name, []).append(float(value))

    report: dict = {
        "file": path.name,
        "fit": fit,
        "frames": len(frames),
        "frames_with_face": with_face,
        "signals": {},
    }
    for name, values in sorted(per_signal.items()):
        ordenados = sorted(values)
        report["signals"][name] = {
            "min": round(ordenados[0], 5),
            "median": round(statistics.median(values), 5),
            # Percentiles altos: el punto medio entre el máximo —que deja que
            # un frame malo condene a alguien— y la mediana, que diluye una
            # evidencia que en el tiempo es esparsa. Sólo algunos frames de un
            # ataque lo delatan, y por eso ninguno de los dos extremos sirve.
            "p75": round(_percentile(ordenados, 0.75), 5),
            "p90": round(_percentile(ordenados, 0.90), 5),
            "max": round(ordenados[-1], 5),
            "samples": len(values),
        }

    # Paralaje: sólo tiene sentido si la cabeza se movió. Un plano y un rostro
    # con volumen se distinguen justo ahí — y una foto quieta no dice nada.
    yaws = per_signal.get("pose_yaw_deg", [])
    span = (max(yaws) - min(yaws)) if yaws else 0.0
    report["yaw_span_deg"] = round(span, 2)
    if len(state.samples) >= 6 and span >= 10.0:
        spec = window.WindowSpec(
            window_id="media",
            axis="yaw",
            target_deg=max(yaws, key=abs),
            tolerance_deg=8.0,
            started_at_us=0,
            ended_at_us=len(frames) * 66_000,
        )
        score = window.analyze(spec, state.samples, config.score_config())
        report["pose_window"] = score.to_dict()
    else:
        report["pose_window"] = None
        report["pose_window_reason"] = (
            "la cabeza no se movió lo suficiente: sin recorrido no hay paralaje que medir"
        )

    return report


#: Detector y modelos, cargados una sola vez por proceso.
#:
#: Construirlos cuesta segundos, y el banco recorre cientos de archivos. Sin
#: caché, medir un dataset de 150 vídeos se iba casi entero en cargar modelos.
_PIPELINE = None


def _pipeline():
    """Devuelve (config, pipeline), creándolos la primera vez."""
    global _PIPELINE
    if _PIPELINE is not None:
        return _PIPELINE

    from analyzer.config import load
    from analyzer.metrics import LatencyBudget
    from analyzer.pipeline import Pipeline
    from analyzer.worker import build_detector, build_pad

    config = load()
    log = logging.getLogger("media")
    pipeline = Pipeline(
        build_detector(config),
        budget=LatencyBudget("media", 30.0, log),
        pad=build_pad(config, log),
    )
    _PIPELINE = (config, pipeline)
    return _PIPELINE


def render(report: dict) -> None:
    print(f"\narchivo: {report['file']}")
    print(f"  frames analizados: {report['frames']}   con rostro: {report['frames_with_face']}")
    if not report["frames_with_face"]:
        print("\n  Sin rostro detectado: no hay nada que medir.")
        return

    print(f"\n{'señal':34} {'mín':>10} {'mediana':>10} {'máx':>10}")
    print("-" * 68)
    for name, stats in report["signals"].items():
        print(f"{name:34} {stats['min']:10.4f} {stats['median']:10.4f} {stats['max']:10.4f}")

    print(f"\n  recorrido de yaw: {report['yaw_span_deg']}°")
    pose = report.get("pose_window")
    if pose:
        print("  ventana de pose:")
        for k, v in pose.get("submetrics", {}).items():
            print(f"    {k:14} {'no medida' if v is None else v}")
    else:
        print(f"  ventana de pose: {report.get('pose_window_reason')}")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("media", type=pathlib.Path)
    parser.add_argument("--json", action="store_true", help="salida en JSON")
    parser.add_argument("--fit", choices=FIT_MODES, default=DEFAULT_FIT,
                        help="cómo encajar un frame de otra proporción")
    args = parser.parse_args()

    report = analyze(args.media, fit=args.fit)
    if args.json:
        print(json.dumps(report, ensure_ascii=False))
    else:
        render(report)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
