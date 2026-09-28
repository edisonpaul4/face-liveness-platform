package challenge

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// ErrScriptExhausted indica que el guion no tiene más pasos.
var ErrScriptExhausted = errors.New("challenge: guion agotado")

// Script es el guion de una sesión: calibración y retos, en orden.
//
// Es de AVANCE ÚNICO por diseño. No expone longitud, ni acceso por índice, ni
// iteración: sólo el paso actual y la posibilidad de avanzar. Un llamante que
// tenga el Script en la mano no puede leer el futuro aunque quiera, y ningún
// error de programación puede filtrarlo hacia el cliente.
//
// No es seguro para uso concurrente: la sesión que lo contiene es su dueña.
type Script struct {
	steps  []Step
	cursor int
}

// Generate construye el guion de forma determinista a partir de la semilla.
//
// La misma semilla y la misma política producen siempre el mismo guion, byte
// a byte: es lo que permite a /bench reproducir un caso exacto.
//
// Composición:
//   - Paso 0: calibración, pantalla neutra durante CalibrationHold.
//   - Después, entre MinSteps y MaxSteps retos en orden aleatorio, con al
//     menos uno de destello siempre presente.
//   - Las poses se muestrean sin reemplazo: repetir una es regalar un segundo
//     intento al mismo movimiento. Los destellos sí pueden repetirse, cada
//     uno con su propia secuencia y duraciones, hasta maxFlashSteps.
func Generate(seed Seed, p Policy) (*Script, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}

	rng := newPRNG(uint64(seed))
	n := p.MinSteps + rng.intn(p.MaxSteps-p.MinSteps+1)

	// Un destello garantizado; el resto se sortea. Si se agotan las poses
	// disponibles, el hueco se cubre con destello.
	poses := make([]PoseAction, len(poseCatalog))
	copy(poses, poseCatalog)
	rng.shuffle(len(poses), func(i, j int) { poses[i], poses[j] = poses[j], poses[i] })
	// La primera pose que se reparta tiene que ser ANGULAR.
	//
	// Acercarse no es un ángulo: el analizador no tiene ventana para eso —lo
	// mide el gateway por el tamaño del rostro— así que un guion cuya única
	// pose sea `move_closer` se queda sin paralaje, cumplimiento, continuidad
	// e identidad. Cuatro señales de trece, y una de ellas es de las dos que
	// de verdad separan un rostro de una superficie plana.
	//
	// El resto del orden sigue sorteado; esto sólo asegura que la primera
	// sirva para medir.
	for i, a := range poses {
		if a != PoseMoveCloser {
			poses[0], poses[i] = poses[i], poses[0]
			break
		}
	}

	kinds := make([]Kind, 0, n)
	usedPoses, usedFlashes, usedGazes := 0, 0, 0

	// El destello va garantizado salvo en el modo de desarrollo: es la única
	// defensa que no depende de que el sujeto colabore bien (§5, A2 y A3).
	if !p.Skips(KindFlash) {
		kinds = append(kinds, KindFlash)
		usedFlashes++
	}

	// Y una mirada, por el mismo motivo. Es de la familia del destello
	// —reto-respuesta atado a ESTE instante— y es la que un vídeo grabado no
	// puede superar: no puede mirar al punto que acaba de aparecer en un lado
	// elegido al azar.
	//
	// Sorteada, el 8,4 % de los guiones salían sin ninguna mirada, medido
	// sobre 5000 semillas. En esas sesiones se decidía sin una señal que pesa
	// 0,11 y que además tiene suelo propio. Dejar al azar si una defensa
	// participa o no es lo mismo que no tenerla una de cada doce veces.
	//
	// El destello tiene prioridad si no caben todas: un guion de un solo paso
	// se queda con él, porque es la única defensa que no depende de que el
	// sujeto colabore bien.
	if !p.Skips(KindGaze) && len(kinds) < n {
		kinds = append(kinds, KindGaze)
		usedGazes++
	}

	// Y una pose, que por el barajado de arriba será angular. Sin ella se
	// pierden paralaje, cumplimiento, continuidad e identidad: medido sobre
	// 5000 semillas, al 22 % de los guiones les faltaba.
	if !p.Skips(KindPose) && len(kinds) < n && usedPoses < len(poses) {
		kinds = append(kinds, KindPose)
		usedPoses++
	}

	for len(kinds) < n {
		// Se sortea entre lo que queda disponible. Cada tipo tiene su techo:
		// las poses no se repiten, y los otros dos se cansan de distinto modo.
		options := make([]Kind, 0, 3)
		if !p.Skips(KindPose) && usedPoses < len(poses) {
			options = append(options, KindPose)
		}
		if !p.Skips(KindFlash) && usedFlashes < maxFlashSteps {
			options = append(options, KindFlash)
		}
		if !p.Skips(KindGaze) && usedGazes < maxGazeSteps {
			options = append(options, KindGaze)
		}
		if len(options) == 0 {
			// Validate lo impide; si pasara, un guion más corto es mejor que
			// uno inventado.
			break
		}

		switch options[rng.intn(len(options))] {
		case KindPose:
			kinds = append(kinds, KindPose)
			usedPoses++
		case KindFlash:
			kinds = append(kinds, KindFlash)
			usedFlashes++
		default:
			kinds = append(kinds, KindGaze)
			usedGazes++
		}
	}

	// Orden aleatorio: el destello garantizado no queda siempre el primero.
	rng.shuffle(len(kinds), func(i, j int) { kinds[i], kinds[j] = kinds[j], kinds[i] })
	// Los destellos, al final y juntos. Y separar mirada de pose DESPUÉS de
	// agrupar: al mover un destello de en medio se pegarían dos pasos que
	// antes estaban separados por él.
	kinds = groupFlashesLast(kinds)
	kinds = separateGazeFromPose(kinds)

	// El tramo de quietud va entre los retos que NO son destello, en posición
	// sorteada. No cuenta como reto: se añade aparte para no quitarle un paso
	// al guion, igual que la calibración.
	//
	// La posición se sortea aunque el tramo no defienda nada por sí mismo. Es
	// barato, y le quita a un atacante el saber exactamente cuándo se le está
	// midiendo el pulso, que es el único momento en que le serviría fingirlo.
	quiet := len(kinds)
	for i, k := range kinds {
		if k == KindFlash {
			quiet = i
			break
		}
	}
	if !p.Skips(KindHold) {
		at := rng.intn(quiet + 1)
		kinds = append(kinds[:at:at], append([]Kind{KindHold}, kinds[at:]...)...)
	}

	steps := make([]Step, 0, len(kinds)+1)
	steps = append(steps, calibrationStep(seed, p))

	nextPose := 0
	var lastGaze *GazeTarget
	// lastFlashColor es el último color pintado por el destello anterior.
	//
	// Dos destellos seguidos pueden pegarse por el mismo color aunque cada
	// secuencia por dentro alterne: medido en una sesión real, el destello 1
	// acabó en rojo 355 ms y el 2 empezó en rojo 424 ms — 779 ms de rojo
	// continuo. La cámara se acomoda por completo en ese tramo unido, y en el
	// segundo destello la cara se DES-enrojece durante su propio tramo rojo.
	//
	// Es la misma razón por la que las secuencias son de dos colores (§4): a
	// partir del segundo tramo se mide la cámara acomodándose, no la piel. La
	// regla existía dentro de una secuencia y faltaba entre secuencias.
	lastFlashColor := FlashUnspecified
	for _, k := range kinds {
		switch k {
		case KindPose:
			steps = append(steps, poseStep(seed, len(steps), p, poses[nextPose]))
			nextPose++
		case KindGaze:
			step := gazeStep(seed, len(steps), p, rng, lastGaze)
			steps = append(steps, step)
			target := step.Gaze
			lastGaze = &target
		case KindHold:
			steps = append(steps, holdStep(seed, len(steps), p))
		default:
			// El primer color de un destello nunca repite el último del
			// anterior. Sin esto los dos se pegan por el mismo color y la
			// cámara se acomoda del todo en el tramo unido.
			step := flashStep(seed, len(steps), p, rng, lastFlashColor)
			lastFlashColor = step.Flash[len(step.Flash)-1].Color
			steps = append(steps, step)
		}
	}

	steps = approachBeforeFlashes(seed, p, steps)

	return &Script{steps: steps}, nil
}

