# CLAUDE.md — Plataforma de Liveness Detection

> Documento normativo del repositorio. Toda contribución (humana o de un agente)
> debe respetar las reglas de este archivo. Si un cambio contradice algo escrito
> aquí, primero se cambia este documento y se justifica el motivo.

Estado actual: **scaffolding**. No hay lógica de negocio implementada. Los
paquetes existen para fijar la frontera arquitectónica, no para funcionar.

---

## 1. Qué es esto

Plataforma de *prueba de vida facial* (face liveness / PAD, Presentation Attack
Detection) equivalente en alcance a AWS Rekognition Face Liveness: el cliente
abre una sesión, el servidor le impone una secuencia de retos, el cliente
transmite frames de cámara en vivo por WebSocket, y el servidor decide si hay
una **persona real presente** frente a la cámara en ese momento.

El resultado de una sesión es una decisión (`LIVE` / `SPOOF` / `INCONCLUSIVE`),
un score de confianza y un paquete de evidencia auditable.

---

## 2. Arquitectura

```
                 ┌───────────────────────────────────────────────┐
   navegador     │                    GO                         │
   (Svelte)      │                                               │
       │  WSS    │  ┌──────────┐   in-proc   ┌────────────────┐  │
       ├────────►│  │ gateway  │────────────►│  orchestrator  │  │
       │  frames │  │          │◄────────────│                │  │
       │  retos  │  │ WS, auth │   eventos   │ FSM, retos,    │  │
       │◄────────┤  │ backprsr │             │ fusión, veredicto│ │
                 │  └────┬─────┘             └───┬────┬───┬───┘  │
                 └───────┼───────────────────────┼────┼───┼──────┘
                         │ NATS core (efímero)   │    │   │
                         │ afinidad de sesión    │    │   │
                    ┌────▼───────────────────┐   │    │   │
                    │        PYTHON          │   │    │   │
                    │       analyzer         │   │    │   │
                    │  CV + procesamiento de │   │    │   │
                    │  señal → SOLO métricas │   │    │   │
                    └────────────────────────┘   │    │   │
                                            Redis│ PG │MinIO
                                            (hot)│(res)│(evid)
```

### Componentes

| Componente     | Lenguaje | Responsabilidad |
|----------------|----------|-----------------|
| `gateway`      | Go       | Terminación WebSocket/TLS, autenticación de sesión, validación del protocolo de cliente, control de flujo (backpressure), troceo de frames, publicación en el bus, entrega de retos al cliente. |
| `orchestrator` | Go       | Máquina de estados de la sesión, generación y guion de retos, ventanas temporales, fusión de scores, veredicto, persistencia, API REST de resultados. |
| `analyzer`     | Python   | Análisis de frames: visión por computador y procesamiento de señal. Emite **vectores de magnitudes con nombre**. Nada más. Hoy: detección de rostro y calidad de captura. |
| `proto`        | —        | Contratos compartidos: mensajes del bus (protobuf) y protocolo de cliente (JSON Schema). Fuente única de verdad. |
| `web`          | JS       | Cliente de prueba mínimo, sin framework, para depurar el protocolo WS a mano. Captura **por aviso de frame nuevo** (`requestVideoFrameCallback`), no por reloj: muestreando a intervalo se copia lo que el `<video>` esté mostrando, y medido sobre una grabación real la mitad de los 139 frames enviados eran idénticos al anterior. No es sólo banda: cada copia lleva un sello de captura nuevo con píxeles de hasta 150 ms antes, y ocupa una plaza de `MaxInflight` que el frame realmente nuevo necesitaba. Comprime en **JPEG**, no en WebP: WebP pesa la mitad pero su codificador tarda 28 ms por frame contra 1 ms, y en una sesión real hundió la cadencia de 21 a 7 fps. El cuello del cliente es el hilo principal, no la banda, así que cambiar bytes por milisegundos va al revés. El camino, si algún día hace falta, es `OffscreenCanvas` en un Worker. **Y no encola**: si el socket todavía tiene un frame esperando, se salta el turno. Captura **con la relación de aspecto REAL de la cámara**: `drawImage` no recorta sino que escala, y una cámara de móvil en vertical entrega 480x640 aunque se le pidan 640x480, así que cada frame salía con la cara **1,78 veces más ancha**. Medido al deshacerlo: `gaze_offset_x` invariante (se normaliza por el ancho del ojo), `pose_yaw_deg` ×1,141, `pose_pitch_deg` ×0,735 y `gaze_openness` ×1,636 — ahí estaba la apertura aplastada que rechazaba ojos abiertos. Lo peor era que dependía del DISPOSITIVO: una webcam apaisada no lo sufre, así que ningún umbral podía valer para las dos. A **480p por defecto**: a 720p pedía 20,7 Mbit/s de subida y un parón del enlace se llevó un reto entero, mientras que a 480p pesa 58 KB en vez de 158 y el iris se mide **mejor** —1 frame sin medir de 24 contra 4—, porque con la cara cerca a 720p le sobran píxeles. `make gateway CAPTURE=720` vuelve a subirla para comparar. Y a **30 fps**, no 15: medido, 137 de 335 frames de una sesión llegaron a exactamente 66 ms —el intervalo del tope— mientras la cámara daba huecos de 17 ms y el analizador tardaba 8,9 ms por frame. El tope lo ponía el número, no la máquina, y las muestras son la señal: el destello llevaba SEIS por tramo. Codifica en un **Worker con `OffscreenCanvas`**, y ahí mide también la nitidez: medido en un móvil, la sonda costaba 21,7 ms en el hilo principal —un `getImageData` es una lectura de vuelta de la GPU— contra 0,9 ms de codificar. El caro nunca fue el códec. Con una tubería de un frame para solapar captura y codificación, y el filtro de cara **una vez por segundo**: rechazaba cero frames y costaba tres por segundo. El tope de envío va en **caudal (9 Mbit/s), no en cadencia**: el recurso escaso son los bits, y pasarse no se paga en frames perdidos sino en frames TARDÍOS. Medido el mismo día en la misma habitación, a 7,2-8,7 Mbit/s el retardo hallado era de 250-350 ms y se medía todo; a 16,5 Mbit/s —53 fps de cámara, 28 enviados— el retardo se fue a 800 ms y dos miradas se cerraron sin medir. |
| `frontend`     | Svelte   | Aplicación cliente real (SvelteKit): cámara, UI de retos, feedback. |
| `bench`        | —        | Banco de pruebas de ataques de presentación: casos, fixtures y runner de métricas (APCER/BPCER). |
| `deploy`       | —        | Infraestructura local y despliegue. |

### Infraestructura

| Servicio  | Uso | Regla |
|-----------|-----|-------|
| NATS core | Bus Go ⇄ Python | **Sin JetStream.** Efímero por diseño: un frame perdido es un frame perdido, no se reintenta. La sesión tolera pérdida; la latencia no. |
| Redis     | Estado caliente de sesión | Guion de retos, FSM, ventanas de señales, tickets. **TTL siempre**: una sesión dura menos de un minuto. |
| Postgres  | Resultados | Auditoría append-only —lo impiden triggers, no una convención— más la línea de tiempo de features, que caduca aparte. |
| MinIO     | Evidencia | Frames y clip, **cifrados en cliente** antes de salir del proceso. Referenciados desde Postgres por clave, nunca embebidos. |

---

## 3. La regla Go / Python (frontera exacta)

**Go decide. Python mide.**

Esta es la regla más importante del repositorio y no admite excepciones
"temporales".

### Go es dueño de

- El protocolo WebSocket con el cliente (y su versionado).
- La identidad y el ciclo de vida de la sesión.
- La máquina de estados: qué reto viene ahora, cuándo expira, cuándo se aborta.
- La generación del guion de retos y su aleatoriedad (semilla, orden, tiempos).
- La correlación de señales con la ventana temporal del reto activo.
- La fusión de scores y los umbrales.
- El veredicto y su explicación.
- Toda persistencia (Redis, Postgres, MinIO).
- Toda API pública.

### Python es dueño de

- Decodificar el frame que le llega.
- Ejecutar detección/landmarks/tracking facial, análisis de textura, moiré,
  reflejo especular, profundidad monocular, rPPG, flujo óptico, análisis de
  frecuencia, calidad de imagen.
- Devolver un **vector de señales numéricas** con su timestamp y su
  identificador de frame.

### Python NO puede

- ❌ Conocer el protocolo WebSocket ni sus mensajes.
- ❌ Saber qué reto está activo, ni que existen los retos.
- ❌ Decidir si algo es real o falso. Ni un booleano `is_live`. Ni un
     `spoof_probability` que se propague sin fusión.
- ❌ Aplicar umbrales de negocio.
- ❌ Leer o escribir en Redis, Postgres o MinIO.
- ❌ Hablar con el cliente, ni directa ni indirectamente.
- ❌ Mantener estado entre sesiones. El estado por sesión que mantenga
     (buffers de tracking, ventana rPPG) es un *detalle de implementación
     local*, reconstruible, y nunca la verdad del sistema.

### Go NO puede

- ❌ Hacer visión por computador. Nada de decodificar píxeles, aplicar filtros
     o cargar modelos en Go.
- ❌ Reimplementar en Go una señal "porque es más rápido".

### Cómo se ve la frontera en el código

Python recibe:

```
FrameTask { session_id, frame_seq, captured_at, mime, width, height, payload }
```

Python devuelve:

```
FrameSignals { session_id, frame_seq, analyzed_at, signals: map<string,double>, quality, face_box? }
```

Nótese lo que **no** viaja: ni `challenge_id`, ni `state`, ni `verdict`, ni
`threshold`. Si un contrato nuevo necesita meter cualquiera de esos campos en un
mensaje hacia Python, el diseño está mal.

> Test mental: *el analyzer debe poder ejecutarse contra un directorio de
> imágenes sueltas, sin NATS, sin sesión y sin retos, y seguir siendo útil.*
> Si no puede, se ha filtrado lógica de negocio a Python.

Y no depende de que nadie se acuerde: `analyzer/src/analyzer/wire.py` rechaza
al serializar cualquier señal con nombre de juicio (`is_live`, `spoof_score`,
`attack_detected`…). Cruzar la frontera rompe la sesión en vez de mentir en
silencio.

### El único sitio donde Go le cuenta algo a Python

Al cerrar un reto de pose, Go pide medir una ventana: *eje, ángulo objetivo,
margen e intervalo de tiempo*. Es la única información que cruza en esa
dirección, y está recortada a propósito:

- **No** lleva identificador de reto, ni posición en el guion, ni cuántos
  quedan.
- **No** lleva umbral de aprobado, ni qué pasa si sale mal.
- Python devuelve medidas y un resumen 0-1. **Nunca un pass/fail.**

La regla sigue en pie —Python no sabe qué es un reto ni qué consecuencia tiene
lo que mide— pero conviene tenerlo presente: es el punto por donde la frontera
se rozaría primero si alguien la fuerza. Cualquier campo nuevo en
`PoseWindowRequest` merece la misma pregunta de §6: *¿esto le cuenta a Python
algo que no necesita para medir?*

La ventana de pulso (`PulseWindowRequest`) es el contraejemplo que conviene
tener al lado: lleva un identificador y dos instantes, y nada más. Elegir *qué*
intervalo —el más largo sin destellos— es una decisión sobre el guion, y por
eso la toma Go. Python no sabe por qué ese tramo y no otro.

---

