// Package session es el núcleo del orquestador: la sesión de liveness como
// lógica pura.
//
// Sin E/S de ningún tipo: sin WebSocket, sin NATS, sin Redis, sin Postgres y
// sin time.Now(). El tiempo entra por un clock.Clock inyectado. Todo el
// paquete es determinista y se prueba con tests unitarios.
//
// PROPIEDAD DE SEGURIDAD CENTRAL (CLAUDE.md §6): el único método que revela
// contenido del guion es Reveal, y devuelve exclusivamente el paso ACTUAL,
// como RevealedStep, un tipo que estructuralmente no puede transportar el
// futuro: no tiene índice, ni total, ni semilla, ni la ventana mínima de
// reacción.
package session

import (
	"errors"
	"fmt"
	"time"

	"github.com/edisonpaul4/biometrics/orchestrator/core/challenge"
	"github.com/edisonpaul4/biometrics/orchestrator/core/clock"
	"github.com/edisonpaul4/biometrics/orchestrator/core/fsm"
)

// Errores del paquete.
var (
	// ErrSessionExpired: la sesión agotó su presupuesto de tiempo. La
	// operación no se ejecutó y la sesión quedó en estado expirada.
	ErrSessionExpired = errors.New("session: sesión expirada")

	// ErrNothingToReveal: el estado actual no tiene paso que revelar.
	ErrNothingToReveal = errors.New("session: no hay paso activo que revelar")

	// ErrInvalidConfig: configuración incoherente.
	ErrInvalidConfig = errors.New("session: configuración inválida")

	// ErrEmptyScript: el guion no contiene retos tras la calibración. Sólo
	// puede ocurrir con una política que Validate acepte y Generate deje
	// vacía; es una invariante rota, no un error de uso.
	ErrEmptyScript = errors.New("session: guion sin retos")
)

// DefaultBudget es el presupuesto total por defecto de una sesión.
const DefaultBudget = 90 * time.Second

// Config son los parámetros de una sesión.
type Config struct {
	// ID identifica la sesión (ULID en producción). Obligatorio.
	ID string
	// Seed es la semilla del guion. La genera el borde con crypto/rand y
	// nunca sale del servidor.
	Seed challenge.Seed
	// Policy son los parámetros de generación. Si es el cero, se usa
	// challenge.DefaultPolicy.
	Policy challenge.Policy
	// Clock es la fuente de tiempo. Si es nil, se usa clock.System.
	Clock clock.Clock
	// Budget es el presupuesto total. Si es 0, DefaultBudget.
	Budget time.Duration
}

// Verdict es lo que la fusión propone al cerrar la sesión.
//
// El núcleo puede ENDURECERLO (un motivo duro registrado durante los retos
// manda sobre un pass de la fusión), nunca ablandarlo.
type Verdict struct {
	Outcome Outcome
	Reason  Reason
}

// Resolution es el desenlace final de la sesión.
type Resolution struct {
	Outcome Outcome
	// Reason es el motivo determinante.
	Reason Reason
	// Reasons son todos los motivos acumulados, en orden de aparición.
	Reasons []Reason
	// ResolvedAt es el instante del veredicto.
	ResolvedAt time.Time
}

// StepVerdict es el resultado de validar la respuesta a un paso.
//
// No incluye la ventana: revelar Window.Min le diría al atacante a partir de
// qué milisegundo su respuesta deja de ser sospechosa.
type StepVerdict struct {
	StepID   string
	Accepted bool
	Reason   Reason
	// Elapsed es lo que tardó la respuesta desde que el paso se reveló.
	Elapsed time.Duration
}

// RevealedStep es lo único que la sesión revela del guion: el paso activo.
//
// Deliberadamente NO tiene: índice, total de pasos, semilla, identificador
// del paso siguiente ni la ventana mínima de reacción. Si algún día alguien
// necesita añadir un campo aquí, la primera pregunta es la de CLAUDE.md §6:
// ¿esto le dice al atacante algo del futuro?
type RevealedStep struct {
	// ID es opaco y no ordenable.
	ID string
	// Kind determina qué campos son significativos.
	Kind challenge.Kind
	// Pose sólo aplica si Kind == KindPose.
	Pose challenge.PoseAction
	// Flash sólo aplica si Kind == KindFlash.
	Flash []challenge.FlashSegment
	// Gaze sólo aplica si Kind == KindGaze. El objetivo SÍ se revela: el
	// cliente tiene que pintarlo. Lo que no se revela es cuántos vienen
	// detrás ni dónde estarán, que es lo que le serviría a un atacante.
	Gaze challenge.GazeTarget
	// GazeFrom es el primer punto del reto, del que se sale. Se revela por la
	// misma razón que Gaze: hay que pintarlo. Los DOS puntos son de este paso
	// y se materializan al emitirlo, así que no dicen nada del futuro.
	GazeFrom challenge.GazeTarget
	// Hold es la duración impuesta del paso (0 en pose).
	Hold time.Duration
	// Deadline es el plazo máximo de respuesta. Es Window.Max: el mínimo
	// nunca se revela.
	Deadline time.Duration
}

