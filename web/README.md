# Cliente de pruebas

Deliberadamente desechable. **No es el producto**: es el aparato con el que se
mide el backend. Sin build, sin dependencias, sin framework — módulos ES que el
navegador carga tal cual.

## Arrancarlo

```sh
make up                      # infraestructura
make analyzer                # worker de análisis (otra terminal)
make gateway                 # gateway (otra terminal)
make web                     # cliente en http://localhost:5173
```

`make web` sirve `public/` y publica `/config.json` con la dirección del
gateway, que el cliente usa para rellenar el campo. Para apuntar a otro:

```sh
make web GATEWAY_URL=http://otro:8080
```

El gateway sólo atiende a los orígenes que se le nombran, y `make gateway` ya
le pasa el del cliente. Si sirves el estático en otro sitio, dilo en los dos
lados o el navegador ni llegará a pedir la sesión:

```sh
make gateway WEB_PORT=4173
make web WEB_PORT=4173
```

`getUserMedia` sólo funciona en origen seguro. `localhost` cuenta como tal, así
que en la misma máquina basta con HTTP; **desde otro equipo hace falta HTTPS**.

## Qué hace, y por qué así

| Requisito | Dónde | Nota |
|---|---|---|
| 480p a 15 fps | `lib/camera.js` | el servidor impone el caudal en `server_hello`; el cliente lo obedece |
| Calidad JPEG por reto | `lib/quality.js` | destello y calibración a 0,92; pose a 0,60 |
| Mínimo 350 ms por color | `lib/flash.js` | `enforceMinimum`, y el tramo se cuenta desde el pintado real |
| Instante REAL de pintado | `lib/flashscreen.js` | dos `requestAnimationFrame` encadenados |
| Filtro local de calidad | `lib/facefilter.js` | MediaPipe WASM; **sólo descarta**, nunca puntúa |
| Panel de depuración | `main.js` | estado, fps real, descartes, latencia, reto activo |

Tres decisiones que no son obvias:

**La calibración va a la misma calidad JPEG que el destello.** Si la línea base
se captura con otro ajuste del compresor, todo lo que se mida contra ella lleva
dentro la diferencia del códec, no la del sujeto.

**Los primarios son saturados, sin fuga.** Un `rgb(255,40,40)` —que es lo que
pide el instinto para suavizar el rojo— se mide como respuesta en los tres
canales: el analizador normaliza contra la línea base del sujeto y amplifica,
así que un 16 % de fuga basta para que el rostro parezca responder a todo y el
servidor no pueda distinguir rojo de blanco. Saturar tampoco encandila más: un
rojo puro tiene menos luminancia que uno lavado con blanco.

**Una secuencia de destello deja de pintar en cuanto no es la vigente.** El
apagado final de la secuencia anterior caía dentro del reto siguiente y le
borraba el primer color; el servidor lo medía como un sujeto que no responde.

## Banco de pruebas

`lib/session.js` conduce la sesión entera sin tocar el DOM: la fuente de frames
y el pintor se inyectan. Eso permite ejercitar **exactamente el mismo código**
contra el gateway de verdad, sin navegador ni cámara:

```sh
make web-fixtures            # genera los frames sintéticos (una vez)
make bench-web GATEWAY_URL=http://localhost:8080
```

Imprime el mismo panel que la pestaña y falla si algo no cuadra. El guion de
retos no se puede fijar desde fuera —la semilla vive en el servidor y no viaja,
CLAUDE.md §6—, así que el banco juega las sesiones que le tocan y repite hasta
dar con una que su maniquí sepa responder.

Lo que el maniquí **no** sabe hacer: `pitch_up`. Una cabeza dibujada deja de ser
detectable antes de que el ángulo medido llegue al umbral. Esas sesiones se
descartan en voz alta, no se dan por buenas.

```sh
make test-client             # tests de los módulos puros
```

## Banco de ataques

`http://localhost:5173/analyze.html` — sube una foto o un vídeo y mira qué
detectores lo pillan.

Existe porque un ataque grabado **no puede responder a un guion aleatorio**: no
mira al punto, no gira cuando se le pide y no reacciona al destello. Eso lo
deja fuera del recorrido normal, y a la vez es justo el material que hay que
medir para llenar la matriz de `/bench`.

Lo que sí se puede juzgar de un archivo es todo lo que no necesita
colaboración: los dos modelos PAD, moiré, bandeo, reflejo especular, y el
**paralaje** si hay movimiento de cabeza — que es lo que separa una foto
impresa girada de un rostro con volumen.

**No es una prueba de vida.** Que un archivo salga limpio aquí no significa que
pasaría una sesión real; sólo dice que los detectores pasivos no lo señalaron.
Sirve para lo contrario: ver qué detector pilla cada ataque y cuál se lo traga.

También desde la línea de órdenes:

```sh
make analyze MEDIA=foto-impresa.jpg
make analyze MEDIA=replay-movil.mp4
```

El archivo se escribe en un temporal, se analiza y se borra. No se guarda: si
es biometría de alguien, no tiene por qué quedarse (CLAUDE.md §6bis).

## Grabar una sesión

La casilla **«grabar la sesión»** guarda los JPEG **tal cual se enviaron** —no
un vídeo recomprimido: lo que hay que poder reproducir es lo que el analizador
vio, y un códec por encima cambiaría justo las señales que se están
depurando— junto con la línea de tiempo de retos. Eso convierte la grabación
en un caso con etiqueta: *aquí se le pidió mirar abajo a la izquierda, y esto
hizo su iris*.

```sh
make replay REC=web/harness/recordings/session-....bin
make replay REC=... 2>/dev/null | less     # la tabla de señales por frame
```

Sirve para ajustar umbrales contra caras de verdad sin pedirle a nadie que
repita la sesión veinte veces.

**Es biometría, categoría especial bajo la LOPDP** (CLAUDE.md §6bis). Por eso:
va apagada por defecto y hay que marcarla en cada sesión; no sale de la
máquina —la recibe el mismo servidor estático que sirve la página—; el
directorio no se versiona; y no debería sobrevivir al ajuste para el que se
tomó. Si alguna acaba en `/bench`, hace falta consentimiento explícito
documentado (regla 6 de CLAUDE.md §9).

## Lo que sigue sin probarse solo

El pegamento del navegador: `getUserMedia`, el `canvas`, la carga de MediaPipe
WASM desde CDN y el camino feliz del doble `requestAnimationFrame` (en una
pestaña oculta `rAF` no corre, así que sólo se comprueba a mano con la pestaña
delante). Para eso hay que abrir `make web`, dar permiso a la cámara y mirar el
panel.
