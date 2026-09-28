// Package conn sirve una sesión de liveness sobre una conexión WebSocket.
//
// Tres goroutines por conexión, comunicadas sólo por canales:
//
//	readLoop   lee del socket, valida límites y encola
//	runLoop    dueño de la sesión: analiza, decide y produce mensajes
//	writeLoop  ÚNICA goroutine que escribe en el socket, cierre incluido
//
// El bucle de sesión está separado del de lectura a propósito: si leer y
// procesar fueran lo mismo, el backpressure sería implícito (se acumularía en
// TCP) y no se podrían descartar frames intermedios, que es justo lo que hace
// falta. Leer nunca se bloquea por culpa del análisis.
//
// El cierre también pasa por el escritor: si el runLoop cerrara el socket por
// su cuenta habría dos goroutines escribiendo.
package conn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/edisonpaul4/biometrics/gateway/internal/analyzer"
	"github.com/edisonpaul4/biometrics/gateway/internal/telemetry"
	"github.com/edisonpaul4/biometrics/gateway/internal/wsproto"
	"github.com/edisonpaul4/biometrics/orchestrator/core/challenge"
	"github.com/edisonpaul4/biometrics/orchestrator/core/clock"
	"github.com/edisonpaul4/biometrics/orchestrator/core/fsm"
	"github.com/edisonpaul4/biometrics/orchestrator/core/fusion"
	"github.com/edisonpaul4/biometrics/orchestrator/core/session"
)

// Engine es la máquina de estados de la sesión, vista desde el transporte.
//
// Es la costura de la arquitectura: hoy la satisface *session.Session en el
// mismo proceso; mañana la satisfará un cliente NATS contra el orquestador,
// sin tocar nada de este paquete.
type Engine interface {
	Start() error
	Reveal() (session.RevealedStep, error)
	SubmitAt(observedAt time.Time) (session.StepVerdict, error)
	Tick() (*session.StepVerdict, error)
	Resolve(v session.Verdict) (session.Resolution, error)
	// Abort cierra la sesión por una causa ajena al usuario.
	Abort(reason session.Reason) (session.Resolution, error)
	State() fsm.State
	Deadline() time.Time
}

// Limits son los límites duros de una conexión. Ninguno es opcional: sin
// ellos, un cliente hostil decide cuánta memoria y CPU consume el servidor.
type Limits struct {
	// MaxFrameBytes es el tamaño máximo de un mensaje binario, cabecera
	// incluida. Superarlo cierra la conexión.
	MaxFrameBytes int
	// MaxControlBytes es el tamaño máximo de un mensaje de control JSON.
	MaxControlBytes int
	// MaxFPS es el caudal máximo de frames. Lo que sobra se descarta.
	MaxFPS float64
	// SessionBudget es la duración máxima de una sesión.
	SessionBudget time.Duration
	// MaxClockDrift es la deriva máxima admisible entre el sello del cliente
	// y el del servidor para que el del cliente sirva de algo.
	MaxClockDrift time.Duration
	// WriteQueue es la profundidad de la cola de salida.
	WriteQueue int
	// WriteTimeout acota cada escritura.
	WriteTimeout time.Duration
	// TickInterval es cada cuánto se revisan los plazos.
	TickInterval time.Duration
	// FrameTimeout es lo que se espera a que un frame produzca medidas. Al
	// vencer, el frame se descarta y se cuenta; la sesión sigue.
	FrameTimeout time.Duration

	// WindowTimeout es lo que se espera a que el analizador puntúe las
	// ventanas al cerrar la sesión. Generoso comparado con FrameTimeout: son
	// pocas, llegan al final y su valor es alto — sin ellas la fusión decide
	// sin paralaje ni gradiente 3D.
	WindowTimeout time.Duration
	// MaxInflight son los frames que pueden estar a la vez en el analizador.
	// Es el tope que impide que un analizador lento acumule trabajo.
	MaxInflight int
	// HandshakeTimeout acota la espera del client_hello.
	HandshakeTimeout time.Duration
}

// rateLimitHeadroom es cuánto se deja pasar por encima del caudal impuesto
// antes de descartar.
//
// Dos, no uno. El cliente captura a la cadencia que se le pidió y la reparte
// con el jitter de su máquina y su red; exigirle el intervalo exacto descarta
// la mitad de sus frames sin que haya nada que frenar. Lo que este límite
// evita es una inundación deliberada, y para eso el doble sigue sobrando.
const rateLimitHeadroom = 2.0

// DefaultLimits son los límites por defecto.
func DefaultLimits() Limits {
	return Limits{
		MaxFrameBytes:   512 * 1024,
		MaxControlBytes: 4 * 1024,
		// 30 fps, no 15, y el motivo es el pulso sanguíneo.
		//
		// La modulación que deja el latido en el color de la piel es del
		// orden del 0,25 %, así que la señal está enterrada en ruido y lo que
		// la saca es promediar muestras. Medido sobre señal sintética a 10 s
		// de ventana, la probabilidad de distinguir un rostro vivo de una
		// superficie pasa de 0,62 a 15 fps a 0,73 a 30 fps, y con 720p a
		// 0,92.
		//
		// Es un TOPE, no una promesa: quien no tenga subida para sostenerlo
		// manda menos frames y el sistema sigue funcionando con el resto de
		// señales. Lo único que se degrada es el pulso.
		// 30 fps, la cadencia de 480p (la resolución por defecto). Es a la vez
		// el tope del limitador y lo que se le pide al cliente.
		//
		// El número lo puso el tope, no la máquina, y eso costó señal. Medido
		// sobre una sesión real: 137 de 335 frames llegaron a exactamente
		// 66 ms —el intervalo de 15 fps clavado—, mientras la cámara entregaba
		// huecos de 17 y 33 ms y el analizador tardaba 8,9 ms por frame a
		// 480p, o sea 112 fps de techo. Nada de la cadena pedía 15.
		//
		// Y las muestras son la señal: el destello llevaba SEIS por tramo, y
		// con cuatro «la correlación no se ha podido calcular» (§4). El pulso
		// va con la raíz del número de muestras. La mirada cerraba su fase de
		// llegada con cuatro, justo en el mínimo.
		//
		// Cuesta banda —30 × 60 KB son 14,4 Mbit/s— y por eso hace falta que
		// el cliente no encole: si el enlace no da, se salta turnos y la
		// cadencia baja sola en vez de crecer el retraso.
		MaxFPS: 30,
		// 60 s, no 45, y no porque la sesión se haya vuelto lenta: el guion
		// incorporó un tramo de quietud de once segundos para poder medir el
		// pulso, y el presupuesto tiene que cubrir el PEOR caso —cada reto
		// agotando su plazo—, que pasa de 29 s a 41.
		//
		// Lo que ve una persona normal es otra cosa: medido sobre una sesión
		// real, respondía a cada reto en poco más de un segundo y la sesión
		// entera duraba 8,3 s. Con el tramo, ronda los 19.
		//
		// Se prefirió subir el presupuesto a recortar retos. Cada reto es una
		// ventana temporal más que un vídeo grabado tiene que acertar, y un
		// guion corto es barato de superar por casualidad.
		// 60 s. Subió de 45 a 60 al añadir el tramo de quietud para el pulso.
		//
		// Estuvo un rato en 75 para dar sitio a tramos de destello largos, y
		// eso se revirtió: los tramos largos no medían más, medían PEOR (ver
		// MinFlashSegment). Una sesión real dura entre 20 y 30 s.
		SessionBudget:    60 * time.Second,
		MaxClockDrift:    30 * time.Second,
		WriteQueue:       64,
		WriteTimeout:     5 * time.Second,
		TickInterval:     100 * time.Millisecond,
		FrameTimeout:     400 * time.Millisecond,
		WindowTimeout:    1500 * time.Millisecond,
		MaxInflight:      4,
		HandshakeTimeout: 10 * time.Second,
	}
}

// Options configura una conexión.
type Options struct {
	// Fusion decide el veredicto a partir de la línea de tiempo de la sesión.
	// Nil deja la sesión sin veredicto posible: se reintenta, nunca se
	// aprueba por defecto.
	Fusion *fusion.Engine

	// InsecureExplainVerdict manda al cliente el desglose por detector.
	// SÓLO PARA PRUEBAS DE SEGURIDAD: en producción esto le enseña al
	// atacante qué corregir.
	InsecureExplainVerdict bool

	SessionID string
	Engine    Engine
	// Analyzers asigna un analizador a la sesión. La asignación dura toda la
	// sesión: es la afinidad de la que depende el estado caliente.
	Analyzers analyzer.Pool
	Clock     clock.Clock
	Metrics   *telemetry.Metrics
	Limits    Limits
	Logger    *slog.Logger
	// Capture son los parámetros de captura que se imponen al cliente.
	Capture wsproto.CaptureParams
}

