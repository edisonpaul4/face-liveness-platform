package challenge

import (
	"errors"
	"fmt"
	"time"
)

// ErrInvalidPolicy marca una política incoherente. Se comprueba con errors.Is.
var ErrInvalidPolicy = errors.New("challenge: política inválida")

// Policy son los parámetros de generación del guion.
//
// Son CONFIGURACIÓN, no constantes de código (CLAUDE.md §9). Los valores por
// defecto de DefaultPolicy son un punto de partida sin calibrar: los números
// definitivos salen del banco de pruebas.
type Policy struct {
	// CalibrationHold es la pantalla neutra inicial.
	CalibrationHold time.Duration
	// CalibrationSlack es la tolerancia por encima de CalibrationHold antes
	// de declarar timeout.
	CalibrationSlack time.Duration

	// MinSteps y MaxSteps acotan el número de retos, calibración aparte.
	MinSteps int
	MaxSteps int

	// MinFlashColors y MaxFlashColors acotan la longitud de la secuencia de
	// destello.
	MinFlashColors int
	MaxFlashColors int

	// MinFlashSegment y MaxFlashSegment acotan la duración de cada color.
	//
	// **Los tramos son CORTOS a propósito, y alargarlos mata la señal.**
	//
	// Se probó lo contrario —800-1400 ms, para que el rango de búsqueda del
	// analizador cupiese dentro de un tramo— y salió al revés: sobre las
	// mismas caras, los tramos de 350-600 ms correlacionaban entre 0,92 y 0,99
	// en doce ventanas, y los de 883-1265 ms dieron **0,049 y 0,014**.
	//
	// La razón es física y se midió mirando dentro de un tramo: la respuesta
	// del rostro SUBE hasta un máximo hacia los 680 ms y luego decae, y en el
	// tramo siguiente el canal del color nuevo llega a irse un 12 % al lado
	// CONTRARIO. La cámara ha tenido tiempo de compensar el color anterior con
	// su balance de blancos, y al cambiar se desanda. Un tramo largo no mide
	// la piel: mide a la cámara acomodándose. Es la misma razón por la que las
	// secuencias son de dos colores y no de cinco.
	//
	// Contra el aliasing de la búsqueda de retardo defiende
	// FlashDurationSpread, que además está medido: ver ahí.
	MinFlashSegment time.Duration
	MaxFlashSegment time.Duration
	// FlashDurationSpread es lo que como mínimo tienen que diferir entre sí
	// las duraciones de dos tramos de la MISMA secuencia.
	//
	// Con dos tramos de la misma duración la secuencia es simétrica en el
	// tiempo, y entonces la búsqueda de retardo puede desplazarla medio
	// periodo y encajarla sobre su CONTRARIA: deja de medir «respondió a
	// ESTOS colores» y pasa a medir «hubo un cambio de brillo con esta forma».
	//
	// El número está medido, no elegido. Sobre 27 ventanas reales, comparando
	// la secuencia emitida contra su inversa:
	//
	//     |d1-d2| = 27 ms   real 0,974   inversa 0,852   <- se cuela
	//     |d1-d2| = 29 ms   real 0,919   inversa 0,718   <- se cuela
	//     |d1-d2| = 34 ms   real 0,754   inversa 0,143
	//     |d1-d2| >= 36 ms  real 0,94+   inversa entre -0,38 y 0,35
	//
	// 100 ms deja un factor tres sobre el punto donde deja de colarse.
	FlashDurationSpread time.Duration
	// FlashSlack es la tolerancia por encima de la duración de la secuencia.
	FlashSlack time.Duration

	// MinReaction es la reacción humana más rápida plausible. Por debajo, la
	// respuesta es tan sospechosa como un timeout.
	MinReaction time.Duration
	// PoseDeadline es el plazo para completar un reto de pose.
	PoseDeadline time.Duration

	// PulseHold es cuánto se le pide al sujeto que se esté quieto para poder
	// medirle el pulso. No es un plazo: es una duración impuesta, como la
	// calibración.
	PulseHold time.Duration

	// GazeDeadline es el plazo para mirar al objetivo. Más corto que el de
	// pose: mover los ojos es inmediato, mover el cuello no.
	GazeDeadline time.Duration
	// GazeMargin es cuánto se aleja el objetivo del borde de la pantalla,
	// como fracción. Pegarlo al borde exacto lo deja fuera del campo cómodo
	// de mucha gente y de casi cualquier pantalla ultraancha; alejarlo mucho
	// del borde cuesta señal, que es lo que este reto no puede permitirse.
	GazeMargin float64
	// GazeDwell es cuánto se queda el punto en el PRIMER sitio antes de saltar
	// al segundo. El reto se mide entre las dos fases, así que la primera
	// tiene que dar tiempo a llegar y asentarse: si es demasiado corta, se
	// mide la mirada aún en camino y el viaje sale recortado.
	GazeDwell time.Duration

	// InsecureSkipKinds quita tipos de reto del guion.
	//
	// SÓLO PARA DESARROLLO, y el nombre lo dice a propósito. Quitar el
	// destello o la mirada deja al sistema sin lo único que un vídeo grabado
	// no puede responder (§5, A2 y A3): un guion así se pasa con una foto en
	// un móvil, sólo que tardando más.
	//
	// Existe porque hace falta poder depurar el resto del recorrido —cámara,
	// pose, transporte— mientras un análisis concreto está a medio calibrar,
	// y porque el maniquí sintético del banco no sabe hacerlo todo.
	InsecureSkipKinds []Kind
}

