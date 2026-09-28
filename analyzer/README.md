# analyzer — Python

Consume frames del bus NATS y emite **vectores de magnitudes con nombre**.

## La regla

**Go decide. Python mide.** Ver `CLAUDE.md` §3.

Este servicio no sabe qué es un WebSocket, no sabe qué reto está activo, no
toca Redis, Postgres ni MinIO, y no emite ningún booleano de "esto es real" ni
ninguna probabilidad de ataque. Sólo magnitudes.

La frontera no depende de la buena voluntad: `wire.encode_features` rechaza
señales con nombre de juicio (`is_live`, `spoof_score`…). Si alguien intenta
cruzarla, la sesión se rompe con estruendo en vez de mentir en silencio.

## Alcance actual

Implementado:

* Consumidor NATS asíncrono, con el contrato de `/proto`.
* Decodificación JPEG y detección de rostro.
* Estado caliente por sesión, con limpieza por cierre y por TTL.
* Por frame: caja del rostro, presencia, número de rostros, calidad (nitidez
  por varianza del laplaciano, luminancia media, saturación de altas luces),
  **pose de la cabeza en grados y escala del rostro**.
* **Análisis de ventanas de pose**: cumplimiento, continuidad de trayectoria,
  paralaje y continuidad de identidad.
* **Análisis de destello**: calibración previa obligatoria, medición
  diferencial contra el fondo, correlación con búsqueda de retardo, gradiente
  3D y detección de superficie emisiva.
* Presupuesto de 30 ms por frame, instrumentado y expuesto.
* Paralelismo por procesos.

Sin implementar todavía (`src/analyzer/signals/`): moiré, textura, reflejo
especular, profundidad, flujo óptico, rPPG y respuesta cromática. Son los
pasos siguientes.

## Ventanas de pose

Al cerrar un reto de pose, Go pide medir un intervalo por
`session.<id>.window` y recibe cuatro sub-métricas por
`session.<id>.challenge_score`. Lo que va de ida es un eje, un ángulo, un
margen y un intervalo: ni reto, ni posición en el guion, ni umbral.

| Sub-métrica | Qué mide | Peso |
|-------------|----------|------|
| `parallax` | Cuánto residuo deja el mejor modelo plano | 0,45 |
| `compliance` | Hasta dónde llegó el ángulo respecto al objetivo | 0,25 |
| `continuity` | Si recorrió los ángulos intermedios o pegó un salto | 0,20 |
| `identity` | Si el embedding se mantuvo o pegó un salto | 0,10 |

Una sub-métrica **ausente** significa "no se pudo medir". No es cero: cero es
"medido y malo". Quien no llegó a girar no tiene paralaje medible, y eso no lo
convierte en un atacante. Los pesos se renormalizan sobre lo que sí se midió.

### Paralaje: cómo y hasta dónde

Se comparan los landmarks del par de frames con **mayor separación angular**
—a más base, más paralaje, como en estereoscopía— y se mide el residuo que
deja la mejor homografía, normalizado por el tamaño del rostro. La homografía
es la hipótesis nula: describe exactamente el movimiento de un plano. Si el
residuo es pequeño, lo que se mueve es un plano.

Medido con material sintético, mismo giro de 30° delante de la misma cámara:

| | residuo | recorrido aparente |
|---|---|---|
| Cabeza con volumen | 0,0426 | 31,4° |
| Foto impresa girada | 0,0098 | 2,9° |

**4,4× de separación**, y el residuo del rostro crece con la base angular
(0,0097 a 7° → 0,0426 a 31°), que es lo que hace que sea paralaje y no ruido.

Segunda defensa, no menos importante: girar una cartulina **no produce la
firma de pose de una cabeza girando**. El ajuste 3D del detector sólo ve 2,9°
de los 30° reales, así que una foto se cae ya en el cumplimiento.

**Límite conocido.** Los landmarks salen de un ajuste de modelo 3D, no de la
imagen. Con una foto sostenida muy cerca de la cámara (perspectiva extrema, a
unos centímetros) ese ajuste se desestabiliza y produce residuo espurio: el
paralaje deja de separar. No es un escenario físicamente plausible —a esa
distancia la foto ni enfoca— pero está anotado. La solución, cuando haga falta,
son rasgos de imagen sobre textura de piel real, y la calibración definitiva
tiene que salir de `/bench` con material real: los números de arriba vienen de
material sintético.