// approachBeforeFlashes mete un "acércate" justo antes del primer destello.
//
// La iluminancia va con **1/d²**: de 55 cm a 30 cm son 3,4 veces más luz de
// pantalla sobre la cara. Y ahí es donde hace falta, porque el destello es la
// familia que más pesa en la fusión (0,41 entre gradiente, correlación y
// ausencia de pantalla) y la que trabaja pegada a su suelo: el propio
// repositorio tiene anotado que el suelo honesto de modulación ronda el 3-4 %
// y que lo medido con cámara real fue el 5 %. Medido en una sesión real a
// distancia normal, un destello correlacionó 0,1356 y el siguiente no se pudo
// medir siquiera. Con 3,4 veces más luz ese 5 % se va al 17 % y deja de estar
// pegado al ruido. Es lo que hace AWS Rekognition, y por esta razón.
//
// No cuenta como reto —igual que la calibración y el tramo de quietud— porque
// no defiende nada: acercarse no es un ángulo, no produce ventana de pose y no
// aporta ninguna señal. Es encuadre, y quitarle un paso al guion para ganar
// encuadre sería cambiar defensa por comodidad.
//
// Lo que cede al §6, dicho exactamente: un atacante aprende que el bloque de
// destellos empieza AHORA. Ya sabía que van al final —concesión documentada al
// agrupar los destellos— así que lo nuevo es sólo la frontera, y la aprendería
// un paso después al ver la pantalla teñirse. Lo que sigue sin saber es cuántos
// destellos vienen, de qué colores, en qué orden y con qué duraciones, que es
// lo que de verdad rompe un vídeo grabado.
func approachBeforeFlashes(seed Seed, p Policy, steps []Step) []Step {
	at := -1
	for i, s := range steps {
		if s.Kind == KindFlash {
			at = i
			break
		}
	}
	if at < 0 {
		return steps
	}
	// El índice va UNA posición más allá del último paso, no `at`: el
	// identificador se deriva de (semilla, índice) y `at` ya lo estaba usando
	// el destello que va justo detrás. Dos pasos con el mismo identificador
	// hacen ambiguo el correlacionar una ventana con su reto.
	approach := poseStep(seed, len(steps), p, PoseMoveCloser)
	// Sin mínimo de reacción, y no es una excepción de conveniencia.
	//
	// Ese mínimo existe porque responder antes de lo humanamente posible a un
	// parámetro SORTEADO demuestra que la respuesta venía pregrabada: nadie
	// puede girar hacia un lado que aún no se había elegido. "Acércate" no
	// tiene parámetro sorteado —no hay lado, ni ángulo, ni color que adivinar—
	// así que la rapidez aquí no delata nada, y aplicarlo convertía a quien se
	// acerca deprisa en un ataque. Medido: la sesión simulada acababa en
	// `reject` por `temporal_response_too_fast` sólo por añadir este paso.
	approach.Window.Min = 0
	out := make([]Step, 0, len(steps)+1)
	out = append(out, steps[:at]...)
	out = append(out, approach)
	return append(out, steps[at:]...)
}

