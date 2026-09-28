package conn

import (
	"testing"
	"time"

	"github.com/edisonpaul4/biometrics/gateway/internal/analyzer"
	"github.com/edisonpaul4/biometrics/orchestrator/core/challenge"
	"github.com/edisonpaul4/biometrics/orchestrator/core/session"
)

var evalT0 = time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)

func features(signals map[string]float64, face bool) analyzer.Features {
	return analyzer.Features{
		Signals: signals,
		Quality: analyzer.Quality{FaceDetected: face},
	}
}

func TestCalibrationNeedsFaceAndFullHold(t *testing.T) {
	step := session.RevealedStep{
		Kind: challenge.KindCalibration,
		Hold: 200 * time.Millisecond,
	}
	e := newEvaluator(step, evalT0)
	neutral := features(map[string]float64{}, true)

	// Sin rostro no cuenta nada, por mucho que pase el tiempo.
	if e.observe(features(map[string]float64{}, false), evalT0.Add(time.Second)) {
		t.Error("calibración cumplida sin rostro")
	}
	// Con rostro pero antes de tiempo, tampoco.
	if e.observe(neutral, evalT0.Add(50*time.Millisecond)) {
		t.Error("calibración cumplida antes de agotar la espera impuesta")
	}
	if e.observe(neutral, evalT0.Add(199*time.Millisecond)) {
		t.Error("calibración cumplida 1 ms antes de tiempo")
	}
	// Ni con el tiempo cumplido si no hay FRAMES suficientes: el analizador
	// necesita un mínimo para que la línea base signifique algo, y cerrar
	// antes le entrega una ventana que va a rechazar.
	if e.observe(neutral, evalT0.Add(200*time.Millisecond)) {
		t.Error("calibración cumplida con sólo 3 frames de rostro")
	}
	for i := range minCalibrationHits {
		at := evalT0.Add(time.Duration(200+i) * time.Millisecond)
		if e.observe(neutral, at) {
			return // cumplida en cuanto junta frames suficientes
		}
	}
	t.Errorf("calibración no cumplida ni con %d frames de rostro", minCalibrationHits)
}

// TestPoseThresholds fija el convenio de signo del yaw, que ya se equivocó
// una vez y costó un "inconclusive" imposible de explicar.
//
// Los retos se enuncian desde el SUJETO ("gira a tu derecha"). La cámara le
// mira de frente y no va espejada: el espejo de la vista previa es CSS y no
// toca los frames que se envían. Así que girar hacia la propia derecha mueve
// la nariz hacia la izquierda de la imagen, y el ángulo medido sale NEGATIVO.
//
// Comprobado con una cámara real: reto "gira a tu derecha", giro cumplido,
// yaw medido -37,04°. Y reto "levanta la barbilla", cumplido, pitch -15,99°.
func TestPoseThresholds(t *testing.T) {
	cases := []struct {
		action  challenge.PoseAction
		signals map[string]float64
		want    bool
		nombre  string
	}{
		{challenge.PoseYawLeft, map[string]float64{"pose_yaw_deg": 25}, true, "a su izquierda: nariz a la derecha de la imagen"},
		{challenge.PoseYawLeft, map[string]float64{"pose_yaw_deg": 10}, false, "giro a la izquierda corto"},
		{challenge.PoseYawLeft, map[string]float64{"pose_yaw_deg": -25}, false, "izquierda pedida, derecha girada"},
		{challenge.PoseYawRight, map[string]float64{"pose_yaw_deg": -25}, true, "a su derecha: nariz a la izquierda de la imagen"},
		{challenge.PoseYawRight, map[string]float64{"pose_yaw_deg": -10}, false, "giro a la derecha corto"},
		{challenge.PoseYawRight, map[string]float64{"pose_yaw_deg": 25}, false, "derecha pedida, izquierda girada"},
		// Levantar la barbilla da pitch NEGATIVO. Medido con cámara real:
		// barbilla arriba a conciencia, -15,99°.
		{challenge.PosePitchUp, map[string]float64{"pose_pitch_deg": -16}, true, "barbilla arriba"},
		{challenge.PosePitchUp, map[string]float64{"pose_pitch_deg": -6}, false, "barbilla apenas: es cabeceo incidental"},
		{challenge.PosePitchUp, map[string]float64{"pose_pitch_deg": 20}, false, "barbilla abajo, que es lo contrario"},
		{challenge.PoseUnspecified, map[string]float64{}, false, "pose desconocida"},
	}

	for _, c := range cases {
		t.Run(c.nombre, func(t *testing.T) {
			e := newEvaluator(session.RevealedStep{Kind: challenge.KindPose, Pose: c.action}, evalT0)
			got := e.observe(features(c.signals, true), evalT0.Add(500*time.Millisecond))
			if got != c.want {
				t.Errorf("cumplido = %v, se esperaba %v", got, c.want)
			}
		})
	}
}

