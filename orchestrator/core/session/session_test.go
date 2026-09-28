package session

import (
	"errors"
	"testing"
	"time"

	"github.com/edisonpaul4/biometrics/orchestrator/core/challenge"
	"github.com/edisonpaul4/biometrics/orchestrator/core/clock"
	"github.com/edisonpaul4/biometrics/orchestrator/core/fsm"
)

const testSeed = challenge.Seed(20260822)

var t0 = time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

func newTestSession(t *testing.T) (*Session, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(t0)
	s, err := New(Config{ID: "01J0TEST", Seed: testSeed, Clock: clk, Budget: 90 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, clk
}

// waitFor devuelve cuánto hay que esperar para responder a tiempo a un paso.
// Para calibración y destello el paso impone su duración; para pose vale
// cualquier reacción humana razonable.
func waitFor(step RevealedStep) time.Duration {
	if step.Hold > 0 {
		return step.Hold
	}
	return 800 * time.Millisecond
}

// playChallenges responde correctamente a todos los retos hasta agotarlos.
func playChallenges(t *testing.T, s *Session, clk *clock.Fake) {
	t.Helper()
	for i := 0; s.State() == fsm.StateChallengeActive; i++ {
		if i > 10 {
			t.Fatal("el guion no termina: bucle infinito")
		}
		step, err := s.Reveal()
		if err != nil {
			t.Fatalf("Reveal en el reto %d: %v", i, err)
		}
		clk.Advance(waitFor(step))
		v, err := s.SubmitAt(clk.Now())
		if err != nil {
			t.Fatalf("SubmitAt en el reto %d: %v", i, err)
		}
		if !v.Accepted {
			t.Fatalf("reto %d rechazado por %s (tardó %v)", i, v.Reason, v.Elapsed)
		}
	}
}

// startAndCalibrate lleva la sesión hasta el primer reto.
func startAndCalibrate(t *testing.T, s *Session, clk *clock.Fake) {
	t.Helper()
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	step, err := s.Reveal()
	if err != nil {
		t.Fatalf("Reveal de calibración: %v", err)
	}
	if step.Kind != challenge.KindCalibration {
		t.Fatalf("el primer paso es %s, se esperaba calibración", step.Kind)
	}
	clk.Advance(step.Hold)
	if _, err := s.SubmitAt(clk.Now()); err != nil {
		t.Fatalf("cierre de calibración: %v", err)
	}
	if s.State() != fsm.StateChallengeActive {
		t.Fatalf("tras calibrar el estado es %s", s.State())
	}
}

func TestHappyPath(t *testing.T) {
	s, clk := newTestSession(t)

	if s.State() != fsm.StateCreated {
		t.Fatalf("estado inicial %s", s.State())
	}
	if s.Outcome() != OutcomeUnspecified {
		t.Errorf("desenlace inicial %s", s.Outcome())
	}
	if s.RevealedCount() != 0 {
		t.Errorf("RevealedCount inicial = %d", s.RevealedCount())
	}
	if !s.CreatedAt().Equal(t0) || !s.Deadline().Equal(t0.Add(90*time.Second)) {
		t.Errorf("CreatedAt=%v Deadline=%v", s.CreatedAt(), s.Deadline())
	}

	startAndCalibrate(t, s, clk)
	playChallenges(t, s, clk)

	if s.State() != fsm.StateEvaluating {
		t.Fatalf("tras los retos el estado es %s", s.State())
	}
	if len(s.Reasons()) != 0 {
		t.Errorf("una sesión limpia no debería acumular motivos: %v", s.Reasons())
	}

	res, err := s.Resolve(Verdict{Outcome: OutcomePass})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Outcome != OutcomePass || res.Reason != ReasonPassed {
		t.Errorf("resolución %s/%s, se esperaba pass/passed", res.Outcome, res.Reason)
	}
	if s.State() != fsm.StateResolved || s.Outcome() != OutcomePass {
		t.Errorf("estado %s, desenlace %s", s.State(), s.Outcome())
	}
	if !res.ResolvedAt.Equal(clk.Now()) {
		t.Errorf("ResolvedAt = %v, se esperaba %v", res.ResolvedAt, clk.Now())
	}
	if s.RevealedCount() < 3 {
		t.Errorf("RevealedCount = %d, se esperaban al menos 3 pasos", s.RevealedCount())
	}

	got, ok := s.Resolution()
	if !ok || got.Outcome != OutcomePass {
		t.Errorf("Resolution() = %+v, %v", got, ok)
	}
}

// --- validación de configuración -------------------------------------------

func TestNewRejectsInvalidConfig(t *testing.T) {
	badPolicy := challenge.DefaultPolicy()
	badPolicy.MaxSteps = 0

	cases := []struct {
		name string
		cfg  Config
	}{
		{"ID vacío", Config{Seed: 1}},
		{"presupuesto negativo", Config{ID: "x", Budget: -time.Second}},
		{"política inválida", Config{ID: "x", Policy: badPolicy}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := New(c.cfg)
			if err == nil {
				t.Fatal("se esperaba un error")
			}
			if !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("errors.Is(ErrInvalidConfig) = false: %v", err)
			}
			if s != nil {
				t.Error("se devolvió una sesión junto al error")
			}
		})
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	s, err := New(Config{ID: "x", Seed: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := s.clk.(clock.System); !ok {
		t.Errorf("sin reloj inyectado se esperaba clock.System, hay %T", s.clk)
	}
	if got := s.Deadline().Sub(s.CreatedAt()); got != DefaultBudget {
		t.Errorf("presupuesto por defecto = %v, se esperaba %v", got, DefaultBudget)
	}
	if s.ID() != "x" {
		t.Errorf("ID() = %q", s.ID())
	}
}

// TestSameSeedSameSession: dos sesiones con la misma semilla revelan lo mismo.
func TestSameSeedSameSession(t *testing.T) {
	revealAll := func() []RevealedStep {
		clk := clock.NewFake(t0)
		s, err := New(Config{ID: "x", Seed: 4242, Clock: clk, Budget: 90 * time.Second})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := s.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		var out []RevealedStep
		for s.State() == fsm.StateCalibrating || s.State() == fsm.StateChallengeActive {
			step, err := s.Reveal()
			if err != nil {
				t.Fatalf("Reveal: %v", err)
			}
			out = append(out, step)
			clk.Advance(waitFor(step))
			if _, err := s.SubmitAt(clk.Now()); err != nil {
				t.Fatalf("SubmitAt: %v", err)
			}
		}
		return out
	}

	a, b := revealAll(), revealAll()
	if len(a) != len(b) {
		t.Fatalf("longitudes distintas: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Kind != b[i].Kind || a[i].Pose != b[i].Pose ||
			a[i].Hold != b[i].Hold || a[i].Deadline != b[i].Deadline ||
			len(a[i].Flash) != len(b[i].Flash) {
			t.Fatalf("paso %d difiere:\n%+v\n%+v", i, a[i], b[i])
		}
		for j := range a[i].Flash {
			if a[i].Flash[j] != b[i].Flash[j] {
				t.Fatalf("paso %d, color %d difiere", i, j)
			}
		}
	}
}

// --- revelación -------------------------------------------------------------

// TestRevealIsIdempotent: pedir el paso otra vez no lo cambia ni reinicia la
// ventana. Un cliente que se reconecta debe poder repetir la pregunta.
func TestRevealIsIdempotent(t *testing.T) {
	s, clk := newTestSession(t)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	first, err := s.Reveal()
	if err != nil {
		t.Fatalf("Reveal: %v", err)
	}
	clk.Advance(400 * time.Millisecond)
	second, err := s.Reveal()
	if err != nil {
		t.Fatalf("Reveal repetido: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("Reveal devolvió pasos distintos: %s vs %s", first.ID, second.ID)
	}
	if s.RevealedCount() != 1 {
		t.Errorf("RevealedCount = %d tras dos Reveal del mismo paso", s.RevealedCount())
	}

	// La ventana sigue corriendo desde la activación, no desde el último
	// Reveal: responder ahora (400 ms) sigue siendo demasiado rápido.
	clk.Advance(400 * time.Millisecond)
	v, err := s.SubmitAt(clk.Now())
	if err != nil {
		t.Fatalf("SubmitAt: %v", err)
	}
	if v.Accepted || v.Reason != ReasonResponseTooFast {
		t.Errorf("veredicto %+v: Reveal reinició la ventana", v)
	}
}

func TestRevealNotAvailableOutsideActiveStates(t *testing.T) {
	s, clk := newTestSession(t)

	if _, err := s.Reveal(); !errors.Is(err, ErrNothingToReveal) {
		t.Errorf("Reveal en estado creada: %v", err)
	}

	startAndCalibrate(t, s, clk)
	playChallenges(t, s, clk)

	if _, err := s.Reveal(); !errors.Is(err, ErrNothingToReveal) {
		t.Errorf("Reveal en estado evaluando: %v", err)
	}
	if _, err := s.Resolve(Verdict{Outcome: OutcomePass}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := s.Reveal(); !errors.Is(err, ErrNothingToReveal) {
		t.Errorf("Reveal en estado resuelta: %v", err)
	}
}

// --- transiciones inválidas -------------------------------------------------

func TestInvalidOperationsPerState(t *testing.T) {
	t.Run("Start dos veces", func(t *testing.T) {
		s, _ := newTestSession(t)
		if err := s.Start(); err != nil {
			t.Fatalf("primer Start: %v", err)
		}
		err := s.Start()
		if !errors.Is(err, fsm.ErrInvalidTransition) {
			t.Errorf("segundo Start: %v", err)
		}
		if s.State() != fsm.StateCalibrating {
			t.Errorf("el estado cambió a %s", s.State())
		}
	})

	t.Run("Submit antes de Start", func(t *testing.T) {
		s, _ := newTestSession(t)
		_, err := s.Submit()
		if !errors.Is(err, fsm.ErrInvalidTransition) {
			t.Errorf("Submit en estado creada: %v", err)
		}
		if s.State() != fsm.StateCreated {
			t.Errorf("el estado cambió a %s", s.State())
		}
	})

	t.Run("Resolve antes de evaluar", func(t *testing.T) {
		s, clk := newTestSession(t)
		if _, err := s.Resolve(Verdict{Outcome: OutcomePass}); !errors.Is(err, fsm.ErrInvalidTransition) {
			t.Errorf("Resolve en estado creada: %v", err)
		}
		startAndCalibrate(t, s, clk)
		if _, err := s.Resolve(Verdict{Outcome: OutcomePass}); !errors.Is(err, fsm.ErrInvalidTransition) {
			t.Errorf("Resolve con retos pendientes: %v", err)
		}
		if s.Outcome() != OutcomeUnspecified {
			t.Errorf("un Resolve fallido fijó el desenlace a %s", s.Outcome())
		}
	})

	t.Run("Resolve dos veces", func(t *testing.T) {
		s, clk := newTestSession(t)
		startAndCalibrate(t, s, clk)
		playChallenges(t, s, clk)
		if _, err := s.Resolve(Verdict{Outcome: OutcomePass}); err != nil {
			t.Fatalf("primer Resolve: %v", err)
		}
		if _, err := s.Resolve(Verdict{Outcome: OutcomeFail}); !errors.Is(err, fsm.ErrInvalidTransition) {
			t.Errorf("segundo Resolve: %v", err)
		}
		if s.Outcome() != OutcomePass {
			t.Errorf("el segundo Resolve cambió el desenlace a %s", s.Outcome())
		}
	})

	t.Run("Submit tras resolver", func(t *testing.T) {
		s, clk := newTestSession(t)
		startAndCalibrate(t, s, clk)
		playChallenges(t, s, clk)
		if _, err := s.Resolve(Verdict{Outcome: OutcomePass}); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if _, err := s.Submit(); !errors.Is(err, fsm.ErrInvalidTransition) {
			t.Errorf("Submit tras resolver: %v", err)
		}
	})

	t.Run("Submit tras un paso rechazado", func(t *testing.T) {
		s, clk := newTestSession(t)
		if err := s.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		clk.Advance(50 * time.Millisecond)
		if _, err := s.SubmitAt(clk.Now()); err != nil {
			t.Fatalf("SubmitAt: %v", err)
		}
		if s.State() != fsm.StateEvaluating {
			t.Fatalf("un rechazo debe cerrar la fase de retos, estado %s", s.State())
		}
		if _, err := s.Submit(); !errors.Is(err, fsm.ErrInvalidTransition) {
			t.Errorf("Submit tras un rechazo: %v", err)
		}
	})
}

// --- ventanas temporales ----------------------------------------------------

// TestTooFastIsAsSuspiciousAsTimeout: el punto 3 del diseño.
func TestResponseWindow(t *testing.T) {
	cases := []struct {
		name       string
		elapsed    time.Duration
		wantOK     bool
		wantReason Reason
	}{
		{"instantánea", 0, false, ReasonResponseTooFast},
		{"justo por debajo del mínimo", 1499 * time.Millisecond, false, ReasonResponseTooFast},
		{"en el mínimo exacto", 1500 * time.Millisecond, true, ReasonNone},
		{"dentro de la ventana", 2 * time.Second, true, ReasonNone},
		{"en el máximo exacto", 3 * time.Second, true, ReasonNone},
		{"pasado el máximo", 3001 * time.Millisecond, false, ReasonResponseTimeout},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, clk := newTestSession(t)
			if err := s.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}
			step, err := s.Reveal()
			if err != nil {
				t.Fatalf("Reveal: %v", err)
			}
			// La calibración por defecto: 1,5 s de espera, 3 s de plazo.
			if step.Hold != 1500*time.Millisecond || step.Deadline != 3*time.Second {
				t.Fatalf("paso de calibración inesperado: %+v", step)
			}

			clk.Advance(c.elapsed)
			v, err := s.SubmitAt(clk.Now())
			if err != nil {
				t.Fatalf("SubmitAt: %v", err)
			}
			if v.Accepted != c.wantOK || v.Reason != c.wantReason {
				t.Errorf("con %v: accepted=%v reason=%q, se esperaba %v/%q",
					c.elapsed, v.Accepted, v.Reason, c.wantOK, c.wantReason)
			}
			if v.Elapsed != c.elapsed {
				t.Errorf("Elapsed = %v, se esperaba %v", v.Elapsed, c.elapsed)
			}
			if v.StepID != step.ID {
				t.Errorf("StepID = %q, se esperaba %q", v.StepID, step.ID)
			}

			// Los dos motivos son distintos y con severidad distinta.
			if !c.wantOK {
				if s.State() != fsm.StateEvaluating {
					t.Errorf("un rechazo debe pasar a evaluando, estado %s", s.State())
				}
				if got := s.Reasons(); len(got) != 1 || got[0] != c.wantReason {
					t.Errorf("motivos acumulados = %v", got)
				}
			}
		})
	}
}

// TestTooFastAndTimeoutDifferInSeverity fija la diferencia de trato: la
// respuesta imposible no se reintenta, la lenta sí.
func TestTooFastAndTimeoutDifferInSeverity(t *testing.T) {
	if ReasonResponseTooFast == ReasonResponseTimeout {
		t.Fatal("los dos casos deben tener códigos distintos")
	}
	if ReasonResponseTooFast.Severity() != SeverityHard {
		t.Errorf("too_fast debería ser duro, es %s", ReasonResponseTooFast.Severity())
	}
	if ReasonResponseTimeout.Severity() != SeveritySoft {
		t.Errorf("timeout debería ser blando, es %s", ReasonResponseTimeout.Severity())
	}
}

// TestPoseWindowIsNotRevealed: el cliente recibe el plazo, nunca el mínimo.
func TestPoseWindowMinimumIsNotRevealed(t *testing.T) {
	s, clk := newTestSession(t)
	startAndCalibrate(t, s, clk)

	step, err := s.Reveal()
	if err != nil {
		t.Fatalf("Reveal: %v", err)
	}
	if step.Deadline <= 0 {
		t.Error("el paso revelado debe llevar plazo")
	}

	// Responder por debajo del mínimo humano se rechaza, y el cliente no
	// tenía forma de saber dónde estaba ese límite.
	clk.Advance(100 * time.Millisecond)
	v, err := s.SubmitAt(clk.Now())
	if err != nil {
		t.Fatalf("SubmitAt: %v", err)
	}
	if v.Accepted || v.Reason != ReasonResponseTooFast {
		t.Errorf("veredicto %+v", v)
	}
}

// TestSubmitWithSkewedTimestamp: una marca anterior a la revelación da tiempo
// negativo y se trata como demasiado rápida, no como aceptada.
func TestSubmitWithTimestampBeforeReveal(t *testing.T) {
	s, clk := newTestSession(t)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	v, err := s.SubmitAt(clk.Now().Add(-2 * time.Second))
	if err != nil {
		t.Fatalf("SubmitAt: %v", err)
	}
	if v.Accepted || v.Reason != ReasonResponseTooFast {
		t.Errorf("veredicto %+v con marca de tiempo anterior a la revelación", v)
	}
	if v.Elapsed >= 0 {
		t.Errorf("Elapsed = %v, se esperaba negativo", v.Elapsed)
	}
}

// --- Tick -------------------------------------------------------------------

func TestTickTimesOutTheActiveStep(t *testing.T) {
	s, clk := newTestSession(t)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	step, err := s.Reveal()
	if err != nil {
		t.Fatalf("Reveal: %v", err)
	}

	// Antes del plazo no pasa nada.
	clk.Advance(step.Deadline)
	v, err := s.Tick()
	if err != nil || v != nil {
		t.Fatalf("Tick en el plazo exacto: %+v, %v", v, err)
	}
	if s.State() != fsm.StateCalibrating {
		t.Errorf("estado %s", s.State())
	}

	clk.Advance(time.Millisecond)
	v, err = s.Tick()
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if v == nil {
		t.Fatal("Tick no detectó el timeout")
	}
	if v.Accepted || v.Reason != ReasonResponseTimeout || v.StepID != step.ID {
		t.Errorf("veredicto %+v", v)
	}
	if s.State() != fsm.StateEvaluating {
		t.Errorf("tras el timeout el estado es %s", s.State())
	}

	// Un Tick posterior ya no tiene nada que hacer.
	v, err = s.Tick()
	if err != nil || v != nil {
		t.Errorf("Tick en estado evaluando: %+v, %v", v, err)
	}
}

func TestTickIsNoopInNonActiveStates(t *testing.T) {
	s, _ := newTestSession(t)
	v, err := s.Tick()
	if err != nil || v != nil {
		t.Errorf("Tick en estado creada: %+v, %v", v, err)
	}
	if s.State() != fsm.StateCreated {
		t.Errorf("Tick alteró el estado a %s", s.State())
	}
}

// --- expiración -------------------------------------------------------------

func TestExplicitExpireFromEveryLiveState(t *testing.T) {
	t.Run("creada", func(t *testing.T) {
		s, _ := newTestSession(t)
		if err := s.Expire(); err != nil {
			t.Fatalf("Expire: %v", err)
		}
		if s.State() != fsm.StateExpired {
			t.Errorf("estado %s", s.State())
		}
		if got := s.Reasons(); len(got) != 1 || got[0] != ReasonSessionExpired {
			t.Errorf("motivos = %v", got)
		}
	})

	t.Run("calibrando", func(t *testing.T) {
		s, _ := newTestSession(t)
		if err := s.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if err := s.Expire(); err != nil {
			t.Fatalf("Expire: %v", err)
		}
		if s.State() != fsm.StateExpired {
			t.Errorf("estado %s", s.State())
		}
	})

	t.Run("reto_activo", func(t *testing.T) {
		s, clk := newTestSession(t)
		startAndCalibrate(t, s, clk)
		if err := s.Expire(); err != nil {
			t.Fatalf("Expire: %v", err)
		}
		if s.State() != fsm.StateExpired {
			t.Errorf("estado %s", s.State())
		}
	})

	t.Run("evaluando", func(t *testing.T) {
		s, clk := newTestSession(t)
		startAndCalibrate(t, s, clk)
		playChallenges(t, s, clk)
		if err := s.Expire(); err != nil {
			t.Fatalf("Expire: %v", err)
		}
		if s.State() != fsm.StateExpired {
			t.Errorf("estado %s", s.State())
		}
	})
}

// TestExpireIsInvalidOnTerminalStates: expirar una sesión ya resuelta
// reescribiría un veredicto final.
func TestExpireIsInvalidOnTerminalStates(t *testing.T) {
	s, clk := newTestSession(t)
	startAndCalibrate(t, s, clk)
	playChallenges(t, s, clk)
	if _, err := s.Resolve(Verdict{Outcome: OutcomePass}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := s.Expire(); !errors.Is(err, fsm.ErrInvalidTransition) {
		t.Errorf("Expire sobre resuelta: %v", err)
	}
	if s.State() != fsm.StateResolved || s.Outcome() != OutcomePass {
		t.Errorf("estado %s, desenlace %s", s.State(), s.Outcome())
	}

	other, _ := newTestSession(t)
	if err := other.Expire(); err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if err := other.Expire(); !errors.Is(err, fsm.ErrInvalidTransition) {
		t.Errorf("Expire dos veces: %v", err)
	}
}

// TestBudgetExpiryOnEveryEntryPoint: agotado el presupuesto, ninguna
// operación sigue adelante.
func TestBudgetExpiryOnEveryEntryPoint(t *testing.T) {
	ops := map[string]func(*Session) error{
		"Start":   func(s *Session) error { return s.Start() },
		"Reveal":  func(s *Session) error { _, err := s.Reveal(); return err },
		"Submit":  func(s *Session) error { _, err := s.Submit(); return err },
		"Tick":    func(s *Session) error { _, err := s.Tick(); return err },
		"Resolve": func(s *Session) error { _, err := s.Resolve(Verdict{Outcome: OutcomePass}); return err },
	}

	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			s, clk := newTestSession(t)
			if err := s.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}
			clk.Set(s.Deadline())

			if err := op(s); !errors.Is(err, ErrSessionExpired) {
				t.Errorf("se esperaba ErrSessionExpired, se obtuvo %v", err)
			}
			if s.State() != fsm.StateExpired {
				t.Errorf("estado %s", s.State())
			}
			if got := s.Reasons(); len(got) == 0 || got[0] != ReasonSessionExpired {
				t.Errorf("motivos = %v", got)
			}
			// Y sigue expirada en la siguiente llamada.
			if err := op(s); !errors.Is(err, ErrSessionExpired) {
				t.Errorf("segunda llamada: %v", err)
			}
		})
	}
}

