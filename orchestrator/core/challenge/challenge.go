// Package challenge genera y administra el guion de retos.
//
// INVARIANTE CENTRAL (CLAUDE.md §6): el cliente NUNCA conoce el guion
// completo.
//
//   - El guion se genera aquí a partir de una semilla y se guarda del lado
//     servidor. La semilla nunca sale del servidor.
//   - Script es de AVANCE ÚNICO: expone el paso actual y el paso siguiente
//     sólo tras avanzar. No hay acceso aleatorio, ni longitud, ni iteración.
//     La propiedad de seguridad es estructural, no una convención.
//   - El identificador de paso es opaco (SHA-256 de semilla+posición): no
//     permite inferir el orden.
//
// El guion impredecible y ligado al tiempo es la contramedida principal
// contra el ataque A3 (video replay en pantalla), ver CLAUDE.md §5.
//
// Este paquete es lógica pura: sin E/S, sin reloj, sin estado global.
package challenge

import (
	"fmt"
	"strings"
	"time"
)

// Seed es la semilla del guion.
//
// En producción se genera con crypto/rand en el borde y se guarda en Redis:
// nunca viaja al cliente ni se registra en logs. En /bench se fija a mano
// para reproducir un caso exacto.
type Seed uint64

// Kind es el tipo de paso del guion.
type Kind uint8

// Tipos de paso.
const (
	KindUnspecified Kind = iota
	// KindCalibration es la pantalla neutra inicial: fija la línea base de
	// color y exposición contra la que se miden los destellos.
	KindCalibration
	// KindPose pide un movimiento de cabeza o de distancia.
	KindPose
	// KindFlash pinta la pantalla con una secuencia de colores y mide la
	// respuesta cromática del rostro.
	KindFlash
	// KindGaze pinta un objetivo en un punto de la pantalla y mide hacia
	// dónde se desplaza el iris.
	//
	// Es de la misma familia que el destello, y por el mismo motivo: un vídeo
	// grabado puede llevar ojos que se mueven, pero no puede mirar al punto
	// que ha aparecido ahora en una esquina elegida al azar (§5, A3).
	KindGaze
	// KindHold pide al sujeto que se quede quieto mirando a la cámara.
	//
	// **No es un reto: es una ventana de medida.** No hay nada que responder,
	// así que no aporta defensa por sí mismo y un vídeo grabado lo supera sin
	// esfuerzo. Existe por una sola razón: el pulso sanguíneo necesita diez
	// segundos seguidos de iluminación estable, y una sesión de retos no los
	// da. Medido sobre una sesión real, el sujeto respondía a cada reto en
	// poco más de un segundo y la sesión entera duraba 8,3 s.
	//
	// Que no defienda nada no lo hace prescindible: es lo que hace medible la
	// ÚNICA señal que una máscara no puede fingir (§5, A4).
	KindHold
)

// String implementa fmt.Stringer.
func (k Kind) String() string {
	switch k {
	case KindCalibration:
		return "calibration"
	case KindPose:
		return "pose"
	case KindFlash:
		return "flash"
	case KindGaze:
		return "gaze"
	case KindHold:
		return "hold"
	case KindUnspecified:
		return "unspecified"
	default:
		return fmt.Sprintf("kind(%d)", uint8(k))
	}
}

// ParseKinds traduce nombres de tipo de reto. Sólo lo usa la configuración de
// desarrollo que excluye tipos del guion; un nombre desconocido es un error y
// no un silencio, porque una errata que se traga el filtro dejaría corriendo
// un guion distinto del que se pidió.
func ParseKinds(names []string) ([]Kind, error) {
	out := make([]Kind, 0, len(names))
	for _, name := range names {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "calibration":
			return nil, fmt.Errorf("challenge: la calibración no se puede omitir")
		case "pose":
			out = append(out, KindPose)
		case "flash":
			out = append(out, KindFlash)
		case "gaze":
			out = append(out, KindGaze)
		default:
			return nil, fmt.Errorf("challenge: tipo de reto desconocido %q", name)
		}
	}
	return out, nil
}

// PoseAction es el movimiento pedido en un paso de tipo pose.
type PoseAction uint8

// Acciones de pose.
const (
	PoseUnspecified PoseAction = iota
	PoseYawLeft
	PoseYawRight
	PosePitchUp
	PoseMoveCloser
)

// poseCatalog son las poses que se SORTEAN. Se muestrea sin reemplazo: un
// guion nunca repite pose.
//
// PoseMoveCloser no está: no se sortea, se coloca siempre justo antes del
// bloque de destellos (ver approachBeforeFlashes). Y no está por dos razones
// que apuntan al mismo sitio. Acercarse no es un ángulo, así que no produce
// ventana de pose ni paralaje: sorteado, gastaba un reto sin aportar ninguna
// señal, y era además el que podía dejar un guion entero sin paralaje. Y donde
// sí sirve —delante del destello, por 1/d²— hace falta SIEMPRE, no una vez de
// cada cuatro.
var poseCatalog = []PoseAction{PoseYawLeft, PoseYawRight, PosePitchUp}

// String implementa fmt.Stringer.
func (a PoseAction) String() string {
	switch a {
	case PoseYawLeft:
		return "yaw_left"
	case PoseYawRight:
		return "yaw_right"
	case PosePitchUp:
		return "pitch_up"
	case PoseMoveCloser:
		return "move_closer"
	case PoseUnspecified:
		return "unspecified"
	default:
		return fmt.Sprintf("pose(%d)", uint8(a))
	}
}

