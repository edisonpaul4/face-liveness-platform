package conn

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/edisonpaul4/biometrics/orchestrator/core/fusion"

	"github.com/edisonpaul4/biometrics/gateway/internal/analyzer"
	"github.com/edisonpaul4/biometrics/orchestrator/core/challenge"
	"github.com/edisonpaul4/biometrics/orchestrator/core/session"
)

// evaluator decide si el paso activo se ha cumplido, a partir de las medidas
// del analizador.
//
// PROVISIONAL: esto es el hueco de la fusión (orchestrator/internal/fusion).
// Cuando exista, la decisión se toma allí con pesos y umbrales configurables,
// no aquí con constantes. Lo que sí es definitivo es la frontera: el
// analizador entrega magnitudes y es Go quien concluye (CLAUDE.md §3).
//
// El evaluador NO mira el reloj para decidir si la respuesta llegó a tiempo:
// de eso se encarga la máquina de estados, que es la única que conoce la
// ventana mínima. Aquí sólo se responde "¿ha ocurrido ya lo que se pidió?".
type evaluator struct {
	step       session.RevealedStep
	revealedAt time.Time

	frames     int
	faceFrames int

	// baselineArea es el tamaño del rostro al empezar el paso, para medir
	// acercamientos en relativo.
	baselineArea float64
	hasBaseline  bool

	// seenSegments son los tramos de la secuencia de destello ya observados
	// con la respuesta cromática correcta.
	seenSegments map[int]bool

	// flashBaseRatio es la relación rostro/fondo ANTES del destello, y
	// flashPeakRatio la mayor durante el primer tramo. Su cociente dice si la
	// pantalla llegó a iluminar la cara.
	flashBaseRatio float64
	flashPeakRatio float64

	// flashSamples es la serie de relaciones rostro/fondo por canal durante
	// todo el destello, con su instante. Es la materia prima de la
	// correlación: preguntar "¿la serie sigue a la secuencia que emití?" en
	// vez de "¿este frame es rojo?".
	flashSamples []flashSample

	// gazeBaseline es hacia dónde miraba el sujeto al empezar el paso. La
	// mirada se mide en RELATIVO contra ella, igual que el destello se mide
	// contra la línea base de color: la posición de reposo del iris depende
	// de la cara, de las gafas y de dónde esté la cámara respecto a la
	// pantalla, y ninguna de las tres se conoce.
	// gazeBestX es el mayor avance observado hacia el lado pedido, para poder
	// explicar un fallo.
	gazeBestX float64
	// gazeStreak son los frames cumplidos dentro de la ventana reciente.
	gazeStreak int
	// gazeSeries son TODAS las miradas medidas del paso, cada una con el
	// momento en que su frame llegó al servidor. No se reparten en dos fases
	// al vuelo porque dónde cae la frontera no se sabe hasta el final: ver
	// gazeAdvance.
	gazeSeries []gazeObs
	// Reparto ganador de la búsqueda de retardo, sólo para el log.
	gazeBestLag time.Duration
	// gazeSplitOK dice si en algún reparto hubo dos fases que comparar. Es
	// la medida de si se PUDO medir, y no puede confundirse con el avance:
	// irse al lado contrario es no cumplir, no es no haberse podido medir.
	gazeSplitOK               bool
	gazeFromN, gazeToN        int
	gazeBestYaw, gazeBestIris float64

	// gazeBestShift es el mayor avance hacia la esquina observado en UN
	// frame. Los máximos por eje se alcanzan en frames distintos, así que
	// combinarlos daría un número que nunca ocurrió.
	gazeBestShift float64

	// Extremos observados. No deciden nada: existen para poder explicar en el
	// log del SERVIDOR por qué un reto no se dio por cumplido. Sin esto, un
	// "inconclusive" con una cámara de verdad no se puede diagnosticar.
	maxYaw   float64
	minYaw   float64
	maxPitch float64
	minPitch float64
	maxArea  float64
}

