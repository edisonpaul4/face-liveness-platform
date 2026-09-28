package conn

import (
	"testing"
	"time"

	"github.com/edisonpaul4/biometrics/orchestrator/core/challenge"
	"github.com/edisonpaul4/biometrics/orchestrator/core/session"
)

// miradaReal son las medidas de un reto de mirada de una sesión REAL, sacadas
// de la grabación del cliente pasada otra vez por el analizador. El sujeto
// salió del punto de su derecha y viajó al de su izquierda, obedeciendo.
//
// Los tiempos van en el reloj del CLIENTE, contados desde que recibió el
// reto. Lo que el gateway ve es esto mismo desplazado por lo que tarde el
// frame en llegarle.
var miradaReal = []struct {
	ms   int
	iris float64
	yaw  float64
}{
	{386, -0.0204, 1.05}, {435, -0.0132, 0.28}, {534, -0.0129, 0.93},
	{633, -0.0094, 0.55}, {715, -0.0109, 0.41}, {732, -0.0069, 0.52},
	{781, -0.0196, 0.54},
	{1423, 0.0135, 0.33}, {1555, 0.0079, 1.71}, {1588, 0.0111, 1.49},
	{1638, 0.0040, 1.59}, {1687, 0.0158, 1.48}, {1769, 0.0148, 1.33},
	{1785, 0.0177, 0.64}, {1884, 0.0089, 2.19}, {1950, 0.0182, 2.22},
	{1983, 0.0115, 1.30}, {2049, 0.0049, 1.50}, {2131, 0.0143, 1.39},
	{2148, 0.0113, 0.76}, {2214, 0.0115, 1.26}, {2279, -0.0013, 0.98},
	{2345, -0.0051, 1.54}, {2411, -0.0032, 1.16}, {2477, 0.0045, 1.23},
	{2592, 0.0095, 1.43}, {2625, 0.0007, 1.75}, {2690, 0.0057, 2.06},
}

// jugarMiradaReal reproduce esa sesión con todos sus frames llegando con
// `retardo` de más, que es lo que hace un enlace de subida saturado.
func jugarMiradaReal(t *testing.T, retardo time.Duration) *evaluator {
	t.Helper()
	e := newEvaluator(session.RevealedStep{
		Kind:     challenge.KindGaze,
		Gaze:     challenge.GazeTarget{X: 0.06, Y: 0.5},
		GazeFrom: challenge.GazeTarget{X: 0.94, Y: 0.5},
		Hold:     900 * time.Millisecond,
	}, evalT0)
	for _, m := range miradaReal {
		at := evalT0.Add(time.Duration(m.ms)*time.Millisecond + retardo)
		e.observe(gazeFeatures(m.iris, m.yaw), at)
	}
	return e
}

// Un frame que llega tarde no es un frame perdido: es un frame que MIENTE
// sobre cuándo pasó lo que muestra. Como el reto parte sus dos fases por el
// reloj del servidor, con los frames atrasados la fase de salida se llena con
// la mirada de llegada y el avance se desploma a cero.
//
// Pasó de verdad: el móvil pedía 18,5 Mbit/s de subida —155 KB por frame a
// 17,9 fps—, el enlace no daba, y el retraso creció 2,0 s en 5,4 s de sesión.
// Esta persona obedeció y la sesión la mandó a reintentar con avance CERO.
//
// El cliente ya no encola sin tope, pero el retardo no desaparece: se acota.
// Por eso el reparto se BUSCA, igual que el destello busca el suyo — y por eso
// se busca hasta 0,8 s y no más: un retardo de dos segundos no se arregla
// buscándolo, porque para entonces la respuesta llega después del plazo del
// reto. Eso se arregla no encolando, que es lo que hace el cliente ahora.
func TestMiradaSobreviveAFramesTardios(t *testing.T) {
	for _, retardo := range []time.Duration{0, 200, 400, 700} {
		e := jugarMiradaReal(t, retardo*time.Millisecond)
		if e.gazeBestShift < gazeMinAdvanceDeg {
			t.Errorf("con %v de retardo el avance fue %.2f°, por debajo del mínimo de %.1f",
				retardo*time.Millisecond, e.gazeBestShift, gazeMinAdvanceDeg)
		}
		if !e.gazeMeasured() {
			t.Errorf("con %v de retardo el reto salió como no medible", retardo*time.Millisecond)
		}
	}
}