## 4. Bus NATS y afinidad de sesión

- **NATS core**, sin persistencia. Pérdida aceptada, latencia no.
- Los frames de **una misma sesión** deben ir siempre **al mismo worker de
  analyzer**, porque las señales temporales (rPPG, flujo óptico, tracking)
  requieren continuidad. Sin afinidad, esas señales son ruido.
- El estado caliente de la sesión vive **en la memoria de ese worker** y no
  viaja con cada frame. Serializarlo costaría más que analizarlo y convertiría
  un flujo continuo en una sucesión de fotos sueltas.

### Asignación: descubrimiento + lease

La sesión se ata a un worker concreto al empezar, y ahí se queda.

1. **Descubrimiento.** Los workers publican su capacidad en `analyzer.announce`
   cada segundo. El gateway se queda con el último anuncio de cada uno. Si un
   worker no se anuncia, no existe: no hace falta darlo de baja.
2. **Asignación.** El gateway elige al **menos cargado** y le pide la sesión
   por `analyzer.lease.<worker_id>` (request/reply). Si no contesta o la
   rechaza, se prueba con el siguiente. Sin candidatos, la sesión no arranca.
3. **Lease con heartbeat.** El worker manda `session.<id>.heartbeat` mientras
   viva. Cualquier mensaje suyo —medidas incluidas— renueva el lease. Si calla
   más de `LeaseTTL`, la sesión se **aborta**.
4. **Liberación.** Al acabar, el gateway manda `session.<id>.control` con
   `session_close` y el worker suelta el estado caliente.

| Subject | Dirección | Nota |
|---------|-----------|------|
| `analyzer.announce` | Python → Go | Capacidad, periódico. Es todo el descubrimiento que hay. |
| `analyzer.lease.<worker_id>` | Go → Python | Asignación de sesión, request/reply. |
| `session.<id>.frames` | Go → Python | Frames. Sin respuesta y sin estado dentro. |
| `session.<id>.features` | Python → Go | Medidas por frame. |
| `session.<id>.heartbeat` | Python → Go | Renovación del lease. |
| `session.<id>.control` | Go → Python | Sólo higiene: `session_close`. Nunca semántica de negocio. |

Los identificadores de sesión y de worker se validan antes de construir un
subject: un `>` o un `*` colado ahí sería una suscripción a sesiones ajenas.

Y el gateway **avisa cuando ve analizadores de versiones distintas** en el bus.
No es paranoia: durante el desarrollo pasó tres veces y las tres desvió el
diagnóstico. Un worker viejo sigue anunciándose y atendiendo sesiones, así que
la mitad se analizan sin las señales nuevas — y el síntoma es un fallo
intermitente que parece del algoritmo y es de despliegue. Sólo avisa: elegir
por versión sería una política de despliegue y no le toca decidirla al
gateway, pero callarse tampoco.

### Cuando el worker se cae

**No se recupera la sesión. Se aborta.**

Se cierra con `analyzer_unavailable`, veredicto no concluyente y código de
cierre 4009, y el usuario reintenta desde cero con ticket, semilla y guion
nuevos.

No es pereza. El estado caliente estaba en la memoria del worker que se fue,
la sesión completa dura menos de un minuto y reconstruirla no se distingue de
empezar otra. Peor todavía: un mecanismo de recuperación sería una vía para
que un atacante forzara reintentos dentro de la misma sesión, que es
exactamente lo que la regla del ticket de un solo uso (§6) impide.

Por eso el lease es corto —TTL de segundo y medio— y por eso el motivo que
llega al cliente es *infrastructure_error* y no *try_again*: el fallo es
nuestro y reintentar tiene sentido.

### Frames que no vuelven

Un frame que no produce medidas en `FrameTimeout` se descarta y se cuenta. No
se espera, no se reintenta y no bloquea la sesión: llegan más frames
enseguida, y uno viejo ya no describe lo que está pasando.

### El worker de análisis

Paralelismo por **procesos**, no hilos: el trabajo es CPU pura y el GIL
serializa los hilos. Cada proceso es un worker independiente que se anuncia
por su cuenta, así que el reparto de sesiones lo hace el mismo descubrimiento
por carga de más arriba. La afinidad se mantiene: una sesión cae en un proceso
y ahí se queda.

Cada proceso configura su motor de inferencia con **un solo hilo**. Con N
procesos × N hilos, las latencias por frame suben en vez de bajar.

Presupuesto de **30 ms por frame**, medido, expuesto en Prometheus y
registrado con desglose por etapas cuando se pasa. Un analizador que se
retrasa no falla: hace que el gateway descarte frames, y sin esta métrica
nadie sabría por qué.

Detección: MediaPipe Face Landmarker (por defecto) u ONNX Runtime en CPU con
YuNet, intercambiables por configuración. **Con reintento con margen**: una cara
pegada a los bordes del encuadre se le escapa al detector aunque se vea
perfectamente, así que si el primer intento no encuentra nada se repite sobre
la imagen con un borde replicado del 15 %. Medido sobre la ventana de mirada de
una sesión real —58 frames con la cara ocupando el 61 % del ancho y el 81 % del
alto—: **33 detectados tal cual (57 %) y 57 con el reintento (98 %)**, por 5,2
ms de media por frame. Los frames que fallaban eran indistinguibles a simple
vista de los que no.

El borde se **replica**, no se rellena de gris (con gris baja al 5 %), y el 15 %
es el óptimo medido: al 30 % cae al 41 % y al 50 % a cero, porque entonces la
cara queda demasiado pequeña. Y los landmarks se devuelven a las coordenadas
del frame original: sin esa corrección, todo lo que se mide después —ROIs de
fotometría, ancho de ojo en píxeles, caja de la cara— saldría desplazado. MediaPipe Tasks trae su propio motor
de inferencia y **no** se puede enrutar por ONNX Runtime; son dos caminos
alternativos. El de ONNX es por donde entrarán los modelos propios de PAD.

Al cerrar un reto de pose emite cuatro sub-métricas: cumplimiento, continuidad
de trayectoria, **paralaje** y continuidad de identidad. El paralaje es la que
más pesa: una foto o una pantalla son un plano, y el movimiento de un plano lo
explica exactamente una homografía. Si el modelo plano explica el movimiento
demasiado bien, lo que se mueve es un plano. Ver §5, ataques A1 y A2.

Al cerrar un reto de destello emite tres: **gradiente 3D**, correlación y
ausencia de pantalla. El gradiente pesa más porque es lo único que separa un
rostro de una foto impresa: las dos siguen al destello igual de bien, pero sólo
una tiene relieve, y la nariz recibe más luz de la pantalla que los pómulos.

Dos reglas de ese analizador que no son opcionales:

0. **La calibración no cierra hasta tener frames suficientes.** El gateway
   exige al menos tantos frames con rostro como pide el analizador para aceptar
   la línea base (`minCalibrationHits` ↔ `MIN_CALIBRATION_FRAMES`, anotado en
   los dos lados). Cerraba con dos cuando el analizador necesita cinco, así que
   entregaba una ventana que iba a rechazar — y **sin línea base ningún
   destello posterior se puede medir**, sin que nada señale que la causa estaba
   en el primer paso. Medido: una sesión cerró la calibración con 4 frames
   porque el caudal está frío al arrancar (2 fps ahí contra 10 fps en el reto
   siguiente).

1. **Calibración obligatoria antes de cualquier destello.** Con pantalla
   neutra se captura la línea base del sujeto, y todo lo posterior se mide
   relativo a ella. Con umbrales absolutos, la luz que devuelve una piel se
   convertiría en motivo de rechazo y el sistema fallaría
   desproporcionadamente con las personas de piel oscura. Eso no sería un
   problema de precisión: sería un producto defectuoso.
2. **Medición diferencial contra el fondo del mismo frame.** La webcam mide la
   exposición ponderando el centro y corrige todo el frame, así que en
   términos absolutos el destello casi no mueve el brillo del rostro. Medido:
   la medida absoluta pierde el 80 % de la señal y encima se inventa un
   relieve que no existe.

Y una consecuencia: cuando la luz ambiente aplasta el destello, el analizador
**dice que no puede medir** en vez de dar un número bajo. Eso lleva a
reintentar, nunca a rechazar a nadie.

Esto se comprueba **sólo cuando la correlación es débil**: una correlación
fuerte demuestra por sí sola que había señal que medir, por poco que subiera el
brillo — que es justo la razón de correlacionar. Preguntarlo al revés declaraba
no medible una secuencia seguida con una correlación de 0,93.

La regla vale para el gateway **y para el analizador**, y hubo que aprenderla
dos veces: `flash.py` aplicaba su propia puerta de amplitud antes de mirar la
correlación, y por eso sesiones reales con correlaciones de 0,815 y 0,703
acababan en «demasiada luz ambiente» mientras una con 0,090 —sin respuesta,
sólo la cara más iluminada— se daba por medida. Estaba al revés en los dos
sentidos. Lo vigila `tests/test_flash_ambient_gate.py`.

Cuando la correlación no llega, entonces sí hace falta distinguir entre "no
respondió" y "no pudimos verlo responder", y ahí la subida de brillo es la que
lo dice: por debajo del 15 %, ventana no medible. Medido con una cámara real y una ventana detrás del
sujeto, el destello aportaba el **3 %** del brillo de la cara y el 97 %
restante era ambiente; a esa proporción se estaría midiendo una modulación del
3 % contra el ruido del sensor.

Y se le **dice a la persona**, con el motivo público `too_much_ambient_light`.
Es una pista de captura, no la explicación de un rechazo: habla de su
habitación, no del guion ni de qué detector la pilló. La regla de §6 impide
explicar por qué se rechaza a alguien; dejar a un usuario legítimo
reintentando a ciegas es otro problema, y también hay que resolverlo.

Y la correlación necesita **frames suficientes por tramo**, no sólo en total.
Costó un rechazo real: una sesión legítima por una red lenta llegó con nueve
frames para dos tramos, la correlación salió 0,22 —por debajo del suelo de
0,30— y la persona fue **acusada de ataque** con un score global de 0,85. El
40 % de sus frames se había perdido en la red.

Cuatro muestras por tramo no son una correlación baja: son una correlación que
no se ha podido calcular. Confundirlas convierte una conexión mala en una
acusación de fraude, que es justo lo que prohíbe el §4.

El destello **se decide por correlación**, no clasificando cada tramo.

Es la diferencia entre medir y no medir. Un umbral por frame necesita que CADA
tramo sea reconocible por sí solo, lo que exige una modulación grande. La
correlación de la serie observada contra la secuencia emitida —que el servidor
conoce y el atacante no— saca la señal de debajo del ruido: el ruido no está
correlacionado con una secuencia aleatoria y la respuesta sí. Medido con una
cámara real, la modulación era del **5 %** y el umbral por frame no veía nada;
la correlación la recupera.

Con búsqueda de retardo, y no es opcional: entre revelar el reto y que el color
llegue a la cámara hay red, decisión del cliente, composición y captura.
Suponer sincronía exacta es suponer lo que no se puede.

Y la ventana de análisis **se alarga con el retardo máximo que se busca**. La
respuesta al último tramo llega después de que la secuencia haya terminado, así
que recortando en su final los retardos grandes se quedan sin muestras y se
descartan por falta de frames: se elige uno peor y se tira la señal. Medido
sobre una sesión real por túnel, el mismo destello daba correlación **0,3212**
con retardo 220 ms y ventana no medible, y **0,7542** con retardo 460 ms al
tener la cola disponible. Por eso el techo del rango es 600 ms y no 400: los
mejores retardos medidos caían en 360, 380 y 400 — el último, justo en el
borde, que es la señal de que el techo se quedaba corto. Ampliarlo no fabrica
señal: una sesión sin respuesta se quedó en −0,02.