// Umbrales del evaluador provisional.
const (
	yawThresholdDeg = 20.0
	// Medido con una cámara real: una barbilla levantada a conciencia da unos
	// 16 grados, y el cabeceo incidental de alguien quieto llega a 6. Doce
	// queda entre los dos. Sigue sin estar calibrado como es debido: hace
	// falta más de una cara para eso.
	// 10 grados, y el número bajó al quitar el estirón de la captura.
	//
	// Ensanchar la cara la hace parecer MÁS inclinada: medido sobre 149
	// frames, el mismo gesto que estirado daba 12 grados corregido da 8,8
	// (x0,735, r=0,993). Con el umbral en 12 y la geometría ya correcta,
	// tres de siete levantadas de barbilla REALES se habrían quedado cortas
	// — y un paso de pose fallado cierra la fase de retos (§6).
	//
	// 10 sale de esos mismos datos: el gesto natural medido corregido va de
	// 8,8 a 17,5 grados con mediana 13,7, y 10 queda por encima del vaivén
	// normal de la cabeza con margen. Lo que hace el trabajo antifraude de
	// este reto no es el ángulo sino el paralaje, que no depende del umbral.
	pitchThresholdDeg = 10.0

	// El acercamiento se mide en RELATIVO contra el tamaño al empezar el
	// paso. La cara de cada persona ocupa lo que ocupa según su cámara y su
	// distancia a la mesa: con una webcam de portátil ronda el 7 % del
	// encuadre, no el 20 % que da un maniquí renderizado a pantalla.
	// 1,3. Se probó 1,8 y hubo que deshacerlo: la teoría era correcta y la
	// medida dijo que no.
	//
	// El razonamiento seguía en pie —el área de una cara en la imagen va con
	// 1/d² y la iluminancia de la pantalla también, así que pedir 1,8 veces el
	// área es pedir 1,8 veces la luz—. Lo que falló es que el beneficio no
	// apareció y el coste sí.
	//
	// No apareció: con el sujeto al 54 % del encuadre, la modulación del
	// destello siguió en el 1,2 % y ninguna de las dos ventanas correlacionó.
	// La medida es diferencial contra el fondo del MISMO frame, y al llenar
	// medio encuadre el fondo pasa a ser lo que hay justo detrás del sujeto,
	// que recibe el destello casi igual que su cara: el cociente cancela justo
	// lo que se quería amplificar.
	//
	// Y el coste fue que la MIRADA dejó de poder medirse. A 20 cm la cámara
	// mira la cara desde muy abajo y el ojo se escorza en vertical: medido
	// sobre tres sesiones, con la cara cerca el ancho de ojo pasó de 68 a 156
	// px mientras la razón alto/ancho caía de 0,166 a 0,087, por debajo del
	// mínimo de 0,15. El 100 % de los frames se descartaron y el reto de
	// mirada se cerró con avance CERO, sin una sola medida.
	//
	// El acercamiento se mide en RELATIVO contra el tamaño al empezar el paso.
	// La cara de cada persona ocupa lo que ocupa según su cámara y su
	// distancia a la mesa: con una webcam de portátil ronda el 7 % del
	// encuadre, no el 20 % que da un maniquí renderizado a pantalla.
	closerGrowthFactor = 1.3
	// Suelo absoluto de cordura, no el criterio. Sólo descarta que se dé por
	// bueno un crecimiento relativo enorme sobre una cara diminuta al fondo
	// de la habitación.
	closerMinArea     = 0.05
	colorLitThreshold = 0.5

	// La mirada se juzga por el VECTOR de desplazamiento, no eje por eje.
	//
	// Exigir a cada eje su propio umbral parece más estricto y es más frágil:
	// los dos ejes no responden igual. Medido con una cámara real mirando a
	// una esquina inferior, el horizontal se movió 0,045 y el vertical 0,010,
	// y el reto se cayó por el eje débil pese a que el movimiento era enorme.
	// La causa es física y no se va a arreglar: la gente acompaña con la
	// cabeza —y si la cabeza baja, el ojo se recentra y el desvío vertical
	// desaparece—, y al mirar abajo el párpado tapa parte del iris.
	//
	// Así que: la DIRECCIÓN confirma el cuadrante y la MAGNITUD confirma que
	// hubo movimiento de verdad.
	//
	// La mirada se mide en DOS canales distintos, uno por eje, y no es una
	// simetría rota por capricho: es que el ojo no funciona igual en los dos.
	//
	// HORIZONTAL: desplazamiento del iris dentro de la órbita. Fiable y
	// grande. Medido en una respuesta real y sostenida: 0,043.
	//
	// VERTICAL: la APERTURA del párpado, no la posición del iris. Al mirar
	// arriba el párpado se retrae y al mirar abajo baja con el ojo, así que
	// el centro del iris apenas se mueve respecto a las comisuras: ahí estaba
	// el error. Medido en la misma respuesta real: el iris se movió 0,007
	// —indistinguible del ruido— mientras la apertura subía un 22 %.
	//
	// Escalas: lo que produce una respuesta correcta en cada canal. Se
	// normaliza por ellas para poder promediar peras con manzanas.
	// La mirada se mide en el MUNDO, no respecto a la cabeza.
	//
	// El desplazamiento del iris dentro de la órbita dice hacia dónde miran
	// los ojos RESPECTO A LA CARA, y eso no basta: si alguien gira la cabeza
	// hacia el objetivo, sus ojos se recentran en la órbita y la medida se
	// queda en cero, aunque esté mirando exactamente donde se le pidió. Peor
	// aún, si gira la cabeza y sigue mirando al mismo sitio, el iris
	// contrarrota y la medida se va en sentido CONTRARIO.
	//
	// Medido con una cámara real: cabeza girada 18° y iris desplazado −0,060.
	// Sumando los dos, el cambio de mirada real fue de 0,1 grados — no se
	// movió. Con sólo el iris, parecía un movimiento enorme hacia el lado
	// equivocado.
	//
	// gazeDegPerOffset convierte desplazamiento de iris a grados de mirada.
	//
	// 221, no 300. El 300 salía de esa única medida de arriba. Re-derivado
	// sobre seis pasos de pose de grabaciones reales —donde la cabeza gira y
	// la mirada se queda en la pantalla, así que el iris contrarrota— la
	// pendiente es limpia y consistente:
	//
	//     Δyaw 28,3°  Δiris 0,137  →  227   r = -0,96
	//     Δyaw 24,8°  Δiris 0,121  →  223   r = -0,93
	//     Δyaw 21,9°  Δiris 0,115  →  218   r = -0,96
	//     Δyaw 30,9°  Δiris 0,151  →  233   r = -0,97
	//     Δyaw 29,2°  Δiris 0,198  →  160   r = -0,99
	//     Δyaw 30,3°  Δiris 0,163  →  215   r = -0,99
	//
	// Con 300 se acreditaban de más los grados: un avance que se reportaba
	// como 9,7° eran 7,1° de verdad.
	// 248, re-derivado sobre grabaciones reales DESPUÉS de quitar el estirón
	// de la captura (ver camera.js). El método es el mismo que fijó el 221:
	// durante un paso de pose la cabeza gira y la mirada se queda en la
	// pantalla, así que el iris contrarrota, y la pendiente de esa
	// contrarrotación es la conversión.
	//
	// El cambio no es cosmético: con la geometría corregida, 7 de 8 pasos de
	// pose correlacionan a -0,85 o mejor, contra 5 de 8 con la estirada, y las
	// pendientes se agrupan mucho más. Sobre los mismos frames sin corregir la
	// mediana salía 201, no 221 — o sea que el número viejo ya no describía ni
	// el mundo estirado.
	gazeDegPerOffset = 248.0

	// gazeMinAdvanceDeg es cuánto tiene que girar la mirada hacia el
	// objetivo.
	//
	// 3°, y el número lo impone la GEOMETRÍA DE LA PANTALLA, no la fisiología.
	//
	// El objetivo se pinta al 6 % del ancho del viewport, así que el ángulo
	// que hay que recorrer depende del tamaño de la pantalla y de a qué
	// distancia se mira:
	//
	//     portátil  ~30 cm de ancho a ~50 cm  →  el punto está a unos 15°
	//     móvil      ~7 cm de ancho a ~30 cm  →  el punto está a unos 5,7°
	//
	// En un móvil, una mirada PERFECTAMENTE obediente produce unos 5,7°: un
	// umbral de 5 se comía casi toda la señal disponible. Medido con el mismo
	// sujeto el mismo día, sus avances fueron 6-13° en el portátil y 3,18° y
	// 4,00° en el teléfono, con el reto fallando las dos veces.
	//
	// Historia previa, que sigue siendo válida para el portátil: se bajó de 8
	// a 5 porque 8 pedía forzar la vista. Y cuidado al leer los cambios por
	// separado — corregir la escala de 300 a 221 ENDURECE el reto, porque los
	// mismos ojos producen menos grados.
	//
	// LO QUE ESTO CUESTA, medido sobre los tramos SIN objetivo de cuatro
	// grabaciones reales (931 muestras), donde cualquier disparo es falso:
	//
	//     umbral 5,0° → 4,0 % de ventanas disparan solas
	//     umbral 3,0° → 9,1 %
	//
	// Se duplica largamente, y 3° queda además POR DEBAJO de la deriva máxima
	// medida con la cabeza quieta (3,76°): a este umbral, quedarse quieto
	// puede contar como respuesta. Lo que sigue protegiendo es la dirección
	// —hay que moverse hacia el lado que se acaba de sortear— y el
	// sostenimiento durante 250 ms.
	//
	// Es una decisión de producto consciente: se acepta más ruido a cambio de
	// que el reto sea alcanzable en un móvil. La forma correcta de arreglarlo
	// no es este número sino escalar el umbral con el ángulo que el objetivo
	// subtiende de verdad, y para eso el cliente tendría que decir el tamaño
	// de su pantalla.
	// 3,4 y no 3,0: al quitar el estirón, la mirada en el mundo entera —cabeza
	// más ojos— crece un 14 %, así que el umbral crece con ella. No es
	// aflojar ni apretar: es el MISMO ángulo real de antes, medido sin la
	// distorsión. El ruido escala igual, así que la tasa de falsos disparos
	// documentada arriba se conserva.
	gazeMinAdvanceDeg = 3.4

	// gazeUnsustainedCap es lo más que puede valer una mirada cuyo avance no
	// se sostuvo.
	//
	// No es cero: hubo movimiento hacia el lado correcto y eso es más que
	// nada. Pero tampoco puede ser 1,0, que es lo que reportaba antes — una
	// ventana no puede declarar perfecta una respuesta que su propio paso
	// acaba de rechazar. Queda por debajo del suelo de `gaze_response` (0,25),
	// así que un pico aislado no absuelve.
	gazeUnsustainedCap = 0.2

	// flashMinRise es cuánto tiene que subir la relación rostro/fondo durante
	// el primer tramo —el blanco de referencia— para que valga la pena medir
	// el resto de la secuencia.
	//
	// El destello sólo funciona si la pantalla es una fuente de luz apreciable
	// sobre el rostro. Medido con una cámara real y una ventana detrás del
	// sujeto: el destello aportaba el 3 % del brillo de la cara y el 97 %
	// restante era ambiente. Con eso no hay técnica que valga — se está
	// midiendo una modulación del 3 % contra el ruido del sensor.
	//
	// Por debajo de este umbral no se dice "no respondió" sino "no se pudo
	// medir", y se le cuenta a la persona qué cambiar. Acusar a alguien de no
	// responder cuando el instrumento no estaba midiendo es exactamente lo
	// que §4 prohíbe.
	flashMinRise = 0.15
	// flashMinCorrelation es cuánto tiene que parecerse la serie observada a
	// la secuencia emitida. Correlación de Pearson, así que va de -1 a 1 y es
	// inmune a la escala: da igual que la modulación sea del 50 % o del 5 %,
	// lo que se mide es si SIGUE al patrón.
	//
	// Ahí está la diferencia con el umbral por frame, que exigía que cada
	// tramo fuera clasificable por sí solo.
	flashMinCorrelation = 0.35
	// flashMinSamples son las muestras mínimas para que una correlación
	// signifique algo. Menos de esto y correlaciona cualquier cosa.
	flashMinSamples = 8
	// flashMaxLag y flashLagStep acotan la búsqueda de retardo entre lo que
	// se emitió y lo que se ve: red, composición, captura y bus.
	flashMaxLag  = 400 * time.Millisecond
	flashLagStep = 33 * time.Millisecond

	// gazeMinOpenness es la fracción de SU PROPIA apertura habitual por debajo
	// de la cual el frame no cuenta. Relativa, no absoluta.
	//
	// Un umbral absoluto convierte la anatomía en motivo de rechazo, que es lo
	// que §4 prohíbe para el reposo de la mirada por la misma razón. Medido
	// sobre un usuario legítimo, su apertura iba de 0,045 a 0,139: con el
	// umbral absoluto de 0,12 se descartaban casi todos sus frames y dos
	// sesiones seguidas se cerraron con avance CERO, cuando su mirada sí
	// respondía —de 1,5° a 15°, con los dos ojos coincidiendo entre 0,67 y
	// 0,92—.
	//
	// La mitad de su mediana reciente: lo que esto rechaza es un PARPADEO, que
	// es una caída brusca respecto a lo normal de esa persona, y no una cara
	// cuyos ojos son como son o una cámara que la mira desde abajo.
	gazeMinOpennessRatio = 0.5
	// gazeOpennessFloor descarta lo que ya no es un ojo abierto en absoluto,
	// pase lo que pase con la mediana.
	gazeOpennessFloor = 0.03
	// gazeOpennessFrames es la ventana de aperturas recientes que fija lo
	// habitual del sujeto.
	gazeOpennessFrames = 15

	// SIGUEN SIN CALIBRAR como es debido: hace falta más de una cara. Para
	// eso están la grabación del cliente y bench/runner/replay.py.
	// gazeBaselineFrames son los frames del principio del paso que fijan la
	// posición de reposo. Con menos, un parpadeo contamina la referencia.
	gazeBaselineFrames = 3
	// gazeMinAgreement descarta la medida cuando los dos ojos no coinciden:
	// es un iris mal detectado —gafas, reflejo—, no una mirada rara.
	gazeMinAgreement = 0.5
	// gazeReferenceFrames es la ventana de miradas previas que sirve de
	// referencia. Medio segundo: suficiente para promediar el temblor, corto
	// para no arrastrar hacia dónde miraba en el reto anterior.
	gazeReferenceFrames = 8
	// gazeSustainSpan y gazeSustainPercent: hay que mantener el
	// desplazamiento durante gazeSustainSpan, en al menos gazeSustainPercent
	// de las muestras de ese tramo.
	//
	// En TIEMPO, no en frames, y la diferencia no es cosmética. La regla era
	// "3 de los últimos 5" y eso mide algo distinto en cada sesión: el ritmo
	// de cámara varía al doble según la luz —a 720p con poca luz la webcam
	// alarga la exposición y entrega 13,6 fps donde se le pidieron 30—, así
	// que cinco frames son 185 ms o 370 ms según la habitación. El propio
	// comentario de la regla vieja ya hablaba de "dos décimas": la cantidad
	// que se quería exigir siempre fue un tiempo.
	//
	// No SEGUIDOS: con el umbral cerca del valor alcanzado, el ruido hace que
	// un frame de cada tres baje por debajo, y exigir una racha limpia tiraba
	// respuestas correctas y sostenidas. Medido: quince frames claramente por
	// encima, ninguna racha de tres.
	// gazeSettle es lo que se descarta al principio de cada fase: el punto
	// acaba de aparecer o de saltar y la mirada aún va en camino. Medir ahí es
	// medir el trayecto, no el destino, y recorta el viaje por los dos
	// extremos.
	gazeSettle = 350 * time.Millisecond
	// gazeMaxLag es el retardo máximo que se busca entre revelar el reto y
	// ver la respuesta en un frame ya recibido.
	//
	// Buscarlo no es opcional, y es la misma lección que el destello ya tenía
	// escrita: entre que el servidor revela el punto y que el frame que
	// muestra la respuesta está en sus manos hay red de bajada, decisión y
	// pintado del cliente, reacción del sujeto, captura y red de subida.
	// Suponer sincronía exacta es suponer lo que no se puede.
	//
	// Costó una sesión legítima: el móvil pedía 18,5 Mbit/s de subida y el
	// enlace no daba, así que los frames llegaban tarde y cada vez más —2,0 s
	// de retraso ganados en 5,4 s de sesión—. Con el reparto fijo, la fase de
	// SALIDA se llenó con la mirada de llegada y el avance salió cero;
	// reprocesada con el reloj del propio cliente daba 6,16°.
	//
	// El techo es 0,8 s a propósito, y no más: la cola sin tope se arregla en
	// el cliente, que es donde estaba (ver `socketAtascado` en session.js).
	// Subirlo aquí para tapar un enlace saturado sería tapar el síntoma —y
	// además no se puede: con el retardo la respuesta llega después del plazo
	// del reto, así que no hay ventana que buscar.
	gazeMaxLag = 800 * time.Millisecond
	// gazeLagStep es el paso de la búsqueda. Más fino no compra nada: la
	// cámara entrega un frame cada 50-100 ms.
	gazeLagStep = 50 * time.Millisecond
	// gazeMaxBoundaryGap es el hueco máximo que puede haber entre la última
	// muestra de salida y la primera de llegada.
	//
	// Es la regla que distingue "no respondió" de "no lo vimos responder", y
	// faltaba. El salto del punto es el instante que hay que observar: si no
	// llega ningún frame alrededor, lo que se mide no es al sujeto.
	//
	// Costó una sesión: el móvil dejó de capturar 1,8 s justo en el salto —el
	// enlace se atascó— y el reto se cerró con avance CERO y la mirada entró
	// en la fusión como respuesta nula. La persona pudo haber obedecido y
	// vuelto la vista antes de que llegara el frame siguiente; no hay forma
	// de saberlo, y ésa es exactamente la razón de no puntuarlo.
	//
	// El hueco nominal ya es de gazeSettle (350 ms) por construcción, así que
	// 700 ms deja sitio a un par de frames perdidos y no a un parón.
	gazeMaxBoundaryGap = 700 * time.Millisecond
	// gazeMinArrivalSpan es cuánto tiene que abarcar en TIEMPO la fase de
	// llegada para que su mediana signifique algo.
	//
	// Es la regla de cobertura de la búsqueda, y hace el mismo papel que
	// MIN_LAG_OVERLAP en el destello: sin ella gana el retardo que deja
	// fuera media ventana —menos muestras, no mejor alineación— porque con
	// pocas muestras el máximo del ruido ya ronda el umbral.
	gazeMinArrivalSpan = 250 * time.Millisecond
	// gazePhaseMin son las muestras mínimas por fase para que su mediana
	// signifique algo. Con una sola, la mediana ES esa muestra.
	gazePhaseMin = 3
	//
	// Es lo que separa mirar de derivar. Medido con una cámara real: alguien
	// quieto, sin objetivo, deriva hasta 0,017 en horizontal y 0,012 en
	// vertical contra su propia línea base — demasiado cerca de los umbrales
	// para fiarse de un solo frame. Sostenerlo tres frames (dos décimas) es
	// trivial para quien mira de verdad y muy improbable por azar.
	gazeSustainPercent = 60
	// gazeSustainMin son las muestras mínimas para poder hablar de sostener.
	// Con una sola no se distingue una mirada de un fallo de detección.
	gazeSustainMin = 2
	// colorDominanceGap es cuánto tiene que despuntar un canal sobre los
	// otros para llamarlo primario. Comparar en relativo y no contra un
	// umbral fijo por canal es lo que hace que esto aguante una paleta con
	// algo de fuga, ganancias distintas del analizador y pieles distintas.
	colorDominanceGap = 0.15
	// minCalibrationHits son los frames CON ROSTRO que hacen falta para cerrar
	// la calibración.
	//
	// Va acoplado a `MIN_CALIBRATION_FRAMES` de `analyzer/calibration.py`, que
	// vale 5: por debajo de eso el analizador rechaza la línea base. El
	// gateway cerraba con 2, así que daba por buena una calibración que el
	// analizador iba a tirar — y sin línea base NINGÚN destello se puede
	// analizar después.
	//
	// Pasó de verdad: una sesión cerró la calibración con 4 frames (el caudal
	// está frío al arrancar: 2 fps ahí contra 10 fps en el reto siguiente), la
	// ventana salió `medible=false` y la sesión se resolvió en reintentar sin
	// que nada dijera que la causa estaba en el primer paso.
	//
	// Seis y no cinco: uno de margen, porque el analizador cuenta los frames
	// que le sirven y no todos los que tienen rostro.
	minCalibrationHits = 6
	// poseWindowTolerance es el margen que se le da al analizador alrededor
	// del ángulo objetivo. No es el umbral de aprobado —ese no sale de aquí—
	// sino cuánto se considera "haber llegado" al medir la trayectoria.
	poseWindowTolerance = 8.0
	// flashPaintDelay es lo que tarda el color en llegar a la pantalla desde
	// que el servidor revela el reto: red, decisión del cliente y composición.
	// Medido: 15-17 ms de pintado más el viaje.
	flashPaintDelay = 120 * time.Millisecond
)

