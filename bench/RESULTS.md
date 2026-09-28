# Resultados del banco

Números medidos, no estimaciones. Cada entrada dice con qué se midió y qué
alcance tiene: casi todo lo de aquí sale de MUESTRAS de decenas o cientos de
archivos, que sirven para descartar y para orientar, no para certificar.

---

## 2026-08-25 · Primera medida APCER/BPCER

**Datos:** `AxonData/face-anti-spoofing-dataset` (muestra pública, no
comercial). 29 rostros reales y 120 ataques de ocho tipos: replay en pantalla
y en móvil, recortes, máscaras de papel 3D, papel envuelto, látex, silicona y
textil. Predominan las máscaras, que es territorio de iBeta **nivel 2**.

**Cómo:** `bench/runner/bench_dataset.py`, todo escalado a 640×480 con
letterbox — la resolución que ve el analizador en producción. Sólo detectores
**pasivos**: un archivo no puede responder a un reto.

### APCER por tipo, a los umbrales que había puestos

Porcentaje de ataques que el detector **deja pasar**. Menos es mejor.

| detector | papel 3D | recortes | látex | replay pant. | replay móvil | silicona | textil | papel env. | BPCER |
|---|---|---|---|---|---|---|---|---|---|
| texture_pad_wide_a | 100 % | 73 % | 100 % | 100 % | 100 % | 100 % | 96 % | 90 % | 0 % |
| texture_pad_tight_a | 100 % | 73 % | 100 % | 100 % | 100 % | 100 % | 100 % | 90 % | 0 % |
| surface_moire_index | 22 % | 7 % | 30 % | 20 % | **0 %** | 64 % | 52 % | **0 %** | 0 % |
| surface_banding_index | 22 % | **0 %** | **0 %** | **0 %** | **0 %** | 55 % | **0 %** | **0 %** | 0 % |
| surface_specular_fraction | 97 % | 100 % | 100 % | 100 % | 100 % | 91 % | 100 % | 100 % | 0 % |

### El resultado depende de cómo se preparen los frames, y mucho

Los vídeos vienen en proporción de móvil y el analizador trabaja en 4:3. Al
convertir hay que elegir, y **la elección cambia la conclusión**:

| encaje | mejor APCER de moiré | mejor APCER de los PAD |
|---|---|---|
| `letterbox` (frame entero, con bandas) | **5,8 %** | 12,5 % |
| `crop` (recorte central, sin bandas) | 23,5 % | **1,7 %** |

Tiene explicación física: el letterbox conserva todo el campo pero encoge el
rostro y mete unas barras negras que ningún modelo vio al entrenarse; el
recorte central conserva la escala del rostro y la textura, pero se come la
periferia donde vive el patrón de moiré.

Los mismos 149 archivos sostienen dos conclusiones contrarias. Por eso el
encaje es ahora un parámetro explícito (`--fit`) y no una decisión enterrada,
y por eso las tablas de abajo dicen con cuál se midieron.

**La primera versión de este documento decía que los modelos PAD no servían.
La mitad de esa conclusión era el letterbox.**

### Lo que hay que leer de ahí

**Con la entrada bien preparada, los modelos PAD son los que mejor separan:**
1,7 % de APCER en su mejor punto de corte. Pero a costa de **20,7 % de BPCER**
—uno de cada cinco rostros reales rechazado—, que es inaceptable. Separan, y
no están calibrados.

Su punto de corte nominal es 0,5 y sus salidas viven entre 0,000 y 0,2. Ese
desajuste de escala es lo que hacía que a 0,5 no detectaran nada.

**Moiré y bandeo separan muy bien con el frame entero, y sus umbrales estaban
mal.**

```
moiré    reales mediana 0,009 (máx 0,270)  ·  ataques mediana 4,55
bandeo   reales mediana 0,046 (máx 1,533)  ·  ataques mediana 9,88
```

Treinta veces de diferencia en la mediana. Pero las referencias estaban en
0,15, **por debajo del máximo de los rostros auténticos**: un rostro real con
algo de textura saturaba el indicador y se leía como pantalla. Subidas a 0,35
(moiré) y 1,8 (bandeo), por encima de ese máximo y aún un orden de magnitud
por debajo de los ataques.

En su mejor punto de corte sobre estos datos, el bandeo da **3,3 % de APCER
con 3,4 % de BPCER**; el moiré, **5,8 % con 0 %**.

**El reflejo especular no separa.** 61 % de APCER incluso en su mejor punto.
Se mantiene porque cuesta cero, pero no puede decidir nada.

### Cómo resumir los frames de un archivo: el máximo, no el promedio

Parece natural promediar los frames de un vídeo. Medido, es peor en todo:

| agregación | APCER | BPCER |
|---|---|---|
| mediana | 10,9 % | 37,9 % |
| p75 | 3,4 % | 31,0 % |
| p90 | 1,7 % | 27,6 % |
| **máximo** | **1,7 %** | **20,7 %** |

(`texture_pad_tight_a`; el máximo gana también en los otros cuatro detectores.)

La causa es que **la evidencia de ataque es esparsa en el tiempo**: sólo
algunos frames delatan una máscara o un replay, y promediar los entierra entre
los que parecen normales. El gateway ya usaba el máximo en producción; esto lo
valida en vez de dejarlo a la intuición.

Y algo que descarta la explicación fácil del BPCER: **se queda en 20,7 % con
cualquier agregación**. No lo causa la forma de resumir. Son seis selfies
concretos que fallan siempre, y entender por qué es lo próximo barato.

### Lo que este resultado NO dice

- Los umbrales "mejores" se eligen sobre los mismos datos con los que se
  miden. Eso infla el resultado y no es una calibración: hace falta partición
  aparte.
- 29 rostros reales es poco para un BPCER, y no hay diversidad declarada de
  tono de piel. Sin eso no se puede afirmar que el sistema trate igual a todo
  el mundo, que es el requisito que más importa.
