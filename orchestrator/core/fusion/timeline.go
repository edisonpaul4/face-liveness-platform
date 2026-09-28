package fusion

import "math"

// WindowKind distingue los dos tipos de ventana que analiza Python.
type WindowKind string

// Tipos de ventana.
const (
	WindowPose  WindowKind = "pose"
	WindowFlash WindowKind = "flash"
	WindowGaze  WindowKind = "gaze"
	// WindowTexture no es un reto: es el resumen de sesión del clasificador
	// pasivo, que mira frames sueltos sin pedir nada al sujeto. Se modela
	// como ventana para que pase por la misma maquinaria que el resto —misma
	// agregación, mismo trato a lo no medido— en vez de tener un camino
	// propio que habría que recordar mantener.
	WindowTexture WindowKind = "texture"
	// WindowPulse tampoco es un reto: es el pulso sanguíneo medido sobre el
	// tramo más largo de la sesión sin destellos. Al sujeto no se le pide
	// nada; sólo que exista.
	WindowPulse WindowKind = "rppg"
)

// Window es el resultado de una ventana, tal y como lo emite el analizador.
//
// Submetrics usa punteros porque la diferencia importa: nil es "no se pudo
// medir" y 0 es "medido y malo". Confundirlos convierte a quien no llegó a
// girar la cabeza en un atacante.
type Window struct {
	ID   string     `json:"window_id"`
	Kind WindowKind `json:"kind"`
	// Score es el resumen que calculó el analizador. La fusión no lo usa: se
	// queda con las sub-métricas, que son las que se pueden explicar.
	Score      float64             `json:"score"`
	Submetrics map[string]*float64 `json:"submetrics"`
	Raw        map[string]float64  `json:"raw,omitempty"`
	// QualitySufficient falso significa que no se pudo medir. Lleva a
	// reintentar, nunca a rechazar.
	QualitySufficient bool   `json:"quality_sufficient"`
	QualityReason     string `json:"quality_reason,omitempty"`
}

// TemporalKind clasifica cómo llegó la respuesta a un paso.
type TemporalKind string

// Desenlaces temporales de un paso.
const (
	TemporalAccepted TemporalKind = "accepted"
	// TemporalTooFast: antes del mínimo humano plausible.
	TemporalTooFast TemporalKind = "too_fast"
	// TemporalTimeout: fuera de plazo.
	TemporalTimeout TemporalKind = "timeout"
)

// TemporalEvent es cómo respondió el usuario a un paso.
//
// Viene de la máquina de estados, no del analizador: se mide con el reloj del
// servidor, así que vale aunque la imagen no sirva para nada.
type TemporalEvent struct {
	StepID    string       `json:"step_id"`
	Kind      TemporalKind `json:"kind"`
	ElapsedMS int64        `json:"elapsed_ms"`
}

// CaptureStats resume si los frames servían para medir.
type CaptureStats struct {
	Frames                  int     `json:"frames"`
	FramesWithFace          int     `json:"frames_with_face"`
	MeanSharpness           float64 `json:"mean_sharpness"`
	MeanBrightness          float64 `json:"mean_brightness"`
	MeanHighlightSaturation float64 `json:"mean_highlight_saturation"`
}

// Timeline es todo lo que se sabe de una sesión al cerrarla.
type Timeline struct {
	SessionID string          `json:"session_id"`
	Windows   []Window        `json:"windows"`
	Temporal  []TemporalEvent `json:"temporal"`
	Capture   CaptureStats    `json:"capture"`

	// ChallengesCompleted indica si la sesión llegó al final de su guion.
	ChallengesCompleted bool `json:"challenges_completed"`
	// InfrastructureFailure marca que se cayó algo nuestro. No es culpa del
	// usuario y no se le puede cobrar como rechazo.
	InfrastructureFailure bool   `json:"infrastructure_failure"`
	InfrastructureDetail  string `json:"infrastructure_detail,omitempty"`
}

// submetricSignals ata cada sub-métrica del analizador a su señal de fusión.
var submetricSignals = map[WindowKind]map[string]Signal{
	WindowPose: {
		"compliance": SignalPoseCompliance,
		"continuity": SignalPoseContinuity,
		"parallax":   SignalPoseParallax,
		"identity":   SignalPoseIdentity,
	},
	WindowFlash: {
		"correlation":    SignalFlashCorrelation,
		"gradient_3d":    SignalFlashGradient3D,
		"screen_absence": SignalFlashScreenAbsence,
	},
	WindowGaze: {
		"response": SignalGazeResponse,
	},
	WindowTexture: {
		"v2":   SignalTexturePADV2,
		"v1se": SignalTexturePADV1SE,
	},
	WindowPulse: {
		"snr": SignalPulse,
	},
}

// collectWindowSignals reúne las sub-métricas medidas, por señal.
//
// Cuando una señal aparece en varias ventanas se queda la PEOR. Es lo
// conservador: si en un reto el destello no produjo relieve, que en otro sí lo
// produjera no lo desmiente. Un atacante sólo necesita que una ventana le
// salga bien.
func collectWindowSignals(windows []Window) map[Signal][]float64 {
	values := map[Signal][]float64{}
	for _, window := range windows {
		if !window.QualitySufficient {
			// Una ventana que no se pudo medir no aporta señal. Tomar sus
			// ceros sería inventarse evidencia.
			continue
		}
		mapping, ok := submetricSignals[window.Kind]
		if !ok {
			continue
		}
		for name, value := range window.Submetrics {
			signal, ok := mapping[name]
			if !ok || value == nil {
				continue
			}
			values[signal] = append(values[signal], *value)
		}
	}
	return values
}

// worst devuelve el menor de los valores.
func worst(values []float64) float64 {
	out := math.Inf(1)
	for _, v := range values {
		out = math.Min(out, v)
	}
	return out
}

// temporalPlausibility convierte los desenlaces temporales en una señal.
//
// Devuelve también si hubo una respuesta imposiblemente rápida, que además de
// bajar la señal veta por sí sola.
func temporalPlausibility(events []TemporalEvent) (value float64, tooFast bool, timeouts int, ok bool) {
	if len(events) == 0 {
		return 0, false, 0, false
	}

	accepted := 0
	for _, event := range events {
		switch event.Kind {
		case TemporalAccepted:
			accepted++
		case TemporalTooFast:
			tooFast = true
		case TemporalTimeout:
			timeouts++
		}
	}

	if tooFast {
		// Nadie reacciona antes del mínimo humano. No es un usuario lento ni
		// rápido: es que no hay usuario.
		return 0, true, timeouts, true
	}
	return float64(accepted) / float64(len(events)), false, timeouts, true
}
