package api_test

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/edisonpaul4/biometrics/gateway/internal/analyzer"
	"github.com/edisonpaul4/biometrics/gateway/internal/api"
	"github.com/edisonpaul4/biometrics/gateway/internal/bus"
	"github.com/edisonpaul4/biometrics/gateway/internal/bus/echoworker"
	"github.com/edisonpaul4/biometrics/gateway/internal/bus/natstest"
	"github.com/edisonpaul4/biometrics/gateway/internal/conn"
	"github.com/edisonpaul4/biometrics/gateway/internal/telemetry"
	"github.com/edisonpaul4/biometrics/gateway/internal/wsproto"
	"github.com/edisonpaul4/biometrics/orchestrator/core/clock"
	"github.com/edisonpaul4/biometrics/orchestrator/core/fusion"
)

// natsPoolOptions: leases cortos. La sesión entera dura segundos, así que
// esperar a ver si el worker revive no sirve de nada.
func natsPoolOptions() bus.PoolOptions {
	return bus.PoolOptions{
		AnnounceTTL:  2 * time.Second,
		LeaseTimeout: 500 * time.Millisecond,
		LeaseTTL:     500 * time.Millisecond,
		LeaseCheck:   50 * time.Millisecond,
		Candidates:   3,
		FeatureQueue: 32,
	}
}

type natsRig struct {
	server  *httptest.Server
	metrics *telemetry.Metrics
	worker  *echoworker.Worker
	url     string
}

