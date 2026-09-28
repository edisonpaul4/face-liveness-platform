// Package wsproto implementa el protocolo WebSocket cliente ⇄ gateway (v1).
//
// Contrato: /proto/ws/v1/client_protocol.schema.json y su README.
//
// Dos canales sobre la misma conexión: mensajes de control en texto (JSON) y
// frames de cámara en binario con cabecera de 24 bytes.
//
// INVARIANTE (CLAUDE.md §6): ningún mensaje saliente puede contener el guion
// de retos ni permitir inferirlo. Los tipos de este paquete están definidos
// para que no quepa: ServerChallenge lleva UN reto, sin índice, sin total,
// sin semilla y sin la ventana mínima de reacción.
package wsproto

import (
	"encoding/json"
	"fmt"
)

// Type es el discriminador de mensaje.
type Type string

// Tipos de mensaje.
const (
	TypeClientHello     Type = "client_hello"
	TypeClientTelemetry Type = "client_telemetry"
	TypeClientAbort     Type = "client_abort"

	TypeServerHello          Type = "server_hello"
	TypeServerChallenge      Type = "server_challenge"
	TypeServerChallengeEnd   Type = "server_challenge_end"
	TypeServerCaptureControl Type = "server_capture_control"
	TypeServerResult         Type = "server_result"
	TypeServerError          Type = "server_error"
)

// ProtocolVersion es la única versión soportada.
const ProtocolVersion = 1

// Envelope sirve para descubrir el tipo antes de decodificar del todo.
type Envelope struct {
	Type Type `json:"type"`
}

// --- cliente → servidor -----------------------------------------------------

// Capabilities son las capacidades declaradas por el cliente. Son promesas,
// no verdades: el cliente es hostil por definición.
type Capabilities struct {
	Encodings []string `json:"encodings,omitempty"`
	MaxFPS    float64  `json:"max_fps,omitempty"`
	MaxWidth  int      `json:"max_width,omitempty"`
	MaxHeight int      `json:"max_height,omitempty"`
	// Flash indica si el cliente puede pintar la pantalla con la secuencia de
	// colores impuesta.
	Flash bool `json:"flash,omitempty"`
}

// ClientHello abre la sesión.
type ClientHello struct {
	Type            Type          `json:"type"`
	ProtocolVersion int           `json:"protocol_version"`
	SessionToken    string        `json:"session_token"`
	Capabilities    *Capabilities `json:"capabilities,omitempty"`
}

// ClientTelemetry son pistas de captura. NO es un reporte de cumplimiento de
// reto: el cliente nunca dice haber superado nada.
type ClientTelemetry struct {
	Type          Type    `json:"type"`
	DroppedFrames int     `json:"dropped_frames,omitempty"`
	ActualFPS     float64 `json:"actual_fps,omitempty"`
	CameraState   string  `json:"camera_state,omitempty"`
}

// ClientAbort abandona la sesión.
type ClientAbort struct {
	Type   Type   `json:"type"`
	Reason string `json:"reason"`
}

// --- servidor → cliente -----------------------------------------------------

