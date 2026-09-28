#!/usr/bin/env python3
"""Recorre un dataset y saca la matriz APCER/BPCER por detector y por ataque.

Sin esto, un dataset son miles de informes sueltos. Lo que hace falta saber es
otra cosa: **qué detector pilla qué ataque, y a costa de rechazar a cuánta
gente real**.

Las dos métricas son las de ISO/IEC 30107-3:

* **APCER** — presentaciones de ataque que el detector deja pasar como
  legítimas. Se calcula POR TIPO de ataque, porque un detector que para el
  papel y se traga las pantallas no es "medio bueno": es inservible contra
  pantallas, y promediarlo lo escondería.
* **BPCER** — personas reales rechazadas. Es el coste, y sin él un APCER del
  0 % se consigue rechazando a todo el mundo.

Estructura esperada del directorio, por nombre de carpeta:

    dataset/
      live/            o real/, bonafide/, genuine/
      spoof/print/     o attack/print/, cualquier nivel
      spoof/replay/

El nombre de la subcarpeta bajo `spoof` es el tipo de ataque. Si no hay
subcarpetas, todo el ataque cuenta como un solo tipo.

    python3 bench/runner/bench_dataset.py ~/datasets/lcc-fasd
    python3 bench/runner/bench_dataset.py ~/datasets/nuaa --limit 200
"""

from __future__ import annotations

import argparse
import json
import pathlib
import re
import sys
from collections import defaultdict

ROOT = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "bench" / "runner"))

MEDIA_SUFFIXES = {".jpg", ".jpeg", ".png", ".bmp", ".webp", ".mp4", ".avi", ".mov", ".mkv"}

#: Nombres de carpeta que significan "persona real".
LIVE_NAMES = {"live", "real", "bonafide", "bona_fide", "genuine", "client", "positive"}
#: Nombres que significan "ataque".
SPOOF_NAMES = {"spoof", "attack", "fake", "imposter", "impostor", "negative"}
#: Pistas de que una carpeta de primer nivel contiene personas reales. Se
#: buscan como subcadena porque los datasets las nombran de mil formas.
LIVE_HINTS = ("selfie", "live", "real", "bonafide", "bona_fide", "genuine", "client")

#: Detectores pasivos que se evalúan, y en qué dirección delatan.
#:
#: `alto` significa que un valor ALTO indica ataque. El umbral es el punto de
#: corte con el que se cuenta; el runner además busca el mejor por su cuenta,
#: porque el que traemos puesto salió de una sola cara.
DETECTORS = {
    # Los umbrales de los dos clasificadores NO son 0,5 aunque salgan de un
    # softmax. La clase "ataque" de MiniFASNet reparte casi toda su masa por
    # debajo de la milésima, así que un corte en 0,5 no dispara nunca: medido,
    # dejaba pasar entre el 44 % y el 100 % de cada familia de ataque mientras
    # aparentaba un BPCER del 0 %, que es la forma más engañosa de estar roto.
    #
    # Estos salen de `best_threshold` sobre el banco completo. Con partición
    # reservada (--holdout) el rendimiento que cabe esperar es peor; ahí está
    # la estimación honesta.
    "texture_pad_v2_a": {"alto": True, "umbral": 0.0085, "agg": "max"},
    "texture_pad_v1se_a": {"alto": True, "umbral": 0.0016, "agg": "max"},
    "surface_moire_index": {"alto": True, "umbral": 0.017, "agg": "max"},
    "surface_banding_index": {"alto": True, "umbral": 0.040, "agg": "max"},
    # `surface_specular_fraction` NO está aquí, y no es un olvido. Medido con
    # partición reservada deja pasar entre el 62 % y el 91 % de los ataques:
    # no separa. Se sigue emitiendo como medida —es una propiedad real de la
    # imagen y puede servir dentro de otra señal— pero presentarla como
    # detector le diría a quien prueba el sistema que hay una defensa donde no
    # la hay.
}


