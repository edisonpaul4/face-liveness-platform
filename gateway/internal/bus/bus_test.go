package bus_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/edisonpaul4/biometrics/gateway/internal/analyzer"
	"github.com/edisonpaul4/biometrics/gateway/internal/bus"
	"github.com/edisonpaul4/biometrics/gateway/internal/bus/echoworker"
	"github.com/edisonpaul4/biometrics/gateway/internal/bus/natstest"
	"github.com/edisonpaul4/biometrics/gateway/internal/telemetry"
)

func fastPoolOptions() bus.PoolOptions {
	return bus.PoolOptions{
		AnnounceTTL:  2 * time.Second,
		LeaseTimeout: 500 * time.Millisecond,
		LeaseTTL:     500 * time.Millisecond,
		LeaseCheck:   50 * time.Millisecond,
		Candidates:   3,
		FeatureQueue: 16,
	}
}

// newPool levanta NATS y un pool listo para asignar sesiones.
func newPool(t *testing.T, opts bus.PoolOptions) (string, *bus.Pool, *telemetry.Metrics) {
	t.Helper()

	url := natstest.Start(t)
	nc := natstest.Connect(t, url)
	met := telemetry.New()

	pool, err := bus.NewPool(nc, opts, nil, met, nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return url, pool, met
}

// startWorker arranca un worker eco con su propia conexión.
func startWorker(t *testing.T, url, id string, capacity int) *echoworker.Worker {
	t.Helper()

	nc := natstest.Connect(t, url)
	w, err := echoworker.New(nc, echoworker.Options{
		ID:                id,
		Capacity:          capacity,
		AnnounceInterval:  100 * time.Millisecond,
		HeartbeatInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("echoworker.New: %v", err)
	}
	if err := w.Start(); err != nil {
		t.Fatalf("worker.Start: %v", err)
	}
	t.Cleanup(w.Stop)
	return w
}

// waitFor espera a que se cumpla una condición, o falla.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no se cumplió a tiempo: %s", what)
}

// --- codec -------------------------------------------------------------------

func TestFrameTaskRoundTrip(t *testing.T) {
	want := bus.FrameTask{
		Encoding:     3,
		Flags:        bus.FlagClientClockTrusted,
		Seq:          987654321,
		CapturedAtUS: 1_755_000_000_000_000,
		ReceivedAtUS: 1_755_000_000_050_000,
		Payload:      []byte(`{"face":true}`),
	}

	got, err := bus.DecodeFrameTask(bus.EncodeFrameTask(want))
	if err != nil {
		t.Fatalf("DecodeFrameTask: %v", err)
	}
	if got.Encoding != want.Encoding || got.Seq != want.Seq ||
		got.CapturedAtUS != want.CapturedAtUS || got.ReceivedAtUS != want.ReceivedAtUS {
		t.Errorf("cabecera %+v, se esperaba %+v", got, want)
	}
	if string(got.Payload) != string(want.Payload) {
		t.Errorf("payload = %q", got.Payload)
	}
	if !got.ClientClockTrusted() {
		t.Error("se perdió la marca de reloj fiable")
	}
}

func TestDecodeFrameTaskRejectsGarbage(t *testing.T) {
	valid := bus.EncodeFrameTask(bus.FrameTask{Seq: 1, Payload: []byte("hola")})

	if _, err := bus.DecodeFrameTask(valid[:10]); !errors.Is(err, bus.ErrShortFrame) {
		t.Errorf("corto: err = %v", err)
	}

	badVersion := append([]byte(nil), valid...)
	badVersion[0] = 9
	if _, err := bus.DecodeFrameTask(badVersion); !errors.Is(err, bus.ErrFrameVersion) {
		t.Errorf("versión: err = %v", err)
	}

	badLen := append([]byte(nil), valid...)
	badLen[31] = 200
	if _, err := bus.DecodeFrameTask(badLen); !errors.Is(err, bus.ErrFrameLength) {
		t.Errorf("longitud: err = %v", err)
	}
}

// TestSubjectsAreScopedPerSession: los subjects atan cada sesión a los suyos.
func TestSubjectsAreScopedPerSession(t *testing.T) {
	if got := bus.Frames("01ABC"); got != "session.01ABC.frames" {
		t.Errorf("Frames = %q", got)
	}
	if got := bus.Features("01ABC"); got != "session.01ABC.features" {
		t.Errorf("Features = %q", got)
	}
	if got := bus.Lease("w1"); got != "analyzer.lease.w1" {
		t.Errorf("Lease = %q", got)
	}
}

// TestValidTokenBlocksSubjectInjection: un identificador con comodines
// permitiría suscribirse a sesiones ajenas.
func TestValidTokenBlocksSubjectInjection(t *testing.T) {
	malos := []string{"", "a.b", "*", ">", "ses ion", "a>b", "a\tb", string(make([]byte, 65))}
	for _, s := range malos {
		if bus.ValidToken(s) {
			t.Errorf("se aceptó el identificador peligroso %q", s)
		}
	}
	for _, s := range []string{"01J0ABC", "worker-1", "w_2"} {
		if !bus.ValidToken(s) {
			t.Errorf("se rechazó el identificador válido %q", s)
		}
	}
}

func TestAnnounceLoad(t *testing.T) {
	cases := []struct {
		a    bus.Announce
		want float64
	}{
		{bus.Announce{Capacity: 4, Sessions: 0}, 0},
		{bus.Announce{Capacity: 4, Sessions: 2}, 0.5},
		{bus.Announce{Capacity: 4, Sessions: 8}, 1},
		{bus.Announce{Capacity: 0}, 1}, // sin capacidad declarada: lleno
	}
	for _, c := range cases {
		if got := c.a.Load(); got != c.want {
			t.Errorf("Load(%+v) = %v, se esperaba %v", c.a, got, c.want)
		}
	}
}

// --- descubrimiento ----------------------------------------------------------

func TestDiscoveryPicksLeastLoadedWorker(t *testing.T) {
	url, pool, _ := newPool(t, fastPoolOptions())
	nc := natstest.Connect(t, url)

	// Tres anuncios a mano con cargas distintas.
	announce := func(id string, capacity, sessions int) {
		payload, err := json.Marshal(bus.Announce{
			WorkerID: id, Capacity: capacity, Sessions: sessions,
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := nc.Publish(bus.SubjectAnnounce, payload); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	announce("cargado", 4, 3)  // 0.75
	announce("libre", 4, 0)    // 0.00
	announce("a-medias", 4, 2) // 0.50
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	waitFor(t, 2*time.Second, "los tres anuncios", func() bool { return len(pool.Workers()) == 3 })

	got := pool.Workers()
	want := []string{"libre", "a-medias", "cargado"}
	for i, w := range got {
		if w.Announce.WorkerID != want[i] {
			t.Errorf("posición %d: %s, se esperaba %s (orden: %v)", i, w.Announce.WorkerID, want[i], got)
		}
	}
}

// TestStaleWorkersAreForgotten: si un worker deja de anunciarse, deja de ser
// candidato. No hace falta que nadie lo dé de baja.
func TestStaleWorkersAreForgotten(t *testing.T) {
	opts := fastPoolOptions()
	opts.AnnounceTTL = 200 * time.Millisecond

	url, pool, _ := newPool(t, opts)
	nc := natstest.Connect(t, url)

	payload, _ := json.Marshal(bus.Announce{WorkerID: "fugaz", Capacity: 4})
	if err := nc.Publish(bus.SubjectAnnounce, payload); err != nil {
		t.Fatalf("publish: %v", err)
	}
	_ = nc.Flush()

	waitFor(t, time.Second, "el anuncio", func() bool { return len(pool.Workers()) == 1 })
	waitFor(t, time.Second, "el olvido del anuncio", func() bool { return len(pool.Workers()) == 0 })
}

func TestAcquireWithoutWorkers(t *testing.T) {
	_, pool, met := newPool(t, fastPoolOptions())

	_, err := pool.Acquire(context.Background(), "01J0NADIE", time.Now().Add(time.Minute))
	if !errors.Is(err, analyzer.ErrNoWorkers) {
		t.Errorf("err = %v, se esperaba ErrNoWorkers", err)
	}
	if got := met.Snapshot()["leases_failed"]; got != 1 {
		t.Errorf("leases_failed = %d, se esperaba 1", got)
	}
}

func TestAcquireRejectsInvalidSessionID(t *testing.T) {
	_, pool, _ := newPool(t, fastPoolOptions())
	if _, err := pool.Acquire(context.Background(), "sesion.con.puntos", time.Now().Add(time.Minute)); err == nil {
		t.Error("se aceptó un identificador que rompe el ruteo por subject")
	}
}

// --- lease y afinidad --------------------------------------------------------

// TestSessionAffinityKeepsHotStateInTheWorker es el motivo de que exista la
// afinidad: el estado se acumula en el worker y los frames viajan pelados.
func TestSessionAffinityKeepsHotStateInTheWorker(t *testing.T) {
	url, pool, met := newPool(t, fastPoolOptions())
	w := startWorker(t, url, "worker-1", 4)

	waitFor(t, 2*time.Second, "el anuncio del worker", func() bool { return len(pool.Workers()) == 1 })

	const sessionID = "01J0AFINIDAD"
	an, err := pool.Acquire(context.Background(), sessionID, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = an.Close(context.Background()) }()

	if an.WorkerID() != "worker-1" {
		t.Errorf("worker asignado %q", an.WorkerID())
	}
	waitFor(t, time.Second, "la sesión en el worker", func() bool { return w.Sessions() == 1 })

	// Diez frames idénticos, sin nada de estado dentro.
	scene := []byte(`{"face":true,"face_area_ratio":0.2,"brightness":0.5}`)
	for seq := uint64(1); seq <= 10; seq++ {
		err := an.Submit(context.Background(), analyzer.Request{
			SessionID: sessionID, Seq: seq, Payload: scene, ReceivedAt: time.Now(),
		})
		if err != nil {
			t.Fatalf("Submit %d: %v", seq, err)
		}
	}

	// Las medidas llegan con un contador que sólo puede existir si el worker
	// recuerda los frames anteriores.
	var last analyzer.Features
	received := 0
	timeout := time.After(3 * time.Second)
	for received < 10 {
		select {
		case f := <-an.Features():
			received++
			last = f
		case <-timeout:
			t.Fatalf("sólo llegaron %d medidas de 10", received)
		}
	}

	if got := last.SignalOr("temporal_frames_seen", 0); got != 10 {
		t.Errorf("temporal_frames_seen = %v, se esperaba 10: el worker no acumuló estado", got)
	}
	if got := last.SignalOr("temporal_window_frames", 0); got != 10 {
		t.Errorf("temporal_window_frames = %v, se esperaba 10", got)
	}
	if got := w.FramesSeen(sessionID); got != 10 {
		t.Errorf("el worker vio %d frames, se esperaban 10", got)
	}

	snap := met.Snapshot()
	if snap["leases_acquired"] != 1 {
		t.Errorf("leases_acquired = %d", snap["leases_acquired"])
	}
	if snap["frames_published"] != 10 {
		t.Errorf("frames_published = %d", snap["frames_published"])
	}
	if snap["features_received"] != 10 {
		t.Errorf("features_received = %d", snap["features_received"])
	}
}

// TestCloseReleasesHotState: cerrar la sesión suelta la memoria del worker.
func TestCloseReleasesHotState(t *testing.T) {
	url, pool, _ := newPool(t, fastPoolOptions())
	w := startWorker(t, url, "worker-1", 4)
	waitFor(t, 2*time.Second, "el anuncio", func() bool { return len(pool.Workers()) == 1 })

	an, err := pool.Acquire(context.Background(), "01J0CIERRE", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	waitFor(t, time.Second, "la sesión en el worker", func() bool { return w.Sessions() == 1 })

	if err := an.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitFor(t, 2*time.Second, "la liberación del estado caliente", func() bool { return w.Sessions() == 0 })
}

// TestWorkerAtCapacityIsSkipped: un worker lleno rechaza el lease y la sesión
// se va al siguiente.
func TestWorkerAtCapacityIsSkipped(t *testing.T) {
	url, pool, _ := newPool(t, fastPoolOptions())
	full := startWorker(t, url, "aaa-lleno", 1)
	startWorker(t, url, "zzz-libre", 4)

	waitFor(t, 2*time.Second, "los dos anuncios", func() bool { return len(pool.Workers()) == 2 })

	first, err := pool.Acquire(context.Background(), "01J0UNO", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("primer Acquire: %v", err)
	}
	defer func() { _ = first.Close(context.Background()) }()
	waitFor(t, time.Second, "la primera sesión", func() bool { return full.Sessions()+1 >= 1 })

	// La segunda no cabe en el que tiene capacidad 1 si ya la gastó.
	second, err := pool.Acquire(context.Background(), "01J0DOS", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("segundo Acquire: %v", err)
	}
	defer func() { _ = second.Close(context.Background()) }()

	if first.WorkerID() == second.WorkerID() && full.Sessions() > 1 {
		t.Errorf("las dos sesiones cayeron en %s, que sólo admitía una", first.WorkerID())
	}
}

// TestLeaseIsLostWhenWorkerGoesSilent es el mecanismo del criterio de
// aceptación, aislado: si el worker calla, el lease se pierde.
func TestLeaseIsLostWhenWorkerGoesSilent(t *testing.T) {
	opts := fastPoolOptions()
	opts.LeaseTTL = 300 * time.Millisecond
	opts.LeaseCheck = 50 * time.Millisecond

	url, pool, met := newPool(t, opts)
	w := startWorker(t, url, "worker-1", 4)
	waitFor(t, 2*time.Second, "el anuncio", func() bool { return len(pool.Workers()) == 1 })

	an, err := pool.Acquire(context.Background(), "01J0CAIDA", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = an.Close(context.Background()) }()

	// Mientras late, no se pierde nada.
	select {
	case cause := <-an.Lost():
		t.Fatalf("lease perdido con el worker vivo: %v", cause)
	case <-time.After(500 * time.Millisecond):
	}

	w.Stop() // el worker se cae sin avisar

	select {
	case cause := <-an.Lost():
		if !errors.Is(cause, analyzer.ErrLost) {
			t.Errorf("causa = %v, se esperaba ErrLost", cause)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("el lease no se dio por perdido tras caerse el worker")
	}

	if got := met.Snapshot()["leases_lost"]; got != 1 {
		t.Errorf("leases_lost = %d, se esperaba 1", got)
	}
}

// TestSubmitAfterCloseFails: cerrado el canal, no se envía nada más.
func TestSubmitAfterCloseFails(t *testing.T) {
	url, pool, _ := newPool(t, fastPoolOptions())
	startWorker(t, url, "worker-1", 4)
	waitFor(t, 2*time.Second, "el anuncio", func() bool { return len(pool.Workers()) == 1 })

	an, err := pool.Acquire(context.Background(), "01J0CERRADO", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := an.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	err = an.Submit(context.Background(), analyzer.Request{Seq: 1, Payload: []byte("{}"), ReceivedAt: time.Now()})
	if !errors.Is(err, analyzer.ErrClosed) {
		t.Errorf("err = %v, se esperaba ErrClosed", err)
	}
}

func TestNewPoolRejectsNilConn(t *testing.T) {
	if _, err := bus.NewPool(nil, bus.PoolOptions{}, nil, nil, nil); err == nil {
		t.Error("se aceptó una conexión nula")
	}
}

var _ = nats.Conn{}
