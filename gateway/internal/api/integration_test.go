package api_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/edisonpaul4/biometrics/gateway/internal/analyzer"
	"github.com/edisonpaul4/biometrics/gateway/internal/api"
	"github.com/edisonpaul4/biometrics/gateway/internal/conn"
	"github.com/edisonpaul4/biometrics/gateway/internal/wsproto"
)

// sessionTrace es lo que el cliente vio durante una sesión.
type sessionTrace struct {
	types      []wsproto.Type
	challenges []wsproto.ServerChallenge
	result     wsproto.ServerResult
	bodies     [][]byte
}

// playSession recorre una sesión entera respondiendo a lo que pida el
// servidor, y devuelve la traza de mensajes.
func playSession(t *testing.T, c *testClient) sessionTrace {
	t.Helper()

	var tr sessionTrace
	c.startCamera()

	for {
		m, ok := c.next(15 * time.Second)
		if !ok {
			t.Fatalf("el servidor dejó de hablar; traza hasta ahora: %v", tr.types)
		}
		tr.types = append(tr.types, m.typ)
		tr.bodies = append(tr.bodies, m.data)

		switch m.typ {
		case wsproto.TypeServerChallenge:
			var ch wsproto.ServerChallenge
			if err := json.Unmarshal(m.data, &ch); err != nil {
				t.Fatalf("server_challenge ilegible: %v", err)
			}
			tr.challenges = append(tr.challenges, ch)
			c.setScene(sceneForChallenge(t, ch))

		case wsproto.TypeServerChallengeEnd:
			c.setScene(neutralScene)

		case wsproto.TypeServerResult:
			if err := json.Unmarshal(m.data, &tr.result); err != nil {
				t.Fatalf("server_result ilegible: %v", err)
			}
			return tr

		case wsproto.TypeServerError:
			t.Fatalf("el servidor devolvió error: %s", m.data)
		}
	}
}

// sceneForChallenge construye lo que el usuario haría delante de la cámara.
// playSessionPassive obedece la calibración y luego se queda quieto.
//
// Es el sujeto que no responde: los retos se cierran sin cumplirse, y el
// primero que se rechaza corta el guion. Sirve para comprobar que un guion
// cortado no se decide como si estuviera completo.
func playSessionPassive(t *testing.T, c *testClient) sessionTrace {
	t.Helper()

	var tr sessionTrace
	c.startCamera()
	c.setScene(neutralScene)

	for {
		m, ok := c.next(20 * time.Second)
		if !ok {
			t.Fatalf("el servidor dejó de hablar; traza: %v", tr.types)
		}
		tr.types = append(tr.types, m.typ)
		switch m.typ {
		case wsproto.TypeServerChallenge:
			var ch wsproto.ServerChallenge
			if err := json.Unmarshal(m.data, &ch); err != nil {
				t.Fatalf("server_challenge ilegible: %v", err)
			}
			tr.challenges = append(tr.challenges, ch)
			// Pase lo que pase, escena neutra: no se responde a nada.
			c.setScene(neutralScene)
		case wsproto.TypeServerResult:
			if err := json.Unmarshal(m.data, &tr.result); err != nil {
				t.Fatalf("server_result ilegible: %v", err)
			}
			return tr
		case wsproto.TypeServerError:
			t.Fatalf("el servidor devolvió error: %s", m.data)
		}
	}
}

func sceneForChallenge(t *testing.T, ch wsproto.ServerChallenge) func(time.Time) analyzer.Scene {
	t.Helper()
	start := time.Now()

	switch ch.Kind {
	case "calibration":
		return neutralScene

	case "hold":
		// El tramo de quietud se supera estando: una cara delante y sin
		// moverse. Es la misma escena que la calibración porque es
		// exactamente lo mismo que se le pide al sujeto.
		return neutralScene

	case "pose":
		var p wsproto.PoseParams
		if err := json.Unmarshal(ch.Params, &p); err != nil {
			t.Fatalf("params de pose ilegibles: %v", err)
		}
		return poseScene(p.Action, start)

	case "flash":
		var p wsproto.FlashParams
		if err := json.Unmarshal(ch.Params, &p); err != nil {
			t.Fatalf("params de destello ilegibles: %v", err)
		}
		return flashScene(p.Sequence, start)

	case "gaze":
		var p wsproto.GazeParams
		if err := json.Unmarshal(ch.Params, &p); err != nil {
			t.Fatalf("params de mirada ilegibles: %v", err)
		}
		return gazeScene(p, start)

	default:
		t.Fatalf("tipo de reto desconocido: %q", ch.Kind)
		return neutralScene
	}
}