type outKind uint8

const (
	outText outKind = iota
	outClose
)

type outbound struct {
	kind   outKind
	data   []byte
	code   int
	reason string
}

// frameJob es un frame ya validado, camino del análisis.
type frameJob struct {
	header  wsproto.FrameHeader
	payload []byte
	// receivedAt es el sello del SERVIDOR. Es el único que puntúa.
	receivedAt time.Time
	// capturedAt es el sello del CLIENTE, sólo para alinear.
	capturedAt time.Time
	clientOK   bool
}

// inflightFrame es un frame que ya va camino del analizador.
type inflightFrame struct {
	// sentAt marca desde cuándo se espera respuesta.
	sentAt time.Time
	// receivedAt es el sello del SERVIDOR al recibir el frame del cliente.
	// Es el instante que puntúa: la espera en el bus no es culpa del usuario.
	receivedAt time.Time
	// stepID ata el frame al paso que estaba activo cuando se envió.
	stepID string
	// stepKind es de qué tipo era ese paso. Hace falta para NO meter en la
	// ventana de reposo las miradas de un reto de mirada.
	stepKind challenge.Kind
}

// Conn sirve una sesión sobre un WebSocket.
type Conn struct {
	ws   *websocket.Conn
	opts Options
	log  *slog.Logger
	met  *telemetry.Metrics
	clk  clock.Clock

	frames  chan frameJob
	control chan []byte

	outMu     sync.Mutex
	out       chan outbound
	outClosed bool

	// closeWritten lo marca el escritor cuando ya despachó el cierre.
	closeWritten atomic.Bool

	analysis analyzer.Analysis
	// inflight son los frames enviados al analizador y todavía sin medidas.
	inflight map[uint64]inflightFrame

	eval *evaluator
	// recentGaze es una ventana corta de las últimas miradas medidas,
	// mantenida SIEMPRE, también entre retos.
	//
	// Existe porque la línea base de un reto de mirada no se puede tomar de
	// sus propios primeros frames: el objetivo es un punto que late y la
	// gente reacciona en uno o dos frames, así que esa ventana cae dentro
	// del propio movimiento. Medido con una cámara real: reposo verdadero
	// +0,023, base capturada +0,008, y media respuesta perdida.
	recentGaze []gazePoint
	// recentOpenness es la ventana de aperturas de ojo recientes. Fija lo
	// habitual del sujeto para que el descarte por parpadeo sea relativo a él
	// y no a un número igual para todo el mundo.
	recentOpenness []float64
	// featureAge* resumen cuánto tardan las medidas en volver, medido desde
	// que el frame llegó al servidor.
	featureAgeMax time.Duration
	featureAgeSum time.Duration
	featureAgeN   int
	// timeline acumula lo que la fusión necesita al cerrar la sesión.
	timeline       fusion.Timeline
	fusionResult   fusion.Result
	finishedScript bool

	// scriptCutShort marca que el guion NO llegó a su final: un paso se
	// rechazó y eso cerró la fase de retos (§6).
	//
	// Hace falta separarlo de `finishedScript` porque las dos cosas llevan la
	// máquina de estados al mismo sitio, y confundirlas abrió un agujero real:
	// una sesión de 4,6 segundos, con la calibración no medible y su único
	// reto FALLADO, se resolvió en **pass con 0,902** — porque
	// `require_completed_challenges` daba por completado un guion que se había
	// cortado a la fuerza.
	scriptCutShort bool
	// ambientTooBright: el destello no llegó a iluminar el rostro por encima
	// de la luz de la habitación.
	ambientTooBright bool
	// firstFrameAt es el sello del primer frame aceptado. Marca el arranque
	// del tramo sobre el que se puede buscar pulso: antes de él no hay cara
	// que medir.
	firstFrameAt time.Time

	// holdInterval es el tramo de quietud, si el guion lo trajo. Es donde se
	// pide el pulso: se le pidió al sujeto que no se moviera, así que es el
	// único trozo de la sesión con la cabeza quieta y la luz estable.
	holdInterval interval

	// litIntervals son los tramos en que la pantalla emitió color. El pulso
	// no se puede medir dentro de ellos: la señal que se busca es una
	// variación de décimas de punto en el color de la piel, y un destello la
	// mueve órdenes de magnitud más. No es ruido que se promedie, es la señal
	// tapada.
	litIntervals []interval

	// pendingWindows son las ventanas pedidas al analizador cuya medida
	// todavía no ha vuelto. La sesión espera por ellas antes de resolver: sin
	// paralaje ni gradiente 3D, la fusión decidiría con la mitad de las
	// señales y sin saberlo.
	pendingWindows int
	padV2Worst     float64
	padV1SEWorst   float64
	padFrames      int
	quality        captureAccumulator
	activeStep     session.RevealedStep
	hasActive      bool
	stepsPassed    int
	framesUsed     int
	finished       bool
}

// New crea la conexión. No arranca nada todavía.
func New(ws *websocket.Conn, opts Options) *Conn {
	if opts.Clock == nil {
		opts.Clock = clock.System{}
	}
	if opts.Metrics == nil {
		opts.Metrics = telemetry.New()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Limits.WriteQueue <= 0 {
		opts.Limits = DefaultLimits()
	}

	return &Conn{
		inflight: make(map[uint64]inflightFrame),
		ws:       ws,
		opts:     opts,
		log:      opts.Logger.With("session_id", opts.SessionID),
		met:      opts.Metrics,
		clk:      opts.Clock,
		// Capacidad 1: el buzón de frames guarda EL MÁS RECIENTE. Nunca cola.
		frames:  make(chan frameJob, 1),
		control: make(chan []byte, 4),
		out:     make(chan outbound, opts.Limits.WriteQueue),
	}
}

// Serve corre la sesión hasta el final y cierra la conexión.
func (c *Conn) Serve(ctx context.Context) error {
	runCtx, cancelRun := context.WithTimeout(ctx, c.opts.Limits.SessionBudget+5*time.Second)
	defer cancelRun()

	// El escritor sobrevive a la cancelación del run: tiene que poder drenar
	// la cola y despachar el cierre aunque la sesión ya haya terminado.
	writeCtx := context.WithoutCancel(ctx)

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		c.writeLoop(writeCtx)
	}()

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		c.readLoop(runCtx)
	}()

	err := c.runLoop(runCtx)

	// Drenaje: el runLoop ya encoló el cierre y cerró la cola.
	c.closeOut(outbound{kind: outClose, code: wsproto.CloseInternalError, reason: "fin inesperado"})
	<-writerDone

	// A partir de aquí no queda ninguna goroutine escribiendo en el socket.
	if !c.closeWritten.Load() {
		_ = c.ws.Close(websocket.StatusCode(wsproto.CloseInternalError), "cierre no despachado")
	}
	cancelRun()
	c.ws.CloseNow()
	<-readerDone

	return err
}

// --- lectura ----------------------------------------------------------------