// Current devuelve el paso actual. El segundo valor es false si el guion
// ya está agotado.
func (s *Script) Current() (Step, bool) {
	if s.cursor >= len(s.steps) {
		return Step{}, false
	}
	return s.steps[s.cursor], true
}

// Advance pasa al siguiente paso. Devuelve false si no queda ninguno, y en
// ese caso el guion queda agotado.
//
// Es la única forma de que un paso futuro se vuelva visible: no hay atajo.
func (s *Script) Advance() bool {
	if s.cursor >= len(s.steps) {
		return false
	}
	s.cursor++
	return s.cursor < len(s.steps)
}

// Position es cuántos pasos se han revelado ya, empezando en 0. Es
// información del pasado (telemetría del servidor) y nunca debe viajar al
// cliente: revelaría el avance dentro del guion.
func (s *Script) Position() int { return s.cursor }

// holdStep es la ventana de quietud donde se mide el pulso.
//
// Como la calibración, tampoco se puede "terminar antes": el mínimo de la
// ventana es la duración entera. Si un cliente dice haberla completado antes
// de tiempo, se la ha saltado, y sin los diez segundos no hay resolución en
// frecuencia con la que hablar de un latido.
func holdStep(seed Seed, index int, p Policy) Step {
	return Step{
		ID:   stepID(seed, index),
		Kind: KindHold,
		Hold: p.PulseHold,
		Window: Window{
			Min: p.PulseHold,
			Max: p.PulseHold + p.CalibrationSlack,
		},
	}
}

