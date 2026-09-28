// Package hot es el estado caliente de sesión en Redis.
//
// Es la única fuente de verdad de una sesión EN CURSO: máquina de estados,
// guion de retos y ventanas de señales. Cuando la sesión acaba, lo que
// importa se persiste en Postgres y esto se tira.
//
// **TTL siempre, sin excepción** (CLAUDE.md §7). Una sesión dura menos de un
// minuto; una clave sin caducidad aquí es un guion de retos y un rastro de
// actividad guardados para siempre por descuido.
package hot

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrNotFound: la clave no existe o ya caducó.
var ErrNotFound = errors.New("hot: no encontrado")

// Prefijos de clave. Ver CLAUDE.md §7.
const (
	keyPrefix   = "liveness:v1:"
	sessionKey  = keyPrefix + "session:"
	scriptKey   = keyPrefix + "script:"
	signalsKey  = keyPrefix + "signals:"
	attemptsKey = keyPrefix + "attempts:"
	ticketKey   = keyPrefix + "ticket:"
)

// Store guarda el estado caliente.
type Store struct {
	client *redis.Client
	// ttl es la caducidad por defecto de todo lo que se escribe.
	ttl time.Duration
}

// Open conecta con Redis.
func Open(ctx context.Context, url string, ttl time.Duration) (*Store, error) {
	options, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("hot: URL inválida %q: %w", url, err)
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}

	client := redis.NewClient(options)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("hot: sin respuesta de Redis: %w", err)
	}
	return &Store{client: client, ttl: ttl}, nil
}

// Close cierra la conexión.
func (s *Store) Close() error { return s.client.Close() }

// TTL es la caducidad por defecto.
func (s *Store) TTL() time.Duration { return s.ttl }

// PutSession guarda el estado de una sesión viva.
func (s *Store) PutSession(ctx context.Context, sessionID string, state []byte) error {
	return s.put(ctx, sessionKey+sessionID, state, s.ttl)
}

// Session lee el estado de una sesión.
func (s *Store) Session(ctx context.Context, sessionID string) ([]byte, error) {
	return s.get(ctx, sessionKey+sessionID)
}

// PutScript guarda el guion de retos.
//
// Esto NO sale nunca al cliente (CLAUDE.md §6). Vive aquí para que el
// orquestador pueda retomar una sesión sin regenerarlo, y muere con ella.
func (s *Store) PutScript(ctx context.Context, sessionID string, script []byte) error {
	return s.put(ctx, scriptKey+sessionID, script, s.ttl)
}

// Script lee el guion de una sesión.
func (s *Store) Script(ctx context.Context, sessionID string) ([]byte, error) {
	return s.get(ctx, scriptKey+sessionID)
}

// AppendSignals añade una medida a la ventana de la sesión.
func (s *Store) AppendSignals(ctx context.Context, sessionID string, sample []byte) error {
	key := signalsKey + sessionID
	pipe := s.client.TxPipeline()
	pipe.RPush(ctx, key, sample)
	// La caducidad se renueva con cada escritura: la clave muere cuando la
	// sesión deja de dar señales, no en un instante fijo.
	pipe.Expire(ctx, key, s.ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("hot: no se pudieron añadir señales de %s: %w", sessionID, err)
	}
	return nil
}

// Signals lee la ventana de señales de una sesión.
func (s *Store) Signals(ctx context.Context, sessionID string) ([][]byte, error) {
	values, err := s.client.LRange(ctx, signalsKey+sessionID, 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("hot: no se pudieron leer las señales: %w", err)
	}
	out := make([][]byte, 0, len(values))
	for _, value := range values {
		out = append(out, []byte(value))
	}
	return out, nil
}

// IncrementAttempts cuenta un intento de un sujeto y devuelve el total.
//
// Con varios orquestadores hace falta que esto sea atómico y compartido, y por
// eso vive en Redis: un contador en memoria de proceso multiplicaría el tope
// por el número de réplicas.
func (s *Store) IncrementAttempts(ctx context.Context, subjectID string,
	window time.Duration) (int64, error) {
	key := attemptsKey + subjectID
	pipe := s.client.TxPipeline()
	incr := pipe.Incr(ctx, key)
	// NX: sólo fija la caducidad la primera vez, para que la ventana cuente
	// desde el primer intento y no se renueve sola con cada uno.
	pipe.ExpireNX(ctx, key, window)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, fmt.Errorf("hot: no se pudo contar el intento de %s: %w", subjectID, err)
	}
	return incr.Val(), nil
}

// Attempts lee los intentos de un sujeto.
func (s *Store) Attempts(ctx context.Context, subjectID string) (int64, error) {
	value, err := s.client.Get(ctx, attemptsKey+subjectID).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("hot: no se pudieron leer los intentos: %w", err)
	}
	return value, nil
}

// ResetAttempts borra el contador de un sujeto.
func (s *Store) ResetAttempts(ctx context.Context, subjectID string) error {
	return s.client.Del(ctx, attemptsKey+subjectID).Err()
}

// PutTicket guarda un ticket de sesión sin canjear.
func (s *Store) PutTicket(ctx context.Context, token string, ticket []byte,
	ttl time.Duration) error {
	if ttl <= 0 {
		ttl = s.ttl
	}
	return s.put(ctx, ticketKey+token, ticket, ttl)
}

// ConsumeTicket canjea un ticket. La segunda vez falla.
//
// Se hace con GETDEL, que lee y borra en una sola operación atómica: con
// varios gateways, un GET seguido de un DEL dejaría una ventana en la que dos
// conexiones canjean el mismo ticket, y el de un solo uso dejaría de serlo.
func (s *Store) ConsumeTicket(ctx context.Context, token string) ([]byte, error) {
	value, err := s.client.GetDel(ctx, ticketKey+token).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("hot: no se pudo canjear el ticket: %w", err)
	}
	return value, nil
}

// Drop borra todo el estado caliente de una sesión.
//
// Se llama al cerrarla: el TTL es la red de seguridad, no el mecanismo.
func (s *Store) Drop(ctx context.Context, sessionID string) error {
	err := s.client.Del(ctx,
		sessionKey+sessionID, scriptKey+sessionID, signalsKey+sessionID).Err()
	if err != nil {
		return fmt.Errorf("hot: no se pudo soltar la sesión %s: %w", sessionID, err)
	}
	return nil
}

// KeyTTL devuelve lo que le queda de vida a una sesión.
func (s *Store) KeyTTL(ctx context.Context, sessionID string) (time.Duration, error) {
	ttl, err := s.client.TTL(ctx, sessionKey+sessionID).Result()
	if err != nil {
		return 0, fmt.Errorf("hot: no se pudo consultar el TTL: %w", err)
	}
	switch ttl {
	case -2:
		return 0, ErrNotFound
	case -1:
		// No debería pasar nunca: aquí todo se escribe con caducidad.
		return 0, errors.New("hot: clave sin TTL; es un fallo de escritura")
	}
	return ttl, nil
}

func (s *Store) put(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := s.client.Set(ctx, key, value, ttl).Err(); err != nil {
		return fmt.Errorf("hot: no se pudo escribir %s: %w", key, err)
	}
	return nil
}

func (s *Store) get(ctx context.Context, key string) ([]byte, error) {
	value, err := s.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("hot: no se pudo leer %s: %w", key, err)
	}
	return value, nil
}