- La muestra es pesada en máscaras. Print y replay, que son el grueso del
  nivel 1, están poco representados.
- Todo es pasivo. La mitad reto-respuesta del sistema —destello, mirada,
  paralaje— no se ejercita con archivos, y es la que para un replay.

### Siguiente

Conseguir **OULU-NPU** (vídeo de móvil, 55 sujetos, el más parecido a una
webcam) y repetir con partición de entrenamiento y prueba separadas.

---

## El recorte del clasificador pasivo no era el que decía ser

`analyzer/src/analyzer/pad.py` pedía dos escalas de recorte distintas —2,7 para
MiniFASNet V2 y 4,0 para V1SE— porque son las que llevan los pesos en el nombre
del fichero. **Ninguna de las dos se alcanzaba nunca.**

El recorte se limita por el propio encuadre: no se puede pedir más imagen de la
que hay. Con la cara cerca —que es justo lo que exige el gate de encuadre— el
límite manda siempre. Medido con `bench/runner/pad_crop_sweep.py` sobre los 147
archivos del banco:

    factor máximo alcanzable: mediana 1,69   mín 1,00   máx 2,39
    archivos que llegan a 2,7:  0 / 147
    archivos que llegan a 4,0:  0 / 147

Dos consecuencias, y la segunda es peor que la primera.

**Las escalas configuradas no hacían nada.** Barriendo de 1,0 a 5,0, el AUC es
plano de 2,4 en adelante porque a partir de ahí todo satura:

    escala   1.0    1.2    1.4    1.6    2.0    2.4    2.7    4.0
    v2      0.730  0.854  0.932  0.967  0.966  0.968  0.968  0.968
    v1se    0.663  0.830  0.892  0.923  0.925  0.928  0.928  0.928

**Los dos modelos veían el MISMO recorte.** El perfil de decisión repartía su
peso a partes iguales justificándolo en que uno mira el contexto y el otro la
piel. Eso era falso: no son dos vistas, son dos modelos sobre una entrada, y
ponderarlos igual contaba una sola medida dos veces.

Se probó rellenar el borde por reflexión para forzar las escalas altas.
**Empeora**: fabrica justo los bordes rectos que el modelo busca para detectar
papel y biseles. v2 baja de 0,968 a 0,930.

Se probó combinar los dos modelos, que era la idea de partida:

    escala 2,4:   máximo 0.949    media 0.954    v2 solo 0.968

Combinar también empeora. v1se arrastra a v2, y sólo le gana en dos categorías
de cinco muestras cada una. Se le dejó peso testimonial en vez de retirarlo:
veinticuatro caras reales no bastan para eliminar un modelo, y v1se puede
servir contra ataques que este banco no tiene.

Además, el recorte **encogía** cuando se salía del encuadre en vez de
desplazarse hacia dentro como hace la implementación de referencia. Encogerlo
cambia la fracción del 80×80 que ocupa el rostro, que es exactamente lo que fija
el modelo entrenado.

## Los umbrales estaban en la escala equivocada

El banco cortaba en 0,5 «porque sale de un softmax». La clase de ataque de
MiniFASNet reparte casi toda su masa por debajo de la milésima, así que ese
corte **no disparaba nunca**: dejaba pasar entre el 44 % y el 100 % de cada
familia de ataque mientras aparentaba un BPCER del 0 %. Es la forma más
engañosa de estar roto, porque la columna que se mira primero se ve perfecta.

Los puntos de operación reales, sobre el banco completo:

    texture_pad_v2_a     0.0085
    texture_pad_v1se_a   0.0016
    surface_moire_index  0.017
    surface_banding      0.040

## Con partición reservada

`--holdout` elige el umbral en una mitad y lo mide en la otra, cinco semillas.
Antes, todo umbral «óptimo» de este banco estaba elegido sobre los mismos datos
con los que se medía, y eso no predice nada.

    detector                  umbral            APCER          BPCER
    texture_pad_v2_a     0.0085-0.0086       5.0-10.0%     13.3-26.7%
    texture_pad_v1se_a   0.0007-0.0297       0.0-30.0%      0.0-33.3%
    surface_moire_index   0.017-0.023       15.5-32.8%      6.7-20.0%
    surface_banding       0.032-0.061       13.8-39.7%     20.0-66.7%

Lo que dice esta tabla más allá de las cifras: el umbral de v2 es **estable**
entre particiones (0,0085-0,0086) y el de v1se **no** (0,0007-0,0297, cuarenta
veces). Un umbral que salta según qué mitad te toque no es un punto de
operación, es ruido, y explica por qué combinarlos empeora.

Con 29 caras reales, la mitad reservada son ~14: **una sola cara mal
clasificada mueve el BPCER 7 puntos**. Estas cifras sirven para comparar
detectores entre sí, no para prometer una tasa a nadie.

## `surface_specular_fraction` no separa

62-91 % de APCER con partición reservada. Se retiró de la matriz de detectores
del banco y de la página de pruebas de ataque. Se sigue **emitiendo** como
medida —es una propiedad real de la imagen y puede servir dentro de otra
señal—, pero presentarla como detector le decía a quien prueba el sistema que
hay una defensa donde no la hay.

## Las caras reales que fallaban eran un problema de luz, no de modelo

De las 24 selfies, 4 superaban el umbral. Al mirarlas, dos cosas:

**La mitad era artefacto del banco.** Las selfies son verticales (1000×1333) y
el banco las recorta al centro a 640×480 apaisado para igualar la geometría de
captura de producción, tirando el 44 % de la altura. Midiendo en vertical
nativo, los rechazos pasan de 4 a 2. No se cambió el preproceso —producción es
una webcam apaisada y el banco debe parecerse a producción— pero conviene saber
que parte del BPCER de esta tabla es geometría, no modelo.

**La otra mitad tenía luz cenital reventada.** Y ahí el arreglo no era retocar
el modelo: el resumen de sesión de los clasificadores es el **peor** frame, así
que un reflejo basta para arrastrar la sesión entera. Rechazar por eso es
acusar de fingir a quien tiene una lámpara encima.