// TestMoveCloserIsRelative: acercarse se mide contra el tamaño con el que se
// empezó el paso, no contra un absoluto; si no, quien ya estaba cerca lo
// tendría regalado.
//
// Las cifras son las de una webcam de portátil de verdad —la cara ocupa en
// torno al 7 % del encuadre—, no las de un maniquí renderizado a pantalla
// completa. Con el suelo absoluto calibrado sobre el maniquí, este paso era
// inalcanzable para cualquier persona sentada a una distancia normal.
func TestMoveCloserIsRelative(t *testing.T) {
	e := newEvaluator(session.RevealedStep{
		Kind: challenge.KindPose, Pose: challenge.PoseMoveCloser,
	}, evalT0)

	// Primer frame: fija la línea base.
	if e.observe(features(map[string]float64{"face_area_ratio": 0.07}, true), evalT0) {
		t.Error("cumplido en el primer frame, sin haberse acercado")
	}
	// Un poco más grande no basta.
	if e.observe(features(map[string]float64{"face_area_ratio": 0.079}, true), evalT0.Add(300*time.Millisecond)) {
		t.Error("cumplido con un crecimiento del 13 %")
	}
	// Un 43 % sí. Se probó exigir el 80 % y hubo que deshacerlo: a esa
	// distancia el ojo se escorza y la mirada deja de medirse (ver
	// closerGrowthFactor).
	if !e.observe(features(map[string]float64{"face_area_ratio": 0.10}, true), evalT0.Add(600*time.Millisecond)) {
		t.Error("no cumplido con un crecimiento del 43 %")
	}
}

// Un crecimiento relativo enorme sobre una cara diminuta no es acercarse: es
// ruido de detección sobre alguien que está al fondo de la habitación.
func TestMoveCloserNecesitaUnaCaraDeVerdad(t *testing.T) {
	e := newEvaluator(session.RevealedStep{
		Kind: challenge.KindPose, Pose: challenge.PoseMoveCloser,
	}, evalT0)

	e.observe(features(map[string]float64{"face_area_ratio": 0.01}, true), evalT0)
	if e.observe(features(map[string]float64{"face_area_ratio": 0.03}, true), evalT0.Add(400*time.Millisecond)) {
		t.Error("cumplido triplicando una cara del 1 % del encuadre")
	}
}

func TestPoseNeedsFace(t *testing.T) {
	e := newEvaluator(session.RevealedStep{
		Kind: challenge.KindPose, Pose: challenge.PoseYawLeft,
	}, evalT0)
	if e.observe(features(map[string]float64{"pose_yaw_deg": -40}, false), evalT0.Add(time.Second)) {
		t.Error("pose cumplida sin rostro en el encuadre")
	}
}

// flashStep construye un paso de destello de tres colores de 100 ms.
func flashStep() session.RevealedStep {
	seq := []challenge.FlashSegment{
		{Color: challenge.FlashRed, Duration: 400 * time.Millisecond},
		{Color: challenge.FlashGreen, Duration: 400 * time.Millisecond},
		{Color: challenge.FlashBlue, Duration: 400 * time.Millisecond},
	}
	return session.RevealedStep{Kind: challenge.KindFlash, Flash: seq, Hold: 1200 * time.Millisecond}
}

