// Package api expone la API de control del orquestador.
//
// Tres cosas: crear una sesión, consultar su resultado y listar sesiones. Todo
// bajo autenticación: aunque aquí no se sirvan imágenes, un listado de
// veredictos con seudónimos de sujeto ya es información sensible.
//
// La biometría no sale por esta API. La línea de tiempo se puede consultar
// mientras no caduque, y cuando caduca el endpoint responde 410 Gone, que es
// justo lo que hay que decir: existió y se borró a propósito.
package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/edisonpaul4/biometrics/orchestrator/core/clock"
	"github.com/edisonpaul4/biometrics/orchestrator/internal/store/hot"
	"github.com/edisonpaul4/biometrics/orchestrator/internal/store/results"
)

// Config configura el servidor.
type Config struct {
	Results *results.Store
	Hot     *hot.Store
	Clock   clock.Clock
	Logger  *slog.Logger

	// APIKeys autorizadas. Vacío deja la API abierta, que sólo vale para
	// desarrollo: se avisa al arrancar.
	APIKeys []string
	// TicketTTL es lo que vive un ticket sin canjear.
	TicketTTL time.Duration
	// WebSocketURL es la dirección del gateway que se devuelve al crear una
	// sesión.
	WebSocketURL string
}

// Server sirve la API de control.
type Server struct {
	cfg Config
	mux *http.ServeMux
}

