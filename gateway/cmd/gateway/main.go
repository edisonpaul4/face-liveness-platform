// Command gateway es el borde público de la plataforma de liveness.
//
// Termina el WebSocket con el cliente, valida el protocolo, aplica los
// límites duros y sirve la sesión contra la máquina de estados.
//
// ATAJO CONSCIENTE: hoy la máquina de estados corre en este mismo proceso
// (orchestrator/core) y el análisis lo hace un stub sintético. En la
// arquitectura final la sesión vive en el orquestador y el análisis en Python
// al otro lado de NATS; ambos entran por sus interfaces (conn.Engine y
// analyzer.Analyzer) sin tocar el resto.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/edisonpaul4/biometrics/gateway/internal/analyzer"
	"github.com/edisonpaul4/biometrics/gateway/internal/api"
	"github.com/edisonpaul4/biometrics/gateway/internal/wsproto"
	"github.com/edisonpaul4/biometrics/gateway/internal/bus"
	"github.com/edisonpaul4/biometrics/gateway/internal/config"
	"github.com/edisonpaul4/biometrics/gateway/internal/session"
	"github.com/edisonpaul4/biometrics/gateway/internal/telemetry"
	"github.com/edisonpaul4/biometrics/orchestrator/core/challenge"
	"github.com/edisonpaul4/biometrics/orchestrator/core/clock"
	"github.com/edisonpaul4/biometrics/orchestrator/core/fusion"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg := config.Load()
	clk := clock.System{}
	metrics := telemetry.New()

	analyzers, closeAnalyzers, err := buildAnalyzerPool(cfg, clk, metrics, log)
	if err != nil {
		log.Error("no se pudo preparar el analizador", "err", err)
		os.Exit(1)
	}
	defer closeAnalyzers()

	policy := challenge.DefaultPolicy()
	if cfg.InsecureExplainVerdict {
		log.Warn("GATEWAY_INSECURE_EXPLAIN_VERDICT activo: el cliente recibe " +
			"el desglose por detector. Eso le dice a un atacante qué le pilló " +
			"y por cuánto. Sólo para pruebas de seguridad.")
	}
	if len(cfg.InsecureSkipChallenges) > 0 {
		skipped, err := challenge.ParseKinds(cfg.InsecureSkipChallenges)
		if err != nil {
			log.Error("GATEWAY_INSECURE_SKIP_CHALLENGES inválido", "err", err)
			os.Exit(1)
		}
		policy.InsecureSkipKinds = skipped
		// Ruidoso a propósito: quien arranque así tiene que verlo.
		log.Warn("GATEWAY_INSECURE_SKIP_CHALLENGES activo: guiones sin "+
			strings.Join(cfg.InsecureSkipChallenges, ", ")+
			". Quitar destello o mirada deja al sistema sin distinguir una "+
			"pantalla de una persona. Sólo desarrollo.",
			"omitidos", cfg.InsecureSkipChallenges)
	}

	// Motor de decisión. El perfil vive fuera del binario, se recarga en
	// caliente y su versión viaja con cada resultado: mover un umbral es una
	// operación de producto, no un despliegue.
	profile, err := fusion.LoadFile(cfg.PolicyPath)
	if err != nil {
		log.Error("no se pudo cargar el perfil de decisión", "path", cfg.PolicyPath, "err", err)
		os.Exit(1)
	}
	engine, err := fusion.New(profile, clock.System{})
	if err != nil {
		log.Error("perfil de decisión inválido", "path", cfg.PolicyPath, "err", err)
		os.Exit(1)
	}
	log.Info("perfil de decisión cargado", "path", cfg.PolicyPath, "version", profile.Version)

	srv := api.NewServer(api.Config{
		Registry:                session.NewRegistry(clk, session.DefaultTicketTTL),
		Analyzers:               analyzers,
		Metrics:                 metrics,
		Clock:                   clk,
		Limits:                  cfg.Limits(),
		Capture: wsproto.CaptureParams{
			Encoding: "jpeg",
			FPS:      cfg.MaxFPS,
			Width:    cfg.CaptureWidth,
			Height:   cfg.CaptureHeight,
			Quality:  0.7,
		},
		Policy:                  policy,
		Fusion:                  engine,
		InsecureExplainVerdict:  cfg.InsecureExplainVerdict,
		Logger:                  log,
		InsecureSkipOriginCheck: cfg.InsecureSkipOriginCheck,
		AllowedOrigins:          cfg.AllowedOrigins,
	})

	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	// Recarga en caliente: cambiar un umbral no puede costar un despliegue.
	go fusion.NewLoader(cfg.PolicyPath, engine, log).Watch(ctx, 5*time.Second)
	defer stop()

	go func() {
		log.Info("gateway listening",
			"addr", cfg.HTTPAddr,
			"session_budget", cfg.SessionBudget,
			"max_fps", cfg.MaxFPS,
			"analyzer_mode", cfg.AnalyzerMode)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("gateway shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", "err", err)
	}
}

// buildAnalyzerPool elige de dónde sale el análisis.
//
// En "nats" el gateway descubre workers por el bus y les asigna sesiones. En
// "local" analiza en proceso con el stub sintético, que sirve para desarrollo
// y para nada más: no mira píxeles.
func buildAnalyzerPool(cfg config.Config, clk clock.Clock, metrics *telemetry.Metrics, log *slog.Logger) (analyzer.Pool, func(), error) {
	if cfg.AnalyzerMode == config.AnalyzerModeLocal {
		log.Warn("analizador en proceso: stub sintético, no apto para producción")
		return analyzer.NewLocalPool(analyzer.NewStub()), func() {}, nil
	}

	nc, err := nats.Connect(cfg.NATSURL,
		nats.Name("liveness-gateway"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(500*time.Millisecond),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			log.Error("bus desconectado", "err", err)
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			log.Info("bus reconectado", "url", nc.ConnectedUrl())
		}),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("no se pudo conectar a NATS en %s: %w", cfg.NATSURL, err)
	}

	pool, err := bus.NewPool(nc, cfg.PoolOptions(), clk, metrics, log)
	if err != nil {
		nc.Close()
		return nil, nil, err
	}

	log.Info("analizador por bus", "nats", cfg.NATSURL, "lease_ttl", cfg.LeaseTTL)
	return pool, func() {
		_ = pool.Close()
		nc.Close()
	}, nil
}