// TestFullSessionHappyPath es el criterio de aceptación: una sesión entera
// contra el stub, comprobando la secuencia de mensajes.
func TestFullSessionHappyPath(t *testing.T) {
	ts, metrics := newTestServer(t, nil)
	ticket := createSession(t, ts)

	c := dialClient(t, ts)
	c.hello(ticket.Token)

	// 1. El primer mensaje es siempre el saludo del servidor.
	first, ok := c.next(5 * time.Second)
	if !ok {
		t.Fatal("no llegó server_hello")
	}
	if first.typ != wsproto.TypeServerHello {
		t.Fatalf("el primer mensaje fue %s, se esperaba server_hello", first.typ)
	}
	var hello wsproto.ServerHello
	if err := json.Unmarshal(first.data, &hello); err != nil {
		t.Fatalf("server_hello ilegible: %v", err)
	}
	if hello.SessionID != ticket.SessionID {
		t.Errorf("session_id = %q, se esperaba %q", hello.SessionID, ticket.SessionID)
	}
	if hello.Capture.FPS <= 0 {
		t.Error("el servidor no impuso parámetros de captura")
	}

	tr := playSession(t, c)

	// 2. La secuencia es reto/fin de reto alternados y un único resultado.
	if len(tr.types) < 3 {
		t.Fatalf("traza demasiado corta: %v", tr.types)
	}
	for i, typ := range tr.types[:len(tr.types)-1] {
		want := wsproto.TypeServerChallenge
		if i%2 == 1 {
			want = wsproto.TypeServerChallengeEnd
		}
		if typ != want {
			t.Fatalf("posición %d: %s, se esperaba %s. Traza: %v", i, typ, want, tr.types)
		}
	}
	if last := tr.types[len(tr.types)-1]; last != wsproto.TypeServerResult {
		t.Fatalf("el último mensaje fue %s, se esperaba server_result", last)
	}
	if got := countType(tr.types, wsproto.TypeServerResult); got != 1 {
		t.Errorf("hay %d server_result, se esperaba 1", got)
	}
	if len(tr.challenges) != countType(tr.types, wsproto.TypeServerChallengeEnd) {
		t.Errorf("%d retos y %d cierres: no cuadran", len(tr.challenges),
			countType(tr.types, wsproto.TypeServerChallengeEnd))
	}

	// 3. La forma del guion: calibración primero, un tramo de quietud, entre
	//    2 y 4 retos, y al menos un destello.
	// El "acércate" de antes del destello no es un reto: es encuadre, y no
	// cuenta para MinSteps ni MaxSteps. Se descuenta aquí por la misma razón
	// que la calibración y el tramo de quietud no inflan la cuenta.
	revelados := 0
	for _, ch := range tr.challenges {
		var pp wsproto.PoseParams
		if ch.Kind == "pose" && json.Unmarshal(ch.Params, &pp) == nil && pp.Action == "move_closer" {
			continue
		}
		revelados++
	}
	if revelados < 4 || revelados > 6 {
		t.Fatalf("%d pasos revelados, se esperaban entre 4 y 6 (%d con el acércate)",
			revelados, len(tr.challenges))
	}
	if tr.challenges[0].Kind != "calibration" {
		t.Errorf("el primer paso fue %q, se esperaba calibración", tr.challenges[0].Kind)
	}
	// Exactamente un tramo de quietud, y antes de cualquier destello: es
	// donde se mide el pulso, y dentro de un destello no se puede.
	holds, vistoDestello := 0, false
	for _, ch := range tr.challenges {
		switch ch.Kind {
		case "flash":
			vistoDestello = true
		case "hold":
			holds++
			if vistoDestello {
				t.Error("el tramo de quietud llegó después de un destello")
			}
		}
	}
	if holds != 1 {
		t.Errorf("%d tramos de quietud, se esperaba 1", holds)
	}
	flashes := 0
	ids := map[string]bool{}
	for i, ch := range tr.challenges {
		if ch.Kind == "flash" {
			flashes++
		}
		if ch.DeadlineMS <= 0 {
			t.Errorf("reto %d sin plazo", i)
		}
		if ids[ch.ChallengeID] {
			t.Errorf("identificador de reto repetido: %s", ch.ChallengeID)
		}
		ids[ch.ChallengeID] = true
		if i > 0 && ch.Kind == "calibration" {
			t.Errorf("calibración repetida en la posición %d", i)
		}
	}
	if flashes == 0 {
		t.Error("ningún reto de destello: siempre debe haber al menos uno")
	}

	// 4. El veredicto.
	if tr.result.Decision != wsproto.DecisionLive {
		t.Errorf("decisión %q, se esperaba live (motivo %q)", tr.result.Decision, tr.result.ReasonKey)
	}
	if tr.result.SessionID != ticket.SessionID {
		t.Errorf("session_id del resultado = %q", tr.result.SessionID)
	}

	// 5. Cierre limpio con código explícito.
	if got := c.waitClosed(5 * time.Second); got != websocket.StatusCode(wsproto.CloseSessionComplete) {
		t.Errorf("código de cierre %d, se esperaba %d", got, wsproto.CloseSessionComplete)
	}

	// 6. Contadores.
	snap := metrics.Snapshot()
	for _, k := range []string{"sessions_issued", "sessions_started", "sessions_completed"} {
		if snap[k] != 1 {
			t.Errorf("%s = %d, se esperaba 1", k, snap[k])
		}
	}
	if snap["frames_analyzed"] == 0 {
		t.Error("no se analizó ningún frame")
	}
	if snap["protocol_violations"] != 0 {
		t.Errorf("hubo %d violaciones de protocolo en una sesión limpia", snap["protocol_violations"])
	}
}