func (c *Conn) readLoop(ctx context.Context) {
	var (
		lastSeq      uint64
		lastAccepted time.Time
		minInterval  time.Duration
	)
	if c.opts.Limits.MaxFPS > 0 {
		// Con MARGEN sobre la cadencia que se le impuso al cliente.
		//
		// Sin él, el límite es un cronómetro exacto contra un cliente al que
		// se le pidió justo esa cadencia, y cualquier jitter normal lo hace
		// saltar. Medido sobre una sesión real capturando a 29,4 fps de media:
		// el 47 % de los intervalos caía por debajo de los 33,33 ms que impone
		// un tope de 30, con un p10 de 24 ms. Casi la mitad de los frames se
		// descartaban por respirar.
		//
		// El límite existe para frenar una inundación, no para exigir cadencia
		// exacta: el buzón de un solo hueco ya se ocupa de que un exceso no se
		// acumule. Con el doble de margen sigue cortando a un cliente hostil y
		// deja pasar a uno normal.
		minInterval = time.Duration(float64(time.Second) / (c.opts.Limits.MaxFPS * rateLimitHeadroom))
	}

	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			return
		}

		switch typ {
		case websocket.MessageText:
			if len(data) > c.opts.Limits.MaxControlBytes {
				c.met.ProtocolViolations.Add(1)
				c.fail(wsproto.ErrorInvalidProtocol, "mensaje de control demasiado grande",
					wsproto.CloseProtocolViolation)
				return
			}
			c.met.ControlMessages.Add(1)
			select {
			case c.control <- data:
			default:
				// La cola de control es diminuta a propósito: si se llena,
				// el cliente está inundando el canal equivocado.
				c.met.ProtocolViolations.Add(1)
				c.fail(wsproto.ErrorRateLimited, "exceso de mensajes de control",
					wsproto.CloseRateLimited)
				return
			}

		case websocket.MessageBinary:
			receivedAt := c.clk.Now()
			c.met.FramesReceived.Add(1)

			if len(data) > c.opts.Limits.MaxFrameBytes {
				c.met.FramesTooLarge.Add(1)
				c.fail(wsproto.ErrorInvalidProtocol,
					fmt.Sprintf("frame de %d bytes, máximo %d", len(data), c.opts.Limits.MaxFrameBytes),
					wsproto.CloseFrameTooLarge)
				return
			}

			header, payload, perr := wsproto.ParseFrame(data)
			if perr != nil {
				c.met.FramesDroppedUnparsable.Add(1)
				c.met.ProtocolViolations.Add(1)
				c.fail(wsproto.ErrorInvalidProtocol, "cabecera de frame inválida",
					wsproto.CloseProtocolViolation)
				return
			}

			// Fuera de secuencia o repetido: se descarta sin cerrar. Un
			// reenvío tardío no es un ataque, es una red mala.
			if header.Seq <= lastSeq && lastSeq != 0 {
				c.met.FramesDroppedOutOfOrder.Add(1)
				continue
			}

			// Caudal máximo. Se descarta el exceso en vez de cerrar: cortar
			// la sesión por un pico de cámara castigaría al usuario legítimo.
			//
			// Se mide sobre el sello de CAPTURA del cliente, no sobre la hora
			// de llegada, y la diferencia es enorme en una red con jitter: los
			// frames se capturan cada 33 ms y llegan en ráfagas de varios en
			// el mismo milisegundo. Limitando por llegada se tiran frames
			// legítimos por el mero hecho de haber viajado juntos.
			//
			// Medido en una sesión por túnel: el cliente capturó 172 frames a
			// 29,4 fps, llegaron los 172, y el gateway descartó **72** —el
			// 42 %—. El reto de mirada se quedó con 7 fps efectivos y una
			// respuesta impecable de medio segundo se resolvió en fallo por no
			// alcanzar la racha de 3 de 5.
			//
			// El sello del cliente sólo se usa si su reloj es de fiar: su
			// deriva ya se valida contra el del servidor. Si no lo es, se
			// vuelve a la hora de llegada, que es lo que impide que un cliente
			// hostil inunde declarando sellos falsos.
			capturedAt, trusted := c.validateClientClock(header, receivedAt)
			limitAt := receivedAt
			if trusted {
				limitAt = capturedAt
			}
			if minInterval > 0 && !lastAccepted.IsZero() &&
				limitAt.Sub(lastAccepted) < minInterval {
				c.met.FramesDroppedRateLimit.Add(1)
				continue
			}

			lastSeq = header.Seq
			lastAccepted = limitAt

			c.offerFrame(frameJob{
				header:     header,
				payload:    payload,
				receivedAt: receivedAt,
				capturedAt: capturedAt,
				clientOK:   trusted,
			})
		}
	}
}

// validateClientClock comprueba que la deriva del reloj del cliente sea
// plausible.
//
// El sello del cliente NO puntúa jamás: para eso está el del servidor. Sólo
// sirve para alinear frames entre sí, y para eso hace falta que la deriva sea
// verosímil. Si no lo es, se ignora y se cuenta; no se cierra la sesión,
// porque un reloj mal puesto es de lo más común y no es un ataque.
func (c *Conn) validateClientClock(h wsproto.FrameHeader, receivedAt time.Time) (time.Time, bool) {
	if h.CapturedAtUS <= 0 {
		return time.Time{}, false
	}
	captured := time.UnixMicro(h.CapturedAtUS)
	drift := receivedAt.Sub(captured)
	if drift < 0 {
		drift = -drift
	}
	if drift > c.opts.Limits.MaxClockDrift {
		c.met.ClientClockImplausible.Add(1)
		return captured, false
	}
	return captured, true
}

// offerFrame deja el frame en el buzón descartando el ANTERIOR si sigue sin
// procesar.
//
// Esto es el backpressure: nunca se acumula cola. Si el cliente envía más
// rápido de lo que se analiza, lo que se pierde son los frames intermedios y
// se conserva el más reciente, que es el que describe lo que está pasando
// ahora. Cada descarte se cuenta.
func (c *Conn) offerFrame(job frameJob) {
	select {
	case c.frames <- job:
		return
	default:
	}

	select {
	case <-c.frames:
		c.met.FramesDroppedBackpressure.Add(1)
	default:
	}

	select {
	case c.frames <- job:
	default:
		c.met.FramesDroppedBackpressure.Add(1)
	}
}

// --- escritura --------------------------------------------------------------

// writeLoop es la ÚNICA goroutine que escribe en el socket.
func (c *Conn) writeLoop(ctx context.Context) {
	for msg := range c.out {
		switch msg.kind {
		case outText:
			wctx, cancel := context.WithTimeout(ctx, c.opts.Limits.WriteTimeout)
			err := c.ws.Write(wctx, websocket.MessageText, msg.data)
			cancel()
			if err != nil {
				c.drain()
				return
			}

		case outClose:
			// Close hace su propio handshake de cierre; no toma contexto.
			_ = c.ws.Close(websocket.StatusCode(msg.code), truncateReason(msg.reason))
			c.closeWritten.Store(true)
			c.drain()
			return
		}
	}
}

// drain vacía lo que quede en la cola tras un cierre, para que nadie se
// bloquee escribiendo en ella.
func (c *Conn) drain() {
	for range c.out {
	}
}

// truncateReason recorta el motivo al límite del protocolo WebSocket.
func truncateReason(s string) string {
	const maxReason = 120
	if len(s) <= maxReason {
		return s
	}
	return s[:maxReason]
}

// send encola un mensaje de control. Devuelve false si la cola está cerrada o
// llena; una cola llena es un cliente que no consume, y se trata como tal.
func (c *Conn) send(msg any) bool {
	data, err := json.Marshal(msg)
	if err != nil {
		c.log.Error("no se pudo serializar el mensaje", "err", err)
		return false
	}

	c.outMu.Lock()
	defer c.outMu.Unlock()
	if c.outClosed {
		return false
	}
	select {
	case c.out <- outbound{kind: outText, data: data}:
		return true
	default:
		c.met.WriteQueueOverflow.Add(1)
		c.outClosed = true
		close(c.out)
		return false
	}
}

// closeOut encola el cierre y cierra la cola. Es idempotente: sólo cuenta la
// primera llamada, así que el motivo real gana sobre cualquier cierre de
// respaldo posterior.
func (c *Conn) closeOut(msg outbound) {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	if c.outClosed {
		return
	}
	c.outClosed = true
	select {
	case c.out <- msg:
	default:
		c.met.WriteQueueOverflow.Add(1)
	}
	close(c.out)
}

// fail avisa al cliente y cierra con un código explícito.
func (c *Conn) fail(code wsproto.ErrorCode, message string, closeCode int) {
	c.send(wsproto.ServerError{Type: wsproto.TypeServerError, Code: code, Message: message})
	c.closeOut(outbound{kind: outClose, code: closeCode, reason: string(code)})
}

// --- bucle de sesión ---------------------------------------------------------