// IsZero indica si la política está sin rellenar, para que el llamante pueda
// caer en la de por defecto. No se compara con ==: la política lleva un slice
// y Go no compara structs con slices dentro.
func (p Policy) IsZero() bool {
	return p.MinSteps == 0 && p.MaxSteps == 0 && p.CalibrationHold == 0
}

// Skips indica si un tipo de reto está excluido del guion.
func (p Policy) Skips(k Kind) bool {
	for _, skipped := range p.InsecureSkipKinds {
		if skipped == k {
			return true
		}
	}
	return false
}

// available es cuántos retos distintos puede ofrecer la política.
func (p Policy) available() int {
	total := 0
	if !p.Skips(KindPose) {
		total += len(poseCatalog)
	}
	if !p.Skips(KindFlash) {
		total += maxFlashSteps
	}
	if !p.Skips(KindGaze) {
		total += maxGazeSteps
	}
	return total
}

// DefaultPolicy devuelve la política por defecto.
func DefaultPolicy() Policy {
	return Policy{
		CalibrationHold:  1500 * time.Millisecond,
		CalibrationSlack: 1500 * time.Millisecond,

		// Cinco retos como mínimo, calibración aparte. Un guion corto es
		// barato de superar por casualidad: cada paso añade una ventana
		// temporal más que un vídeo grabado tiene que acertar.
		//
		// El techo lo pone el presupuesto de sesión (45 s): en el peor caso
		// —seis pasos, con los plazos máximos de cada tipo— salen unos 32 s
		// contando la calibración. Subirlo más deja la sesión sin margen.
		MinSteps: 5,
		MaxSteps: 6,

		// DOS colores, no de tres a cinco.
		//
		// No es que sobren tramos: es que cada tramo extra ESTROPEA la
		// medida. El destello es lo que más adapta la exposición y el balance
		// de blancos de la cámara, así que a partir del segundo tramo lo que
		// se mide es la cámara acomodándose, no la piel respondiendo.
		//
		// Medido sobre diez ventanas de destello de grabaciones reales, la
		// correlación por número de tramos analizados. En las DIEZ, dos
		// tramos correlacionan mejor que cualquier prefijo más largo:
		//
		//     sesión      2 tramos   3      4      5
		//     22-53         0,815   0,396  0,243  0,198
		//     09-59         0,792   0,787  0,511  0,362
		//     09-59b        0,703   0,689    —      —
		//     23-02         0,545   0,231    —      —
		//     13-48         0,444   0,325  0,175    —
		//
		// Con el umbral de 0,35: a dos tramos pasan cinco de seis sesiones;
		// a longitud completa, una. Una sesión legítima acabó en reintentar
		// por esto, y encima con el motivo equivocado —decía que sobraba luz
		// ambiente cuando lo que sobraba eran tramos.
		//
		// La variación no se pierde: sigue habiendo qué dos colores, en qué
		// orden, con qué duraciones, y uno o dos destellos por guion.
		//
		// Recuperar secuencias largas exige antes arreglar la deriva de la
		// cámara —medir cada tramo contra el ANTERIOR en vez de contra la
		// calibración—, que es la deuda anotada en §4.
		MinFlashColors: 2,
		MaxFlashColors: 2,

		MinFlashSegment:     350 * time.Millisecond,
		MaxFlashSegment:     600 * time.Millisecond,
		FlashDurationSpread: 100 * time.Millisecond,
		FlashSlack:          1500 * time.Millisecond,

		MinReaction:  350 * time.Millisecond,
		PoseDeadline: 5 * time.Second,

		// El reto de mirada es más largo que antes porque son dos puntos, pero
		// no tanto como para comerse el presupuesto de sesión: con 900 ms de
		// permanencia quedan 550 ms de muestras útiles en la fase de salida
		// —ocho a 15 fps— después de descartar los primeros 350.
		GazeDeadline: 3300 * time.Millisecond,
		GazeDwell:    900 * time.Millisecond,
		GazeMargin:   0.06,

		// Once segundos de quietud para medir el pulso.
		//
		// Diez es el suelo aritmético —la resolución de una FFT es 1/T, y con
		// menos el pulso de una persona y el de la siguiente caen en el mismo
		// bin—, y el segundo extra cubre el arranque: los primeros frames
		// llegan con el sujeto todavía acomodándose.
		//
		// Cuesta lo que cuesta: una sesión real duraba 8,3 s y pasa a rondar
		// los 19. Es el precio de que la señal contra máscaras exista.
		PulseHold: 11 * time.Second,
	}
}