// TestNoMessageLeaksTheScript comprueba en el cable lo que los tests del
// núcleo comprueban en los tipos: por el WebSocket no sale nada del guion
// más allá del paso activo.
func TestNoMessageLeaksTheScript(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	ticket := createSession(t, ts)

	c := dialClient(t, ts)
	c.hello(ticket.Token)
	if _, ok := c.next(5 * time.Second); !ok {
		t.Fatal("no llegó server_hello")
	}
	tr := playSession(t, c)

	forbidden := []string{"seed", "index", "total", "script", "next_challenge", "remaining", "steps"}
	for i, body := range tr.bodies {
		lower := strings.ToLower(string(body))
		for _, bad := range forbidden {
			if strings.Contains(lower, bad) {
				t.Errorf("el mensaje %d menciona %q: %s", i, bad, body)
			}
		}
	}

	// Y ningún reto se anuncia antes de tiempo: la PRIMERA vez que un
	// identificador de reto aparece en el cable tiene que ser en su propio
	// server_challenge, nunca antes.
	for j, ch := range tr.challenges {
		firstAt := -1
		for k, body := range tr.bodies {
			if strings.Contains(string(body), ch.ChallengeID) {
				firstAt = k
				break
			}
		}
		if firstAt < 0 {
			t.Fatalf("el reto %d no aparece en la traza", j)
		}
		var env wsproto.Envelope
		if err := json.Unmarshal(tr.bodies[firstAt], &env); err != nil {
			t.Fatalf("mensaje %d ilegible: %v", firstAt, err)
		}
		if env.Type != wsproto.TypeServerChallenge {
			t.Errorf("el reto %d (%s) se filtró antes de tiempo, en un %s",
				j, ch.ChallengeID, env.Type)
		}
	}
}