def classify(path: pathlib.Path, root: pathlib.Path) -> tuple[str, str]:
    """Devuelve (etiqueta, tipo de ataque) a partir de la ruta.

    Tres convenciones, porque los datasets no se ponen de acuerdo:

    1. `live/` y `spoof/<tipo>/` explícitos, en cualquier nivel.
    2. Una carpeta por clase en la raíz, con el nombre del ataque. Es lo que
       usan las muestras de Hugging Face: `Selfies/`, `Replay_mobile_attacks/`,
       `Silicone_mask/`… Todo lo que no suene a persona real se toma como
       ataque, con el nombre de la carpeta como especie.

    3. La etiqueta en el NOMBRE del fichero, que es lo que hace CASIA-FASD:
       `<sujeto>_<video>.avi_<frame>_<real|fake>.jpg`. Y el número de vídeo
       codifica además el tipo de ataque, así que se puede desglosar sin
       carpetas.

    Lo segundo es una heurística, y por eso el runner imprime el reparto antes
    de medir: si una carpeta cayó del lado que no era, se ve enseguida.
    """
    if (etiquetado := _casia_fasd(path)) is not None:
        return etiquetado

    relative = path.relative_to(root).parts[:-1]
    parts = [p.strip().lower() for p in relative]

    for i, part in enumerate(parts):
        if part in LIVE_NAMES:
            return "live", ""
        if part in SPOOF_NAMES:
            # El siguiente nivel, si lo hay, es el tipo de ataque.
            especie = parts[i + 1] if i + 1 < len(parts) else "sin_especificar"
            return "spoof", especie

    if not parts:
        return "desconocido", ""

    top = parts[0]
    if top.startswith("."):
        return "desconocido", ""
    if any(hint in top for hint in LIVE_HINTS):
        return "live", ""
    return "spoof", relative[0].strip()


#: Numeración de CASIA-FASD: el número de vídeo dice qué ataque es.
#:
#: Cada sujeto se grabó doce veces, en tres calidades × cuatro condiciones.
#: Sin este mapa el dataset entero sería un solo montón de "spoof" y se
#: perdería lo único que lo hace útil: que separa foto impresa de pantalla.
CASIA_SPECIES = {
    "1": "", "2": "", "HR_1": "",  # reales
    "3": "foto_doblada", "4": "foto_doblada", "HR_2": "foto_doblada",
    "5": "foto_recortada", "6": "foto_recortada", "HR_3": "foto_recortada",
    "7": "replay_pantalla", "8": "replay_pantalla", "HR_4": "replay_pantalla",
}

_CASIA_NAME = re.compile(r"^(\d+)_(HR_\d|\d+)\.avi_\d+_(real|fake)\.jpg$")


def _casia_fasd(path: pathlib.Path) -> tuple[str, str] | None:
    """Etiqueta y especie de un frame de CASIA-FASD, o None si no lo es."""
    m = _CASIA_NAME.match(path.name)
    if m is None:
        return None
    _, video, label = m.groups()
    if label == "real":
        return "live", ""
    return "spoof", CASIA_SPECIES.get(video, f"video_{video}")


def casia_subject(path: pathlib.Path) -> str | None:
    """Sujeto al que pertenece un frame, para no mezclarlo entre particiones.

    Importa mucho más de lo que parece: hay ~160 frames por sujeto, y tratarlos
    como muestras independientes infla la potencia estadística por un factor de
    cien. Cincuenta sujetos son cincuenta muestras, no ocho mil.
    """
    m = _CASIA_NAME.match(path.name)
    return m.group(1) if m else None


def sujeto_de(path: pathlib.Path) -> str:
    """A quién pertenece un archivo, para no partirlo entre mitades.

    CASIA-FASD lo lleva en el nombre. Cuando no se puede saber, cada archivo
    es su propio sujeto: es lo conservador —equivale a la partición por
    fichero de antes— y nunca junta a dos personas por error.
    """
    return casia_subject(path) or str(path)