## Destello

El componente central. Mide cómo responde la piel del sujeto a una secuencia de
colores emitida por la pantalla.

### Calibración: obligatoria y primero

Con pantalla neutra se captura la línea base del sujeto: respuesta cromática de
su piel, luz de la habitación y comportamiento de esa cámara. **Todo lo
posterior se mide relativo a esa línea base, nunca contra umbrales absolutos.**

No es un detalle de implementación. La luz que devuelve una piel depende de su
albedo, y un umbral absoluto convertiría el tono de piel en motivo de rechazo:
el sistema fallaría desproporcionadamente con las personas de piel oscura. Eso
no sería un problema de precisión, sería un producto defectuoso.

Midiendo relativo, lo que se compara es cuánto cambia cada sujeto respecto a sí
mismo, y el albedo se cancela en la división.

### Medición diferencial: el problema número uno

Cada región facial se mide contra una región de FONDO **del mismo frame**. La
webcam mide la exposición ponderando el centro —donde está la cara— y corrige
todo el frame: cuando el destello ilumina al sujeto, baja la ganancia y el
brillo absoluto del rostro apenas se mueve. Dividir por el fondo lo cancela,
porque la corrección afecta a los dos por igual.

Medido con la cámara del banco compensando al máximo:

| | amplitud de modulación | dispersión entre regiones |
|---|---|---|
| Diferencial (contra el fondo) | 0,3947 | 0,2025 |
| Absoluta | 0,0803 | 0,9889 |

La medida absoluta pierde el 80 % de la señal. Y hace algo peor: al comprimir
unas regiones más que otras, **se inventa un relieve que no existe** (0,99 de
dispersión frente a 0,20 real). Sin el diferencial, el gradiente 3D mediría el
auto-exposición en vez de la cara.

### Las tres sub-métricas

| Sub-métrica | Qué mide | Peso |
|-------------|----------|------|
| `gradient_3d` | Dispersión de la respuesta entre frente, nariz y pómulos | 0,45 |
| `correlation` | Si la piel siguió a la secuencia, buscando el retardo | 0,35 |
| `screen_absence` | 1 menos la sospecha de superficie emisiva | 0,20 |

El gradiente pesa más porque es **lo único** que separa un rostro de una foto
impresa: las dos siguen al destello igual de bien, pero sólo una tiene relieve.

El retardo se busca, nunca se supone: entre pintar un color y verlo pasan de
100 a 300 ms, variables. Recuperado exacto en el banco para retardos de 0, 100,
180, 260 y 340 ms.

### Métricas obtenidas

Banco sintético, mismo estímulo y misma cámara, distinto objeto delante:

| escena | score | correlación | gradiente 3D | sin_pantalla |
|--------|-------|-------------|--------------|--------------|
| **Rostro real** | **1,000** | 1,000 | 1,000 | 1,000 |
| **Foto impresa** | **0,602** | 1,000 | 0,115 | 1,000 |
| **Replay en pantalla** | **0,139** | 0,000 | 0,226 | 0,187 |

La dispersión entre regiones separa rostro de foto por **9,8×** (0,2019 frente
a 0,0207). Amplitudes medidas en el rostro real: nariz 0,524, frente 0,389,
pómulos 0,331 — la nariz está más cerca de la pantalla y recibe más.

**Tonos de piel**, rostro real:

| tono | albedo | score | correlación | gradiente | SNR |
|------|--------|-------|-------------|-----------|-----|
| muy clara | 0,78-0,82 | 0,982 | 1,000 | 1,000 | 323 |
| clara | 0,58-0,68 | 0,993 | 1,000 | 1,000 | 986 |
| media | 0,36-0,50 | 1,000 | 1,000 | 1,000 | 508 |
| oscura | 0,17-0,26 | 1,000 | 1,000 | 1,000 | 149 |
| muy oscura | 0,09-0,14 | 1,000 | 1,000 | 1,000 | 53 |

Diferencia entre el mejor y el peor: **0,018**. Correlación y gradiente valen
1,000 en todos los tonos.