// TestSessionTokenIsSingleUse es el segundo criterio de aceptación.
func TestSessionTokenIsSingleUse(t *testing.T) {
	ts, metrics := newTestServer(t, nil)
	ticket := createSession(t, ts)

	// Primer uso: el ticket se canjea y la sesión arranca.
	first := dialClient(t, ts)
	first.hello(ticket.Token)
	m, ok := first.next(5 * time.Second)
	if !ok || m.typ != wsproto.TypeServerHello {
		t.Fatalf("el primer uso debería arrancar la sesión, llegó %+v", m)
	}
	first.stop()

	// Segundo uso con el MISMO token: rechazado.
	second := dialClient(t, ts)
	second.hello(ticket.Token)

	m, ok = second.next(5 * time.Second)
	if !ok {
		t.Fatal("el servidor no contestó al segundo intento")
	}
	if m.typ != wsproto.TypeServerError {
		t.Fatalf("segundo intento: llegó %s, se esperaba server_error", m.typ)
	}
	var serr wsproto.ServerError
	if err := json.Unmarshal(m.data, &serr); err != nil {
		t.Fatalf("server_error ilegible: %v", err)
	}
	if serr.Code != wsproto.ErrorUnauthorized {
		t.Errorf("código %q, se esperaba %q", serr.Code, wsproto.ErrorUnauthorized)
	}

	if got := second.waitClosed(5 * time.Second); got != websocket.StatusCode(wsproto.CloseUnauthorized) {
		t.Errorf("código de cierre %d, se esperaba %d", got, wsproto.CloseUnauthorized)
	}

	if got := metrics.Snapshot()["sessions_rejected_reuse"]; got != 1 {
		t.Errorf("sessions_rejected_reuse = %d, se esperaba 1", got)
	}

	// Y un tercer intento sigue fallando: no se "descongela" con el tiempo.
	third := dialClient(t, ts)
	third.hello(ticket.Token)
	m, ok = third.next(5 * time.Second)
	if !ok || m.typ != wsproto.TypeServerError {
		t.Fatalf("tercer intento: %+v", m)
	}
	if got := metrics.Snapshot()["sessions_rejected_reuse"]; got != 2 {
		t.Errorf("sessions_rejected_reuse = %d, se esperaba 2", got)
	}
}

func TestUnknownTokenIsRejected(t *testing.T) {
	ts, _ := newTestServer(t, nil)

	c := dialClient(t, ts)
	c.hello("00000000000000000000000000000000")

	m, ok := c.next(5 * time.Second)
	if !ok || m.typ != wsproto.TypeServerError {
		t.Fatalf("llegó %+v, se esperaba server_error", m)
	}
	if got := c.waitClosed(5 * time.Second); got != websocket.StatusCode(wsproto.CloseUnauthorized) {
		t.Errorf("código de cierre %d, se esperaba %d", got, wsproto.CloseUnauthorized)
	}
}

func countType(types []wsproto.Type, want wsproto.Type) int {
	n := 0
	for _, t := range types {
		if t == want {
			n++
		}
	}
	return n
}

// TestDefaultSessionBudget fija el límite pedido.
//
// Subió de 45 s a 60 cuando el guion incorporó el tramo de quietud de once
// segundos que hace medible el pulso, y de 60 a 75 al alargar los tramos de
// destello: el mínimo pasó de 350 ms a 800 para que el rango de búsqueda de
// retardo quepa DENTRO de un tramo. Sin eso el buscador desliza el estímulo
// un tramo entero y encaja una secuencia sobre su contraria, medido.
//
// El presupuesto cubre el PEOR caso —cada reto agotando su plazo—, que pasó
// de 41 a 46 s. Una sesión real sigue durando entre 20 y 30.
func TestDefaultSessionBudget(t *testing.T) {
	if got := conn.DefaultLimits().SessionBudget; got != 60*time.Second {
		t.Errorf("presupuesto de sesión por defecto = %v, se esperaba 60s", got)
	}
}