// NewServer construye el servidor con sus rutas.
func NewServer(cfg Config) *Server {
	if cfg.Clock == nil {
		cfg.Clock = clock.System{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.TicketTTL <= 0 {
		cfg.TicketTTL = 2 * time.Minute
	}
	if cfg.WebSocketURL == "" {
		cfg.WebSocketURL = "ws://localhost:8080/v1/liveness"
	}
	if len(cfg.APIKeys) == 0 {
		cfg.Logger.Warn("API de control SIN autenticación: sólo para desarrollo")
	}

	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("POST /v1/sessions", s.authed(s.handleCreateSession))
	s.mux.HandleFunc("GET /v1/sessions", s.authed(s.handleListSessions))
	s.mux.HandleFunc("GET /v1/sessions/{id}", s.authed(s.handleGetSession))
	s.mux.HandleFunc("GET /v1/sessions/{id}/timeline", s.authed(s.handleGetTimeline))
	s.mux.HandleFunc("GET /v1/sessions/{id}/retention", s.authed(s.handleGetRetention))
	return s
}

// Handler devuelve el enrutador.
func (s *Server) Handler() http.Handler { return s.mux }

// authed exige una clave válida.
func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(s.cfg.APIKeys) == 0 {
			next(w, r)
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		for _, key := range s.cfg.APIKeys {
			// Comparación en tiempo constante: comparar con == filtraría la
			// clave a base de medir cuánto tarda en fallar.
			if subtle.ConstantTimeCompare([]byte(token), []byte(key)) == 1 {
				next(w, r)
				return
			}
		}
		writeError(w, http.StatusUnauthorized, "unauthorized", "clave de API ausente o inválida")
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// --- crear sesión -------------------------------------------------------------

// CreateSessionRequest es lo que se pide al crear una sesión.
type CreateSessionRequest struct {
	// SubjectID es un seudónimo del sujeto, para la política de reintentos.
	// No se guarda identidad civil: eso es del sistema que llama.
	SubjectID string `json:"subject_id,omitempty"`
}

// CreateSessionResponse es el ticket emitido.
type CreateSessionResponse struct {
	SessionID string `json:"session_id"`
	// SessionToken es de un solo uso: lo canjea el gateway al abrir el
	// WebSocket, y el canje es atómico.
	SessionToken string    `json:"session_token"`
	WebSocketURL string    `json:"ws_url"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// ticket es lo que se guarda en Redis a la espera del canje.
//
// La semilla del guion va aquí y NO sale en la respuesta: quien la tenga puede
// regenerar el guion entero (CLAUDE.md §6).
type ticket struct {
	SessionID string    `json:"session_id"`
	SubjectID string    `json:"subject_id,omitempty"`
	Seed      uint64    `json:"seed"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var request CreateSessionRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&request)
	}

	token, err := randomHex(16)
	if err != nil {
		s.fail(w, "no se pudo generar el token", err)
		return
	}
	seed, err := randomUint64()
	if err != nil {
		s.fail(w, "no se pudo generar la semilla", err)
		return
	}

	now := s.cfg.Clock.Now().UTC()
	entry := ticket{
		SessionID: ulid.Make().String(),
		SubjectID: request.SubjectID,
		Seed:      seed,
		IssuedAt:  now,
		ExpiresAt: now.Add(s.cfg.TicketTTL),
	}

	payload, err := json.Marshal(entry)
	if err != nil {
		s.fail(w, "no se pudo serializar el ticket", err)
		return
	}
	if err := s.cfg.Hot.PutTicket(r.Context(), token, payload, s.cfg.TicketTTL); err != nil {
		s.fail(w, "no se pudo emitir el ticket", err)
		return
	}

	writeJSON(w, http.StatusCreated, CreateSessionResponse{
		SessionID:    entry.SessionID,
		SessionToken: token,
		WebSocketURL: s.cfg.WebSocketURL,
		ExpiresAt:    entry.ExpiresAt,
	})
}

// --- consultar ----------------------------------------------------------------

// SessionResponse es el resultado de una sesión.
type SessionResponse struct {
	SessionID   string          `json:"session_id"`
	SubjectID   string          `json:"subject_id,omitempty"`
	Outcome     string          `json:"outcome"`
	Score       float64         `json:"score"`
	ReasonCodes []string        `json:"reason_codes"`
	Reasons     json.RawMessage `json:"reasons"`
	Signals     json.RawMessage `json:"signals"`

	ProfileVersion  string `json:"profile_version"`
	ProfileChecksum string `json:"profile_checksum"`
	AnalyzerVersion string `json:"analyzer_version,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	DecidedAt time.Time `json:"decided_at"`

	// Evidence dice qué queda de la biometría y hasta cuándo.
	Evidence EvidenceStatus `json:"evidence"`
}

// EvidenceStatus resume el estado del dato biométrico de una sesión.
type EvidenceStatus struct {
	TimelineAvailable bool       `json:"timeline_available"`
	TimelineExpiresAt *time.Time `json:"timeline_expires_at,omitempty"`
	Objects           int        `json:"objects"`
	ObjectsExpireAt   *time.Time `json:"objects_expire_at,omitempty"`
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	view, err := s.cfg.Results.Session(r.Context(), r.PathValue("id"))
	if errors.Is(err, results.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "la sesión no existe")
		return
	}
	if err != nil {
		s.fail(w, "no se pudo leer la sesión", err)
		return
	}

	writeJSON(w, http.StatusOK, SessionResponse{
		SessionID: view.SessionID, SubjectID: view.SubjectID,
		Outcome: view.Outcome, Score: view.Score,
		ReasonCodes: view.ReasonCodes, Reasons: view.Reasons, Signals: view.Signals,
		ProfileVersion: view.ProfileVersion, ProfileChecksum: view.ProfileChecksum,
		AnalyzerVersion: view.AnalyzerVersion,
		CreatedAt:       view.CreatedAt, DecidedAt: view.DecidedAt,
		Evidence: EvidenceStatus{
			TimelineAvailable: view.TimelineAvailable,
			TimelineExpiresAt: view.TimelineExpiresAt,
			Objects:           view.EvidenceObjects,
			ObjectsExpireAt:   view.EvidenceExpiresAt,
		},
	})
}

// handleGetTimeline devuelve la línea de tiempo si sigue viva.
//
// Cuando ha caducado responde 410 Gone, no 404: la diferencia importa. 404 es
// "nunca existió"; 410 es "existió y se borró a propósito", que es la
// respuesta honesta cuando la retención ha hecho su trabajo.
func (s *Server) handleGetTimeline(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")

	if _, err := s.cfg.Results.Session(r.Context(), sessionID); errors.Is(err, results.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "la sesión no existe")
		return
	}

	timeline, err := s.cfg.Results.Timeline(r.Context(), sessionID)
	if errors.Is(err, results.ErrNotFound) {
		writeError(w, http.StatusGone, "evidence_expired",
			"la línea de tiempo se borró al vencer su plazo de conservación")
		return
	}
	if err != nil {
		s.fail(w, "no se pudo leer la línea de tiempo", err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(timeline)
}

// handleGetRetention devuelve los recibos de borrado de una sesión.
func (s *Server) handleGetRetention(w http.ResponseWriter, r *http.Request) {
	events, err := s.cfg.Results.RetentionEvents(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, "no se pudieron leer los recibos", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// --- listar -------------------------------------------------------------------

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	filter := results.Filter{
		Outcome:    query.Get("outcome"),
		SubjectID:  query.Get("subject_id"),
		ReasonCode: query.Get("reason_code"),
		From:       parseTime(query.Get("from")),
		To:         parseTime(query.Get("to")),
		Before:     parseTime(query.Get("before")),
	}
	if limit, err := strconv.Atoi(query.Get("limit")); err == nil {
		filter.Limit = limit
	}

	if filter.Outcome != "" && !validOutcome(filter.Outcome) {
		writeError(w, http.StatusBadRequest, "bad_request",
			"outcome debe ser pass, reject o retry")
		return
	}

	items, err := s.cfg.Results.ListSessions(r.Context(), filter)
	if err != nil {
		s.fail(w, "no se pudo listar", err)
		return
	}

	response := map[string]any{"sessions": items, "count": len(items)}
	// Cursor para la siguiente página: la fecha del último visto.
	if len(items) > 0 {
		response["next_before"] = items[len(items)-1].DecidedAt
	}
	writeJSON(w, http.StatusOK, response)
}

// --- ayudas -------------------------------------------------------------------

func (s *Server) fail(w http.ResponseWriter, message string, err error) {
	s.cfg.Logger.Error(message, "err", err)
	// Al cliente sólo el mensaje: el error interno puede llevar DSN, rutas o
	// nombres de tabla.
	writeError(w, http.StatusInternalServerError, "internal", message)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": code, "message": message})
}

func parseTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func validOutcome(outcome string) bool {
	return outcome == "pass" || outcome == "reject" || outcome == "retry"
}

func randomHex(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

func randomUint64() (uint64, error) {
	var buffer [8]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(buffer[:]), nil
}