// La búsqueda no puede fabricar respuesta donde no la hubo: si el sujeto se
// queda quieto, ningún reparto separa dos poblaciones porque sólo hay una.
func TestLaBusquedaNoInventaMiradas(t *testing.T) {
	e := newEvaluator(session.RevealedStep{
		Kind:     challenge.KindGaze,
		Gaze:     challenge.GazeTarget{X: 0.06, Y: 0.5},
		GazeFrom: challenge.GazeTarget{X: 0.94, Y: 0.5},
		Hold:     900 * time.Millisecond,
	}, evalT0)
	for ms := 0; ms < 3300; ms += 66 {
		e.observe(gazeFeatures(0.001, 0.2), evalT0.Add(time.Duration(ms)*time.Millisecond))
	}
	if e.gazeBestShift >= gazeMinAdvanceDeg {
		t.Errorf("una mirada quieta dio %.2f° de avance", e.gazeBestShift)
	}
}

// Irse al lado CONTRARIO es no cumplir; no es no haberse podido medir. Si se
// confunden, una respuesta que sí dice algo se cae de la fusión.
func TestMirarAlLadoContrarioSeMideIgual(t *testing.T) {
	e := jugarMiradaReal(t, 0)
	// El mismo viaje, pero el reto pedía el otro lado.
	e2 := newEvaluator(session.RevealedStep{
		Kind:     challenge.KindGaze,
		Gaze:     challenge.GazeTarget{X: 0.94, Y: 0.5},
		GazeFrom: challenge.GazeTarget{X: 0.06, Y: 0.5},
		Hold:     900 * time.Millisecond,
	}, evalT0)
	for _, m := range miradaReal {
		e2.observe(gazeFeatures(m.iris, m.yaw), evalT0.Add(time.Duration(m.ms)*time.Millisecond))
	}
	if e2.gazeBestShift >= gazeMinAdvanceDeg {
		t.Errorf("mirar al lado contrario dio %.2f° de avance", e2.gazeBestShift)
	}
	if !e2.gazeMeasured() {
		t.Error("mirar al lado contrario salió como NO MEDIBLE: eso es no cumplir, no es no poder medir")
	}
	if !e.gazeMeasured() {
		t.Error("la mirada correcta salió como no medible")
	}
}

// Un parón de la red en el momento del salto no es una respuesta nula: es una
// respuesta que nadie vio. Puntuarla como cero es inventar evidencia contra
// quien a lo mejor obedeció.
//
// Medido: el móvil dejó de capturar 1,8 s justo en el salto y el reto se cerró
// con avance CERO, con la mirada entrando en la fusión como respuesta nula.
func TestUnParonEnElSaltoNoSePuntua(t *testing.T) {
	e := newEvaluator(session.RevealedStep{
		Kind:     challenge.KindGaze,
		Gaze:     challenge.GazeTarget{X: 0.94, Y: 0.5},
		GazeFrom: challenge.GazeTarget{X: 0.06, Y: 0.5},
		Hold:     900 * time.Millisecond,
	}, evalT0)
	// Serie real de la sesión que lo destapó: seis muestras antes del parón,
	// hueco de 785 ms, y doce después.
	serie := []struct {
		ms   int
		iris float64
	}{
		{99, -0.003}, {130, -0.003}, {279, 0.008}, {577, -0.004}, {684, -0.004},
		{833, -0.002},
		{1618, 0.022}, {1764, 0.007}, {1862, 0.014}, {2008, 0.001}, {2055, 0.015},
		{2200, 0.003}, {2348, 0.005}, {2494, -0.010}, {2592, 0.009}, {2741, 0.031},
		{2794, 0.005}, {3087, 0.005},
	}
	for _, m := range serie {
		e.observe(gazeFeatures(m.iris, 0), evalT0.Add(time.Duration(m.ms)*time.Millisecond))
	}
	if e.gazeMeasured() {
		t.Error("un parón de 785 ms sobre el salto se dio por medido")
	}
	w, _ := e.window(false)
	if w.QualitySufficient {
		t.Error("la ventana entró en la fusión pese a no haberse podido medir")
	}
	if w.Submetrics["response"] != nil {
		t.Error("se reportó una respuesta de una mirada que no se pudo ver")
	}
}
