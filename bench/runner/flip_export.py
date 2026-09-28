#!/usr/bin/env python3
"""Convierte un checkpoint de FLIP a lo que el analizador sabe cargar: ONNX.

FLIP (ICCV 2023) es CLIP ViT-B/16 afinado para anti-spoofing con guía de
lenguaje. Su inferencia no es un clasificador al uso: compara la imagen contra
dos conjuntos de frases —seis que describen un rostro real y seis que
describen un ataque— y se queda con la más parecida.

Eso permite un artefacto pequeño y sin PyTorch en producción:

  1. El codificador de IMAGEN se exporta a ONNX.
  2. Las doce frases se codifican UNA vez aquí y se guardan como una matriz de
     2x512. En producción es un producto escalar, no un modelo de lenguaje.

El analizador queda como está: ONNX Runtime y nada más.

    python3 bench/runner/flip_export.py oulu_flip_mcl.pth.tar --out analyzer/models

Sin checkpoint exporta CLIP sin afinar, que sirve para validar la cadena y
como línea base: si FLIP no le gana a su propio punto de partida, no aporta.
"""

from __future__ import annotations

import argparse
import pathlib
import sys

#: Las frases del repositorio de FLIP (`prompt_templates.py`), literales.
SPOOF_TEMPLATES = [
    "This is an example of a spoof face",
    "This is an example of an attack face",
    "This is not a real face",
    "This is how a spoof face looks like",
    "a photo of a spoof face",
    "a printout shown to be a spoof face",
]
REAL_TEMPLATES = [
    "This is an example of a real face",
    "This is a bonafide face",
    "This is a real face",
    "This is how a real face looks like",
    "a photo of a real face",
    "This is not a spoof face",
]

#: Normalización de FLIP: **ImageNet, no CLIP**.
#:
#: Se comprueba en `utils/dataset.py` del repositorio original. Es el error
#: más fácil de cometer aquí, porque casi todo el código que usa CLIP aplica
#: las constantes de CLIP (0.4815/0.2686...) y nadie lo mira dos veces.
IMAGENET_MEAN = [0.485, 0.456, 0.406]
IMAGENET_STD = [0.229, 0.224, 0.225]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("checkpoint", nargs="?", type=pathlib.Path)
    parser.add_argument("--out", type=pathlib.Path, default=pathlib.Path("analyzer/models"))
    parser.add_argument("--name", default="flip")
    args = parser.parse_args()

    import clip
    import numpy as np
    import torch

    model, _ = clip.load("ViT-B/16", device="cpu")
    model = model.float().eval()

    if args.checkpoint:
        raw = torch.load(args.checkpoint, map_location="cpu", weights_only=False)
        state = raw.get("state_dict", raw)
        # flip_mcl envuelve el CLIP entero en `self.model`; el resto del
        # state_dict es la cabeza de entrenamiento auto-supervisado, que no
        # interviene en la inferencia.
        inner = {k[len("model.") :]: v for k, v in state.items() if k.startswith("model.")}
        if not inner:
            inner = {k[len("module.model.") :]: v for k, v in state.items() if k.startswith("module.model.")}
        if not inner:
            print("no se reconocieron pesos de CLIP en el checkpoint.", file=sys.stderr)
            print(f"claves de ejemplo: {list(state)[:6]}", file=sys.stderr)
            return 1
        missing, unexpected = model.load_state_dict(inner, strict=False)
        print(f"pesos cargados · sin asignar {len(missing)} · sobrantes {len(unexpected)}")
        if missing:
            print(f"  sin asignar (primeros): {missing[:5]}")
    else:
        print("SIN checkpoint: se exporta CLIP sin afinar como línea base.")

    args.out.mkdir(parents=True, exist_ok=True)

    # --- las frases, codificadas de una vez ---------------------------------
    with torch.no_grad():
        def encode(templates: list[str]) -> torch.Tensor:
            tokens = clip.tokenize(templates)
            feats = model.encode_text(tokens).float()
            feats = feats / feats.norm(dim=-1, keepdim=True)
            # Media del conjunto de frases, renormalizada: es el "prompt
            # ensemble" que usa FLIP, no una frase suelta.
            mean = feats.mean(dim=0)
            return mean / mean.norm()

        # Fila 0 = real, fila 1 = ataque. El orden importa y se documenta
        # aquí porque en producción sólo se publica la fila 1: concluir que
        # NO hay ataque es decidir, y decide Go (CLAUDE.md §3).
        text = torch.stack([encode(REAL_TEMPLATES), encode(SPOOF_TEMPLATES)]).numpy()

    np.save(args.out / f"{args.name}_text.npy", text.astype(np.float32))
    print(f"frases → {args.out / f'{args.name}_text.npy'}  {text.shape}")

    # --- el codificador de imagen -------------------------------------------
    class Visual(torch.nn.Module):
        """Sólo la rama de imagen, con la salida ya normalizada."""

        def __init__(self, clip_model) -> None:  # noqa: ANN001
            super().__init__()
            self.clip = clip_model

        def forward(self, pixels: torch.Tensor) -> torch.Tensor:
            feats = self.clip.encode_image(pixels)
            return feats / feats.norm(dim=-1, keepdim=True)

    onnx_path = args.out / f"{args.name}_vision.onnx"
    dummy = torch.zeros(1, 3, 224, 224)
    torch.onnx.export(
        Visual(model).eval(),
        (dummy,),
        str(onnx_path),
        input_names=["pixels"],
        output_names=["features"],
        opset_version=17,
        dynamo=False,
    )
    print(f"visión → {onnx_path}  ({onnx_path.stat().st_size / 1e6:.0f} MB)")

    print("\npreprocesado que espera (de utils/dataset.py del repo original):")
    print(f"  redimensionar a 224x224, RGB, escala 0-1")
    print(f"  normalizar con media {IMAGENET_MEAN} y desviación {IMAGENET_STD}")
    print("  ImageNet, NO las constantes de CLIP.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
