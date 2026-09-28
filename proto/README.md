# proto — contratos compartidos

Fuente única de verdad de todo lo que cruza un límite de proceso.

```
nats/v1/   mensajes del bus Go ⇄ Python (protobuf)
ws/v1/     protocolo WebSocket cliente ⇄ gateway (JSON Schema)
subjects.md  mapa de subjects NATS
```

## Reglas

- Nada que viaje entre procesos se define fuera de aquí.
- `v1` es inmutable: sólo se añaden campos opcionales con número nuevo.
  Un cambio incompatible crea `v2`.
- Los mensajes hacia Python **no pueden** contener estado de sesión, retos,
  umbrales ni veredictos (ver `CLAUDE.md` §3).
- Los mensajes hacia el cliente **no pueden** revelar el guion de retos
  (ver `CLAUDE.md` §6).

## Codificación en el cable

Los `.proto` son el **esquema de referencia**: definen qué campos existen, con
qué nombre y qué significan. Todavía NO se generan: hoy los dos lados hablan
una codificación equivalente escrita a mano.

| Mensaje | Codificación actual | Por qué |
|---------|--------------------|---------|
| `FrameTask` | binario, cabecera de 32 bytes + payload | Es el camino caliente. En JSON el payload iría en base64: un tercio más de red por frame. |
| Todo lo demás | JSON | Bajo caudal, y así el worker de Python lo lee sin generar código. |

Las dos implementaciones de esa codificación —`gateway/internal/bus/codec.go`
y `analyzer/src/analyzer/wire.py`— están sujetas por vectores dorados en
`proto/testdata/`: cada lado comprueba que decodifica exactamente lo que el
otro escribió. Si alguien cambia una y no la otra, los tests lo cazan.

### Cuando se genere código

- Go     → `protoc-gen-go` a `gateway/internal/pb`.
- Python → `grpcio-tools` a `analyzer/src/analyzer/pb`.

Es un cambio coordinado de los dos lados: hay que cambiar la codificación,
regenerar, actualizar los vectores dorados y desplegar a la vez. El código
generado no se edita a mano.