Ahora los clasificadores pasivos **no votan** cuando los quemados del rostro
superan `max_highlight_saturation`, el mismo umbral con el que ya se puntúa la
calidad de captura: si una exposición se considera mala para medir, tampoco
sirve para acusar. Lo comprueba
`orchestrator/core/fusion/engine_test.go::TestPADNoVotaConCapturaQuemada`.

## Pulso sanguíneo (rPPG)

Construido y conectado, **sin calibrar contra ataques reales**.

### De dónde sale el mínimo de 10 segundos

Del error en la frecuencia estimada, que cae en picado exactamente ahí. 150
series sintéticas por punto, modulación débil:

    ventana     4s     6s     8s    10s    12s    15s    20s    30s
    error    51 lpm 31 lpm 21 lpm  3 lpm  4 lpm  1 lpm  0 lpm  1 lpm

Por debajo de 10 s lo que sale no es un pulso mal medido: es un pico del ruido.
La resolución de una FFT es 1/T y con 8 s los bins miden 7,5 lpm.

### Lo que 10 segundos NO garantizan

Discriminar. La separación entre rostro vivo y superficie depende mucho más de
la fuerza de la modulación que de la duración:

    ventana              4s     8s    10s    15s    20s    30s
    AUC modulación 0,9%  0,955  0,997  0,995  1,000  1,000  1,000
    AUC modulación 0,25% 0,618  0,613  0,615  0,678  0,749  0,802

Con una señal fuerte, 4 segundos ya separan casi perfecto. Con una señal débil
—0,25 %, que es la realista con una webcam corriente— **ninguna ventana corta
sirve**, y hacen falta 20-30 s para llegar a un AUC de 0,75-0,80.

Esto corrige una afirmación anterior de este documento: los «13 dB de
separación» salían de una sola semilla con modulación fuerte. Con la
distribución completa y modulación realista, la separación a 10 s es AUC 0,62,
que es poco más que nada.

### Los 10 segundos eran el ruido de una captura de 480p, no un límite físico

Corrección de lo anterior, y es la más importante de las tres.

El argumento de que «la resolución de una FFT es 1/T» es cierto pero se estaba
usando mal: 1/T fija el ANCHO del bin, no el error de estimación. Con buena
relación señal/ruido, el pico cae en el bin correcto aunque los bins sean
anchos, y se estima la frecuencia mucho mejor que la anchura del bin.

Medido, AUC de detectar que hay pulso (modulación 0,25 %):

    captura           4s      6s      8s     10s     15s     20s
    480p 15fps      0.618   0.579   0.613   0.615   0.678   0.749
    720p 30fps      0.759   0.837   0.887   0.915   0.968   0.986
    1080p 30fps     0.913   0.970   0.994   0.993   1.000   1.000
    1080p 60fps     0.984   0.999   1.000   1.000   1.000   1.000

Y el error en la frecuencia estimada:

    480p 15fps      51 lpm  31 lpm  21 lpm   3 lpm   1 lpm   0 lpm
    1080p 30fps      6 lpm   1 lpm   1,5 lpm 3 lpm   1 lpm   0 lpm

Con 1080p/30fps la señal es utilizable a los **4 segundos**. El «mínimo de 10
segundos» no era una ley de Fourier: era el ruido de 480p a 15 fps.

### El JPEG no es el muro

La duda razonable era si comprimir destruye una modulación del 0,25 %. Medido
sobre un frame real de una sesión (rostro de 197×241 = 47.477 px):

    calidad JPEG    60      70      80      90     100
    desvío       0,056%  0,024%  0,019%  0,007%  0,006%

A la calidad de hoy (0,7) el error de cuantización es diez veces menor que la
señal. Promediar decenas de miles de píxeles cancela el error por bloque. Subir
resolución sirve de verdad; no se está peleando contra el códec.

### Lo que estas cifras NO modelan

El ruido simulado es blanco e independiente, así que baja con la raíz del
número de píxeles. El ruido real tiene componentes que **no** bajan así: el
parpadeo de la luz artificial, la caza del autoexposición, y el movimiento del
sujeto. Son cotas superiores, y el siguiente paso honesto es medirlas con una
captura real a 720p o 1080p, no dar por bueno el modelo.

Consecuencia para el guion: con la captura actual (480p/15fps) hacen falta
20-30 s de tramo tranquilo para que la señal sirva. **Subiendo a 720p/30fps
bastan unos 10 s, y con 1080p/30fps unos 4-6 s.** Sale más barato subir la
cámara que alargar la sesión.

### Lo que se cambió a raíz de esto

Captura a **720p/30fps** (era 480p/15fps). El analizador aguanta: medido sobre
frames reales, 13,0 ms de p90 contra un presupuesto de 33 ms a 30 fps. A 1080p
serían 20,2 ms, así que también cabría; lo que lo desaconseja es la subida,
que se dobla otra vez.

**Destellos agrupados al final** y con techo de dos por guion, para que quede
delante un tramo continuo donde medir. Medido sobre 2000 semillas:

    techo de destellos    p05     p25   mediana    p95   sesiones <10s
    1                   17,0s   19,0s    22,0s   26,0s      0,0 %
    2                   12,0s   16,0s    17,0s   23,0s      0,0 %
    3                    9,0s   13,0s    16,0s   23,0s      6,5 %
    4 (antes)            8,0s   11,0s    14,0s   23,0s     12,3 %

Se eligió 2 y no 1 porque con 1 el número de destellos dejaría de variar, y esa
variación es lo que impide predecir la longitud del bloque final.

Sobre las grabaciones reales de sesión, **no se puede medir**, y el motivo es
estructural, no del método: la serie utilizable más larga de todas las
grabaciones son 11,5 segundos, y la mayoría no llegan a 8. Con eso la
resolución en frecuencia es de 5 a 14 lpm.