def collect(root: pathlib.Path, limit: int | None) -> list[tuple[pathlib.Path, str, str]]:
    files = []
    for path in sorted(root.rglob("*")):
        if not path.is_file() or path.suffix.lower() not in MEDIA_SUFFIXES:
            continue
        label, species = classify(path, root)
        if label == "desconocido":
            continue
        files.append((path, label, species))

    if limit is None:
        return files

    # Se recorta por grupo, no del total: quedarse con los primeros N
    # ordenados alfabéticamente dejaría fuera clases enteras.
    por_grupo: dict[tuple[str, str], list] = defaultdict(list)
    for item in files:
        por_grupo[(item[1], item[2])].append(item)
    out = []
    for grupo in por_grupo.values():
        out.extend(grupo[:limit])
    return out


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("dataset", type=pathlib.Path)
    parser.add_argument("--limit", type=int, default=None, help="máximo de archivos por clase")
    parser.add_argument("--json", type=pathlib.Path, help="guarda los valores crudos")
    parser.add_argument("--fit", default="crop", choices=("crop", "letterbox"),
                        help="cómo encajar frames de otra proporción; cambia el resultado")
    parser.add_argument("--agg", choices=("median", "p75", "p90", "max"),
                        help="cómo resumir los frames de un archivo en un valor")
    parser.add_argument(
        "--holdout",
        action="store_true",
        help=(
            "parte los datos en dos: elige el umbral en una mitad y lo mide "
            "en la otra. Sin esto, el 'mejor umbral' está elegido sobre los "
            "mismos datos con los que se mide y no predice nada."
        ),
    )
    parser.add_argument(
        "--seed",
        type=int,
        default=0,
        help="semilla de la partición. Cambiarla mueve el resultado, y cuánto lo mueve es el dato.",
    )
    args = parser.parse_args()

    root = args.dataset.expanduser().resolve()
    if not root.is_dir():
        print(f"no es un directorio: {root}")
        return 1

    files = collect(root, args.limit)
    if not files:
        print(f"sin archivos reconocibles en {root}")
        print("se esperan carpetas live/ y spoof/<tipo>/ en algún nivel")
        return 1

    grupos: dict[tuple[str, str], int] = defaultdict(int)
    for _, label, species in files:
        grupos[(label, species)] += 1
    print(f"dataset: {root}   encaje: {args.fit}")
    for (label, species), n in sorted(grupos.items()):
        print(f"  {label:8} {species or '—':16} {n:5} archivos")
    print()

    # El aviso de presupuesto por frame es para el camino en vivo; aquí sólo
    # ensucia la salida. Lo que sí importa es la latencia real, y de eso ya
    # informan las métricas del worker.
    import logging

    logging.getLogger("analyzer.metrics").setLevel(logging.ERROR)

    if args.agg:
        for spec in DETECTORS.values():
            spec["agg"] = args.agg

    from analyze_media import analyze

    # crudos[detector][(etiqueta, especie)] = lista de resúmenes por archivo,
    # cada uno con min/median/p75/p90/max. Se guardan TODOS para poder
    # comparar formas de agregar sin volver a analizar: cada pasada completa
    # son cinco minutos.
    crudos: dict[str, dict[tuple[str, str], list[tuple[dict, str]]]] = defaultdict(
        lambda: defaultdict(list)
    )
    sin_rostro = 0

    for i, (path, label, species) in enumerate(files, start=1):
        if i % 25 == 0 or i == len(files):
            print(f"  analizando {i}/{len(files)}…", flush=True)
        try:
            report = analyze(path, fit=args.fit)
        except Exception as exc:  # noqa: BLE001 - un archivo malo no para el banco
            print(f"    {path.name}: {exc}")
            continue
        if not report["frames_with_face"]:
            sin_rostro += 1
            continue
        for name in DETECTORS:
            stats = report["signals"].get(name)
            if stats is None:
                continue
            # Se guarda junto al SUJETO. Sin eso, la partición reservada
            # parte por fichero y la misma persona cae en las dos mitades:
            # el "held-out" deja de serlo y las cifras salen infladas.
            crudos[name][(label, species)].append((stats, sujeto_de(path)))

    print()
    if sin_rostro:
        # Importa y mucho: un ataque en el que no se detecta rostro no lo para
        # el PAD, lo para el detector de caras. Contarlo como acierto del PAD
        # sería atribuirle un mérito que no es suyo.
        print(f"⚠ {sin_rostro} archivos sin rostro detectado, excluidos del cálculo\n")

    especies = sorted({s for (lab, s) in grupos if lab == "spoof"})

    # Cómo resumir los frames de un archivo NO es un detalle. La evidencia de
    # ataque es esparsa en el tiempo —sólo algunos frames delatan—, así que la
    # mediana la diluye; y el máximo deja que un frame malo condene a una
    # persona real. Los percentiles altos son el punto medio, y cuál gana se
    # decide midiendo.
    print("cómo se resumen los frames de cada archivo\n")
    print(f"{'detector':26}{'agregación':>12}{'umbral':>10}{'APCER':>10}{'BPCER':>10}")
    print("-" * 68)
    for name, spec in DETECTORS.items():
        grupos_det = crudos.get(name)
        if not grupos_det:
            continue
        for agg in ("median", "p75", "p90", "max"):
            vivos = [st[agg] for st, _ in grupos_det.get(("live", ""), [])]
            ataques = [st[agg] for esp in especies for st, _ in grupos_det.get(("spoof", esp), [])]
            t, apcer, bpcer = best_threshold(vivos, ataques, spec["alto"])
            if t is None:
                continue
            print(f"{name if agg == 'median' else '':26}{agg:>12}{t:>10.4f}"
                  f"{apcer*100:>9.1f}%{bpcer*100:>9.1f}%")
        print()

    # valores[detector][(etiqueta, especie)] = [(valor, sujeto), ...]
    valores = {
        name: {clave: [(st[DETECTORS[name]["agg"]], sujeto) for st, sujeto in lista]
               for clave, lista in grupos_det.items()}
        for name, grupos_det in crudos.items()
    }
    render(valores, especies)
    if args.holdout:
        render_holdout(valores, especies, args.seed)

    if args.json:
        crudo = {
            det: {f"{lab}/{sp}" if sp else lab: vals for (lab, sp), vals in grupos_det.items()}
            for det, grupos_det in crudos.items()
        }
        args.json.write_text(json.dumps(crudo, ensure_ascii=False, indent=1))
        print(f"\nvalores crudos en {args.json}")
    return 0


