// Package api expone los endpoints HTTP del gateway: salud, métricas,
// emisión de tickets de sesión y el upgrade a WebSocket.
//
// La API pública de resultados vive en el orchestrator, no aquí.
//
// PROVISIONAL: POST /v1/sessions emite el ticket desde el gateway porque
// todavía no hay servicio de orquestación al que pedírselo. Cuando lo haya,
// la emisión se va allí y aquí sólo queda el canje.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/edisonpaul4/biometrics/gateway/internal/analyzer"
	"github.com/edisonpaul4/biometrics/gateway/internal/conn"
	"github.com/edisonpaul4/biometrics/gateway/internal/session"
	"github.com/edisonpaul4/biometrics/gateway/internal/telemetry"
	"github.com/edisonpaul4/biometrics/gateway/internal/wsproto"
	"github.com/edisonpaul4/biometrics/orchestrator/core/challenge"
	"github.com/edisonpaul4/biometrics/orchestrator/core/clock"
	"github.com/edisonpaul4/biometrics/orchestrator/core/fusion"
	core "github.com/edisonpaul4/biometrics/orchestrator/core/session"
)

// Config configura el servidor.
type Config struct {
	Registry *session.Registry
	// Analyzers asigna un analizador a cada sesión. Si es nil se construye un
	// pool en proceso sobre Analyzer.
	Analyzers analyzer.Pool
	// Analyzer es el analizador en proceso. Sólo se usa si Analyzers es nil.
	Analyzer analyzer.Analyzer
	Metrics  *telemetry.Metrics
	Clock    clock.Clock
	Limits   conn.Limits
	Policy   challenge.Policy
	Capture  wsproto.CaptureParams
	Logger   *slog.Logger
	// InsecureSkipOriginCheck desactiva la comprobación de Origin. Sólo para
	// desarrollo y tests.
	InsecureSkipOriginCheck bool

	// AllowedOrigins son los orígenes de navegador autorizados, exactos
	// ("http://localhost:5173"). Vacío —lo normal— significa que sólo se
	// atiende desde el mismo origen: ni cabeceras CORS, ni upgrade desde
	// fuera. Se rellena cuando el cliente vive en otro puerto o dominio, que
	// es el caso del cliente de pruebas y del frontend Svelte.
	AllowedOrigins []string

	// Fusion decide el veredicto. Sin motor no hay veredicto posible y las
	// sesiones se resuelven en reintentar: no se aprueba por defecto.
	Fusion *fusion.Engine

	// InsecureExplainVerdict manda al cliente el desglose por detector.
	InsecureExplainVerdict bool
}

// Server sirve el gateway.
type Server struct {
	cfg Config
	mux *http.ServeMux
}

// NewServer construye el servidor con sus rutas.
func NewServer(cfg Config) *Server {
	if cfg.Clock == nil {
		cfg.Clock = clock.System{}
	}
	if cfg.Metrics == nil {
		cfg.Metrics = telemetry.New()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Analyzer == nil {
		cfg.Analyzer = analyzer.NewStub()
	}
	if cfg.Analyzers == nil {
		cfg.Analyzers = analyzer.NewLocalPool(cfg.Analyzer)
	}
	if cfg.Registry == nil {
		cfg.Registry = session.NewRegistry(cfg.Clock, session.DefaultTicketTTL)
	}
	if cfg.Limits.WriteQueue <= 0 {
		cfg.Limits = conn.DefaultLimits()
	}
	if cfg.Policy.IsZero() {
		cfg.Policy = challenge.DefaultPolicy()
	}
	if cfg.Capture == (wsproto.CaptureParams{}) {
		// 720p: el iris y el pulso se miden en píxeles, así que cada uno de
		// más es señal gratis. El pulso es el que más lo nota —el ruido baja
		// con la raíz del número de píxeles promediados del rostro— y medido
		// sobre 10 s de ventana su AUC pasa de 0,62 a 480p/15fps a 0,92 a
		// 720p/30fps.
		//
		// No se sube a 1080p aunque mediría mejor (0,99): el coste en subida
		// se dobla otra vez y deja fuera a quien tenga una conexión normal.
		cfg.Capture = wsproto.CaptureParams{
			Encoding: "jpeg", FPS: cfg.Limits.MaxFPS, Width: 1280, Height: 720, Quality: 0.7,
		}
	}

	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)
	s.mux.HandleFunc("POST /v1/sessions", s.handleCreateSession)
	s.mux.HandleFunc("OPTIONS /v1/sessions", s.handlePreflight)
	s.mux.HandleFunc("GET /v1/liveness", s.handleLiveness)
	return s
}

// Handler devuelve el enrutador.
func (s *Server) Handler() http.Handler { return s.cors(s.mux) }

// cors autoriza al navegador a llamar desde otro origen, y sólo desde uno de
// la lista. La lista se compara ENTERA y exacta: nada de comodines ni de
// devolver el origen que venga, que es como una API acaba abierta a todo el
// mundo sin que nadie lo haya decidido.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && s.originAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			// La respuesta depende del origen: sin esto una caché
			// intermedia serviría la de un origen a otro.
			w.Header().Add("Vary", "Origin")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) originAllowed(origin string) bool {
	for _, allowed := range s.cfg.AllowedOrigins {
		if strings.EqualFold(allowed, origin) {
			return true
		}
	}
	return false
}

