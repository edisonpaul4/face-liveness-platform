# Subjects NATS

Bus **NATS core**, sin JetStream. Sin persistencia, sin reintentos.

| Subject | Dirección | Payload | Notas |
|---------|-----------|---------|-------|
| `analyzer.announce` | analyzer → gateway | `Announce` (JSON) | Capacidad y carga, cada segundo. Es todo el descubrimiento que hay. |
| `analyzer.lease.<worker_id>` | gateway → analyzer | `LeaseRequest` / `LeaseReply` (JSON) | Request/reply. Asigna la sesión a ese worker. |
| `session.<id>.frames` | gateway → analyzer | `FrameTask` (binario) | Frames. Sin respuesta. |
| `session.<id>.features` | analyzer → gateway | `FrameFeatures` (JSON) | Medidas por frame. |
| `session.<id>.heartbeat` | analyzer → gateway | `SessionHeartbeat` (JSON) | Renueva el lease. |
| `session.<id>.control` | gateway → analyzer | `SessionControl` (JSON) | Sólo higiene: `session_close`. Nunca semántica de negocio. |
| `session.<id>.window` | gateway → analyzer | `PoseWindowRequest`, `CalibrationWindowRequest` o `FlashWindowRequest` (JSON) | Discriminadas por el campo `kind`. |
| `session.<id>.challenge_score` | analyzer → gateway | `ChallengeScore` (JSON) | Sub-métricas de la ventana. Sin veredicto. |

Los frames van en binario compacto porque son el camino caliente: en JSON, el
payload iría en base64 y costaría un tercio más de red por frame. Todo lo
demás es de bajo caudal y va en JSON, para que el worker de Python lo lea sin
generar código.

## Afinidad de sesión

Todos los frames de una sesión van al mismo worker: las señales temporales
(rPPG, flujo óptico, tracking) no tienen sentido sin continuidad.

La asignación es explícita, no por hash:

1. Los workers se anuncian en `analyzer.announce`.
2. El gateway elige al **menos cargado** y le pide la sesión por
   `analyzer.lease.<worker_id>`.
3. El worker acepta y crea el estado caliente **en su memoria**.
4. Mientras viva, renueva el lease con heartbeats.

El estado caliente **no viaja con cada frame**. `FrameTask` lleva el frame y
nada más: ni sesión, ni reto activo, ni estado. La sesión la sabe el worker
porque aceptó el lease, y el reto activo no lo sabe ni tiene por qué
(CLAUDE.md §3).

## Cuando un worker se cae

Silencio mayor que el TTL del lease → **la sesión se aborta**. No hay
reasignación ni recuperación: ver CLAUDE.md §4 para el porqué.

Un worker que deja de anunciarse deja de recibir sesiones nuevas por sí solo.
No hace falta darlo de baja en ningún sitio.

## Ventanas de pose

Al cerrar un reto de pose, el gateway pide medir el intervalo por
`session.<id>.window` y recibe cuatro sub-métricas por
`session.<id>.challenge_score`.

Lo que va de ida es lo mínimo para medir: un eje, un ángulo objetivo, un
margen y un intervalo de tiempo. **No lleva identificador de reto, ni posición
en el guion, ni umbral de aprobado.** El analizador mide geometría en un trozo
de la serie que ya tiene en memoria; qué signifique el resultado es de Go.

Lo que vuelve son medidas, nunca un veredicto:

| Sub-métrica | Qué mide |
|-------------|----------|
| `compliance` | Hasta dónde llegó el ángulo respecto al objetivo. |
| `continuity` | Si recorrió los ángulos intermedios o pegó un salto (corte de vídeo, frame sustituido). |
| `parallax` | Cuánto residuo deja el mejor modelo plano. Si lo explica demasiado bien, es una superficie plana. |
| `identity` | Si el embedding se mantuvo o pegó un salto. |

Una sub-métrica **ausente** significa "no se pudo medir", que no es cero. Cero
es "medido y malo". Quien no llegó a girar no tiene paralaje medible, y eso no
lo convierte en un atacante.

## Ventanas de destello

La calibración es **obligatoria y va primero**: con pantalla neutra, el
analizador captura la línea base del sujeto. Todo lo posterior se mide relativo
a ella, nunca contra umbrales absolutos, y de eso depende que el sistema
funcione igual con cualquier tono de piel.

Después, por cada reto de destello, Go manda la secuencia de colores emitida y
el intervalo, y recibe tres sub-métricas:

| Sub-métrica | Qué mide |
|-------------|----------|
| `correlation` | Si la piel siguió a la secuencia, buscando el retardo (100-300 ms). |
| `gradient_3d` | Si frente, nariz y pómulos respondieron con intensidades distintas. Una superficie plana responde igual en todas partes. |
| `screen_absence` | 1 menos la sospecha de superficie emisiva. |

Más `quality_sufficient`. Cuando es `false` —la luz ambiente aplastó el
destello, o faltó línea base— la sesión se **reintenta**, no se rechaza: no se
midió algo malo, es que no se pudo medir.

## Validación de identificadores

Antes de construir un subject se comprueba que el identificador no lleve `.`,
`*`, `>` ni espacios. Un subject construido con datos sin validar es una vía
para suscribirse a sesiones ajenas.
