package fusion

import (
	"fmt"
	"math"
	"sort"
	"sync/atomic"
	"time"

	"github.com/edisonpaul4/biometrics/orchestrator/core/clock"
)

// Outcome es el desenlace de una sesión.
//
// Son TRES, no dos, y la diferencia es el producto entero: rechazar es acusar
// a alguien de intentar engañar al sistema. Que la cámara sea mala o la
// habitación esté iluminada no es eso.
type Outcome uint8

// Desenlaces.
const (
	OutcomeUnknown Outcome = iota
	// OutcomePass: hay persona real delante.
	OutcomePass
	// OutcomeReject: hay indicio de ataque. Es una acusación.
	OutcomeReject
	// OutcomeRetry: no se pudo decidir. No es culpa de nadie.
	OutcomeRetry
)

// String implementa fmt.Stringer.
func (o Outcome) String() string {
	switch o {
	case OutcomePass:
		return "pass"
	case OutcomeReject:
		return "reject"
	case OutcomeRetry:
		return "retry"
	case OutcomeUnknown:
		return "unknown"
	default:
		return fmt.Sprintf("outcome(%d)", uint8(o))
	}
}

// Result es un veredicto explicado.
//
// Se persiste entero. Un rechazo sin las señales que lo produjeron y sin la
// versión del perfil que lo decidió no se puede auditar ni recurrir.
type Result struct {
	Outcome Outcome `json:"outcome"`
	// Score es la media ponderada de las señales medidas.
	Score float64 `json:"score"`
	// Reasons explica el veredicto. El primero es el determinante.
	Reasons []Reason `json:"reasons"`
	// Signals es todo lo que entró en la fusión, con su peso y su suelo.
	Signals []SignalValue `json:"signals"`

	ProfileVersion  string `json:"profile_version"`
	ProfileChecksum string `json:"profile_checksum"`

	DecidedAt time.Time `json:"decided_at"`
}

// Primary devuelve el motivo determinante.
func (r Result) Primary() Reason {
	if len(r.Reasons) == 0 {
		return Reason{Code: CodePassed, Family: FamilyQuality}
	}
	return r.Reasons[0]
}

// HasCode indica si el resultado incluye ese motivo.
func (r Result) HasCode(code Code) bool {
	for _, reason := range r.Reasons {
		if reason.Code == code {
			return true
		}
	}
	return false
}

// Engine decide sobre líneas de tiempo.
//
// El perfil se puede cambiar en caliente: mover un umbral es una operación de
// producto que ocurre a menudo y no puede costar un despliegue.
type Engine struct {
	profile atomic.Pointer[Profile]
	clk     clock.Clock
}

// New crea el motor con un perfil ya validado.
func New(profile *Profile, clk clock.Clock) (*Engine, error) {
	if profile == nil {
		return nil, fmt.Errorf("%w: perfil nulo", ErrInvalidProfile)
	}
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	if clk == nil {
		clk = clock.System{}
	}
	engine := &Engine{clk: clk}
	engine.profile.Store(profile)
	return engine, nil
}

// Profile devuelve el perfil en vigor.
func (e *Engine) Profile() *Profile { return e.profile.Load() }

// Reload cambia el perfil en caliente.
//
// Un perfil inválido no entra: es preferible seguir decidiendo con el anterior
// que empezar a decidir con umbrales que nadie ha revisado.
func (e *Engine) Reload(profile *Profile) error {
	if profile == nil {
		return fmt.Errorf("%w: perfil nulo", ErrInvalidProfile)
	}
	if err := profile.Validate(); err != nil {
		return err
	}
	e.profile.Store(profile)
	return nil
}