func newEvaluator(step session.RevealedStep, revealedAt time.Time) *evaluator {
	return &evaluator{
		step:         step,
		revealedAt:   revealedAt,
		seenSegments: make(map[int]bool, len(step.Flash)),
	}
}

// observe incorpora las medidas de un frame y devuelve si el paso ya está
// cumplido.
func (e *evaluator) observe(f analyzer.Features, at time.Time) bool {
	e.frames++
	if f.Quality.FaceDetected {
		e.faceFrames++
	}
	e.record(f)
	elapsed := at.Sub(e.revealedAt)

	switch e.step.Kind {
	case challenge.KindCalibration:
		if !f.Quality.FaceDetected {
			return false
		}
		// La pantalla neutra tiene que sostenerse el tiempo impuesto: es la
		// línea base contra la que se medirán los destellos.
		return e.faceFrames >= minCalibrationHits && elapsed >= e.step.Hold

	case challenge.KindHold:
		if !f.Quality.FaceDetected {
			return false
		}
		// Como la calibración: se sostiene el tiempo impuesto. No hay nada
		// que responder —es una ventana de medida, no un reto— así que lo
		// único que se exige es que siga habiendo una cara delante.
		return e.faceFrames >= minCalibrationHits && elapsed >= e.step.Hold

	case challenge.KindPose:
		if !f.Quality.FaceDetected {
			return false
		}
		if !e.hasBaseline {
			e.baselineArea = f.SignalOr("face_area_ratio", 0)
			e.hasBaseline = true
		}
		return e.poseReached(f)

	case challenge.KindGaze:
		return e.gazeReached(f, elapsed)

	case challenge.KindFlash:
		e.trackFlashRise(f, elapsed)
		e.recordFlashSample(f, elapsed)
		// Se sigue anotando qué tramos se reconocieron uno a uno: no decide,
		// pero explica un fallo mucho mejor que un solo número.
		e.matchSegment(f, elapsed)

		// El paso se cumple por haber SONADO la secuencia entera. Si la piel
		// la siguió o no lo decide la ventana de Python, que es quien tiene
		// la medida buena.
		//
		// Antes se exigía aquí una correlación calculada por el gateway, y
		// costaba sesiones legítimas: medido, una sesión con correlación de
		// **0,4934** según Python —holgadamente por encima del umbral— se
		// resolvió como «demasiada luz ambiente» porque la correlación propia
		// del gateway no llegó. Es exactamente lo que §6 advierte de tener la
		// misma señal en dos sitios: la versión pobre arrastra a la buena.
		//
		// Que no haya respuesta no se pierde: llega a la fusión como una
		// correlación baja en la ventana, que es donde tiene que pesar.
		return elapsed >= e.step.Hold
	}

	return false
}

