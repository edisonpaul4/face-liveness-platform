package fusion

import "fmt"

// Family agrupa los motivos por naturaleza.
//
// La familia es lo que decide el desenlace, no el motivo suelto: un problema
// de calidad nunca puede acabar en rechazo, por muy grave que sea.
type Family string

// Familias de motivo.
const (
	// FamilyQuality: no se pudo medir. Cámara mala, luz de más, rostro fuera
	// de encuadre. **Nunca es un rechazo**: se reintenta.
	FamilyQuality Family = "quality"
	// FamilyAttack: hay indicio de presentación fraudulenta.
	FamilyAttack Family = "attack"
	// FamilyTemporal: la respuesta no encaja con lo que puede hacer una
	// persona.
	FamilyTemporal Family = "temporal"
	// FamilyInfrastructure: se cayó algo nuestro. No es culpa del usuario.
	FamilyInfrastructure Family = "infrastructure"
	// FamilyPolicy: decisión de política, no de medida.
	FamilyPolicy Family = "policy"
)

// Code es un motivo estable.
//
// Estos códigos salen por la API, se guardan en Postgres y los lee el
// backoffice. Se añaden códigos nuevos; no se renombran ni se reciclan.
type Code string

// Motivos.
const (
	// --- calidad: llevan a reintentar ---

	// CodeQualityInsufficientSignal: la luz ambiente aplastó el destello, o
	// no hubo material suficiente para medir.
	CodeQualityInsufficientSignal Code = "quality_insufficient_signal"
	// CodeQualityNoFace: no se detectó rostro en suficientes frames.
	CodeQualityNoFace Code = "quality_no_face"
	// CodeQualityCapture: nitidez, exposición o encuadre insuficientes.
	CodeQualityCapture Code = "quality_capture"
	// CodeQualityNoSignals: no llegó ninguna señal medible.
	CodeQualityNoSignals Code = "quality_no_signals"
	// CodeQualityIncomplete: la sesión no llegó a completar sus retos.
	CodeQualityIncomplete Code = "quality_incomplete"

	// --- ataque: llevan a rechazo ---

	// CodeAttackFlatSurface: el destello no produjo relieve. Una foto
	// impresa responde igual en la frente que en la nariz.
	CodeAttackFlatSurface Code = "attack_flat_surface"
	// CodeAttackEmissiveSurface: indicios de pantalla — brilla sin seguir al
	// destello, reflejo concentrado, moiré, bandeo.
	CodeAttackEmissiveSurface Code = "attack_emissive_surface"
	// CodeAttackPlanarMotion: al girar, el movimiento lo explica un plano.
	CodeAttackPlanarMotion Code = "attack_planar_motion"
	// CodeAttackNoColorResponse: la piel no siguió a la secuencia de colores.
	CodeAttackNoColorResponse Code = "attack_no_color_response"
	// CodeAttackIdentityChange: el rostro cambió a mitad de sesión.
	CodeAttackIdentityChange Code = "attack_identity_change"

	// CodeAttackNoGazeResponse: la mirada no fue hacia el objetivo. Un vídeo
	// grabado puede llevar ojos que se mueven; lo que no puede es mirar al
	// punto que ha aparecido ahora en un sitio elegido al azar.
	CodeAttackNoGazeResponse Code = "attack_no_gaze_response"
	// CodeAttackFusedScoreLow: ninguna señal falló por sí sola, pero el
	// conjunto no llega.
	CodeAttackFusedScoreLow Code = "attack_fused_score_low"

	// --- temporal ---

	// CodeTemporalTooFast: la respuesta llegó antes del mínimo humano. Nadie
	// reacciona tan rápido: hay automatización o reproducción.
	CodeTemporalTooFast Code = "temporal_response_too_fast"
	// CodeTemporalTimeout: la respuesta llegó tarde o no llegó. Puede ser un
	// ataque o una persona con dudas: se reintenta.
	CodeTemporalTimeout Code = "temporal_response_timeout"
	// CodeTemporalDiscontinuity: la trayectoria pegó un salto que ningún
	// cuello puede hacer.
	CodeTemporalDiscontinuity Code = "temporal_trajectory_discontinuity"

	// --- infraestructura ---

	// CodeInfraAnalyzerUnavailable: el analizador dejó de responder.
	CodeInfraAnalyzerUnavailable Code = "infrastructure_analyzer_unavailable"

	// --- política ---

	// CodePolicyRetriesExhausted: se agotaron los intentos.
	CodePolicyRetriesExhausted Code = "policy_retries_exhausted"
	// CodePolicyManualReview: escalado a revisión manual.
	CodePolicyManualReview Code = "policy_manual_review"

	// --- aprobado ---

	// CodePassed: sin incidencias.
	CodePassed Code = "passed"
)