// newNATSRig monta el circuito completo: NATS, un worker y el gateway
// hablando con él por el bus.
func newNATSRig(t *testing.T, workers int, mutate func(*api.Config), wopts *echoworker.Options) *natsRig {
	t.Helper()

	url := natstest.Start(t)
	gwConn := natstest.Connect(t, url)
	metrics := telemetry.New()

	pool, err := bus.NewPool(gwConn, natsPoolOptions(), nil, metrics, nil)
	if err != nil {
		t.Fatalf("bus.NewPool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	var first *echoworker.Worker
	for i := range workers {
		opts := echoworker.Options{
			ID:                "worker-" + string(rune('a'+i)),
			Capacity:          4,
			AnnounceInterval:  100 * time.Millisecond,
			HeartbeatInterval: 100 * time.Millisecond,
		}
		if wopts != nil {
			opts.Analyzer = wopts.Analyzer
			if wopts.Capacity > 0 {
				opts.Capacity = wopts.Capacity
			}
		}
		w, err := echoworker.New(natstest.Connect(t, url), opts)
		if err != nil {
			t.Fatalf("echoworker.New: %v", err)
		}
		if err := w.Start(); err != nil {
			t.Fatalf("worker.Start: %v", err)
		}
		t.Cleanup(w.Stop)
		if first == nil {
			first = w
		}
	}

	// El gateway no puede asignar sesiones hasta oír a alguien.
	deadline := time.Now().Add(3 * time.Second)
	for len(pool.Workers()) < workers {
		if time.Now().After(deadline) {
			t.Fatalf("sólo se anunciaron %d de %d workers", len(pool.Workers()), workers)
		}
		time.Sleep(10 * time.Millisecond)
	}

	limits := testLimits()
	limits.FrameTimeout = 500 * time.Millisecond
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
		Limits:                  limits,
		Policy:                  fastPolicy(),
		Analyzers:               pool,
		InsecureSkipOriginCheck: true,
	}
	if mutate != nil {
		mutate(&cfg)
	}

	ts := httptest.NewServer(api.NewServer(cfg).Handler())
	t.Cleanup(ts.Close)

	return &natsRig{server: ts, metrics: metrics, worker: first, url: url}
}

// TestFullSessionOverNATS: el mismo recorrido de siempre, pero con el
// análisis al otro lado del bus.
func TestFullSessionOverNATS(t *testing.T) {
	rig := newNATSRig(t, 1, nil, nil)
	ticket := createSession(t, rig.server)

	c := dialClient(t, rig.server)
	c.hello(ticket.Token)

	first, ok := c.next(5 * time.Second)
	if !ok || first.typ != wsproto.TypeServerHello {
		t.Fatalf("no llegó server_hello: %+v", first)
	}

	tr := playSession(t, c)

	if tr.result.Decision != wsproto.DecisionLive {
		t.Errorf("decisión %q, se esperaba live (motivo %q)", tr.result.Decision, tr.result.ReasonKey)
	}
	if got := c.waitClosed(5 * time.Second); got != websocket.StatusCode(wsproto.CloseSessionComplete) {
		t.Errorf("código de cierre %d, se esperaba CloseSessionComplete", got)
	}

	snap := rig.metrics.Snapshot()
	if snap["leases_acquired"] != 1 {
		t.Errorf("leases_acquired = %d, se esperaba 1", snap["leases_acquired"])
	}
	if snap["frames_published"] == 0 {
		t.Error("no se publicó ningún frame en el bus")
	}
	if snap["features_received"] == 0 {
		t.Error("no volvieron medidas por el bus")
	}
	if snap["leases_lost"] != 0 {
		t.Errorf("leases_lost = %d en una sesión sana", snap["leases_lost"])
	}

	// Al cerrar la sesión, el worker suelta su estado caliente.
	deadline := time.Now().Add(3 * time.Second)
	for rig.worker.Sessions() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("el worker sigue con %d sesiones abiertas", rig.worker.Sessions())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestWorkerDeathAbortsSessionCleanly es el criterio de aceptación: se cae el
// worker a mitad de sesión y la sesión se cierra sola, con motivo de
// infraestructura y sin intentar recuperar nada.
func TestWorkerDeathAbortsSessionCleanly(t *testing.T) {
	rig := newNATSRig(t, 1, nil, nil)
	ticket := createSession(t, rig.server)

	c := dialClient(t, rig.server)
	c.hello(ticket.Token)

	first, ok := c.next(5 * time.Second)
	if !ok || first.typ != wsproto.TypeServerHello {
		t.Fatalf("no llegó server_hello: %+v", first)
	}
	c.startCamera()

	var (
		killed     bool
		killedAt   time.Time
		result     wsproto.ServerResult
		challenges int
	)

	for {
		m, ok := c.next(10 * time.Second)
		if !ok {
			t.Fatal("la sesión se quedó colgada tras caerse el worker: nadie la cerró")
		}

		switch m.typ {
		case wsproto.TypeServerChallenge:
			challenges++
			ch := decodeChallenge(t, m.data)
			c.setScene(sceneForChallenge(t, ch))

			// Se mata al worker con la sesión ya en marcha: calibración
			// superada y un reto de verdad activo.
			if !killed && ch.Kind != "calibration" {
				killed = true
				killedAt = time.Now()
				rig.worker.Stop()
			}

		case wsproto.TypeServerChallengeEnd:
			c.setScene(neutralScene)

		case wsproto.TypeServerResult:
			result = decodeResult(t, m.data)

		case wsproto.TypeServerError:
			t.Fatalf("se esperaba un cierre limpio, llegó server_error: %s", m.data)
		}

		if result.Decision != "" {
			break
		}
	}

	if !killed {
		t.Fatal("el guion no llegó a ningún reto: el test no probó la caída")
	}

	// 1. Veredicto no concluyente con motivo de infraestructura.
	if result.Decision != wsproto.DecisionInconclusive {
		t.Errorf("decisión %q, se esperaba inconclusive: el usuario no ha hecho nada mal",
			result.Decision)
	}
	if result.ReasonKey != conn.ReasonKeyInfrastructure {
		t.Errorf("reason_key %q, se esperaba %q", result.ReasonKey, conn.ReasonKeyInfrastructure)
	}

	// 2. Cierre con el código explícito de infraestructura.
	if got := c.waitClosed(5 * time.Second); got != websocket.StatusCode(wsproto.CloseInfrastructureError) {
		t.Errorf("código de cierre %d, se esperaba CloseInfrastructureError", got)
	}

	// 3. Se detectó rápido: el lease es corto porque la sesión es corta.
	if elapsed := time.Since(killedAt); elapsed > 5*time.Second {
		t.Errorf("se tardó %v en detectar la caída", elapsed)
	}

	// 4. Los contadores lo cuentan como lo que es.
	snap := rig.metrics.Snapshot()
	if snap["leases_lost"] != 1 {
		t.Errorf("leases_lost = %d, se esperaba 1", snap["leases_lost"])
	}
	if snap["sessions_aborted_infra"] != 1 {
		t.Errorf("sessions_aborted_infra = %d, se esperaba 1", snap["sessions_aborted_infra"])
	}
	if snap["sessions_completed"] != 0 {
		t.Errorf("sessions_completed = %d: un aborto no es una sesión completada", snap["sessions_completed"])
	}

	// 5. No se intentó recuperar: ni se pidió otro lease ni se reasignó.
	if snap["leases_acquired"] != 1 {
		t.Errorf("leases_acquired = %d: se intentó recuperar la sesión en vez de abortarla",
			snap["leases_acquired"])
	}
}

// TestSessionWithoutWorkersAbortsAtStart: si no hay a quién asignar la
// sesión, se dice enseguida y con el mismo motivo.
func TestSessionWithoutWorkersAbortsAtStart(t *testing.T) {
	url := natstest.Start(t)
	gwConn := natstest.Connect(t, url)
	metrics := telemetry.New()

	pool, err := bus.NewPool(gwConn, natsPoolOptions(), nil, metrics, nil)
	if err != nil {
		t.Fatalf("bus.NewPool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	ts := httptest.NewServer(api.NewServer(api.Config{
		Metrics:                 metrics,
		Limits:                  testLimits(),
		Policy:                  fastPolicy(),
		Analyzers:               pool, // sin ningún worker anunciado
		InsecureSkipOriginCheck: true,
	}).Handler())
	t.Cleanup(ts.Close)

	ticket := createSession(t, ts)
	c := dialClient(t, ts)
	c.hello(ticket.Token)

	var result wsproto.ServerResult
	for range 4 {
		m, ok := c.next(5 * time.Second)
		if !ok {
			break
		}
		if m.typ == wsproto.TypeServerResult {
			result = decodeResult(t, m.data)
			break
		}
	}

	if result.Decision != wsproto.DecisionInconclusive {
		t.Errorf("decisión %q, se esperaba inconclusive", result.Decision)
	}
	if result.ReasonKey != conn.ReasonKeyInfrastructure {
		t.Errorf("reason_key %q, se esperaba %q", result.ReasonKey, conn.ReasonKeyInfrastructure)
	}
	if got := c.waitClosed(5 * time.Second); got != websocket.StatusCode(wsproto.CloseInfrastructureError) {
		t.Errorf("código de cierre %d, se esperaba CloseInfrastructureError", got)
	}
	if got := metrics.Snapshot()["leases_failed"]; got == 0 {
		t.Error("no se contó el lease fallido")
	}
}

// TestSlowWorkerFramesTimeOut: un worker que tarda más de la cuenta no
// congela la sesión. Los frames sin respuesta se tiran y se cuentan.
func TestSlowWorkerFramesTimeOut(t *testing.T) {
	rig := newNATSRig(t, 1, func(cfg *api.Config) {
		cfg.Limits.FrameTimeout = 80 * time.Millisecond
	}, &echoworker.Options{
		Analyzer: &analyzer.Stub{Delay: 400 * time.Millisecond},
	})

	ticket := createSession(t, rig.server)
	c := dialClient(t, rig.server)
	c.hello(ticket.Token)

	if m, ok := c.next(5 * time.Second); !ok || m.typ != wsproto.TypeServerHello {
		t.Fatalf("no llegó server_hello: %+v", m)
	}
	c.startCamera()

	// La sesión tiene que terminar sola, no quedarse colgada.
	var result wsproto.ServerResult
	deadline := time.Now().Add(25 * time.Second)
	for result.Decision == "" && time.Now().Before(deadline) {
		m, ok := c.next(10 * time.Second)
		if !ok {
			break
		}
		if m.typ == wsproto.TypeServerResult {
			result = decodeResult(t, m.data)
		}
	}
	if result.Decision == "" {
		t.Fatal("la sesión no terminó con un analizador lento: se quedó bloqueada")
	}

	snap := rig.metrics.Snapshot()
	if snap["frames_timed_out"] == 0 {
		t.Errorf("ningún frame venció el plazo con un analizador 5 veces más lento "+
			"(publicados %d, medidas %d)", snap["frames_published"], snap["features_received"])
	}
	if snap["sessions_aborted_infra"] != 0 {
		t.Error("un analizador lento no es un analizador caído: no debía abortarse por infraestructura")
	}
}

// --- ayudas ------------------------------------------------------------------

func decodeResult(t *testing.T, data []byte) wsproto.ServerResult {
	t.Helper()
	var r wsproto.ServerResult
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("server_result ilegible: %v", err)
	}
	return r
}

func decodeChallenge(t *testing.T, data []byte) wsproto.ServerChallenge {
	t.Helper()
	var ch wsproto.ServerChallenge
	if err := json.Unmarshal(data, &ch); err != nil {
		t.Fatalf("server_challenge ilegible: %v", err)
	}
	return ch
}
