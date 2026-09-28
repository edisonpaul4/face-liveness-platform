// Este fichero está en el paquete de test EXTERNO a propósito: sólo ve la API
// pública, igual que la verá la capa de transporte. Es el criterio de
// aceptación de la propiedad de seguridad central (CLAUDE.md §6): ningún
// método público expone pasos futuros del guion.
package session_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/edisonpaul4/biometrics/orchestrator/core/challenge"
	"github.com/edisonpaul4/biometrics/orchestrator/core/clock"
	"github.com/edisonpaul4/biometrics/orchestrator/core/fsm"
	"github.com/edisonpaul4/biometrics/orchestrator/core/session"
)

const leakSeed = challenge.Seed(0xA11CE)

var leakT0 = time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

// mirrorScript reconstruye el guion completo con la misma semilla. El test
// necesita conocer el futuro para poder comprobar que la sesión no lo suelta;
// es lo único que puede hacerlo, y por eso se genera aparte.
func mirrorScript(t *testing.T, seed challenge.Seed) []challenge.Step {
	t.Helper()
	sc, err := challenge.Generate(seed, challenge.DefaultPolicy())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var steps []challenge.Step
	for {
		st, ok := sc.Current()
		if !ok {
			break
		}
		steps = append(steps, st)
		if !sc.Advance() {
			break
		}
	}
	if len(steps) < 3 {
		t.Fatalf("guion demasiado corto para el test: %d pasos", len(steps))
	}
	return steps
}

// publicSnapshot vuelca TODO lo que la API pública puede devolver en este
// instante. Es el material que la capa de transporte podría llegar a
// serializar hacia el cliente.
func publicSnapshot(s *session.Session) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ID=%+v\n", s.ID())
	fmt.Fprintf(&b, "State=%+v\n", s.State())
	fmt.Fprintf(&b, "Outcome=%+v\n", s.Outcome())
	fmt.Fprintf(&b, "RevealedCount=%+v\n", s.RevealedCount())
	fmt.Fprintf(&b, "Reasons=%+v\n", s.Reasons())
	fmt.Fprintf(&b, "CreatedAt=%+v\n", s.CreatedAt())
	fmt.Fprintf(&b, "Deadline=%+v\n", s.Deadline())

	step, err := s.Reveal()
	fmt.Fprintf(&b, "Reveal=%+v err=%v\n", step, err)

	res, ok := s.Resolution()
	fmt.Fprintf(&b, "Resolution=%+v ok=%v\n", res, ok)
	return b.String()
}

