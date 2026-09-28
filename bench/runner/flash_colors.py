#!/usr/bin/env python3
"""Mide la respuesta cromática real, color por color, sobre una grabación.

Responde a una pregunta que el banco sintético no puede: cuánta señal deja de
verdad cada color del destello en una cara concreta, con su piel, su cámara y
su habitación.

Alinea la línea de tiempo de retos que anotó el cliente —con la secuencia de
colores y sus duraciones— contra las señales que produce el analizador, y
resume por color.

    python3 bench/runner/flash_colors.py web/harness/recordings/session-....bin

Sobre el dato: la grabación es biometría (CLAUDE.md §6bis). Vive en el disco
de quien la grabó y no debería sobrevivir al ajuste para el que se tomó.
"""

from __future__ import annotations

import argparse
import json
import logging
import pathlib
import struct
import sys
import time

ROOT = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "analyzer"))
sys.path.insert(0, str(ROOT / "analyzer" / "src"))


def read_recording(path: pathlib.Path):
    raw = path.read_bytes()
    nl = raw.index(b"\n")
    header = json.loads(raw[:nl])
    frames, offset = [], nl + 1
    while offset < len(raw):
        (length,) = struct.unpack_from(">I", raw, offset)
        offset += 4
        frames.append(raw[offset : offset + length])
        offset += length
    return header, frames


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("recording", type=pathlib.Path)
    parser.add_argument("--fps", type=float, default=15.0)
    args = parser.parse_args()

    header, frames = read_recording(args.recording)
    events = [e for e in header.get("events", []) if e.get("kind") == "challenge"]
    flashes = [e for e in events if e["detail"]["kind"] == "flash"]
    if not flashes:
        print("la grabación no contiene ningún reto de destello")
        return 1

    import os

    os.chdir(ROOT / "analyzer")

    from analyzer.config import load
    from analyzer.metrics import LatencyBudget
    from analyzer.pipeline import Pipeline
    from analyzer.sessions import SessionState
    from analyzer.worker import build_detector

    from analyzer import wire

    pipeline = Pipeline(
        build_detector(load()),
        budget=LatencyBudget("colors", 30.0, logging.getLogger("colors")),
    )
    state = SessionState("replay", 0, time.monotonic(), time.monotonic())

    # Señales por frame.
    per_frame = []
    for i, jpeg in enumerate(frames, start=1):
        task = wire.FrameTask(seq=i, encoding=1, payload=jpeg, received_at_us=i * 66_000)
        try:
            per_frame.append(pipeline.process(task, state).features.signals)
        except Exception:  # noqa: BLE001
            per_frame.append({})

    # El cliente anota los retos con SU reloj; el frame 1 coincide con el
    # primer reto anotado, así que todo se referencia a él.
    origin = events[0]["at_ms"]
    step = 1000.0 / args.fps

    print(f"grabación: {args.recording.name}   {len(frames)} frames\n")
    por_color: dict[str, list[float]] = {}

    for flash in flashes:
        start = flash["at_ms"] - origin
        print(f"destello en t={start:.0f} ms")
        elapsed = 0.0
        for segment in flash["detail"]["params"]["sequence"]:
            color = segment["color"]
            duration = segment["duration_ms"]
            # Ventana del tramo, con margen por la latencia de pintado.
            first = int((start + elapsed + 120) / step)
            last = int((start + elapsed + duration) / step)
            elapsed += duration

            respuestas = []
            for idx in range(max(0, first), min(len(per_frame), last + 1)):
                s = per_frame[idx]
                valores = [s.get(f"color_response_{c}") for c in "rgb"]
                if any(v is None for v in valores):
                    continue
                # Cuánto se separa del reposo, en el canal que más se mueve.
                respuestas.append(max(abs(v - 0.45) for v in valores))
            if not respuestas:
                print(f"  {color:7} {duration:4} ms   sin frames medibles")
                continue
            pico = max(respuestas)
            por_color.setdefault(color, []).append(pico)
            print(f"  {color:7} {duration:4} ms   {len(respuestas):2} frames   respuesta {pico:.3f}")

    print("\nresumen por color (respuesta mayor = más señal deja en esta cara):")
    for color, valores in sorted(por_color.items(), key=lambda kv: -min(kv[1])):
        print(f"  {color:7} peor {min(valores):.3f}   media {sum(valores)/len(valores):.3f}"
              f"   ({len(valores)} tramos)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
