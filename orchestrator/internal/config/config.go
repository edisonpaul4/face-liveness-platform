// Package config carga la configuración del orchestrator desde el entorno.
//
// Los umbrales de decisión NO están aquí: viven en el perfil YAML, que se
// recarga en caliente (CLAUDE.md §9). Aquí sólo hay dónde está cada cosa y
// cuánto se conserva.
package config

import (
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/edisonpaul4/biometrics/orchestrator/internal/retention"
)

// Config agrupa la configuración de proceso del orchestrator.
type Config struct {
	HTTPAddr string

	NATSURL     string
	RedisURL    string
	PostgresDSN string

	// --- almacén de objetos ---
	MinIOEndpoint  string
	MinIOAccessKey string
	MinIOSecretKey string
	MinIOBucket    string
	MinIOUseSSL    bool

	// EvidenceKeyHex es la clave AES-256 de la evidencia, en hexadecimal.
	//
	// En producción sale de un KMS, no de una variable de entorno. Se lee de
	// aquí para que el vertical slice funcione, y el arranque avisa si se está
	// usando la de desarrollo.
	EvidenceKeyHex string
	EvidenceKeyID  string

	// --- retención ---
	EvidenceTTL       time.Duration
	TimelineTTL       time.Duration
	RetentionInterval time.Duration

	// --- estado caliente ---
	SessionTTL time.Duration
	TicketTTL  time.Duration

	// --- API ---
	APIKeys      []string
	WebSocketURL string

	// --- perfil de decisión ---
	ProfilePath     string
	ProfileInterval time.Duration
}

// DevelopmentEvidenceKey es la clave de desarrollo. Si se usa esta en
// producción, la evidencia está cifrada con una clave pública.
const DevelopmentEvidenceKey = "0000000000000000000000000000000000000000000000000000000000000000"

// Load lee la configuración del entorno aplicando valores por defecto de
// desarrollo local.
func Load() Config {
	defaults := retention.DefaultConfig()
	return Config{
		HTTPAddr:    env("ORCHESTRATOR_HTTP_ADDR", ":8081"),
		NATSURL:     env("NATS_URL", "nats://localhost:4222"),
		RedisURL:    env("REDIS_URL", "redis://localhost:6379/0"),
		PostgresDSN: env("POSTGRES_DSN", "postgres://liveness:liveness@localhost:5432/liveness?sslmode=disable"),

		MinIOEndpoint:  env("MINIO_ENDPOINT", "localhost:9000"),
		MinIOAccessKey: env("MINIO_ROOT_USER", "minioadmin"),
		MinIOSecretKey: env("MINIO_ROOT_PASSWORD", "minioadmin"),
		MinIOBucket:    env("MINIO_EVIDENCE_BUCKET", "liveness-evidence"),
		MinIOUseSSL:    envBool("MINIO_USE_SSL", false),

		EvidenceKeyHex: env("EVIDENCE_KEY_HEX", DevelopmentEvidenceKey),
		EvidenceKeyID:  env("EVIDENCE_KEY_ID", "dev-key-1"),

		EvidenceTTL:       envDuration("RETENTION_EVIDENCE_TTL", defaults.EvidenceTTL),
		TimelineTTL:       envDuration("RETENTION_TIMELINE_TTL", defaults.TimelineTTL),
		RetentionInterval: envDuration("RETENTION_INTERVAL", defaults.Interval),

		SessionTTL: envDuration("SESSION_TTL", 15*time.Minute),
		TicketTTL:  envDuration("TICKET_TTL", 2*time.Minute),

		APIKeys:      envList("ORCHESTRATOR_API_KEYS"),
		WebSocketURL: env("GATEWAY_WS_URL", "ws://localhost:8080/v1/liveness"),

		ProfilePath:     env("DECISION_PROFILE_PATH", "deploy/policy/decision-profile.yaml"),
		ProfileInterval: envDuration("DECISION_PROFILE_INTERVAL", 15*time.Second),
	}
}

// EvidenceKey devuelve la clave de cifrado ya decodificada.
func (c Config) EvidenceKey() ([]byte, error) {
	key, err := hex.DecodeString(c.EvidenceKeyHex)
	if err != nil {
		return nil, fmt.Errorf("config: EVIDENCE_KEY_HEX no es hexadecimal: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("config: EVIDENCE_KEY_HEX debe tener 32 bytes (64 hex), tiene %d", len(key))
	}
	return key, nil
}

// UsesDevelopmentKey indica si se está cifrando con la clave de ejemplo.
func (c Config) UsesDevelopmentKey() bool {
	return c.EvidenceKeyHex == DevelopmentEvidenceKey
}

// RetentionConfig proyecta los plazos de conservación.
func (c Config) RetentionConfig() retention.Config {
	base := retention.DefaultConfig()
	base.EvidenceTTL = c.EvidenceTTL
	base.TimelineTTL = c.TimelineTTL
	base.Interval = c.RetentionInterval
	return base
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envList(key string) []string {
	value := os.Getenv(key)
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