> Dos sesgos reales aparecieron durante el desarrollo y están corregidos, con
> test de regresión cada uno. El primero: medir el bandeo como razón
> pico/mediana del espectro se disparaba en recortes oscuros (24,7 frente a
> 5,4 en el mismo sujeto). El segundo: la caja que encierra los landmarks
> incluye esquinas de fondo, y con piel muy oscura la cámara sube tanto la
> ganancia que ese fondo se quema y se leía como el reflejo de una pantalla
> (fracción especular 0,045 frente a 0,000). Los dos habrían sido defectos de
> producto, no de precisión.

### Calidad insuficiente lleva a reintentar

Si la luz ambiente aplasta el destello, la señal no llega al suelo de ruido y
el analizador lo **dice** en vez de fingir una medida: `quality_sufficient`
falso, sub-métricas a `None` y motivo explícito. Eso lleva a repetir la sesión,
nunca a rechazar a la persona.

Degradación medida, rostro real con piel media:

| destello/ambiente | SNR | calidad |
|-------------------|-----|---------|
| 0,87 | 508 | ok |
| 0,22 | 162 | ok |
| 0,13 | 99 | ok |
| 0,013 | 11 | ok |
| 0,003 | 0,5 | **insuficiente** |

### Detección de superficie emisiva

Una pantalla fabrica su propia luz: brilla mucho y apenas modula. Se combinan
cuatro indicios, todos **espaciales** (rostro frente a fondo, mismo frame):

* Brillar sin seguir al destello. Sólo cuenta acompañado de correlación baja:
  alguien con la piel clara en una habitación a oscuras también brilla.
* Reflejo especular concentrado en el cristal.
* Moiré: la rejilla de píxeles del panel, muestreada por la del sensor.
* Bandeo: desfase entre el refresco del panel y la exposición.

Tienen que ser espaciales y no relativos a la línea base: si el replay ya
estaba delante durante la calibración —y lo está, porque es la misma sesión—,
sus artefactos están en la línea base y compararse contra ella no delataría
nada. Lo que delata es que el rostro los tenga y la habitación no.

### Continuidad de identidad

Embedding de 128 dimensiones con ONNX Runtime en CPU (SFace), sobre el rostro
alineado a la plantilla canónica de cinco puntos. Sin alinear, el embedding
cambiaría con cada giro y la continuidad se confundiría con el propio reto.

Se comparan frames **consecutivos**, no todos contra el primero: girar la
cabeza deriva el embedding poco a poco, y penalizar esa deriva castigaría al
usuario por hacer justo lo que se le pidió. Lo que delata una sustitución es
el salto.

No es reconocimiento facial: no se compara contra ninguna base de datos ni se
identifica a nadie. Los vectores viven en memoria mientras dura la sesión y se
tiran con ella; al bus sólo sale la similitud, un número.

## Puesta en marcha

```bash
make venv      # entorno virtual con las dependencias
make models    # descarga los modelos de detección (no se versionan)
make up        # NATS
make analyzer  # arranca el supervisor
```

O a mano:

```bash
cd analyzer
python -m analyzer            # supervisor con N procesos
python -m analyzer worker     # un worker en primer plano
python -m analyzer preflight  # comprueba el detector y sale
```

## Detección

Dos implementaciones intercambiables (`ANALYZER_DETECTOR`):

| Backend | Qué es | Latencia típica |
|---------|--------|-----------------|
| `mediapipe` (por defecto) | MediaPipe Face Landmarker; la caja sale de los landmarks | ~5 ms |
| `onnx` | ONNX Runtime en CPU con YuNet | ~7 ms |

**MediaPipe Tasks trae su propio motor de inferencia (TFLite/XNNPACK) y no se
puede enrutar por ONNX Runtime.** Son dos caminos alternativos, no uno encima
del otro. El de ONNX Runtime existe porque es por donde entrarán los modelos
propios de PAD, que sí se exportarán a ONNX.

Los dos se comprueban entre sí en los tests, y el de ONNX se contrasta además
con el decodificador propio de OpenCV para el mismo modelo: la salida cruda de
YuNet hay que decodificarla a mano y un error ahí daría cajas plausibles pero
mal puestas.

