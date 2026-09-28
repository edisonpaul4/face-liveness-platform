// Package fsm implementa la máquina de estados de una sesión de liveness.
//
//	creada ──start──▶ calibrando ──calibrada──▶ reto_activo(n) ──retos_agotados──▶ evaluando ──resuelta──▶ resuelta
//	                       │                        │  ▲                              │
//	                       │                        └──┘ paso_aceptado                │
//	                       └────────────paso_rechazado───────────────────────────────┘
//
//	cualquier estado vivo ──expirada──▶ expirada
//	cualquier estado vivo ──abortada──▶ resuelta (retry)
//
// Reglas:
//   - Toda transición es autoritativa del servidor. El cliente nunca provoca
//     una transición declarando haber cumplido un reto.
//   - Una transición inválida es un error explícito (ErrInvalidTransition),
//     nunca un panic, y deja el estado intacto.
//   - Los deadlines son del servidor; el reloj del cliente no es de fiar.
//
// Este paquete es lógica pura: no conoce el tiempo, ni el guion, ni la E/S.
// El índice del reto activo (la "n" de reto_activo) lo lleva la sesión, no
// el estado: el estado es un enum, no un contador.
package fsm

import (
	"errors"
	"fmt"
)

// State es el estado de una sesión.
type State uint8

// Estados de una sesión de liveness.
const (
	StateCreated State = iota
	StateCalibrating
	StateChallengeActive
	StateEvaluating
	StateResolved
	StateExpired
)

// String implementa fmt.Stringer.
func (s State) String() string {
	switch s {
	case StateCreated:
		return "creada"
	case StateCalibrating:
		return "calibrando"
	case StateChallengeActive:
		return "reto_activo"
	case StateEvaluating:
		return "evaluando"
	case StateResolved:
		return "resuelta"
	case StateExpired:
		return "expirada"
	default:
		return fmt.Sprintf("estado_desconocido(%d)", uint8(s))
	}
}

// Terminal indica si el estado es final: ya no admite transiciones.
func (s State) Terminal() bool {
	return s == StateResolved || s == StateExpired
}

// Event es lo que puede provocar una transición.
type Event uint8

// Eventos admitidos.
const (
	// EventStart arranca la sesión y entra en calibración.
	EventStart Event = iota
	// EventCalibrated cierra la calibración con éxito.
	EventCalibrated
	// EventStepAdvanced acepta el paso actual y activa el siguiente reto.
	EventStepAdvanced
	// EventStepRejected descarta el paso actual (fuera de ventana temporal).
	// Cierra la fase de retos: se falla en cerrado.
	EventStepRejected
	// EventChallengesDone indica que no quedan retos por revelar.
	EventChallengesDone
	// EventResolved aplica el veredicto final.
	EventResolved
	// EventExpired agota la sesión por presupuesto de tiempo.
	EventExpired
	// EventAborted cierra la sesión por una causa ajena al protocolo: se ha
	// caído una pieza de la infraestructura. No es culpa del usuario y no es
	// indicio de ataque, pero la sesión no puede continuar.
	EventAborted
)

// String implementa fmt.Stringer.
func (e Event) String() string {
	switch e {
	case EventStart:
		return "start"
	case EventCalibrated:
		return "calibrada"
	case EventStepAdvanced:
		return "paso_aceptado"
	case EventStepRejected:
		return "paso_rechazado"
	case EventChallengesDone:
		return "retos_agotados"
	case EventResolved:
		return "resuelta"
	case EventExpired:
		return "expirada"
	case EventAborted:
		return "abortada"
	default:
		return fmt.Sprintf("evento_desconocido(%d)", uint8(e))
	}
}

// ErrInvalidTransition marca cualquier transición no contemplada en la tabla.
// Se comprueba con errors.Is.
var ErrInvalidTransition = errors.New("fsm: transición inválida")

// TransitionError describe la transición rechazada. Envuelve
// ErrInvalidTransition para que errors.Is funcione.
type TransitionError struct {
	From  State
	Event Event
}

// Error implementa error.
func (e *TransitionError) Error() string {
	return fmt.Sprintf("fsm: transición inválida: %s no admite %s", e.From, e.Event)
}

// Unwrap permite errors.Is(err, ErrInvalidTransition).
func (e *TransitionError) Unwrap() error { return ErrInvalidTransition }

// transitions es la tabla completa. Lo que no está aquí es un error.
//
// Nótese que los estados terminales (resuelta, expirada) no admiten ningún
// evento, EventExpired incluido: expirar una sesión ya resuelta reescribiría
// un veredicto final.
var transitions = map[State]map[Event]State{
	StateCreated: {
		EventStart:   StateCalibrating,
		EventExpired: StateExpired,
		EventAborted: StateResolved,
	},
	StateCalibrating: {
		EventCalibrated:   StateChallengeActive,
		EventStepRejected: StateEvaluating,
		EventExpired:      StateExpired,
		EventAborted:      StateResolved,
	},
	StateChallengeActive: {
		EventStepAdvanced:   StateChallengeActive,
		EventStepRejected:   StateEvaluating,
		EventChallengesDone: StateEvaluating,
		EventExpired:        StateExpired,
		EventAborted:        StateResolved,
	},
	StateEvaluating: {
		EventResolved: StateResolved,
		EventExpired:  StateExpired,
		EventAborted:  StateResolved,
	},
	StateResolved: {},
	StateExpired:  {},
}

// Machine es la máquina de estados de una sesión. No es segura para uso
// concurrente: la sesión que la contiene es la dueña.
type Machine struct {
	state State
}

// New crea una máquina en el estado inicial (creada).
func New() *Machine { return &Machine{state: StateCreated} }

// State devuelve el estado actual.
func (m *Machine) State() State { return m.state }

// Can indica si el evento es admisible en el estado actual.
func (m *Machine) Can(e Event) bool {
	_, ok := transitions[m.state][e]
	return ok
}

// Apply intenta la transición. Si el evento no es admisible devuelve un
// *TransitionError y deja el estado intacto.
func (m *Machine) Apply(e Event) (State, error) {
	next, ok := transitions[m.state][e]
	if !ok {
		return m.state, &TransitionError{From: m.state, Event: e}
	}
	m.state = next
	return m.state, nil
}