// TestPublicAPINeverExposesFutureSteps es el test de aceptación.
//
// Recorre una sesión completa y, en cada instante, comprueba que nada de lo
// que la API pública puede devolver menciona un paso que todavía no toca.
func TestPublicAPINeverExposesFutureSteps(t *testing.T) {
	steps := mirrorScript(t, leakSeed)

	clk := clock.NewFake(leakT0)
	s, err := session.New(session.Config{
		ID:     "01J0LEAK",
		Seed:   leakSeed,
		Clock:  clk,
		Budget: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// current es el índice del paso activo; -1 mientras la sesión no arranca.
	current := -1

	check := func(moment string) {
		t.Helper()
		snap := publicSnapshot(s)
		for i := current + 1; i < len(steps); i++ {
			if strings.Contains(snap, steps[i].ID) {
				t.Errorf("%s: la API pública filtró el paso futuro %d (%s):\n%s",
					moment, i, steps[i].ID, snap)
			}
		}
		// Y de paso: nunca puede aparecer nada del guion salvo el paso activo.
		for i := range steps {
			if i == current {
				continue
			}
			if strings.Contains(snap, steps[i].ID) {
				t.Errorf("%s: la API pública mencionó el paso %d (%s), que no es el activo:\n%s",
					moment, i, steps[i].ID, snap)
			}
		}
	}

	check("recién creada")

	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	current = 0
	check("calibrando")

	for s.State() == fsm.StateCalibrating || s.State() == fsm.StateChallengeActive {
		step, err := s.Reveal()
		if err != nil {
			t.Fatalf("Reveal en el paso %d: %v", current, err)
		}
		if step.ID != steps[current].ID {
			t.Fatalf("Reveal devolvió %s, se esperaba el paso activo %s", step.ID, steps[current].ID)
		}

		wait := step.Hold
		if wait == 0 {
			wait = 800 * time.Millisecond
		}
		clk.Advance(wait)

		v, err := s.SubmitAt(clk.Now())
		if err != nil {
			t.Fatalf("SubmitAt en el paso %d: %v", current, err)
		}
		if !v.Accepted {
			t.Fatalf("paso %d rechazado: %s", current, v.Reason)
		}
		// El veredicto sólo puede hablar del paso que se acaba de responder.
		if v.StepID != steps[current].ID {
			t.Errorf("StepVerdict.StepID = %s, se esperaba %s", v.StepID, steps[current].ID)
		}

		if current+1 < len(steps) {
			current++
		}
		check(fmt.Sprintf("tras responder al paso %d", current-1))
	}

	check("evaluando")

	if _, err := s.Resolve(session.Verdict{Outcome: session.OutcomePass}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Ya resuelta, current apunta al último paso: nada del guion debe salir.
	current = len(steps)
	check("resuelta")
}

// TestRejectedSessionDoesNotLeakTheRest: al rechazar un paso, la sesión se
// cierra sin enseñar lo que venía después. Es justo el momento en que un
// atacante querría sonsacar el resto del guion.
func TestRejectedSessionDoesNotLeakTheRest(t *testing.T) {
	steps := mirrorScript(t, leakSeed)

	clk := clock.NewFake(leakT0)
	s, err := session.New(session.Config{ID: "01J0LEAK2", Seed: leakSeed, Clock: clk, Budget: time.Minute})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Respuesta imposiblemente rápida a la calibración.
	clk.Advance(10 * time.Millisecond)
	if _, err := s.SubmitAt(clk.Now()); err != nil {
		t.Fatalf("SubmitAt: %v", err)
	}
	if _, err := s.Resolve(session.Verdict{Outcome: session.OutcomePass}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	snap := publicSnapshot(s)
	for i := 1; i < len(steps); i++ {
		if strings.Contains(snap, steps[i].ID) {
			t.Errorf("tras el rechazo se filtró el paso %d (%s):\n%s", i, steps[i].ID, snap)
		}
	}
}

// TestExpiredSessionDoesNotLeakTheRest: lo mismo al expirar.
func TestExpiredSessionDoesNotLeakTheRest(t *testing.T) {
	steps := mirrorScript(t, leakSeed)

	clk := clock.NewFake(leakT0)
	s, err := session.New(session.Config{ID: "01J0LEAK3", Seed: leakSeed, Clock: clk, Budget: time.Minute})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.Expire(); err != nil {
		t.Fatalf("Expire: %v", err)
	}

	snap := publicSnapshot(s)
	for i := range steps {
		if strings.Contains(snap, steps[i].ID) {
			t.Errorf("una sesión expirada filtró el paso %d (%s):\n%s", i, steps[i].ID, snap)
		}
	}
}

// --- comprobaciones estructurales -------------------------------------------

// forbidden son los tipos que jamás pueden ser alcanzables desde un valor
// devuelto por la API pública de la sesión.
func forbiddenTypes() map[reflect.Type]string {
	return map[reflect.Type]string{
		reflect.TypeFor[challenge.Step]():    "un paso del guion en crudo",
		reflect.TypeFor[challenge.Script]():  "el guion",
		reflect.TypeFor[challenge.Seed]():    "la semilla (permite regenerar el guion entero)",
		reflect.TypeFor[challenge.Window]():  "la ventana de reacción (revela el mínimo humano)",
		reflect.TypeFor[challenge.Policy]():  "la política (revela cuántos retos hay)",
		reflect.TypeFor[*challenge.Script](): "el guion",
	}
}

// walkType busca tipos prohibidos alcanzables desde t.
func walkType(t reflect.Type, forbidden map[reflect.Type]string, seen map[reflect.Type]bool, path string, report func(string, string)) {
	if t == nil || seen[t] {
		return
	}
	seen[t] = true

	if why, bad := forbidden[t]; bad {
		report(path, why)
		return
	}

	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Chan:
		walkType(t.Elem(), forbidden, seen, path+"→"+t.Elem().String(), report)
	case reflect.Map:
		walkType(t.Key(), forbidden, seen, path+"[clave]", report)
		walkType(t.Elem(), forbidden, seen, path+"[valor]", report)
	case reflect.Struct:
		for i := range t.NumField() {
			f := t.Field(i)
			walkType(f.Type, forbidden, seen, path+"."+f.Name, report)
		}
	}
}

// TestPublicSurfaceCannotReachTheScript: ningún método público de Session
// devuelve, ni directa ni indirectamente, un tipo desde el que se pueda leer
// el guion.
func TestPublicSurfaceCannotReachTheScript(t *testing.T) {
	forbidden := forbiddenTypes()
	st := reflect.TypeFor[*session.Session]()

	for i := range st.NumMethod() {
		m := st.Method(i)
		for out := range m.Type.NumOut() {
			ret := m.Type.Out(out)
			walkType(ret, forbidden, map[reflect.Type]bool{}, m.Name+"() "+ret.String(),
				func(path, why string) {
					t.Errorf("Session.%s alcanza %s: %s", m.Name, path, why)
				})
		}
	}
}

// TestScriptHasNoBulkAccessor: el propio guion tampoco ofrece una vía de
// acceso masivo. Es de avance único por construcción.
func TestScriptHasNoBulkAccessor(t *testing.T) {
	st := reflect.TypeFor[*challenge.Script]()
	allowed := map[string]bool{
		"Current":  true, // el paso actual, uno solo
		"Advance":  true, // la única forma de avanzar
		"Position": true, // cuántos van (pasado)
		"String":   true,
	}
	for i := range st.NumMethod() {
		m := st.Method(i)
		if !allowed[m.Name] {
			t.Errorf("Script tiene un método público nuevo, %s: revisa si permite leer el guion de golpe", m.Name)
		}
		for out := range m.Type.NumOut() {
			if m.Type.Out(out).Kind() == reflect.Slice {
				t.Errorf("Script.%s devuelve una colección: el guion no se entrega en bloque", m.Name)
			}
		}
	}
}

// TestRevealedStepCannotCarryTheFuture: el tipo que sale del núcleo no tiene
// dónde meter información del guion aunque alguien lo intente.
func TestRevealedStepCannotCarryTheFuture(t *testing.T) {
	rt := reflect.TypeFor[session.RevealedStep]()

	forbiddenNames := []string{
		"index", "position", "order", "number", "total", "count", "length",
		"seed", "script", "steps", "next", "remaining", "min", "attempt",
	}
	for i := range rt.NumField() {
		f := rt.Field(i)
		lower := strings.ToLower(f.Name)
		for _, bad := range forbiddenNames {
			if strings.Contains(lower, bad) {
				t.Errorf("RevealedStep.%s: el nombre sugiere información del guion (%q)", f.Name, bad)
			}
		}
	}

	forbidden := forbiddenTypes()
	walkType(rt, forbidden, map[reflect.Type]bool{}, "RevealedStep", func(path, why string) {
		t.Errorf("RevealedStep alcanza %s: %s", path, why)
	})
}

// TestRevealedStepOmitsTheMinimumWindow: al cliente se le da el plazo, nunca
// el umbral por debajo del cual su respuesta se considera imposible. Decirlo
// sería regalarle al atacante el margen exacto en el que debe responder.
func TestRevealedStepOmitsTheMinimumWindow(t *testing.T) {
	policy := challenge.DefaultPolicy()
	poses := 0

	// Recorre varias semillas: no todas generan poses, y la comprobación sólo
	// tiene sentido sobre ellas.
	for seed := challenge.Seed(0); seed < 20; seed++ {
		steps := mirrorScript(t, seed)

		clk := clock.NewFake(leakT0)
		s, err := session.New(session.Config{
			ID: fmt.Sprintf("01J0MIN%02d", seed), Seed: seed, Clock: clk, Budget: 5 * time.Minute,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := s.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}

		for i := 0; s.State() == fsm.StateCalibrating || s.State() == fsm.StateChallengeActive; i++ {
			step, err := s.Reveal()
			if err != nil {
				t.Fatalf("semilla %d, paso %d: Reveal: %v", seed, i, err)
			}
			if step.Deadline != steps[i].Window.Max {
				t.Errorf("semilla %d, paso %d: Deadline = %v, se esperaba Window.Max = %v",
					seed, i, step.Deadline, steps[i].Window.Max)
			}
			if step.Kind == challenge.KindPose {
				poses++
				// En una pose, el mínimo (350 ms) no se deduce de nada de lo
				// revelado: ni se manda, ni coincide con ningún otro campo.
				if step.Hold != 0 {
					t.Errorf("semilla %d, paso %d: una pose no impone duración, Hold = %v", seed, i, step.Hold)
				}
				if step.Deadline == policy.MinReaction {
					t.Errorf("semilla %d, paso %d: el plazo revelado coincide con el mínimo de reacción", seed, i)
				}
				// El "acércate" de antes del destello va sin mínimo: no tiene
				// parámetro sorteado, así que la rapidez no puede delatar una
				// respuesta pregrabada. Sigue sin revelarse nada de él.
				wantMin := policy.MinReaction
				if steps[i].Pose == challenge.PoseMoveCloser {
					wantMin = 0
				}
				if steps[i].Window.Min != wantMin {
					t.Fatalf("semilla %d, paso %d: la ventana mínima de la pose no es la de la política", seed, i)
				}
			}

			wait := step.Hold
			if wait == 0 {
				wait = 800 * time.Millisecond
			}
			clk.Advance(wait)
			if _, err := s.SubmitAt(clk.Now()); err != nil {
				t.Fatalf("semilla %d, paso %d: SubmitAt: %v", seed, i, err)
			}
		}
	}

	if poses == 0 {
		t.Fatal("ninguna de las 20 semillas generó una pose: el test no comprobó nada")
	}
}

// TestSessionHasNoExportedFields: sin campos públicos no hay forma de leer el
// guion saltándose los métodos.
func TestSessionHasNoExportedFields(t *testing.T) {
	st := reflect.TypeFor[session.Session]()
	for i := range st.NumField() {
		if f := st.Field(i); f.IsExported() {
			t.Errorf("Session.%s es un campo público", f.Name)
		}
	}
}

// TestPublicSurfaceIsLocked congela la superficie pública de Session.
//
// No es burocracia: cada método nuevo es una vía potencial de fuga. Si este
// test falla, añade el método a la lista DESPUÉS de comprobar que no revela
// nada del futuro del guion.
func TestPublicSurfaceIsLocked(t *testing.T) {
	expected := map[string]bool{
		// Abort cierra por causa externa; devuelve Resolution, que no lleva
		// nada del guion.
		"Abort":         true,
		"CreatedAt":     true,
		"Deadline":      true,
		"Expire":        true,
		"ID":            true,
		"Outcome":       true,
		"Reasons":       true,
		"Resolution":    true,
		"Resolve":       true,
		"Reveal":        true, // la ÚNICA vía de revelación, y sólo del paso activo
		"RevealedCount": true,
		"Start":         true,
		"State":         true,
		"Submit":        true,
		"SubmitAt":      true,
		"Tick":          true,
	}

	st := reflect.TypeFor[*session.Session]()
	got := map[string]bool{}
	for i := range st.NumMethod() {
		name := st.Method(i).Name
		got[name] = true
		if !expected[name] {
			t.Errorf("método público nuevo en Session: %s. ¿Revela algo del guion futuro?", name)
		}
	}
	for name, want := range expected {
		if want && !got[name] {
			t.Errorf("desapareció el método público Session.%s", name)
		}
	}
}