func TestExpiryIsNotRetroactiveForResolvedSessions(t *testing.T) {
	s, clk := newTestSession(t)
	startAndCalibrate(t, s, clk)
	playChallenges(t, s, clk)
	if _, err := s.Resolve(Verdict{Outcome: OutcomePass}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	clk.Set(s.Deadline().Add(time.Hour))
	if _, err := s.Reveal(); !errors.Is(err, ErrNothingToReveal) {
		t.Errorf("una sesión resuelta no expira a posteriori: %v", err)
	}
	if s.State() != fsm.StateResolved {
		t.Errorf("estado %s", s.State())
	}
}

// --- política de resolución --------------------------------------------------

func TestResolutionPolicy(t *testing.T) {
	cases := []struct {
		name        string
		play        string // limpio | rapido | timeout
		verdict     Verdict
		wantOutcome Outcome
		wantReason  Reason
	}{
		{"limpio + pass", "limpio", Verdict{Outcome: OutcomePass}, OutcomePass, ReasonPassed},
		{"limpio + fail", "limpio", Verdict{Outcome: OutcomeFail}, OutcomeFail, ReasonSignalsIndicateSpoof},
		{"limpio + retry", "limpio", Verdict{Outcome: OutcomeRetry}, OutcomeRetry, ReasonQualityInsufficient},
		{"limpio sin veredicto", "limpio", Verdict{}, OutcomeRetry, ReasonQualityInsufficient},
		{"limpio + motivo duro de la fusión", "limpio",
			Verdict{Outcome: OutcomePass, Reason: ReasonSignalsIndicateSpoof},
			OutcomeFail, ReasonSignalsIndicateSpoof},
		{"limpio + motivo blando de la fusión", "limpio",
			Verdict{Outcome: OutcomeRetry, Reason: ReasonQualityInsufficient},
			OutcomeRetry, ReasonQualityInsufficient},

		// Lo importante: un motivo duro registrado durante los retos manda
		// sobre un pass de la fusión.
		{"demasiado rápido + pass", "rapido", Verdict{Outcome: OutcomePass}, OutcomeFail, ReasonResponseTooFast},
		{"demasiado rápido + retry", "rapido", Verdict{Outcome: OutcomeRetry}, OutcomeFail, ReasonResponseTooFast},

		// Un motivo blando degrada a reintento, pero no absuelve un fail.
		{"timeout + pass", "timeout", Verdict{Outcome: OutcomePass}, OutcomeRetry, ReasonResponseTimeout},
		{"timeout + fail", "timeout", Verdict{Outcome: OutcomeFail}, OutcomeFail, ReasonSignalsIndicateSpoof},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, clk := newTestSession(t)

			switch c.play {
			case "limpio":
				startAndCalibrate(t, s, clk)
				playChallenges(t, s, clk)
			case "rapido":
				if err := s.Start(); err != nil {
					t.Fatalf("Start: %v", err)
				}
				clk.Advance(10 * time.Millisecond)
				if _, err := s.SubmitAt(clk.Now()); err != nil {
					t.Fatalf("SubmitAt: %v", err)
				}
			case "timeout":
				if err := s.Start(); err != nil {
					t.Fatalf("Start: %v", err)
				}
				clk.Advance(10 * time.Second)
				if _, err := s.Tick(); err != nil {
					t.Fatalf("Tick: %v", err)
				}
			}

			if s.State() != fsm.StateEvaluating {
				t.Fatalf("estado previo a resolver: %s", s.State())
			}

			res, err := s.Resolve(c.verdict)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if res.Outcome != c.wantOutcome {
				t.Errorf("desenlace %s, se esperaba %s", res.Outcome, c.wantOutcome)
			}
			if res.Reason != c.wantReason {
				t.Errorf("motivo %q, se esperaba %q", res.Reason, c.wantReason)
			}
			if s.Outcome() != c.wantOutcome {
				t.Errorf("Outcome() = %s", s.Outcome())
			}
			if len(res.Reasons) == 0 && c.wantReason != ReasonNone {
				t.Error("Resolution.Reasons vacío")
			}
		})
	}
}

