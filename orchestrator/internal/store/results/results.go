// Package results persiste en Postgres el resultado de las sesiones.
//
// Dos horizontes distintos y deliberados:
//
//   - El REGISTRO DE AUDITORÍA (`sessions`) no contiene dato biométrico:
//     veredicto, motivos, versión del perfil, huellas. Es append-only —la base
//     lo impide con triggers, no es una convención— y se conserva.
//   - La LÍNEA DE TIEMPO (`session_timelines`) son features derivadas de
//     biometría. Categoría especial bajo la LOPDP ecuatoriana: TTL corto y
//     borrado verificable.
//
// Un rechazo del mes pasado se sigue pudiendo explicar sin conservar la cara
// de nadie.
package results

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Errores del paquete.
var (
	// ErrNotFound: la sesión no existe.
	ErrNotFound = errors.New("results: sesión no encontrada")
	// ErrLocked: otro proceso está barriendo. No es un fallo.
	ErrLocked = errors.New("results: el barrendero está ocupado en otra réplica")
)

// SessionRecord es lo que se guarda al cerrar una sesión.
//
// Nótese lo que NO lleva: ni imágenes, ni landmarks, ni embeddings. Eso va a
// la línea de tiempo y a la evidencia, que caducan aparte.
type SessionRecord struct {
	SessionID string
	// SubjectID es un seudónimo del llamante para la política de reintentos.
	// Aquí no se guarda identidad civil.
	SubjectID string

	Outcome     string
	Score       float64
	ReasonCodes []string
	// Reasons y Signals van en JSON: el detalle que hace explicable un
	// rechazo.
	Reasons json.RawMessage
	Signals json.RawMessage

	ProfileVersion  string
	ProfileChecksum string
	AnalyzerVersion string

	CreatedAt time.Time
	DecidedAt time.Time
}

// EvidenceObject describe un objeto guardado en el almacén.
type EvidenceObject struct {
	ObjectID  string
	SessionID string
	// Kind: key_frame, reference_frame o clip.
	Kind      string
	Bucket    string
	ObjectKey string
	SizeBytes int64
	// SHA256 es del CIFRADO, no del claro: prueba igual que se borró ese
	// objeto exacto y no deja en la base una huella derivable de la persona.
	SHA256     string
	Encryption string
	KeyID      string
	StoredAt   time.Time
	ExpiresAt  time.Time
}

// TimelineRef identifica una línea de tiempo caducada.
type TimelineRef struct {
	SessionID string
	SHA256    string
	ExpiresAt time.Time
}

// RetentionEvent es el recibo de un borrado.
type RetentionEvent struct {
	SessionID  string
	TargetKind string
	TargetID   string
	SHA256     string
	Reason     string
	DeletedAt  time.Time
	Verified   bool
	VerifiedAt *time.Time
	Detail     string
}

// SessionView es una sesión tal y como se consulta.
type SessionView struct {
	SessionRecord
	// TimelineAvailable dice si la biometría derivada sigue viva o ya caducó.
	TimelineAvailable bool
	TimelineExpiresAt *time.Time
	EvidenceObjects   int
	EvidenceExpiresAt *time.Time
	PersistedAt       time.Time
}

// Summary es una sesión en un listado.
type Summary struct {
	SessionID       string     `json:"session_id"`
	SubjectID       string     `json:"subject_id,omitempty"`
	Outcome         string     `json:"outcome"`
	Score           float64    `json:"score"`
	ReasonCodes     []string   `json:"reason_codes"`
	ProfileVersion  string     `json:"profile_version"`
	DecidedAt       time.Time  `json:"decided_at"`
	EvidenceExpires *time.Time `json:"evidence_expires_at,omitempty"`
}

// Filter acota un listado.
type Filter struct {
	Outcome    string
	SubjectID  string
	ReasonCode string
	From       time.Time
	To         time.Time
	Limit      int
	// Before pagina hacia atrás en el tiempo: la última fecha ya vista.
	Before time.Time
}

