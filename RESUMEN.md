# Resumen de la plataforma de liveness

Plataforma de **prueba de vida facial** (face liveness / PAD, *Presentation
Attack Detection*), equivalente en alcance a AWS Rekognition Face Liveness.
El cliente abre una sesión, el servidor le impone retos uno a uno, el cliente
transmite frames de cámara en vivo por WebSocket y el servidor decide si hay
una **persona real presente** en ese momento.

Resultado de cada sesión: `LIVE` / `SPOOF` / `INCONCLUSIVE`, un score de
confianza y un paquete de evidencia auditable.

> El documento normativo es `CLAUDE.md`. Este archivo es un resumen para
> orientarse rápido; si algo discrepa, manda `CLAUDE.md`.

---

## 1. Cómo funciona una sesión

1. `POST /v1/sessions` crea la sesión y devuelve un token de un solo uso.
2. El cliente abre el WebSocket en `/v1/liveness` y empieza a mandar frames
   JPEG (480p, hasta 30 fps, tope de 9 Mbit/s).
3. El servidor sortea un guion y lo revela **un paso cada vez**:
   - **Calibración**: pantalla neutra, se toma la línea base del sujeto.
   - **Poses**: girar a la izquierda, a la derecha, levantar la barbilla.
   - **Mirada**: un punto aparece a un lado, salta al contrario y se mide el
     viaje de los ojos.
   - **Quietud** de 11 s: ventana para medir el pulso (rPPG).
   - **Acércate**: siempre justo antes de los destellos.
   - **Destellos**: la pantalla se tiñe con dos colores saturados, con
     duraciones sorteadas, y se mide si la piel responde.
4. El analizador mide cada frame y cada ventana. El gateway fusiona las
   medidas con el perfil de decisión y emite el veredicto.

La regla crítica: **el cliente nunca conoce el guion**. Ni cuántos pasos hay,
ni cuál viene, ni la semilla, ni los umbrales. Es lo que impide que un vídeo
pregrabado pase.

---

## 2. Arquitectura

```
 navegador ──WSS──► gateway (Go) ◄─in-proc─► orchestrator (Go)
                        │                       │    │    │
                        │ NATS core             Redis  PG  MinIO
                        ▼
                   analyzer (Python)
                   visión + señal → SOLO métricas
```

| Componente | Lenguaje | Qué hace |
|---|---|---|
| `gateway` | Go 1.23 | WebSocket, autenticación de sesión, backpressure, bus, entrega de retos, evaluación de mirada y calidad. |
| `orchestrator` | Go 1.23 | Máquina de estados, guion de retos, fusión de scores, veredicto, persistencia. |
| `analyzer` | Python | MediaPipe Face Landmarker, fotometría, pose, mirada, rPPG, PAD pasivo (MiniFASNet en ONNX). Devuelve números con nombre. |
| `web` | JS sin framework | Cliente de pruebas: cámara, pintado de retos, grabación de sesiones. |
| `frontend` | SvelteKit | Aplicación cliente real. |
| `bench` | Python | Banco de ataques, métricas APCER/BPCER, replay de grabaciones. |
| `proto` | protobuf / JSON Schema | Contratos del bus y del protocolo de cliente. |

**Go decide, Python mide.** Python no sabe que existen los retos, no aplica
umbrales y no emite juicios. El codec rechaza señales con nombres como
`is_live` o `spoof_score`.

### Infraestructura

| Servicio | Uso |
|---|---|
| NATS core, sin JetStream | Bus Go ⇄ Python. Un frame perdido no se reintenta. Afinidad de sesión por lease con heartbeat. |
| Redis | Estado caliente de sesión, siempre con TTL. |
| Postgres | Resultados y auditoría append-only, protegida por triggers. |
| MinIO | Evidencia cifrada en cliente con AES-256-GCM. Se borra a las 24 h. |

---

## 3. Qué señales se miden y contra qué ataque

