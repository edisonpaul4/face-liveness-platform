#!/usr/bin/env python3
"""Reproduce una grabación de sesión contra el analizador real.

Sirve para lo que no se puede hacer de otra forma: ajustar umbrales contra
caras de verdad sin tener que pedirle a alguien que repita la sesión cada vez.

Lee el formato que escribe web/serve.mjs —una línea JSON de cabecera y después
4 bytes de longitud + JPEG por frame—, pasa cada frame por el mismo Pipeline
que corre en producción, y alinea las señales con la línea de tiempo de retos
que el cliente anotó. El resultado es una tabla de "esto se le pidió, esto
midió su cara".

    python3 bench/runner/replay.py web/harness/recordings/session-....bin
    python3 bench/runner/replay.py --signals gaze_offset_x,gaze_offset_y <fichero>

Sobre el dato: estas grabaciones son biometría (CLAUDE.md §6bis). Viven en el
disco de quien las grabó, no se versionan, y no deberían sobrevivir al ajuste
para el que se tomaron.
"""

from __future__ import annotations

import argparse
import json
import pathlib
import struct
import sys
import time

ROOT = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "analyzer"))
sys.path.insert(0, str(ROOT / "analyzer" / "src"))


def frame_times_us(header: dict, count: int, *, fps: float | None = None) -> list[int]:
    """Sellos de captura de cada frame, en microsegundos y relativos al primero.

    Los usa de la cabecera si están. Si no —grabaciones anteriores a que se
    guardaran— los reconstruye a la cadencia NOMINAL y avisa, porque esa
    reconstrucción es la que hizo inútiles cinco diagnósticos: el servidor pide
    30 fps y llegan 14 a 21, así que la ventana de un destello acaba donde no
    es y se mide ruido.
    """
    stamps = header.get("frame_captured_at_us")
    if isinstance(stamps, list) and len(stamps) >= count:
        base = stamps[0]
        return [int(s - base) for s in stamps[:count]]

    nominal = fps or (header.get("capture") or {}).get("fps") or 15.0
    print(
        f"  ⚠ grabación sin sellos por frame: se reconstruyen a {nominal:.0f} fps.\n"
        "    La cadencia real casi nunca coincide, así que las ventanas de "
        "destello y pulso caerán desplazadas.",
        file=sys.stderr,
    )
    step = int(1e6 / nominal)
    return [i * step for i in range(count)]


def event_times_us(header: dict) -> list[tuple[int, dict]]:
    """Eventos con su instante en el MISMO origen que frame_times_us().

    Se niega a alinear en vez de estimar, y ésa es toda la razón de existir.
    Las grabaciones viejas sellaban los frames en época y los eventos con
    `performance.now()` —dos relojes sin nada en común—, así que había que
    cuadrarlos a ojo. Un desfase de segundo y medio hizo diagnosticar un
    retraso del gateway que no existía: medido después, sus medidas volvían en
    23 ms. Una alineación inventada no se distingue de una buena hasta que ya
    ha costado dos sesiones.
    """
    stamps = header.get("frame_captured_at_us")
    if not isinstance(stamps, list) or not stamps:
        raise ValueError("grabación sin sellos por frame: no se puede alinear")
    base = int(stamps[0])
    eventos = header.get("events") or []
    fuera = [e for e in eventos if abs(int(e["at_ms"]) * 1000 - base) > 3_600_000_000]
    if fuera:
        raise ValueError(
            "los eventos de esta grabación NO van en época (reloj de "
            "performance.now()): no hay forma de alinearlos con los frames. "
            "Regrábala con el cliente actual."
        )
    return [(int(e["at_ms"]) * 1000 - base, e) for e in eventos]


def read_recording(path: pathlib.Path):
    """Devuelve (cabecera, [jpeg, ...])."""
    raw = path.read_bytes()
    nl = raw.index(b"\n")
    header = json.loads(raw[:nl])

    frames = []
    offset = nl + 1
    while offset < len(raw):
        (length,) = struct.unpack_from(">I", raw, offset)
        offset += 4
        frames.append(raw[offset : offset + length])
        offset += length
    return header, frames


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("recording", type=pathlib.Path)
    parser.add_argument(
        "--signals",
        default="gaze_offset_x,gaze_offset_y,gaze_agreement,pose_yaw_deg,pose_pitch_deg,face_area_ratio",
        help="señales a mostrar, separadas por coma",
    )
    args = parser.parse_args()

    header, frames = read_recording(args.recording)
    wanted = [s.strip() for s in args.signals.split(",") if s.strip()]

    print(f"grabación: {args.recording.name}")
    print(f"  sesión   {header.get('session_id')}")
    print(f"  frames   {len(frames)}")
    print(f"  captura  {header.get('capture')}")
    print(f"  resultado{header.get('result')}")

    # El detector se resuelve con rutas relativas al analizador.
    import os

    os.chdir(ROOT / "analyzer")

    import logging

    from analyzer.config import load
    from analyzer.metrics import LatencyBudget
    from analyzer.pipeline import Pipeline
    from analyzer.sessions import SessionState
    from analyzer.worker import build_detector

    from analyzer import wire

    detector = build_detector(load())
    pipeline = Pipeline(detector, budget=LatencyBudget("replay", 30.0, logging.getLogger("replay")))
    state = SessionState("replay", 0, time.monotonic(), time.monotonic())

    # Retos, en orden. El cliente los anotó con su reloj; los frames llevan el
    # suyo de captura. Se alinean por posición temporal relativa.
    events = [e for e in header.get("events", []) if e.get("kind") == "challenge"]
    print(f"\nretos anotados: {len(events)}")
    for e in events:
        d = e["detail"]
        extra = json.dumps(d.get("params", {}), ensure_ascii=False)
        print(f"  t={e['at_ms']:6} ms  {d['kind']:12} {extra}")

    print(f"\n{'#':>4} {'ms':>7}  " + "  ".join(f"{s:>16}" for s in wanted))
    t0 = None
    for i, jpeg in enumerate(frames, start=1):
        task = wire.FrameTask(seq=i, encoding=1, payload=jpeg, received_at_us=i * 66_000)
        try:
            signals = pipeline.process(task, state).features.signals
        except Exception as exc:  # noqa: BLE001 - un frame ilegible no para el análisis
            print(f"{i:4} frame ilegible: {exc}")
            continue

        if t0 is None:
            t0 = 0
        cells = []
        for name in wanted:
            value = signals.get(name)
            cells.append("       —" if value is None else f"{value:16.4f}")
        print(f"{i:4} {i * 66:7}  " + "  ".join(cells))

    detector.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
