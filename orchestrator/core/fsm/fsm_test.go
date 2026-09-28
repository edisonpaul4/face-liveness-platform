package fsm

import (
	"errors"
	"testing"
)

var allStates = []State{
	StateCreated,
	StateCalibrating,
	StateChallengeActive,
	StateEvaluating,
	StateResolved,
	StateExpired,
}

var allEvents = []Event{
	EventStart,
	EventCalibrated,
	EventStepAdvanced,
	EventStepRejected,
	EventChallengesDone,
	EventResolved,
	EventExpired,
	EventAborted,
}

// expected es la tabla de transiciones válidas, escrita a mano e
// independiente de la del código: si alguien toca transitions sin querer,
// este test lo caza.
var expected = map[State]map[Event]State{
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

// TestTransitionMatrix recorre las 42 combinaciones (6 estados × 7 eventos).
func TestTransitionMatrix(t *testing.T) {
	for _, from := range allStates {
		for _, ev := range allEvents {
			want, valid := expected[from][ev]

			m := &Machine{state: from}
			if got := m.Can(ev); got != valid {
				t.Errorf("%s + %s: Can = %v, se esperaba %v", from, ev, got, valid)
			}

			got, err := m.Apply(ev)

			if valid {
				if err != nil {
					t.Errorf("%s + %s: error inesperado: %v", from, ev, err)
				}
				if got != want {
					t.Errorf("%s + %s: estado %s, se esperaba %s", from, ev, got, want)
				}
				if m.State() != want {
					t.Errorf("%s + %s: State() = %s, se esperaba %s", from, ev, m.State(), want)
				}
				continue
			}

			if err == nil {
				t.Errorf("%s + %s: se esperaba error de transición inválida", from, ev)
				continue
			}
			if !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("%s + %s: errors.Is(ErrInvalidTransition) = false: %v", from, ev, err)
			}
			var te *TransitionError
			if !errors.As(err, &te) {
				t.Errorf("%s + %s: se esperaba *TransitionError, se obtuvo %T", from, ev, err)
			} else if te.From != from || te.Event != ev {
				t.Errorf("%s + %s: TransitionError{%s,%s} no coincide", from, ev, te.From, te.Event)
			}
			// Una transición inválida no toca el estado.
			if m.State() != from {
				t.Errorf("%s + %s: el estado cambió a %s tras un error", from, ev, m.State())
			}
			if got != from {
				t.Errorf("%s + %s: Apply devolvió %s, se esperaba el estado intacto %s", from, ev, got, from)
			}
		}
	}
}

// TestTerminalStatesRejectEverything: una sesión resuelta o expirada es
// definitiva; ni siquiera se la puede volver a expirar.
func TestTerminalStatesRejectEverything(t *testing.T) {
	for _, st := range []State{StateResolved, StateExpired} {
		if !st.Terminal() {
			t.Errorf("%s debería ser terminal", st)
		}
		for _, ev := range allEvents {
			m := &Machine{state: st}
			if _, err := m.Apply(ev); !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("%s + %s: se esperaba transición inválida, err=%v", st, ev, err)
			}
		}
	}
	for _, st := range []State{StateCreated, StateCalibrating, StateChallengeActive, StateEvaluating} {
		if st.Terminal() {
			t.Errorf("%s no debería ser terminal", st)
		}
	}
}

// TestExpireFromEveryLiveState: expirada es alcanzable desde todo estado vivo.
func TestExpireFromEveryLiveState(t *testing.T) {
	for _, st := range []State{StateCreated, StateCalibrating, StateChallengeActive, StateEvaluating} {
		m := &Machine{state: st}
		got, err := m.Apply(EventExpired)
		if err != nil {
			t.Fatalf("expirar desde %s: %v", st, err)
		}
		if got != StateExpired {
			t.Errorf("expirar desde %s dio %s", st, got)
		}
	}
}

func TestNewStartsInCreated(t *testing.T) {
	if got := New().State(); got != StateCreated {
		t.Errorf("New().State() = %s, se esperaba %s", got, StateCreated)
	}
}

func TestHappyPath(t *testing.T) {
	m := New()
	seq := []struct {
		event Event
		want  State
	}{
		{EventStart, StateCalibrating},
		{EventCalibrated, StateChallengeActive},
		{EventStepAdvanced, StateChallengeActive},
		{EventStepAdvanced, StateChallengeActive},
		{EventChallengesDone, StateEvaluating},
		{EventResolved, StateResolved},
	}
	for i, s := range seq {
		got, err := m.Apply(s.event)
		if err != nil {
			t.Fatalf("paso %d (%s): %v", i, s.event, err)
		}
		if got != s.want {
			t.Fatalf("paso %d (%s): %s, se esperaba %s", i, s.event, got, s.want)
		}
	}
}

func TestStateString(t *testing.T) {
	cases := map[State]string{
		StateCreated:         "creada",
		StateCalibrating:     "calibrando",
		StateChallengeActive: "reto_activo",
		StateEvaluating:      "evaluando",
		StateResolved:        "resuelta",
		StateExpired:         "expirada",
		State(200):           "estado_desconocido(200)",
	}
	for st, want := range cases {
		if got := st.String(); got != want {
			t.Errorf("State(%d).String() = %q, se esperaba %q", uint8(st), got, want)
		}
	}
}

func TestEventString(t *testing.T) {
	cases := map[Event]string{
		EventStart:          "start",
		EventCalibrated:     "calibrada",
		EventStepAdvanced:   "paso_aceptado",
		EventStepRejected:   "paso_rechazado",
		EventChallengesDone: "retos_agotados",
		EventResolved:       "resuelta",
		EventExpired:        "expirada",
		EventAborted:        "abortada",
		Event(200):          "evento_desconocido(200)",
	}
	for ev, want := range cases {
		if got := ev.String(); got != want {
			t.Errorf("Event(%d).String() = %q, se esperaba %q", uint8(ev), got, want)
		}
	}
}

func TestTransitionErrorMessage(t *testing.T) {
	err := &TransitionError{From: StateResolved, Event: EventStart}
	want := "fsm: transición inválida: resuelta no admite start"
	if err.Error() != want {
		t.Errorf("Error() = %q, se esperaba %q", err.Error(), want)
	}
}