| Señal | Qué detecta |
|---|---|
| `flash_gradient_3d` | Foto impresa: responde al destello, pero plana, sin relieve. |
| `flash_correlation` | Replay: un vídeo no puede seguir colores emitidos ahora. |
| `flash_screen_absence` | Pantalla: brilla por sí misma, moiré, bandeo. |
| `pose_parallax` | Superficie plana en movimiento: su giro lo explica una homografía. |
| `gaze_response` | Replay: no puede mirar al punto que acaba de aparecer. |
| `rppg_snr` | Máscara: la silicona no tiene pulso. |
| `texture_pad_v2`, `texture_pad_v1se` | PAD pasivo por textura de un frame. |
| `pose_continuity`, `temporal_plausibility` | Cortes de vídeo y respuestas más rápidas que un humano. |

### Fusión

El perfil vive en `deploy/policy/decision-profile.yaml`, se recarga en caliente
y su versión viaja con cada resultado.

- Media ponderada de las señales medidas, con pesos puestos a mano.
- Aprobado desde 0,75. Rechazado por debajo de 0,45. En medio, reintentar.
- Suelos por señal que vetan solos, por ejemplo gradiente 3D por debajo de 0,30.
- Hasta 3 intentos con espera de 5 s, 30 s y 120 s.

Tres principios que atraviesan todo:

- **Lo que no se pudo medir no entra en la fusión.** Nunca cuenta como cero.
- **Calidad mala no es ataque.** Ningún camino de calidad acaba en rechazo.
- **El pulso puede absolver pero nunca acusar**, porque su señal depende del
  tono de piel.

---

## 4. Modelos y algoritmos

### En producción

Todos corren en CPU, un hilo por proceso, dentro de un presupuesto de 30 ms
por frame. Se descargan con `make models` y no se versionan.

| Modelo | Fichero | Para qué | Origen |
|---|---|---|---|
| MediaPipe Face Landmarker | `face_landmarker.task` | Detector por defecto. 478 puntos de la cara, iris incluidos, y matriz de pose de la cabeza. | Google MediaPipe |
| YuNet | `face_detection_yunet_2023mar.onnx` | Detector alternativo por ONNX Runtime, intercambiable por configuración. | OpenCV Zoo |
| SFace | `face_recognition_sface_2021dec.onnx` | Embedding de 128 dimensiones cada 3 frames para la continuidad de identidad. No identifica a nadie ni compara con ninguna base. | OpenCV Zoo |
| MiniFASNet V2 | `MiniFASNetV2.onnx` | PAD pasivo por textura de un frame. Peso 0,06. | yakhyo/face-anti-spoofing, Apache-2.0 |
| MiniFASNet V1SE | `MiniFASNetV1SE.onnx` | Segunda variante de PAD pasivo. Peso 0,02. | yakhyo/face-anti-spoofing, Apache-2.0 |

MediaPipe está fijado en la línea 0.10. La 1.0 aborta el proceso en macOS ARM
al inicializar Metal.

Si el detector no encuentra la cara, se reintenta sobre la imagen con un borde
replicado del 15 %. Con la cara pegada a los bordes eso sube la detección del
57 % al 98 %.

### Algoritmos sin red neuronal

| Medida | Método |
|---|---|
| Destello | Fotometría por región de la cara partida por el fondo del mismo frame, relativa a la calibración. Correlación contra la secuencia emitida con búsqueda de retardo de 0 a 600 ms, ponderada por amplitud de cada región. |
| Gradiente 3D | Dispersión de la respuesta al destello entre frente, mejillas y nariz. |
| Paralaje | Residuo de una homografía sobre los landmarks durante el giro. Un plano lo explica exactamente. |
| Mirada | Posición del iris entre las comisuras, convertida a grados con 248 grados por unidad y sumada al giro de cabeza. |
| Pulso | Método POS sobre el color de la piel partido por el fondo, con FFT sobre 10 s o más. |
| Pantalla | Índices de moiré y bandeo, reflejo especular y brillo de la cara frente al fondo. |
| Sensor | Obturador rodante en el frame de transición y reacción del control automático de la cámara. Se registran sin voto. |

