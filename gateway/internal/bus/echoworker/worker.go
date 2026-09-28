// Package echoworker es un worker de análisis en Go: el que existe mientras
// no exista el de Python.
//
// Sirve para dos cosas:
//
//   - Cerrar el circuito de extremo a extremo y poder probar la caída de un
//     worker a mitad de sesión.
//   - Ser la referencia ejecutable del contrato que tendrá que cumplir el
//     worker de Python: qué subjects, qué mensajes, cuándo anunciarse, cuándo
//     latir y qué significa aceptar un lease.
//
// El análisis de verdad lo hace un stub sintético. Lo que aquí es de verdad es
// el protocolo.
//
// ESTADO CALIENTE: cada sesión tiene su estado en la memoria de ESTE proceso,
// creado al aceptar el lease y destruido al cerrarla. Los frames no lo
// transportan; llegan pelados. Esa es toda la razón de que exista la afinidad
// de sesión.
package echoworker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/edisonpaul4/biometrics/gateway/internal/analyzer"
	"github.com/edisonpaul4/biometrics/gateway/internal/bus"
	"github.com/edisonpaul4/biometrics/orchestrator/core/clock"
)

// Options configura el worker.
type Options struct {
	ID       string
	Capacity int
	// Analyzer es quien mide de verdad. Por defecto, el stub sintético.
	Analyzer          analyzer.Analyzer
	AnnounceInterval  time.Duration
	HeartbeatInterval time.Duration
	Clock             clock.Clock
	Logger            *slog.Logger
	// Version identifica el pipeline de análisis en las trazas.
	Version string
}

func (o Options) withDefaults() Options {
	if o.Capacity <= 0 {
		o.Capacity = 8
	}
	if o.Analyzer == nil {
		o.Analyzer = analyzer.NewStub()
	}
	if o.AnnounceInterval <= 0 {
		o.AnnounceInterval = time.Second
	}
	if o.HeartbeatInterval <= 0 {
		o.HeartbeatInterval = 300 * time.Millisecond
	}
	if o.Clock == nil {
		o.Clock = clock.System{}
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Version == "" {
		o.Version = "echoworker-go-1"
	}
	return o
}

// hot es el estado caliente de una sesión, en memoria de este worker.
//
// Es justo lo que no viaja con cada frame: si viajara, cada frame sería una
// foto suelta y las señales temporales —pulso, flujo óptico, tracking— no
// existirían.
type hot struct {
	sessionID string
	deadline  time.Time

	frames  int
	lastSeq uint64
	// brightness es la ventana temporal reciente. Aquí iría el buffer de
	// rPPG del worker de verdad.
	brightness []float64

	frameSub *nats.Subscription
	ctrlSub  *nats.Subscription
	stopHB   chan struct{}
}

// Worker atiende sesiones de análisis.
type Worker struct {
	nc   *nats.Conn
	opts Options

	leaseSub *nats.Subscription

	mu       sync.Mutex
	sessions map[string]*hot
	stopped  bool

	stopAnnounce chan struct{}
	wg           sync.WaitGroup
}

// New crea el worker.
func New(nc *nats.Conn, opts Options) (*Worker, error) {
	if nc == nil {
		return nil, fmt.Errorf("echoworker: conexión NATS nula")
	}
	opts = opts.withDefaults()
	if !bus.ValidToken(opts.ID) {
		return nil, fmt.Errorf("echoworker: identificador inválido: %q", opts.ID)
	}
	return &Worker{
		nc:           nc,
		opts:         opts,
		sessions:     make(map[string]*hot),
		stopAnnounce: make(chan struct{}),
	}, nil
}

// Start empieza a anunciarse y a aceptar sesiones.
func (w *Worker) Start() error {
	sub, err := w.nc.Subscribe(bus.Lease(w.opts.ID), w.onLease)
	if err != nil {
		return fmt.Errorf("echoworker: no se pudo escuchar el lease: %w", err)
	}
	w.leaseSub = sub

	w.announce() // uno inmediato: que no haya que esperar al primer tick
	w.wg.Add(1)
	go w.announceLoop()
	return w.nc.Flush()
}

// Stop simula la muerte del worker: deja de anunciarse, de latir y de
// responder. No avisa a nadie, que es justo lo que hace un proceso al caerse.
func (w *Worker) Stop() {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	w.stopped = true
	sessions := make([]*hot, 0, len(w.sessions))
	for _, h := range w.sessions {
		sessions = append(sessions, h)
	}
	w.sessions = make(map[string]*hot)
	w.mu.Unlock()

	close(w.stopAnnounce)
	if w.leaseSub != nil {
		_ = w.leaseSub.Unsubscribe()
	}
	for _, h := range sessions {
		w.releaseHot(h)
	}
	w.wg.Wait()
}

// Sessions es cuántas sesiones lleva.
func (w *Worker) Sessions() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.sessions)
}