// Session es una sesión de liveness. No es segura para uso concurrente: el
// orquestador la posee y la serializa por sesión.
type Session struct {
	id      string
	clk     clock.Clock
	machine *fsm.Machine
	script  *challenge.Script

	createdAt  time.Time
	deadline   time.Time
	revealedAt time.Time
	revealed   int

	reasons    []Reason
	outcome    Outcome
	resolution *Resolution
}

// New crea una sesión en estado creada, con su guion ya generado y guardado
// del lado servidor.
func New(cfg Config) (*Session, error) {
	if cfg.ID == "" {
		return nil, fmt.Errorf("%w: ID vacío", ErrInvalidConfig)
	}
	if cfg.Budget < 0 {
		return nil, fmt.Errorf("%w: Budget negativo", ErrInvalidConfig)
	}
	policy := cfg.Policy
	if policy.IsZero() {
		policy = challenge.DefaultPolicy()
	}
	script, err := challenge.Generate(cfg.Seed, policy)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}

	clk := cfg.Clock
	if clk == nil {
		clk = clock.System{}
	}
	budget := cfg.Budget
	if budget == 0 {
		budget = DefaultBudget
	}
	now := clk.Now()

	return &Session{
		id:        cfg.ID,
		clk:       clk,
		machine:   fsm.New(),
		script:    script,
		createdAt: now,
		deadline:  now.Add(budget),
	}, nil
}

// ID devuelve el identificador de la sesión.
func (s *Session) ID() string { return s.id }

// State devuelve el estado actual.
func (s *Session) State() fsm.State { return s.machine.State() }

// Outcome devuelve el desenlace, o OutcomeUnspecified si aún no hay.
func (s *Session) Outcome() Outcome { return s.outcome }

// RevealedCount es cuántos pasos se han revelado ya, calibración incluida.
// Es información del pasado: cuántos van, nunca cuántos quedan.
func (s *Session) RevealedCount() int { return s.revealed }

// Reasons devuelve una copia de los motivos acumulados.
func (s *Session) Reasons() []Reason {
	out := make([]Reason, len(s.reasons))
	copy(out, s.reasons)
	return out
}

// CreatedAt es el instante de creación.
func (s *Session) CreatedAt() time.Time { return s.createdAt }

// Deadline es el instante en que la sesión expira.
func (s *Session) Deadline() time.Time { return s.deadline }

// Start arranca la sesión y entra en calibración. El paso de calibración
// queda activo, pero hay que pedirlo con Reveal.
func (s *Session) Start() error {
	if err := s.expireIfDue(); err != nil {
		return err
	}
	if _, err := s.machine.Apply(fsm.EventStart); err != nil {
		return err
	}
	s.markRevealed()
	return nil
}

// Reveal devuelve el paso ACTIVO, y sólo ese.
//
// Es idempotente: llamarlo varias veces devuelve el mismo paso y no reinicia
// la ventana de reacción. No avanza el guion; sólo Submit lo hace.
func (s *Session) Reveal() (RevealedStep, error) {
	if err := s.expireIfDue(); err != nil {
		return RevealedStep{}, err
	}
	switch s.machine.State() {
	case fsm.StateCalibrating, fsm.StateChallengeActive:
	default:
		return RevealedStep{}, fmt.Errorf("%w: estado %s", ErrNothingToReveal, s.machine.State())
	}
	step, ok := s.script.Current()
	if !ok {
		return RevealedStep{}, ErrNothingToReveal
	}
	return revealed(step), nil
}

// Submit valida la respuesta al paso activo en el instante actual del reloj.
func (s *Session) Submit() (StepVerdict, error) {
	return s.SubmitAt(s.clk.Now())
}