func (c *Conn) runLoop(ctx context.Context) error {
	if !c.send(wsproto.ServerHello{
		Type:              wsproto.TypeServerHello,
		SessionID:         c.opts.SessionID,
		Capture:           c.opts.Capture,
		SessionDeadlineMS: c.opts.Limits.SessionBudget.Milliseconds(),
	}) {
		c.closeOut(outbound{kind: outClose, code: wsproto.CloseSlowClient, reason: "cliente lento"})
		return errors.New("conn: no se pudo enviar server_hello")
	}

	// Asignación del analizador: a partir de aquí, esta sesión y ese worker
	// van juntos hasta el final (afinidad de sesión).
	analysis, err := c.opts.Analyzers.Acquire(ctx, c.opts.SessionID, c.opts.Engine.Deadline())
	if err != nil {
		c.abortInfrastructure(err)
		return nil
	}
	c.analysis = analysis
	defer func() {
		// El cierre libera el estado caliente del worker. Va con un contexto
		// sin cancelar: el aviso tiene que salir aunque la sesión acabe mal.
		_ = analysis.Close(context.WithoutCancel(ctx))
	}()

	if err := c.opts.Engine.Start(); err != nil {
		c.fail(wsproto.ErrorInternal, "no se pudo arrancar la sesión", wsproto.CloseInternalError)
		return fmt.Errorf("conn: Start: %w", err)
	}
	c.met.SessionsStarted.Add(1)

	if err := c.revealNext(); err != nil {
		return err
	}

	ticker := time.NewTicker(c.opts.Limits.TickInterval)
	defer ticker.Stop()

	features := analysis.Features()
	scores := analysis.Scores()
	lost := analysis.Lost()

	for !c.finished {
		select {
		case <-ctx.Done():
			c.expire("presupuesto de sesión agotado")
			return nil

		case job := <-c.frames:
			c.submitFrame(ctx, job)

		case f := <-features:
			if err := c.onFeatures(ctx, f); err != nil {
				return err
			}

		case score := <-scores:
			c.onWindowScore(score)

		case cause := <-lost:
			// El worker dejó de responder. No se intenta recuperar nada.
			c.abortInfrastructure(cause)
			return nil

		case data := <-c.control:
			if err := c.onControl(data); err != nil {
				return err
			}

		case <-ticker.C:
			c.expireInflight()
			if err := c.onTick(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

// requestWindow pide al analizador que puntúe la ventana del paso que acaba
// de cerrarse.
//
// Es el ÚNICO sitio donde Go le cuenta algo a Python, y va recortado: eje,
// objetivo, margen e intervalo, o la secuencia de colores emitida. Ni
// identificador de reto, ni posición en el guion, ni umbral, ni consecuencia
// (CLAUDE.md §3).
func (c *Conn) requestWindow(ctx context.Context) {
	req, ok := c.windowRequest()
	if !ok {
		return
	}
	if err := c.analysis.RequestWindow(ctx, req); err != nil {
		// Que no se pueda pedir no tumba la sesión: la señal saldrá como no
		// medida, que es distinto de medida y mala.
		c.log.Warn("no se pudo pedir la medida de la ventana", "err", err, "window_id", req.ID)
		return
	}
	c.pendingWindows++
}

// windowRequest traduce el paso cerrado a lo que el analizador necesita.
func (c *Conn) windowRequest() (analyzer.WindowRequest, bool) {
	step := c.activeStep
	req := analyzer.WindowRequest{
		ID:        step.ID,
		StartedAt: c.eval.revealedAt,
		EndedAt:   c.clk.Now(),
	}

	switch step.Kind {
	case challenge.KindCalibration:
		req.Kind = analyzer.WindowCalibration

	case challenge.KindPose:
		axis, target, tolerance, ok := poseWindowSpec(step.Pose)
		if !ok {
			// Acercarse no es un ángulo: el analizador no tiene ventana para
			// eso y el gateway ya lo mide por el tamaño del rostro.
			return analyzer.WindowRequest{}, false
		}
		req.Kind = analyzer.WindowPose
		req.Axis, req.TargetDeg, req.ToleranceDeg = axis, target, tolerance

	case challenge.KindFlash:
		req.Kind = analyzer.WindowFlash
		c.litIntervals = append(c.litIntervals, interval{from: req.StartedAt, to: req.EndedAt})
		for _, seg := range step.Flash {
			req.Sequence = append(req.Sequence, analyzer.FlashSegment{
				Color:      seg.Color.String(),
				DurationMS: seg.Duration.Milliseconds(),
			})
		}

	case challenge.KindHold:
		// No hay ventana que pedirle a Python por el tramo en sí: lo que se
		// mide en él es el pulso, y eso se pide una sola vez al resolver.
		c.holdInterval = interval{from: req.StartedAt, to: req.EndedAt}
		return analyzer.WindowRequest{}, false

	default:
		// La mirada la mide el gateway con las señales por frame: no hay
		// ventana que pedir.
		return analyzer.WindowRequest{}, false
	}
	return req, true
}

// poseWindowSpec traduce la pose pedida a eje y ángulo objetivo.
//
// Los signos son los del convenio de §9: se miden desde la CÁMARA aunque el
// reto se enuncie desde el sujeto.
func poseWindowSpec(pose challenge.PoseAction) (axis string, target, tolerance float64, ok bool) {
	switch pose {
	case challenge.PoseYawLeft:
		return "yaw", yawThresholdDeg, poseWindowTolerance, true
	case challenge.PoseYawRight:
		return "yaw", -yawThresholdDeg, poseWindowTolerance, true
	case challenge.PosePitchUp:
		return "pitch", -pitchThresholdDeg, poseWindowTolerance, true
	default:
		return "", 0, 0, false
	}
}

// interval es un tramo cerrado de tiempo del servidor.
type interval struct{ from, to time.Time }

// pulseMinSpan es el tramo mínimo que merece la pena mandar a medir.
//
// Es la misma aritmética que aplica el analizador: la resolución en
// frecuencia de una FFT es 1/T, y con menos de esto un pulso de 60 lpm y uno
// de 75 caen en el mismo bin. Se filtra aquí además de allí para no gastar un
// viaje por el bus en algo que va a volver como no medible.
const pulseMinSpan = 10 * time.Second

// requestPulseWindow pide el pulso sobre el tramo más largo sin destellos.
//
// Elegir el intervalo es cosa de Go, no de Python: es una decisión sobre el
// guion —qué se le pidió al sujeto y cuándo— y Python no sabe que existe un
// guion (CLAUDE.md §3). Lo que cruza es lo mismo de siempre: dos instantes.
//
// Los tramos de pose SÍ se incluyen aunque el sujeto mueva la cabeza. POS
// está construido para aguantar movimiento, y excluirlos no dejaría tramo
// alguno: con el guion actual la sesión entera son veinte segundos con un
// destello en medio.
func (c *Conn) requestPulseWindow(ctx context.Context) {
	span, ok := c.pulseSpan()
	if !ok {
		c.log.Info("pulso no pedido: sin tramo suficiente de quietud")
		return
	}

	req := analyzer.WindowRequest{
		ID:        c.opts.SessionID + ":pulse",
		Kind:      analyzer.WindowPulse,
		StartedAt: span.from,
		EndedAt:   span.to,
	}
	if err := c.analysis.RequestWindow(ctx, req); err != nil {
		c.log.Warn("no se pudo pedir el pulso", "err", err)
		return
	}
	c.pendingWindows++
}

// pulseSpan elige dónde medir el pulso.
//
// El tramo de quietud manda cuando lo hay: es el único trozo de la sesión en
// que se le pidió al sujeto que no se moviera. El resto de la parte sin
// destellos también tiene la luz estable, pero tiene la cabeza girando, y el
// movimiento le mete al pulso ruido en la misma banda.
//
// Cuando no hay tramo —porque se excluyó por configuración— se cae al hueco
// más largo sin destellos, que es mejor que no medir.
func (c *Conn) pulseSpan() (interval, bool) {
	if span := c.holdInterval.to.Sub(c.holdInterval.from); span >= pulseMinSpan {
		return c.holdInterval, true
	}
	return c.longestUnlitSpan()
}

// longestUnlitSpan devuelve el hueco más largo entre destellos.
func (c *Conn) longestUnlitSpan() (interval, bool) {
	if c.firstFrameAt.IsZero() {
		return interval{}, false
	}
	start, end := c.firstFrameAt, c.clk.Now()

	lit := append([]interval(nil), c.litIntervals...)
	sort.Slice(lit, func(i, j int) bool { return lit[i].from.Before(lit[j].from) })

	var best interval
	var bestSpan time.Duration
	cursor := start
	for _, l := range append(lit, interval{from: end, to: end}) {
		if gap := l.from.Sub(cursor); gap > bestSpan {
			best, bestSpan = interval{from: cursor, to: l.from}, gap
		}
		if l.to.After(cursor) {
			cursor = l.to
		}
	}
	if bestSpan < pulseMinSpan {
		return interval{}, false
	}
	return best, true
}

// awaitWindows espera las medidas de ventana que falten, con tope.
func (c *Conn) awaitWindows() {
	if c.pendingWindows <= 0 {
		return
	}

	deadline := c.clk.Now().Add(c.opts.Limits.WindowTimeout)
	scores := c.analysis.Scores()
	for c.pendingWindows > 0 {
		remaining := deadline.Sub(c.clk.Now())
		if remaining <= 0 {
			break
		}
		timer := time.NewTimer(remaining)
		select {
		case score := <-scores:
			timer.Stop()
			c.onWindowScore(score)
		case <-timer.C:
		}
	}

	if c.pendingWindows > 0 {
		c.met.WindowsUnanswered.Add(int64(c.pendingWindows))
		c.log.Warn("medidas de ventana sin respuesta",
			"pendientes", c.pendingWindows,
			"detalle", "la fusión decidirá sin esas señales, y lo sabe")
		c.pendingWindows = 0
	}
}

// onWindowScore incorpora la medida de una ventana a la línea de tiempo.
//
// Aquí llegan las señales que de verdad separan un rostro de una foto:
// paralaje, gradiente 3D, ausencia de pantalla, continuidad e identidad. El
// gateway no las calcula ni podría: son visión por computador, y eso es de
// Python (CLAUDE.md §3). Lo único que hace es pedirlas y pesarlas.
func (c *Conn) onWindowScore(score analyzer.WindowScore) {
	c.met.WindowScores.Add(1)
	c.log.Info("medida de ventana",
		"window_id", score.WindowID,
		"kind", string(score.Kind),
		"score", round4(score.Score),
		"frames", score.FramesUsed,
		"medible", score.QualitySufficient,
		"submetricas", submetricSummary(score.Submetrics),
		"sensor", sensorCues(score.Raw),
		"destello", flashCues(score.Kind, score.Raw))

	if c.pendingWindows > 0 {
		c.pendingWindows--
	}

	kind, ok := fusionWindowKind(score.Kind)
	if !ok {
		// La calibración no aporta señal a la fusión: fija la línea base del
		// analizador y su valor es haber ocurrido.
		return
	}

	// El aviso de que la habitación tapaba el destello lo levanta ahora
	// PYTHON, no el gateway. Es quien tiene la correlación buena, y quien
	// pregunta por la amplitud en el orden correcto: sólo si la correlación no
	// llegó. El gateway se limita a traducirlo al motivo público.
	if score.Kind == analyzer.WindowFlash && !score.QualitySufficient &&
		score.QualityReason == analyzerAmbientReason {
		c.ambientTooBright = true
	}

	c.timeline.Windows = append(c.timeline.Windows, fusion.Window{
		ID:                score.WindowID,
		Kind:              kind,
		Score:             score.Score,
		Submetrics:        score.Submetrics,
		Raw:               score.Raw,
		QualitySufficient: score.QualitySufficient,
		QualityReason:     score.QualityReason,
	})
}

func fusionWindowKind(k analyzer.WindowKind) (fusion.WindowKind, bool) {
	switch k {
	case analyzer.WindowPose:
		return fusion.WindowPose, true
	case analyzer.WindowFlash:
		return fusion.WindowFlash, true
	case analyzer.WindowPulse:
		return fusion.WindowPulse, true
	default:
		return "", false
	}
}

// sensorCues saca al log las medidas sin calibrar que acompañan al destello:
// obturador rodante y reacción del control automático —que miran a la CÁMARA
// en vez de al sujeto— y la dispersión subsuperficial, que mira al material
// del que está hecha la cara.
//
// Van sueltas y sin peso en la fusión a propósito. Miden otra cosa que el
// resto de la ventana —si hay un sensor físico detrás, no si hay una cara viva
// delante— y están sin calibrar contra ataques reales. Se registran para poder
// calibrarlas con sesiones de verdad, que es el único camino que este
// repositorio acepta para darle voto a una señal.
// flashCues saca al log las tripas de una medida de destello.
//
// Existe porque sin ellas un destello no medible es indistinguible de otro:
// no se sabe si faltó correlación, si el retardo se fue al borde del rango, o
// cuál de las cuatro huellas de pantalla se disparó. Diagnosticarlo exigía
// tener la grabación de la sesión, y las sesiones no siempre se graban.
//
// Sólo para ventanas de destello: en las demás estos campos no existen y
// llenarían el log de "no medida".
func flashCues(kind analyzer.WindowKind, raw map[string]float64) map[string]any {
	if kind != analyzer.WindowFlash {
		return nil
	}
	out := map[string]any{}
	for _, name := range []string{
		"correlation", "lag_ms", "signal_to_noise", "modulation_amplitude",
		"screen_suspicion", "screen_bright_and_deaf", "screen_specular",
		"screen_moire", "screen_banding",
	} {
		if v, ok := raw[name]; ok {
			out[name] = round4(v)
		} else {
			out[name] = "n/d"
		}
	}
	return out
}

func sensorCues(raw map[string]float64) map[string]any {
	out := map[string]any{}
	for _, name := range []string{
		"sensor_rolling_shutter", "sensor_agc_response", "skin_subsurface_red",
	} {
		if v, ok := raw[name]; ok {
			out[name] = round4(v)
		} else {
			out[name] = "no medida"
		}
	}
	return out
}

// submetricSummary resume las sub-métricas para el log, distinguiendo lo no
// medido de lo medido y malo.
func submetricSummary(sub map[string]*float64) map[string]any {
	out := make(map[string]any, len(sub))
	for k, v := range sub {
		if v == nil {
			out[k] = "no medida"
			continue
		}
		out[k] = round4(*v)
	}
	return out
}

// revealNext pide el paso activo a la máquina de estados y lo manda. Es la
// ÚNICA vía por la que sale contenido del guion, y sale de uno en uno.
func (c *Conn) revealNext() error {
	step, err := c.opts.Engine.Reveal()
	if err != nil {
		if errors.Is(err, session.ErrSessionExpired) {
			c.expire("sesión expirada")
			return nil
		}
		c.fail(wsproto.ErrorInternal, "no hay paso que revelar", wsproto.CloseInternalError)
		return fmt.Errorf("conn: Reveal: %w", err)
	}

	c.activeStep = step
	c.hasActive = true
	c.eval = newEvaluator(step, c.clk.Now())
	// Ya no se instala ninguna referencia de reposo: el reto de mirada se
	// resuelve comparando sus DOS fases, y esa referencia era la causa de tres
	// fallos distintos —ventana desfasada, contaminación entre miradas
	// consecutivas, y un rechazo por fraude contra quien miraba a un lado
	// durante la calibración—.
	if step.Kind == challenge.KindGaze {
		media := time.Duration(0)
		if c.featureAgeN > 0 {
			media = c.featureAgeSum / time.Duration(c.featureAgeN)
		}
		c.log.Info("mirada revelada",
			"desde", round2(step.GazeFrom.X), "hasta", round2(step.Gaze.X),
			"permanencia_ms", step.Hold.Milliseconds(),
			"medida_edad_media_ms", media.Milliseconds(),
			"medida_edad_max_ms", c.featureAgeMax.Milliseconds())
	}

	if !c.send(challengeMessage(step)) {
		c.closeOut(outbound{kind: outClose, code: wsproto.CloseSlowClient, reason: "cliente lento"})
		return errors.New("conn: no se pudo enviar el reto")
	}
	return nil
}

// submitFrame manda el frame al analizador. No espera respuesta: si esperara,
// un analizador lento congelaría la sesión.
func (c *Conn) submitFrame(ctx context.Context, job frameJob) {
	if !c.hasActive {
		return
	}

	// Tope de frames en vuelo: sin él, un analizador lento acumularía trabajo
	// que ya no le sirve a nadie.
	if c.opts.Limits.MaxInflight > 0 && len(c.inflight) >= c.opts.Limits.MaxInflight {
		c.met.FramesDroppedBackpressure.Add(1)
		return
	}

	err := c.analysis.Submit(ctx, analyzer.Request{
		SessionID:          c.opts.SessionID,
		Seq:                job.header.Seq,
		Encoding:           uint8(job.header.Encoding),
		Payload:            job.payload,
		ReceivedAt:         job.receivedAt,
		CapturedAt:         job.capturedAt,
		ClientClockTrusted: job.clientOK,
	})
	switch {
	case errors.Is(err, analyzer.ErrBusy):
		// El analizador va por detrás: se tira el frame, no se encola.
		c.met.FramesDroppedBackpressure.Add(1)
		return
	case err != nil:
		c.log.Warn("no se pudo enviar el frame al analizador", "err", err, "seq", job.header.Seq)
		return
	}

	if c.firstFrameAt.IsZero() {
		c.firstFrameAt = job.receivedAt
	}

	c.inflight[job.header.Seq] = inflightFrame{
		stepKind:   c.activeStep.Kind,
		sentAt:     c.clk.Now(),
		receivedAt: job.receivedAt,
		stepID:     c.activeStep.ID,
	}
}

// onFeatures incorpora las medidas de un frame.
func (c *Conn) onFeatures(ctx context.Context, features analyzer.Features) error {
	in, ok := c.inflight[features.Seq]
	if !ok {
		// Medidas de un frame ya descartado por plazo, o repetidas.
		c.met.FeaturesLate.Add(1)
		return nil
	}
	delete(c.inflight, features.Seq)

	c.met.FramesAnalyzed.Add(1)
	c.framesUsed++
	// Cuánto tiene la medida cuando se usa. Sin este número, un gateway que
	// puntúa frames de hace dos segundos es indistinguible de un sujeto que
	// no respondió: los dos dan avance cero.
	if age := c.clk.Now().Sub(in.receivedAt); age > c.featureAgeMax {
		c.featureAgeMax = age
	}
	c.featureAgeSum += c.clk.Now().Sub(in.receivedAt)
	c.featureAgeN++
	// Durante un reto de MIRADA el sujeto no está en reposo: está mirando al
	// punto. Meter esos frames en la ventana de referencia hace que el
	// siguiente reto tome como "reposo" la posición desplazada del anterior.
	//
	// Medido: con dos miradas seguidas a lados opuestos, la segunda arrancaba
	// con reposo −13,26° y el sujeto, al volver la vista, producía un avance
	// de 13,26° en DOS frames. Más rápido que el mínimo humano, que es motivo
	// duro: la sesión acababa en `reject` por `temporal_response_too_fast`
	// contra alguien que sólo estaba obedeciendo.
	if in.stepKind != challenge.KindGaze {
		c.trackGaze(features, in.receivedAt)
	}
	c.quality.observe(features)
	c.trackPad(features)

	if !c.hasActive || in.stepID != c.activeStep.ID {
		// El frame era del paso anterior: ya no dice nada del actual.
		c.met.FeaturesLate.Add(1)
		return nil
	}

	// El instante que cuenta es cuando el frame llegó al servidor, no cuando
	// volvieron las medidas: la vuelta por el bus no es culpa del usuario.
	if !c.eval.observe(features, in.receivedAt) {
		return nil
	}

	// El paso se ha cumplido. Quien dice si llegó a tiempo es la máquina de
	// estados, con el sello del SERVIDOR.
	verdict, err := c.opts.Engine.SubmitAt(in.receivedAt)
	if err != nil {
		if errors.Is(err, session.ErrSessionExpired) {
			c.expire("sesión expirada")
			return nil
		}
		c.fail(wsproto.ErrorInternal, "no se pudo registrar la respuesta", wsproto.CloseInternalError)
		return fmt.Errorf("conn: SubmitAt: %w", err)
	}
	return c.afterVerdict(ctx, verdict)
}

// captureAccumulator resume si los frames servían para medir.
type captureAccumulator struct {
	frames     int
	withFace   int
	sharpness  float64
	brightness float64
	highlights float64
}

func (a *captureAccumulator) observe(f analyzer.Features) {
	a.frames++
	if f.Quality.FaceDetected {
		a.withFace++
	}
	a.sharpness += f.SignalOr("quality_sharpness", 0)
	a.brightness += f.SignalOr("quality_brightness", 0)
	// El quemado que cuenta es el del ROSTRO, no el del encuadre. Una ventana
	// detrás del sujeto quema medio frame sin afectar a su cara: medido, 10,4 %
	// del encuadre contra 0,53 % del rostro, y una sesión con todos los retos
	// superados resolviéndose en reintentar por calidad.
	//
	// Si el analizador no la emite —versión antigua— se cae a la del
	// encuadre, que es peor pero no inventa nada.
	a.highlights += f.SignalOr("quality_face_highlight",
		f.SignalOr("quality_highlight_saturation", 0))
}

func (a captureAccumulator) stats() fusion.CaptureStats {
	if a.frames == 0 {
		return fusion.CaptureStats{}
	}
	n := float64(a.frames)
	return fusion.CaptureStats{
		Frames:                  a.frames,
		FramesWithFace:          a.withFace,
		MeanSharpness:           a.sharpness / n,
		MeanBrightness:          a.brightness / n,
		MeanHighlightSaturation: a.highlights / n,
	}
}

// trackPad se queda con el PEOR frame de la sesión según el clasificador
// pasivo, no con la media.
//
// Un atacante sólo necesita que la mayoría de sus frames pasen desapercibidos;
// promediar le regala precisamente eso. Y al revés: si de cincuenta frames uno
// grita ataque, eso es lo que hay que llevar a la fusión.
func (c *Conn) trackPad(f analyzer.Features) {
	wide, okWide := worstOf(f, "texture_pad_v2_a", "texture_pad_v2_b")
	tight, okTight := worstOf(f, "texture_pad_v1se_a", "texture_pad_v1se_b")
	if !okWide && !okTight {
		return
	}
	c.padFrames++
	c.padV2Worst = max(c.padV2Worst, wide)
	c.padV1SEWorst = max(c.padV1SEWorst, tight)
}

// worstOf devuelve la mayor probabilidad de ataque entre varias familias del
// mismo modelo.
func worstOf(f analyzer.Features, names ...string) (float64, bool) {
	worst, found := 0.0, false
	for _, name := range names {
		if v, ok := f.Signal(name); ok {
			found = true
			worst = max(worst, v)
		}
	}
	return worst, found
}

// trackGaze mantiene la ventana de miradas recientes, pase lo que pase con
// el paso activo. Es lo que permite tener una referencia ya hecha cuando
// aparece un objetivo.
func (c *Conn) trackGaze(f analyzer.Features, capturedAt time.Time) {
	// La apertura se acumula SIEMPRE, aunque el frame no sirva para la
	// referencia: es lo que define qué es normal para esta persona.
	if open, ok := f.Signal("gaze_openness"); ok {
		c.recentOpenness = append(c.recentOpenness, open)
		if len(c.recentOpenness) > gazeOpennessFrames {
			c.recentOpenness = c.recentOpenness[len(c.recentOpenness)-gazeOpennessFrames:]
		}
	}
	if !c.openEnough(f) {
		return
	}
	world, ok := worldGaze(f)
	if !ok {
		return
	}
	c.recentGaze = append(c.recentGaze, gazePoint{deg: world, at: capturedAt})
	if len(c.recentGaze) > gazeReferenceFrames {
		c.recentGaze = c.recentGaze[len(c.recentGaze)-gazeReferenceFrames:]
	}
}

// expireInflight descarta los frames que no produjeron medidas a tiempo.
//
// Un frame sin respuesta no bloquea nada: se tira, se cuenta y la sesión
// sigue con los siguientes.
func (c *Conn) expireInflight() {
	if c.opts.Limits.FrameTimeout <= 0 {
		return
	}
	now := c.clk.Now()
	for seq, in := range c.inflight {
		if now.Sub(in.sentAt) > c.opts.Limits.FrameTimeout {
			delete(c.inflight, seq)
			c.met.FramesTimedOut.Add(1)
		}
	}
}

// abortInfrastructure cierra la sesión porque se ha caído algo nuestro.
//
// No se intenta recuperar: el estado caliente estaba en la memoria del worker
// que se fue, la sesión dura segundos y reconstruirla no se distingue de
// empezar otra. El usuario reintenta con un ticket nuevo.
func (c *Conn) abortInfrastructure(cause error) {
	if c.finished {
		return
	}
	c.finished = true
	c.met.SessionsAbortedInfra.Add(1)
	c.log.Error("sesión abortada por infraestructura", "err", cause)

	decision := wsproto.DecisionInconclusive
	if res, err := c.opts.Engine.Abort(session.ReasonAnalyzerUnavailable); err == nil {
		decision = decisionOf(res.Outcome)
	}

	c.send(wsproto.ServerResult{
		Type:      wsproto.TypeServerResult,
		SessionID: c.opts.SessionID,
		Decision:  decision,
		ReasonKey: ReasonKeyInfrastructure,
	})
	c.closeOut(outbound{
		kind:   outClose,
		code:   wsproto.CloseInfrastructureError,
		reason: string(session.ReasonAnalyzerUnavailable),
	})
}

// afterVerdict cierra el reto activo y decide si viene otro o toca resolver.
func (c *Conn) afterVerdict(ctx context.Context, verdict session.StepVerdict) error {
	c.hasActive = false
	// Sólo en el log del servidor: por qué se cerró así. Un "inconclusive"
	// sin esto es indiagnosticable en cuanto hay una cámara de verdad
	// delante, porque el cliente —a propósito— no recibe ningún detalle.
	if c.eval != nil {
		c.log.Info("reto cerrado", append([]any{
			"aceptado", verdict.Accepted,
		}, c.eval.summary()...)...)
	}
	c.send(wsproto.ServerChallengeEnd{
		Type:        wsproto.TypeServerChallengeEnd,
		ChallengeID: verdict.StepID,
	})

	if verdict.Accepted {
		c.stepsPassed++
	}

	// La ventana y el evento temporal del paso que acaba de cerrarse. Es lo
	// que la fusión leerá al final; guardarlo aquí evita tener que recordar
	// el estado de retos ya cerrados.
	if c.eval != nil {
		if w, ok := c.eval.window(verdict.Accepted); ok {
			c.timeline.Windows = append(c.timeline.Windows, w)
		}
		// Y se le pide al analizador la parte que sólo él puede medir:
		// paralaje, gradiente 3D, ausencia de pantalla, continuidad e
		// identidad. Son las señales que separan un rostro de una foto, y el
		// gateway no las calcula ni podría.
		c.requestWindow(ctx)
	}
	c.timeline.Temporal = append(c.timeline.Temporal, fusion.TemporalEvent{
		StepID:    verdict.StepID,
		Kind:      temporalKind(verdict),
		ElapsedMS: verdict.Elapsed.Milliseconds(),
	})

	switch c.opts.Engine.State() {
	case fsm.StateCalibrating, fsm.StateChallengeActive:
		return c.revealNext()
	case fsm.StateEvaluating:
		// Se acabó la fase de retos. Pero hay dos formas de llegar aquí, y no
		// significan lo mismo: agotar el guion, o que un paso se rechazara y
		// lo cerrara de golpe.
		//
		// En el segundo caso la evidencia está incompleta —faltan los retos
		// que nunca se emitieron— y decidir con ella es decidir a ciegas.
		c.finishedScript = true
		if !verdict.Accepted {
			c.scriptCutShort = true
		}
		return c.resolve()
	case fsm.StateExpired:
		c.expire("sesión expirada")
		return nil
	default:
		c.fail(wsproto.ErrorInternal, "estado inesperado", wsproto.CloseInternalError)
		return fmt.Errorf("conn: estado inesperado %s", c.opts.Engine.State())
	}
}

func (c *Conn) onTick(ctx context.Context) error {
	verdict, err := c.opts.Engine.Tick()
	if err != nil {
		if errors.Is(err, session.ErrSessionExpired) {
			c.expire("sesión expirada")
			return nil
		}
		c.fail(wsproto.ErrorInternal, "fallo temporal", wsproto.CloseInternalError)
		return fmt.Errorf("conn: Tick: %w", err)
	}
	if verdict == nil {
		return nil
	}
	// Se acabó el plazo del paso activo sin respuesta válida.
	return c.afterVerdict(ctx, *verdict)
}

func (c *Conn) onControl(data []byte) error {
	typ, err := wsproto.PeekType(data)
	if err != nil {
		c.met.ProtocolViolations.Add(1)
		c.fail(wsproto.ErrorInvalidProtocol, "mensaje de control ilegible", wsproto.CloseProtocolViolation)
		return nil
	}

	switch typ {
	case wsproto.TypeClientTelemetry:
		// Pistas de captura. No mueven la máquina de estados: el cliente no
		// decide nada (CLAUDE.md §6).
		var msg wsproto.ClientTelemetry
		if err := json.Unmarshal(data, &msg); err != nil {
			c.met.ProtocolViolations.Add(1)
			c.fail(wsproto.ErrorInvalidProtocol, "telemetría ilegible", wsproto.CloseProtocolViolation)
			return nil
		}
		c.log.Debug("telemetría del cliente",
			"dropped", msg.DroppedFrames, "fps", msg.ActualFPS, "camera", msg.CameraState)
		return nil

	case wsproto.TypeClientAbort:
		c.finished = true
		c.met.SessionsExpired.Add(1)
		c.send(wsproto.ServerResult{
			Type:      wsproto.TypeServerResult,
			SessionID: c.opts.SessionID,
			Decision:  wsproto.DecisionInconclusive,
			ReasonKey: "aborted",
		})
		c.closeOut(outbound{kind: outClose, code: wsproto.CloseClientAbort, reason: "client_abort"})
		return nil

	case wsproto.TypeClientHello:
		// El saludo ya se hizo antes de arrancar los bucles.
		c.met.ProtocolViolations.Add(1)
		c.fail(wsproto.ErrorInvalidProtocol, "client_hello repetido", wsproto.CloseProtocolViolation)
		return nil

	default:
		c.met.ProtocolViolations.Add(1)
		c.fail(wsproto.ErrorInvalidProtocol, "tipo de mensaje no admitido", wsproto.CloseProtocolViolation)
		return nil
	}
}

// resolve cierra la sesión con veredicto y cierra la conexión limpiamente.
func (c *Conn) resolve() error {
	// Antes de decidir, se espera a las medidas de ventana pendientes. Sin
	// ellas la fusión decidiría con la mitad de las señales —sin paralaje ni
	// gradiente 3D, que son las que separan un rostro de una foto— y sin
	// saber que le faltan.
	//
	// Con tope: una medida que no llega no puede colgar la sesión. Lo que se
	// pierde sale como no medida, que es distinto de medida y mala.
	c.requestPulseWindow(context.Background())
	c.awaitWindows()

	c.finished = true

	res, err := c.opts.Engine.Resolve(c.proposeVerdict())
	if err != nil {
		if errors.Is(err, session.ErrSessionExpired) {
			c.expire("sesión expirada")
			return nil
		}
		c.fail(wsproto.ErrorInternal, "no se pudo resolver", wsproto.CloseInternalError)
		return fmt.Errorf("conn: Resolve: %w", err)
	}

	c.met.SessionsCompleted.Add(1)
	c.send(wsproto.ServerResult{
		Type:        wsproto.TypeServerResult,
		SessionID:   c.opts.SessionID,
		Decision:    decisionOf(res.Outcome),
		ReasonKey:   c.publicReasonKey(res),
		Explanation: c.explain(),
	})
	c.closeOut(outbound{kind: outClose, code: wsproto.CloseSessionComplete, reason: "session_complete"})
	return nil
}

// expire cierra por agotamiento de tiempo.
func (c *Conn) expire(reason string) {
	if c.finished {
		return
	}
	c.finished = true
	c.met.SessionsExpired.Add(1)
	c.send(wsproto.ServerResult{
		Type:      wsproto.TypeServerResult,
		SessionID: c.opts.SessionID,
		Decision:  wsproto.DecisionInconclusive,
		ReasonKey: "session_expired",
	})
	c.closeOut(outbound{kind: outClose, code: wsproto.CloseSessionExpired, reason: reason})
}

// explain arma el desglose por detector para el modo de depuración.
//
// Nil salvo que la bandera insegura esté puesta. Al cliente normal no le llega
// nada de esto: contarle a un atacante qué detector le pilló y por cuánto es
// darle exactamente lo que necesita para afinar (CLAUDE.md §6).
func (c *Conn) explain() *wsproto.VerdictExplanation {
	if !c.opts.InsecureExplainVerdict {
		return nil
	}

	out := &wsproto.VerdictExplanation{
		Score:          round4(c.fusionResult.Score),
		ProfileVersion: c.fusionResult.ProfileVersion,
	}

	// Todas las señales del catálogo, no sólo las medidas: que un detector no
	// llegara a medir es información, y la más fácil de pasar por alto.
	measured := map[fusion.Signal]fusion.SignalValue{}
	for _, v := range c.fusionResult.Signals {
		measured[v.Signal] = v
	}
	for _, signal := range fusion.AllSignals {
		v, ok := measured[signal]
		out.Signals = append(out.Signals, wsproto.SignalReport{
			Signal:   string(signal),
			Value:    round4(v.Value),
			Weight:   round4(v.Weight),
			Floor:    round4(v.Floor),
			Measured: ok,
			Passed:   ok && (v.Floor <= 0 || v.Value >= v.Floor),
		})
	}
	for _, r := range c.fusionResult.Reasons {
		out.Reasons = append(out.Reasons, string(r.Code))
	}
	return out
}

func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

// codesOf lista los motivos, para el log.
func codesOf(r fusion.Result) []string {
	out := make([]string, 0, len(r.Reasons))
	for _, reason := range r.Reasons {
		out = append(out, string(reason.Code))
	}
	return out
}

// temporalKind traduce el cierre de un paso a lo que entiende la fusión.
func temporalKind(v session.StepVerdict) fusion.TemporalKind {
	switch {
	case v.Accepted:
		return fusion.TemporalAccepted
	case v.Reason == session.ReasonResponseTooFast:
		return fusion.TemporalTooFast
	default:
		return fusion.TemporalTimeout
	}
}

// buildTimeline cierra la línea de tiempo con lo transversal de la sesión.
func (c *Conn) buildTimeline() fusion.Timeline {
	t := c.timeline
	t.SessionID = c.opts.SessionID
	t.Capture = c.quality.stats()
	// "Completado" significa que el guion llegó a su final, no que la fase
	// terminara. Un paso rechazado la termina y deja fuera todos los retos
	// siguientes: eso es evidencia parcial, y el perfil la trata como calidad
	// insuficiente para llevar a reintentar en vez de decidir.
	t.ChallengesCompleted = c.finishedScript && !c.scriptCutShort

	// El clasificador pasivo entra como una ventana más, para que pase por la
	// misma maquinaria que el resto. Se invierte porque la fusión habla en
	// "cuánto se parece a una persona real": lo que el modelo da es
	// probabilidad de ataque.
	if c.padFrames > 0 {
		// Cada modelo, su propia sub-métrica. Se invierten porque la fusión
		// habla en "cuánto se parece a una persona real" y lo que el modelo
		// da es probabilidad de ataque.
		v2 := 1 - c.padV2Worst
		v1se := 1 - c.padV1SEWorst
		t.Windows = append(t.Windows, fusion.Window{
			ID:                "texture",
			Kind:              fusion.WindowTexture,
			QualitySufficient: true,
			Submetrics:        map[string]*float64{"v2": &v2, "v1se": &v1se},
		})
	}
	return t
}

// proposeVerdict decide con el motor de fusión: pesos, suelos y umbrales del
// perfil desplegado, que se recarga en caliente y viaja versionado con cada
// resultado.
//
// La propuesta es sólo eso: el núcleo la endurece si durante los retos pasó
// algo grave, y nunca la ablanda.
func (c *Conn) proposeVerdict() session.Verdict {
	const minFramesForDecision = 3
	if c.framesUsed < minFramesForDecision {
		return session.Verdict{
			Outcome: session.OutcomeRetry,
			Reason:  session.ReasonQualityInsufficient,
		}
	}
	if c.opts.Fusion == nil {
		// Sin motor no se inventa un veredicto: se reintenta.
		return session.Verdict{
			Outcome: session.OutcomeRetry,
			Reason:  session.ReasonQualityInsufficient,
		}
	}

	c.fusionResult = c.opts.Fusion.Decide(c.buildTimeline())
	// La explicación completa, siempre en el log del SERVIDOR. Es el registro
	// que exige el §6bis: qué señal falló, con qué valor y contra qué umbral,
	// más la versión del perfil que decidió. Sin esto un veredicto del mes
	// pasado no se puede reproducir ni recurrir.
	c.log.Info("veredicto",
		"outcome", c.fusionResult.Outcome.String(),
		"score", round4(c.fusionResult.Score),
		"perfil", c.fusionResult.ProfileVersion,
		"señales_medidas", len(c.fusionResult.Signals),
		"motivos", codesOf(c.fusionResult))
	return session.Verdict{
		Outcome: outcomeOf(c.fusionResult.Outcome),
		Reason:  reasonOf(c.fusionResult),
	}
}

func outcomeOf(o fusion.Outcome) session.Outcome {
	switch o {
	case fusion.OutcomePass:
		return session.OutcomePass
	case fusion.OutcomeReject:
		return session.OutcomeFail
	default:
		return session.OutcomeRetry
	}
}

// reasonOf traduce el motivo determinante de la fusión al del núcleo.
func reasonOf(r fusion.Result) session.Reason {
	if len(r.Reasons) == 0 {
		return session.ReasonNone
	}
	switch r.Reasons[0].Family {
	case fusion.FamilyQuality:
		return session.ReasonQualityInsufficient
	case fusion.FamilyTemporal:
		return session.ReasonResponseTooFast
	default:
		return session.ReasonSignalsIndicateSpoof
	}
}

func decisionOf(o session.Outcome) string {
	switch o {
	case session.OutcomePass:
		return wsproto.DecisionLive
	case session.OutcomeFail:
		return wsproto.DecisionSpoof
	default:
		return wsproto.DecisionInconclusive
	}
}

// publicReasonKey traduce el motivo interno a algo que se le pueda decir al
// cliente.
//
// Deliberadamente genérico: contarle a un atacante que se le rechazó por
// responder demasiado rápido es enseñarle exactamente qué corregir
// (CLAUDE.md §6).
func (c *Conn) publicReasonKey(res session.Resolution) string {
	if res.Outcome == session.OutcomePass {
		return "ok"
	}
	switch res.Reason {
	case session.ReasonSessionExpired:
		return "session_expired"
	case session.ReasonAnalyzerUnavailable:
		// Esto sí se cuenta: el fallo es nuestro, no suyo, y el cliente
		// tiene que saber que reintentar tiene sentido. No dice nada del
		// guion, así que no filtra nada.
		return ReasonKeyInfrastructure
	}

	// Pistas de CAPTURA. Se le cuentan a la persona porque son sobre su
	// entorno y puede arreglarlas, y porque no dicen nada del guion ni de qué
	// detector la pilló. La regla de §6 es no explicar un RECHAZO; ayudar a
	// alguien a colocarse bien es otra cosa, y sin ello el usuario legítimo
	// se queda reintentando a ciegas.
	if c.ambientTooBright {
		return ReasonKeyAmbientLight
	}
	return "try_again"
}

// ReasonKeyInfrastructure es el motivo público de un aborto por caída de
// infraestructura.
const ReasonKeyInfrastructure = "infrastructure_error"

// ReasonKeyAmbientLight avisa de que la luz de la habitación tapa el destello.
const ReasonKeyAmbientLight = "too_much_ambient_light"

// analyzerAmbientReason es la cadena con la que el analizador marca una
// ventana de destello que no se pudo medir por exceso de luz ambiente.
//
// Va acoplada a `AMBIENT_REASON` de `analyzer/flash.py`. El acoplamiento es a
// propósito y está en los dos sitios: es la única pista de captura que se le
// da a la persona, y si se rompe en silencio la deja reintentando a ciegas.
const analyzerAmbientReason = "flash_ambient_light"

// challengeMessage traduce el paso activo al mensaje del protocolo.
//
// Lo que sale de aquí es todo lo que el cliente llegará a saber del guion.
func challengeMessage(step session.RevealedStep) wsproto.ServerChallenge {
	msg := wsproto.ServerChallenge{
		Type:        wsproto.TypeServerChallenge,
		ChallengeID: step.ID,
		Kind:        step.Kind.String(),
		DeadlineMS:  step.Deadline.Milliseconds(),
	}

	switch step.Kind {
	case challenge.KindCalibration:
		msg.PromptKey = "challenge.calibration"
		msg.Params = wsproto.MustParams(wsproto.CalibrationParams{
			HoldMS: step.Hold.Milliseconds(),
		})

	case challenge.KindHold:
		// La duración viaja porque el cliente la necesita para pintar la
		// cuenta atrás: son once segundos, y sin nada que mirar la gente cree
		// que se ha colgado y se mueve. No revela nada del guion — es la
		// duración del paso que ya está corriendo, igual que la calibración.
		msg.PromptKey = "challenge.hold"
		msg.Params = wsproto.MustParams(wsproto.CalibrationParams{
			HoldMS: step.Hold.Milliseconds(),
		})

	case challenge.KindPose:
		msg.PromptKey = "challenge.pose." + step.Pose.String()
		msg.Params = wsproto.MustParams(wsproto.PoseParams{Action: step.Pose.String()})

	case challenge.KindFlash:
		msg.PromptKey = "challenge.flash"
		seq := make([]wsproto.FlashSegment, 0, len(step.Flash))
		for _, seg := range step.Flash {
			seq = append(seq, wsproto.FlashSegment{
				Color:      seg.Color.String(),
				DurationMS: seg.Duration.Milliseconds(),
			})
		}
		msg.Params = wsproto.MustParams(wsproto.FlashParams{Sequence: seq})

	case challenge.KindGaze:
		msg.PromptKey = "challenge.gaze"
		msg.Params = wsproto.MustParams(wsproto.GazeParams{
			FromX:   step.GazeFrom.X,
			FromY:   step.GazeFrom.Y,
			DwellMS: step.Hold.Milliseconds(),
			TargetX: step.Gaze.X,
			TargetY: step.Gaze.Y,
		})
	}

	return msg
}