// Decide emite el veredicto de una sesión.
//
// El orden de comprobación es deliberado:
//
//  1. Infraestructura caída → reintentar. El fallo es nuestro.
//  2. Respuesta imposiblemente rápida → rechazo. Se mide con el reloj del
//     servidor, así que vale aunque la imagen no sirva.
//  3. Calidad insuficiente → reintentar. Antes de acusar a nadie hay que
//     haber podido medir.
//  4. Suelos por señal → rechazo, con la señal y el valor que lo provocaron.
//  5. Media ponderada contra las tres bandas.
func (e *Engine) Decide(timeline Timeline) Result {
	profile := e.profile.Load()
	result := Result{
		ProfileVersion:  profile.Version,
		ProfileChecksum: profile.checksum,
		DecidedAt:       e.clk.Now(),
	}

	// 1. Infraestructura.
	if timeline.InfrastructureFailure {
		result.Outcome = OutcomeRetry
		result.Reasons = []Reason{newReason(
			CodeInfraAnalyzerUnavailable, "", 0, 0, timeline.InfrastructureDetail,
		)}
		return result
	}

	signals, temporal := e.collect(timeline, profile)
	result.Signals = signals
	result.Score = weightedScore(signals)

	// 2. Veto temporal duro.
	if temporal.tooFast {
		result.Outcome = OutcomeReject
		result.Reasons = append(result.Reasons, newReason(
			CodeTemporalTooFast, SignalTemporalPlausibility, 0, 0,
			"respuesta anterior al mínimo humano plausible",
		))
		result.Reasons = append(result.Reasons, e.floorReasons(signals)...)
		return result
	}

	// 3. Calidad.
	if reasons := e.qualityReasons(timeline, signals, profile); len(reasons) > 0 {
		result.Outcome = OutcomeRetry
		result.Reasons = reasons
		return result
	}

	// 4. Suelos por señal.
	if reasons := e.floorReasons(signals); len(reasons) > 0 {
		result.Outcome = OutcomeReject
		result.Reasons = reasons
		return result
	}

	// 5. Bandas.
	switch {
	case result.Score >= profile.Thresholds.PassMin:
		result.Outcome = OutcomePass
		result.Reasons = []Reason{newReason(
			CodePassed, "", result.Score, profile.Thresholds.PassMin, "",
		)}
	case result.Score <= profile.Thresholds.RejectMax:
		result.Outcome = OutcomeReject
		result.Reasons = []Reason{newReason(
			CodeAttackFusedScoreLow, "", result.Score, profile.Thresholds.RejectMax,
			weakestSignalDetail(signals),
		)}
	default:
		// Zona intermedia: no hay evidencia suficiente ni para aprobar ni
		// para acusar. Se repite.
		result.Outcome = OutcomeRetry
		result.Reasons = []Reason{newReason(
			CodeQualityInsufficientSignal, "", result.Score, profile.Thresholds.PassMin,
			"score en la banda intermedia: sin evidencia para decidir",
		)}
		if temporal.timeouts > 0 {
			result.Reasons = append(result.Reasons, newReason(
				CodeTemporalTimeout, SignalTemporalPlausibility,
				float64(temporal.timeouts), 0, "pasos fuera de plazo",
			))
		}
	}
	return result
}

// temporalSummary resume lo que dieron los eventos temporales.
type temporalSummary struct {
	tooFast  bool
	timeouts int
}

// collect reúne el valor de cada señal con peso en el perfil.
func (e *Engine) collect(timeline Timeline, profile *Profile) ([]SignalValue, temporalSummary) {
	windowValues := collectWindowSignals(timeline.Windows)

	plausibility, tooFast, timeouts, hasTemporal := temporalPlausibility(timeline.Temporal)
	capture, hasCapture := captureQuality(timeline.Capture, profile.Capture)

	// Los clasificadores pasivos no votan sobre una captura quemada.
	//
	// Su resumen de sesión es el PEOR frame, así que basta un reflejo cenital
	// para que la sesión entera arrastre ese valor. Medido sobre el banco:
	// de las caras reales que el modelo marcaba, las que fallaban con
	// cualquier preproceso eran justo las de luz reventada.
	//
	// Rechazar por eso sería acusar de fingir a quien tiene una lámpara
	// encima. La calidad insuficiente lleva a reintentar, nunca a rechazo
	// (CLAUDE.md §4), y la forma de respetarlo aquí es la de siempre: lo que
	// no se pudo medir no entra en la fusión.
	if overexposed(timeline.Capture, profile.Capture) {
		delete(windowValues, SignalTexturePADV2)
		delete(windowValues, SignalTexturePADV1SE)
	}

	values := make([]SignalValue, 0, len(profile.Weights))
	for _, signal := range profile.SignalsByWeight() {
		var value float64
		var windows int
		aggregation := "worst"

		switch signal {
		case SignalTemporalPlausibility:
			if !hasTemporal {
				continue
			}
			value, windows, aggregation = plausibility, len(timeline.Temporal), "ratio"
		case SignalCaptureQuality:
			if !hasCapture {
				continue
			}
			value, windows, aggregation = capture, timeline.Capture.Frames, "min"
		default:
			measured, ok := windowValues[signal]
			if !ok || len(measured) == 0 {
				// Una señal que no se pudo medir no entra. Meterla como cero
				// sería fabricar evidencia contra el usuario.
				continue
			}
			value, windows = worst(measured), len(measured)
		}

		floor := 0.0
		if f, ok := profile.FloorFor(signal); ok {
			floor = f.Min
		}
		values = append(values, SignalValue{
			Signal:      signal,
			Value:       clamp01(value),
			Weight:      profile.Weight(signal),
			Floor:       floor,
			Windows:     windows,
			Aggregation: aggregation,
		})
	}

	return values, temporalSummary{tooFast: tooFast, timeouts: timeouts}
}

