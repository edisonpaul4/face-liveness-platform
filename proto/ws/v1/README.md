# Protocolo WebSocket cliente ⇄ gateway (v1)

Dos canales sobre la misma conexión:

- **Texto (JSON)** — mensajes de control, validados contra
  `client_protocol.schema.json`.
- **Binario** — frames de cámara. Cabecera de 24 bytes big-endian seguida del
  payload codificado:

```
offset  0  uint8   version (=1)
offset  1  uint8   encoding (1=jpeg, 2=webp, 3=rgb24, 0xF0=escena sintética)
offset  2  uint16  flags (reservado, 0)
offset  4  uint64  frame_seq
offset 12  uint64  captured_at_us  ← reloj del CLIENTE
offset 20  uint32  longitud del payload
offset 24  ...     payload
```

**`captured_at_us` no puntúa.** El gateway sella cada frame con su propio
reloj al recibirlo, y ese sello es el único que decide si una respuesta llegó
dentro de la ventana. El del cliente sirve para alinear frames entre sí, y
sólo si su deriva contra el reloj del servidor resulta plausible; si no, se
ignora y se cuenta como anomalía.

`frame_seq` debe ser estrictamente creciente. Un frame repetido o retrasado se
descarta sin cerrar la sesión.

## Límites duros

El servidor impone y el cliente no negocia:

| Límite | Por defecto | Al superarlo |
|--------|-------------|--------------|
| Tamaño de frame (mensaje binario completo) | 512 KiB | cierre 4003 |
| Tamaño de mensaje de control | 4 KiB | cierre 4002 |
| Frames por segundo | 15 | se descarta el exceso |
| Duración de sesión | 45 s | veredicto no concluyente y cierre 4001 |
| Deriva del reloj del cliente | 30 s | se ignora el sello del cliente |

Si el cliente envía frames más rápido de lo que se analizan, el gateway
**descarta los intermedios y se queda con el más reciente**. Nunca acumula
cola: un buzón de un hueco, y cada descarte contado.

## Códigos de cierre

Toda desconexión lleva un código del rango privado, incluido el cierre
correcto: un 1000 pelado no distingue "sesión completada" de "se cerró la
pestaña".

| Código | Significado |
|--------|-------------|
| 4000 | sesión completada, el veredicto ya se envió |
| 4001 | presupuesto de sesión agotado |
| 4002 | violación de protocolo |
| 4003 | frame demasiado grande |
| 4004 | exceso de mensajes de control |
| 4005 | token ausente, desconocido o **ya consumido** |
| 4006 | error interno |
| 4007 | cliente lento: no consume lo que se le envía |
| 4008 | el cliente abandonó |

## Sesión de un solo uso

El `session_token` de `client_hello` se canjea una vez. Un `session_id` ya
consumido se rechaza siempre, aunque la sesión anterior fallara: reintentar
exige un ticket nuevo, y con él una semilla nueva y un guion nuevo. Si un
atacante pudiera reengancharse a la misma sesión, tendría barra libre para
sonsacar el guion a base de reconexiones.

## Invariantes (ver CLAUDE.md §6)

1. El servidor emite **un reto a la vez**. Nunca una lista.
2. Ningún mensaje contiene el total de retos, el índice actual, la semilla,
   los retos futuros, los umbrales ni los scores parciales.
3. El cliente **no** reporta haber cumplido un reto. Sólo envía frames.
   El servidor decide a partir de las señales.
4. El resultado de un reto individual no se comunica al cliente. Sólo se
   comunica el veredicto final de la sesión.
5. Los mensajes de cierre de reto son de tamaño y timing uniformes: no debe
   poder inferirse el resultado por canal lateral.