// TestResolveDoesNotDuplicateReason: el motivo determinante ya registrado no
// se apunta dos veces.
func TestResolveDoesNotDuplicateReason(t *testing.T) {
	s, clk := newTestSession(t)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	clk.Advance(10 * time.Millisecond)
	if _, err := s.SubmitAt(clk.Now()); err != nil {
		t.Fatalf("SubmitAt: %v", err)
	}
	res, err := s.Resolve(Verdict{Outcome: OutcomePass})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	count := 0
	for _, r := range res.Reasons {
		if r == ReasonResponseTooFast {
			count++
		}
	}
	if count != 1 {
		t.Errorf("el motivo aparece %d veces: %v", count, res.Reasons)
	}
}

func TestReasonsReturnsACopy(t *testing.T) {
	s, _ := newTestSession(t)
	if err := s.Expire(); err != nil {
		t.Fatalf("Expire: %v", err)
	}
	got := s.Reasons()
	if len(got) != 1 {
		t.Fatalf("motivos = %v", got)
	}
	got[0] = "manipulado"
	if s.Reasons()[0] != ReasonSessionExpired {
		t.Error("Reasons() expone el estado interno: el llamante lo pudo modificar")
	}
}

func TestResolutionBeforeResolve(t *testing.T) {
	s, _ := newTestSession(t)
	if _, ok := s.Resolution(); ok {
		t.Error("Resolution() disponible antes de resolver")
	}
}