// SubmitAt valida la respuesta al paso activo con una marca de tiempo
// explícita, la del analizador que observó la señal. Es la forma que usa
// /bench para reproducir una traza con sus tiempos originales.
//
// Reglas temporales (punto 3 del diseño):
//   - Antes de Window.Min → ReasonResponseTooFast. Una respuesta más rápida
//     que el mínimo humano plausible es tan sospechosa como un timeout, y
//     además no es reintentable.
//   - Después de Window.Max → ReasonResponseTimeout.
//
// Un paso rechazado cierra la fase de retos y pasa a evaluando: se falla en
// cerrado, sin dejar que el atacante siga probando pasos.
func (s *Session) SubmitAt(observedAt time.Time) (StepVerdict, error) {
	if err := s.expireIfDue(); err != nil {
		return StepVerdict{}, err
	}
	state := s.machine.State()
	if state != fsm.StateCalibrating && state != fsm.StateChallengeActive {
		return StepVerdict{}, fmt.Errorf("session: no se admiten respuestas en estado %s: %w",
			state, fsm.ErrInvalidTransition)
	}
	step, ok := s.script.Current()
	if !ok {
		return StepVerdict{}, ErrNothingToReveal
	}

	elapsed := observedAt.Sub(s.revealedAt)
	verdict := StepVerdict{StepID: step.ID, Elapsed: elapsed}

	switch {
	case elapsed < step.Window.Min:
		verdict.Reason = ReasonResponseTooFast
	case elapsed > step.Window.Max:
		verdict.Reason = ReasonResponseTimeout
	default:
		verdict.Accepted = true
	}

	if !verdict.Accepted {
		s.record(verdict.Reason)
		if _, err := s.machine.Apply(fsm.EventStepRejected); err != nil {
			return verdict, err
		}
		return verdict, nil
	}

	if err := s.advance(state); err != nil {
		return verdict, err
	}
	return verdict, nil
}

// Tick adelanta el tiempo lógico sin respuesta del usuario: expira la sesión
// o da por vencido el paso activo si se pasó de plazo.
//
// Devuelve nil, nil si no hubo cambio. El orquestador lo llama desde su
// temporizador; es la vía por la que un usuario que simplemente deja de
// responder acaba en timeout.
func (s *Session) Tick() (*StepVerdict, error) {
	if err := s.expireIfDue(); err != nil {
		return nil, err
	}
	state := s.machine.State()
	if state != fsm.StateCalibrating && state != fsm.StateChallengeActive {
		return nil, nil
	}
	step, ok := s.script.Current()
	if !ok {
		return nil, nil
	}
	elapsed := s.clk.Now().Sub(s.revealedAt)
	if elapsed <= step.Window.Max {
		return nil, nil
	}

	verdict := &StepVerdict{
		StepID:  step.ID,
		Reason:  ReasonResponseTimeout,
		Elapsed: elapsed,
	}
	s.record(verdict.Reason)
	if _, err := s.machine.Apply(fsm.EventStepRejected); err != nil {
		return verdict, err
	}
	return verdict, nil
}

// Expire agota la sesión explícitamente. Es un error en un estado terminal:
// expirar una sesión ya resuelta reescribiría un veredicto final.
func (s *Session) Expire() error {
	if _, err := s.machine.Apply(fsm.EventExpired); err != nil {
		return err
	}
	s.record(ReasonSessionExpired)
	return nil
}

// Resolve cierra la sesión aplicando el veredicto de la fusión, endurecido
// por lo ocurrido durante los retos.
func (s *Session) Resolve(v Verdict) (Resolution, error) {
	if err := s.expireIfDue(); err != nil {
		return Resolution{}, err
	}
	if _, err := s.machine.Apply(fsm.EventResolved); err != nil {
		return Resolution{}, err
	}

	outcome, reason := s.decide(v)
	s.outcome = outcome
	if reason != ReasonNone && (len(s.reasons) == 0 || s.reasons[len(s.reasons)-1] != reason) {
		s.record(reason)
	}
	res := Resolution{
		Outcome:    outcome,
		Reason:     reason,
		Reasons:    s.Reasons(),
		ResolvedAt: s.clk.Now(),
	}
	s.resolution = &res
	return res, nil
}

// Abort cierra la sesión por una causa ajena al usuario: se ha caído una
// pieza de la infraestructura y la sesión no puede continuar.
//
// No es lo mismo que Expire. Expirar significa "se acabó el tiempo"; abortar
// significa "esto no ha llegado a evaluarse". El motivo se conserva tal cual
// en la resolución para que la auditoría no confunda una caída del analizador
// con un usuario lento.
//
// Un motivo blando resuelve en retry: la sesión se repite desde cero, con
// ticket y guion nuevos. Un motivo duro resuelve en fail, porque el núcleo
// endurece pero nunca ablanda.
func (s *Session) Abort(reason Reason) (Resolution, error) {
	if _, err := s.machine.Apply(fsm.EventAborted); err != nil {
		return Resolution{}, err
	}
	s.record(reason)

	outcome := OutcomeRetry
	if reason.Severity() == SeverityHard {
		outcome = OutcomeFail
	}
	// Un motivo duro anterior manda igualmente.
	for _, r := range s.reasons {
		if r.Severity() == SeverityHard {
			outcome = OutcomeFail
			reason = r
			break
		}
	}

	s.outcome = outcome
	res := Resolution{
		Outcome:    outcome,
		Reason:     reason,
		Reasons:    s.Reasons(),
		ResolvedAt: s.clk.Now(),
	}
	s.resolution = &res
	return res, nil
}