// codeFamilies ata cada motivo a su familia. Un motivo sin familia es un
// motivo que nadie sabe cómo tratar.
var codeFamilies = map[Code]Family{
	CodeQualityInsufficientSignal: FamilyQuality,
	CodeQualityNoFace:             FamilyQuality,
	CodeQualityCapture:            FamilyQuality,
	CodeQualityNoSignals:          FamilyQuality,
	CodeQualityIncomplete:         FamilyQuality,

	CodeAttackFlatSurface:     FamilyAttack,
	CodeAttackEmissiveSurface: FamilyAttack,
	CodeAttackPlanarMotion:    FamilyAttack,
	CodeAttackNoColorResponse: FamilyAttack,
	CodeAttackIdentityChange:  FamilyAttack,
	CodeAttackNoGazeResponse:  FamilyAttack,
	CodeAttackFusedScoreLow:   FamilyAttack,

	CodeTemporalTooFast:       FamilyTemporal,
	CodeTemporalTimeout:       FamilyTemporal,
	CodeTemporalDiscontinuity: FamilyTemporal,

	CodeInfraAnalyzerUnavailable: FamilyInfrastructure,

	CodePolicyRetriesExhausted: FamilyPolicy,
	CodePolicyManualReview:     FamilyPolicy,

	CodePassed: FamilyQuality,
}

// Family devuelve la familia del motivo.
func (c Code) Family() Family {
	if family, ok := codeFamilies[c]; ok {
		return family
	}
	// Un código desconocido se trata como calidad: en la duda se reintenta,
	// no se acusa.
	return FamilyQuality
}

// Known indica si el motivo está en la taxonomía.
func (c Code) Known() bool {
	_, ok := codeFamilies[c]
	return ok
}

// AllCodes son todos los motivos conocidos.
func AllCodes() []Code {
	out := make([]Code, 0, len(codeFamilies))
	for code := range codeFamilies {
		out = append(out, code)
	}
	return out
}

// Reason explica una parte del veredicto.
//
// Lleva la prueba, no sólo la conclusión: qué señal, qué valor y contra qué
// umbral. Un rechazo sin esto no es explicable, y sin explicación no hay
// backoffice ni gestión de excepciones posible.
type Reason struct {
	Code   Code   `json:"code"`
	Family Family `json:"family"`
	// Signal es la señal que lo provocó, si lo provocó una señal.
	Signal Signal `json:"signal,omitempty"`
	// Observed es el valor medido.
	Observed float64 `json:"observed"`
	// Threshold es el umbral contra el que se comparó.
	Threshold float64 `json:"threshold"`
	// Detail añade contexto para el backoffice. No para el usuario final.
	Detail string `json:"detail,omitempty"`
}

// String implementa fmt.Stringer.
func (r Reason) String() string {
	if r.Signal == "" {
		return string(r.Code)
	}
	return fmt.Sprintf("%s (%s=%.3f, umbral %.3f)", r.Code, r.Signal, r.Observed, r.Threshold)
}

// newReason construye un motivo con su familia ya resuelta.
func newReason(code Code, signal Signal, observed, threshold float64, detail string) Reason {
	return Reason{
		Code:      code,
		Family:    code.Family(),
		Signal:    signal,
		Observed:  observed,
		Threshold: threshold,
		Detail:    detail,
	}
}