// playFlash reproduce una sesión de destello con la profundidad de modulación
// que se le pida, y devuelve si el paso se dio por cumplido.
//
// `depth` es cuánto sube la relación rostro/fondo en el canal iluminado. Con
// 0,50 la señal es evidente; con 0,05 está por debajo de lo que cualquier
// umbral por frame puede ver, y es justo la que se midió con una cámara real.
//
// `follow` en falso hace que la cara responda a otro color del que toca.
func playFlash(e *evaluator, step session.RevealedStep, depth float64, follow bool) bool {
	const frame = 66 * time.Millisecond
	// Relación rostro/fondo en reposo, y una deriva lenta de la cámara para
	// que el test no se apoye en que la escena esté quieta.
	const base = 0.74

	ok := false
	for elapsed := time.Duration(0); elapsed <= step.Hold+frame; elapsed += frame {
		// El color tarda en llegar a la pantalla: red, decisión del cliente y
		// composición. Antes de eso el rostro está en reposo, y ese reposo es
		// la referencia contra la que se mide si la pantalla ilumina.
		painted := elapsed >= flashPaintDelay
		idx, inside := segmentAt(step.Flash, elapsed)
		inside = inside && painted

		bgr := [3]float64{base, base, base}
		if inside {
			color := step.Flash[idx].Color
			if !follow {
				color = challenge.FlashGreen // siempre el mismo, siga o no
			}
			for c, w := range channelWeights(color) {
				bgr[c] += base * depth * w
			}
		}
		// Deriva: la cámara se adapta y todo cae poco a poco. La correlación
		// tiene que sobrevivir a esto.
		drift := 1 - 0.10*float64(elapsed)/float64(step.Hold)
		luminance := base * drift
		if painted {
			luminance = base * (1 + depth) * drift
		}
		ok = e.observe(features(map[string]float64{
			"surface_face_bg_b":               bgr[0] * drift,
			"surface_face_bg_g":               bgr[1] * drift,
			"surface_face_bg_r":               bgr[2] * drift,
			"surface_face_bg_luminance_ratio": luminance,
		}, true), evalT0.Add(elapsed))
	}
	return ok
}

// El paso de destello se cumple por haber SONADO la secuencia entera.
//
// Si la piel la siguió o no lo decide la ventana de Python, que es quien tiene
// la medida buena. Antes se exigía aquí una correlación calculada por el
// gateway y costaba sesiones legítimas: medido, una sesión que Python midió en
// **0,4934** de correlación —holgadamente por encima del umbral— acabó en
// «demasiada luz ambiente» porque la del gateway no llegó.
//
// Es lo que §6 advierte de tener la misma señal en dos sitios: la versión
// pobre arrastra a la buena.
func TestElDestelloSeCumplePorHaberSonado(t *testing.T) {
	casos := []struct {
		nombre string
		depth  float64
		follow bool
	}{
		{"modulación evidente", 0.50, true},
		{"modulación del 5 %, la real", 0.05, true},
		{"sin modulación ninguna", 0.0, true},
		{"respondiendo a otra cosa", 0.50, false},
	}

	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			step := flashStep()
			e := newEvaluator(step, evalT0)
			if got := playFlash(e, step, c.depth, c.follow); !got {
				t.Error("la secuencia sonó entera y el paso no se dio por cumplido")
			}
		})
	}
}

// La búsqueda de retardo no es opcional: entre que el servidor revela el reto
// y el color llega a la cámara hay red, composición y captura. Suponer
// sincronía exacta es suponer lo que no se puede.
func TestDestelloAguantaElRetardoDePintado(t *testing.T) {
	step := flashStep()
	e := newEvaluator(step, evalT0)

	const frame = 66 * time.Millisecond
	const lag = 200 * time.Millisecond
	const base = 0.74

	ok := false
	for elapsed := time.Duration(0); elapsed <= step.Hold+lag+frame; elapsed += frame {
		// La cara responde a lo que se pintó hace `lag`.
		idx, inside := segmentAt(step.Flash, elapsed-lag)
		bgr := [3]float64{base, base, base}
		if inside {
			for c, w := range channelWeights(step.Flash[idx].Color) {
				bgr[c] += base * 0.08 * w
			}
		}
		ok = e.observe(features(map[string]float64{
			"surface_face_bg_b":               bgr[0],
			"surface_face_bg_g":               bgr[1],
			"surface_face_bg_r":               bgr[2],
			"surface_face_bg_luminance_ratio": base * 1.20,
		}, true), evalT0.Add(elapsed))
	}
	if !ok {
		score, found, _ := e.flashCorrelation()
		t.Errorf("no cumplido con 200 ms de retardo (correlación %.2f, retardo hallado %v)", score, found)
	}
}