// Store es el acceso a Postgres.
type Store struct {
	pool *pgxpool.Pool
}

// Open abre el pool de conexiones.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("results: no se pudo abrir %s: %w", dsn, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("results: sin respuesta de Postgres: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close cierra el pool.
func (s *Store) Close() { s.pool.Close() }

// SaveSession guarda el registro de auditoría.
//
// Se inserta una vez y no se toca más: la tabla no admite UPDATE ni DELETE.
// Para corregir un resultado se inserta otro, no se reescribe el anterior.
func (s *Store) SaveSession(ctx context.Context, record SessionRecord) error {
	// Un slice nil llega a SQL como NULL y choca con el NOT NULL. Se
	// normaliza aquí y no en cada llamante: olvidarlo es demasiado fácil.
	if record.ReasonCodes == nil {
		record.ReasonCodes = []string{}
	}
	if record.Reasons == nil {
		record.Reasons = json.RawMessage("[]")
	}
	if record.Signals == nil {
		record.Signals = json.RawMessage("[]")
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO liveness.sessions (
			session_id, subject_id, outcome, score, reason_codes, reasons, signals,
			profile_version, profile_checksum, analyzer_version, created_at, decided_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		record.SessionID, nullable(record.SubjectID), record.Outcome, record.Score,
		record.ReasonCodes, []byte(record.Reasons), []byte(record.Signals),
		record.ProfileVersion, record.ProfileChecksum, nullable(record.AnalyzerVersion),
		record.CreatedAt, record.DecidedAt,
	)
	if err != nil {
		return fmt.Errorf("results: no se pudo guardar la sesión %s: %w", record.SessionID, err)
	}
	return nil
}

// SaveTimeline guarda la línea de tiempo de features con su caducidad.
func (s *Store) SaveTimeline(ctx context.Context, sessionID string, timeline []byte,
	expiresAt time.Time) (string, error) {
	if len(timeline) == 0 {
		timeline = []byte("{}")
	}
	sum := sha256.Sum256(timeline)
	digest := hex.EncodeToString(sum[:])

	_, err := s.pool.Exec(ctx, `
		INSERT INTO liveness.session_timelines (session_id, timeline, sha256, expires_at)
		VALUES ($1,$2,$3,$4)`,
		sessionID, timeline, digest, expiresAt)
	if err != nil {
		return "", fmt.Errorf("results: no se pudo guardar la línea de tiempo de %s: %w", sessionID, err)
	}
	return digest, nil
}

// RecordEvidence apunta un objeto de evidencia ya subido.
func (s *Store) RecordEvidence(ctx context.Context, object EvidenceObject) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO liveness.evidence_objects (
			object_id, session_id, kind, bucket, object_key, size_bytes,
			sha256, encryption, key_id, stored_at, expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		object.ObjectID, object.SessionID, object.Kind, object.Bucket, object.ObjectKey,
		object.SizeBytes, object.SHA256, object.Encryption, object.KeyID,
		object.StoredAt, object.ExpiresAt)
	if err != nil {
		return fmt.Errorf("results: no se pudo apuntar la evidencia %s: %w", object.ObjectID, err)
	}
	return nil
}

