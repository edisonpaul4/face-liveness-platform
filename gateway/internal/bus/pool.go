package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/edisonpaul4/biometrics/gateway/internal/analyzer"
	"github.com/edisonpaul4/biometrics/gateway/internal/telemetry"
	"github.com/edisonpaul4/biometrics/orchestrator/core/clock"
)

// PoolOptions configura el descubrimiento y el lease.
type PoolOptions struct {
	// AnnounceTTL es cuánto vale un anuncio. Pasado ese tiempo el worker deja
	// de ser candidato: si no se anuncia, no está.
	AnnounceTTL time.Duration
	// LeaseTimeout es lo que se espera a que un worker acepte una sesión.
	LeaseTimeout time.Duration
	// LeaseTTL es el silencio máximo del worker antes de dar la sesión por
	// perdida. Cualquier mensaje suyo —medidas o heartbeat— lo renueva.
	LeaseTTL time.Duration
	// LeaseCheck es cada cuánto se comprueba el silencio.
	LeaseCheck time.Duration
	// Candidates es a cuántos workers se le ofrece la sesión antes de
	// rendirse.
	Candidates int
	// FeatureQueue es cuántas medidas caben esperando a que la sesión las
	// consuma. Lo que no quepa se tira: nunca se acumula cola.
	FeatureQueue int
}

// DefaultPoolOptions son los valores por defecto.
//
// El lease es corto a propósito. La sesión entera dura menos de un minuto, así
// que esperar varios segundos a ver si el worker revive no sirve de nada:
// para cuando volviera, la sesión ya no tendría sentido.
func DefaultPoolOptions() PoolOptions {
	return PoolOptions{
		AnnounceTTL:  5 * time.Second,
		LeaseTimeout: 500 * time.Millisecond,
		LeaseTTL:     1500 * time.Millisecond,
		LeaseCheck:   250 * time.Millisecond,
		Candidates:   3,
		FeatureQueue: 16,
	}
}

func (o PoolOptions) withDefaults() PoolOptions {
	d := DefaultPoolOptions()
	if o.AnnounceTTL <= 0 {
		o.AnnounceTTL = d.AnnounceTTL
	}
	if o.LeaseTimeout <= 0 {
		o.LeaseTimeout = d.LeaseTimeout
	}
	if o.LeaseTTL <= 0 {
		o.LeaseTTL = d.LeaseTTL
	}
	if o.LeaseCheck <= 0 {
		o.LeaseCheck = d.LeaseCheck
	}
	if o.Candidates <= 0 {
		o.Candidates = d.Candidates
	}
	if o.FeatureQueue <= 0 {
		o.FeatureQueue = d.FeatureQueue
	}
	return o
}

// WorkerInfo es lo que el gateway sabe de un worker.
type WorkerInfo struct {
	Announce Announce
	SeenAt   time.Time
}

// Pool descubre workers y les asigna sesiones.
//
// El descubrimiento es deliberadamente simple: los workers se anuncian, el
// gateway se queda con el último anuncio de cada uno y elige al menos
// cargado. Sin registro externo ni consenso: si un worker no se anuncia, no
// existe, y si se cae a mitad de sesión el lease lo detecta.
type Pool struct {
	nc   *nats.Conn
	opts PoolOptions
	clk  clock.Clock
	met  *telemetry.Metrics
	log  *slog.Logger

	sub *nats.Subscription

	mu      sync.RWMutex
	workers map[string]WorkerInfo
}

// NewPool arranca el descubrimiento.
func NewPool(nc *nats.Conn, opts PoolOptions, clk clock.Clock, met *telemetry.Metrics, log *slog.Logger) (*Pool, error) {
	if nc == nil {
		return nil, fmt.Errorf("bus: conexión NATS nula")
	}
	if clk == nil {
		clk = clock.System{}
	}
	if met == nil {
		met = telemetry.New()
	}
	if log == nil {
		log = slog.Default()
	}

	p := &Pool{
		nc:      nc,
		opts:    opts.withDefaults(),
		clk:     clk,
		met:     met,
		log:     log,
		workers: make(map[string]WorkerInfo),
	}

	sub, err := nc.Subscribe(SubjectAnnounce, p.onAnnounce)
	if err != nil {
		return nil, fmt.Errorf("bus: no se pudo escuchar %s: %w", SubjectAnnounce, err)
	}
	p.sub = sub
	return p, nil
}

