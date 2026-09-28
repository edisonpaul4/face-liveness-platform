"""Ejecutor del banco de pruebas.

Cliente externo: habla con la plataforma únicamente por su API pública y su
WebSocket, igual que un atacante real.

SCAFFOLDING: sin implementación.
"""

from __future__ import annotations

import sys


def main(argv: list[str]) -> int:
    """Ejecuta los casos indicados y reporta las métricas PAD."""
    del argv
    print("scaffolding: runner no implementado")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
