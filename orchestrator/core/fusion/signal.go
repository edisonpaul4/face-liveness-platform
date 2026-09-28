// Package fusion es el motor de decisión: convierte la línea de tiempo de
// medidas que emite el analizador en un veredicto explicable.
//
// Tres reglas que gobiernan el diseño:
//
//  1. **Pesos a mano, no aprendidos.** Todavía no hay modelo: primero hay que
//     entender qué aporta cada señal, y para eso hace falta poder leer por qué
//     salió cada veredicto. Un modelo aprendido con los datos que aún no
//     tenemos sería un generador de decisiones inexplicables.
//  2. **Tres desenlaces, no dos.** Calidad insuficiente NO es ataque
//     detectado. Confundirlos convierte una webcam mala o una habitación
//     iluminada en una acusación.
//  3. **Todo rechazo se explica.** Qué señal falló, con qué valor y contra qué
//     umbral. El backoffice y la gestión de excepciones viven de eso.
package fusion

import "fmt"

// Signal identifica una magnitud que entra en la fusión.
//
// Los nombres son estables: aparecen en configuración, en resultados
// persistidos y en el backoffice. Renombrar una es romper el histórico.
type Signal string

// Señales que entran en la fusión.
const (
	// --- reto de pose ---

	// SignalPoseCompliance: hasta dónde llegó el ángulo pedido.
	SignalPoseCompliance Signal = "pose_compliance"
	// SignalPoseContinuity: si la pose recorrió los ángulos intermedios o
	// pegó un salto propio de un corte de vídeo.
	SignalPoseContinuity Signal = "pose_continuity"
	// SignalPoseParallax: si el movimiento lo explica un plano. Una foto y
	// una pantalla lo son.
	SignalPoseParallax Signal = "pose_parallax"
	// SignalPoseIdentity: si el rostro siguió siendo el mismo.
	SignalPoseIdentity Signal = "pose_identity"

	// --- reto de destello ---

	// SignalFlashCorrelation: si la piel siguió a la secuencia de colores.
	SignalFlashCorrelation Signal = "flash_correlation"
	// SignalFlashGradient3D: si frente, nariz y pómulos respondieron con
	// intensidades distintas. Una superficie plana responde igual en todas
	// partes: es lo único que separa un rostro de una foto impresa.
	SignalFlashGradient3D Signal = "flash_gradient_3d"
	// SignalFlashScreenAbsence: ausencia de indicios de superficie emisiva.
	SignalFlashScreenAbsence Signal = "flash_screen_absence"

	// --- reto de mirada ---

	// SignalGazeResponse: si la mirada fue hacia la esquina que se le puso.
	// Un vídeo grabado puede llevar ojos que se mueven; lo que no puede es
	// mirar al punto que ha aparecido ahora en un sitio elegido al azar.
	SignalGazeResponse Signal = "gaze_response"

	// --- clasificador pasivo ---

	// Los dos clasificadores de textura votan POR SEPARADO, cada uno con su
	// peso. Colapsarlos en un número ahorraría una fila y perdería lo único
	// interesante: en qué se contradicen. Medido sobre el mismo frame, el
	// estrecho dio 0,98 de ataque y el ancho 0,24 — juntarlos habría tirado
	// esa discrepancia, que es exactamente el dato que dice cuál de los dos
	// sirve contra qué ataque.
	//
	// Son la única familia que no viene de un reto: miran un frame suelto,
	// sin pedirle nada al sujeto.

	// SignalTexturePADV2: recorte ancho. Abarca el entorno, donde se ven
	// los bordes de una foto y el bisel de una pantalla. Por eso mismo es el
	// que más se debilita ante un replay a pantalla completa sin marco.
	SignalTexturePADV2 Signal = "texture_pad_v2"
	// SignalTexturePADV1SE: recorte estrecho. Se queda en la piel, así que
	// vive de la textura y no del contexto.
	SignalTexturePADV1SE Signal = "texture_pad_v1se"

	// --- transversales ---

	// SignalPulse: si la piel late.
	//
	// Es la única señal del sistema que una máscara no puede fingir. Todo lo
	// demás —moiré, bandeo, paralaje— delata una SUPERFICIE, y una máscara de
	// silicona bien hecha no es una superficie plana: tiene volumen, se mueve
	// con la cabeza y responde al destello con relieve. Medido en /bench, se
	// cuela en el 55-73 % de los casos. Lo que no tiene es corazón.
	//
	// Contrapartida: es la más frágil de medir. Necesita un tramo largo, sin
	// destellos y con la cara quieta, y con el guion actual pocas veces lo
	// hay. Por eso pesa poco y **no tiene suelo**: no puede acusar sola.
	SignalPulse Signal = "rppg_snr"

	// SignalTemporalPlausibility: si las respuestas llegaron dentro de la
	// ventana humana. Una respuesta imposiblemente rápida entra en la fusión
	// como una señal más, y además veta.
	SignalTemporalPlausibility Signal = "temporal_plausibility"
	// SignalCaptureQuality: si los frames servían para medir.
	SignalCaptureQuality Signal = "capture_quality"
)

