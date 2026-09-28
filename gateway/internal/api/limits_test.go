package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/edisonpaul4/biometrics/gateway/internal/analyzer"
	"github.com/edisonpaul4/biometrics/gateway/internal/api"
	"github.com/edisonpaul4/biometrics/gateway/internal/wsproto"
)

// TestFrameTooLargeClosesConnection: el tamaño máximo de frame es un límite
// duro; superarlo corta la conexión con código propio.
func TestFrameTooLargeClosesConnection(t *testing.T) {
	ts, metrics := newTestServer(t, func(cfg *api.Config) {
		cfg.Limits.MaxFrameBytes = 2048
	})
	ticket := createSession(t, ts)

	c := dialClient(t, ts)
	c.hello(ticket.Token)
	if m, ok := c.next(5 * time.Second); !ok || m.typ != wsproto.TypeServerHello {
		t.Fatalf("no llegó server_hello: %+v", m)
	}

	oversized := wsproto.AppendFrame(nil, wsproto.FrameHeader{
		Encoding: wsproto.EncodingSyntheticScene, Seq: 1, CapturedAtUS: time.Now().UnixMicro(),
	}, bytes.Repeat([]byte("x"), 4096))

	if err := c.writeRaw(websocket.MessageBinary, oversized); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got := c.waitClosed(5 * time.Second); got != websocket.StatusCode(wsproto.CloseFrameTooLarge) {
		t.Errorf("código de cierre %d, se esperaba %d", got, wsproto.CloseFrameTooLarge)
	}
	if got := metrics.Snapshot()["frames_too_large"]; got != 1 {
		t.Errorf("frames_too_large = %d, se esperaba 1", got)
	}
}

