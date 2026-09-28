// Package config carga la configuración del gateway desde el entorno.
//
// Regla: aquí no hay valores de negocio (umbrales de decisión, catálogos de
// retos). Eso vive en el orchestrator. Sí están los límites duros de
// transporte, que son de este proceso.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/edisonpaul4/biometrics/gateway/internal/bus"
	"github.com/edisonpaul4/biometrics/gateway/internal/conn"
)

// Config agrupa la configuración de proceso del gateway.
type Config struct {
	HTTPAddr string // dirección de escucha HTTP/WS

	NATSURL string // bus core, sin JetStream

	RedisURL string // estado caliente de sesión

	// --- límites duros de conexión ---
	//
	// Ninguno es opcional: sin ellos, un cliente hostil decide cuánta
	// memoria y cuánta CPU consume el servidor.
	MaxFrameBytes    int
	MaxControlBytes  int
	MaxFPS           float64
	SessionBudget    time.Duration
	MaxClockDrift    time.Duration
	WriteQueue       int
	WriteTimeout     time.Duration
	TickInterval     time.Duration
	HandshakeTimeout time.Duration

	FrameTimeout time.Duration
	// WindowTimeout es lo que se espera a las medidas de ventana al cerrar.
	WindowTimeout time.Duration
	MaxInflight   int

	// --- captura impuesta al cliente ---
	//
	// Existen como variables de entorno para poder comparar resoluciones sin
	// recompilar. Hizo falta al descubrir que el destello dejaba de
	// correlacionar a 720p: seis sesiones de seis fallaron, y la sospecha era
	// que a esa resolución el fondo recibe el destello y la medida
	// diferencial —cara partida por fondo— lo cancela. Sin un interruptor no
	// se puede aislar la resolución del resto de variables de la habitación.
	CaptureWidth  int
	CaptureHeight int

	// --- analizador ---

	// AnalyzerMode elige de dónde sale el análisis: "nats" (workers reales
	// por el bus) o "local" (stub en proceso, sólo desarrollo).
	AnalyzerMode string

	AnnounceTTL  time.Duration
	LeaseTimeout time.Duration
	LeaseTTL     time.Duration
	LeaseCheck   time.Duration

	// InsecureSkipOriginCheck desactiva la comprobación de Origin en el
	// upgrade. Sólo para desarrollo.
	InsecureSkipOriginCheck bool

	// AllowedOrigins son los orígenes de navegador autorizados. Vacío por
	// defecto: sólo mismo origen.
	AllowedOrigins []string

	// PolicyPath es el perfil de decisión: pesos, suelos y umbrales.
	PolicyPath string

	// InsecureExplainVerdict manda al cliente el desglose por detector. Sólo
	// para pruebas de seguridad: en producción enseña al atacante qué
	// corregir.
	InsecureExplainVerdict bool

	// InsecureSkipChallenges son tipos de reto excluidos del guion. Sólo
	// desarrollo: sin destello ni mirada el sistema no defiende contra una
	// pantalla.
	InsecureSkipChallenges []string
}

// AnalyzerModes admitidos.
const (
	AnalyzerModeNATS  = "nats"
	AnalyzerModeLocal = "local"
)