// qualityReasons comprueba si hubo con qué medir.
func (e *Engine) qualityReasons(timeline Timeline, signals []SignalValue, profile *Profile) []Reason {
	var reasons []Reason

	// Ventanas que el analizador no pudo medir.
	//
	// Con una excepción, y es la que sostiene toda la regla de equidad del
	// sistema: una ventana cuyas señales NO ADMITEN SUELO tampoco puede
	// forzar un reintento.
	//
	// Sin esta excepción, la regla del «sin suelo» sólo impedía que el pulso
	// RECHAZARA, y dejaba abierto que BLOQUEARA. Y el pulso es
	// estructuralmente no medible en piel muy oscura —la melanina está por
	// encima del lecho capilar y no hay fotones que devolver—, así que ese
	// grupo quedaría reintentando en bucle. Un rechazo disfrazado de
	// «inténtalo otra vez» sigue siendo un rechazo, y encima uno que no se
	// puede recurrir porque nunca se declara.
	//
	// Se detectó con una sesión real: score 0,9154 con doce de trece señales
	// medidas y el destello correlacionando a 1,0, resuelta en reintentar
	// porque el pulso no se pudo medir.
	for _, window := range timeline.Windows {
		if window.QualitySufficient || optionalWindow(window.Kind) {
			continue
		}
		reasons = append(reasons, newReason(
			CodeQualityInsufficientSignal, "", 0, 0,
			fmt.Sprintf("ventana %s: %s", window.ID, window.QualityReason),
		))
	}

	coverage := faceCoverage(timeline.Capture)
	if timeline.Capture.Frames > 0 && coverage < profile.Capture.MinFaceCoverage {
		reasons = append(reasons, newReason(
			CodeQualityNoFace, SignalCaptureQuality, coverage, profile.Capture.MinFaceCoverage,
			"rostro ausente en demasiados frames",
		))
	}

	for _, value := range signals {
		if value.Signal == SignalCaptureQuality && value.Value < profile.Quality.MinCaptureQuality {
			reasons = append(reasons, newReason(
				CodeQualityCapture, SignalCaptureQuality, value.Value,
				profile.Quality.MinCaptureQuality, "captura por debajo del mínimo",
			))
		}
	}

	if measurable(signals) < profile.Quality.MinSignals {
		reasons = append(reasons, newReason(
			CodeQualityNoSignals, "", float64(measurable(signals)),
			float64(profile.Quality.MinSignals), "señales medibles insuficientes",
		))
	}

	if profile.Quality.RequireCompletedChallenges && !timeline.ChallengesCompleted {
		reasons = append(reasons, newReason(
			CodeQualityIncomplete, "", 0, 0, "la sesión no completó sus retos",
		))
	}

	return reasons
}

// floorReasons devuelve las señales que vetan por sí solas.
func (e *Engine) floorReasons(signals []SignalValue) []Reason {
	profile := e.profile.Load()
	var reasons []Reason

	for _, value := range signals {
		floor, ok := profile.FloorFor(value.Signal)
		if !ok || value.Value >= floor.Min {
			continue
		}
		reasons = append(reasons, newReason(
			floor.Code, value.Signal, value.Value, floor.Min,
			fmt.Sprintf("por debajo del suelo en %d ventana(s)", value.Windows),
		))
	}

	// El más grave primero: es el que se le enseña a quien revise el caso.
	sort.SliceStable(reasons, func(i, j int) bool {
		return (reasons[i].Threshold - reasons[i].Observed) >
			(reasons[j].Threshold - reasons[j].Observed)
	})
	return reasons
}