Un fallo propio que conviene dejar escrito, porque casi se convierte en un
hallazgo falso: la primera versión daba pulsos de 42-46 lpm en **todas** las
grabaciones. 42 lpm es exactamente `MIN_HZ`. No era bradicardia: la suma
solapada de POS daba menos aportaciones a los extremos de la serie que al
centro, y esa envolvente es una componente de baja frecuencia que asoma por el
borde inferior de la banda disfrazada de latido. Se normaliza por el número de
ventanas que aportan, se quita la tendencia lineal, y se rechaza cualquier pico
que caiga en el primer bin de la banda.

Para que esta señal sirva de verdad hace falta un tramo de diez segundos
seguidos sin destellos dentro de la sesión. Eso alarga la sesión y es una
decisión de producto que no está tomada.


## El tramo tranquilo del guion no existía fuera del modelo

Agrupar los destellos al final dejaba, sobre 2000 semillas, una mediana de 17 s
de tramo continuo por delante. Ese cálculo sumaba los **plazos** de cada reto.

Una sesión real medida después:

    t= 0.00s  sesión asignada
    t= 1.58s  calibración
    t= 2.37s  mirada
    t= 3.59s  pose
    t= 4.93s  pose
    t= 6.12s  pose        <- fin del tramo sin destellos
    t= 7.25s  destello
    t= 8.28s  destello + VEREDICTO

**8,3 segundos la sesión entera**, y 6,1 de tramo. El sujeto respondía a cada
reto en poco más de un segundo, o sea en una fracción de su plazo. El modelo
medía el peor caso y se leyó como el caso normal.

De ahí el tramo explícito de quietud (`KindHold`, once segundos). No es un reto
—no hay nada que responder— sino una ventana de medida. La sesión pasa de 8 a
unos 19 segundos.

### Un fallo silencioso que casi se cuela

El analizador guardaba 600 muestras por sesión. A 15 fps sobraban; a 30 fps una
sesión de 60 s produce 1800, y el recorte se comía las muestras del tramo antes
de que el pulso se pidiera —que ocurre al RESOLVER, no al cerrar el paso—.

El síntoma habría sido «no se pudo medir el pulso», indistinguible de no tener
pulso. Tope subido a 2000 (~8 MB por sesión, ~95 MB con las doce en marcha) y
test de regresión en `tests/test_rppg.py`.

---

## anti-spoof-mn3 (OpenVINO / CelebA-Spoof): probado y descartado

Candidato a sustituir a MiniFASNet: MobileNetV3, 3,02 M de parámetros (siete
veces MiniFASNet), entrada 128×128 normalizada, licencia Apache-2.0, y una
cifra publicada de **3,81 % de ACER**.

Descarga (no está versionado; el fichero se borró tras medir):

    curl -L -o analyzer/models/anti-spoof-mn3.onnx \
      https://storage.openvinotoolkit.org/repositories/open_model_zoo/public/2022.1/anti-spoof-mn3/anti-spoof-mn3.onnx

### El recorte, otra vez

Con el recorte de MiniFASNet (2,4) daba **0,64 de probabilidad de ataque sobre
caras reales**: inservible. Su óptimo está en 1,3 — mucho más ceñido — y la
documentación no lo dice en ninguna parte. Segunda vez que la escala de recorte
resulta ser el parámetro que decide si un modelo mide o inventa.

    recorte     1.0    1.3    1.6    2.0    2.4    3.0   encuadre
    AUC        0.800  0.837  0.795  0.762  0.769  0.769   0.680

### Y con cinco preprocesos distintos, por si acaso

    RGB con media/escala de la doc   0.823
    BGR con media/escala de la doc   0.857   ← el mejor
    RGB con media/escala ImageNet    0.828
    RGB normalizado a 0-1            0.628
    RGB crudo                        0.713

Curioso: BGR gana a RGB pese a que el `model.yml` pide `reverse_input_channels`.
La diferencia está dentro del ruido con 29 caras reales, y da igual: ninguna
variante se acerca.

### El resultado, por tipo de ataque

AUC contra 24 caras reales, cada modelo con su recorte óptimo:

    ataque                    n     mn3      v2
    3D_paper_mask            36   0.956   0.985
    Cutout_attacks           15   0.978   1.000
    Galaxy_a54-Pixel7         8   0.943   1.000
    Mask 3                    5   0.892   0.942
    Mask 6                    6   0.562   0.882
    Mask_6                    5   0.683   1.000
    Mask_8                    5   0.200   0.950
    Samsung_S23              11   0.943   0.962
    Screen                    5   0.917   1.000
    iPhone14Pro              12   0.774   0.969
    id_06                     4   0.812   1.000
    id_14                     6   0.819   0.993
    ─────────────────────────────────────────
    GLOBAL                  123   0.843   0.970

**Pierde en las trece categorías, sin una sola excepción**, y cuesta 2,58 ms
por frame contra los 1,9 de las dos variantes de MiniFASNet juntas. No se
integra.

### Lo que este experimento enseña sobre las cifras publicadas

El 3,81 % de ACER es real y no es comparable con nada de aquí, por dos motivos.

**Está medido dentro de su propia distribución**, sobre el test split de
CelebA-Spoof, con el que se entrenó. Todo lo de este documento es
cross-dataset. Un modelo con siete veces más parámetros y 625.537 imágenes de
entrenamiento pierde 0,13 de AUC contra uno de 0,43 M en cuanto sale de casa.

**Y ACER es la media de APCER y BPCER**, que para este sistema son cosas
distintas: una deja pasar ataques y la otra rechaza personas reales. Un 3,81 %
puede ser 7/0,6 o 0,6/7. Publicar sólo la media oculta exactamente la
distinción sobre la que se apoya el §4.

No es un argumento contra el modelo: es un argumento contra elegir modelo por
la cifra del README.

---

## FLIP (CLIP + guía de lenguaje): probado y descartado