func TestFlashRejectsFlatSurface(t *testing.T) {
	e := newEvaluator(flashStep(), evalT0)

	flat := features(map[string]float64{
		"color_response_r": 0.42, "color_response_g": 0.18, "color_response_b": 0.18,
	}, true)

	for _, at := range []time.Duration{50, 150, 250, 400} {
		if e.observe(flat, evalT0.Add(at*time.Millisecond)) {
			t.Fatalf("cumplido a los %v con una superficie plana", at*time.Millisecond)
		}
	}
}

func TestDominantColor(t *testing.T) {
	cases := []struct {
		nombre string
		r      float64
		g      float64
		b      float64
		want   challenge.FlashColor
	}{
		{"blanco", 0.9, 0.9, 0.9, challenge.FlashWhite},
		{"rojo", 0.9, 0.18, 0.18, challenge.FlashRed},
		{"verde", 0.18, 0.9, 0.18, challenge.FlashGreen},
		{"azul", 0.18, 0.18, 0.9, challenge.FlashBlue},
		{"sin respuesta", 0.1, 0.1, 0.1, challenge.FlashUnspecified},
		{"dos canales: ambiguo", 0.9, 0.9, 0.1, challenge.FlashUnspecified},
		// Regresión: con un rojo lavado los tres canales responden por
		// encima del umbral. Comparando en absoluto esto se leía como
		// blanco y el reto de destello dejaba de discriminar nada.
		{"rojo con fuga sigue siendo rojo", 0.95, 0.72, 0.70, challenge.FlashRed},
		// Y el blanco no se convierte en primario por un desequilibrio
		// pequeño del balance de blancos de la cámara.
		{"blanco algo desequilibrado", 0.95, 0.88, 0.86, challenge.FlashWhite},
	}
	for _, c := range cases {
		t.Run(c.nombre, func(t *testing.T) {
			f := features(map[string]float64{
				"color_response_r": c.r, "color_response_g": c.g, "color_response_b": c.b,
			}, true)
			if got := dominantColor(f); got != c.want {
				t.Errorf("dominantColor = %s, se esperaba %s", got, c.want)
			}
		})
	}
}

func TestSegmentAt(t *testing.T) {
	seq := []challenge.FlashSegment{
		{Color: challenge.FlashWhite, Duration: 100 * time.Millisecond},
		{Color: challenge.FlashRed, Duration: 200 * time.Millisecond},
	}
	cases := []struct {
		elapsed time.Duration
		want    int
		ok      bool
	}{
		{-time.Millisecond, 0, false},
		{0, 0, true},
		{99 * time.Millisecond, 0, true},
		{100 * time.Millisecond, 1, true},
		{299 * time.Millisecond, 1, true},
		{300 * time.Millisecond, 0, false}, // la secuencia terminó
	}
	for _, c := range cases {
		got, ok := segmentAt(seq, c.elapsed)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("segmentAt(%v) = %d,%v; se esperaba %d,%v", c.elapsed, got, ok, c.want, c.ok)
		}
	}
}

func TestUnknownKindIsNeverSatisfied(t *testing.T) {
	e := newEvaluator(session.RevealedStep{Kind: challenge.KindUnspecified}, evalT0)
	if e.observe(features(map[string]float64{}, true), evalT0.Add(time.Hour)) {
		t.Error("un paso de tipo desconocido se dio por cumplido")
	}
}

// La mirada se juzga en DOS canales: el iris para el eje horizontal y la
// APERTURA del párpado para el vertical. No es una simetría rota por
// capricho, es que el ojo no funciona igual en los dos ejes: al mirar arriba
// el párpado se retrae y al mirar abajo baja con el ojo, así que el centro
// del iris apenas se mueve respecto a las comisuras.
//
// Medido con una cámara real, en una respuesta correcta y sostenida: el iris
// se desplazó 0,043 en horizontal y sólo 0,007 en vertical —indistinguible
// del ruido—, mientras la apertura subía un 22 %.