Un retardo candidato tiene que **seguir cubriendo el 60 % de la secuencia**. Si
no, con el rango ampliado gana el que deja fuera media ventana: menos muestras,
no mejor alineación.

Y la correlación de las regiones se agrega **ponderada por la amplitud de cada
una**, no en media. La pantalla es una fuente de área en una posición concreta:
unas regiones la miran de frente y otras quedan lavadas por la luz de la
habitación. Medido, en los dos destellos de una sesión la mejilla izquierda
correlacionó 0,66 y 0,58 —con el doble de amplitud que el resto— mientras
frente y mejilla derecha daban 0,06 · 0,09 · 0,07 · −0,02. La media diluía esa
respuesta real a 0,32. Ponderar y no coger el máximo: con cuatro regiones y
quince muestras, el máximo de cuatro correlaciones de ruido ya ronda el
umbral.

Así es como lo hacen los sistemas del ramo. AWS Rekognition Face Liveness pide
además **acercar la cara a la pantalla** antes de los destellos, que es la otra
palanca: la iluminancia va con 1/d².

Y aquí también: el guion mete **siempre** un «acércate» justo antes del primer
destello. Hacía falta: medido en una sesión real a distancia normal, un
destello correlacionó **0,1356** y el siguiente ni se pudo medir.

Cuánto hay que acercarse **se quedó en 1,3 veces el área**, y la historia de
por qué merece estar escrita. La proporcionalidad es exacta —el área de una
cara en la imagen va con 1/d² y la iluminancia de la pantalla también, así que
la iluminancia es proporcional al área— y por eso se subió el umbral a 1,8.
Hubo que deshacerlo: la teoría era correcta y la medida dijo que no.

El beneficio no apareció. Con el sujeto al 54 % del encuadre la modulación
siguió en el 1,2 % y ninguna ventana correlacionó, porque la medida es
diferencial contra el fondo del MISMO frame: al llenar medio encuadre, el fondo
pasa a ser lo que hay justo detrás del sujeto, que recibe el destello casi
igual que su cara, y el cociente cancela lo que se quería amplificar.

Y el coste sí apareció, en otra señal: a esa distancia la cámara mira la cara
desde muy abajo y el ojo se escorza en vertical. Medido sobre tres sesiones,
con la cara cerca el ancho de ojo pasó de 68 a 156 px mientras la razón
alto/ancho caía de 0,166 a **0,087**, por debajo del mínimo de 0,15: el 100 %
de los frames se descartaron y el reto de mirada se cerró con avance CERO, sin
una sola medida.

Tres reglas suyas:

1. **No cuenta como reto**, igual que la calibración y el tramo de quietud.
   Acercarse no es un ángulo, no produce ventana de pose y no aporta ninguna
   señal: es encuadre. Quitarle un paso al guion para ganar encuadre sería
   cambiar defensa por comodidad.
2. **Ya no se sortea.** `PoseMoveCloser` salió del catálogo de poses: sorteado
   gastaba un reto sin aportar señal y podía dejar un guion entero sin
   paralaje, y donde sí sirve hace falta siempre, no una de cada cuatro veces.
3. **Sin mínimo de reacción.** Ese mínimo existe porque responder antes de lo
   humanamente posible a un parámetro SORTEADO demuestra que la respuesta venía
   pregrabada. «Acércate» no tiene parámetro que adivinar, así que la rapidez
   no delata nada — y aplicarlo convertía a quien se acerca deprisa en un
   ataque: la sesión simulada acababa en `reject` por
   `temporal_response_too_fast` sólo por añadir el paso.

Lo que cede al §6, exactamente: un atacante aprende que el bloque de destellos
empieza AHORA. Ya sabía que van al final —concesión documentada al agruparlos—
así que lo nuevo es sólo la frontera, y la aprendería un paso después al ver la
pantalla teñirse. Sigue sin saber cuántos destellos vienen, de qué colores, en
qué orden y con qué duraciones.

> **Deuda:** el suelo honesto está en torno al 3-4 % de modulación. Por debajo,
> la deriva de la cámara domina y no hay correlación que valga — y eso es lo
> correcto: un destello que aprueba con un 2 % aprobaría también una foto.
> Medido
> con una cámara real: el MISMO blanco dio 0,550 de respuesta como primer tramo
> y 0,129 como tercero, con el fondo sin moverse. La medida diferencial
> rostro/fondo cancela la ganancia global de la cámara, pero el fondo está
> lejos de la pantalla y no recibe el destello, así que no sirve de referencia
> común para lo que la cámara hace con el balance de blancos.
>
> Mientras no se arregle, un destello del que no se vio responder **ningún**
> tramo se declara no medible y lleva a reintentar. Antes acusaba: una sesión
> real, con todos los demás retos superados, acabó en rechazo por fraude. Esa
> es la razón de que exista la regla de que ningún camino de calidad pueda
> terminar en acusación.
>
> El camino que queda por probar es comparar cada tramo contra el ANTERIOR en
> vez de contra la calibración, usando `surface_face_bg_*`, que se emite ya
> justo para eso.

> **El rango de búsqueda de retardo tiene que ser MENOR que el tramo más
> corto, y hoy no lo es.** Es un defecto de diseño medido, no una sospecha.
>
> Con tramos de 350-600 ms y una búsqueda de 0-600 ms, la búsqueda puede
> **deslizar el estímulo un tramo entero** y encajar una secuencia sobre su
> contraria. Medido sobre doce ventanas reales de un mismo sujeto, cambiando
> sólo el techo de búsqueda y con los mismos frames:
>
> | ventana | secuencia real | la INVERTIDA con techo 1000 | con techo 250 |
> |---|---|---|---|
> | A | 0,965 | **0,98** | −0,38 |
> | B | 0,974 | **0,97** | 0,03 |
> | C | 0,919 | 0,72 | −0,30 |
>
> En 4 de 12 ventanas una secuencia EQUIVOCADA puntuaba más que la emitida.
> Eso no es medir la respuesta a un color: es encajar una onda cuadrada donde
> quepa. Y explica el control: buscando la secuencia real en tramos donde la
> pantalla estaba neutra, el 29 % pasaba de 0,30 con el techo actual y el 39 %
> con el techo a 1000.
>
> Estrechar el techo por sí solo no vale, porque entonces no se alcanza el
> retardo real: medido, el retardo entre revelar y ver la respuesta es de
> 160-300 ms por el lado del cliente MÁS la bajada de red, y con el techo en
> 600 los mejores retardos en vivo salían pegados al borde —340, 400, 480,
> 560— y las mismas ventanas que offline correlacionan a 0,92-0,99 daban en
> vivo −0,52 · −0,18 · 0,17.
>
> **Arreglado con duraciones desiguales, NO alargando los tramos.** Las dos
> duraciones de una secuencia difieren al menos **100 ms**, y con eso ningún
> desplazamiento encaja una secuencia sobre su contraria. Medido sobre 27
> ventanas reales:
>
> | \|d1−d2\| | real | inversa |
> |---|---|---|
> | 27 ms | 0,974 | **0,852** |
> | 29 ms | 0,919 | **0,718** |
> | 34 ms | 0,754 | 0,143 |
> | ≥36 ms | 0,94-0,99 | −0,38 a 0,35 |
>
> La otra salida —alargar los tramos por encima del rango de búsqueda— se
> probó en producción y **salió al revés**: con tramos de 800-1400 ms la
> correlación de caras reales se hundió de 0,92-0,99 a **0,049 y 0,014**.
>
> La razón es física y se midió mirando DENTRO de un tramo: la respuesta del
> rostro sube hasta un máximo hacia los **680 ms** y luego decae, y en el
> tramo siguiente el canal del color nuevo llega a irse un **12 % al lado
> contrario** — la cámara ya había compensado el color anterior con su balance
> de blancos y al cambiar se desanda. Un tramo largo no mide la piel: mide a
> la cámara acomodándose. Es la misma razón por la que las secuencias son de
> dos colores y no de cinco, encontrada por el otro extremo.
>
> La segunda duración se **sortea del conjunto válido**, no se sortea y se
> reintenta: con la primera en mitad del rango no existe ninguna separada, así
> que el que reintenta acaba aceptando una inválida sin decirlo.
>
> Lo vigila `TestLasDuracionesDeUnDestelloSeparanBastante` sobre 2000
> semillas.
>
> Lo que sí quedó demostrado por el camino: **la cara responde**. Recalculando
> las doce ventanas de un día con los frames de la grabación y el ancla en su
> sitio, las doce correlacionan entre 0,92 y 0,99. La señal existe y se estaba
> perdiendo en la alineación.

**Dos destellos seguidos no se pegan por el mismo color.** Cada secuencia
alterna por dentro, pero nada impedía que una acabara en rojo y la siguiente
empezara en rojo. Medido en una sesión real: 355 ms + 424 ms de rojo continuo,
y en el segundo destello la cara se **des-enrojecía** durante su propio tramo
rojo — la cámara ya se había acomodado del todo en el tramo unido. Es la misma
razón de la regla siguiente, que existía dentro de una secuencia y faltaba
entre secuencias. Lo vigila `TestDestellosSeguidosNoRepitenColorEnLaJuntura`
sobre 3000 semillas.

**Las secuencias son de DOS colores.** No es que sobren tramos: es que cada
tramo extra estropea la medida, porque el destello adapta la exposición de la
cámara y a partir del segundo se está midiendo la cámara acomodándose. Medido
sobre diez ventanas de grabaciones reales, en las diez dos tramos correlacionan
mejor que cualquier prefijo más largo — una bajaba de 0,815 a 0,198 al llegar
al quinto. La variación se conserva en qué dos colores, en qué orden, con qué
duraciones, y cuántos destellos hay.

**Las secuencias no llevan blanco.** Sólo primarios saturados, y por dos
razones que apuntan al mismo sitio.

El blanco **no lleva información cromática**: enciende los tres canales, que
es el estado menos informativo que puede devolver un rostro. Su papel era ser
la referencia, pero la referencia ya la da la fase de calibración con pantalla
neutra, que es exactamente para lo que existe.

Y es el color que más luz emite, así que es **el que más adapta** la exposición
y el balance de blancos de la cámara — y esa adaptación se come los tramos
siguientes, que son los que sí discriminan. Medido con una cámara real, en una
secuencia blanco-verde-blanco-rojo: los dos primeros tramos midieron y los dos
últimos no.

El orden de preferencia sigue la reflectancia de la piel viva: **rojo** (la
piel lo devuelve con fuerza), **verde** (a medias), **azul** (poco). Esa
asimetría es además parte del discriminante — la literatura de PAD activo
describe que una reflexión dominada por azul cuando se esperaba rojo delata
una superficie que no es piel.

### Las dos señales que miran a la cámara

Todo lo demás de esta sección pregunta si hay una cara viva delante. Estas dos
preguntan otra cosa: **si hay una cámara**. Distinguen un sensor físico de un
flujo de píxeles inyectado, que es una clase de ataque que puentea la
superficie de presentación entera — sin papel, sin pantalla, sin moiré, sin
bisel, y con los retos activos superados por una persona real cuya cara se
sustituye en vuelo. Ninguna otra señal del sistema la ve.