// Load lee la configuración del entorno aplicando valores por defecto de
// desarrollo local.
func Load() Config {
	d := conn.DefaultLimits()
	return Config{
		HTTPAddr: env("GATEWAY_HTTP_ADDR", ":8080"),
		NATSURL:  env("NATS_URL", "nats://localhost:4222"),
		RedisURL: env("REDIS_URL", "redis://localhost:6379/0"),

		MaxFrameBytes:   envInt("GATEWAY_MAX_FRAME_BYTES", d.MaxFrameBytes),
		MaxControlBytes: envInt("GATEWAY_MAX_CONTROL_BYTES", d.MaxControlBytes),
		MaxFPS:          envFloat("GATEWAY_MAX_FPS", d.MaxFPS),
		// 480p por defecto. A 720p el cliente pide 20,7 Mbit/s de subida y
		// desde un móvil eso va al límite: un parón del enlace se lleva un
		// reto entero. Y no se paga con precisión — medido sobre frames
		// reales con la cara cerca, a 480p el iris se mide MEJOR (1 frame
		// sin medir de 24 contra 4) por un tercio de los bytes. Se sube con
		// GATEWAY_CAPTURE_WIDTH/HEIGHT, o con `make gateway CAPTURE=720`.
		CaptureWidth:     envInt("GATEWAY_CAPTURE_WIDTH", 640),
		CaptureHeight:    envInt("GATEWAY_CAPTURE_HEIGHT", 480),
		SessionBudget:    envDuration("GATEWAY_SESSION_BUDGET", d.SessionBudget),
		MaxClockDrift:    envDuration("GATEWAY_MAX_CLOCK_DRIFT", d.MaxClockDrift),
		WriteQueue:       envInt("GATEWAY_WRITE_QUEUE", d.WriteQueue),
		WriteTimeout:     envDuration("GATEWAY_WRITE_TIMEOUT", d.WriteTimeout),
		TickInterval:     envDuration("GATEWAY_TICK_INTERVAL", d.TickInterval),
		HandshakeTimeout: envDuration("GATEWAY_HANDSHAKE_TIMEOUT", d.HandshakeTimeout),
		FrameTimeout:     envDuration("GATEWAY_FRAME_TIMEOUT", d.FrameTimeout),
		WindowTimeout:    envDuration("GATEWAY_WINDOW_TIMEOUT", d.WindowTimeout),
		MaxInflight:      envInt("GATEWAY_MAX_INFLIGHT", d.MaxInflight),

		AnalyzerMode: env("GATEWAY_ANALYZER_MODE", AnalyzerModeNATS),
		AnnounceTTL:  envDuration("GATEWAY_ANNOUNCE_TTL", 5*time.Second),
		LeaseTimeout: envDuration("GATEWAY_LEASE_TIMEOUT", 500*time.Millisecond),
		LeaseTTL:     envDuration("GATEWAY_LEASE_TTL", 1500*time.Millisecond),
		LeaseCheck:   envDuration("GATEWAY_LEASE_CHECK", 250*time.Millisecond),

		InsecureSkipOriginCheck: envBool("GATEWAY_INSECURE_SKIP_ORIGIN_CHECK", false),
		AllowedOrigins:          envList("GATEWAY_ALLOWED_ORIGINS"),
		PolicyPath:              env("GATEWAY_POLICY_PATH", "../deploy/policy/decision-profile.yaml"),
		InsecureExplainVerdict:  envBool("GATEWAY_INSECURE_EXPLAIN_VERDICT", false),
		InsecureSkipChallenges:  envList("GATEWAY_INSECURE_SKIP_CHALLENGES"),
	}
}

// Limits proyecta la configuración a los límites de conexión.
func (c Config) Limits() conn.Limits {
	return conn.Limits{
		MaxFrameBytes:    c.MaxFrameBytes,
		MaxControlBytes:  c.MaxControlBytes,
		MaxFPS:           c.MaxFPS,
		SessionBudget:    c.SessionBudget,
		MaxClockDrift:    c.MaxClockDrift,
		WriteQueue:       c.WriteQueue,
		WriteTimeout:     c.WriteTimeout,
		TickInterval:     c.TickInterval,
		FrameTimeout:     c.FrameTimeout,
		WindowTimeout:    c.WindowTimeout,
		MaxInflight:      c.MaxInflight,
		HandshakeTimeout: c.HandshakeTimeout,
	}
}

// PoolOptions proyecta la configuración a las opciones del bus.
func (c Config) PoolOptions() bus.PoolOptions {
	return bus.PoolOptions{
		AnnounceTTL:  c.AnnounceTTL,
		LeaseTimeout: c.LeaseTimeout,
		LeaseTTL:     c.LeaseTTL,
		LeaseCheck:   c.LeaseCheck,
	}
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v, ok := os.LookupEnv(key); ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// envList lee una lista separada por comas. Vacía si la variable no está.
func envList(key string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