// TestEmptyScriptInvariant fuerza la invariante rota (un guion sin retos tras
// la calibración) para comprobar que da error explícito y no un estado
// silenciosamente incoherente.
func TestEmptyScriptInvariant(t *testing.T) {
	s, clk := newTestSession(t)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Un guion espejo (misma semilla, mismo contenido) da la longitud sin
	// tocar el de la sesión.
	mirror, err := challenge.Generate(testSeed, challenge.DefaultPolicy())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	length := 1
	for mirror.Advance() {
		length++
	}

	// Deja el cursor en el último paso: Current() responde, Advance() no.
	for range length - 1 {
		s.script.Advance()
	}
	last, ok := s.script.Current()
	if !ok {
		t.Fatal("el guion quedó agotado")
	}

	s.revealedAt = clk.Now()
	clk.Advance(last.Window.Min)
	if _, err := s.SubmitAt(clk.Now()); !errors.Is(err, ErrEmptyScript) {
		t.Errorf("se esperaba ErrEmptyScript, se obtuvo %v", err)
	}
}

func TestSeverityAndOutcomeStrings(t *testing.T) {
	severities := map[Severity]string{
		SeverityNone: "none",
		SeveritySoft: "soft",
		SeverityHard: "hard",
		Severity(9):  "severity(9)",
	}
	for s, want := range severities {
		if got := s.String(); got != want {
			t.Errorf("Severity(%d) = %q, se esperaba %q", uint8(s), got, want)
		}
	}
	outcomes := map[Outcome]string{
		OutcomeUnspecified: "unspecified",
		OutcomePass:        "pass",
		OutcomeFail:        "fail",
		OutcomeRetry:       "retry",
		Outcome(9):         "outcome(9)",
	}
	for o, want := range outcomes {
		if got := o.String(); got != want {
			t.Errorf("Outcome(%d) = %q, se esperaba %q", uint8(o), got, want)
		}
	}
}