// poseReached comprueba si la cabeza llegó donde se le pidió.
//
// Convenio de signo, contraintuitivo y ya colado dos veces: los retos se
// enuncian desde el SUJETO ("gira a tu derecha", "levanta la barbilla"), pero
// los ángulos se miden desde la CÁMARA, que le mira de frente y NO va
// espejada —el espejo de la vista previa es puro CSS y no toca los frames—.
//
//	· girar hacia su derecha  → nariz a la izquierda de la imagen → yaw < 0
//	· girar hacia su izquierda → nariz a la derecha de la imagen  → yaw > 0
//	· levantar la barbilla     → pitch < 0
//
// Los tres comprobados contra una cámara real, no deducidos.
func (e *evaluator) poseReached(f analyzer.Features) bool {
	switch e.step.Pose {
	case challenge.PoseYawLeft:
		return f.SignalOr("pose_yaw_deg", 0) >= yawThresholdDeg
	case challenge.PoseYawRight:
		return f.SignalOr("pose_yaw_deg", 0) <= -yawThresholdDeg
	case challenge.PosePitchUp:
		return f.SignalOr("pose_pitch_deg", 0) <= -pitchThresholdDeg
	case challenge.PoseMoveCloser:
		area := f.SignalOr("face_area_ratio", 0)
		return area >= closerMinArea && area >= e.baselineArea*closerGrowthFactor
	default:
		return false
	}
}