func calibrationStep(seed Seed, p Policy) Step {
	return Step{
		ID:   stepID(seed, 0),
		Kind: KindCalibration,
		Hold: p.CalibrationHold,
		Window: Window{
			// La calibración no se puede "terminar antes": si el cliente
			// dice haberla completado antes de tiempo, se la ha saltado.
			Min: p.CalibrationHold,
			Max: p.CalibrationHold + p.CalibrationSlack,
		},
	}
}

func poseStep(seed Seed, index int, p Policy, action PoseAction) Step {
	return Step{
		ID:   stepID(seed, index),
		Kind: KindPose,
		Pose: action,
		Window: Window{
			Min: p.MinReaction,
			Max: p.PoseDeadline,
		},
	}
}

// gazeStep pone un objetivo de mirada a un lado de la pantalla.
//
// Sólo izquierda y derecha: ver GazeTarget para por qué el eje vertical se
// descartó. Dos opciones en vez de cuatro es un bit menos por reto, y importa
// poco — lo que rompe un vídeo grabado no es cuántas opciones hay, sino que
// no puede responder a ninguna en el momento. Se compensa con más retos de
// mirada, que cuestan un segundo cada uno.
func gazeStep(seed Seed, index int, p Policy, rng *prng, previous *GazeTarget) Step {
	left := rng.intn(2) == 0
	// Nunca el mismo lado dos veces seguidas. Si el objetivo no se mueve, el
	// sujeto ya está mirando ahí y no hay nada que medir: medido, dos
	// objetivos iguales seguidos dieron 0,000 de desplazamiento y el reto se
	// cayó por hacer exactamente lo que se pedía.
	if previous != nil && (previous.Side() < 0) == left {
		left = !left
	}

	x := 1 - p.GazeMargin
	if left {
		x = p.GazeMargin
	}
	// El primer punto va en el lado contrario: lo que se mide es el viaje
	// entre los dos, no la distancia a un reposo que puede estar mal.
	desde := 1 - x

	return Step{
		ID:       stepID(seed, index),
		Kind:     KindGaze,
		Gaze:     GazeTarget{X: x, Y: 0.5},
		GazeFrom: GazeTarget{X: desde, Y: 0.5},
		// Hold es cuánto se queda el punto en el PRIMER sitio antes de saltar.
		// Tiene que dar tiempo a llegar y asentarse: por debajo, la fase de
		// salida se mide con la mirada aún en camino.
		Hold: p.GazeDwell,
		Window: Window{
			Min: p.MinReaction,
			Max: p.GazeDeadline,
		},
	}
}

