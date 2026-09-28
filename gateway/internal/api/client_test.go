package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/edisonpaul4/biometrics/gateway/internal/analyzer"
	"github.com/edisonpaul4/biometrics/gateway/internal/api"
	"github.com/edisonpaul4/biometrics/gateway/internal/conn"
	"github.com/edisonpaul4/biometrics/gateway/internal/telemetry"
	"github.com/edisonpaul4/biometrics/gateway/internal/wsproto"
	"github.com/edisonpaul4/biometrics/orchestrator/core/challenge"
	"github.com/edisonpaul4/biometrics/orchestrator/core/clock"
	"github.com/edisonpaul4/biometrics/orchestrator/core/fusion"
)

// --- servidor de pruebas -----------------------------------------------------

// fastPolicy escala los tiempos del guion para que un test tarde ~1 s en vez
// de ~20. Las proporciones se mantienen: la calibración sigue siendo una
// espera impuesta, el destello sigue durando más que su secuencia y la
// reacción mínima sigue existiendo.
func fastPolicy() challenge.Policy {
	return challenge.Policy{
		CalibrationHold:  200 * time.Millisecond,
		CalibrationSlack: 2 * time.Second,

		MinSteps: 2,
		MaxSteps: 4,

		MinFlashColors: 3,
		MaxFlashColors: 5,

		MinFlashSegment: 90 * time.Millisecond,
		MaxFlashSegment: 120 * time.Millisecond,
		FlashSlack:      2 * time.Second,

		MinReaction:  60 * time.Millisecond,
		PoseDeadline: 3 * time.Second,

		GazeDeadline: 2 * time.Second,
		GazeMargin:   0.08,
		// La permanencia en el primer punto tiene que dar tiempo a que la
		// fase de salida junte muestras después del asentamiento. Sin ella el
		// reto no tiene fase de salida y no se puede medir nada.
		GazeDwell: 700 * time.Millisecond,
	}
}

func testLimits() conn.Limits {
	l := conn.DefaultLimits()
	l.MaxFPS = 100
	l.SessionBudget = 20 * time.Second
	l.TickInterval = 20 * time.Millisecond
	return l
}

// frameInterval es el ritmo de la cámara simulada.
const frameInterval = 20 * time.Millisecond

func newTestServer(t *testing.T, mutate func(*api.Config)) (*httptest.Server, *telemetry.Metrics) {
	t.Helper()

	metrics := telemetry.New()
	profile, err := fusion.LoadFile("../../../deploy/policy/decision-profile.yaml")
	if err != nil {
		t.Fatalf("no se pudo cargar el perfil desplegado: %v", err)
	}
	engine, err := fusion.New(profile, clock.System{})
	if err != nil {
		t.Fatalf("perfil desplegado inválido: %v", err)
	}

	cfg := api.Config{
		Fusion:                  engine,
		Metrics:                 metrics,
		Limits:                  testLimits(),
		Policy:                  fastPolicy(),
		Analyzer:                analyzer.NewStub(),
		InsecureSkipOriginCheck: true,
	}
	if mutate != nil {
		mutate(&cfg)
	}

	ts := httptest.NewServer(api.NewServer(cfg).Handler())
	t.Cleanup(ts.Close)
	return ts, metrics
}

// createSession pide un ticket por la API REST.
func createSession(t *testing.T, ts *httptest.Server) api.CreateSessionResponse {
	t.Helper()

	res, err := http.Post(ts.URL+"/v1/sessions", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /v1/sessions: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("POST /v1/sessions: %d %s", res.StatusCode, body)
	}

	var out api.CreateSessionResponse
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("respuesta ilegible: %v", err)
	}
	if out.SessionID == "" || out.Token == "" {
		t.Fatalf("ticket incompleto: %+v", out)
	}
	return out
}

// --- cliente de pruebas ------------------------------------------------------

type inbound struct {
	typ  wsproto.Type
	data []byte
}

// testClient simula un cliente de cámara: una goroutine lee, otra bombea
// frames, y la escena que pinta depende del reto que tenga delante.
type testClient struct {
	t  *testing.T
	ws *websocket.Conn

	msgs     chan inbound
	readDone chan struct{}
	pumpStop chan struct{}
	pumpDone chan struct{}

	mu      sync.Mutex
	scene   func(time.Time) analyzer.Scene
	seq     uint64
	skewFn  func(time.Time) time.Time
	badSeq  bool
	stopped bool
	camera  bool

	closeStatus websocket.StatusCode
	closeReason string
}

