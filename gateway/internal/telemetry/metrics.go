// Package telemetry centraliza los contadores del gateway.
//
// Nunca registra payloads de frames ni datos biométricos: sólo cuentas,
// identificadores de sesión y latencias.
package telemetry

import (
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
)

// Metrics son los contadores del proceso. Seguro para uso concurrente.
type Metrics struct {
	SessionsIssued    atomic.Int64
	SessionsStarted   atomic.Int64
	SessionsCompleted atomic.Int64
	SessionsExpired   atomic.Int64
	// SessionsRejectedReuse cuenta los intentos de reutilizar un sessionId.
	SessionsRejectedReuse atomic.Int64

	FramesReceived atomic.Int64
	FramesAnalyzed atomic.Int64

	// FramesDroppedBackpressure son los frames intermedios descartados
	// porque el cliente enviaba más rápido de lo que se procesa. Es el
	// contador que dice si el cliente va sobrado de caudal.
	FramesDroppedBackpressure atomic.Int64
	// FramesDroppedRateLimit son los que superaron los FPS máximos.
	FramesDroppedRateLimit atomic.Int64
	// FramesDroppedOutOfOrder son duplicados o llegados fuera de secuencia.
	FramesDroppedOutOfOrder atomic.Int64
	// FramesDroppedUnparsable son los que no cumplen el formato de cabecera.
	FramesDroppedUnparsable atomic.Int64

	ControlMessages    atomic.Int64
	ProtocolViolations atomic.Int64
	FramesTooLarge     atomic.Int64
	// ClientClockImplausible cuenta los frames cuyo sello de cliente tenía
	// una deriva inverosímil. El sello se ignora; el frame se procesa igual.
	ClientClockImplausible atomic.Int64
	WriteQueueOverflow     atomic.Int64

	// --- bus de análisis ---

	// FramesPublished son los frames enviados al worker.
	FramesPublished atomic.Int64
	// FramesTimedOut son los frames que no produjeron medidas a tiempo. Se
	// descartan; la sesión sigue.
	FramesTimedOut atomic.Int64
	// FeaturesReceived son las medidas recibidas y aprovechadas.
	FeaturesReceived atomic.Int64
	// FeaturesDropped son las medidas tiradas porque la sesión iba por
	// detrás. Nunca se acumulan.
	FeaturesDropped atomic.Int64
	// FeaturesLate son las medidas de un frame que ya se había dado por
	// perdido.
	FeaturesLate atomic.Int64

	// WindowScores son las medidas de ventana recibidas: paralaje, gradiente
	// 3D y compañía. Si esto se queda a cero con retos cerrados, el
	// analizador no está puntuando ventanas y la fusión decide con la mitad
	// de las señales sin enterarse.
	WindowScores atomic.Int64
	// WindowsUnanswered son las ventanas pedidas que no volvieron a tiempo.
	WindowsUnanswered atomic.Int64

	// LeasesAcquired son las sesiones asignadas a un worker.
	LeasesAcquired atomic.Int64
	// LeasesFailed son las sesiones que no encontraron worker.
	LeasesFailed atomic.Int64
	// LeasesLost son los workers que se callaron a mitad de sesión.
	LeasesLost atomic.Int64
	// SessionsAbortedInfra son las sesiones abortadas por caída de
	// infraestructura.
	SessionsAbortedInfra atomic.Int64
}

// New crea un juego de contadores.
func New() *Metrics { return &Metrics{} }

// Snapshot es una foto de los contadores.
type Snapshot map[string]int64

// Snapshot toma una foto coherente en el orden de lectura, sin bloquear.
func (m *Metrics) Snapshot() Snapshot {
	return Snapshot{
		"sessions_issued":             m.SessionsIssued.Load(),
		"sessions_started":            m.SessionsStarted.Load(),
		"sessions_completed":          m.SessionsCompleted.Load(),
		"sessions_expired":            m.SessionsExpired.Load(),
		"sessions_rejected_reuse":     m.SessionsRejectedReuse.Load(),
		"frames_received":             m.FramesReceived.Load(),
		"frames_analyzed":             m.FramesAnalyzed.Load(),
		"frames_dropped_backpressure": m.FramesDroppedBackpressure.Load(),
		"frames_dropped_rate_limit":   m.FramesDroppedRateLimit.Load(),
		"frames_dropped_out_of_order": m.FramesDroppedOutOfOrder.Load(),
		"frames_dropped_unparsable":   m.FramesDroppedUnparsable.Load(),
		"control_messages":            m.ControlMessages.Load(),
		"protocol_violations":         m.ProtocolViolations.Load(),
		"frames_too_large":            m.FramesTooLarge.Load(),
		"client_clock_implausible":    m.ClientClockImplausible.Load(),
		"write_queue_overflow":        m.WriteQueueOverflow.Load(),

		"frames_published":       m.FramesPublished.Load(),
		"frames_timed_out":       m.FramesTimedOut.Load(),
		"features_received":      m.FeaturesReceived.Load(),
		"features_dropped":       m.FeaturesDropped.Load(),
		"features_late":          m.FeaturesLate.Load(),
		"window_scores":          m.WindowScores.Load(),
		"windows_unanswered":     m.WindowsUnanswered.Load(),
		"leases_acquired":        m.LeasesAcquired.Load(),
		"leases_failed":          m.LeasesFailed.Load(),
		"leases_lost":            m.LeasesLost.Load(),
		"sessions_aborted_infra": m.SessionsAbortedInfra.Load(),
	}
}

// Text renderiza los contadores en formato de exposición sencillo.
func (s Snapshot) Text() string {
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "liveness_gateway_%s %d\n", k, s[k])
	}
	return b.String()
}