// Close deja de escuchar anuncios.
func (p *Pool) Close() error {
	if p.sub != nil {
		return p.sub.Unsubscribe()
	}
	return nil
}

func (p *Pool) onAnnounce(msg *nats.Msg) {
	var a Announce
	if err := json.Unmarshal(msg.Data, &a); err != nil {
		p.log.Warn("anuncio ilegible", "err", err)
		return
	}
	if !ValidToken(a.WorkerID) {
		p.log.Warn("anuncio con identificador inválido", "worker_id", a.WorkerID)
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.workers[a.WorkerID] = WorkerInfo{Announce: a, SeenAt: p.clk.Now()}
	p.warnMixedVersions()
}

// warnMixedVersions avisa cuando en el bus conviven analizadores de versiones
// distintas.
//
// No es paranoia: pasó tres veces durante el desarrollo y las tres desvió el
// diagnóstico. Un worker viejo sigue anunciándose y atendiendo sesiones, así
// que la mitad de ellas se analizan sin las señales nuevas — y el síntoma es
// un fallo intermitente que parece del algoritmo y es de despliegue.
//
// Sólo avisa: elegir por versión sería una política de despliegue y no le toca
// decidirla al gateway. Pero callarse tampoco.
//
// Se llama con p.mu tomado.
func (p *Pool) warnMixedVersions() {
	first := ""
	for _, w := range p.workers {
		if w.Announce.Version == "" {
			continue
		}
		if first == "" {
			first = w.Announce.Version
			continue
		}
		if w.Announce.Version != first {
			p.log.Warn("hay analizadores de versiones distintas en el bus",
				"versiones", []string{first, w.Announce.Version},
				"workers", len(p.workers))
			return
		}
	}
}

// Workers devuelve los workers vivos, del menos al más cargado.
func (p *Pool) Workers() []WorkerInfo {
	now := p.clk.Now()

	p.mu.RLock()
	out := make([]WorkerInfo, 0, len(p.workers))
	for _, w := range p.workers {
		if now.Sub(w.SeenAt) <= p.opts.AnnounceTTL {
			out = append(out, w)
		}
	}
	p.mu.RUnlock()

	// Menos cargado primero; a igualdad, orden estable por identificador
	// para que la elección sea reproducible.
	sort.Slice(out, func(i, j int) bool {
		li, lj := out[i].Announce.Load(), out[j].Announce.Load()
		if li != lj {
			return li < lj
		}
		return out[i].Announce.WorkerID < out[j].Announce.WorkerID
	})
	return out
}

// Acquire asigna la sesión al worker menos cargado que la acepte.
//
// A partir de aquí la sesión y el worker van juntos hasta el final: no hay
// reasignación. Cambiar de worker a mitad de sesión significaría empezar el
// estado caliente de cero, y además sería una vía para que un atacante
// forzara reintentos.
func (p *Pool) Acquire(ctx context.Context, sessionID string, deadline time.Time) (analyzer.Analysis, error) {
	if !ValidToken(sessionID) {
		return nil, fmt.Errorf("bus: identificador de sesión inválido: %q", sessionID)
	}

	candidates := p.Workers()
	if len(candidates) == 0 {
		p.met.LeasesFailed.Add(1)
		return nil, analyzer.ErrNoWorkers
	}
	if len(candidates) > p.opts.Candidates {
		candidates = candidates[:p.opts.Candidates]
	}

	req, err := json.Marshal(LeaseRequest{
		SessionID:  sessionID,
		DeadlineUS: deadline.UnixMicro(),
	})
	if err != nil {
		return nil, fmt.Errorf("bus: no se pudo serializar el lease: %w", err)
	}

	for _, w := range candidates {
		workerID := w.Announce.WorkerID

		reqCtx, cancel := context.WithTimeout(ctx, p.opts.LeaseTimeout)
		msg, err := p.nc.RequestWithContext(reqCtx, Lease(workerID), req)
		cancel()
		if err != nil {
			p.log.Warn("el worker no respondió al lease", "worker_id", workerID, "err", err)
			p.forget(workerID)
			continue
		}

		var reply LeaseReply
		if err := json.Unmarshal(msg.Data, &reply); err != nil || !reply.Accepted {
			p.log.Warn("lease rechazado", "worker_id", workerID, "reason", reply.Reason)
			continue
		}

		an, err := p.attach(sessionID, workerID)
		if err != nil {
			return nil, err
		}
		p.met.LeasesAcquired.Add(1)
		p.log.Info("sesión asignada", "session_id", sessionID, "worker_id", workerID,
			"load", w.Announce.Load())
		return an, nil
	}

	p.met.LeasesFailed.Add(1)
	return nil, analyzer.ErrNoWorkers
}

// forget descarta a un worker que no responde, para que no se lo ofrezcan a
// la siguiente sesión hasta que vuelva a anunciarse.
func (p *Pool) forget(workerID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.workers, workerID)
}