// Session devuelve una sesión con el estado de su biometría.
func (s *Store) Session(ctx context.Context, sessionID string) (*SessionView, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT s.session_id, coalesce(s.subject_id,''), s.outcome, s.score,
		       s.reason_codes, s.reasons, s.signals,
		       s.profile_version, s.profile_checksum, coalesce(s.analyzer_version,''),
		       s.created_at, s.decided_at, s.persisted_at,
		       (t.session_id IS NOT NULL), t.expires_at,
		       (SELECT count(*) FROM liveness.evidence_objects e WHERE e.session_id = s.session_id),
		       (SELECT min(e.expires_at) FROM liveness.evidence_objects e WHERE e.session_id = s.session_id)
		FROM liveness.sessions s
		LEFT JOIN liveness.session_timelines t ON t.session_id = s.session_id
		WHERE s.session_id = $1`, sessionID)

	var view SessionView
	var reasons, signals []byte
	err := row.Scan(
		&view.SessionID, &view.SubjectID, &view.Outcome, &view.Score,
		&view.ReasonCodes, &reasons, &signals,
		&view.ProfileVersion, &view.ProfileChecksum, &view.AnalyzerVersion,
		&view.CreatedAt, &view.DecidedAt, &view.PersistedAt,
		&view.TimelineAvailable, &view.TimelineExpiresAt,
		&view.EvidenceObjects, &view.EvidenceExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("results: no se pudo leer la sesión %s: %w", sessionID, err)
	}
	view.Reasons = reasons
	view.Signals = signals
	return &view, nil
}

// Timeline devuelve la línea de tiempo si todavía no ha caducado.
func (s *Store) Timeline(ctx context.Context, sessionID string) ([]byte, error) {
	var timeline []byte
	err := s.pool.QueryRow(ctx,
		`SELECT timeline FROM liveness.session_timelines WHERE session_id = $1`,
		sessionID).Scan(&timeline)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("results: no se pudo leer la línea de tiempo: %w", err)
	}
	return timeline, nil
}

// ListSessions lista sesiones con filtros.
func (s *Store) ListSessions(ctx context.Context, filter Filter) ([]Summary, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	rows, err := s.pool.Query(ctx, `
		SELECT s.session_id, coalesce(s.subject_id,''), s.outcome, s.score,
		       s.reason_codes, s.profile_version, s.decided_at,
		       (SELECT min(e.expires_at) FROM liveness.evidence_objects e
		         WHERE e.session_id = s.session_id)
		FROM liveness.sessions s
		WHERE ($1 = '' OR s.outcome = $1)
		  AND ($2 = '' OR s.subject_id = $2)
		  AND ($3 = '' OR $3 = ANY(s.reason_codes))
		  AND ($4::timestamptz IS NULL OR s.decided_at >= $4)
		  AND ($5::timestamptz IS NULL OR s.decided_at <= $5)
		  AND ($6::timestamptz IS NULL OR s.decided_at < $6)
		ORDER BY s.decided_at DESC
		LIMIT $7`,
		filter.Outcome, filter.SubjectID, filter.ReasonCode,
		nullableTime(filter.From), nullableTime(filter.To), nullableTime(filter.Before), limit)
	if err != nil {
		return nil, fmt.Errorf("results: no se pudo listar: %w", err)
	}
	defer rows.Close()

	var out []Summary
	for rows.Next() {
		var item Summary
		if err := rows.Scan(&item.SessionID, &item.SubjectID, &item.Outcome, &item.Score,
			&item.ReasonCodes, &item.ProfileVersion, &item.DecidedAt, &item.EvidenceExpires); err != nil {
			return nil, fmt.Errorf("results: fila ilegible: %w", err)
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// --- retención ---------------------------------------------------------------

// retentionLockID identifica el cerrojo del barrendero. Es un número
// arbitrario pero fijo: lo importante es que todas las réplicas usen el mismo.
const retentionLockID = 0x11_5E_57_01

// WithRetentionLock ejecuta la función con el cerrojo del barrendero tomado.
//
// Hace falta de verdad: con varias réplicas del orquestador, dos barrenderos
// listan los mismos objetos vencidos, los borran los dos y escriben dos
// recibos por cada borrado. El registro de auditoría acabaría contando dos
// veces lo que pasó una.
//
// Si otro lo tiene tomado, no espera: se salta el ciclo. Ya barrerá el otro.
func (s *Store) WithRetentionLock(ctx context.Context, fn func(context.Context) error) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("results: no se pudo tomar conexión para el cerrojo: %w", err)
	}
	defer conn.Release()

	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, retentionLockID).
		Scan(&acquired); err != nil {
		return fmt.Errorf("results: no se pudo pedir el cerrojo: %w", err)
	}
	if !acquired {
		return ErrLocked
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, retentionLockID)
	}()

	return fn(ctx)
}

// ExpiredEvidence devuelve los objetos de evidencia ya caducados.
func (s *Store) ExpiredEvidence(ctx context.Context, now time.Time, limit int) ([]EvidenceObject, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
		SELECT object_id, session_id, kind, bucket, object_key, size_bytes,
		       sha256, encryption, key_id, stored_at, expires_at
		FROM liveness.evidence_objects
		WHERE expires_at <= $1
		ORDER BY expires_at
		LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("results: no se pudo listar la evidencia caducada: %w", err)
	}
	defer rows.Close()

	var out []EvidenceObject
	for rows.Next() {
		var object EvidenceObject
		if err := rows.Scan(&object.ObjectID, &object.SessionID, &object.Kind, &object.Bucket,
			&object.ObjectKey, &object.SizeBytes, &object.SHA256, &object.Encryption,
			&object.KeyID, &object.StoredAt, &object.ExpiresAt); err != nil {
			return nil, fmt.Errorf("results: fila de evidencia ilegible: %w", err)
		}
		out = append(out, object)
	}
	return out, rows.Err()
}

// ExpiredTimelines devuelve las líneas de tiempo caducadas.
func (s *Store) ExpiredTimelines(ctx context.Context, now time.Time, limit int) ([]TimelineRef, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
		SELECT session_id, sha256, expires_at
		FROM liveness.session_timelines
		WHERE expires_at <= $1
		ORDER BY expires_at
		LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("results: no se pudo listar las líneas caducadas: %w", err)
	}
	defer rows.Close()

	var out []TimelineRef
	for rows.Next() {
		var ref TimelineRef
		if err := rows.Scan(&ref.SessionID, &ref.SHA256, &ref.ExpiresAt); err != nil {
			return nil, fmt.Errorf("results: fila ilegible: %w", err)
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// DeleteTimeline borra la línea de tiempo de una sesión.
func (s *Store) DeleteTimeline(ctx context.Context, sessionID string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM liveness.session_timelines WHERE session_id = $1`, sessionID)
	if err != nil {
		return fmt.Errorf("results: no se pudo borrar la línea de tiempo de %s: %w", sessionID, err)
	}
	return nil
}