def rates(vals_live: list[float], vals_spoof: list[float], umbral: float, alto: bool):
    """APCER y BPCER a un umbral dado."""
    if alto:
        # Ataque si el valor supera el umbral.
        apcer = sum(1 for v in vals_spoof if v < umbral) / len(vals_spoof) if vals_spoof else None
        bpcer = sum(1 for v in vals_live if v >= umbral) / len(vals_live) if vals_live else None
    else:
        apcer = sum(1 for v in vals_spoof if v > umbral) / len(vals_spoof) if vals_spoof else None
        bpcer = sum(1 for v in vals_live if v <= umbral) / len(vals_live) if vals_live else None
    return apcer, bpcer


def split_halves(
    samples: list[tuple[float, str]], seed: int
) -> tuple[list[float], list[float]]:
    """Parte las muestras en dos mitades **por SUJETO**, no por fichero.

    Ésta es la diferencia entre una cifra citable y una inflada. En CASIA-FASD
    hay unos 270 frames por persona: partiendo por fichero, la misma cara cae
    en las dos mitades y el modelo ya la ha visto cuando se le "reserva". Lo
    que mide entonces no es generalización, es memoria.

    Partiendo por sujeto, la mitad reservada son personas que no aparecen en
    la otra. Es lo que de verdad se quiere saber.

    Consecuencia que hay que aceptar: las dos mitades no salen del mismo
    tamaño, porque los sujetos no aportan el mismo número de muestras. Es
    correcto — forzarlas a ser iguales volvería a mezclarlos.
    """
    if len(samples) < 2:
        valores = [v for v, _ in samples]
        return valores, valores

    sujetos = sorted({s for _, s in samples})
    if len(sujetos) < 2:
        # Un solo sujeto: no hay partición honesta posible. Se devuelve lo
        # mismo a los dos lados y el resultado se lee sabiendo eso.
        valores = [v for v, _ in samples]
        return valores, valores

    order = _permutation(len(sujetos), seed)
    barajados = [sujetos[i] for i in order]
    corte = len(barajados) // 2
    primera = set(barajados[:corte])

    a = [v for v, s in samples if s in primera]
    b = [v for v, s in samples if s not in primera]
    return a, b