// CaptureParams son los parámetros de captura impuestos por el servidor.
type CaptureParams struct {
	Encoding string  `json:"encoding"`
	FPS      float64 `json:"fps"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	Quality  float64 `json:"quality,omitempty"`
}

// ServerHello confirma la sesión. No incluye nada del guion: ni cuántos retos
// hay, ni de qué tipo.
type ServerHello struct {
	Type              Type          `json:"type"`
	SessionID         string        `json:"session_id"`
	Capture           CaptureParams `json:"capture"`
	SessionDeadlineMS int64         `json:"session_deadline_ms,omitempty"`
}

// ServerChallenge revela UN reto, el activo.
//
// Prohibido añadir aquí: índice, total, semilla, siguiente reto o la ventana
// mínima de reacción (CLAUDE.md §6).
type ServerChallenge struct {
	Type        Type            `json:"type"`
	ChallengeID string          `json:"challenge_id"`
	Kind        string          `json:"kind"`
	Params      json.RawMessage `json:"params,omitempty"`
	// DeadlineMS es el plazo máximo. El mínimo NUNCA viaja.
	DeadlineMS int64  `json:"deadline_ms"`
	PromptKey  string `json:"prompt_key,omitempty"`
}

// CalibrationParams son los parámetros de un paso de calibración.
type CalibrationParams struct {
	HoldMS int64 `json:"hold_ms"`
}

// PoseParams son los parámetros de un reto de pose.
type PoseParams struct {
	Action string `json:"action"`
}

// FlashSegment es un color mantenido durante una duración.
type FlashSegment struct {
	Color      string `json:"color"`
	DurationMS int64  `json:"duration_ms"`
}

// GazeParams son los parámetros de un reto de mirada.
//
// TargetX y TargetY son fracciones del viewport: (0,0) arriba a la izquierda,
// (1,1) abajo a la derecha. El cliente pinta ahí el objetivo.
//
// Deliberadamente NO lleva: cuántos objetivos vienen después, dónde estarán,
// ni con qué margen se evalúa este. El punto exacto lleva jitter, así que
// tampoco se puede aprender de una tanda de sesiones (CLAUDE.md §6).
type GazeParams struct {
	// FromX/FromY es dónde aparece el punto primero, y DwellMS cuánto se
	// queda ahí antes de saltar a TargetX/TargetY.
	//
	// Los dos puntos viajan porque el cliente tiene que pintarlos, y los dos
	// son de ESTE paso: se materializan al emitirlo y no dicen nada de los
	// siguientes. Sigue sin viajar cuántos objetivos vendrán después, ni con
	// qué margen se evalúa, ni el mínimo de reacción (§6).
	FromX   float64 `json:"from_x"`
	FromY   float64 `json:"from_y"`
	DwellMS int64   `json:"dwell_ms"`
	TargetX float64 `json:"target_x"`
	TargetY float64 `json:"target_y"`
}

// FlashParams son los parámetros de un reto de destello.
type FlashParams struct {
	Sequence []FlashSegment `json:"sequence"`
}

// SignalReport es lo que midió UN detector, para depuración.
//
// ⚠ Esto NO puede viajar en producción. La regla de §6 es que al cliente se le
// dice `try_again` y nada más: contarle a un atacante qué detector le pilló y
// por cuánto es entregarle el bucle de realimentación que necesita para
// afinar el ataque. Va detrás de una bandera insegura y apagada por defecto,
// y existe para que quien hace el pentest vea contra qué está peleando.
type SignalReport struct {
	Signal   string  `json:"signal"`
	Value    float64 `json:"value"`
	Weight   float64 `json:"weight"`
	Floor    float64 `json:"floor"`
	Passed   bool    `json:"passed"`
	Measured bool    `json:"measured"`
}

// VerdictExplanation acompaña al resultado en modo depuración.
type VerdictExplanation struct {
	Score          float64        `json:"score"`
	ProfileVersion string         `json:"profile_version"`
	Signals        []SignalReport `json:"signals"`
	Reasons        []string       `json:"reasons"`
}

// ServerChallengeEnd cierra el reto activo.
//
// No dice si se superó: eso sólo se sabe en el veredicto final. Su tamaño es
// constante a propósito, para no filtrar el resultado por canal lateral.
type ServerChallengeEnd struct {
	Type        Type   `json:"type"`
	ChallengeID string `json:"challenge_id"`
}

// ServerCaptureControl ajusta el caudal de captura.
type ServerCaptureControl struct {
	Type      Type    `json:"type"`
	Action    string  `json:"action"`
	TargetFPS float64 `json:"target_fps,omitempty"`
}

// ServerResult es el único mensaje con semántica de decisión.
type ServerResult struct {
	Type      Type   `json:"type"`
	SessionID string `json:"session_id"`
	Decision  string `json:"decision"`
	ReasonKey string `json:"reason_key,omitempty"`
	// Explanation sólo se rellena en modo depuración inseguro.
	Explanation *VerdictExplanation `json:"explanation,omitempty"`
	ReferenceID string              `json:"reference_id,omitempty"`
}

// Decisiones posibles de ServerResult.
const (
	DecisionLive         = "live"
	DecisionSpoof        = "spoof"
	DecisionInconclusive = "inconclusive"
)

// ServerError comunica un fallo de protocolo o de servicio.
type ServerError struct {
	Type    Type      `json:"type"`
	Code    ErrorCode `json:"code"`
	Message string    `json:"message,omitempty"`
}

// --- ayudas -----------------------------------------------------------------

// MustParams serializa los parámetros de un reto. El error es imposible con
// los tipos de este paquete; si ocurriera, devuelve params vacíos antes que
// romper la sesión.
func MustParams(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// PeekType devuelve el tipo de un mensaje de control sin decodificarlo entero.
func PeekType(data []byte) (Type, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return "", fmt.Errorf("wsproto: JSON inválido: %w", err)
	}
	if env.Type == "" {
		return "", fmt.Errorf("wsproto: mensaje sin campo type")
	}
	return env.Type, nil
}