// gazeReached comprueba si la mirada se desplazó hacia el cuadrante del
// objetivo.
//
// Se comprueba el CUADRANTE, no la posición: con el iris ocupando una docena
// de píxeles, la resolución no da para más, y prometer precisión que no se
// tiene es peor que no medir.
// gazeSample es una muestra de la ventana de sostenimiento.
type gazeSample struct {
	at time.Duration
	ok bool
}

// gazeReached mide el VIAJE entre los dos puntos del reto.
//
// No hay referencia de reposo, y ésa es toda la idea. Medir contra el reposo
// tenía dos defectos que costaron sesiones reales:
//
//   - el recorrido disponible era del centro al borde, unos 5,7° en un móvil;
//     entre los dos puntos es el doble;
//   - y la referencia venía de fuera del paso, así que podía estar desfasada o
//     desplazada. Con el sujeto mirando 6,49° a un lado durante la
//     calibración, su vuelta al centro dio 4,68° de "avance" en dos frames y
//     la sesión acabó en `reject` por `temporal_response_too_fast`. Acusaba a
//     quien obedecía.
//
// Cada fase se resume por su MEDIANA, no por su pico: el pico premia un frame
// con el iris mal detectado, y de esos hay. La mediana necesita que la mayoría
// de la fase esté donde toca, que es justo lo que se quiere comprobar.
func (e *evaluator) gazeReached(f analyzer.Features, elapsed time.Duration) bool {
	world, ok := worldGaze(f)
	if !ok {
		// Gafas con reflejo, ojos entornados, cara lejos, o sin pose. No se
		// pudo medir, y eso no es un fallo del sujeto: no cuenta ni a favor
		// ni en contra.
		return false
	}
	e.gazeSeries = append(e.gazeSeries, gazeObs{
		at:   elapsed,
		deg:  world,
		yaw:  f.SignalOr("pose_yaw_deg", 0),
		iris: f.SignalOr("gaze_offset_x", 0),
	})
	return e.gazeAdvance() >= gazeMinAdvanceDeg
}

// gazeObs es una mirada medida, con el momento en que su frame llegó.
type gazeObs struct {
	at   time.Duration
	deg  float64
	yaw  float64
	iris float64
}

// gazeAdvance devuelve cuánto viajó la mirada hacia el segundo punto, en
// grados, buscando dónde cae de verdad la frontera entre las dos fases.
//
// Para cada retardo candidato se parte la serie igual que la partiría un
// reloj sin latencia y se compara la MEDIANA de cada mitad. Se queda con el
// mejor reparto: el retardo real es desconocido pero acotado, y el que mejor
// separa las dos fases es el que mejor las describe.
//
// La mediana y no el pico: el pico premia un frame con el iris mal detectado,
// y de ésos hay. Y la mediana es además lo que hace honesta la búsqueda —un
// sujeto que no responde no tiene ningún reparto que separe nada, porque no
// hay dos poblaciones que separar.
func (e *evaluator) gazeAdvance() float64 {
	sentido := float64(-e.step.Gaze.Side())
	mejor := 0.0
	// El reparto se recalcula ENTERO en cada frame, así que lo que se anota
	// para el log también. Anotarlo sólo cuando mejora dejaba en el registro
	// el reparto de un frame anterior —con la serie todavía a medias— y ese
	// número contaba una historia falsa justo cuando más falta hacía: en un
	// paso que acaba en cero, que es cuando nada ha mejorado nunca.
	e.gazeSplitOK = false
	e.gazeBestLag, e.gazeFromN, e.gazeToN = 0, 0, 0
	e.gazeBestYaw, e.gazeBestIris = 0, 0
	for lag := time.Duration(0); lag <= gazeMaxLag; lag += gazeLagStep {
		salida := e.gazePhase(lag+gazeSettle, lag+e.step.Hold)
		llegada := e.gazePhase(lag+e.step.Hold+gazeSettle, time.Duration(math.MaxInt64))
		if len(salida) < gazePhaseMin || len(llegada) < gazePhaseMin {
			continue
		}
		if llegada[len(llegada)-1].at-llegada[0].at < gazeMinArrivalSpan {
			continue
		}
		// Hay que haber visto el ANTES y el DESPUÉS cerca del salto. Un
		// reparto que se apoya en muestras lejanas a los dos lados de un
		// parón describe cualquier cosa menos la respuesta al punto.
		if llegada[0].at-salida[len(salida)-1].at > gazeMaxBoundaryGap {
			continue
		}
		// Hubo dos fases que comparar. Se anota aunque el sujeto se haya ido
		// al lado contrario o se haya quedado quieto: eso es NO CUMPLIR, que
		// es muy distinto de no haberse podido medir. Confundirlos deja fuera
		// de la fusión una respuesta que sí dice algo.
		if !e.gazeSplitOK {
			e.gazeSplitOK = true
			e.gazeBestLag = lag
			e.gazeFromN, e.gazeToN = len(salida), len(llegada)
			e.gazeBestYaw = medianOf(yaws(llegada)) - medianOf(yaws(salida))
			e.gazeBestIris = medianOf(irises(llegada)) - medianOf(irises(salida))
		}
		avance := (medianOf(degs(llegada)) - medianOf(degs(salida))) * sentido
		if avance <= mejor {
			continue
		}
		mejor = avance
		e.gazeBestLag = lag
		e.gazeFromN, e.gazeToN = len(salida), len(llegada)
		// Cómo obedeció, separando cabeza de ojos. Sin esto no se puede
		// calibrar el umbral con sesiones reales en vez de a ojo.
		e.gazeBestYaw = medianOf(yaws(llegada)) - medianOf(yaws(salida))
		e.gazeBestIris = medianOf(irises(llegada)) - medianOf(irises(salida))
	}
	e.gazeBestShift = mejor
	return mejor
}