func dialClient(t *testing.T, ts *httptest.Server) *testClient {
	t.Helper()

	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/liveness"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	c := &testClient{
		t:           t,
		ws:          ws,
		msgs:        make(chan inbound, 64),
		readDone:    make(chan struct{}),
		pumpStop:    make(chan struct{}),
		pumpDone:    make(chan struct{}),
		scene:       neutralScene,
		closeStatus: -1,
	}
	t.Cleanup(func() { c.stop() })
	return c
}

// hello envía el saludo. Se hace antes de arrancar el bombeo para que sólo
// haya una goroutine escribiendo en el socket.
func (c *testClient) hello(token string) {
	c.t.Helper()
	c.writeJSON(wsproto.ClientHello{
		Type:            wsproto.TypeClientHello,
		ProtocolVersion: wsproto.ProtocolVersion,
		SessionToken:    token,
		Capabilities:    &wsproto.Capabilities{Encodings: []string{"jpeg"}, MaxFPS: 30, Flash: true},
	})
	go c.readPump()
}

// startCamera arranca el bombeo de frames.
func (c *testClient) startCamera() {
	c.mu.Lock()
	c.camera = true
	c.mu.Unlock()
	go c.framePump()
}

func (c *testClient) readPump() {
	defer close(c.readDone)
	for {
		typ, data, err := c.ws.Read(context.Background())
		if err != nil {
			c.mu.Lock()
			c.closeStatus = websocket.CloseStatus(err)
			c.closeReason = closeReasonOf(err)
			c.mu.Unlock()
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		mt, err := wsproto.PeekType(data)
		if err != nil {
			continue
		}
		select {
		case c.msgs <- inbound{typ: mt, data: append([]byte(nil), data...)}:
		default:
		}
	}
}

func (c *testClient) framePump() {
	defer close(c.pumpDone)
	ticker := time.NewTicker(frameInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.pumpStop:
			return
		case now := <-ticker.C:
			c.mu.Lock()
			sceneFn := c.scene
			c.seq++
			seq := c.seq
			if c.badSeq {
				seq = 1
			}
			skew := c.skewFn
			c.mu.Unlock()

			captured := now
			if skew != nil {
				captured = skew(now)
			}

			payload, err := json.Marshal(sceneFn(now))
			if err != nil {
				return
			}
			frame := wsproto.AppendFrame(nil, wsproto.FrameHeader{
				Encoding:     wsproto.EncodingSyntheticScene,
				Seq:          seq,
				CapturedAtUS: captured.UnixMicro(),
			}, payload)

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err = c.ws.Write(ctx, websocket.MessageBinary, frame)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// setSkew hace que el cliente selle sus frames con un reloj desviado.
func (c *testClient) setSkew(fn func(time.Time) time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.skewFn = fn
}

// setBadSeq hace que el cliente repita siempre el mismo número de secuencia.
func (c *testClient) setBadSeq() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.badSeq = true
}

func (c *testClient) setScene(fn func(time.Time) analyzer.Scene) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.scene = fn
}

func (c *testClient) writeJSON(v any) {
	c.t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		c.t.Fatalf("marshal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, data); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func (c *testClient) writeRaw(typ websocket.MessageType, data []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.ws.Write(ctx, typ, data)
}

// next espera el siguiente mensaje del servidor.
func (c *testClient) next(timeout time.Duration) (inbound, bool) {
	c.t.Helper()
	select {
	case m := <-c.msgs:
		return m, true
	case <-time.After(timeout):
		return inbound{}, false
	}
}

// waitClosed espera a que el servidor cierre y devuelve el código.
func (c *testClient) waitClosed(timeout time.Duration) websocket.StatusCode {
	c.t.Helper()
	select {
	case <-c.readDone:
		return c.status()
	case <-time.After(timeout):
		c.t.Fatal("el servidor no cerró la conexión a tiempo")
		return -1
	}
}

// status devuelve el código de cierre visto por el lector.
func (c *testClient) status() websocket.StatusCode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeStatus
}

func (c *testClient) stop() {
	c.mu.Lock()
	already := c.stopped
	c.stopped = true
	camera := c.camera
	c.mu.Unlock()
	if already {
		return
	}
	close(c.pumpStop)
	if camera {
		select {
		case <-c.pumpDone:
		case <-time.After(2 * time.Second):
		}
	}
	c.ws.CloseNow()
}

func closeReasonOf(err error) string {
	var ce websocket.CloseError
	if ok := asCloseError(err, &ce); ok {
		return ce.Reason
	}
	return ""
}

// --- escenas -----------------------------------------------------------------

// neutralScene es un rostro real, quieto y centrado.
func neutralScene(time.Time) analyzer.Scene {
	return analyzer.Scene{
		Face:          true,
		FaceAreaRatio: 0.07,
		Sharpness:     600,
		Brightness:    0.55,
	}
}

// poseRamp devuelve una escena en la que el usuario ejecuta la pose poco a
// poco, como una persona: si llegara instantáneamente al ángulo pedido, la
// máquina de estados lo rechazaría por imposible.
const poseRampDuration = 400 * time.Millisecond

func poseScene(action string, start time.Time) func(time.Time) analyzer.Scene {
	return func(now time.Time) analyzer.Scene {
		progress := float64(now.Sub(start)) / float64(poseRampDuration)
		if progress > 1 {
			progress = 1
		}
		if progress < 0 {
			progress = 0
		}

		s := neutralScene(now)
		switch action {
		// Signo desde la CÁMARA, que mira al sujeto de frente: girar hacia la
		// propia izquierda lleva la nariz a la derecha de la imagen y da un
		// ángulo positivo. Ver TestPoseThresholds en internal/conn.
		case "yaw_left":
			s.YawDeg = 30 * progress
		case "yaw_right":
			s.YawDeg = -30 * progress
		case "pitch_up":
			// Barbilla arriba es pitch negativo desde la cámara.
			s.PitchDeg = -25 * progress
		case "move_closer":
			// Una cara de webcam ocupa en torno al 7 % del encuadre, no el 20 %
			// de un maniquí a pantalla completa.
			//
			// Y se acerca DE VERDAD: hasta 0,17, no 0,12. El umbral pide 1,3
			// veces el tamaño del PRIMER frame observado, así que con un tope
			// de 0,12 el margen era 1,71 y bastaba con que el primer frame
			// llegara a mitad del movimiento —la referencia sube, el objetivo
			// sube con ella— para que el paso fuera inalcanzable. Salía como
			// un fallo intermitente de 2 de cada 5 ejecuciones. Alguien que se
			// acerca a la pantalla dobla largamente el área de su cara.
			//
			// La rampa espera un cuarto de la ventana antes de empezar. La
			// referencia se toma del PRIMER frame observado, así que si ese
			// frame cae con el movimiento ya empezado la referencia sube y el
			// objetivo sube con ella. Sin esa espera el test fallaba de forma
			// intermitente según cuándo llegara el primer frame, y no por
			// nada del código que se está probando.
			// Espera el 40 % de la ventana antes de empezar, y se acerca
			// hasta 2,3 veces su tamaño.
			//
			// La referencia se toma del PRIMER frame observado, así que un
			// primer frame que llegue con el movimiento empezado la sube, y
			// con ella el objetivo. Con poco recorrido eso hacía fallar el
			// test una de cada cinco veces según la carga de la máquina, y no
			// por nada del código que se prueba. Alguien a quien se le pide
			// acercarse dobla largamente el área de su cara.
			ramp := max(0.0, (progress-0.4)/0.6)
			s.FaceAreaRatio = 0.07 + 0.09*ramp
		}
		return s
	}
}

// flashScene pinta la pantalla con la secuencia impuesta, cada color durante
// lo que toca.
func flashScene(seq []wsproto.FlashSegment, start time.Time) func(time.Time) analyzer.Scene {
	return func(now time.Time) analyzer.Scene {
		s := neutralScene(now)
		elapsed := now.Sub(start)

		var acc time.Duration
		for _, seg := range seq {
			acc += time.Duration(seg.DurationMS) * time.Millisecond
			if elapsed < acc {
				s.ScreenColor = seg.Color
				return s
			}
		}
		return s
	}
}