// DeleteEvidenceRow borra el apunte de un objeto ya eliminado del almacén.
func (s *Store) DeleteEvidenceRow(ctx context.Context, objectID string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM liveness.evidence_objects WHERE object_id = $1`, objectID)
	if err != nil {
		return fmt.Errorf("results: no se pudo borrar el apunte %s: %w", objectID, err)
	}
	return nil
}

// RecordRetention guarda el recibo de un borrado.
//
// Va a una tabla append-only: un borrado que se puede borrar no prueba nada.
func (s *Store) RecordRetention(ctx context.Context, event RetentionEvent) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO liveness.retention_events (
			session_id, target_kind, target_id, sha256, reason,
			deleted_at, verified, verified_at, detail
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		event.SessionID, event.TargetKind, event.TargetID, event.SHA256, event.Reason,
		event.DeletedAt, event.Verified, event.VerifiedAt, nullable(event.Detail))
	if err != nil {
		return fmt.Errorf("results: no se pudo guardar el recibo de borrado: %w", err)
	}
	return nil
}

// RetentionEvents devuelve los recibos de borrado de una sesión.
func (s *Store) RetentionEvents(ctx context.Context, sessionID string) ([]RetentionEvent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT session_id, target_kind, target_id, sha256, reason,
		       deleted_at, verified, verified_at, coalesce(detail,'')
		FROM liveness.retention_events
		WHERE session_id = $1
		ORDER BY deleted_at`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("results: no se pudieron leer los recibos: %w", err)
	}
	defer rows.Close()

	var out []RetentionEvent
	for rows.Next() {
		var event RetentionEvent
		if err := rows.Scan(&event.SessionID, &event.TargetKind, &event.TargetID, &event.SHA256,
			&event.Reason, &event.DeletedAt, &event.Verified, &event.VerifiedAt,
			&event.Detail); err != nil {
			return nil, fmt.Errorf("results: recibo ilegible: %w", err)
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