### Probados y descartados

| Modelo | Por qué se descartó |
|---|---|
| anti-spoof-mn3, OpenVINO | Siete veces el tamaño de MiniFASNet y no rindió mejor en nuestro banco pese a su cifra publicada. |
| FLIP, CLIP ViT-B/16 afinado | AUC de 0,88 contra 0,93 de MiniFASNet V2, y peor que el CLIP sin afinar. Tarda 45 ms por pasada. |
| Combinar V2 y V1SE a partes iguales | Ven el mismo recorte, así que contaba una medida dos veces. Combinados rinden peor que V2 solo. |

---

## 5. Cómo probamos y refinamos

El método es siempre el mismo: **medir, formular una hipótesis, refutarla con
datos y sólo entonces cambiar código**. Cada decisión queda escrita con la
medida que la justifica en `CLAUDE.md` o en `bench/RESULTS.md`.

### Tres niveles de prueba

1. **Tests automáticos.** `make test` corre 30 ficheros de tests Go, 20 de
   Python y los del cliente JS. Varios son de propiedad sobre miles de
   semillas, por ejemplo que el guion no filtre pasos futuros o que las
   duraciones de un destello siempre se separen.
2. **Banco de ataques sin cámara.** `make bench-dataset` mide APCER y BPCER de
   los detectores pasivos sobre datasets públicos, con umbral elegido en una
   mitad y medido en la otra, partiendo por sujeto.
3. **Sesiones reales desde el móvil.** Se juega la sesión entera por los
   túneles ngrok con el modo pentest encendido, y se lee el log del gateway y
   la tabla de detectores.

### Datasets del banco

| Dataset | Contenido | Uso |
|---|---|---|
| AxonData face-anti-spoofing | 29 caras reales y 120 ataques de ocho tipos, sobre todo máscaras. | Primera matriz APCER/BPCER. |
| CASIA-FASD | 8.126 imágenes de 30 sujetos: fotos dobladas, recortadas y replay en pantalla. | Cubre papel y pantalla, que faltaban. Sólo evaluación interna por licencia. |

Lo que salió de ahí: la textura pilla el papel y se le escapa la pantalla, y
moiré y bandeo hacen lo contrario. Son complementarios. Y un umbral ajustado
en un banco no transfiere al otro, por eso el PAD pasivo no tiene suelo.

### El ciclo con grabaciones reales

1. El usuario hace una sesión desde el móvil y se graba.
2. `bench/runner/replay.py` y scripts de análisis recalculan offline cada
   ventana con el código de producción, frame a frame.
3. Cada medida se compara con **controles**: la secuencia invertida y la misma
   secuencia colocada sobre un tramo sin destello. Una medida que puntúa igual
   en el control no mide nada.
4. Si la hipótesis sobrevive, se cambia el código o el perfil, se añade un
   test con el caso real y se repite la sesión.

Hipótesis refutadas así, que ya no hay que volver a probar:

- El filtro de cara o el códec no eran el cuello de la cadencia.
- Los frames perdidos no explicaban los destellos fallidos.
- Alargar los tramos de destello no daba más señal: la destruía.
- Ningún estimador alternativo recupera el destello con luz de día.

### Lo que falta para pasar de refinar a certificar

- Ataques reales delante de la cámara: foto impresa, replay y máscara.
- Más sujetos, y medir por tono de piel antes de dar voto al PAD pasivo y al pulso.
- Una partición reservada de sesiones reales para fijar los umbrales finales.

---

## 6. Cómo lo usamos en el día a día

### Levantar todo

```bash
make up          # NATS, Redis, Postgres, MinIO en Docker
make analyzer    # workers de análisis
make gateway     # 480p@30 por defecto; CAPTURE=720 · EXPLAIN=1 modo pentest
make web         # cliente de pruebas en http://localhost:5173
```