**Obturador rodante** (`sensor_rolling_shutter`). Un sensor de webcam no
captura el frame de golpe: lo barre fila por fila durante los ~33 ms que dura.
Si la pantalla cambia de color a mitad de barrido, las filas de arriba quedan
expuestas con el color viejo y las de abajo con el nuevo, y ese frame lleva una
frontera horizontal. Se mide qué fracción del salto total de color cabe dentro
del propio frame de transición.

Lo que la hace distinta de todo lo demás: la fila donde cae la frontera depende
del instante EXACTO en que el servidor decidió cambiar el color. **No se puede
pregrabar**, y hereda toda la impredecibilidad que ya compra §6. Un flujo
compuesto cambia el frame entero de golpe y no tiene frontera.

**Reacción del control automático** (`sensor_agc_response`). Se mide en el
**fondo**, y ésa es toda la idea: el fondo está lejos de la pantalla y no
recibe el destello, así que en teoría no debería moverse. Se mueve igual, y
sólo puede ser por una razón — los lazos de exposición, ganancia y balance de
blancos reaccionando a que entra mucha más luz por el centro del encuadre. Eso
es una propiedad de un sensor con realimentación, y un plano de píxeles
inyectado no la tiene.

Ya estaba medido sin saberlo y archivado como deuda: el mismo blanco daba 0,550
de respuesta como primer tramo y 0,129 como tercero con el fondo quieto. Esa
caída **es** la curva de convergencia del control automático.

Se correlaciona contra la **luminancia** del estímulo, no contra la media de
los canales: la media es idéntica para los tres primarios saturados —los tres
valen 85— así que con ella el estímulo sería una constante. Un sensor reacciona
a la luz que le entra, donde el verde pesa el doble que el rojo.

Y lo que **no** prueban, que hay que tener escrito: un replay en pantalla
delante de una cámara real produce obturador rodante y respuesta 3A genuinos,
porque hay un sensor de verdad barriendo. Son un eje distinto, no uno mejor.

Las dos viajan en `raw` y **no** en las sub-métricas: están sin calibrar contra
ataques reales, y darles voto en el resumen 0-1 antes de medirlas sería el
mismo error que este repositorio ya evitó con los clasificadores pasivos. El
gateway las registra en cada ventana para poder calibrarlas con sesiones de
verdad.

### La dispersión subsuperficial

La luz no rebota en la piel viva: entra unos milímetros, se dispersa dentro del
tejido y sale por otro sitio. Y ese recorrido filtra por longitud de onda —la
hemoglobina y la melanina se comen el azul y el verde en el primer milímetro,
mientras el rojo penetra varios y vuelve a salir—. Es lo que hace que una oreja
a contraluz se vea roja, y lo que hace difícil renderizar piel creíble.

Medible sin reto nuevo: si la pantalla emite **azul** saturado, una cara viva
devuelve una componente **roja** que el azul emitido no explica. Un papel
impreso o una máscara de silicona reflejan en superficie y no la tienen.
`skin_subsurface_red` es esa fracción sobrante.

Sólo se mide en tramos que **no** son rojos: con un destello rojo, el rojo
devuelto viene del propio estímulo y no dice nada. Si la secuencia no trae
ningún tramo no-rojo, se declara no medible — que es distinto de cero.

Como las dos señales de sensor, viaja en `raw` y **sin peso en la fusión**
hasta medirla contra ataques reales. Y con una salvedad que hay que tener
delante desde el principio: la magnitud depende del tono de piel, porque la
melanina es justo lo que absorbe las longitudes cortas. Antes de darle voto hay
que medirla por tono, igual que el pulso — o repetiría el mismo defecto.

### El reto de mirada

Aparece un punto a un lado de la pantalla, se queda ahí un momento, **salta al
lado contrario**, y se mide el viaje entre las dos posiciones. Es de la **misma familia que el destello**: lo que rompe un
vídeo grabado no es que tenga o no ojos que se mueven, sino que no puede mirar
al punto que ha aparecido ahora en un sitio elegido al azar.

Tres cosas que no son opcionales:

1. **Sólo izquierda y derecha.** El eje vertical no se puede medir con una
   webcam frontal, y se intentó de tres formas antes de descartarlo:

   - la posición del iris respecto a las comisuras apenas cambia al mirar
     arriba o abajo, porque el párpado sigue al ojo — medido: 0,007,
     indistinguible del ruido;
   - la apertura del párpado sí cambia, pero es asimétrica (arriba 24-30 %,
     abajo 13-15 %) y sobre todo es un **proxy contaminado**: al girar
     mínimamente la cabeza, la distancia entre comisuras se escorza, el ancho
     del ojo baja y la apertura sube sola. Medido: 38 % de subida en una
     mirada puramente lateral;
   - y con la mirada baja el párpado tapa el iris, así que estropea también
     el canal horizontal.

   El horizontal sí se puede: 0,043 de señal contra 0,004 de ruido.

   La conversión de desplazamiento de iris a grados es **248 grados por
   unidad**, re-derivada sobre pasos de pose de grabaciones reales —donde la
   cabeza gira y la mirada se queda en la pantalla, así que el iris
   contrarrota— y **con la captura ya sin estirar**. Fueron 300 primero y 221
   después; las dos veces el número salió de frames distorsionados.

   El estirón importaba aquí más que en ningún sitio: sobre los mismos frames
   sin corregir la mediana da 201 y corregidos 248, y con la geometría buena
   **7 de 8 pasos** correlacionan a -0,85 o mejor contra 5 de 8. La constante
   no era un ajuste fino: era un error de medida.

   Y devolvió una discriminación que se había perdido. Girar la cabeza sin
   mover la mirada deja ahora 3,12° de residuo, por debajo del umbral, cuando
   con 221 dejaba 4,74° y contaba como obedecer.

   Dos opciones en vez de cuatro es un bit menos por reto y **importa poco**:
   lo que rompe un vídeo grabado no es cuántas opciones hay, sino que no puede
   responder a ninguna en el momento. Se compensa con más retos de mirada, que
   cuestan un segundo cada uno.

2. **Se mide la mirada en el MUNDO: cabeza más ojo.** El desplazamiento del
   iris dentro de la órbita dice hacia dónde miran los ojos *respecto a la
   cara*, y eso no basta. Quien gira la cabeza hacia el objetivo no mueve los
   ojos dentro de la órbita y daría cero; y quien gira la cabeza sin dejar de
   mirar al mismo sitio produce una contrarrotación enorme **en sentido
   contrario**. Medido con una cámara real: cabeza girada 18°, iris desplazado
   −0,060, y la mirada real sin moverse ni un grado.

   Sumando `pose_yaw_deg` y el desplazamiento del iris, las dos formas de
   obedecer —girar la cabeza, mover los ojos, o las dos— cuentan igual, y la
   contrarrotación se cancela sola.

3. **Una mirada nunca va justo después de una pose.** La pose deja la cabeza
   girada, y la referencia se toma de los frames anteriores al objetivo: con
   la cabeza a medio volver, esa referencia describe una postura que ya no
   existe. Se arregla intercambiando los dos pasos, no agrupando las miradas
   al principio: eso habría hecho el guion predecible, que es lo que §6
   prohíbe.

4. **Se mide el VIAJE entre los dos puntos, no la distancia a un reposo.**
   La posición de reposo del iris depende de la cara, de las gafas y de dónde
   esté la cámara respecto a la pantalla, y ninguna de las tres se conoce; con
   dos puntos, un desplazamiento propio está en las dos fases por igual y se
   cancela solo.

   Se hizo así después de que la referencia de reposo causara tres fallos
   distintos: venía desfasada (1,88° cuando el valor real era 5,79°), venía
   contaminada por la mirada anterior (−13,26°), y describía a alguien que
   miraba a un lado durante la calibración — eso último acabó en un **rechazo
   por fraude** contra un usuario legítimo, porque su vuelta al centro dio
   4,68° de «avance» en dos frames, por debajo del mínimo humano.

   Y el recorrido se **duplica**, que es lo que hacía falta en un móvil: del
   centro al borde son unos 5,7°, y de un borde al otro el doble.

   Cada fase se resume por su **mediana**, no por su pico: el pico premia un
   frame con el iris mal detectado, y de ésos hay. Se descartan los primeros
   350 ms de cada fase, porque ahí la mirada aún va en camino y medir el
   trayecto recorta el viaje por los dos extremos.

   De regalo, es más difícil de falsificar: un vídeo grabado tiene que producir
   una transición concreta en un instante concreto, no basta con estar mirando
   a un lado.
5. **Una mirada que no se pudo medir no cuenta.** Gafas con reflejo, ojos
   cerrados, cara lejos, o los dos ojos en desacuerdo. No entra en la fusión,
   ni a favor ni en contra.

   Pero «ojos cerrados» se juzga **contra la apertura habitual del propio
   sujeto**, no contra un número fijo. Un umbral absoluto convierte la anatomía
   y el ángulo de la cámara en motivo de rechazo, que es lo mismo que la regla
   4 prohíbe para el reposo. Medido sobre un usuario legítimo: su apertura iba
   de 0,045 a 0,139 y el umbral absoluto era 0,15, así que se descartaron
   **todos** sus frames y dos sesiones seguidas cerraron el reto con avance
   CERO — mientras su mirada respondía de 1,5° a 15° con los dos ojos
   coincidiendo entre 0,67 y 0,92. Reprocesada con la puerta relativa: 58 de 58
   frames aceptados, avance máximo 13,71° y el 62 % por encima del mínimo.

   Lo que la puerta debe rechazar es un **parpadeo**, que es una caída brusca
   respecto a lo normal de esa persona. Por eso es la mitad de su mediana
   reciente, más un suelo de existencia para cuando ya no hay ojo.

   Y el descarte lo hace **Go, no Python** (§3): descartar una medida es
   decidir. Python mide y publica `gaze_openness`; su único suelo es el de
   «aquí ya no hay párpado abierto».
6bis. **La ventana de reposo NO se alimenta durante un reto de mirada.**
   Mientras se mide una mirada el sujeto está mirando al punto, no en reposo.
   Metiendo esos frames, el siguiente reto toma como reposo la posición
   desplazada del anterior: medido, con dos miradas seguidas a lados opuestos
   la segunda arrancó con reposo −13,26° y el sujeto, al volver la vista al
   centro, produjo 13,26° de avance en DOS frames. Por debajo del mínimo
   humano, que es motivo **duro** — así que la sesión acusaba de ataque a quien
   sólo estaba obedeciendo.

   Se arregla en la fuente y no en el guion: prohibir dos miradas seguidas
   choca con la regla de no poner una mirada tras una pose, y con muchas
   miradas y pocos pasos de otro tipo las dos no pueden cumplirse a la vez.

6. **La referencia se toma ANTES de revelar el objetivo.** El punto late y la
   gente reacciona en uno o dos frames, así que los primeros frames del paso
   ya están dentro del movimiento. Medido: reposo verdadero +0,023, base
   sacada del propio paso +0,008, y media respuesta perdida. El gateway
   mantiene una ventana de medio segundo de miradas recientes, siempre, y
   entrega su mediana —no la media: un parpadeo se la llevaría— al empezar
   un reto de mirada.
7. **Dos miradas seguidas nunca piden el mismo lado.** Si el objetivo no se
   mueve, el sujeto ya está mirando ahí y no hay nada que medir: medido, dos
   objetivos iguales seguidos dieron 0,000 de desplazamiento y el reto se cayó
   por hacer exactamente lo que se pedía.