// gazePhase devuelve las muestras del intervalo [desde, hasta).
func (e *evaluator) gazePhase(desde, hasta time.Duration) []gazeObs {
	out := make([]gazeObs, 0, len(e.gazeSeries))
	for _, o := range e.gazeSeries {
		if o.at >= desde && o.at < hasta {
			out = append(out, o)
		}
	}
	return out
}

func degs(o []gazeObs) []float64 { return campo(o, func(g gazeObs) float64 { return g.deg }) }
func yaws(o []gazeObs) []float64 { return campo(o, func(g gazeObs) float64 { return g.yaw }) }
func irises(o []gazeObs) []float64 {
	return campo(o, func(g gazeObs) float64 { return g.iris })
}

func campo(o []gazeObs, f func(gazeObs) float64) []float64 {
	out := make([]float64, len(o))
	for i, g := range o {
		out[i] = f(g)
	}
	return out
}

// gazeTrace resume la serie como "t@grados/iris", con t en milisegundos desde
// que se reveló el paso.
//
// Es lo único que separa las tres formas de dar cero, que desde fuera son
// idénticas: que el sujeto no respondiera, que no llegaran frames mientras
// respondía, o que la mirada llegara pero el iris viniera plano.
func (e *evaluator) gazeTrace() string {
	var b strings.Builder
	for i, o := range e.gazeSeries {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%d@%.1f/%.3f", o.at.Milliseconds(), o.deg, o.iris)
	}
	return b.String()
}

// gazeMeasured dice si el reto llegó a tener dos fases que comparar.
//
// Es distinto de que el sujeto no respondiera: aquí no se pudo ni mirar.
func (e *evaluator) gazeMeasured() bool { return e.gazeSplitOK }

// medianOf devuelve la mediana sin alterar el original.
func medianOf(v []float64) float64 {
	c := append([]float64(nil), v...)
	slices.Sort(c)
	return median(c)
}

// worldGaze estima hacia dónde mira el sujeto en el mundo, en grados, como
// suma de hacia dónde apunta su cabeza y hacia dónde miran sus ojos dentro de
// ella. Positivo es hacia su izquierda.
//
// Los dos términos hacen falta: quien gira la cabeza hacia el objetivo no
// mueve los ojos dentro de la órbita, y quien mira de reojo no gira la cabeza.
// Cualquiera de las dos formas de obedecer tiene que contar igual.
func worldGaze(f analyzer.Features) (float64, bool) {
	offset, okOffset := f.Signal("gaze_offset_x")
	yaw, okYaw := f.Signal("pose_yaw_deg")
	if !okOffset || !okYaw {
		return 0, false
	}
	if agreement, ok := f.Signal("gaze_agreement"); ok && agreement < gazeMinAgreement {
		return 0, false
	}
	return yaw + offset*gazeDegPerOffset, true
}

// openEnough dice si el ojo estaba lo bastante abierto PARA ESTA PERSONA.
//
// La apertura ya no mide la mirada —el eje vertical se abandonó— pero sigue
// diciendo si hay ojo que medir: en pleno parpadeo el iris que se detecta es
// medio iris. Lo que no puede es compararse contra un número fijo, porque la
// apertura depende de la cara y del ángulo de la cámara.
func (c *Conn) openEnough(f analyzer.Features) bool {
	open, ok := f.Signal("gaze_openness")
	if !ok {
		return true
	}
	if open < gazeOpennessFloor {
		return false
	}
	if len(c.recentOpenness) < 3 {
		return true
	}
	valores := append([]float64(nil), c.recentOpenness...)
	slices.Sort(valores)
	return open >= median(valores)*gazeMinOpennessRatio
}

// trackFlashRise mide cuánto sube la relación rostro/fondo al encender el
// blanco de referencia.
//
// Es la comprobación de que hay instrumento: si la pantalla no ilumina la
// cara por encima del ambiente, no hay nada que medir en el resto de la
// secuencia por muy bien que responda el sujeto.
func (e *evaluator) trackFlashRise(f analyzer.Features, elapsed time.Duration) {
	ratio, ok := f.Signal("surface_face_bg_luminance_ratio")
	if !ok || ratio <= 0 {
		return
	}
	// Antes de que el primer color llegue a la pantalla: reposo.
	if elapsed < flashPaintDelay {
		if e.flashBaseRatio == 0 {
			e.flashBaseRatio = ratio
		} else {
			e.flashBaseRatio = (e.flashBaseRatio + ratio) / 2
		}
		return
	}
	if len(e.step.Flash) > 0 && elapsed <= e.step.Flash[0].Duration {
		e.flashPeakRatio = max(e.flashPeakRatio, ratio)
	}
}

// flashRise es cuánto subió la relación rostro/fondo, en tanto por uno.
func (e *evaluator) flashRise() (float64, bool) {
	if e.flashBaseRatio <= 0 || e.flashPeakRatio <= 0 {
		return 0, false
	}
	return e.flashPeakRatio/e.flashBaseRatio - 1, true
}

// flashSample es la relación rostro/fondo por canal en un instante.
type flashSample struct {
	elapsed time.Duration
	// bgr en el orden del analizador.
	bgr [3]float64
}

func (e *evaluator) recordFlashSample(f analyzer.Features, elapsed time.Duration) {
	b, okB := f.Signal("surface_face_bg_b")
	g, okG := f.Signal("surface_face_bg_g")
	r, okR := f.Signal("surface_face_bg_r")
	if !okB || !okG || !okR {
		return
	}
	e.flashSamples = append(e.flashSamples, flashSample{elapsed: elapsed, bgr: [3]float64{b, g, r}})
}

// flashCorrelation mide cuánto se parece la serie observada a la secuencia
// emitida, con búsqueda de retardo.
//
// Es un filtro adaptado, y es la diferencia entre medir y no medir. Un umbral
// por frame necesita que CADA tramo sea clasificable por sí solo, lo que exige
// una modulación grande. La correlación contra una secuencia conocida y
// aleatoria saca la señal de debajo del ruido, porque el ruido no está
// correlacionado con esa secuencia y la respuesta sí. Medido con una cámara
// real, la modulación era del 5 % y el umbral por frame no veía nada.
//
// La búsqueda de retardo no es opcional: entre que el servidor revela el reto
// y el color llega a la retina hay red, decisión del cliente, composición y
// captura. Suponer sincronía exacta es suponer lo que no se puede.
func (e *evaluator) flashCorrelation() (score float64, lag time.Duration, ok bool) {
	if len(e.flashSamples) < flashMinSamples || len(e.step.Flash) == 0 {
		return 0, 0, false
	}

	best := -2.0
	var bestLag time.Duration
	for shift := time.Duration(0); shift <= flashMaxLag; shift += flashLagStep {
		if c, okc := e.correlateAt(shift); okc && c > best {
			best, bestLag = c, shift
		}
	}
	if best < -1 {
		return 0, 0, false
	}
	return best, bestLag, true
}

