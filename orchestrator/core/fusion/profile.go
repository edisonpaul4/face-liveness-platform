package fusion

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// ErrInvalidProfile marca un perfil incoherente. Se comprueba con errors.Is.
var ErrInvalidProfile = errors.New("fusion: perfil inválido")

// Floor es el suelo de una señal: por debajo, veta por sí sola.
//
// Existe además de la media ponderada porque hay señales que no se
// compensan: que el destello no produzca relieve no lo arregla haber girado
// muy bien la cabeza.
type Floor struct {
	Min  float64 `yaml:"min" json:"min"`
	Code Code    `yaml:"code" json:"code"`
}

// Thresholds son las tres bandas del veredicto.
type Thresholds struct {
	// PassMin: a partir de aquí, aprobado.
	PassMin float64 `yaml:"pass_min" json:"pass_min"`
	// RejectMax: por debajo de aquí, rechazado.
	RejectMax float64 `yaml:"reject_max" json:"reject_max"`
	// Entre los dos: reintentar. La banda intermedia es deliberada: sin ella
	// habría que decidir a cara o cruz en la zona donde no hay evidencia.
}

// QualityGate son las condiciones mínimas para que la medida signifique algo.
type QualityGate struct {
	// MinSignals: señales medibles por debajo de las cuales no se decide.
	MinSignals int `yaml:"min_signals" json:"min_signals"`
	// MinCaptureQuality: calidad de captura mínima.
	MinCaptureQuality float64 `yaml:"min_capture_quality" json:"min_capture_quality"`
	// RequireCompletedChallenges: si la sesión tiene que haber terminado sus
	// retos para poder aprobarse.
	RequireCompletedChallenges bool `yaml:"require_completed_challenges" json:"require_completed_challenges"`
}

// CaptureGate traduce las estadísticas de captura a una señal en [0,1].
//
// Las referencias van aquí, en configuración, y no en el código: qué se
// considera "suficientemente nítido" depende de las cámaras que use la gente,
// y eso cambia sin que cambie el binario.
type CaptureGate struct {
	// SharpnessReference es la varianza del laplaciano a partir de la cual la
	// nitidez se considera plena.
	SharpnessReference float64 `yaml:"sharpness_reference" json:"sharpness_reference"`
	// MinBrightness y MaxBrightness acotan la exposición aceptable.
	MinBrightness float64 `yaml:"min_brightness" json:"min_brightness"`
	MaxBrightness float64 `yaml:"max_brightness" json:"max_brightness"`
	// MaxHighlightSaturation es la fracción de quemados que ya estropea la
	// medida.
	MaxHighlightSaturation float64 `yaml:"max_highlight_saturation" json:"max_highlight_saturation"`
	// MinFaceCoverage es la fracción mínima de frames con rostro.
	MinFaceCoverage float64 `yaml:"min_face_coverage" json:"min_face_coverage"`
}

// RetryPolicy acota cuántas veces puede reintentar un mismo identificador.
type RetryPolicy struct {
	// MaxAttempts es el total de intentos, incluido el primero.
	MaxAttempts int `yaml:"max_attempts" json:"max_attempts"`
	// Backoff es la espera antes de cada reintento. Si hay menos entradas
	// que intentos, se repite la última.
	Backoff []time.Duration `yaml:"backoff" json:"backoff"`
	// Window es el tiempo tras el cual los intentos caducan.
	Window time.Duration `yaml:"window" json:"window"`
	// EscalateOnExhaustion manda a revisión manual al agotar los intentos,
	// en vez de rechazar sin más.
	EscalateOnExhaustion bool `yaml:"escalate_on_exhaustion" json:"escalate_on_exhaustion"`
}

// Profile es la configuración del motor de decisión.
//
// Vive en YAML, fuera del binario y versionada, porque mover un umbral es
// una operación de producto que ocurre a menudo y no puede costar un
// despliegue. La versión viaja con cada resultado: sin ella, un veredicto del
// mes pasado no se puede reproducir ni explicar.
type Profile struct {
	Version     string `yaml:"version" json:"version"`
	Description string `yaml:"description" json:"description,omitempty"`

	// Weights son los pesos de la media ponderada.
	//
	// Se ponen A MANO y no salen de ningún modelo. Es deliberado: hasta
	// entender qué aporta cada señal, un peso aprendido sería un número que
	// nadie sabe defender delante de un rechazo.
	Weights map[Signal]float64 `yaml:"weights" json:"weights"`

	// Floors son los vetos por señal.
	Floors map[Signal]Floor `yaml:"floors" json:"floors"`

	Thresholds Thresholds  `yaml:"thresholds" json:"thresholds"`
	Quality    QualityGate `yaml:"quality" json:"quality"`
	Capture    CaptureGate `yaml:"capture" json:"capture"`
	Retry      RetryPolicy `yaml:"retry" json:"retry"`

	// checksum identifica los bytes exactos que produjeron una decisión.
	// La versión la escribe una persona y puede olvidarse de subirla; el
	// resumen, no.
	checksum string
}