def _permutation(n: int, seed: int) -> list[int]:
    """Permutación reproducible sin depender del PRNG de la biblioteca."""
    state = seed * 6364136223846793005 + 1442695040888963407
    order = list(range(n))
    for i in range(n - 1, 0, -1):
        state = (state * 6364136223846793005 + 1442695040888963407) & ((1 << 64) - 1)
        j = (state >> 33) % (i + 1)
        order[i], order[j] = order[j], order[i]
    return order


def holdout_rates(
    vals_live: list[tuple[float, str]],
    vals_spoof: list[tuple[float, str]],
    alto: bool,
    seed: int,
) -> tuple[float | None, float | None, float | None]:
    """Elige el umbral en una mitad y lo mide en la otra.

    Devuelve (umbral, APCER, BPCER) sobre la mitad reservada. Es la única cifra
    de esta herramienta que se puede citar como estimación: el resto están
    elegidas sobre los mismos datos que miden.
    """
    live_a, live_b = split_halves(vals_live, seed)
    spoof_a, spoof_b = split_halves(vals_spoof, seed + 1)
    umbral, _, _ = best_threshold(live_a, spoof_a, alto)
    if umbral is None:
        return None, None, None
    apcer, bpcer = rates(live_b, spoof_b, umbral, alto)
    return umbral, apcer, bpcer


def best_threshold(vals_live: list[float], vals_spoof: list[float], alto: bool):
    """Umbral que minimiza APCER+BPCER. Es orientativo, no una calibración.

    Elegir el umbral sobre los mismos datos con los que se mide infla el
    resultado: para calibrar de verdad hace falta partición aparte.
    """
    if not vals_live or not vals_spoof:
        return None, None, None
    candidatos = sorted(set(vals_live + vals_spoof))
    mejor = (2.0, None, None, None)
    for t in candidatos:
        apcer, bpcer = rates(vals_live, vals_spoof, t, alto)
        if apcer is None or bpcer is None:
            continue
        if apcer + bpcer < mejor[0]:
            mejor = (apcer + bpcer, t, apcer, bpcer)
    return mejor[1], mejor[2], mejor[3]


def sin_sujeto(muestras: list[tuple[float, str]]) -> list[float]:
    """Quita el sujeto. Sólo para lo que NO parte en mitades."""
    return [v for v, _ in muestras]


def todos_los_ataques(grupos, especies) -> list[float]:
    """Junta los valores de todas las especies de ataque."""
    out: list[float] = []
    for esp in especies:
        out.extend(grupos.get(("spoof", esp), []))
    return out