FLIP (ICCV 2023) es CLIP ViT-B/16 afinado para anti-spoofing alineando la
imagen con frases —seis que describen un rostro real, seis un ataque—. Su
promesa es la generalización *cross-domain*, que es exactamente la debilidad
que este documento venía midiendo.

Se probaron los dos checkpoints de 0-shot: `oulu` (protocolo MCIO: impresión y
replay) y `wmca` (protocolo WCS, que sí incluye máscaras).

Herramientas: `bench/runner/flip_export.py` convierte el checkpoint a ONNX más
una matriz 2×512 con las doce frases ya codificadas —en producción sería un
producto escalar, no un modelo de lenguaje— y `flip_eval.py` lo mide.

### Coste: por sesión, nunca por frame

CLIP ViT-B/16 tarda **45,3 ms** por pasada con un hilo. El presupuesto por
frame son 33 ms a 30 fps, así que por frame es imposible. Tres pasadas por
sesión son 136 ms de una sesión de 19 s: irrelevante.

Esa restricción llevaba arrastrándose sin cuestionar. El PAD pasivo no tiene
por qué correr en cada frame, y verlo abre la puerta a modelos que se
descartaban por coste.

### El resultado

AUC sobre el banco, 29 caras reales contra 118 ataques, cada modelo con su
recorte óptimo y dos frames por archivo:

    flip-oulu    0.880
    flip-wmca    0.877
    clip-base    0.883      ← CLIP SIN AFINAR
    minifas-v2   0.933

**FLIP rinde peor que el CLIP del que parte.** Tiene una explicación coherente
y algo irónica: afinar especializa hacia los dominios de entrenamiento y gasta
parte de la generalidad que traía el modelo base. CLIP sin afinar nunca ha
visto un dataset de anti-spoofing, así que tampoco se ha alejado del nuestro.

Por categorías sí hay complementariedad: en `3D_paper_mask` —la más grande del
banco, n=36— `flip-oulu` da 1,000 contra 0,932 de MiniFASNet. Pero en `Mask_8`
da 0,075, que es peor que el azar.

### La mejora que no era

Combinando por rango normalizado salía 0,962 contra 0,933. Dos errores míos:

1. **Normalizar por rangos usa el dataset entero.** En producción no hay
   dataset contra el que rankear; la combinación no era desplegable.
2. **Elegí la mejor de seis combinaciones mirando los datos con los que
   medía.** Es exactamente lo que `--holdout` existe para evitar.

Rehecho con una regla desplegable (sigmoide fija por modelo, luego media) y
bootstrap de 300 remuestreos:

    combinación            AUC     intervalo 90%   gana a solo
    minifas-v2 solo       0.936    0.886-0.973        —
    + flip-oulu           0.908    0.869-0.945       18 %
    + flip-wmca           0.914    0.869-0.954       25 %
    + clip-base           0.945    0.907-0.975       67 %

FLIP **empeora** en las dos variantes. CLIP a secas mejora 0,009 de AUC y sólo
en dos de cada tres remuestreos: con 29 caras reales eso no es una mejora, es
ruido con signo. No paga 345 MB de modelo ni 45 ms.

### Conclusión

Ninguno entra. Y el patrón de las tres evaluaciones de hoy —anti-spoof-mn3,
FLIP-oulu, FLIP-wmca— es el mismo: modelos con más parámetros, más datos de
entrenamiento y mejores cifras publicadas rinden peor aquí que uno de 0,43 M.
Las cifras publicadas están medidas dentro de su propia distribución.

Recordatorio de proporción: el PAD pasivo pesa **0,08 de 1,00** en la fusión.
Aunque alguno hubiera ganado, el veredicto apenas se movería. Lo que decide son
los retos activos, y ésos siguen sin medirse contra un solo ataque físico.

---

## El pulso se medía en absoluto, y el balance de blancos lo destruía

Tercer tropiezo con la misma piedra. El camino del pulso pasaba a POS la media
cruda de las regiones faciales —`regions.mean(axis=0)`— y era **el único del
analizador que no usaba `region_ratio()`**, cuyo propio docstring dice que es
«la medida base de todo lo demás». Calibración, destello y pipeline sí la usan.

Por qué importa, y no es lo obvio: POS normaliza cada ventana en el tiempo, así
que **ya cancela la deriva común a los tres canales**, que es lo que hace la
auto-exposición. Lo que no cancela es el **balance de blancos automático**, que
mueve cada canal por su lado; esa oscilación cromática cae dentro de la banda
del pulso y se lee como latido.

Medido con el POS real sobre escena sintética, AUC de vivo contra superficie
plana:

    deriva de balance de blancos    absoluto    diferencial
                            0 %       0,713          0,617
                          0,5 %       0,415          0,625
                          2,0 %       0,424          0,625

**Por debajo de 0,5 significa que la máscara puntúa más que el rostro vivo.**
El diferencial cuesta algo con la cámara perfectamente quieta —el fondo aporta
su propio ruido— y a cambio no se desploma, que es lo que hace una webcam real
con el balance automático puesto.

Arreglado además: la nariz sale del promedio (peor irrigada, más especular, más
sensible al giro de cabeza), y un frame con el fondo por debajo de 12 de
luminancia se descarta porque sin divisor no hay medida diferencial. El cliente
pide `whiteBalanceMode: 'manual'` como pista adicional, sabiendo que muchas
cámaras lo ignoran.

## El pulso no puede acusar: el tono de piel lo impide

El hallazgo más serio de toda la investigación, y cambia el perfil de decisión.

El SNR del rPPG depende del tono de piel tanto como de que haya latido. La
melanina está en la epidermis, **por encima** del lecho capilar, y atenúa por
igual la componente continua y la pulsátil: no reduce la profundidad de
modulación, reduce el **número de fotones**. Medido (Nowara et al., CVPRW
2020), el SNR de POS por tipo Fitzpatrick:

    FST      I      II     III    IV     V      VI
    dB    +0,05  ...........................  −5,58