// TestControlMessageTooLargeClosesConnection: mismo trato para el canal de
// control.
func TestControlMessageTooLargeClosesConnection(t *testing.T) {
	ts, metrics := newTestServer(t, func(cfg *api.Config) {
		cfg.Limits.MaxControlBytes = 512
	})
	ticket := createSession(t, ts)

	c := dialClient(t, ts)
	c.hello(ticket.Token)
	if m, ok := c.next(5 * time.Second); !ok || m.typ != wsproto.TypeServerHello {
		t.Fatalf("no llegó server_hello: %+v", m)
	}

	huge, err := json.Marshal(wsproto.ClientTelemetry{
		Type:        wsproto.TypeClientTelemetry,
		CameraState: string(bytes.Repeat([]byte("a"), 2048)),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := c.writeRaw(websocket.MessageText, huge); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got := c.waitClosed(5 * time.Second); got != websocket.StatusCode(wsproto.CloseProtocolViolation) {
		t.Errorf("código de cierre %d, se esperaba %d", got, wsproto.CloseProtocolViolation)
	}
	if got := metrics.Snapshot()["protocol_violations"]; got == 0 {
		t.Error("no se contó la violación de protocolo")
	}
}

// TestBackpressureDropsIntermediateFrames: con un analizador lento, el
// gateway descarta los frames intermedios en vez de acumular cola.
func TestBackpressureDropsIntermediateFrames(t *testing.T) {
	ts, metrics := newTestServer(t, func(cfg *api.Config) {
		// 80 ms por frame frente a los 20 ms del cliente: cuatro de cada
		// cinco frames sobran.
		cfg.Analyzer = &analyzer.Stub{Delay: 80 * time.Millisecond}
	})
	ticket := createSession(t, ts)

	c := dialClient(t, ts)
	c.hello(ticket.Token)
	if m, ok := c.next(5 * time.Second); !ok || m.typ != wsproto.TypeServerHello {
		t.Fatalf("no llegó server_hello: %+v", m)
	}
	c.startCamera()

	time.Sleep(1200 * time.Millisecond)
	c.stop()

	snap := metrics.Snapshot()
	if snap["frames_received"] < 20 {
		t.Fatalf("sólo llegaron %d frames; el test no prueba nada", snap["frames_received"])
	}
	if snap["frames_dropped_backpressure"] == 0 {
		t.Errorf("no se descartó ningún frame con un analizador 4 veces más lento que la cámara "+
			"(recibidos %d, analizados %d)", snap["frames_received"], snap["frames_analyzed"])
	}
	if snap["frames_analyzed"] >= snap["frames_received"] {
		t.Errorf("se analizaron %d de %d frames: no hubo descarte",
			snap["frames_analyzed"], snap["frames_received"])
	}
	// Lo importante: lo recibido se descarta o se analiza, nunca se acumula.
	accounted := snap["frames_analyzed"] + snap["frames_dropped_backpressure"] +
		snap["frames_dropped_rate_limit"] + snap["frames_dropped_out_of_order"]
	if accounted > snap["frames_received"] {
		t.Errorf("la contabilidad no cuadra: %d contabilizados de %d recibidos",
			accounted, snap["frames_received"])
	}
	// La cota es el buzón MÁS los frames en vuelo hacia el analizador: uno
	// que se envió y todavía no ha devuelto medidas no está analizado ni
	// descartado, y de ésos caben MaxInflight. Con la cota en 2 el test
	// fallaba de vez en cuando —visto: 3 sin contabilizar— por contar sólo el
	// buzón. Lo que se comprueba sigue siendo lo mismo: que lo que no se
	// analiza se descarta y nunca se acumula una cola sin tope.
	const enVuelo = 1 + 4 // buzón + conn.DefaultLimits().MaxInflight
	if diff := snap["frames_received"] - accounted; diff > enVuelo {
		t.Errorf("%d frames sin contabilizar: ¿se está acumulando cola?", diff)
	}
}

// TestRateLimitDropsExcessFrames: por encima de los FPS máximos se descarta,
// pero no se corta la sesión: un pico de cámara no es un ataque.
func TestRateLimitDropsExcessFrames(t *testing.T) {
	ts, metrics := newTestServer(t, func(cfg *api.Config) {
		cfg.Limits.MaxFPS = 5 // el cliente bombea a 50
	})
	ticket := createSession(t, ts)

	c := dialClient(t, ts)
	c.hello(ticket.Token)
	if m, ok := c.next(5 * time.Second); !ok || m.typ != wsproto.TypeServerHello {
		t.Fatalf("no llegó server_hello: %+v", m)
	}
	c.startCamera()

	time.Sleep(800 * time.Millisecond)

	snap := metrics.Snapshot()
	if snap["frames_dropped_rate_limit"] == 0 {
		t.Errorf("no se descartó nada a 50 fps con un límite de 5 (recibidos %d)",
			snap["frames_received"])
	}
	if got := c.status(); got != -1 {
		t.Errorf("la conexión se cerró con %d; un exceso de caudal no debe cortar la sesión", got)
	}
}

// TestOutOfOrderFramesAreDropped: repetir el número de secuencia no sirve
// para colar frames viejos.
func TestOutOfOrderFramesAreDropped(t *testing.T) {
	ts, metrics := newTestServer(t, nil)
	ticket := createSession(t, ts)

	c := dialClient(t, ts)
	c.hello(ticket.Token)
	if m, ok := c.next(5 * time.Second); !ok || m.typ != wsproto.TypeServerHello {
		t.Fatalf("no llegó server_hello: %+v", m)
	}
	c.setBadSeq()
	c.startCamera()

	time.Sleep(500 * time.Millisecond)

	if got := metrics.Snapshot()["frames_dropped_out_of_order"]; got == 0 {
		t.Error("no se descartó ningún frame repetido")
	}
}

// TestImplausibleClientClockIsIgnoredButSessionWorks: la deriva del reloj del
// cliente se valida y se anota, pero no puntúa. La sesión sale adelante
// porque quien manda es el reloj del servidor.
func TestImplausibleClientClockIsIgnored(t *testing.T) {
	ts, metrics := newTestServer(t, nil)
	ticket := createSession(t, ts)

	c := dialClient(t, ts)
	c.hello(ticket.Token)
	if m, ok := c.next(5 * time.Second); !ok || m.typ != wsproto.TypeServerHello {
		t.Fatalf("no llegó server_hello: %+v", m)
	}
	// Dos horas de desfase: imposible.
	c.setSkew(func(now time.Time) time.Time { return now.Add(-2 * time.Hour) })

	tr := playSession(t, c)

	if tr.result.Decision != wsproto.DecisionLive {
		t.Errorf("decisión %q: el reloj del cliente no debería influir en el veredicto",
			tr.result.Decision)
	}
	if got := metrics.Snapshot()["client_clock_implausible"]; got == 0 {
		t.Error("no se anotó ninguna deriva inverosímil")
	}
}

// TestSessionBudgetExpires: pasado el presupuesto, la sesión se cierra con
// veredicto no concluyente y código explícito.
func TestSessionBudgetExpires(t *testing.T) {
	ts, metrics := newTestServer(t, func(cfg *api.Config) {
		cfg.Limits.SessionBudget = 400 * time.Millisecond
	})
	ticket := createSession(t, ts)

	c := dialClient(t, ts)
	c.hello(ticket.Token)
	if m, ok := c.next(5 * time.Second); !ok || m.typ != wsproto.TypeServerHello {
		t.Fatalf("no llegó server_hello: %+v", m)
	}
	// El cliente no envía un solo frame.

	var result wsproto.ServerResult
	found := false
	for range 5 {
		m, ok := c.next(3 * time.Second)
		if !ok {
			break
		}
		if m.typ == wsproto.TypeServerResult {
			if err := json.Unmarshal(m.data, &result); err != nil {
				t.Fatalf("server_result ilegible: %v", err)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no llegó server_result al expirar")
	}
	if result.Decision != wsproto.DecisionInconclusive {
		t.Errorf("decisión %q, se esperaba inconclusive", result.Decision)
	}
	if result.ReasonKey != "session_expired" {
		t.Errorf("reason_key %q, se esperaba session_expired", result.ReasonKey)
	}
	if got := c.waitClosed(5 * time.Second); got != websocket.StatusCode(wsproto.CloseSessionExpired) {
		t.Errorf("código de cierre %d, se esperaba %d", got, wsproto.CloseSessionExpired)
	}
	if got := metrics.Snapshot()["sessions_expired"]; got != 1 {
		t.Errorf("sessions_expired = %d, se esperaba 1", got)
	}
}

// TestBadHandshakeIsRejected: el primer mensaje tiene que ser un client_hello.
func TestBadHandshakeIsRejected(t *testing.T) {
	ts, _ := newTestServer(t, nil)

	c := dialClient(t, ts)
	go c.readPump()
	if err := c.writeRaw(websocket.MessageText, []byte(`{"type":"client_telemetry"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got := c.waitClosed(5 * time.Second); got != websocket.StatusCode(wsproto.CloseProtocolViolation) {
		t.Errorf("código de cierre %d, se esperaba %d", got, wsproto.CloseProtocolViolation)
	}
}

// TestUnexpectedControlMessageIsRejected: durante la sesión sólo se admiten
// los mensajes del contrato.
func TestUnexpectedControlMessageIsRejected(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	ticket := createSession(t, ts)

	c := dialClient(t, ts)
	c.hello(ticket.Token)
	if m, ok := c.next(5 * time.Second); !ok || m.typ != wsproto.TypeServerHello {
		t.Fatalf("no llegó server_hello: %+v", m)
	}
	if err := c.writeRaw(websocket.MessageText, []byte(`{"type":"inventado"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got := c.waitClosed(5 * time.Second); got != websocket.StatusCode(wsproto.CloseProtocolViolation) {
		t.Errorf("código de cierre %d, se esperaba %d", got, wsproto.CloseProtocolViolation)
	}
}

// TestClientAbortClosesCleanly: el cliente puede abandonar, y eso no es un
// aprobado ni un suspenso.
func TestClientAbortClosesCleanly(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	ticket := createSession(t, ts)

	c := dialClient(t, ts)
	c.hello(ticket.Token)
	if m, ok := c.next(5 * time.Second); !ok || m.typ != wsproto.TypeServerHello {
		t.Fatalf("no llegó server_hello: %+v", m)
	}
	c.writeJSON(wsproto.ClientAbort{Type: wsproto.TypeClientAbort, Reason: "user_cancelled"})

	var result wsproto.ServerResult
	for range 4 {
		m, ok := c.next(3 * time.Second)
		if !ok {
			break
		}
		if m.typ == wsproto.TypeServerResult {
			_ = json.Unmarshal(m.data, &result)
			break
		}
	}
	if result.Decision != wsproto.DecisionInconclusive {
		t.Errorf("decisión %q, se esperaba inconclusive", result.Decision)
	}
	if got := c.waitClosed(5 * time.Second); got != websocket.StatusCode(wsproto.CloseClientAbort) {
		t.Errorf("código de cierre %d, se esperaba %d", got, wsproto.CloseClientAbort)
	}
}

// TestPhotoAttackDoesNotPass: una superficie plana que no responde a los
// retos no supera la sesión. No es el banco de pruebas de verdad —el stub es
// sintético— pero cierra el circuito: reto sin respuesta, plazo agotado,
// veredicto no concluyente.
func TestPhotoAttackDoesNotPass(t *testing.T) {
	ts, _ := newTestServer(t, func(cfg *api.Config) {
		// Plazos cortos para no esperar en balde.
		p := fastPolicy()
		p.PoseDeadline = 600 * time.Millisecond
		p.FlashSlack = 400 * time.Millisecond
		cfg.Policy = p
	})
	ticket := createSession(t, ts)

	c := dialClient(t, ts)
	c.hello(ticket.Token)
	if m, ok := c.next(5 * time.Second); !ok || m.typ != wsproto.TypeServerHello {
		t.Fatalf("no llegó server_hello: %+v", m)
	}

	// Una foto: hay "rostro", pero es plano y no se mueve ni responde a la
	// luz de la pantalla.
	c.setScene(func(time.Time) analyzer.Scene {
		return analyzer.Scene{Face: true, FaceAreaRatio: 0.22, Flat: true, Sharpness: 600}
	})
	c.startCamera()

	var result wsproto.ServerResult
	found := false
	for range 12 {
		m, ok := c.next(5 * time.Second)
		if !ok {
			break
		}
		if m.typ == wsproto.TypeServerResult {
			if err := json.Unmarshal(m.data, &result); err != nil {
				t.Fatalf("server_result ilegible: %v", err)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("la sesión no terminó")
	}
	if result.Decision == wsproto.DecisionLive {
		t.Errorf("una foto plana superó la prueba de vida (motivo %q)", result.ReasonKey)
	}
	if result.ReasonKey == "response_timeout" {
		t.Error("el motivo público le está diciendo al atacante qué falló")
	}
}

func TestHealthAndMetricsEndpoints(t *testing.T) {
	ts, _ := newTestServer(t, nil)

	res, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("/healthz devolvió %d", res.StatusCode)
	}

	createSession(t, ts)

	mres, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer mres.Body.Close()
	body, _ := io.ReadAll(mres.Body)

	for _, want := range []string{
		"liveness_gateway_sessions_issued 1",
		"liveness_gateway_frames_dropped_backpressure 0",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics no expone %q:\n%s", want, body)
		}
	}
}

// Frames capturados con separación correcta pero LLEGADOS a ráfagas no se
// descartan por ritmo.
//
// El caso real, medido por un túnel: el cliente capturó 172 frames a 29,4 fps,
// llegaron los 172, y el gateway tiró **72** —el 42 %— porque limitaba el
// caudal por la hora de LLEGADA. Una red con jitter entrega varios frames en
// el mismo milisegundo aunque se capturaran con 33 ms de separación.
//
// El efecto en el usuario no era sutil: el reto de mirada se quedó con 7 fps
// efectivos, y una respuesta impecable de medio segundo —trece frames entre
// 22° y 40°— se resolvió en fallo por no alcanzar la racha de 3 de 5.
//
// El sello del cliente sólo se usa si su reloj es plausible, que ya se valida.
func TestUnaRafagaNoSeDescartaPorRitmo(t *testing.T) {
	ts, metrics := newTestServer(t, nil)
	ticket := createSession(t, ts)
	c := dialClient(t, ts)
	c.hello(ticket.Token)
	if first, ok := c.next(5 * time.Second); !ok || first.typ != wsproto.TypeServerHello {
		t.Fatal("no llegó server_hello")
	}

	// Diez frames capturados con el JITTER de una cámara real —intervalos de
	// 24 a 45 ms para una media de 30 fps— y enviados de golpe.
	//
	// El jitter es la mitad del caso: medido sobre una sesión real, el 47 % de
	// los intervalos de captura caía por debajo de los 33,33 ms que impone un
	// tope de 30 fps. Un limitador sin margen descarta la mitad de los frames
	// de un cliente que está haciendo exactamente lo que se le pidió.
	base := time.Now()
	for i := 1; i <= 10; i++ {
		payload, err := json.Marshal(neutralScene(base))
		if err != nil {
			t.Fatalf("escena ilegible: %v", err)
		}
		frame := wsproto.AppendFrame(nil, wsproto.FrameHeader{
			Encoding:     wsproto.EncodingSyntheticScene,
			Seq:          uint64(i),
			CapturedAtUS: base.Add(time.Duration(i) * 26 * time.Millisecond).UnixMicro(),
		}, payload)
		if err := c.writeRaw(websocket.MessageBinary, frame); err != nil {
			t.Fatalf("no se pudo enviar el frame %d: %v", i, err)
		}
	}

	// Se le da margen al servidor para procesarlos.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && metrics.FramesReceived.Load() < 10 {
		time.Sleep(20 * time.Millisecond)
	}

	tirados := metrics.FramesDroppedRateLimit.Load()
	if tirados > 2 {
		t.Errorf("se descartaron %d de 10 frames de una ráfaga: el limitador está "+
			"midiendo la hora de llegada en vez del sello de captura", tirados)
	}
}