// originPatterns traduce la lista de orígenes a los patrones de host que
// espera el verificador del upgrade, que compara hosts y no orígenes.
func (s *Server) originPatterns() []string {
	patterns := make([]string, 0, len(s.cfg.AllowedOrigins))
	for _, origin := range s.cfg.AllowedOrigins {
		if u, err := url.Parse(origin); err == nil && u.Host != "" {
			patterns = append(patterns, u.Host)
		}
	}
	return patterns
}

// handlePreflight responde al sondeo del navegador. No dice nada que no diga
// ya la respuesta real.
func (s *Server) handlePreflight(w http.ResponseWriter, r *http.Request) {
	if !s.originAllowed(r.Header.Get("Origin")) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	w.Header().Set("Access-Control-Allow-Methods", "POST")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(s.cfg.Metrics.Snapshot().Text()))
}

// CreateSessionResponse es la respuesta de POST /v1/sessions.
//
// No contiene NADA del guion: ni cuántos retos habrá, ni de qué tipo. La
// semilla tampoco sale de aquí (CLAUDE.md §6).
type CreateSessionResponse struct {
	SessionID string `json:"session_id"`
	// Token es de un solo uso.
	Token     string `json:"session_token"`
	ExpiresAt string `json:"expires_at"`
	WSPath    string `json:"ws_path"`
}

func (s *Server) handleCreateSession(w http.ResponseWriter, _ *http.Request) {
	ticket, err := s.cfg.Registry.Issue()
	if err != nil {
		s.cfg.Logger.Error("no se pudo emitir el ticket", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	s.cfg.Metrics.SessionsIssued.Add(1)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(CreateSessionResponse{
		SessionID: ticket.SessionID,
		Token:     ticket.Token,
		ExpiresAt: ticket.ExpiresAt.UTC().Format(time.RFC3339),
		WSPath:    "/v1/liveness",
	})
}

func (s *Server) handleLiveness(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: s.cfg.InsecureSkipOriginCheck,
		OriginPatterns:     s.originPatterns(),
	})
	if err != nil {
		s.cfg.Logger.Warn("upgrade fallido", "err", err)
		return
	}
	defer ws.CloseNow()

	// Límite de lectura del socket con holgura: el límite duro de verdad lo
	// aplica la conexión, para poder cerrar con un código propio en vez del
	// 1009 genérico de la librería.
	ws.SetReadLimit(int64(s.cfg.Limits.MaxFrameBytes) + 4096)

	ctx := r.Context()

	hello, err := conn.Handshake(ctx, ws, s.cfg.Limits)
	if err != nil {
		s.cfg.Metrics.ProtocolViolations.Add(1)
		s.reject(ctx, ws, wsproto.ErrorInvalidProtocol, "saludo inválido", wsproto.CloseProtocolViolation)
		return
	}

	// Canje del ticket: aquí es donde una sesión ya usada se rechaza.
	ticket, err := s.cfg.Registry.Consume(hello.SessionToken)
	if err != nil {
		code := wsproto.ErrorUnauthorized
		msg := "ticket no válido"
		if errors.Is(err, session.ErrTicketConsumed) {
			s.cfg.Metrics.SessionsRejectedReuse.Add(1)
			msg = "la sesión ya se usó"
		}
		if errors.Is(err, session.ErrTicketExpired) {
			code = wsproto.ErrorSessionExpired
			msg = "el ticket caducó"
		}
		s.reject(ctx, ws, code, msg, wsproto.CloseUnauthorized)
		return
	}

	engine, err := core.New(core.Config{
		ID:     ticket.SessionID,
		Seed:   ticket.Seed,
		Policy: s.cfg.Policy,
		Clock:  s.cfg.Clock,
		Budget: s.cfg.Limits.SessionBudget,
	})
	if err != nil {
		s.cfg.Logger.Error("no se pudo crear la sesión", "err", err)
		s.reject(ctx, ws, wsproto.ErrorInternal, "no se pudo crear la sesión", wsproto.CloseInternalError)
		return
	}

	c := conn.New(ws, conn.Options{
		SessionID: ticket.SessionID,
		Engine:    engine,
		Analyzers: s.cfg.Analyzers,
		Clock:     s.cfg.Clock,
		Metrics:   s.cfg.Metrics,
		Limits:    s.cfg.Limits,
		Logger:    s.cfg.Logger,
		Capture:   s.cfg.Capture,
		Fusion:    s.cfg.Fusion,

		InsecureExplainVerdict: s.cfg.InsecureExplainVerdict,
	})

	if err := c.Serve(ctx); err != nil {
		s.cfg.Logger.Warn("la sesión terminó con error", "session_id", ticket.SessionID, "err", err)
	}
}

// reject avisa y cierra antes de que exista conexión servida, así que aquí
// todavía se puede escribir directamente: no hay ninguna otra goroutine
// tocando el socket.
func (s *Server) reject(ctx context.Context, ws *websocket.Conn, code wsproto.ErrorCode, message string, closeCode int) {
	payload, err := json.Marshal(wsproto.ServerError{
		Type: wsproto.TypeServerError, Code: code, Message: message,
	})
	if err == nil {
		wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_ = ws.Write(wctx, websocket.MessageText, payload)
		cancel()
	}
	_ = ws.Close(websocket.StatusCode(closeCode), string(code))
}