// correlateAt correlaciona la serie con la secuencia desplazada `lag`.
func (e *evaluator) correlateAt(lag time.Duration) (float64, bool) {
	// Series por canal, centradas: restar la media quita la deriva lenta de
	// la cámara, que es justo lo que contamina la medida absoluta.
	var obs, exp [3][]float64
	for _, sample := range e.flashSamples {
		idx, inside := segmentAt(e.step.Flash, sample.elapsed-lag)
		if !inside {
			continue
		}
		want := channelWeights(e.step.Flash[idx].Color)
		for c := range 3 {
			obs[c] = append(obs[c], sample.bgr[c])
			exp[c] = append(exp[c], want[c])
		}
	}
	if len(obs[0]) < flashMinSamples {
		return 0, false
	}

	// Se promedian los tres canales: la respuesta cromática reparte la señal
	// entre ellos y quedarse con uno tiraría dos tercios.
	total, used := 0.0, 0
	for c := range 3 {
		if v, ok := pearson(obs[c], exp[c]); ok {
			total += v
			used++
		}
	}
	if used == 0 {
		return 0, false
	}
	return total / float64(used), true
}

// channelWeights dice qué canales enciende un color, en orden BGR.
func channelWeights(color challenge.FlashColor) [3]float64 {
	switch color {
	case challenge.FlashRed:
		return [3]float64{0, 0, 1}
	case challenge.FlashGreen:
		return [3]float64{0, 1, 0}
	case challenge.FlashBlue:
		return [3]float64{1, 0, 0}
	case challenge.FlashWhite:
		return [3]float64{1, 1, 1}
	default:
		return [3]float64{0, 0, 0}
	}
}

// pearson es la correlación entre dos series. false si alguna es constante:
// una serie sin variación no correlaciona con nada, y forzar un número ahí
// sería inventarlo.
func pearson(a, b []float64) (float64, bool) {
	n := float64(len(a))
	var ma, mb float64
	for i := range a {
		ma += a[i]
		mb += b[i]
	}
	ma /= n
	mb /= n

	var num, da, db float64
	for i := range a {
		x, y := a[i]-ma, b[i]-mb
		num += x * y
		da += x * x
		db += y * y
	}
	if da <= 1e-12 || db <= 1e-12 {
		return 0, false
	}
	return num / math.Sqrt(da*db), true
}

// matchSegment anota el tramo de destello si la respuesta cromática del
// rostro coincide con el color que tocaba en ese instante.
//
// Un desajuste no se castiga: puede ser un frame capturado justo en la
// frontera entre dos colores. Lo que cuenta es haber visto responder a TODOS
// los tramos, que es lo que un vídeo grabado no puede hacer.
func (e *evaluator) matchSegment(f analyzer.Features, elapsed time.Duration) {
	idx, ok := segmentAt(e.step.Flash, elapsed)
	if !ok {
		return
	}
	if dominantColor(f) == e.step.Flash[idx].Color {
		e.seenSegments[idx] = true
	}
}

// segmentAt localiza qué tramo de la secuencia se está pintando.
func segmentAt(seq []challenge.FlashSegment, elapsed time.Duration) (int, bool) {
	if elapsed < 0 {
		return 0, false
	}
	var acc time.Duration
	for i, seg := range seq {
		acc += seg.Duration
		if elapsed < acc {
			return i, true
		}
	}
	return 0, false
}

// dominantColor traduce la respuesta cromática medida al color que la
// provocó. Devuelve FlashUnspecified si no hay respuesta clara: es lo que
// pasa con una foto o una pantalla, que reflejan mal y de forma uniforme.
//
// La comparación es ENTRE canales, no contra un umbral por canal. Un umbral
// fijo obliga a que los primarios lleguen puros al analizador; en cuanto la
// pantalla pinta un rojo lavado, o el analizador amplifica un poco más, los
// tres canales cruzan el umbral y todo parece blanco.
func dominantColor(f analyzer.Features) challenge.FlashColor {
	values := [3]float64{
		f.SignalOr("color_response_r", 0),
		f.SignalOr("color_response_g", 0),
		f.SignalOr("color_response_b", 0),
	}

	top, second, low := 0, 0.0, values[0]
	for i, v := range values {
		if v > values[top] {
			top = i
		}
		if v < low {
			low = v
		}
	}
	for i, v := range values {
		if i != top && v > second {
			second = v
		}
	}

	switch {
	case values[top] < colorLitThreshold:
		// No respondió nada: ni destello, ni superficie que sepa responder.
		return challenge.FlashUnspecified
	case values[top]-low <= colorDominanceGap:
		// Los tres a la par y encendidos: luz blanca.
		return challenge.FlashWhite
	case values[top]-second <= colorDominanceGap:
		// Dos canales empatados arriba. No es ninguno de los colores de la
		// paleta, así que no se afirma nada.
		return challenge.FlashUnspecified
	}

	return [3]challenge.FlashColor{
		challenge.FlashRed,
		challenge.FlashGreen,
		challenge.FlashBlue,
	}[top]
}

// record anota los extremos de lo observado, para el resumen del log.
func (e *evaluator) record(f analyzer.Features) {
	yaw := f.SignalOr("pose_yaw_deg", 0)
	e.maxYaw = max(e.maxYaw, yaw)
	e.minYaw = min(e.minYaw, yaw)
	pitch := f.SignalOr("pose_pitch_deg", 0)
	e.maxPitch = max(e.maxPitch, pitch)
	e.minPitch = min(e.minPitch, pitch)
	e.maxArea = max(e.maxArea, f.SignalOr("face_area_ratio", 0))
}