func TestReasonSeverityClassification(t *testing.T) {
	cases := map[Reason]Severity{
		ReasonNone:                 SeverityNone,
		ReasonPassed:               SeverityNone,
		ReasonResponseTooFast:      SeverityHard,
		ReasonSignalsIndicateSpoof: SeverityHard,
		ReasonResponseTimeout:      SeveritySoft,
		ReasonSessionExpired:       SeveritySoft,
		ReasonQualityInsufficient:  SeveritySoft,
		Reason("inventado"):        SeveritySoft, // lo desconocido no acusa
	}
	for r, want := range cases {
		if got := r.Severity(); got != want {
			t.Errorf("Reason(%q).Severity() = %s, se esperaba %s", r, got, want)
		}
	}
}

// TestActiveStateWithExhaustedScript cubre la vía defensiva: si el guion se
// agotara sin que la máquina de estados lo supiera, las operaciones deben dar
// error explícito en vez de trabajar sobre un paso inexistente.
func TestActiveStateWithExhaustedScript(t *testing.T) {
	exhausted := func(t *testing.T) (*Session, *clock.Fake) {
		t.Helper()
		s, clk := newTestSession(t)
		if err := s.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		for s.script.Advance() {
		}
		if _, ok := s.script.Current(); ok {
			t.Fatal("el guion no quedó agotado")
		}
		return s, clk
	}

	t.Run("Reveal", func(t *testing.T) {
		s, _ := exhausted(t)
		if _, err := s.Reveal(); !errors.Is(err, ErrNothingToReveal) {
			t.Errorf("Reveal: %v", err)
		}
	})

	t.Run("SubmitAt", func(t *testing.T) {
		s, clk := exhausted(t)
		if _, err := s.SubmitAt(clk.Now()); !errors.Is(err, ErrNothingToReveal) {
			t.Errorf("SubmitAt: %v", err)
		}
	})

	t.Run("Tick", func(t *testing.T) {
		s, _ := exhausted(t)
		v, err := s.Tick()
		if err != nil || v != nil {
			t.Errorf("Tick: %+v, %v", v, err)
		}
	})
}

