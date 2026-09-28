package session

import "fmt"

// Reason es un código de motivo estable. Viaja a la auditoría y a /bench; al
// cliente sólo llega, como mucho, una versión genérica (CLAUDE.md §6).
type Reason string

// Motivos.
const (
	// ReasonNone es la ausencia de motivo.
	ReasonNone Reason = ""

	// ReasonResponseTooFast: la respuesta llegó antes del mínimo humano
	// plausible. Nadie reacciona tan rápido: hay automatización o
	// reproducción. Es un motivo DURO.
	ReasonResponseTooFast Reason = "response_too_fast"

	// ReasonResponseTimeout: la respuesta llegó tarde o no llegó. Puede ser
	// un ataque o una persona con mala conexión, mala cámara o dudas. Es un
	// motivo BLANDO.
	ReasonResponseTimeout Reason = "response_timeout"

	// ReasonSessionExpired: se agotó el presupuesto total de la sesión.
	// Motivo BLANDO.
	ReasonSessionExpired Reason = "session_expired"

	// ReasonQualityInsufficient: no hubo señal utilizable suficiente para
	// decidir. Motivo BLANDO.
	ReasonQualityInsufficient Reason = "quality_insufficient"

	// ReasonSignalsIndicateSpoof: la fusión concluyó ataque de presentación.
	// Motivo DURO.
	ReasonSignalsIndicateSpoof Reason = "signals_indicate_spoof"

	// ReasonAnalyzerUnavailable: el analizador dejó de responder a mitad de
	// sesión. No es culpa del usuario ni indicio de ataque: la sesión se
	// tira y se repite desde cero, con ticket y guion nuevos. Motivo BLANDO.
	ReasonAnalyzerUnavailable Reason = "analyzer_unavailable"

	// ReasonPassed: sin incidencias.
	ReasonPassed Reason = "passed"
)

// Severity clasifica un motivo.
type Severity uint8

// Severidades.
const (
	// SeverityNone no penaliza.
	SeverityNone Severity = iota
	// SeveritySoft admite reintento: la sesión pudo fallar por el entorno.
	SeveritySoft
	// SeverityHard no admite reintento: hay indicio de ataque. Reintentar
	// sólo le daría al atacante otra tirada.
	SeverityHard
)

// String implementa fmt.Stringer.
func (s Severity) String() string {
	switch s {
	case SeverityNone:
		return "none"
	case SeveritySoft:
		return "soft"
	case SeverityHard:
		return "hard"
	default:
		return fmt.Sprintf("severity(%d)", uint8(s))
	}
}

// Severity clasifica el motivo. Un motivo desconocido se trata como blando:
// el núcleo no inventa acusaciones a partir de códigos que no reconoce.
func (r Reason) Severity() Severity {
	switch r {
	case ReasonNone, ReasonPassed:
		return SeverityNone
	case ReasonResponseTooFast, ReasonSignalsIndicateSpoof:
		return SeverityHard
	default:
		return SeveritySoft
	}
}

// Outcome es el desenlace de una sesión resuelta.
type Outcome uint8

// Desenlaces.
const (
	// OutcomeUnspecified: la sesión no está resuelta.
	OutcomeUnspecified Outcome = iota
	// OutcomePass: persona real presente.
	OutcomePass
	// OutcomeFail: ataque o fallo no reintentable.
	OutcomeFail
	// OutcomeRetry: no concluyente, el usuario puede volver a intentarlo.
	OutcomeRetry
)

// String implementa fmt.Stringer.
func (o Outcome) String() string {
	switch o {
	case OutcomePass:
		return "pass"
	case OutcomeFail:
		return "fail"
	case OutcomeRetry:
		return "retry"
	case OutcomeUnspecified:
		return "unspecified"
	default:
		return fmt.Sprintf("outcome(%d)", uint8(o))
	}
}