con el suelo de ruido de la métrica en torno a −6,5 dB. Es decir: **para FST VI
la salida es estadísticamente indistinguible de no haber medido nada.** En
MMPD la correlación de POS en FST 5 es ρ = −0,03; en VitalVideo (893 sujetos)
el error va de <1 lpm en FST 1 a 11-17 lpm en FST 6, monótono en los nueve
modelos probados.

La física cuadra: ~5× menos luz devuelta predice 10·log₁₀(5) ≈ 7,0 dB de
pérdida; Nowara mide 7,1.

**Consecuencia: no existe umbral que separe una máscara de una persona de piel
oscura.** Un suelo sobre `rppg_snr` rechazaría por tono de piel y lo llamaría
fraude.

Implementado: `rppg_snr` está en `signalsWithoutFloor` y el perfil **no se
carga** si alguien le pone un suelo, con el motivo escrito al lado. Lo comprueba
`TestPulsoNoAdmiteSuelo`. La asimetría que queda es la correcta — un SNR alto
absuelve (si hay pulso, no es máscara, y eso vale para cualquiera), uno bajo no
acusa.

Y el límite honesto: con webcam RGB de consumo, **A4 sigue sin cubrirse para
ese grupo** aunque se alargue el tramo tranquilo. La alternativa a explorar es
la balistocardiografía (micro-movimiento de cabeza por la eyección sanguínea),
que no depende de la reflectancia: brecha claro-oscuro medida de 0,99 lpm
frente a 4,97 de los métodos cromáticos, con la misma webcam y sin hardware.

## Correcciones sobre rPPG y deepfakes

Se comparó lo que reportaron dos líneas de investigación independientes y
sobrevivió lo siguiente:

- **`0,999 AUC` de DeepFakesON-Phys no se sostiene fuera de su dominio.**
  Se cae a 0,622 al cambiar de condiciones.
- **PAD-Phys** (COMPSAC 2023) es la evidencia directa: una representación rPPG
  aprendida sobre deepfakes da 39-43 % de ACER al llevarla a PAD real, contra
  un azar del 50 %. **No transfiere.**
- En el benchmark conjunto de IEEE TDSC 2024, añadir rPPG a un detector de
  deepfakes **empeora** el resultado.
- Las cifras de DeepRhythm (0,641) y MesoNet (0,745) son **accuracy, no AUC**;
  una de las dos líneas las había mezclado con AUC en la misma tabla. Y el
  dato interesante es que DeepRhythm **lleva un Meso-4 dentro**: fuera de
  dominio pierde contra su propio submódulo de textura, o sea que todo lo que
  el rPPG añade encima es negativo al cruzar de dataset.
- Las tablas internas del paper de ICIAP 2023 **no están verificadas**: todos
  los espejos dan 403. Sólo el resumen.

La distinción que lo explica: contra una máscara el pulso es señal de **primer
orden** —hay latido o no lo hay—; contra un deepfake es de **segundo orden**
—el latido existe y se busca que esté distorsionado—. La segunda es mucho más
frágil, y es la que no transfiere.

**Conclusión: el rPPG se queda como contramedida contra máscaras en captura
controlada, que es lo que este sistema hace. No se importa nada de la línea de
deepfakes**, ni por licencia (lo único con pesos abiertos no la declara) ni por
eficacia.

## Dónde encaja este sistema en las normas

Verificado contra el texto de ISO, no de memoria:

- **ISO/IEC 30107** se limita al dispositivo de captura por declaración
  explícita de alcance, repetida literalmente en las partes 1, 3 y 4: «los
  ataques considerados tienen lugar en el dispositivo de captura durante la
  presentación; cualquier otro queda fuera de alcance». La inyección **no**
  aparece en ninguna parte de 30107-3, ni en su Anexo A de clasificación de
  ataques.
- La inyección se trata aparte: **ISO/IEC WD 25456**, «Biometric data
  injection attack detection», y no es de SC 37 sino de **SC 27**. Estado:
  borrador de trabajo en resolución de comentarios (etapa 20.60). Nada citable
  todavía como requisito.
- Los deepfakes, más atrás aún: **ISO/IEC AWI 26655** (SC 37), etapa 20.00.
- **ISO/IEC 30107-4** NO aplica aquí: es un perfil para dispositivos móviles
  con reconocimiento **local**, y excluye explícitamente el reconocimiento
  remoto, que es lo que hace este sistema.
- La única norma publicada que ya nombra la detección de inyección es
  **ISO/IEC 27553-2:2025** (SC 27), sobre autenticación biométrica remota en
  móvil, que la incluye como factor del vector de nivel de garantía.

O sea que el «fuera de alcance en esta fase» del §5 está **alineado con las
normas**, y ahora se puede citar capítulo y verso.

---

## CASIA-FASD: el banco por fin tiene papel y pantalla

`~/datasets/liveness/casia-fasd`, 8.126 imágenes, 90 MB. Descargable sin
trámite desde `akahana/anti-spoofing-casiafasd` en Hugging Face.

Llena el hueco que tenía el banco anterior, que era 72 % máscaras y **cero
fotos impresas planas**:

    caras reales        1.990   (antes: 24)
    foto doblada        2.370   (antes: 0)
    foto recortada      1.758   (antes: 0)
    replay en pantalla  2.008   (antes: 20)

Son **30 sujetos**, con ~270 frames cada uno. Ése es el n que cuenta.

Procedencia: es una re-subida no oficial de un dataset cuyo original exige
acuerdo firmado. Sirve para evaluación interna; no para uso comercial ni para
publicar métricas sin conseguir la licencia original.

**Por eso esta sección se queda en términos cualitativos.** Las cifras de
APCER, BPCER y umbrales medidas sobre CASIA no se publican aquí: se guardan
fuera del repositorio hasta tener la licencia original. Lo que sigue son las
conclusiones, que no dependen de los números exactos.

### El umbral no transfiere entre bancos, y ahora está medido

