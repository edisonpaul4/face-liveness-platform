// Package analyzer es el puerto hacia el análisis de frames.
//
// FRONTERA (CLAUDE.md §3): el analizador MIDE, no decide. Recibe bytes de un
// frame y devuelve un vector de magnitudes con nombre. No sabe qué reto está
// activo, no conoce el protocolo WebSocket y no emite ningún booleano de
// "esto es real" ni ninguna probabilidad de ataque.
//
// La implementación definitiva vive en Python al otro lado de NATS. Aquí sólo
// está el puerto y un stub sintético para el vertical slice.
package analyzer

import (
	"context"
	"time"
)

// Request es un frame a analizar.
//
// Nótese lo que NO lleva: ni reto activo, ni estado de la sesión, ni umbrales.
// Si algún día hiciera falta meter algo de eso aquí, el diseño está mal.
type Request struct {
	SessionID string
	Seq       uint64
	Encoding  uint8
	Payload   []byte

	// ReceivedAt es el sello del reloj del SERVIDOR al recibir el frame. Es
	// el único instante que puntúa.
	ReceivedAt time.Time

	// CapturedAt es el sello del reloj del CLIENTE, sólo para alinear frames
	// entre sí. Vale la pena únicamente si ClientClockTrusted.
	CapturedAt time.Time
	// ClientClockTrusted indica si la deriva del reloj del cliente resultó
	// plausible.
	ClientClockTrusted bool
}

// Quality describe si el frame sirve para medir. Permite a Go declarar una
// sesión no concluyente; no es un juicio sobre la sesión.
type Quality struct {
	FaceDetected bool
	FaceCount    int
	Sharpness    float64
	Brightness   float64
}

// Features son las magnitudes medidas en un frame.
//
// Los nombres de Signals son snake_case, con familia como prefijo y sin
// juicio de valor (CLAUDE.md §7): pose_yaw_deg, color_response_r,
// moire_peak_ratio. Prohibidos: is_live, spoof_score, attack_detected.
type Features struct {
	Seq          uint64
	Signals      map[string]float64
	Quality      Quality
	ProcessingMS int64
	Version      string
}

// Signal devuelve una señal y si estaba presente.
func (f Features) Signal(name string) (float64, bool) {
	v, ok := f.Signals[name]
	return v, ok
}

// SignalOr devuelve una señal o un valor por defecto.
func (f Features) SignalOr(name string, def float64) float64 {
	if v, ok := f.Signals[name]; ok {
		return v
	}
	return def
}

// Analyzer mide frames.
type Analyzer interface {
	Analyze(ctx context.Context, req Request) (Features, error)
}