// Checksum es el resumen de los bytes del perfil.
func (p *Profile) Checksum() string { return p.checksum }

// Weight devuelve el peso de una señal, o cero si no participa.
func (p *Profile) Weight(signal Signal) float64 { return p.Weights[signal] }

// FloorFor devuelve el suelo de una señal, si tiene.
func (p *Profile) FloorFor(signal Signal) (Floor, bool) {
	floor, ok := p.Floors[signal]
	return floor, ok
}

// Parse lee un perfil desde YAML y lo valida.
//
// Es pura: no toca el disco. Cargar el fichero es cosa del Loader.
func Parse(data []byte) (*Profile, error) {
	var profile Profile
	if err := yaml.Unmarshal(data, &profile); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidProfile, err)
	}

	sum := sha256.Sum256(data)
	profile.checksum = hex.EncodeToString(sum[:8])

	if err := profile.Validate(); err != nil {
		return nil, err
	}
	return &profile, nil
}

// Validate comprueba la coherencia del perfil.
//
// Un perfil incoherente no se carga: mejor seguir con el anterior que decidir
// con umbrales que nadie ha revisado.
func (p *Profile) Validate() error {
	if p.Version == "" {
		return fmt.Errorf("%w: falta la versión", ErrInvalidProfile)
	}
	if len(p.Weights) == 0 {
		return fmt.Errorf("%w: sin pesos", ErrInvalidProfile)
	}

	total := 0.0
	for signal, weight := range p.Weights {
		if !signal.Known() {
			return fmt.Errorf("%w: señal desconocida %q", ErrInvalidProfile, signal)
		}
		if weight < 0 {
			return fmt.Errorf("%w: peso negativo en %q", ErrInvalidProfile, signal)
		}
		total += weight
	}
	// Los pesos suman uno para que el score se lea como lo que es: una media
	// ponderada en [0,1]. Si no sumaran, el umbral dejaría de significar nada.
	if total < 0.999 || total > 1.001 {
		return fmt.Errorf("%w: los pesos suman %.4f, deben sumar 1", ErrInvalidProfile, total)
	}

	for signal, floor := range p.Floors {
		if !signal.Known() {
			return fmt.Errorf("%w: suelo sobre señal desconocida %q", ErrInvalidProfile, signal)
		}
		if floor.Min < 0 || floor.Min > 1 {
			return fmt.Errorf("%w: suelo de %q fuera de [0,1]", ErrInvalidProfile, signal)
		}
		if !floor.Code.Known() {
			return fmt.Errorf("%w: motivo desconocido %q en el suelo de %q",
				ErrInvalidProfile, floor.Code, signal)
		}
		if ok, why := signal.FloorAllowed(); !ok {
			return fmt.Errorf("%w: la señal %q no admite suelo: %s",
				ErrInvalidProfile, signal, why)
		}
		if floor.Code.Family() == FamilyQuality {
			// Un suelo dispara un rechazo. Si su motivo fuera de calidad, el
			// resultado se contradiría a sí mismo.
			return fmt.Errorf("%w: el suelo de %q usa un motivo de calidad (%q); "+
				"un veto tiene que acusar, no excusar", ErrInvalidProfile, signal, floor.Code)
		}
	}

	if p.Thresholds.PassMin <= p.Thresholds.RejectMax {
		return fmt.Errorf("%w: pass_min (%.3f) debe ser mayor que reject_max (%.3f); "+
			"sin banda intermedia no hay reintento posible",
			ErrInvalidProfile, p.Thresholds.PassMin, p.Thresholds.RejectMax)
	}
	if p.Thresholds.PassMin > 1 || p.Thresholds.RejectMax < 0 {
		return fmt.Errorf("%w: umbrales fuera de [0,1]", ErrInvalidProfile)
	}

	if p.Quality.MinSignals < 1 {
		return fmt.Errorf("%w: min_signals debe ser al menos 1", ErrInvalidProfile)
	}
	if p.Retry.MaxAttempts < 1 {
		return fmt.Errorf("%w: max_attempts debe ser al menos 1", ErrInvalidProfile)
	}

	if p.Capture.SharpnessReference <= 0 {
		return fmt.Errorf("%w: sharpness_reference debe ser > 0", ErrInvalidProfile)
	}
	if p.Capture.MinBrightness >= p.Capture.MaxBrightness {
		return fmt.Errorf("%w: el rango de exposición está al revés", ErrInvalidProfile)
	}
	if p.Capture.MaxHighlightSaturation <= 0 {
		return fmt.Errorf("%w: max_highlight_saturation debe ser > 0", ErrInvalidProfile)
	}
	return nil
}

// SignalsByWeight devuelve las señales con peso, de mayor a menor.
func (p *Profile) SignalsByWeight() []Signal {
	out := make([]Signal, 0, len(p.Weights))
	for signal, weight := range p.Weights {
		if weight > 0 {
			out = append(out, signal)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if p.Weights[out[i]] != p.Weights[out[j]] {
			return p.Weights[out[i]] > p.Weights[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}