// flashStep construye un destello. `previousLast` es el ÚLTIMO color del
// destello anterior, o FlashUnspecified si es el primero.
func flashStep(seed Seed, index int, p Policy, rng *prng, previousLast FlashColor) Step {
	count := p.MinFlashColors + rng.intn(p.MaxFlashColors-p.MinFlashColors+1)

	minMS := int(p.MinFlashSegment / time.Millisecond)
	maxMS := int(p.MaxFlashSegment / time.Millisecond)

	segments := make([]FlashSegment, 0, count)
	var total time.Duration
	for i := range count {
		// Todos los tramos son primarios saturados: sin blanco. La línea base
		// la da la fase de calibración con pantalla neutra, y un blanco aquí
		// sólo serviría para adaptar la cámara antes de los colores que
		// discriminan. Ver chromaticPalette.
		previous := previousLast
		if i > 0 {
			previous = segments[i-1].Color
		}
		color := pickColor(rng, previous)
		d := pickDuration(rng, minMS, maxMS, segments, p.FlashDurationSpread)
		segments = append(segments, FlashSegment{Color: color, Duration: d})
		total += d
	}

	return Step{
		ID:    stepID(seed, index),
		Kind:  KindFlash,
		Flash: segments,
		Hold:  total,
		Window: Window{
			// La secuencia tiene que haber sonado entera: nadie puede
			// responder a un color que todavía no se ha pintado.
			Min: total,
			Max: total + p.FlashSlack,
		},
	}
}

// pickDuration sortea la duración de un tramo DENTRO del conjunto permitido.
//
// Dos tramos de la misma duración hacen la secuencia simétrica en el tiempo, y
// entonces un desplazamiento de medio periodo la convierte en su contraria: la
// búsqueda de retardo del analizador encaja una secuencia sobre la inversa y
// la correlación deja de significar «respondió a ESTOS colores». Medido, con
// duraciones parecidas la secuencia invertida llegaba a 0,98 sobre los mismos
// frames en que la real daba 0,965.
//
// Se sortea del conjunto válido en vez de sortear y reintentar: con la
// duración anterior en mitad del rango, ningún número de reintentos encuentra
// una separada, y el que reintenta acaba aceptando una inválida sin decirlo.
// Aquí el sorteo no puede fallar porque la política garantiza que el conjunto
// no está vacío.
//
// No hace el guion más predecible (§6): que las duraciones difieran es una
// propiedad del diseño, y de ella no se deduce ninguno de los dos valores.
func pickDuration(rng *prng, minMS, maxMS int, previas []FlashSegment, spread time.Duration) time.Duration {
	spreadMS := int(spread / time.Millisecond)
	// Tramos permitidos: el rango entero menos una banda alrededor de cada
	// duración ya elegida.
	tramos := [][2]int{{minMS, maxMS}}
	for _, s := range previas {
		d := int(s.Duration / time.Millisecond)
		var siguiente [][2]int
		for _, t := range tramos {
			if lo, hi := t[0], min(t[1], d-spreadMS); lo <= hi {
				siguiente = append(siguiente, [2]int{lo, hi})
			}
			if lo, hi := max(t[0], d+spreadMS), t[1]; lo <= hi {
				siguiente = append(siguiente, [2]int{lo, hi})
			}
		}
		tramos = siguiente
	}
	if len(tramos) == 0 {
		// No debería ocurrir: lo impide Policy.Validate. Si ocurre, una
		// duración del rango es mejor que un pánico.
		return time.Duration(rng.durationMS(minMS, maxMS)) * time.Millisecond
	}

	// Sorteo uniforme sobre la UNIÓN, no sobre un tramo elegido al azar:
	// elegir tramo primero y valor después sesgaría hacia el tramo corto.
	ancho := 0
	for _, t := range tramos {
		ancho += t[1] - t[0] + 1
	}
	pos := rng.intn(ancho)
	for _, t := range tramos {
		n := t[1] - t[0] + 1
		if pos < n {
			return time.Duration(t[0]+pos) * time.Millisecond
		}
		pos -= n
	}
	return time.Duration(tramos[0][0]) * time.Millisecond
}

// pickColor elige un color distinto del anterior: dos colores iguales
// seguidos no producen transición cromática y no miden nada.
func pickColor(rng *prng, previous FlashColor) FlashColor {
	candidates := make([]FlashColor, 0, len(chromaticPalette))
	for _, c := range chromaticPalette {
		if c != previous {
			candidates = append(candidates, c)
		}
	}
	return candidates[rng.intn(len(candidates))]
}