7bis. **El umbral de avance depende del TAMAÑO DE LA PANTALLA, y hoy es 3°.**
   El objetivo se pinta al 6 % del ancho del viewport, así que el ángulo que
   hay que recorrer no es el mismo en todas partes: un portátil de ~30 cm a
   50 cm pone el punto a unos 15°, y un móvil de ~7 cm a 30 cm lo pone a unos
   **5,7°**. Medido con el mismo sujeto el mismo día: 6-13° de avance en el
   portátil y 3,18° y 4,00° en el teléfono, fallando las dos veces con el
   umbral en 5.

   Bajarlo a 3 tiene un precio medido, y conviene tenerlo escrito en vez de
   descubrirlo. Sobre los tramos SIN objetivo de cuatro grabaciones reales
   (931 muestras), donde cualquier disparo es falso: con 5° dispara solo el
   4,0 % de las ventanas y con 3° el **9,1 %**. Y 3° queda por debajo de la
   deriva máxima con la cabeza quieta (3,76°). Además dejan de distinguirse
   dos casos que antes se rechazaban: la contrarrotación —girar la cabeza sin
   mover la mirada deja 4,74° de residuo, porque cancelarla exigiría la escala
   vieja de 300 y la medida real es 221— y un desplazamiento de 0,015, que es
   poco más que acomodarse. Lo vigila `TestMiradaSigueAlLado`, que los tiene
   marcados como *ya no se distinguen*.

   Lo que sigue protegiendo es la **dirección**, que se sortea justo antes, y
   el sostenimiento durante 250 ms. La forma correcta de arreglarlo no es este
   número sino escalar el umbral con el ángulo que el objetivo subtiende de
   verdad — y eso no puede salir de un dato que declare el cliente, porque
   sería un campo controlado por el atacante que RELAJA un umbral.

7ter. **Hace falta haber visto el ANTES dentro del propio paso.** Un avance
   por encima del umbral no basta: tiene que haber al menos una muestra por
   debajo antes que él. Una respuesta es un CAMBIO y tiene un antes; una
   referencia de reposo equivocada aparece ya desplazada en la primera
   muestra, y las dos producen exactamente la misma firma.

   Sin esto se acusaba a usuarios legítimos, y es un motivo **duro**: medido,
   el sujeto miraba 6,49° a su izquierda durante la calibración, el objetivo
   salió a la derecha, y su vuelta natural al centro dio 4,68° de avance en
   DOS frames. Por debajo del mínimo humano, así que la sesión se resolvió en
   `reject` con `temporal_response_too_fast` + `attack_no_gaze_response`.

   El precio es que una respuesta genuinamente instantánea —cuyo primer frame
   ya la recoge— no se acepta. Pero entonces sale como no cumplida y lleva a
   **reintentar**, que es lo que debe pasar cuando no se ha podido medir, en
   vez de a una acusación.

8. **El reparto entre las dos fases se BUSCA, no se supone.** Entre que el
   servidor revela el punto y que el frame que muestra la respuesta está en
   sus manos hay red de bajada, pintado del cliente, reacción, captura y red
   de subida. Es exactamente el mismo argumento que obliga al destello a
   buscar su retardo, y aquí faltaba.

   Costó una sesión legítima entera. El móvil pedía **18,5 Mbit/s** de subida
   —155 KB por frame a 17,9 fps— y el enlace no daba, así que los frames no se
   perdían: llegaban **tarde, y cada vez más**. Medido comparando el sello de
   cada mensaje en el cliente con la hora a la que el gateway lo mandó, el
   retraso creció **2,0 s en 5,4 s de sesión**. Con el reparto fijo, la fase
   de salida se llenó con la mirada de LLEGADA y el avance salió cero;
   reprocesada con el reloj del propio cliente, la misma persona daba
   **6,16°**.

   Un frame que llega tarde no es un frame perdido: es un frame que **miente
   sobre cuándo pasó lo que muestra**. Y eso no lo ve nadie, porque el
   síntoma es idéntico al de alguien que no responde.

   Se busca hasta 0,8 s y no más, porque la cola sin tope se arregla donde
   estaba —en el cliente, que ahora se salta el turno si el socket aún tiene
   un frame esperando, igual que el buzón de un solo hueco del gateway—. Un
   retardo de dos segundos no se arregla buscándolo: para entonces la
   respuesta llega después del plazo del reto.

9. **Una mirada que no se vio no se puntúa.** Si entre la última muestra
   antes del salto y la primera de después hay más de 0,7 s, el reto sale como
   **no medible**, no como respuesta nula. El salto del punto es el instante
   que hay que observar; sin frames alrededor, lo que se mide no es al sujeto.

   Medido: el móvil dejó de capturar **1,8 s justo en el salto** —el enlace se
   atascó— y el reto se cerró con avance CERO y entró en la fusión como
   respuesta nula. La persona pudo haber obedecido y haber vuelto la vista
   antes del frame siguiente, y no hay forma de saberlo. Ésa es justo la razón
   de no puntuarlo.

10. **El avance hay que sostenerlo un cuarto de segundo, no tres frames.** En
   el 60 % de las muestras de ese tramo, y no seguidas: con el umbral cerca de
   lo que produce una respuesta real, el ruido hace que alguna baje por debajo,
   y exigir una racha limpia tiraba respuestas correctas —medido, catorce
   frames claramente por encima y ninguna racha de tres—. Un pico aislado
   sigue sin valer, que es lo que la regla protege.

   En **tiempo** y no en frames porque el ritmo de cámara varía al doble según
   la luz de la habitación: medido sobre una grabación real, a 720p la webcam
   alargaba la exposición y entregaba 13,6 fps donde se le habían pedido 30.
   Cinco frames son 185 ms o 370 ms según el sitio, así que contar frames
   exigía una cosa distinta a cada persona.

La referencia vertical son las **comisuras del ojo, no los párpados**. Los
párpados siguen a la mirada —al mirar abajo bajan los dos—, así que el iris se
queda centrado entre ellos y la componente vertical se cancela sola. Con una
cara real eso daba 0,002 de desvío donde tenía que haber diez veces más. Las
comisuras están fijas al cráneo, y usar su eje compensa además el balanceo de
cabeza.

El cliente exige además un **encuadre mínimo antes de empezar**: el iris se
mide en píxeles, así que cada centímetro que la persona se acerca es señal
gratis. Esa puerta es del cliente y el servidor no se fía de ella; saltársela
sólo empeora las propias señales.

### PAD pasivo por textura

Un clasificador pequeño (MiniFASNet, Apache-2.0, ~0,43M parámetros por
variante) que mira un solo frame y devuelve probabilidades de ataque. Corre
por ONNX Runtime en CPU, que es donde el §2 dejó dicho que entrarían los
modelos de PAD. **1,9 ms** por frame las dos variantes juntas; el total del
pipeline queda en 10,2 de los 30 de presupuesto.

Complementa, no sustituye. Todo lo demás aquí es reto-respuesta activo: lo
pasivo pilla una impresión excelente que se queda quieta, y lo activo pilla un
replay bueno, que no puede responder a un color emitido ahora ni mirar al
punto que acaba de aparecer.

Dos variantes con recortes de distinta amplitud, y no es la misma medida
repetida: la ancha abarca el entorno —donde se ven bordes de papel y biseles
de pantalla— y la estrecha se queda en la piel. Por eso mismo la ancha es la
que más se debilita ante un replay a pantalla completa sin marco visible, que
es justo el ataque A3.

> **Hoy el destello no puede acusar a nadie, y conviene tenerlo delante.** Sus
> tres vías se remontan a la misma correlación: `flash_correlation`
> directamente; `flash_screen_absence` por el término «brilla y es sordo», que
> multiplica el brillo por lo que le FALTA a la correlación; y
> `flash_gradient_3d` porque comparte sus amplitudes. Sobre caras REALES de
> webcam la correlación ha dado 0 · 0,090 · 0,1356 · 0,4934 · 0,703 · 0,815 ·
> 1,0, así que ninguna de las tres separa nada, y con los suelos puestos
> acusaban: una sesión legítima —dos miradas aceptadas, dos poses con paralaje
> 1— acabó en `attack_no_color_response` + `attack_identity_change`.
>
> Ahora una correlación que no llega marca la ventana como no medida y ninguna
> sub-métrica suya entra en la fusión. El destello sigue **pesando en la
> media**; lo que ha perdido es el veto.
>
> El brillo a solas tampoco sirve, y la razón ya estaba escrita en el propio
> detector: alguien de piel clara en una habitación a oscuras también brilla.
> Y sin el término de correlación, la sospecha de pantalla de la escena
> sintética cae a 0,09 — o sea que el reflejo especular, el moiré y el bandeo
> no aportan nada ahí. Están sin medir contra pantallas reales, que es la
> siguiente medida pendiente junto con hacer medible la correlación.

**Se publican las probabilidades de ATAQUE, nunca la de "real".** Concluir que
no hay ataque es una decisión, y las decisiones son de Go.

**Los dos votan por separado**, con su peso cada uno, y así aparecen en la
fusión y en la tabla del modo pentest. Colapsarlos en un número ahorraría una
fila y perdería lo único interesante: en qué se contradicen. Medido sobre el
mismo frame, el estrecho dio 0,98 de ataque y el ancho 0,24 — y esa
discrepancia es exactamente el dato que dirá cuál de los dos sirve contra qué
ataque.

> **Sin calibrar contra ataques reales.** Hoy sólo está medido lo que NO debe
> hacer: sobre 80 frames de una cara real dio 1,000 de probabilidad de real,
> cero falsos rechazos. Que acierte con una foto impresa o un replay está sin
> comprobar, y hasta comprobarlo su peso en la fusión es cero. El repositorio
> de origen no publica ni datos de entrenamiento ni métricas, y los modelos de
> esta familia se entrenaron sobre poblaciones estrechas: medirlo por tono de
> piel antes de darle voto no es opcional, es la misma trampa que evita la
> calibración del destello.

### El modo pentest

`GATEWAY_INSECURE_EXPLAIN_VERDICT` manda al cliente el desglose por detector:
qué midió cada uno, su suelo, su peso y si pasó. El cliente lo pinta en una
tabla.

**Contradice el §6 a propósito y sólo puede estar puesto en pruebas.** En
producción al cliente se le dice `try_again` y nada más, porque contarle a un
atacante qué detector le pilló y por cuánto es entregarle el bucle de
realimentación que necesita para afinar. Existe porque quien hace un pentest
necesita ver contra qué pelea, y porque es la forma de saber qué detector se
traga cada ataque.

La tabla distingue **tres estados, no dos**: pasó, falló, y **no medido**. El
tercero es el que más se pasa por alto: una señal que no se pudo medir no
entra en la fusión, ni a favor ni en contra, y confundirla con un fallo es
convertir una cámara mala en una acusación.

### La puerta de calidad

Antes de acusar a nadie hay que haber podido medir, y eso se comprueba con
cuatro términos de los que manda **el peor**: cobertura de rostro, nitidez,
exposición y quemado.

El quemado se mide **dentro del rostro**, no sobre el encuadre. Una ventana
detrás del sujeto quema medio frame sin afectar a su cara: medido con una
cámara real, 10,4 % del encuadre contra 0,53 % del rostro, veinte veces menos.
Con la medida del encuadre, una sesión que había superado **todos** los retos
—score 0,95 contra un umbral de 0,75— se resolvía en reintentar por calidad.