// AllSignals son todas las señales conocidas, en orden estable.
var AllSignals = []Signal{
	SignalPoseCompliance,
	SignalPoseContinuity,
	SignalPoseParallax,
	SignalPoseIdentity,
	SignalGazeResponse,
	SignalTexturePADV2,
	SignalTexturePADV1SE,
	SignalFlashCorrelation,
	SignalFlashGradient3D,
	SignalFlashScreenAbsence,
	SignalPulse,
	SignalTemporalPlausibility,
	SignalCaptureQuality,
}

// signalsWithoutFloor son las señales que NO pueden vetar por sí solas,
// pase lo que pase en el perfil.
//
// No es una precaución: es que su valor bajo no significa lo mismo para todo
// el mundo, y un suelo lo trataría como si sí.
var signalsWithoutFloor = map[Signal]string{
	SignalPulse: "el SNR del pulso depende del tono de piel tanto como de que " +
		"haya latido: la melanina está por encima del lecho capilar y atenúa " +
		"los fotones que vuelven. Medido (Nowara et al., CVPRW 2020), POS da " +
		"entre +0,05 y +1,76 dB para los tipos Fitzpatrick I-V y -5,58 dB para " +
		"el VI, con un suelo de ruido de la métrica en torno a -6,5. Para una " +
		"piel muy oscura la salida es indistinguible de no haber medido nada, " +
		"así que NO existe umbral que separe una máscara de una persona de piel " +
		"oscura. Un suelo aquí rechazaría por tono de piel y lo llamaría fraude",
}

// FloorAllowed dice si una señal admite suelo, y por qué no si no lo admite.
//
// La asimetría que impone: un SNR alto ABSUELVE —si hay pulso, no es una
// máscara, y eso vale para cualquiera—, pero un SNR bajo no puede ACUSAR.
// La señal sigue pesando en la media ponderada; lo que no puede es vetar.
func (s Signal) FloorAllowed() (bool, string) {
	reason, blocked := signalsWithoutFloor[s]
	return !blocked, reason
}

// Known indica si la señal está en la taxonomía.
func (s Signal) Known() bool {
	for _, known := range AllSignals {
		if known == s {
			return true
		}
	}
	return false
}

// SignalValue es lo que una señal aportó a una decisión.
//
// Se persiste con el resultado: sin esto, un rechazo es una afirmación sin
// pruebas.
type SignalValue struct {
	Signal Signal `json:"signal"`
	// Value en [0,1]. 1 es lo que se espera de una persona real.
	Value float64 `json:"value"`
	// Weight con el que entró en la media ponderada.
	Weight float64 `json:"weight"`
	// Floor es el mínimo por debajo del cual la señal veta por sí sola.
	Floor float64 `json:"floor"`
	// Windows es de cuántas ventanas salió el valor.
	Windows int `json:"windows"`
	// Aggregation dice cómo se combinaron esas ventanas.
	Aggregation string `json:"aggregation"`
}

// String implementa fmt.Stringer.
func (v SignalValue) String() string {
	return fmt.Sprintf("%s=%.3f (peso %.2f, suelo %.2f, %d ventanas)",
		v.Signal, v.Value, v.Weight, v.Floor, v.Windows)
}