> MediaPipe está fijado a la línea `0.10.x`. La `1.0.x` aborta el proceso en
> macOS ARM: el grafo inicializa Metal incondicionalmente y muere con SIGABRT,
> sin excepción que capturar. Por eso existe `python -m analyzer preflight`.

## Paralelismo: procesos, no hilos

El análisis es CPU pura y el GIL serializa los hilos. El supervisor levanta N
procesos, cada uno con su intérprete, su detector y su bucle de eventos.

Encaja con el resto del diseño sin esfuerzo: **cada proceso es un worker
independiente** que se anuncia por su cuenta, y el gateway reparte sesiones
entre ellos por carga. La afinidad de sesión sigue valiendo, porque una sesión
cae en un proceso y ahí se queda.

Cada proceso configura su motor de inferencia con **un hilo**: si además cada
uno abriera su propio pool, se pisarían y la latencia por frame subiría en vez
de bajar.

## Estado caliente

Al aceptar un lease se crea el estado de esa sesión en memoria: contador de
frames, última secuencia y ventana temporal reciente. **Los frames llegan
pelados**; lo que se sabe de la sesión está aquí desde el lease. Sin esa
continuidad, rPPG, flujo óptico y tracking son ruido.

Se suelta al recibir `session_close` o por TTL. El TTL hace falta aunque
exista el aviso: el bus es efímero y ese aviso se puede perder.

Si el proceso se cae, sus sesiones se pierden y el gateway las aborta. Es lo
previsto: duran segundos y se reintentan desde cero (`CLAUDE.md` §4).

## Presupuesto de latencia

30 ms por frame. Se mide siempre, se expone siempre y se registra cuando se
pasa, con el desglose por etapas: saber que "tardó 45 ms" no arregla nada,
saber que fueron 40 en detección sí.

Métricas Prometheus en `ANALYZER_METRICS_PORT` (un puerto por proceso):
`liveness_analyzer_frame_latency_ms` (histograma con cubos a caballo del
presupuesto), `..._frames_over_budget_total`, `..._frames_total`,
`..._sessions_open`, `..._frames_failed_total`.

Medido en este repositorio, 640×480 JPEG:

| Pipeline | p50 | p95 | máx | caudal |
|----------|-----|-----|-----|--------|
| Detección + calidad + pose | 4,9 ms | 5,0 ms | 7,7 ms | 204 fps |
| Con embeddings (1 de cada 3) | 5,0 ms | 10,3 ms | 11,8 ms | 148 fps |
| Con embeddings en cada frame | 10,4 ms | 10,8 ms | 20,6 ms | 94 fps |

## Estructura

```
src/analyzer/
  wire.py         codec del bus, espejo de gateway/internal/bus/codec.go
  frames.py       decodificación JPEG
  quality.py      nitidez, luminancia, altas luces
  detector/       protocolo + backends mediapipe y onnx
  rois.py         regiones faciales y de fondo desde los landmarks
  photometry.py   medición diferencial por frame y pistas de superficie
  calibration.py  línea base del sujeto con pantalla neutra
  flash.py        análisis de una ventana de destello
  pose.py         ángulos de cabeza y escala del rostro
  identity.py     embeddings faciales (ONNX Runtime)
  window.py       las cuatro sub-métricas de una ventana de pose
  sessions.py     estado caliente por sesión y TTL
  pipeline.py     decodificar → detectar → medir, con latencia
  worker.py       consumidor NATS asíncrono
  supervisor.py   N procesos worker
  metrics.py      instrumentación y presupuesto
  signals/        detectores de PAD (pasos siguientes)
```

## Desarrollo

```bash
make venv && make models
cd analyzer
.venv/bin/python -m pytest        # incluye el criterio de aceptación
.venv/bin/python -m ruff check .
.venv/bin/python -m mypy
```

Los tests contra NATS se saltan solos si no hay bus a mano (`make up`).

## Contrato

`/proto/nats/v1/*.proto` es el esquema de referencia; la codificación en el
cable está descrita en `/proto/README.md`. Las dos implementaciones —`wire.py`
y `codec.go`— están sujetas por los vectores dorados de `/proto/testdata/`:
cada lado comprueba que decodifica exactamente lo que el otro escribió.