const aperturaBase = 0.30

// gazeFeatures construye un frame con una mirada dada.
//
// `irisX` es el desplazamiento del iris DENTRO de la órbita y `yaw` hacia
// dónde apunta la cabeza. La mirada real es la suma de los dos: quien gira la
// cabeza hacia el objetivo no mueve los ojos dentro de la órbita, y quien
// mira de reojo no gira la cabeza.
func gazeFeatures(irisX, yaw float64) analyzer.Features {
	return features(map[string]float64{
		"gaze_offset_x":  irisX,
		"pose_yaw_deg":   yaw,
		"gaze_openness":  aperturaBase,
		"gaze_agreement": 1.0,
	}, true)
}

func nuevoGaze(x, y float64) *evaluator {
	return nuevoGazeTarget(challenge.GazeTarget{X: x, Y: y})
}

func nuevoGazeTarget(target challenge.GazeTarget) *evaluator {
	// El reto lleva DOS puntos: se sale del contrario y se llega a `target`.
	// No hay referencia de reposo — el viaje entre las dos fases ES la medida.
	return newEvaluator(session.RevealedStep{
		Kind:     challenge.KindGaze,
		Gaze:     target,
		GazeFrom: challenge.GazeTarget{X: 1 - target.X, Y: target.Y},
		Hold:     gazeDwellTest,
	}, evalT0)
}

// gazeDwellTest es la permanencia en el primer punto que usan estos tests.
const gazeDwellTest = 1200 * time.Millisecond

// sostenerMuestras son las muestras que hay que dar, al paso de 66 ms de
// estos tests, para cubrir gazeSustainSpan de sobra. La regla del evaluador
// va en TIEMPO; aquí se traduce a cuántas veces llamar a observe.
const sostenerMuestras = 5

// mirar sostiene una respuesta durante la ventana que exige el evaluador.
// mirar juega las dos fases del reto: el sujeto está en el primer punto y
// luego viaja al segundo. `irisX` y `yaw` describen dónde acaba.
func mirar(e *evaluator, irisX, yaw float64) bool {
	return mirarDesde(e, 0, 0, irisX, yaw)
}

func mirarDesde(e *evaluator, desdeIris, desdeYaw, hastaIris, hastaYaw float64) bool {
	got := false
	// Fase de salida. El evaluador descarta los primeros gazeSettle de cada
	// fase, así que hay que cubrirlos y dejar muestras suficientes después.
	for at := evalT0.Add(66 * time.Millisecond); at.Sub(evalT0) < gazeDwellTest; at = at.Add(66 * time.Millisecond) {
		e.observe(gazeFeatures(desdeIris, desdeYaw), at)
	}
	// Fase de llegada.
	fin := gazeDwellTest + gazeSettle + 500*time.Millisecond
	for d := gazeDwellTest; d < fin; d += 66 * time.Millisecond {
		got = e.observe(gazeFeatures(hastaIris, hastaYaw), evalT0.Add(d))
	}
	return got
}