func TestRecordIgnoresEmptyReason(t *testing.T) {
	s, _ := newTestSession(t)
	s.record(ReasonNone)
	if got := s.Reasons(); len(got) != 0 {
		t.Errorf("motivos = %v, se esperaba ninguno", got)
	}
}

// --- aborto por causa externa ------------------------------------------------

// TestAbortFromEveryLiveState: una caída de infraestructura puede pillar la
// sesión en cualquier punto.
func TestAbortFromEveryLiveState(t *testing.T) {
	setups := map[string]func(*testing.T) (*Session, *clock.Fake){
		"creada": func(t *testing.T) (*Session, *clock.Fake) {
			return newTestSession(t)
		},
		"calibrando": func(t *testing.T) (*Session, *clock.Fake) {
			s, clk := newTestSession(t)
			if err := s.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}
			return s, clk
		},
		"reto_activo": func(t *testing.T) (*Session, *clock.Fake) {
			s, clk := newTestSession(t)
			startAndCalibrate(t, s, clk)
			return s, clk
		},
		"evaluando": func(t *testing.T) (*Session, *clock.Fake) {
			s, clk := newTestSession(t)
			startAndCalibrate(t, s, clk)
			playChallenges(t, s, clk)
			return s, clk
		},
	}

	for name, setup := range setups {
		t.Run(name, func(t *testing.T) {
			s, _ := setup(t)

			res, err := s.Abort(ReasonAnalyzerUnavailable)
			if err != nil {
				t.Fatalf("Abort: %v", err)
			}
			if s.State() != fsm.StateResolved {
				t.Errorf("estado %s, se esperaba resuelta", s.State())
			}
			if res.Outcome != OutcomeRetry {
				t.Errorf("desenlace %s, se esperaba retry: una caída nuestra no es culpa del usuario", res.Outcome)
			}
			if res.Reason != ReasonAnalyzerUnavailable {
				t.Errorf("motivo %q, se esperaba %q", res.Reason, ReasonAnalyzerUnavailable)
			}
			// El motivo se conserva tal cual: la auditoría no puede
			// confundir una caída del analizador con un usuario lento.
			if got := s.Reasons(); len(got) == 0 || got[len(got)-1] != ReasonAnalyzerUnavailable {
				t.Errorf("motivos = %v", got)
			}
			if got, ok := s.Resolution(); !ok || got.Reason != ReasonAnalyzerUnavailable {
				t.Errorf("Resolution() = %+v, %v", got, ok)
			}
		})
	}
}