// FramesSeen es cuántos frames ha analizado de una sesión. Prueba de que el
// estado caliente es acumulativo y vive aquí.
func (w *Worker) FramesSeen(sessionID string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if h, ok := w.sessions[sessionID]; ok {
		return h.frames
	}
	return 0
}

func (w *Worker) announceLoop() {
	defer w.wg.Done()
	ticker := time.NewTicker(w.opts.AnnounceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stopAnnounce:
			return
		case <-ticker.C:
			w.announce()
		}
	}
}

func (w *Worker) announce() {
	w.mu.Lock()
	sessions := len(w.sessions)
	stopped := w.stopped
	w.mu.Unlock()
	if stopped {
		return
	}

	payload, err := json.Marshal(bus.Announce{
		WorkerID:    w.opts.ID,
		Capacity:    w.opts.Capacity,
		Sessions:    sessions,
		Version:     w.opts.Version,
		EmittedAtUS: w.opts.Clock.Now().UnixMicro(),
	})
	if err != nil {
		return
	}
	_ = w.nc.Publish(bus.SubjectAnnounce, payload)
}

// onLease acepta o rechaza hacerse cargo de una sesión.
func (w *Worker) onLease(msg *nats.Msg) {
	var req bus.LeaseRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		w.reply(msg, bus.LeaseReply{WorkerID: w.opts.ID, Reason: "petición ilegible"})
		return
	}
	if !bus.ValidToken(req.SessionID) {
		w.reply(msg, bus.LeaseReply{WorkerID: w.opts.ID, Reason: "sesión inválida"})
		return
	}

	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return // muerto: ni siquiera contesta
	}
	if len(w.sessions) >= w.opts.Capacity {
		w.mu.Unlock()
		w.reply(msg, bus.LeaseReply{WorkerID: w.opts.ID, Reason: "sin capacidad"})
		return
	}
	if _, exists := w.sessions[req.SessionID]; exists {
		w.mu.Unlock()
		w.reply(msg, bus.LeaseReply{WorkerID: w.opts.ID, Reason: "sesión ya asignada"})
		return
	}

	h := &hot{
		sessionID: req.SessionID,
		deadline:  time.UnixMicro(req.DeadlineUS),
		stopHB:    make(chan struct{}),
	}
	w.sessions[req.SessionID] = h
	w.mu.Unlock()

	if err := w.attach(h); err != nil {
		w.opts.Logger.Error("no se pudo atender la sesión", "err", err, "session_id", req.SessionID)
		w.release(req.SessionID)
		w.reply(msg, bus.LeaseReply{WorkerID: w.opts.ID, Reason: "no se pudo suscribir"})
		return
	}

	w.reply(msg, bus.LeaseReply{Accepted: true, WorkerID: w.opts.ID})
	w.opts.Logger.Info("sesión aceptada", "session_id", req.SessionID, "worker_id", w.opts.ID)
}

func (w *Worker) attach(h *hot) error {
	frameSub, err := w.nc.Subscribe(bus.Frames(h.sessionID), func(msg *nats.Msg) {
		w.onFrame(h.sessionID, msg)
	})
	if err != nil {
		return err
	}
	h.frameSub = frameSub

	ctrlSub, err := w.nc.Subscribe(bus.Control(h.sessionID), func(msg *nats.Msg) {
		w.onControl(h.sessionID, msg)
	})
	if err != nil {
		_ = frameSub.Unsubscribe()
		return err
	}
	h.ctrlSub = ctrlSub

	w.wg.Add(1)
	go w.heartbeatLoop(h)
	return w.nc.Flush()
}