// summary describe lo que se llegó a ver durante el paso.
//
// Va al log del servidor y NUNCA al cliente: son exactamente los números que
// le dirían a un atacante qué le faltó para colar el intento (CLAUDE.md §6).
func (e *evaluator) summary() []any {
	fields := []any{
		"kind", e.step.Kind.String(),
		"frames", e.frames,
		"con_rostro", e.faceFrames,
	}
	switch e.step.Kind {
	case challenge.KindPose:
		fields = append(fields,
			"pose", e.step.Pose.String(),
			"yaw_max", round2(e.maxYaw), "yaw_min", round2(e.minYaw),
			"pitch_max", round2(e.maxPitch), "pitch_min", round2(e.minPitch),
			"area_max", round2(e.maxArea), "area_base", round2(e.baselineArea))
	case challenge.KindFlash:
		fields = append(fields,
			"tramos", len(e.step.Flash),
			"tramos_vistos", len(e.seenSegments),
			"subida_destello", round3(e.flashRiseOrZero()), "minimo_subida", flashMinRise)
	case challenge.KindHold:
		fields = append(fields, "quietud_s", e.step.Hold.Seconds())
	case challenge.KindGaze:
		fields = append(fields,
			"lado", map[int]string{-1: "izquierda", 1: "derecha"}[e.step.Gaze.Side()],
			"avance_grados", round2(e.gazeBestShift), "minimo", gazeMinAdvanceDeg,
			// Cuánto retardo hizo falta suponer para que las dos fases se
			// separaran. Es el diagnóstico de red del reto: si sube, los
			// frames están llegando tarde y todo lo que se mide en ventanas
			// —destello incluido— está en peligro.
			"retardo_ms", e.gazeBestLag.Milliseconds(),
			"delta_yaw", round2(e.gazeBestYaw), "delta_iris", round3(e.gazeBestIris),
			"muestras", len(e.gazeSeries),
			"muestras_salida", e.gazeFromN,
			"muestras_llegada", e.gazeToN,
			// La serie entera. Es ruidoso y sólo sirve en desarrollo, pero
			// sin ella un avance de cero no se distingue de una serie que
			// nunca llegó a cubrir las dos fases — y las dos veces que ha
			// fallado este reto la diferencia era justo ésa.
			"serie", e.gazeTrace())
	}
	return fields
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// gazePoint es una mirada medida en el mundo, en grados. Positivo es hacia la
// izquierda del sujeto.
type gazePoint struct {
	deg float64
	// at es cuándo se CAPTURÓ el frame que produjo esta mirada, no cuándo
	// volvieron sus medidas. Sin esto no se puede saber si la referencia de
	// reposo describe el instante en que apareció el objetivo o uno de hace
	// dos segundos, que es un fallo indistinguible de una mirada que no se
	// movió.
	at time.Time
}

// gazeReference resume una ventana de miradas en una referencia.
//
// Mediana y no media: si en la ventana cayó un parpadeo o un frame con el
// iris mal detectado, la media se lo lleva y la mediana no.
func gazeReference(window []gazePoint) (gazePoint, bool) {
	if len(window) < gazeBaselineFrames {
		return gazePoint{}, false
	}
	degrees := make([]float64, 0, len(window))
	newest := time.Time{}
	for _, p := range window {
		degrees = append(degrees, p.deg)
		if p.at.After(newest) {
			newest = p.at
		}
	}
	slices.Sort(degrees)
	return gazePoint{deg: median(degrees), at: newest}, true
}

func median(sorted []float64) float64 {
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// window traduce lo observado a una ventana de fusión.
//
// Sólo se rellenan las sub-métricas que el gateway MIDE de verdad hoy. Las
// que dependen de la puntuación por ventana del analizador —paralaje,
// gradiente 3D, continuidad, identidad, ausencia de pantalla— se quedan
// fuera, y eso es exactamente lo correcto: una señal que no se pudo medir no
// entra en la fusión, porque meterla como cero sería fabricar evidencia
// contra quien simplemente no fue medido (CLAUDE.md §4).
//
// El motor renormaliza por los pesos de las señales presentes, así que una
// fusión con menos señales no es un apaño: es su comportamiento diseñado.
// window traduce lo observado a una ventana de fusión.
//
// Reparto de trabajo, y no es arbitrario: PYTHON puntúa pose y destello,
// porque paralaje, gradiente 3D, ausencia de pantalla, continuidad e identidad
// son visión por computador y eso no se hace en Go (CLAUDE.md §3). El gateway
// aporta lo que Python no puede: la mirada, que se mide con las señales por
// frame y el guion en la mano, y el aviso de que la habitación tapaba el
// destello.
//
// De ahí que aquí no se emita ni `compliance` ni `correlation`: las emite
// quien las mide mejor. Duplicarlas las haría competir, y la fusión se queda
// con la PEOR de las ventanas que reporten una señal — así que la versión
// pobre arrastraría a la buena.
func (e *evaluator) window(satisfied bool) (fusion.Window, bool) {
	w := fusion.Window{ID: e.step.ID, QualitySufficient: true}

	switch e.step.Kind {
	case challenge.KindGaze:
		w.Kind = fusion.WindowGaze
		if !e.gazeMeasured() {
			// Gafas con reflejo, ojos cerrados, cara lejos, o una de las dos
			// fases sin muestras suficientes. No se pudo medir.
			w.QualitySufficient = false
			w.QualityReason = "no se pudo medir la mirada"
			return w, true
		}
		// El avance que se reporta es el SOSTENIDO, no el pico.
		//
		// Antes iba el pico, y la ventana contradecía a su propio paso: una
		// mirada con avance de 6,17° sobre un mínimo de 5° reportaba
		// `response = 1,0` —perfecto— mientras el paso se rechazaba por no
		// haberlo sostenido (racha de 1, y hacen falta 3 de los últimos 5).
		//
		// Un pico aislado es exactamente lo que la regla del sostenimiento
		// existe para descartar: el ruido de los landmarks del iris produce
		// picos sueltos en cualquier dirección. Reportarlo como medida buena
		// le daba a la fusión la señal que el paso acababa de rechazar.
		response := clamp01(e.gazeBestShift / gazeMinAdvanceDeg)
		if !satisfied {
			// No se sostuvo: lo que hubo fue un pico, y vale a lo sumo lo que
			// la regla admite sin darlo por bueno.
			response = math.Min(response, gazeUnsustainedCap)
		}
		w.Submetrics = map[string]*float64{"response": ptr(response)}
		return w, true

	case challenge.KindFlash:
		// El gateway ya NO emite ventana de destello, ni siquiera para avisar
		// de la luz ambiente.
		//
		// Lo hacía, y su aviso se apoyaba en una correlación propia peor que
		// la de Python. Medido: una sesión con 0,4934 de correlación según
		// Python acabó en «demasiada luz ambiente» porque la del gateway no
		// llegó. Python ya toma esa misma decisión —y en el orden correcto,
		// mirando la correlación ANTES que la amplitud— así que tener aquí una
		// segunda opinión sólo servía para que la peor ganara.
		//
		// Lo que sí se conserva es el registro por tramos (`trackFlashRise`,
		// `matchSegment`): no decide nada, y explica un fallo mucho mejor que
		// un solo número.
		return w, false

	case challenge.KindPose:
		// Que no hubiera rostro sí lo sabe el gateway, y lleva a reintentar.
		if e.faceFrames == 0 {
			w.Kind = fusion.WindowPose
			w.QualitySufficient = false
			w.QualityReason = "sin rostro en la ventana"
			return w, true
		}
		return w, false

	case challenge.KindHold:
		// El tramo de quietud no produce señal por sí mismo: lo que se mide
		// en él —el pulso— lo calcula Python y llega por su propia ventana.
		// Aquí no hay nada que aportar a la fusión.
		return w, false

	default:
		return w, false
	}
}

func ptr(v float64) *float64 { return &v }

// boolScore traduce un cumplido/no cumplido a algo que la fusión pueda pesar.
//
// El fallo no vale 0 sino 0,15: no llegar al ángulo pedido es mala señal, pero
// no es la misma evidencia que un plano moviéndose. Reservar el 0 para lo
// segundo es lo que impide que un despiste se lea como un ataque.
func boolScore(ok bool) float64 {
	if ok {
		return 1.0
	}
	return 0.15
}

func clamp01(v float64) float64 {
	return math.Max(0, math.Min(1, v))
}

// flashRiseOrZero es flashRise para el log, sin el segundo valor.
func (e *evaluator) flashRiseOrZero() float64 {
	rise, _ := e.flashRise()
	return rise
}

// flashAmbientReason marca la ventana de destello que no se pudo medir porque
// la habitación tapaba la pantalla. Lo mira la conexión para poder avisar.
const flashAmbientReason = "la luz ambiente aplasta el destello"