// weightedScore es la media ponderada de lo que se pudo medir.
//
// Los pesos se renormalizan sobre las señales presentes: lo que no se midió no
// arrastra el score hacia abajo.
func weightedScore(signals []SignalValue) float64 {
	total, weights := 0.0, 0.0
	for _, value := range signals {
		total += value.Value * value.Weight
		weights += value.Weight
	}
	if weights <= 0 {
		return 0
	}
	return clamp01(total / weights)
}

// captureQuality traduce las estadísticas de captura a [0,1].
//
// Se queda con el peor de los cuatro términos: de nada sirve estar bien
// expuesto si no hay rostro en el encuadre.
func captureQuality(stats CaptureStats, gate CaptureGate) (float64, bool) {
	if stats.Frames <= 0 {
		return 0, false
	}

	coverage := faceCoverage(stats)
	sharpness := clamp01(stats.MeanSharpness / gate.SharpnessReference)

	exposure := 1.0
	switch {
	case stats.MeanBrightness < gate.MinBrightness:
		exposure = clamp01(stats.MeanBrightness / math.Max(gate.MinBrightness, 1e-6))
	case stats.MeanBrightness > gate.MaxBrightness:
		exposure = clamp01((1 - stats.MeanBrightness) / math.Max(1-gate.MaxBrightness, 1e-6))
	}

	highlights := clamp01(1 - stats.MeanHighlightSaturation/math.Max(gate.MaxHighlightSaturation, 1e-6))

	return math.Min(math.Min(coverage, sharpness), math.Min(exposure, highlights)), true
}

// overexposed dice si los quemados del rostro pasan del límite del perfil.
//
// El umbral es el mismo que puntúa la calidad de captura: si una exposición
// ya se considera mala para medir, tampoco sirve para acusar.
func overexposed(stats CaptureStats, gate CaptureGate) bool {
	if stats.Frames <= 0 || gate.MaxHighlightSaturation <= 0 {
		return false
	}
	return stats.MeanHighlightSaturation > gate.MaxHighlightSaturation
}

// optionalWindow dice si una ventana puede faltar sin bloquear la sesión.
//
// Lo es cuando NINGUNA de sus señales admite suelo: si su valor bajo no puede
// acusar, su ausencia tampoco puede exigir. Las dos reglas salen del mismo
// sitio —que la señal no significa lo mismo para todo el mundo— y tenerlas por
// separado fue justo lo que dejó el hueco.
func optionalWindow(kind WindowKind) bool {
	mapping, ok := submetricSignals[kind]
	if !ok || len(mapping) == 0 {
		return false
	}
	for _, signal := range mapping {
		if allowed, _ := signal.FloorAllowed(); allowed {
			return false
		}
	}
	return true
}

func faceCoverage(stats CaptureStats) float64 {
	if stats.Frames <= 0 {
		return 0
	}
	return clamp01(float64(stats.FramesWithFace) / float64(stats.Frames))
}

// measurable cuenta las señales que aportaron evidencia de verdad.
//
// La calidad de captura no cuenta: dice si se podía medir, no qué se midió.
// Sin esto, una sesión sin una sola señal discriminante podría aprobarse por
// tener buena cámara.
func measurable(signals []SignalValue) int {
	count := 0
	for _, value := range signals {
		if value.Signal != SignalCaptureQuality {
			count++
		}
	}
	return count
}

// weakestSignalDetail nombra la señal que más tiró del score hacia abajo.
func weakestSignalDetail(signals []SignalValue) string {
	if len(signals) == 0 {
		return "sin señales"
	}
	weakest := signals[0]
	for _, value := range signals[1:] {
		if value.Value*value.Weight < weakest.Value*weakest.Weight {
			weakest = value
		}
	}
	return fmt.Sprintf("la señal más débil fue %s", weakest)
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return math.Max(0, math.Min(1, v))
}