Es la misma lección que el destello ya había aprendido: medir donde importa y
no en absoluto sobre toda la escena. Que dos partes del sistema tropiecen con
la misma piedra sugiere mirar con lupa cualquier medida que promedie el
encuadre entero.

### El pulso

Hay un analizador más que no cuelga de ningún reto: mide el **pulso sanguíneo**
en el color de la piel (`analyzer/rppg.py`, método POS). Cada sístole cambia la
absorción de luz verde del rostro unas décimas de punto; es invisible al ojo y
lo capta una webcam.

Está porque es lo único que una **máscara** no puede fingir. Todo lo demás de
§5 delata una *superficie*, y una máscara de silicona no lo es: tiene volumen,
gira con la cabeza y devuelve relieve al destello. Medido en `/bench`, se cuela
en el 55-73 % de los casos. Lo que no tiene es corazón.

Se mide **contra el fondo del mismo frame**, como todo lo demás del sistema.
Fue el tercer tropiezo con la misma piedra: el camino del pulso era el único
que usaba valor absoluto en vez de `region_ratio()`. Importa porque POS ya
cancela la deriva común a los tres canales —la auto-exposición— pero no el
**balance de blancos**, que mueve cada canal por su lado y cuya oscilación cae
dentro de la banda del pulso. Medido con el POS real, una deriva de balance de
blancos del 1 % hundía el AUC de 0,71 a **0,42**: por debajo de 0,5, o sea que
la máscara puntuaba más que el rostro vivo.

**El pulso puede absolver, nunca acusar**, y esto no es opcional. El SNR del
rPPG depende del tono de piel tanto como de que haya latido: la melanina está
por encima del lecho capilar y atenúa los fotones que vuelven. Medido (Nowara
et al., CVPRW 2020), POS da entre +0,05 y +1,76 dB para los tipos Fitzpatrick
I-V y **−5,58 dB para el VI**, con el suelo de ruido de la métrica en torno a
−6,5. Para una piel muy oscura la salida es indistinguible de no haber medido,
así que **no existe umbral que separe una máscara de una persona de piel
oscura**. Un suelo aquí rechazaría por tono de piel y lo llamaría fraude.

Por eso `rppg_snr` está en la lista de señales que no admiten suelo y el perfil
no se carga si alguien le pone uno. Sigue pesando en la media ponderada: un SNR
alto demuestra que hay pulso —y eso vale para cualquiera—, pero uno bajo no
puede vetar.

Y hace falta la otra mitad, que se descubrió tarde: **una ventana de pulso no
medible tampoco puede forzar un reintento.** La regla del suelo impide que el
pulso RECHACE; sin esta segunda, seguía pudiendo BLOQUEAR. Medido sobre una
sesión real: score 0,9154, doce de trece señales medidas, el destello
correlacionando a 1,0, y resuelta en reintentar sólo porque no se pudo medir el
pulso. Con piel muy oscura eso sería un bucle de reintentos — un rechazo
disfrazado de «inténtalo otra vez», que además no se puede recurrir porque
nunca se declara.

Las dos reglas salen del mismo sitio —que la señal no significa lo mismo para
todo el mundo— y tenerlas separadas fue justo lo que dejó el hueco. Ahora
`optionalWindow` las ata: si ninguna señal de una ventana admite suelo, su
ausencia no exige nada.

Consecuencia honesta que hay que tener delante: con webcam RGB de consumo, A4
**sigue sin cubrirse para ese grupo** aunque se le dé un tramo tranquilo al
guion. La alternativa que merece un experimento es la balistocardiografía
—el micro-movimiento de cabeza por la eyección sanguínea—, que no depende de
la reflectancia de la piel y cuya brecha claro-oscuro medida es de 0,99 lpm
frente a 4,97 de los métodos cromáticos.

Tres reglas suyas:

1. **Hacen falta diez segundos seguidos.** No es un número de catálogo: la
   resolución en frecuencia de una FFT es 1/T, y con cinco segundos el pulso de
   una persona y el de la siguiente caen en el mismo bin. Por debajo de ese
   tramo el analizador **dice que no puede medir**.
2. **Dentro de un destello no se mide.** La señal que se busca son décimas de
   punto y el destello mueve el color órdenes de magnitud más: no la ensucia,
   la tapa. Go elige el tramo más largo sin destellos y pide ese.
3. **Un SNR bajo por sesión corta no es una máscara.** Son indistinguibles si
   se puntúan igual, y confundirlos rechaza a personas vivas. Por eso lo no
   medible sale de la fusión en vez de entrar como cero.

Para que se pueda medir, el guion trae un **tramo de quietud** de once
segundos (`KindHold`): se le pide al sujeto que no se mueva y se le pinta una
cuenta atrás. No es un reto —no hay nada que responder, y un vídeo grabado lo
supera sin esfuerzo— sino una ventana de medida, como la calibración. Por eso
no cuenta para `MinSteps` ni `MaxSteps`: no le quita el sitio a ningún reto.

Hizo falta porque reordenar el guion no bastaba. Los destellos van agrupados al
final (`groupFlashesLast`) y limitados a dos, lo que en el peor caso dejaba
delante 17 s; pero eso se calculó sobre los PLAZOS, no sobre lo que tarda una
persona. Medido sobre una sesión real, el sujeto respondía a cada reto en poco
más de un segundo y **la sesión entera duraba 8,3 s**. El hueco existía en el
modelo y no en la realidad.

El tramo se coloca en posición sorteada entre los retos que no son destello.
No defiende nada por sí mismo, pero sortear su posición es barato y le quita al
atacante saber exactamente cuándo se le mide el pulso, que es el único momento
en que le serviría fingirlo.

Cuesta lo que cuesta: la sesión pasa de 8 s a unos 19, y el presupuesto de
sesión de 45 s a 60 para cubrir el peor caso.

Eso cede algo de §6 a propósito, y conviene ser explícito sobre qué:

- **Se cede:** un atacante sabe que los destellos llegan al final.
- **No se cede:** cuántos son (uno o dos), de qué colores, con qué duraciones,
  cuándo empiezan, ni cuántos pasos hay antes ni de qué tipo.

Lo que rompe un vídeo grabado no es ignorar el ORDEN de los tipos de reto: es
no poder responder a una secuencia de colores emitida ahora ni mirar a un punto
que acaba de aparecer. Eso sigue intacto, y lo vigila
`TestScriptStaysUnpredictable`.

Es la decisión contraria a la que §4 tomó con las miradas —allí se prefirió
intercambiar dos pasos antes que agruparlas al principio— y la diferencia es
qué se gana: allí, evitar una referencia mala; aquí, la única contramedida que
existe contra máscaras de silicona.

### El motor de decisión

Convierte la línea de tiempo de medidas en un veredicto. Vive en
`orchestrator/core/fusion` y se rige por tres reglas:

**Pesos a mano, no aprendidos.** Todavía no hay modelo, y es deliberado:
primero hay que entender qué aporta cada señal, y para eso hace falta poder
leer por qué salió cada veredicto. Un modelo entrenado con los datos que aún no
tenemos sería un generador de decisiones que nadie sabe defender.

**Tres desenlaces, no dos.** Aprobado, rechazado y **reintentar**. Calidad
insuficiente NO es ataque detectado: rechazar es acusar a alguien de intentar
engañar al sistema, y que su cámara sea mala o su habitación esté iluminada no
es eso. Ningún camino de calidad puede acabar en rechazo, y hay un test que lo
comprueba para todos ellos.

**Todo rechazo se explica.** Qué señal falló, con qué valor y contra qué
umbral, más la versión y el resumen del perfil que decidió. Sin eso, un
veredicto del mes pasado no se puede reproducir ni recurrir.

Además de la media ponderada hay **suelos por señal**: una señal por debajo del
suyo veta sola, sin importar el resto. Existen porque hay evidencias que no se
compensan — que el destello no produzca relieve no lo arregla haber girado muy
bien la cabeza. Un suelo sólo puede acusar: si su motivo fuera de la familia
`quality`, el perfil no se carga.

Y una regla que atraviesa todo el sistema: **una señal que no se pudo medir no
entra en la fusión**. Meterla como cero sería fabricar evidencia contra quien
simplemente no llegó a girar la cabeza.

## 5. Modelo de amenaza

El sistema debe distinguir a una persona real presente de estos tres ataques de
presentación. Están ordenados por dificultad creciente.

### A1 — Foto impresa

Papel o cartulina con un rostro, posiblemente recortado en ojos/boca.

| Señal | Por qué funciona |
|-------|------------------|
| Textura / micro-textura (LBP, ruido de sensor) | El papel tiene grano y pierde el ruido del sensor original. |
| Ausencia de micro-movimiento facial | Un rostro real nunca está perfectamente rígido. |
| Profundidad monocular / paralaje | Superficie plana ante movimiento de cabeza. |
| Reflejo especular | El papel refleja de forma difusa y uniforme. |
| Ausencia de parpadeo espontáneo | — |
| rPPG plano | Sin pulso sanguíneo en la señal cromática. |

Retos efectivos: girar cabeza, acercarse/alejarse.

### A2 — Foto en pantalla

Imagen estática mostrada en un móvil, tablet o monitor.

| Señal | Por qué funciona |
|-------|------------------|
| Patrón de moiré | Rejilla de píxeles muestreada por la cámara. |
| Bandas de refresco / *rolling shutter* | Frecuencia del panel vs. exposición. |
| Marco / bisel detectado | Bordes rectos y contraste alrededor del rostro. |
| Reflejo especular puntual | Las pantallas son emisoras y muy especulares. |
| Gama de color y saturación fuera de rango | Reproducción de segunda generación. |
| Planaridad | Igual que A1. |

Retos efectivos: reto de iluminación activa (el cliente pinta la pantalla de un
color impuesto por el servidor y el analyzer mide la respuesta cromática del
rostro).

### A3 — Video replay en pantalla

El ataque serio: un vídeo pregrabado del usuario legítimo reproducido en una
pantalla. Derrota parpadeo, micro-movimiento y a veces rPPG.

| Señal | Por qué funciona |
|-------|------------------|
| Moiré + bisel + banding | Sigue siendo una pantalla (todo lo de A2). |
| Consistencia temporal de iluminación activa | Un vídeo grabado **no puede responder** a un color aleatorio emitido *ahora*. Implementado: `analyzer/flash.py`. |
| Latencia de respuesta al reto | El humano responde en una ventana estrecha; el vídeo, nunca o siempre igual. |
| Doble compresión / huellas de códec | El frame ya fue codificado una vez. |
| Coherencia de flujo óptico global vs. facial | El plano de la pantalla se mueve en bloque. |
| rPPG con SNR anómalo | Sobrevive parcialmente a la recompresión, con firma distinta. |

**Contramedida principal contra A3: retos impredecibles y ligados al tiempo.**
De ahí la regla de la sección 6.

### A4 — Máscara 3D (parcialmente cubierto)

Estaba fuera de alcance y ha dejado de estarlo del todo, así que conviene
decir exactamente hasta dónde llega la cobertura.

Una máscara de silicona o látex derrota casi todo lo anterior: tiene volumen,
así que da paralaje y responde al destello con relieve; no es una pantalla, así
que no hay moiré ni bandeo. Medido en `/bench`, los clasificadores pasivos
dejan pasar entre el 55 % y el 73 % de las máscaras de silicona.

