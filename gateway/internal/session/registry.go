// Package session emite y consume los tickets de sesión del gateway.
//
// Un ticket es de UN SOLO USO: autoriza exactamente una conexión WebSocket.
// Un sessionId ya consumido se rechaza aunque el token sea correcto y aunque
// la sesión anterior fallara. Reintentar exige un ticket nuevo, y con él una
// semilla nueva y un guion nuevo: si un ataque pudiera reengancharse a la
// misma sesión, tendría barra libre para sonsacar el guion a base de
// reconexiones.
//
// Este paquete no toca Redis todavía; el registro vive en memoria. Cuando
// haya varios gateways, la marca de consumido tiene que ser atómica y
// compartida (SET NX en Redis), no local.
package session

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/edisonpaul4/biometrics/orchestrator/core/challenge"
	"github.com/edisonpaul4/biometrics/orchestrator/core/clock"
)

// Errores del registro.
var (
	// ErrUnknownTicket: el token no existe.
	ErrUnknownTicket = errors.New("session: ticket desconocido")
	// ErrTicketConsumed: el ticket ya se usó. Es el caso de reutilización.
	ErrTicketConsumed = errors.New("session: ticket ya consumido")
	// ErrTicketExpired: el ticket caducó sin usarse.
	ErrTicketExpired = errors.New("session: ticket caducado")
)

// DefaultTicketTTL es lo que vive un ticket sin usar.
const DefaultTicketTTL = 2 * time.Minute

// Ticket autoriza una sesión.
//
// Seed nunca sale del servidor: quien la tenga puede regenerar el guion
// entero (CLAUDE.md §6). No se serializa hacia el cliente ni se registra en
// logs.
type Ticket struct {
	SessionID string
	Token     string
	Seed      challenge.Seed
	IssuedAt  time.Time
	ExpiresAt time.Time
}

type record struct {
	ticket     Ticket
	consumedAt time.Time
}

// Registry emite tickets y garantiza que cada uno se use una sola vez.
// Es seguro para uso concurrente.
type Registry struct {
	clk clock.Clock
	ttl time.Duration

	mu       sync.Mutex
	byToken  map[string]*record
	consumed map[string]time.Time // sessionID → instante de consumo
}

// NewRegistry crea un registro en memoria.
func NewRegistry(clk clock.Clock, ttl time.Duration) *Registry {
	if clk == nil {
		clk = clock.System{}
	}
	if ttl <= 0 {
		ttl = DefaultTicketTTL
	}
	return &Registry{
		clk:      clk,
		ttl:      ttl,
		byToken:  make(map[string]*record),
		consumed: make(map[string]time.Time),
	}
}

// Issue emite un ticket nuevo con identificador ULID, token y semilla
// criptográficamente aleatorios.
func (r *Registry) Issue() (Ticket, error) {
	token, err := randomToken()
	if err != nil {
		return Ticket{}, err
	}
	seed, err := randomSeed()
	if err != nil {
		return Ticket{}, err
	}

	now := r.clk.Now()
	t := Ticket{
		SessionID: ulid.Make().String(),
		Token:     token,
		Seed:      seed,
		IssuedAt:  now,
		ExpiresAt: now.Add(r.ttl),
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked(now)
	r.byToken[t.Token] = &record{ticket: t}
	return t, nil
}

// Consume canjea el ticket. La segunda vez falla, y ahí está la gracia.
func (r *Registry) Consume(token string) (Ticket, error) {
	now := r.clk.Now()

	r.mu.Lock()
	defer r.mu.Unlock()

	rec, ok := r.byToken[token]
	if !ok {
		return Ticket{}, ErrUnknownTicket
	}
	if !rec.consumedAt.IsZero() {
		return Ticket{}, fmt.Errorf("%w: sesión %s el %s",
			ErrTicketConsumed, rec.ticket.SessionID, rec.consumedAt.UTC().Format(time.RFC3339))
	}
	if _, used := r.consumed[rec.ticket.SessionID]; used {
		return Ticket{}, fmt.Errorf("%w: sesión %s", ErrTicketConsumed, rec.ticket.SessionID)
	}
	if !now.Before(rec.ticket.ExpiresAt) {
		return Ticket{}, ErrTicketExpired
	}

	rec.consumedAt = now
	r.consumed[rec.ticket.SessionID] = now
	return rec.ticket, nil
}

// Consumed indica si esa sesión ya se usó.
func (r *Registry) Consumed(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.consumed[sessionID]
	return ok
}

// Pending es cuántos tickets siguen sin canjear.
func (r *Registry) Pending() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, rec := range r.byToken {
		if rec.consumedAt.IsZero() {
			n++
		}
	}
	return n
}

// Sweep descarta los tickets caducados sin consumir.
func (r *Registry) Sweep() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked(r.clk.Now())
}

// sweepLocked exige r.mu tomado.
//
// La marca de consumido NO se borra aquí: es justo lo que impide reutilizar
// un sessionId. Caducarla es tarea aparte, con una ventana mucho más larga
// que la vida de una sesión.
func (r *Registry) sweepLocked(now time.Time) {
	for token, rec := range r.byToken {
		if rec.consumedAt.IsZero() && !now.Before(rec.ticket.ExpiresAt) {
			delete(r.byToken, token)
		}
	}
}

func randomToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("session: no se pudo generar el token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func randomSeed() (challenge.Seed, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, fmt.Errorf("session: no se pudo generar la semilla: %w", err)
	}
	return challenge.Seed(binary.BigEndian.Uint64(b[:])), nil
}