var _ = api.Config{}

// gazeScene simula a alguien que mira al punto que le acaban de poner.
//
// El objetivo viene en coordenadas de PANTALLA y el iris se mide en
// coordenadas de IMAGEN, con la cámara mirando al sujeto de frente: para
// mirar a la izquierda de su pantalla, el iris se va a la DERECHA de la
// imagen. El eje X se invierte; el Y no.
func gazeScene(params wsproto.GazeParams, start time.Time) func(time.Time) analyzer.Scene {
	// Apertura de párpado en reposo. El canal vertical de la mirada se mide
	// como cambio RELATIVO contra ella.
	const apertura = 0.30

	return func(now time.Time) analyzer.Scene {
		s := neutralScene(now)

		// El reto tiene DOS puntos: el maniquí mira al primero mientras dura
		// la permanencia, y viaja al segundo cuando salta.
		//
		// 450 ms de reacción, por encima del mínimo humano de la política.
		//
		// Estaba en 250 y eso es MÁS RÁPIDO de lo que el sistema considera
		// humanamente posible, así que el maniquí merecía que lo pillaran: la
		// sesión acababa en `temporal_response_too_fast` en cuanto el
		// sostenimiento se cumplía pronto. Con la regla vieja hacían falta tres
		// muestras (~316 ms) y con la nueva bastan dos (~283 ms), las dos por
		// debajo del mínimo: el fallo pasó de raro a frecuente, pero llevaba
		// ahí desde el principio.
		const reaccion = 450 * time.Millisecond
		dwell := time.Duration(params.DwellMS) * time.Millisecond

		// Sólo hay eje horizontal: la mirada se mide por el desplazamiento
		// del iris, y el vertical se descartó por no ser medible con una
		// webcam frontal (ver challenge.GazeTarget).
		mirarA := func(x float64) float64 {
			if x >= 0.5 {
				return -0.06 // pantalla derecha -> iris a la izquierda de la imagen
			}
			return 0.06
		}

		elapsed := now.Sub(start)
		switch {
		case elapsed < reaccion:
			// Aún no ha reaccionado al primer punto.
			s.EyeOpenness = apertura
		case elapsed < dwell:
			s.GazeX = mirarA(params.FromX)
		case elapsed < dwell+reaccion:
			// El punto acaba de saltar y la mirada aún va en camino.
			s.GazeX = mirarA(params.FromX)
		default:
			s.GazeX = mirarA(params.TargetX)
		}
		return s
	}
}

// Un guion cortado por un fallo NO cuenta como completado.
//
// El agujero real: una sesión de 4,6 segundos, con la calibración no medible y
// su único reto FALLADO, se resolvió en **pass con score 0,902**. Un paso
// rechazado cierra la fase de retos (§6) y eso llevaba la máquina al mismo
// estado que agotar el guion, así que `require_completed_challenges` daba por
// completo un guion cortado a la fuerza — y la fusión decidía con la evidencia
// de dos pasos en vez de ocho.
//
// Las dos cosas terminan la fase; sólo una significa que hay evidencia.
func TestUnGuionCortadoNoCuentaComoCompletado(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	ticket := createSession(t, ts)
	c := dialClient(t, ts)
	c.hello(ticket.Token)

	if first, ok := c.next(5 * time.Second); !ok || first.typ != wsproto.TypeServerHello {
		t.Fatal("no llegó server_hello")
	}

	// Se obedece la calibración y luego nada: el primer reto que se rechaza
	// corta el guion.
	tr := playSessionPassive(t, c)

	if tr.result.Decision == "" {
		t.Fatal("la sesión no produjo resultado")
	}
	if tr.result.Decision == wsproto.DecisionLive {
		t.Errorf("un guion cortado por un fallo se resolvió en %q con la evidencia "+
			"de %d pasos: eso es decidir a ciegas",
			tr.result.Decision, len(tr.challenges))
	}
}