| Señal | Por qué funciona |
|-------|------------------|
| rPPG (`rppg_snr_db`) | La silicona no tiene riego sanguíneo. Es la única señal que no depende de que la máscara sea una superficie. |

El guion le reserva sitio: los destellos van agrupados al final, así que queda
por delante un tramo seguido de 17 s de mediana. Con captura de 720p a 30 fps
eso da una probabilidad de distinguir rostro vivo de superficie en torno a
0,92 sobre señal sintética. **Sin calibrar contra máscaras reales**: es la
siguiente medida pendiente.

### Fuera de alcance en esta fase

Deepfakes en tiempo real e inyección en el driver de la cámara
(*camera injection*). Se documentan aquí para que nadie asuma que están
cubiertos. La defensa contra inyección es de otro plano (attestation del
cliente) y se abordará por separado.

---

## 6. Regla del guion de retos (crítica)

> **El cliente NUNCA conoce el guion de retos completo.**

- El guion (secuencia, parámetros, tiempos, semilla) se genera **en el
  orchestrator**, se guarda en Redis y **jamás se serializa entero hacia el
  cliente**.
- El cliente recibe **un reto a la vez**, y sólo tras haber cerrado el anterior
  (cumplido, fallado o expirado).
- El cliente no recibe: la longitud del guion, el índice del reto actual, los
  retos futuros, la semilla, los umbrales, ni los scores parciales.
- Los parámetros del reto se materializan en el momento de emitirlo, no antes.
- Ningún mensaje hacia el cliente puede contener campos de los que se pueda
  inferir lo anterior (incluido inferir por tamaño de mensaje o por timing
  constante).
- El cliente no reporta "he completado el reto". El cliente sólo envía frames.
  **Quien decide si el reto se cumplió es el servidor**, a partir de las señales
  del analyzer correlacionadas con la ventana del reto.

Motivo: cualquier filtración del guion convierte A3 (video replay) de ataque
difícil en ataque trivial y automatizable.

Corolario para revisores: si un PR añade un campo al protocolo de cliente,
la primera pregunta es *"¿esto le dice al atacante algo del futuro?"*.

### El guion es aleatorio; las defensas no

Que el guion se sortee no significa que se sortee **qué se puede medir**. Cada
señal de la fusión tiene un prerrequisito en el guion, y todos van
garantizados:

| Señales | Necesitan |
|---------|-----------|
| `pose_compliance`, `pose_continuity`, `pose_parallax`, `pose_identity` | una pose **angular** |

> **`pose_identity` no puede acusar, y no es afinable.** Sobre un sujeto
> legítimo, ventana a ventana: 0 · 0,1178 · 0,2239 · 0,3335 · 0,3834 · 0,6582 ·
> 0,7499 · 0,8301 · 0,9832. Con suelo en 0,35 rechazó **dos** sesiones reales
> por `attack_identity_change`, las dos con las miradas aceptadas y el paralaje
> en 1.
>
> Se intentó arreglar el estadístico tres veces —caída sostenida, promedio de
> mitades, cruce local del hueco— y la tercera reveló por qué no se puede: en
> la grabación de una de esas sesiones, los frames a un lado y otro del peor
> hueco se parecen **0,357** en una pose y **0,567** en la ventana de QUIETUD,
> donde el sujeto no se movía. Una sustitución por otra cara real daría valores
> del mismo orden. La variación legítima de este embebedor ya solapa con lo que
> se quiere detectar, así que no hay umbral posible.
>
> Dato útil que salió de ahí: la MEDIANA de similitud se mantiene en 0,96-0,98
> incluso girando 25°, así que el embebedor no es sensible a la pose. Lo que
> hay son caídas aisladas del detector, y también en la quietud.
>
> Sigue pesando 0,03 en la media. Lo que pierde es el veto — y con él, la
> detección de sustitución de cara a mitad de sesión, que estaba declarada
> **fuera de alcance** en §5 de todas formas. `TestDecisionTable` lo tiene
> escrito para que nadie lo descubra por sorpresa. Recuperarlo exige un rasgo
> invariante o medir la identidad sólo en tramos sin rotación ni glitches.
| `flash_correlation`, `flash_gradient_3d`, `flash_screen_absence` | un destello |
| `gaze_response` | una mirada |
| `rppg_snr` | el tramo de quietud |

Dejarlo al azar es no tener esa defensa una parte de las veces, y encima sin
que nadie se entere: un veredicto sale igual de convincente con once señales
que con trece. Medido antes de arreglarlo, sobre 5000 semillas:

- al **8,4 %** de los guiones les faltaba la mirada;
- al **22 %** les faltaba una pose angular, y con ella el paralaje. Acercarse
  no vale: no es un ángulo, no produce ventana de pose, y la única pose de un
  guion podía ser ésa.

Lo que sigue sorteado es todo lo demás: cuántos pasos hay, cuántos de cada
tipo por encima del mínimo, en qué orden, qué ángulo, qué lado, qué colores y
con qué duraciones. `TestTodoGuionPermiteMedirTodo` es el criterio: si se añade
una señal a la fusión, su prerrequisito se declara ahí.

### Cómo se hace cumplir en el código

La regla no vive en la buena voluntad de quien escribe el gateway, sino en los
tipos del núcleo (`orchestrator/core/`):

| Paquete | Papel |
|---------|-------|
| `clock` | Única fuente de tiempo. El núcleo **nunca** llama a `time.Now()`: recibe un `Clock`. Por eso todo es determinista en tests y reproducible en `/bench`. |
| `fsm` | Máquina de estados pura. Transición inválida = error explícito, jamás un panic, y el estado no se toca. |
| `challenge` | Generación del guion desde la semilla. `Script` es de **avance único**: expone el paso actual y nada más. No hay acceso por índice, ni longitud, ni iteración. |
| `session` | El agregado. Su único método que revela guion es `Reveal()`, y devuelve el paso **activo** como `RevealedStep`. |
| `fusion` | El motor de decisión: convierte la línea de tiempo de medidas en un veredicto explicado. Puro salvo `loader.go`, que carga el perfil de disco. |
| `retry` | Política de reintentos por identificador, con backoff y escalado a revisión manual. |

`RevealedStep` es un tipo distinto de `challenge.Step` a propósito: no tiene
índice, ni total, ni semilla, ni la ventana mínima de reacción. No es que no
se rellenen esos campos — es que **no existen**, así que ningún descuido
posterior puede filtrarlos.

Tres consecuencias que hay que respetar:

1. **El mínimo de reacción no se revela nunca.** El cliente recibe el plazo
   (`deadline_ms`), no el umbral por debajo del cual su respuesta se considera
   imposible. Revelarlo le da al atacante el margen exacto en el que debe
   responder.
2. **Un paso rechazado cierra la fase de retos.** Se falla en cerrado: no se
   deja al atacante seguir probando pasos para sonsacar el guion.

   Pero cerrar la fase **no es completar el guion**, y confundirlo abrió un
   agujero real: las dos cosas llevan la máquina de estados al mismo sitio, así
   que `require_completed_challenges` daba por completo un guion cortado a la
   fuerza. Una sesión de 4,6 segundos, con la calibración no medible y su
   único reto FALLADO, se resolvió en **pass con 0,902** decidiendo con la
   evidencia de dos pasos en vez de ocho. Ahora la fusión sabe distinguirlas.
3. **Lo que huele a ataque no se reintenta.** Una respuesta más rápida que el
   mínimo humano es un motivo *duro*: manda sobre un `pass` de la fusión y
   resuelve en `fail`. Un reintento sería una tirada más para el atacante.

Los tests `orchestrator/core/session/leak_test.go` son el criterio de
aceptación de todo esto: recorren la superficie pública por reflexión y
comprueban, sesión real en marcha, que ningún método menciona un paso que
todavía no toca. Están en el paquete de test **externo** para ver sólo lo que
verá el transporte.

### El gateway

Tres goroutines por conexión, comunicadas sólo por canales:

| Goroutine | Papel |
|-----------|-------|
| `readLoop` | Lee del socket, aplica los límites duros y encola. Nunca se bloquea por culpa del análisis. |
| `runLoop` | Dueño de la sesión: analiza, consulta la máquina de estados y produce mensajes. |
| `writeLoop` | **Única** goroutine que escribe en el socket, cierre incluido. |

Leer y procesar están separados a propósito: si fueran lo mismo, el
backpressure sería implícito —se acumularía en TCP— y no se podrían descartar
frames intermedios, que es justo lo que hace falta. El buzón de frames tiene
**un solo hueco**: cuando llega uno nuevo y el anterior sigue sin procesar, se
tira el viejo y se cuenta. Nunca hay cola que crezca.

El cierre también pasa por el escritor. Si el `runLoop` cerrara el socket por
su cuenta habría dos goroutines escribiendo, que es exactamente lo que la
regla prohíbe.

Reglas que este paquete hace cumplir:

1. **El sello que puntúa es el del servidor**, puesto al recibir el frame (no
   al procesarlo: la espera en el buzón no es culpa del usuario). El sello del
   cliente se guarda, se valida su deriva y sólo sirve para alinear.

   Con una excepción: el **límite de caudal** se mide sobre el sello del
   CLIENTE cuando su reloj es plausible. Una red con jitter entrega en el mismo
   milisegundo frames capturados con 33 ms de separación, y limitando por
   llegada se tiran frames legítimos por haber viajado juntos. Medido por un
   túnel: llegaron los 172 frames que el cliente capturó y el gateway descartó
   **72**; el reto de mirada se quedó con 7 fps efectivos y una respuesta
   impecable de medio segundo se resolvió en fallo. Si el reloj del cliente no
   es de fiar se vuelve a la hora de llegada, que es lo que impide que uno
   hostil inunde declarando sellos falsos.
2. **Sesión de un solo uso.** Un `session_id` consumido se rechaza siempre,
   aunque el intento anterior fallara. Reintentar exige ticket nuevo, semilla
   nueva y guion nuevo; si no, reconectarse sería una vía barata para sonsacar
   el guion.
3. **Motivos públicos genéricos.** Al cliente se le dice `try_again`, nunca
   `response_too_fast`: contarle a un atacante por qué se le rechazó es
   enseñarle qué corregir.
4. **Los ángulos se miden desde la cámara; los retos se enuncian desde el
   sujeto.** La cámara le mira de frente y los frames NO van espejados (el
   espejo del cliente es CSS y no toca lo que se envía), así que girar hacia
   la propia derecha da yaw **negativo**, hacia la propia izquierda positivo,
   y levantar la barbilla pitch **negativo**. Los tres comprobados contra una
   cámara real. El convenio se invirtió dos veces antes de escribirlo aquí, y
   las dos veces el síntoma fue un `inconclusive` que no se podía explicar.
5. **Los orígenes se nombran uno a uno.** `GATEWAY_ALLOWED_ORIGINS` es una
   lista exacta y vacía por defecto, y vale igual para las cabeceras CORS y
   para el upgrade del WebSocket. Devolver el origen que venga —el atajo de
   siempre— deja la API abierta a cualquier página sin que nadie lo haya
   decidido.

### El reparto de quién mide qué

Al cerrar cada reto, el gateway le pide al analizador que puntúe esa ventana, y
espera esas medidas antes de resolver. Sin ellas la fusión decidiría con la
mitad de las señales y sin saberlo, así que la espera tiene tope y lo que no
llegue sale como **no medida** — nunca como cero.

