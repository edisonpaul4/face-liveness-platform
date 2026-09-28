// Command orchestrator es el cerebro de la plataforma de liveness.
//
// Posee la máquina de estados de la sesión, el guion de retos, la fusión de
// señales, el veredicto, la persistencia y la API de control.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/edisonpaul4/biometrics/orchestrator/core/clock"
	"github.com/edisonpaul4/biometrics/orchestrator/core/fusion"
	"github.com/edisonpaul4/biometrics/orchestrator/internal/api"
	"github.com/edisonpaul4/biometrics/orchestrator/internal/config"
	"github.com/edisonpaul4/biometrics/orchestrator/internal/retention"
	"github.com/edisonpaul4/biometrics/orchestrator/internal/store/evidence"
	"github.com/edisonpaul4/biometrics/orchestrator/internal/store/hot"
	"github.com/edisonpaul4/biometrics/orchestrator/internal/store/results"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("el orquestador no pudo arrancar", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg := config.Load()
	clk := clock.System{}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// --- perfil de decisión, recargable en caliente ---
	profile, err := fusion.LoadFile(cfg.ProfilePath)
	if err != nil {
		return err
	}
	engine, err := fusion.New(profile, clk)
	if err != nil {
		return err
	}
	loader := fusion.NewLoader(cfg.ProfilePath, engine, log)
	go loader.Watch(ctx, cfg.ProfileInterval)
	log.Info("perfil de decisión cargado",
		"version", profile.Version, "checksum", profile.Checksum())

	// --- persistencia ---
	resultsStore, err := results.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		return err
	}
	defer resultsStore.Close()

	hotStore, err := hot.Open(ctx, cfg.RedisURL, cfg.SessionTTL)
	if err != nil {
		return err
	}
	defer func() { _ = hotStore.Close() }()

	key, err := cfg.EvidenceKey()
	if err != nil {
		return err
	}
	if cfg.UsesDevelopmentKey() {
		log.Warn("la evidencia se está cifrando con la CLAVE DE DESARROLLO; " +
			"en producción tiene que venir de un KMS")
	}

	evidenceStore, err := evidence.Open(ctx, evidence.Config{
		Endpoint:  cfg.MinIOEndpoint,
		AccessKey: cfg.MinIOAccessKey,
		SecretKey: cfg.MinIOSecretKey,
		Bucket:    cfg.MinIOBucket,
		UseSSL:    cfg.MinIOUseSSL,
		Key:       key,
		KeyID:     cfg.EvidenceKeyID,
	}, clk)
	if err != nil {
		return err
	}

	// --- retención: borra biometría vencida y deja el recibo ---
	job := retention.New(resultsStore, evidenceStore, cfg.RetentionConfig(), clk, log)
	go job.Run(ctx)
	log.Info("retención activa",
		"evidencia_ttl", cfg.EvidenceTTL,
		"linea_tiempo_ttl", cfg.TimelineTTL,
		"cada", cfg.RetentionInterval)

	// --- API de control ---
	server := api.NewServer(api.Config{
		Results:      resultsStore,
		Hot:          hotStore,
		Clock:        clk,
		Logger:       log,
		APIKeys:      cfg.APIKeys,
		TicketTTL:    cfg.TicketTTL,
		WebSocketURL: cfg.WebSocketURL,
	})

	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Info("orchestrator listening", "addr", cfg.HTTPAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("orchestrator shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
}