// stepID deriva un identificador opaco de la semilla y la posición.
//
// Es determinista (mismo guion para la misma semilla) pero no ordenable: dos
// identificadores no dicen cuál va antes, y de uno no se deduce la semilla.
func stepID(seed Seed, index int) string {
	var buf [24]byte
	copy(buf[0:8], "livestep")
	binary.BigEndian.PutUint64(buf[8:16], uint64(seed))
	binary.BigEndian.PutUint64(buf[16:24], uint64(index))
	sum := sha256.Sum256(buf[:])
	return hex.EncodeToString(sum[:8])
}

// String implementa fmt.Stringer sin revelar el contenido del guion.
func (s *Script) String() string {
	return fmt.Sprintf("Script{posición:%d}", s.cursor)
}

// separateGazeFromPose reordena para que ninguna mirada quede justo después de
// una pose.
//
// La pose deja la cabeza girada, y la referencia de mirada se toma de los
// frames ANTERIORES a que aparezca el objetivo: con la cabeza a medio volver,
// esa referencia describe una postura que ya no existe. Medido con una cámara
// real: cabeza todavía a 20 grados del reto anterior, ojos contrarrotando para
// seguir mirando al mismo sitio, y el reto irresoluble hiciera lo que hiciera
// el sujeto.
//
// Se arregla intercambiando la mirada con la pose que la precede, no
// recolocando el guion entero: así el orden sigue siendo el que salió del
// sorteo salvo en lo imprescindible. Agrupar todas las miradas al principio
// habría sido más simple y habría hecho el guion predecible, que es
// exactamente lo que §6 prohíbe.
//
// Termina siempre: cada intercambio mueve una mirada una posición a la
// izquierda, y no puede haber más intercambios que posiciones.
// groupFlashesLast junta los destellos en un bloque al final del guion.
//
// El motivo es el pulso sanguíneo. Medirlo necesita un tramo largo, seguido y
// con la iluminación estable: la modulación que deja un latido en el color de
// la piel es del orden del 0,25 %, y un destello mueve ese color órdenes de
// magnitud más. No lo ensucia, lo tapa. Con los destellos repartidos, la
// sesión queda troceada en cachos de cinco segundos y el pulso —la ÚNICA
// señal que una máscara no puede fingir— no se puede medir nunca.
//
// **Esto sacrifica algo de imprevisibilidad del guion a propósito**, y va
// contra el espíritu de §6, así que conviene ser explícito sobre qué se cede:
//
//   - Se cede: un atacante sabe que los destellos llegan al final.
//   - NO se cede: cuántos son, de qué colores, con qué duraciones, ni cuándo
//     empiezan. Tampoco cuántos pasos hay antes, ni de qué tipo, ni con qué
//     parámetros.
//
// Lo que rompe un vídeo grabado no es ignorar el ORDEN de los tipos de reto:
// es no poder responder a una secuencia de colores emitida ahora, ni mirar a
// un punto que acaba de aparecer. Eso sigue intacto.
//
// Es la misma decisión que §4 tomó al revés para las miradas —allí se prefirió
// intercambiar dos pasos antes que agruparlas al principio—, y la diferencia
// es qué se gana: allí, evitar una referencia mala; aquí, la única
// contramedida que hay contra máscaras de silicona, que hoy se cuelan en el
// 55-73 % de los casos.
func groupFlashesLast(kinds []Kind) []Kind {
	out := make([]Kind, 0, len(kinds))
	flashes := 0
	for _, k := range kinds {
		if k == KindFlash {
			flashes++
			continue
		}
		out = append(out, k)
	}
	for range flashes {
		out = append(out, KindFlash)
	}
	return out
}

func separateGazeFromPose(kinds []Kind) []Kind {
	for range len(kinds) {
		swapped := false
		for i := 1; i < len(kinds); i++ {
			if kinds[i] == KindGaze && kinds[i-1] == KindPose {
				kinds[i], kinds[i-1] = kinds[i-1], kinds[i]
				swapped = true
			}
		}
		if !swapped {
			break
		}
	}
	return kinds
}