// La mirada se mide en el MUNDO: hacia dónde apunta la cabeza MÁS hacia dónde
// miran los ojos dentro de ella. Los dos términos hacen falta.
//
// Con sólo el iris, quien gira la cabeza hacia el objetivo da cero —sus ojos
// se recentran en la órbita— y quien gira la cabeza sin dejar de mirar al
// mismo sitio da un valor grande en sentido CONTRARIO, por contrarrotación.
// Medido con una cámara real: cabeza girada 18°, iris desplazado −0,060, y la
// mirada real sin moverse ni un grado.
//
// El eje vertical no existe aquí: se intentó de tres formas y no se puede
// medir con una webcam frontal. Ver challenge.GazeTarget.
func TestMiradaSigueAlLado(t *testing.T) {
	const m = 0.06
	izquierda := challenge.GazeTarget{X: m, Y: 0.5}
	derecha := challenge.GazeTarget{X: 1 - m, Y: 0.5}

	// 0,045 de iris son unos 10 grados; el umbral son 5.
	cases := []struct {
		nombre string
		target challenge.GazeTarget
		irisX  float64
		yaw    float64
		quiero bool
	}{
		{"mueve los ojos hacia la izquierda", izquierda, +0.045, 0, true},
		{"mueve los ojos hacia la derecha", derecha, -0.045, 0, true},

		// Girar la cabeza cuenta igual: es otra forma de obedecer, y con
		// sólo el iris habría dado cero.
		{"gira la cabeza a la izquierda", izquierda, 0, +14, true},
		{"gira la cabeza a la derecha", derecha, 0, -14, true},
		{"un poco de cada", izquierda, +0.020, +6, true},

		// Contrarrotación: girar la cabeza SIN mover la mirada. Se rechaza.
		//
		// Estuvo roto y volvió solo al arreglar el estirón de la captura. Con
		// la escala vieja (221 grados por unidad de iris) cancelar 18° de
		// cabeza exigía −0,081 de iris, así que −0,060 dejaba 4,74° de
		// residuo y contaba como obedecer. Con la escala re-derivada sobre
		// geometría correcta —248— el residuo baja a 3,12° y se queda por
		// debajo del umbral.
		//
		// Es la prueba de que aquella constante no era un ajuste fino sino un
		// error de medida: la mirada en el mundo sólo se cancela sola si la
		// cabeza y los ojos están en las mismas unidades.
		{"contrarrotación: se distingue otra vez", izquierda, -0.060, +18, false},

		// Éste sigue sin distinguirse, y está aquí para que nadie crea que sí.
		// Un desplazamiento de 0,015 —poco más que acomodarse en la silla— da
		// 3,7° y pasa el umbral. Lo que protege es la DIRECCIÓN, que se
		// sortea justo antes, y el sostenimiento.
		{"se mueve poco: ya no se distingue", derecha, -0.015, 0, true},

		{"mira al lado contrario", izquierda, -0.045, 0, false},
		{"se queda quieto", derecha, 0, 0, false},
	}

	for _, c := range cases {
		t.Run(c.nombre, func(t *testing.T) {
			e := nuevoGazeTarget(c.target)
			if got := mirar(e, c.irisX, c.yaw); got != c.quiero {
				t.Errorf("cumplido = %v, se esperaba %v (avance %.1f grados)", got, c.quiero, e.gazeBestShift)
			}
		})
	}
}

// El umbral queda cerca de lo que produce una respuesta real, así que el ruido
// hace que algún frame baje por debajo. Exigir una racha limpia tiraba
// respuestas correctas: medido, catorce frames claramente por encima y
// ninguna racha de tres seguidos.
func TestMiradaToleraUnFrameFlojo(t *testing.T) {
	e := nuevoGazeTarget(challenge.GazeTarget{X: 0.06, Y: 0.5})

	// Fase de salida, limpia.
	for at := evalT0.Add(66 * time.Millisecond); at.Sub(evalT0) < gazeDwellTest; at = at.Add(66 * time.Millisecond) {
		e.observe(gazeFeatures(0, 0), at)
	}
	// Fase de llegada con un frame malo en medio: el iris mal detectado
	// produce valores disparatados en cualquier dirección, y de esos hay.
	// La MEDIANA de la fase no se entera; un pico o una racha sí.
	cumplido := false
	malo := 3
	for i, d := 0, gazeDwellTest; d < gazeDwellTest+gazeSettle+500*time.Millisecond; i, d = i+1, d+66*time.Millisecond {
		iris := 0.045
		if i == malo {
			iris = -0.030
		}
		cumplido = e.observe(gazeFeatures(iris, 0), evalT0.Add(d))
	}
	if !cumplido {
		t.Errorf("un frame malo en la fase de llegada tumbó el reto (avance %.1f)", e.gazeBestShift)
	}
}