El umbral del PAD pasivo ajustado sobre el banco de máscaras, llevado tal cual
a CASIA, **rechaza a la mayoría de las personas reales**. El umbral óptimo en
CASIA resulta ser de otro orden de magnitud que el de axon-fas.

Lo que salva al sistema de esto es una decisión de diseño anterior: en
producción el PAD pasivo **no tiene suelo**. Alimenta la fusión con peso 0,06 y
0,02, así que un umbral mal calibrado no puede rechazar a nadie. El desastre
existe en el banco, no en el producto.

### Los detectores son complementarios, y eso sí es buena noticia

- **Textura** pilla bien el papel, doblado o recortado, y se le escapa buena
  parte de la pantalla.
- **Moiré y bandeo** pillan casi toda la pantalla y se les escapa la mayoría
  del papel.

Lo segundo no es un fallo: una foto impresa no tiene rejilla de píxeles. Cada
uno cubre lo que el otro no.

### La partición reservada ahora parte por SUJETO

Partía por fichero. Con ~270 frames por persona, la misma cara caía en las dos
mitades y lo que medía el "held-out" no era generalización sino memoria.

Partiendo por sujeto, las tasas empeoran de forma clara para todos los
detectores pasivos, que es lo esperable al medir generalización de verdad. Y
el bandeo es el que justifica el cambio: su umbral óptimo varía en **dos
órdenes de magnitud** según qué mitad toque. Eso no es un punto de operación,
es ruido — y partiendo por fichero quedaba escondido.

---

## Una red lenta acusaba de fraude

Primera sesión remota, por túnel, de una persona distinta. Resultado:

    VEREDICTO reject  score=0.8501  ['attack_no_color_response']

Score 0,8501 —muy por encima del 0,75 de aprobado— y trece de trece señales
medidas. Lo tumbó un suelo: `flash_correlation` exige 0,30 y dieron 0,2201 y
0,2768.

La causa no era la persona:

    flash  frames=10        flash  frames=9
    frames_dropped_rate_limit 141 de 352 recibidos   (40 %)

**Nueve frames para dos tramos de color.** La correlación se calculó sobre
cuatro muestras por tramo. Eso no es una correlación baja: es una correlación
que no se ha podido calcular — y el sistema la trató como prueba de ataque.

Arreglado con `min_frames_per_segment = 5`: por debajo, la ventana sale como
**no medible** y lleva a reintentar. Test de regresión en
`tests/test_flash_ambient_gate.py`.

### Y el ancho de banda es una restricción de producto, no de la prueba

    captura            subida necesaria
    720p @ 30 fps         49 Mbit/s
    720p @ 15 fps         25 Mbit/s
    480p @ 30 fps          8 Mbit/s
    480p @ 15 fps          4 Mbit/s

La decisión de subir a 720p/30fps asumía red local. Por internet no es viable,
y en móvil menos. Toca donde duele: se subió precisamente para que el pulso
tuviera muestras, y a 480p/15fps su AUC medido cae de 0,92 a 0,62.

Hay un conflicto real entre **medir bien el pulso** y **funcionar por
internet**, y hoy no tiene solución limpia. Las salidas serían subir resolución
sólo durante el tramo de quietud, o aceptar que el pulso es una señal de redes
buenas.

---

## WebP: probado en producción y revertido

El cliente comprimía a JPEG con calidad 0,92-0,95 en casi todos los pasos.
Medido sobre frames reales de una sesión, comparando cuánto distorsiona cada
formato la respuesta diferencial cara/fondo —la magnitud sobre la que se
construye todo lo demás—:

    formato       KB/frame   Mbit/s @30fps   error
    JPEG q92        166,2         40,8       0,121 %
    JPEG q75         73,8         18,1       0,220 %
    WebP q75         35,8          8,8       0,272 %
    WebP q50         26,1          6,4       0,318 %

Contra qué se compara: **el destello mueve la piel un 5 %** y **el pulso un
0,25 %**. WebP q50 distorsiona un 0,32 %, quince veces por debajo del destello.

De 41 Mbit/s a 6,4. Eso convierte 720p/30fps de inviable por internet a
perfectamente posible — que era el conflicto directo con el pulso, cuyo AUC
medido cae de 0,92 a 0,62 al bajar a 480p/15fps.

### La política de calidad ya no es un número

Tres necesidades distintas, y sólo una necesita calidad alta:

    fase                  antes        ahora     por qué
    quietud (pulso)    JPEG 0,95    WebP 0,85    señal del 0,25 %: no se baja
    mirada             JPEG 0,92    WebP 0,75    el BORDE del iris, no el color
    destello           JPEG 0,92    WebP 0,60    señal del 5 %: sobra margen
    calibración        JPEG 0,92    WebP 0,60    se compara con el destello
    pose               JPEG 0,60    WebP 0,50    geometría, no matiz

La mirada va por ENCIMA del destello aunque su señal sea mayor: no mide color
sino el borde del iris, que ocupa una docena de píxeles y es lo primero que
emborrona un códec.

### Lo que NO se hizo, y por qué

**Vídeo de verdad (H.264, VP9) queda descartado por física, no por
arquitectura.** Un códec inter-frame comprime descartando lo que apenas cambia
entre frames, y eso es exactamente lo que hay que medir: el pulso mueve la piel
un 0,25 % de un frame al siguiente. Además, los macrobloques *skip* de H.264
reproducen píxeles idénticos entre frames — que es justo la firma que sirve
para **detectar replays**. Sería fabricar el propio ataque.

Lo único de esa familia que serviría es todo-intra por WebCodecs, que gana otro
30-40 % sobre WebP. WebP ya da el 84 % del ahorro sin tocar la arquitectura.

**AVIF** comprime aún mejor pero `canvas.toBlob('image/avif')` casi no está
soportado y codificar es lento.

### Una trampa del navegador que hay que conocer

`canvas.toBlob` con un tipo no soportado **no falla**: devuelve PNG en
silencio. Un PNG de 720p pesa un mega y saturaría la subida sin que nada lo
delatara. Por eso `pickEncoding()` comprueba el `blob.type` de verdad al abrir
la cámara, en vez de fiarse, y cae a JPEG si el navegador no trae WebP.