def render_holdout(valores, especies, seed: int) -> None:
    """APCER/BPCER sobre la mitad reservada, con el umbral de la otra mitad.

    Se imprimen varias semillas a propósito. Con un banco de dos docenas de
    caras reales, una sola partición dice más de qué caras cayeron dónde que
    del detector; ver el recorrido entre semillas es lo que impide leer una
    cifra afortunada como si fuera el rendimiento del sistema.
    """
    ancho = max(len(n) for n in DETECTORS) + 2
    print("\n\nCON PARTICIÓN RESERVADA")
    print("umbral elegido en una mitad, medido en la OTRA. Cinco semillas.\n")
    print(f"{'detector':{ancho}}{'umbral':>18}{'APCER':>18}{'BPCER':>18}")
    print("-" * (ancho + 54))

    for name, spec in DETECTORS.items():
        grupos = valores.get(name)
        if not grupos:
            continue
        vivos = grupos.get(("live", ""), [])
        ataques = todos_los_ataques(grupos, especies)

        umbrales, apcers, bpcers = [], [], []
        for offset in range(5):
            t, apcer, bpcer = holdout_rates(vivos, ataques, spec["alto"], seed + offset * 2)
            if t is None or apcer is None or bpcer is None:
                continue
            umbrales.append(t)
            apcers.append(apcer)
            bpcers.append(bpcer)
        if not apcers:
            continue

        def span(vals: list[float], pct: bool) -> str:
            lo, hi = min(vals), max(vals)
            if pct:
                return f"{lo*100:.1f}-{hi*100:.1f}%" if lo != hi else f"{lo*100:.1f}%"
            # Los umbrales van en cifras significativas, no en decimales
            # fijos: el de un clasificador vive en 0,004 y el de una medida
            # de textura en 3. Con dos decimales, el primero se imprime como
            # cero y parece que no hay umbral.
            return f"{lo:.3g}-{hi:.3g}" if lo != hi else f"{lo:.3g}"

        print(
            f"{name:{ancho}}{span(umbrales, False):>18}"
            f"{span(apcers, True):>18}{span(bpcers, True):>18}"
        )

    vivos_all = valores[next(iter(DETECTORS))].get(("live", ""), [])
    n_live = len(vivos_all)
    n_suj = len({s for _, s in vivos_all})
    print(
        f"\n  Partido por SUJETO: {n_suj} personas reales ({n_live} muestras). La mitad\n"
        f"  reservada son ~{n_suj // 2} personas que el umbral no ha visto.\n"
        "  Es el n que cuenta: los frames de una misma cara no son independientes."
    )


def render(valores, especies) -> None:
    if not valores:
        print("ningún detector produjo valores")
        return

    ancho = 26
    print("APCER por tipo de ataque, al umbral configurado")
    print("(ataques que el detector DEJA PASAR: menos es mejor)\n")
    cabecera = "detector".ljust(ancho) + "".join(e[:12].rjust(14) for e in especies) + "BPCER".rjust(14)
    print(cabecera)
    print("-" * len(cabecera))

    for name, spec in DETECTORS.items():
        grupos = valores.get(name)
        if not grupos:
            continue
        vivos = grupos.get(("live", ""), [])
        fila = name.ljust(ancho)
        for esp in especies:
            ataques = grupos.get(("spoof", esp), [])
            apcer, _ = rates(sin_sujeto(vivos), sin_sujeto(ataques), spec["umbral"], spec["alto"])
            fila += ("—" if apcer is None else f"{apcer*100:.1f}%").rjust(14)
        _, bpcer = rates(sin_sujeto(vivos), sin_sujeto(todos_los_ataques(grupos, especies)),
                         spec["umbral"], spec["alto"])
        fila += ("—" if bpcer is None else f"{bpcer*100:.1f}%").rjust(14)
        print(fila)

    print("\numbral que mejor separa en ESTOS datos (orientativo, no calibración)")
    print(f"{'detector':{ancho}}{'puesto':>10}{'mejor':>10}{'APCER':>10}{'BPCER':>10}")
    print("-" * (ancho + 40))
    for name, spec in DETECTORS.items():
        grupos = valores.get(name)
        if not grupos:
            continue
        vivos = grupos.get(("live", ""), [])
        ataques = todos_los_ataques(grupos, especies)
        t, apcer, bpcer = best_threshold(sin_sujeto(vivos), sin_sujeto(ataques), spec["alto"])
        if t is None:
            continue
        print(f"{name:{ancho}}{spec['umbral']:>10.4g}{t:>10.4g}"
              f"{apcer*100:>9.1f}%{bpcer*100:>9.1f}%")


if __name__ == "__main__":
    raise SystemExit(main())