Para probar desde el móvil se abren dos túneles ngrok, uno al cliente y otro
al gateway. El gateway necesita el origen del túnel del cliente en
`GATEWAY_ALLOWED_ORIGINS`, y el cliente se arranca apuntando al túnel del
gateway:

```bash
node web/serve.mjs 5173 --gateway https://<tunel-gateway>.ngrok-free.app
```

### Diagnosticar con grabaciones

Cada sesión del cliente de pruebas se graba en `web/harness/recordings/`:
cabecera JSON con eventos, sellos por frame, ajustes de cámara y descartes, y
después los JPEG. Eso permite recalcular offline cualquier medida con el mismo
código de producción.

```bash
make replay REC=<fichero>        # reproduce una grabación contra el analizador
make flash-colors REC=<fichero>  # respuesta a cada color del destello
make bench-web                   # sesión entera sin navegador
make test                        # tests de todos los componentes
```

Las grabaciones son **biometría real**. Se quedan en esta máquina, no se
versionan y se borran cuando termina el ajuste para el que se tomaron.

### Modo pentest

`EXPLAIN=1` activa `GATEWAY_INSECURE_EXPLAIN_VERDICT`: el cliente recibe el
desglose por detector con su valor, su suelo y si pasó. Sólo para pruebas. En
producción el cliente recibe únicamente `try_again`.

---

## 7. Lo que hemos aprendido midiendo

Decisiones que salieron de sesiones reales y no de la teoría:

- **480p a 30 fps, en JPEG, codificado en un Worker.** A 720p el iris se medía
  peor y el enlace se saturaba. WebP pesaba la mitad pero tardaba 28 ms por
  frame y hundía la cadencia.
- **Relación de aspecto nativa de la cámara.** El móvil entrega en vertical y
  se estiraba la cara 1,78 veces. Corregirlo obligó a re-derivar la constante
  de mirada a 248 grados por unidad.
- **Tope por caudal, no por cadencia.** Pasarse del enlace no pierde frames:
  los retrasa, y un frame tardío miente sobre cuándo pasó lo que muestra.
- **La mirada busca su retardo** hasta 0,8 s, y una transición sin frames
  alrededor sale como no medible en vez de como respuesta nula.
- **Destellos de dos colores, sin blanco, de 350 a 600 ms, con duraciones que
  difieren al menos 100 ms.** Tramos más largos dejan que la cámara compense el
  color y la señal desaparece. Duraciones parecidas permitían encajar la
  secuencia invertida.

### Estado del destello con luz de día

De noche las 12 ventanas medidas correlacionan entre 0,92 y 0,99. De día
fallan las 6, porque la cámara expone de 5 a 10 veces menos y la luz ambiente
entierra el destello. No es un fallo del algoritmo: ocho estimadores
alternativos se probaron contra controles y ninguno discrimina.

Es la misma limitación que tiene AWS en web, que pide al usuario subir el
brillo de pantalla al máximo a mano. El plan pendiente:

1. Medir una sesión de día con el brillo al máximo y la cámara fijada.
2. Pedir brillo máximo en el cliente antes de empezar.
3. Garantizar un ancho mínimo a los parches de fondo con la cara cerca.
4. Esperar a que el tamaño de cara se estabilice tras el «acércate».

---

## 8. Pendientes conocidos

- Medir APCER contra ataques reales: foto impresa y replay delante de la cámara.
- Calibrar contra pantallas reales las pistas de superficie emisiva.
- Mover los umbrales de mirada del código Go al perfil de decisión.
- Medir el PAD pasivo y el pulso por tono de piel antes de darles más peso.
- `ANALYZER_MIN_FACE_CONFIDENCE` no llega a MediaPipe.
- Inyección de cámara y deepfakes en tiempo real siguen fuera de alcance.

---

Autor: Paul Mosquera · [linkedin.com/in/paul-mosquera](https://www.linkedin.com/in/paul-mosquera/)