// attach abre las suscripciones de la sesión y arranca el vigilante del lease.
func (p *Pool) attach(sessionID, workerID string) (*analysis, error) {
	a := &analysis{
		nc:        p.nc,
		opts:      p.opts,
		clk:       p.clk,
		met:       p.met,
		log:       p.log.With("session_id", sessionID, "worker_id", workerID),
		sessionID: sessionID,
		workerID:  workerID,
		features:  make(chan analyzer.Features, p.opts.FeatureQueue),
		scores:    make(chan analyzer.WindowScore, 8),
		lost:      make(chan error, 1),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	a.touch()

	featSub, err := p.nc.Subscribe(Features(sessionID), a.onFeatures)
	if err != nil {
		return nil, fmt.Errorf("bus: no se pudo escuchar las medidas: %w", err)
	}
	a.featSub = featSub

	scoreSub, err := p.nc.Subscribe(ChallengeScore(sessionID), a.onScore)
	if err != nil {
		_ = featSub.Unsubscribe()
		return nil, fmt.Errorf("bus: suscripción de medidas de ventana: %w", err)
	}
	a.scoreSub = scoreSub

	hbSub, err := p.nc.Subscribe(Heartbeat(sessionID), a.onHeartbeat)
	if err != nil {
		_ = scoreSub.Unsubscribe()
		_ = featSub.Unsubscribe()
		return nil, fmt.Errorf("bus: no se pudo escuchar el heartbeat: %w", err)
	}
	a.hbSub = hbSub

	go a.watchLease()
	return a, nil
}

// analysis es el canal de análisis de una sesión sobre NATS.
type analysis struct {
	nc   *nats.Conn
	opts PoolOptions
	clk  clock.Clock
	met  *telemetry.Metrics
	log  *slog.Logger

	sessionID string
	workerID  string

	featSub  *nats.Subscription
	hbSub    *nats.Subscription
	scoreSub *nats.Subscription

	features chan analyzer.Features
	scores   chan analyzer.WindowScore
	lost     chan error

	// lastSeen es el último instante en que se supo del worker, en
	// nanosegundos. Lo tocan las callbacks de NATS y lo lee el vigilante.
	lastSeen atomic.Int64

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func (a *analysis) touch() { a.lastSeen.Store(a.clk.Now().UnixNano()) }

// onFeatures recibe las medidas de un frame.
//
// Se ejecuta en la goroutine de NATS: no puede bloquearse nunca. Si la sesión
// va por detrás, se tiran medidas y se cuentan.
func (a *analysis) onFeatures(msg *nats.Msg) {
	a.touch()

	var ff FrameFeatures
	if err := json.Unmarshal(msg.Data, &ff); err != nil {
		a.log.Warn("medidas ilegibles", "err", err)
		return
	}

	features := analyzer.Features{
		Seq:     ff.Seq,
		Signals: ff.Signals,
		Quality: analyzer.Quality{
			FaceDetected: ff.Quality.FaceDetected,
			FaceCount:    ff.Quality.FaceCount,
			Sharpness:    ff.Quality.Sharpness,
			Brightness:   ff.Quality.Brightness,
		},
		ProcessingMS: ff.ProcessingMS,
		Version:      ff.Version,
	}

	select {
	case a.features <- features:
		a.met.FeaturesReceived.Add(1)
	default:
		a.met.FeaturesDropped.Add(1)
	}
}

// onScore recoge la medida de una ventana.
//
// Cualquier mensaje del worker renueva el lease, medidas de ventana incluidas:
// hablar es estar vivo.
func (a *analysis) onScore(msg *nats.Msg) {
	a.touch()

	score, err := DecodeChallengeScore(msg.Data)
	if err != nil {
		a.log.Warn("medida de ventana ilegible", "err", err)
		return
	}

	select {
	case a.scores <- score:
	default:
		// El buzón de medidas es pequeño porque son pocas —una por reto— y
		// si se llena es que la sesión ya no las está leyendo.
		a.log.Warn("medida de ventana descartada", "window_id", score.WindowID)
	}
}

// RequestWindow pide medir una ventana ya cerrada. No espera respuesta: llega
// por Scores.
func (a *analysis) RequestWindow(_ context.Context, req analyzer.WindowRequest) error {
	payload, err := EncodeWindowRequest(req)
	if err != nil {
		return fmt.Errorf("bus: serializando la ventana: %w", err)
	}
	if err := a.nc.Publish(Window(a.sessionID), payload); err != nil {
		return fmt.Errorf("bus: publicando la ventana: %w", err)
	}
	return nil
}

func (a *analysis) Scores() <-chan analyzer.WindowScore { return a.scores }

func (a *analysis) onHeartbeat(*nats.Msg) { a.touch() }

// watchLease da la sesión por perdida si el worker se calla.
//
// No hay recuperación a propósito: la sesión dura segundos, su estado caliente
// estaba en la memoria del worker muerto y reconstruirlo desde cero no es
// distinguible de empezar otra vez. Se aborta y el usuario reintenta.
func (a *analysis) watchLease() {
	defer close(a.done)

	ticker := time.NewTicker(a.opts.LeaseCheck)
	defer ticker.Stop()

	for {
		select {
		case <-a.stop:
			return
		case <-ticker.C:
			silence := a.clk.Now().Sub(time.Unix(0, a.lastSeen.Load()))
			if silence <= a.opts.LeaseTTL {
				continue
			}
			a.met.LeasesLost.Add(1)
			a.log.Warn("lease perdido: el worker lleva callado demasiado",
				"silence", silence, "ttl", a.opts.LeaseTTL)
			select {
			case a.lost <- fmt.Errorf("%w: %s lleva %v sin dar señales",
				analyzer.ErrLost, a.workerID, silence.Round(time.Millisecond)):
			default:
			}
			return
		}
	}
}

// Submit publica el frame hacia el worker asignado. No espera respuesta.
func (a *analysis) Submit(_ context.Context, req analyzer.Request) error {
	select {
	case <-a.stop:
		return analyzer.ErrClosed
	default:
	}

	task := FrameTask{
		Encoding:     req.Encoding,
		Seq:          req.Seq,
		ReceivedAtUS: req.ReceivedAt.UnixMicro(),
		Payload:      req.Payload,
	}
	if !req.CapturedAt.IsZero() {
		task.CapturedAtUS = req.CapturedAt.UnixMicro()
	}
	if req.ClientClockTrusted {
		task.Flags |= FlagClientClockTrusted
	}

	if err := a.nc.Publish(Frames(a.sessionID), EncodeFrameTask(task)); err != nil {
		return fmt.Errorf("bus: no se pudo publicar el frame: %w", err)
	}
	a.met.FramesPublished.Add(1)
	return nil
}

func (a *analysis) Features() <-chan analyzer.Features { return a.features }

func (a *analysis) Lost() <-chan error { return a.lost }

func (a *analysis) WorkerID() string { return a.workerID }

// Close suelta el estado caliente del worker y cierra las suscripciones.
func (a *analysis) Close(context.Context) error {
	a.closeOnce.Do(func() {
		close(a.stop)

		// Aviso de higiene: si no llega, el worker soltará el estado al
		// vencer el deadline de la sesión.
		if payload, err := json.Marshal(SessionControl{
			Kind:      ControlSessionClose,
			SessionID: a.sessionID,
		}); err == nil {
			_ = a.nc.Publish(Control(a.sessionID), payload)
			_ = a.nc.Flush()
		}

		if a.scoreSub != nil {
			_ = a.scoreSub.Unsubscribe()
		}
		if a.featSub != nil {
			_ = a.featSub.Unsubscribe()
		}
		if a.hbSub != nil {
			_ = a.hbSub.Unsubscribe()
		}
	})
	<-a.done
	return nil
}