// Un pico aislado sigue sin valer: eso es deriva, no una mirada.
func TestMiradaDeUnSoloFrameNoCuenta(t *testing.T) {
	e := nuevoGazeTarget(challenge.GazeTarget{X: 0.06, Y: 0.5})
	at := evalT0
	for range 8 {
		at = at.Add(66 * time.Millisecond)
		if e.observe(gazeFeatures(0.05, 0), at) {
			t.Fatal("cumplido con picos aislados")
		}
		at = at.Add(66 * time.Millisecond)
		e.observe(gazeFeatures(0, 0), at)
		at = at.Add(66 * time.Millisecond)
		e.observe(gazeFeatures(0, 0), at)
	}
}

// La anatomía de cada uno no puede decidir el resultado: hay quien tiene la
// cámara descentrada, la cabeza ladeada, o simplemente mira así.
//
// Con el reto de DOS puntos eso deja de importar por construcción: lo que se
// mide es la DIFERENCIA entre las dos fases, y un desplazamiento propio está
// en las dos por igual, así que se cancela. Antes había que restar una
// referencia de reposo, y esa referencia fue la causa de tres fallos: venía
// desfasada, venía contaminada por la mirada anterior, o describía a alguien
// que miraba a un lado durante la calibración —y eso acabó en un rechazo por
// fraude contra un usuario legítimo.
func TestElDesplazamientoPropioNoDecideNada(t *testing.T) {
	// Alguien cuyo reposo está 12° desplazado: se queda quieto ahí todo el
	// reto, sin viajar de un punto al otro.
	quieto := nuevoGazeTarget(challenge.GazeTarget{X: 0.06, Y: 0.5})
	if mirarDesde(quieto, 0.02, 6, 0.02, 6) {
		t.Error("cumplido sin moverse, sólo por tener la mirada desplazada")
	}

	// Y el mismo sujeto, viajando de verdad entre los dos puntos.
	viaja := nuevoGazeTarget(challenge.GazeTarget{X: 0.06, Y: 0.5})
	if !mirarDesde(viaja, 0.02, 6, 0.02+0.045, 6) {
		t.Error("no cumplido con un viaje claro entre los dos puntos")
	}
}

// Una mirada que no se pudo medir no es un fallo del sujeto.
func TestMiradaNoMedibleNoCumpleNiRompe(t *testing.T) {
	e := nuevoGaze(0.94, 0.94)
	sinOjos := features(map[string]float64{"face_area_ratio": 0.08}, true)
	for i := range 10 {
		if e.observe(sinOjos, evalT0.Add(time.Duration(i)*66*time.Millisecond)) {
			t.Fatal("cumplido sin medida de mirada")
		}
	}
}

// Dos ojos que no coinciden son un iris mal detectado, no una mirada rara.
func TestMiradaConOjosEnDesacuerdoSeIgnora(t *testing.T) {
	e := nuevoGazeTarget(challenge.GazeTarget{X: 0.06, Y: 0.5})
	at := evalT0
	for range sostenerMuestras + 2 {
		at = at.Add(66 * time.Millisecond)
		f := features(map[string]float64{
			"gaze_offset_x": 0.05, "pose_yaw_deg": 0,
			"gaze_openness": aperturaBase, "gaze_agreement": 0.1,
		}, true)
		if e.observe(f, at) {
			t.Fatal("cumplido con los dos ojos en desacuerdo")
		}
	}
}

// La referencia buena es la mirada de ANTES de que apareciera el objetivo.
func TestReferenciaDeMiradaIgnoraUnAtipico(t *testing.T) {
	window := make([]gazePoint, 0, gazeReferenceFrames)
	for range gazeReferenceFrames - 1 {
		window = append(window, gazePoint{deg: 6})
	}
	window = append(window, gazePoint{deg: 90}) // frame con el iris perdido

	base, ok := gazeReference(window)
	if !ok {
		t.Fatal("ventana llena y sin referencia")
	}
	if base.deg > 8 {
		t.Errorf("la referencia se fue con el atípico: %+v", base)
	}
}

func TestSinVentanaNoHayReferenciaPrevia(t *testing.T) {
	if _, ok := gazeReference([]gazePoint{{deg: 5}}); ok {
		t.Error("se dio por buena una referencia de un solo frame")
	}
}