### Y en una sesión real se cayó

Todo lo anterior medía **distorsión**, no **resultado**. Puesto en producción,
el resultado fue malo:

    cadencia en el tramo de quietud    JPEG: 21 fps    WebP: 7 fps
    frames tirados por la red                          1

Los frames no se perdían: **no se producían**. Codificar WebP en el navegador
es 28 veces más lento que JPEG, medido sobre un frame de 720p:

    JPEG q92    1,3 ms   158 KB
    JPEG q70    1,0 ms    63 KB
    WebP q60   28,2 ms    27 KB

A 30 fps el presupuesto por frame son 33 ms, así que el códec se los come casi
enteros él solo. El cuello de botella no era la red: era la CPU del cliente, y
cambié una por otra sin comprobarlo.

Para el pulso es el peor cambio posible — vive de tener muestras, y le quité
dos tercios.

**Lección que vale más que el experimento:** medir la distorsión de un códec
dice cuánta señal sobrevive, no cuántos frames vas a poder mandar. Son dos
presupuestos distintos y hay que mirar los dos.

### Lo que sí se quedó

El ahorro de banda se saca por el otro lado, bajando la calidad del JPEG. La
escala nueva sale de la misma tabla de distorsión:

    fase                  antes        ahora     error     por qué
    quietud (pulso)        0,95         0,92     0,12 %    señal del 0,25 %
    mirada                 0,92         0,82     ~0,2 %    el BORDE del iris
    destello               0,92         0,70     0,22 %    señal del 5 %
    calibración            0,92         0,70     0,22 %    se compara con él
    pose                   0,60         0,55       —       geometría

De 166 KB a unos 70 en los pasos que dominan la sesión: **60 % menos de banda
por un milisegundo de CPU**, que es el 84 % de lo que prometía WebP sin su
coste.

Si algún día se quiere WebP de verdad, el camino es `OffscreenCanvas` en un
Worker para sacar la codificación del hilo principal.

---

## Aprobar con la evidencia de dos pasos

Una sesión duró 4,6 segundos:

    calibration  aceptada, pero NO medible
    gaze         RECHAZADA (avance 6,17° sobre 5, pero racha 1 de 3)
    VEREDICTO    pass  score=0.902  señales=5

Aprobó a alguien tras un reto fallido, sin destello, sin pose y sin pulso. Dos
fallos independientes se sumaron:

**1. Un guion cortado contaba como completado.** Un paso rechazado cierra la
fase de retos (§6) y eso lleva la máquina al mismo estado que agotar el guion.
`finishedScript` se ponía en los dos casos, así que
`require_completed_challenges` —que existe justo para no decidir con evidencia
parcial— daba el visto bueno a una sesión que se había cortado en el segundo
paso.

**2. La ventana de mirada contradecía a su propio paso.** Reportaba
`response = 1,0` porque el avance (6,17°) superaba el mínimo (5°), mientras el
paso se rechazaba por no haberlo **sostenido** — racha de 1 cuando hacen falta
3 de los últimos 5.

Un pico aislado es exactamente lo que la regla del sostenimiento existe para
descartar: el ruido de los landmarks del iris produce picos en cualquier
dirección. Reportarlo como medida buena le entregaba a la fusión justo la señal
que el paso acababa de negar.

Arreglados los dos, con test cada uno. El de la mirada tapa el valor a 0,2
cuando no se sostuvo: por debajo del suelo de `gaze_response`, así que un pico
no absuelve, pero tampoco se pierde que hubo movimiento hacia el lado correcto.

## Las grabaciones ya guardan el sello de cada frame

Causa raíz de cinco diagnósticos fallidos en un solo día. El replay
reconstruía los tiempos a la cadencia nominal —30 fps— cuando la real es de 14
a 21, así que la ventana de un destello caía desplazada y se medía ruido: daba
correlación 0,000 incluso sobre grabaciones que en vivo habían dado 1,0.

Son ocho bytes por frame. Las grabaciones nuevas ya se pueden analizar offline;
las viejas no, y el replay ahora **avisa** cuando le faltan en vez de
inventarlos en silencio.

Primera medida con sellos reales: **29,4 fps**, contra los 30 pedidos.

---

## El gateway tiraba el 42 % de los frames que le llegaban

Sesión por túnel, reto de mirada fallado. La traza de la respuesta era
impecable — trece frames seguidos entre 22° y 40°, medio segundo largo, con los
dos ojos de acuerdo a 0,82 estable. Los landmarks del iris no fallaron nada.

Las cuentas del gateway:

    el cliente capturó      172 frames a 29,4 fps
    frames_received         100
    dropped_rate_limit       72
                           ────
    100 + 72 = 172   ← llegaron TODOS

Ni uno se perdió en la red. El limitador de caudal medía la separación por la
hora de **llegada**, y una red con jitter entrega en el mismo milisegundo
frames capturados con 33 ms de separación. Los tiraba por haber viajado juntos.

El efecto no era sutil: el reto de mirada se quedó con **7 fps efectivos**. Una
respuesta de medio segundo son 15 frames a 30 fps y sólo 3 a 7, así que la
regla de «3 de los últimos 5» caía del lado equivocado.

Arreglado: se limita sobre el sello de CAPTURA del cliente cuando su reloj es
plausible —cosa que ya se validaba— y se vuelve a la hora de llegada cuando no
lo es, que es lo que impide que un cliente hostil inunde declarando sellos
falsos. Test en `internal/api/limits_test.go`.

**Y una lección de método:** durante dos días atribuí estos fallos al ancho de
banda, a la resolución, a la calidad del JPEG y al detector de iris. Ninguna era
la causa. La cuenta que lo resolvió —sumar los recibidos y los descartados y ver
que daba exactamente los capturados— estaba disponible desde el principio en
`/metrics`.