// TestAbortIsInvalidOnTerminalStates: abortar una sesión ya resuelta
// reescribiría un veredicto final.
func TestAbortIsInvalidOnTerminalStates(t *testing.T) {
	s, clk := newTestSession(t)
	startAndCalibrate(t, s, clk)
	playChallenges(t, s, clk)
	if _, err := s.Resolve(Verdict{Outcome: OutcomePass}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if _, err := s.Abort(ReasonAnalyzerUnavailable); !errors.Is(err, fsm.ErrInvalidTransition) {
		t.Errorf("Abort sobre resuelta: %v", err)
	}
	if s.Outcome() != OutcomePass {
		t.Errorf("el aborto cambió el desenlace a %s", s.Outcome())
	}

	other, _ := newTestSession(t)
	if err := other.Expire(); err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if _, err := other.Abort(ReasonAnalyzerUnavailable); !errors.Is(err, fsm.ErrInvalidTransition) {
		t.Errorf("Abort sobre expirada: %v", err)
	}
}

// TestAbortKeepsHardReasons: si durante los retos ya había indicio de ataque,
// una caída posterior de infraestructura no lo absuelve.
func TestAbortKeepsHardReasons(t *testing.T) {
	s, clk := newTestSession(t)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	clk.Advance(10 * time.Millisecond)
	if _, err := s.SubmitAt(clk.Now()); err != nil { // demasiado rápido: motivo duro
		t.Fatalf("SubmitAt: %v", err)
	}

	res, err := s.Abort(ReasonAnalyzerUnavailable)
	if err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if res.Outcome != OutcomeFail {
		t.Errorf("desenlace %s, se esperaba fail: había un motivo duro registrado", res.Outcome)
	}
	if res.Reason != ReasonResponseTooFast {
		t.Errorf("motivo %q, se esperaba %q", res.Reason, ReasonResponseTooFast)
	}
}

// TestAbortWithHardReason: abortar con un motivo duro resuelve en fail.
func TestAbortWithHardReason(t *testing.T) {
	s, _ := newTestSession(t)
	res, err := s.Abort(ReasonSignalsIndicateSpoof)
	if err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if res.Outcome != OutcomeFail {
		t.Errorf("desenlace %s, se esperaba fail", res.Outcome)
	}
}

func TestAnalyzerUnavailableIsSoft(t *testing.T) {
	if got := ReasonAnalyzerUnavailable.Severity(); got != SeveritySoft {
		t.Errorf("severidad %s, se esperaba blanda: una caída nuestra se reintenta", got)
	}
}