// El gateway NO emite ventana de destello, ni siquiera para avisar de la luz
// ambiente.
//
// Lo hacía, y su aviso se apoyaba en una correlación propia peor que la de
// Python. Python ya toma esa decisión —y en el orden correcto, mirando la
// correlación ANTES que la amplitud— así que una segunda opinión aquí sólo
// servía para que ganara la peor.
//
// Que un destello sin respuesta lleve a reintentar y nunca a acusación sigue
// garantizado: lo hace `analyzer/flash.py`, y lo vigila
// `tests/test_flash_ambient_gate.py` del analizador.
func TestElGatewayNoOpinaSobreElDestello(t *testing.T) {
	casos := []struct {
		nombre string
		depth  float64
	}{
		{"sin respuesta ninguna", 0.0},
		{"respuesta aplastada por la luz", 0.02},
		{"respuesta clarísima", 0.50},
	}

	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			step := flashStep()
			e := newEvaluator(step, evalT0)
			playFlash(e, step, c.depth, true)

			if _, ok := e.window(true); ok {
				t.Error("el gateway emitió ventana de destello: eso le pisa la medida a Python")
			}
		})
	}
}

// Una correlación fuerte demuestra por sí sola que había señal que medir, por
// poco que subiera el brillo. Preguntarlo al revés —comprobar primero la
// subida— declaraba no medible una secuencia que se había seguido con una
// correlación de 0,93, medida con una cámara real.
func TestUnaCorrelacionFuerteNoSeDescartaPorPocaSubida(t *testing.T) {
	step := flashStep()
	e := newEvaluator(step, evalT0)

	// Modulación pequeña: la subida de brillo se queda muy por debajo del
	// umbral de ambiente, pero la serie sigue a la secuencia.
	playFlash(e, step, 0.05, true)

	score, _, ok := e.flashCorrelation()
	if !ok || score < flashMinCorrelation {
		t.Fatalf("la correlación no llegó al umbral: %.2f", score)
	}
	rise, _ := e.flashRise()
	if rise >= flashMinRise {
		t.Fatalf("el test necesita una subida por debajo del umbral, y fue %.3f", rise)
	}

	// Con la correlación por encima del umbral no hay nada que avisar, así
	// que no se emite ventana: la medida del destello la trae Python.
	if w, okw := e.window(true); okw && !w.QualitySufficient {
		t.Errorf("declarada no medible con correlación %.2f: la correlación manda", score)
	}
}

// Una mirada que no se sostuvo no puede reportarse como perfecta.
//
// El caso real: avance de 6,17° sobre un mínimo de 5° —o sea por encima del
// umbral— pero con racha de 1 cuando hacen falta 3 de los últimos 5. El paso
// se rechazó y la ventana reportó `response = 1,0`. La ventana contradecía a
// su propio paso, y le daba a la fusión justo la señal que el paso acababa de
// negar.
//
// Un pico aislado es lo que la regla del sostenimiento existe para descartar:
// el ruido de los landmarks del iris los produce en cualquier dirección.
func TestUnaMiradaNoSostenidaNoValeComoBuena(t *testing.T) {
	target := challenge.GazeTarget{X: 0.06, Y: 0.5}

	sostenida := nuevoGazeTarget(target)
	if !mirar(sostenida, +0.045, 0) {
		t.Fatal("el caso de control debería cumplirse")
	}
	wOK, _ := sostenida.window(true)

	// Un paso que NO se cumplió no puede reportar una respuesta alta, aunque
	// alguna muestra suelta llegara lejos.
	pico := nuevoGazeTarget(target)
	mirarDesde(pico, 0, 0, 0, 0) // se queda quieto: no cumple
	pico.gazeBestShift = 6.17    // pero una muestra suelta llegó por encima
	wPico, _ := pico.window(false)

	if wOK.Submetrics["response"] == nil || *wOK.Submetrics["response"] < 0.9 {
		t.Fatalf("una mirada sostenida debería reportarse alta, dio %v",
			wOK.Submetrics["response"])
	}
	got := wPico.Submetrics["response"]
	if got == nil {
		t.Fatal("una mirada no sostenida sigue siendo una medida, no un hueco")
	}
	if *got > gazeUnsustainedCap {
		t.Errorf("un pico aislado reportó %.3f: la ventana contradice a su paso", *got)
	}
}
