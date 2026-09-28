#!/usr/bin/env python3
"""Descarga muestras públicas de datasets de ataques de presentación.

Las descarga FUERA del repositorio, a `~/datasets/liveness/` por defecto. Son
biometría de personas reales y no pueden vivir bajo control de versiones
(CLAUDE.md §9, regla 6).

Cada dataset trae su licencia. Las muestras de Hugging Face que se descargan
aquí son **no comerciales**: sirven para medir, no para entrenar un modelo que
se vaya a vender. Comprobarlo es responsabilidad de quien lo use.

    python3 bench/runner/fetch_datasets.py            # las públicas
    python3 bench/runner/fetch_datasets.py --list     # qué hay y de dónde
"""

from __future__ import annotations

import argparse
import pathlib
import sys

#: Muestras públicas en Hugging Face, sin credenciales ni acuerdo previo.
#:
#: Son MUESTRAS: decenas de archivos, no miles. Bastan para comprobar que un
#: detector separa, no para calibrar umbrales ni para certificar nada.
PUBLIC = {
    "axon-fas": {
        "repo": "AxonData/face-anti-spoofing-dataset",
        "que": "selfies + replay (pantalla y móvil), recortes, máscaras de papel, látex, silicona y textil",
        "licencia": "no comercial — ver la ficha del dataset",
    },
    "axon-paper": {
        "repo": "AxonData/face-anti-spoofing-advanced-paper-attacks",
        "que": "ataques de papel avanzados, del tipo que usa iBeta nivel 2",
        "licencia": "no comercial — ver la ficha del dataset",
    },
    "axon-replay": {
        "repo": "AxonData/replay-attack-dataset",
        "que": "replay en pantalla",
        "licencia": "no comercial — ver la ficha del dataset",
    },
}

#: Los que hay que pedir. Se listan para que estén en un sitio y no en la
#: cabeza de nadie.
GATED = {
    "OULU-NPU": "5.940 vídeos, 55 sujetos, 6 móviles. El más parecido a una webcam. Acuerdo por correo.",
    "CASIA-FASD": "600 vídeos, 50 sujetos: foto deformada, foto recortada, replay. Acuerdo por correo.",
    "Replay-Attack (Idiap)": "300 vídeos, 50 sujetos: print y replay. EULA de Idiap.",
    "CelebA-Spoof": "625k imágenes, 10k sujetos. Google Drive en 74 partes, con cuota que suele estar agotada.",
    "LCC-FASD / NUAA": "en Kaggle: hace falta ~/.kaggle/kaggle.json.",
}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dest", type=pathlib.Path, default=pathlib.Path.home() / "datasets" / "liveness")
    parser.add_argument("--list", action="store_true", help="sólo enumera, no descarga")
    parser.add_argument("--only", help="descarga sólo este")
    args = parser.parse_args()

    if args.list:
        print("públicos, se descargan sin credenciales:\n")
        for key, spec in PUBLIC.items():
            print(f"  {key:14} {spec['repo']}")
            print(f"  {'':14} {spec['que']}")
            print(f"  {'':14} {spec['licencia']}\n")
        print("hay que pedirlos:\n")
        for name, note in GATED.items():
            print(f"  {name:24} {note}")
        return 0

    try:
        from huggingface_hub import snapshot_download
    except ImportError:
        print("falta huggingface_hub: pip install huggingface_hub")
        return 1

    args.dest.mkdir(parents=True, exist_ok=True)
    for key, spec in PUBLIC.items():
        if args.only and key != args.only:
            continue
        target = args.dest / key
        print(f"\n{key} ← {spec['repo']}")
        try:
            snapshot_download(
                spec["repo"], repo_type="dataset", local_dir=str(target),
                # Sin los .gitattributes y demás ruido del repositorio.
                ignore_patterns=[".gitattributes"],
            )
            n = sum(1 for p in target.rglob("*") if p.is_file())
            print(f"  {n} archivos en {target}")
        except Exception as exc:  # noqa: BLE001
            print(f"  no se pudo: {type(exc).__name__}: {str(exc)[:200]}")

    print(f"\nlisto. Para medir:\n  make bench-dataset DS={args.dest}/axon-fas")
    return 0


if __name__ == "__main__":
    sys.exit(main())
