// Package retry acota cuántas veces puede repetirse una verificación.
//
// Un reintento no es gratis: cada uno le da al atacante otra tirada con un
// guion nuevo. Sin tope, la política de "en la duda, reintentar" se convierte
// en una barra libre.
//
// Cuando se agotan, la sesión no se rechaza sin más: escala a revisión
// manual. Rechazar a alguien porque su cámara es mala tres veces seguidas
// sigue siendo rechazar a alguien por su cámara.
package retry

import (
	"fmt"
	"sync"
	"time"

	"github.com/edisonpaul4/biometrics/orchestrator/core/clock"
	"github.com/edisonpaul4/biometrics/orchestrator/core/fusion"
)

// Disposition es qué hacer después de un veredicto.
type Disposition uint8

// Disposiciones.
const (
	DispositionUnknown Disposition = iota
	// DispositionFinal: no hay nada más que hacer. Aprobado o rechazado.
	DispositionFinal
	// DispositionRetry: se puede repetir, tras esperar.
	DispositionRetry
	// DispositionEscalate: se agotaron los intentos. A revisión manual.
	DispositionEscalate
)

// String implementa fmt.Stringer.
func (d Disposition) String() string {
	switch d {
	case DispositionFinal:
		return "final"
	case DispositionRetry:
		return "retry"
	case DispositionEscalate:
		return "escalate"
	case DispositionUnknown:
		return "unknown"
	default:
		return fmt.Sprintf("disposition(%d)", uint8(d))
	}
}

// Decision es qué hacer con un identificador tras un veredicto.
type Decision struct {
	Disposition Disposition    `json:"disposition"`
	Outcome     fusion.Outcome `json:"outcome"`
	// Attempt es el número de este intento, empezando en 1.
	Attempt int `json:"attempt"`
	// Remaining son los intentos que quedan.
	Remaining int `json:"remaining"`
	// RetryAfter es cuánto hay que esperar antes del siguiente.
	RetryAfter time.Duration `json:"retry_after"`
	// Reasons son los del veredicto, más los de política.
	Reasons []fusion.Reason `json:"reasons"`
}

// Store guarda los intentos por identificador.
//
// El identificador es el del SUJETO, no el de la sesión: las sesiones son de
// un solo uso, así que contarlas no acotaría nada.
type Store interface {
	// Attempts devuelve los intentos registrados y cuándo fue el último.
	Attempts(subject string) (count int, last time.Time)
	// Record apunta un intento.
	Record(subject string, at time.Time)
	// Reset borra el histórico de un identificador.
	Reset(subject string)
}

// MemoryStore es un Store en memoria.
//
// Vale para un proceso. Con varios orquestadores hace falta uno compartido
// —Redis con INCR y TTL—, o el tope se multiplica por el número de réplicas.
type MemoryStore struct {
	mu      sync.Mutex
	window  time.Duration
	entries map[string]*entry
}

type entry struct {
	attempts []time.Time
}

// NewMemoryStore crea el almacén con la ventana de caducidad dada.
func NewMemoryStore(window time.Duration) *MemoryStore {
	if window <= 0 {
		window = time.Hour
	}
	return &MemoryStore{window: window, entries: make(map[string]*entry)}
}

// Attempts implementa Store.
func (s *MemoryStore) Attempts(subject string) (int, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	found, ok := s.entries[subject]
	if !ok || len(found.attempts) == 0 {
		return 0, time.Time{}
	}
	return len(found.attempts), found.attempts[len(found.attempts)-1]
}

// Record implementa Store.
func (s *MemoryStore) Record(subject string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	found, ok := s.entries[subject]
	if !ok {
		found = &entry{}
		s.entries[subject] = found
	}

	// Los intentos caducan: quien lo intentó ayer no arrastra el tope de hoy.
	cutoff := at.Add(-s.window)
	kept := found.attempts[:0]
	for _, previous := range found.attempts {
		if previous.After(cutoff) {
			kept = append(kept, previous)
		}
	}
	found.attempts = append(kept, at)
}

// Reset implementa Store.
func (s *MemoryStore) Reset(subject string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, subject)
}

// Gate aplica la política de reintentos.
type Gate struct {
	policy fusion.RetryPolicy
	store  Store
	clk    clock.Clock
}

// NewGate crea la puerta.
func NewGate(policy fusion.RetryPolicy, store Store, clk clock.Clock) *Gate {
	if store == nil {
		store = NewMemoryStore(policy.Window)
	}
	if clk == nil {
		clk = clock.System{}
	}
	return &Gate{policy: policy, store: store, clk: clk}
}

// Evaluate decide qué hacer con el sujeto tras un veredicto.
func (g *Gate) Evaluate(subject string, result fusion.Result) Decision {
	now := g.clk.Now()
	g.store.Record(subject, now)
	attempts, _ := g.store.Attempts(subject)

	decision := Decision{
		Outcome: result.Outcome,
		Attempt: attempts,
		Reasons: append([]fusion.Reason(nil), result.Reasons...),
	}

	switch result.Outcome {
	case fusion.OutcomePass:
		// Aprobado: se limpia el histórico. Los intentos fallidos de hoy no
		// pueden penalizar la próxima verificación de quien ya demostró ser
		// quien dice.
		g.store.Reset(subject)
		decision.Disposition = DispositionFinal
		return decision

	case fusion.OutcomeReject:
		// Un rechazo no se reintenta: repetir un ataque sólo le da al
		// atacante otra tirada con un guion nuevo.
		decision.Disposition = DispositionFinal
		return decision
	}

	// Reintentar.
	decision.Remaining = max(0, g.policy.MaxAttempts-attempts)
	if decision.Remaining <= 0 {
		decision.Disposition = DispositionEscalate
		code := fusion.CodePolicyManualReview
		if !g.policy.EscalateOnExhaustion {
			decision.Disposition = DispositionFinal
			code = fusion.CodePolicyRetriesExhausted
		}
		decision.Reasons = append(decision.Reasons, fusion.Reason{
			Code:      code,
			Family:    code.Family(),
			Observed:  float64(attempts),
			Threshold: float64(g.policy.MaxAttempts),
			Detail:    "intentos agotados",
		})
		return decision
	}

	decision.Disposition = DispositionRetry
	decision.RetryAfter = g.backoff(attempts)
	return decision
}

// Attempts devuelve los intentos registrados de un sujeto.
func (g *Gate) Attempts(subject string) int {
	count, _ := g.store.Attempts(subject)
	return count
}

// Reset borra el histórico de un sujeto.
func (g *Gate) Reset(subject string) { g.store.Reset(subject) }

// backoff devuelve la espera del intento número `attempt`.
//
// Si hay menos entradas configuradas que intentos, se repite la última: el
// backoff no puede desaparecer justo cuando más falta hace.
func (g *Gate) backoff(attempt int) time.Duration {
	if len(g.policy.Backoff) == 0 {
		return 0
	}
	index := attempt - 1
	if index >= len(g.policy.Backoff) {
		index = len(g.policy.Backoff) - 1
	}
	if index < 0 {
		index = 0
	}
	return g.policy.Backoff[index]
}