// FlashColor es un color de la paleta de destello.
type FlashColor uint8

// Paleta: blanco de referencia + primarios saturados.
const (
	FlashUnspecified FlashColor = iota
	FlashWhite
	FlashRed
	FlashGreen
	FlashBlue
)

// flashPalette es la paleta completa que el cliente sabe pintar. El blanco
// sigue existiendo como ESTADO detectable —si el rostro devuelve los tres
// canales encendidos cuando se pidió un primario, eso es información— pero ya
// no se emite en ninguna secuencia.
var flashPalette = []FlashColor{FlashWhite, FlashRed, FlashGreen, FlashBlue}

var chromaticPalette = []FlashColor{FlashRed, FlashGreen, FlashBlue}

// String implementa fmt.Stringer.
func (c FlashColor) String() string {
	switch c {
	case FlashWhite:
		return "white"
	case FlashRed:
		return "red"
	case FlashGreen:
		return "green"
	case FlashBlue:
		return "blue"
	case FlashUnspecified:
		return "unspecified"
	default:
		return fmt.Sprintf("color(%d)", uint8(c))
	}
}

// FlashSegment es un color mantenido durante una duración.
type FlashSegment struct {
	Color    FlashColor
	Duration time.Duration
}

// Window es la ventana de reacción admisible de un paso, medida desde el
// instante en que el paso se revela al cliente.
//
// Min NO SE REVELA NUNCA al cliente: decirle a un atacante a partir de qué
// milisegundo una respuesta deja de ser sospechosa le regala el ataque.
type Window struct {
	// Min es la reacción más rápida plausible. Por debajo, la respuesta no
	// la ha producido una persona.
	Min time.Duration
	// Max es el plazo. Por encima, timeout.
	Max time.Duration
}

// Contains indica si elapsed cae dentro de la ventana, extremos incluidos.
func (w Window) Contains(elapsed time.Duration) bool {
	return elapsed >= w.Min && elapsed <= w.Max
}

// GazeTarget es el punto de la pantalla al que se pide mirar.
//
// X e Y son fracciones del viewport del cliente: (0,0) arriba a la izquierda,
// (1,1) abajo a la derecha.
//
// Los objetivos van a IZQUIERDA y DERECHA, a media altura, y sólo ahí. No es
// una simplificación por pereza: es que el eje vertical no se puede medir con
// una webcam frontal, y se intentó de tres formas.
//
// El horizontal se mide por el desplazamiento del iris DENTRO de la órbita,
// contra la línea que une las comisuras. Como esa línea gira con la cabeza,
// acompañar el gesto con el cuello queda compensado por construcción. Medido
// con una cámara real: 0,043 de señal contra 0,004 de ruido, diez a uno.
//
// El vertical no tiene equivalente. La posición del iris respecto a las
// comisuras apenas cambia, porque el párpado sigue al ojo; y la apertura del
// párpado, que sí cambia, es un proxy contaminado — al girar mínimamente la
// cabeza, la distancia entre comisuras se escorza, el ancho del ojo baja y la
// apertura sube sola. Medido: 38 % de subida en una mirada puramente lateral.
type GazeTarget struct {
	X float64
	Y float64
}

// Side devuelve hacia qué lado de la PANTALLA está el objetivo: -1 izquierda,
// +1 derecha.
func (g GazeTarget) Side() int {
	if g.X < 0.5 {
		return -1
	}
	return 1
}

// Step es un paso del guion, ya materializado.
//
// No lleva índice ni referencia al guion: la posición la conoce sólo el
// Script, y el Script no la revela.
type Step struct {
	// ID es opaco y no ordenable.
	ID string
	// Kind determina qué campos son significativos.
	Kind Kind
	// Pose sólo aplica si Kind == KindPose.
	Pose PoseAction
	// Flash sólo aplica si Kind == KindFlash.
	Flash []FlashSegment
	// Gaze sólo aplica si Kind == KindGaze. Es el objetivo FINAL, al que hay
	// que llegar; GazeFrom es de donde se sale.
	Gaze GazeTarget
	// GazeFrom es el primer punto de un reto de mirada, en el lado contrario
	// a Gaze.
	//
	// El reto se mide entre los dos puntos y NO contra una referencia de
	// reposo, y ése es todo el motivo de que existan dos. Medir contra el
	// reposo tenía dos defectos que costaron sesiones reales:
	//
	//  · el recorrido disponible es sólo del centro al borde, que en un móvil
	//    son unos 5,7°; entre los dos puntos es el doble;
	//  · y la referencia venía de fuera del paso, así que podía estar
	//    desfasada o desplazada. Eso produjo un rechazo por fraude contra un
	//    usuario legítimo que simplemente miraba a un lado durante la
	//    calibración.
	//
	// Además cuesta más de falsificar: un vídeo grabado tiene que producir una
	// transición concreta en un instante concreto, no basta con estar mirando
	// a un lado.
	GazeFrom GazeTarget
	// Hold es la duración impuesta del paso (calibración y destello); 0 en
	// pose, donde manda la reacción del usuario.
	Hold time.Duration
	// Window es la ventana de reacción admisible.
	Window Window
}

// FlashDuration es la duración total de la secuencia de destello.
func (s Step) FlashDuration() time.Duration {
	var total time.Duration
	for _, seg := range s.Flash {
		total += seg.Duration
	}
	return total
}