// Validate comprueba la coherencia de la política. Devuelve un error
// explícito; nunca entra en pánico.
func (p Policy) Validate() error {
	checks := []struct {
		bad bool
		msg string
	}{
		{p.CalibrationHold <= 0, "CalibrationHold debe ser > 0"},
		{p.CalibrationSlack < 0, "CalibrationSlack no puede ser negativo"},
		{p.MinSteps < 1, "MinSteps debe ser >= 1"},
		{p.MaxSteps < p.MinSteps, "MaxSteps debe ser >= MinSteps"},
		{p.MaxSteps > p.available(), "MaxSteps excede el catálogo disponible"},
		{p.MinFlashColors < 1, "MinFlashColors debe ser >= 1"},
		{p.MaxFlashColors < p.MinFlashColors, "MaxFlashColors debe ser >= MinFlashColors"},
		{p.MinFlashSegment <= 0, "MinFlashSegment debe ser > 0"},
		{p.GazeDeadline <= 0, "GazeDeadline debe ser > 0"},
		{p.GazeMargin < 0 || p.GazeMargin >= 0.5, "GazeMargin debe estar en [0, 0.5)"},
		{p.MaxFlashSegment < p.MinFlashSegment, "MaxFlashSegment debe ser >= MinFlashSegment"},
		{p.FlashDurationSpread < 0, "FlashDurationSpread no puede ser negativo"},
		// Hace falta el DOBLE de la separación: con una duración justo en
		// mitad del rango, lo que queda a cada lado es (rango − 2·separación).
		// Si eso es cero, no existe segunda duración válida y el sorteo no
		// tiene de dónde elegir.
		{p.MaxFlashSegment-p.MinFlashSegment < 2*p.FlashDurationSpread,
			"el rango de duraciones no da para separar dos tramos: hace falta al menos el doble de FlashDurationSpread"},
		{p.FlashSlack < 0, "FlashSlack no puede ser negativo"},
		{p.MinReaction < 0, "MinReaction no puede ser negativo"},
		{p.PoseDeadline <= p.MinReaction, "PoseDeadline debe ser > MinReaction"},
	}
	for _, c := range checks {
		if c.bad {
			return fmt.Errorf("%w: %s", ErrInvalidPolicy, c.msg)
		}
	}
	return nil
}

// maxFlashSteps acota cuántos retos de destello puede contener un guion.
// Las poses se muestrean sin reemplazo (4 disponibles); los destellos sí
// pueden repetirse, cada uno con su propia secuencia.
//
// Dos, no cuatro, y por dos motivos que apuntan al mismo sitio.
//
// El primero es el pulso. Los destellos van agrupados al final
// (`groupFlashesLast`), así que cuantos más haya, más corto es el tramo
// tranquilo que queda delante para medir el latido. Medido sobre 2000
// semillas, con el techo en cuatro el 12,3 % de las sesiones dejaban menos de
// los diez segundos que hacen falta; con el techo en dos, ninguna. La mediana
// sube de 14 a 17 segundos.
//
// El segundo es que cuatro destellos seguidos se estorban entre sí. El
// destello es lo que más adapta la exposición y el balance de blancos de la
// cámara, y esa adaptación se come los tramos siguientes (§4). Repartidos por
// el guion había tiempo de recuperarse entre uno y otro; en bloque, no.
//
// Sigue variando entre uno y dos, así que la longitud del bloque final no es
// predecible.
const maxFlashSteps = 2

// maxGazeSteps acota cuántos retos de mirada puede contener un guion. Menos
// que destellos: la mirada se mide peor y cansa más rápido que un destello.
const maxGazeSteps = 3