// heartbeatLoop renueva el lease mientras el worker siga vivo.
func (w *Worker) heartbeatLoop(h *hot) {
	defer w.wg.Done()
	ticker := time.NewTicker(w.opts.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-h.stopHB:
			return
		case <-w.stopAnnounce:
			return
		case <-ticker.C:
			w.mu.Lock()
			frames := h.frames
			w.mu.Unlock()

			payload, err := json.Marshal(bus.SessionHeartbeat{
				WorkerID:    w.opts.ID,
				SessionID:   h.sessionID,
				InFlight:    frames,
				EmittedAtUS: w.opts.Clock.Now().UnixMicro(),
			})
			if err != nil {
				continue
			}
			_ = w.nc.Publish(bus.Heartbeat(h.sessionID), payload)
		}
	}
}

// onFrame analiza un frame y devuelve las medidas.
//
// El frame llega pelado: ni sesión, ni reto, ni estado. Lo que se sabe de la
// sesión está aquí, en memoria, desde que se aceptó el lease.
func (w *Worker) onFrame(sessionID string, msg *nats.Msg) {
	task, err := bus.DecodeFrameTask(msg.Data)
	if err != nil {
		w.opts.Logger.Warn("frame ilegible", "err", err, "session_id", sessionID)
		return
	}

	w.mu.Lock()
	h, ok := w.sessions[sessionID]
	if !ok || w.stopped {
		w.mu.Unlock()
		return
	}
	h.frames++
	h.lastSeq = task.Seq
	frames := h.frames
	w.mu.Unlock()

	features, err := w.opts.Analyzer.Analyze(context.Background(), analyzer.Request{
		SessionID:          sessionID,
		Seq:                task.Seq,
		Encoding:           task.Encoding,
		Payload:            task.Payload,
		ReceivedAt:         time.UnixMicro(task.ReceivedAtUS),
		CapturedAt:         time.UnixMicro(task.CapturedAtUS),
		ClientClockTrusted: task.ClientClockTrusted(),
	})
	if err != nil {
		return
	}

	signals := make(map[string]float64, len(features.Signals)+2)
	for k, v := range features.Signals {
		signals[k] = v
	}
	// Señales que sólo existen porque hay continuidad: sin afinidad de
	// sesión, este contador valdría 1 en cada frame.
	signals["temporal_frames_seen"] = float64(frames)

	w.mu.Lock()
	h.brightness = append(h.brightness, features.Quality.Brightness)
	if len(h.brightness) > 64 {
		h.brightness = h.brightness[1:]
	}
	window := len(h.brightness)
	w.mu.Unlock()
	signals["temporal_window_frames"] = float64(window)

	payload, err := json.Marshal(bus.FrameFeatures{
		Seq:     task.Seq,
		Signals: signals,
		Quality: bus.Quality{
			FaceDetected: features.Quality.FaceDetected,
			FaceCount:    features.Quality.FaceCount,
			Sharpness:    features.Quality.Sharpness,
			Brightness:   features.Quality.Brightness,
		},
		ProcessingMS: features.ProcessingMS,
		Version:      w.opts.Version,
		AnalyzedAtUS: w.opts.Clock.Now().UnixMicro(),
	})
	if err != nil {
		return
	}
	_ = w.nc.Publish(bus.Features(sessionID), payload)
}

func (w *Worker) onControl(sessionID string, msg *nats.Msg) {
	var ctrl bus.SessionControl
	if err := json.Unmarshal(msg.Data, &ctrl); err != nil {
		return
	}
	if ctrl.Kind == bus.ControlSessionClose {
		w.release(sessionID)
	}
}

// release suelta el estado caliente de una sesión.
func (w *Worker) release(sessionID string) {
	w.mu.Lock()
	h, ok := w.sessions[sessionID]
	if ok {
		delete(w.sessions, sessionID)
	}
	w.mu.Unlock()
	if ok {
		w.releaseHot(h)
	}
}

func (w *Worker) releaseHot(h *hot) {
	close(h.stopHB)
	if h.frameSub != nil {
		_ = h.frameSub.Unsubscribe()
	}
	if h.ctrlSub != nil {
		_ = h.ctrlSub.Unsubscribe()
	}
}

func (w *Worker) reply(msg *nats.Msg, reply bus.LeaseReply) {
	payload, err := json.Marshal(reply)
	if err != nil {
		return
	}
	_ = msg.Respond(payload)
}