// Resolution devuelve el desenlace si la sesión ya está resuelta.
func (s *Session) Resolution() (Resolution, bool) {
	if s.resolution == nil {
		return Resolution{}, false
	}
	return *s.resolution, true
}

// decide aplica la política de resolución del núcleo:
//
//  1. Un motivo DURO registrado manda sobre todo: fail. No se reintenta lo
//     que huele a ataque; un reintento es una tirada más para el atacante.
//  2. Un fail de la fusión se respeta tal cual: el núcleo endurece, nunca
//     ablanda.
//  3. Un motivo BLANDO degrada a retry.
//  4. Sin incidencias, manda la fusión. Si no propuso nada, retry: la
//     ausencia de veredicto no es un aprobado.
func (s *Session) decide(v Verdict) (Outcome, Reason) {
	for _, r := range s.reasons {
		if r.Severity() == SeverityHard {
			return OutcomeFail, r
		}
	}
	if v.Reason.Severity() == SeverityHard {
		return OutcomeFail, v.Reason
	}
	if v.Outcome == OutcomeFail {
		return OutcomeFail, firstNonEmpty(v.Reason, ReasonSignalsIndicateSpoof)
	}
	for _, r := range s.reasons {
		if r.Severity() == SeveritySoft {
			return OutcomeRetry, r
		}
	}
	switch v.Outcome {
	case OutcomePass:
		return OutcomePass, firstNonEmpty(v.Reason, ReasonPassed)
	case OutcomeRetry:
		return OutcomeRetry, firstNonEmpty(v.Reason, ReasonQualityInsufficient)
	default:
		return OutcomeRetry, firstNonEmpty(v.Reason, ReasonQualityInsufficient)
	}
}

// advance acepta el paso actual y activa el siguiente.
func (s *Session) advance(from fsm.State) error {
	hasNext := s.script.Advance()

	if from == fsm.StateCalibrating {
		if !hasNext {
			return ErrEmptyScript
		}
		if _, err := s.machine.Apply(fsm.EventCalibrated); err != nil {
			return err
		}
		s.markRevealed()
		return nil
	}

	if !hasNext {
		_, err := s.machine.Apply(fsm.EventChallengesDone)
		return err
	}
	if _, err := s.machine.Apply(fsm.EventStepAdvanced); err != nil {
		return err
	}
	s.markRevealed()
	return nil
}

// expireIfDue expira la sesión si se agotó el presupuesto. Deja el estado en
// expirada y devuelve ErrSessionExpired; la operación en curso no se ejecuta.
func (s *Session) expireIfDue() error {
	if s.machine.State().Terminal() {
		if s.machine.State() == fsm.StateExpired {
			return ErrSessionExpired
		}
		return nil
	}
	if s.clk.Now().Before(s.deadline) {
		return nil
	}
	if _, err := s.machine.Apply(fsm.EventExpired); err != nil {
		return err
	}
	s.record(ReasonSessionExpired)
	return ErrSessionExpired
}

// markRevealed arranca la ventana de reacción del paso que acaba de
// activarse. Reveal no la toca: el reloj corre desde que el paso está
// disponible, lo pida el cliente o no.
func (s *Session) markRevealed() {
	s.revealedAt = s.clk.Now()
	s.revealed++
}

func (s *Session) record(r Reason) {
	if r == ReasonNone {
		return
	}
	s.reasons = append(s.reasons, r)
}

// revealed proyecta un paso del guion al tipo que sí puede salir del núcleo.
func revealed(step challenge.Step) RevealedStep {
	out := RevealedStep{
		ID:       step.ID,
		Kind:     step.Kind,
		Pose:     step.Pose,
		Gaze:     step.Gaze,
		GazeFrom: step.GazeFrom,
		Hold:     step.Hold,
		Deadline: step.Window.Max,
	}
	if len(step.Flash) > 0 {
		out.Flash = make([]challenge.FlashSegment, len(step.Flash))
		copy(out.Flash, step.Flash)
	}
	return out
}

func firstNonEmpty(r, fallback Reason) Reason {
	if r != ReasonNone {
		return r
	}
	return fallback
}