| Quién | Qué mide |
|---|---|
| Python | pose: cumplimiento, continuidad, **paralaje**, identidad |
| Python | destello: correlación, **gradiente 3D**, ausencia de pantalla |
| Gateway | mirada, calidad de captura, plausibilidad temporal |
| Python | el aviso de que la habitación tapaba el destello |

Las dos en negrita son las que de verdad separan un rostro de una superficie
plana, y no se pueden calcular en Go: son visión por computador.

Ninguna señal la emiten los dos. La fusión se queda con la PEOR ventana que
reporte una señal, así que una versión pobre arrastraría a la buena — y el
gateway tenía versiones pobres de `compliance` y `correlation` que ahora no
emite.

Y no bastaba con dejar de emitirlas: el gateway seguía **usando** su
correlación para decidir si el paso de destello se había cumplido, y para
levantar el aviso de luz ambiente. Costaba sesiones legítimas — medido, una que
Python midió en **0,4934** de correlación acabó en «demasiada luz ambiente»
porque la del gateway no llegó. Ahora el paso se cumple por haber **sonado** la
secuencia, y quien juzga si la piel respondió es la ventana de Python.

El motivo público `too_much_ambient_light` sigue existiendo: lo levanta Python
marcando la ventana con `AMBIENT_REASON`, una cadena estable que el gateway
traduce. El acoplamiento entre los dos idiomas está anotado en ambos lados y
tiene test, porque romperlo en silencio dejaría a una persona reintentando a
ciegas.

Estado provisional, marcado en el código: la máquina de estados corre en el
mismo proceso a través de `conn.Engine`. Ésa es la costura por donde entrará
el orquestador como servicio aparte.

---

## 6bis. Retención y protección del dato

La biometría es **categoría especial** bajo la LOPDP ecuatoriana (art. 25).
Conservarla más de lo necesario no es un descuido operativo: es un
incumplimiento. Por eso el borrado está diseñado desde el principio y es
automático, no una tarea que alguien recuerde hacer.

### La separación que lo hace posible

| Qué | Dónde | Es biometría | Vida |
|-----|-------|--------------|------|
| Veredicto, motivos, versión del perfil | `liveness.sessions` | No | Se conserva |
| Features por frame y ventana | `liveness.session_timelines` | Derivada | 72 h |
| Frames y clip | MinIO | **Sí** | 24 h |
| Recibos de borrado | `liveness.retention_events` | No | Se conservan |

Un rechazo del mes pasado se sigue pudiendo explicar —qué señal falló, con qué
valor, contra qué umbral y con qué perfil— **sin conservar la cara de nadie**.
Eso es todo el diseño en una frase.

### Las cuatro reglas

1. **Plazos cortos por defecto.** 24 h la evidencia, 72 h las features.
   Alargarlos tiene que ser una decisión consciente con base legal detrás, no
   lo que pasa si nadie configura nada.
2. **Cifrado antes de salir del proceso.** AES-256-GCM en cliente, no del lado
   del servidor: el almacén no ve un píxel en claro ni aunque alguien se lleve
   el bucket entero. Cada objeto apunta con qué clave se cifró, para poder
   rotarlas.
3. **Borrado verificable.** Se borra, **se comprueba que ya no está**, y se
   escribe el recibo con la huella de lo borrado. Un borrado que no se
   verifica es un borrado que se supone.
4. **Append-only donde importa.** La auditoría y los recibos no admiten UPDATE
   ni DELETE, y lo impide la base de datos con triggers. Un resultado que se
   puede reescribir no sirve para auditar, y un recibo de borrado que se puede
   borrar no prueba nada.

### Detalles que parecen menores y no lo son

- La huella de la evidencia es del **cifrado**, no del claro. Prueba igual de
  bien que se borró ese objeto exacto y no deja en la base un identificador
  derivable de la persona.
- La ruta de los objetos empieza por fecha (`<yyyy>/<mm>/<dd>/...`) para que
  borrar un día entero sea un prefijo.
- La API responde **410 Gone** —no 404— cuando la biometría ya caducó. 404 es
  "nunca existió"; 410 es "existió y se borró a propósito", que es lo que hay
  que decir cuando la retención ha hecho su trabajo.
- En `sessions` no hay identidad civil: sólo un seudónimo de sujeto que provee
  quien llama, para la política de reintentos.

## 7. Convenciones de nombres

### Transversales

- Identificadores de sesión: **ULID** en minúscula, campo `session_id`.
- Tiempos: UTC, RFC 3339 con milisegundos en JSON; `google.protobuf.Timestamp`
  o epoch en microsegundos (`int64`) en el bus. Sufijo `_at` para instantes,
  `_ms` para duraciones.
- Versionado: `v1` explícito en subjects, rutas HTTP y paquetes proto. Nunca
  se rompe un `v1`; se crea `v2`.
- Nombres de señales: `snake_case`, con familia como prefijo y **sin** juicio de
  valor. En uso hoy: `face_count`, `face_present`, `face_box_area`,
  `face_scale`, `pose_yaw_deg`, `pose_pitch_deg`, `pose_roll_deg`,
  `quality_sharpness`, `quality_brightness`, `quality_highlight_saturation`,
  `quality_face_highlight`,
  `gaze_offset_x`, `gaze_offset_y`, `gaze_agreement`, `gaze_openness`,
  `texture_pad_wide_a`, `texture_pad_wide_b`, `texture_pad_tight_a`,
  `texture_pad_tight_b`,
  `surface_face_bg_luminance_ratio`, `surface_face_bg_r`, `surface_face_bg_g`,
  `surface_face_bg_b`, `surface_specular_fraction`,
  `surface_moire_index`, `surface_banding_index`, `temporal_frames_seen`,
  `sensor_rolling_shutter`, `sensor_agc_response`, `skin_subsurface_red`.
  `rppg_snr_db`, `rppg_bpm`, `rppg_seconds`.
  Previstos: `texture_lbp_energy`, `flow_face_bg_ratio`.
  Prohibido: `is_fake`, `spoof_score`, `liveness_ok` — y el codec los rechaza.
- Nada de acrónimos inventados nuevos sin registrarlos en este archivo.

### Go

- Módulos: `github.com/edisonpaul4/biometrics/<componente>`.
- Paquetes: una palabra, minúscula, sin guiones bajos (`session`, `challenge`).
- `internal/` para todo lo que no sea contrato público.
- Errores: `ErrSessionExpired`, envueltos con `fmt.Errorf("...: %w", err)`.
- Interfaces pequeñas, definidas en el consumidor, nombradas por el rol
  (`ChallengeIssuer`, `SignalSink`), no por la implementación.
- Ficheros: `snake_case.go`. Tests: `_test.go` junto al código.

### Python

- Paquete único `analyzer`, layout `src/`.
- Módulos y funciones `snake_case`, clases `PascalCase`.
- Un detector = un módulo en `analyzer/signals/`, con una función pura
  `compute(frame, state) -> dict[str, float]`.
- Tipado obligatorio (`mypy` en estricto para `analyzer.signals`).
- Prohibido `print`; logging estructurado.

### Svelte / frontend

- Componentes `PascalCase.svelte`, rutas SvelteKit en `src/routes`.
- Stores `camelCase` en `src/lib/stores`.
- El cliente no contiene constantes de negocio (umbrales, listas de retos).

### Bus y datos

- Subjects: `analyzer.<acción>` para lo global y `session.<id>.<canal>` para
  lo de una sesión. Todo minúscula salvo el identificador de sesión, que es
  un ULID. El identificador se valida antes de construir el subject: un `>`
  o un `*` colado ahí es una suscripción a sesiones ajenas.
- Tablas Postgres: `snake_case` plural (`sessions`, `session_signals`).
- Claves Redis: `liveness:<v>:<entidad>:<session_id>` con TTL **siempre**.
- Objetos MinIO: `<bucket>/<yyyy>/<mm>/<dd>/<session_id>/<artefacto>`.

---

## 8. Estructura del repositorio

```
/proto          contratos compartidos (bus protobuf + protocolo cliente JSON Schema)
/gateway        Go — WebSocket, sesión de transporte, bus
                internal/{wsproto,conn,session,analyzer,api,telemetry}
/orchestrator   Go — FSM, retos, fusión, persistencia, API
                core/{clock,fsm,challenge,session,fusion,retry}
/deploy/policy  perfil de decisión: pesos, umbrales y reintentos (YAML)
/analyzer       Python — CV y procesamiento de señal
/web            cliente de prueba mínimo (sin framework)
                public/ lo que carga el navegador · harness/ banco sin navegador
                public/analyze.html sube un ataque y ve qué detectores lo pillan
/frontend       aplicación Svelte (SvelteKit)
/bench          banco de pruebas de ataques y métricas PAD
                runner/replay.py reproduce una grabación real contra el analizador
                runner/analyze_media.py juzga una foto o vídeo con los detectores pasivos
/deploy         docker-compose, configuración de infraestructura
```

Reglas de dependencia:

- `web/public/lib` **no** toca el DOM salvo `flashscreen.js` y `camera.js`. El
  resto conduce la sesión con la fuente de frames y el pintor inyectados, y por
  eso `web/harness` puede ejercitar el mismo código contra el gateway de verdad
  sin navegador ni cámara.
- `analyzer` **no** importa nada de Go ni conoce `proto/ws`.
- `gateway` **no** importa `orchestrator/internal`. Sí puede importar
  `orchestrator/core`, que es la API pública del núcleo y por eso está fuera
  de `internal/`.
- Todo lo que cruce un proceso pasa por `proto/`.

---

## 9. Cómo se trabaja aquí

```bash
make up        # levanta NATS, Redis, Postgres, MinIO
make down      # los baja
make test      # tests de todos los componentes
make lint      # linters de todos los componentes

make analyzer  # worker de análisis contra el NATS local
make gateway   # gateway contra el NATS local
make web       # cliente de pruebas en http://localhost:5173
make bench-web # juega una sesión entera con el código del cliente, sin navegador
```

Requisitos previos: Docker. Go y Python sólo si se van a tocar esos
componentes; los targets se saltan con aviso lo que no esté instalado.

### Reglas para agentes y contribuciones

1. No mover lógica de negocio a Python. Nunca.
2. No añadir campos al protocolo de cliente sin pasar por la sección 6.
3. No introducir JetStream ni reintentos en el camino de frames.
4. Cualquier señal nueva se declara en `proto/nats/v1/` y se documenta aquí.
5. Los umbrales viven en `deploy/policy/decision-profile.yaml`, no en el
   código. Se recarga en caliente y su versión viaja con cada resultado:
   cambiar un umbral es una operación de producto, no un despliegue.
6. Sin datos biométricos reales en el repositorio. `bench/fixtures/` es sintético
   o con consentimiento explícito documentado. Lo mismo vale para
   `web/harness/fixtures/`, que se regenera con `make web-fixtures` y no se
   versiona. `web/harness/recordings/` —las grabaciones de depuración del
   cliente— es biometría real: se queda en la máquina de quien la grabó, está
   fuera del control de versiones, y no debe sobrevivir al ajuste para el que
   se tomó.
7. Los colores del destello son primarios **saturados**. Suavizarlos con una
   fuga en los otros dos canales parece inofensivo y no lo es: el analizador
   mide relativo a la línea base del sujeto y amplifica, así que la fuga
   enciende los tres canales y el servidor deja de poder distinguir un
   primario del blanco de referencia.
