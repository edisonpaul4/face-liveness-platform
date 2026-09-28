// Package retention borra el dato biométrico caducado y deja constancia.
//
// Bajo la LOPDP ecuatoriana la biometría es categoría especial: conservarla
// más de lo necesario no es un descuido operativo, es un incumplimiento. Por
// eso el borrado es un trabajo automático y periódico, no una tarea que
// alguien recuerde hacer.
//
// Lo que hace, por cada objeto vencido:
//
//  1. Borra del almacén.
//  2. **Comprueba que ya no está.** Un borrado que no se verifica es un
//     borrado que se supone.
//  3. Escribe el recibo en el registro de auditoría, que es append-only.
//  4. Sólo entonces quita el apunte.
//
// Ese orden importa: si el proceso muere a mitad, el objeto ya no está y el
// apunte sigue, así que el siguiente ciclo lo reintenta y acaba el trabajo. Al
// revés se perdería el rastro.
package retention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/edisonpaul4/biometrics/orchestrator/core/clock"
	"github.com/edisonpaul4/biometrics/orchestrator/internal/store/evidence"
	"github.com/edisonpaul4/biometrics/orchestrator/internal/store/results"
)

// Motivos de borrado.
const (
	// ReasonExpired: venció el plazo de conservación.
	ReasonExpired = "expired"
	// ReasonSubjectRequest: lo pidió el titular. Derecho de supresión.
	ReasonSubjectRequest = "subject_request"
)

// Config son los plazos de conservación.
//
// Cortos por defecto, y a propósito. Alargar un plazo tiene que ser una
// decisión consciente con una base legal detrás, no lo que pasa si nadie
// configura nada.
type Config struct {
	// EvidenceTTL es cuánto viven los frames y el clip. Son una cara.
	EvidenceTTL time.Duration
	// TimelineTTL es cuánto viven las features derivadas. No son una cara,
	// pero salen de una.
	TimelineTTL time.Duration
	// Interval es cada cuánto pasa el barrendero.
	Interval time.Duration
	// BatchSize acota cuánto borra de una vez.
	BatchSize int
}

// DefaultConfig son los plazos por defecto.
func DefaultConfig() Config {
	return Config{
		// Un día. La evidencia sirve para revisar una decisión reciente y
		// para el match 1:1 contra documento, que ocurre en minutos.
		EvidenceTTL: 24 * time.Hour,
		// Tres días: da margen para depurar una racha de rechazos.
		TimelineTTL: 72 * time.Hour,
		Interval:    15 * time.Minute,
		BatchSize:   500,
	}
}

// Report resume una pasada.
type Report struct {
	EvidenceDeleted  int
	EvidenceFailed   int
	TimelinesDeleted int
	// Unverified son los borrados que no se pudieron comprobar. Cada uno es
	// un aviso: el objeto puede seguir ahí.
	Unverified int
	StartedAt  time.Time
	Duration   time.Duration
}

// Job borra lo vencido.
type Job struct {
	results  *results.Store
	evidence *evidence.Store
	cfg      Config
	clk      clock.Clock
	log      *slog.Logger
}

// New crea el trabajo.
func New(res *results.Store, ev *evidence.Store, cfg Config, clk clock.Clock,
	log *slog.Logger) *Job {
	if cfg.EvidenceTTL <= 0 {
		cfg = DefaultConfig()
	}
	if clk == nil {
		clk = clock.System{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Job{results: res, evidence: ev, cfg: cfg, clk: clk, log: log}
}

// Config devuelve los plazos en vigor.
func (j *Job) Config() Config { return j.cfg }

// EvidenceExpiry es cuándo caduca la evidencia de una sesión que empieza ahora.
func (j *Job) EvidenceExpiry() time.Time { return j.clk.Now().Add(j.cfg.EvidenceTTL) }

// TimelineExpiry es cuándo caduca la línea de tiempo.
func (j *Job) TimelineExpiry() time.Time { return j.clk.Now().Add(j.cfg.TimelineTTL) }

// RunOnce hace una pasada.
func (j *Job) RunOnce(ctx context.Context) (Report, error) {
	started := j.clk.Now()
	report := Report{StartedAt: started}

	// Cerrojo compartido: con varias réplicas, dos barrenderos borrarían lo
	// mismo y escribirían dos recibos por cada borrado. La auditoría contaría
	// dos veces lo que pasó una.
	err := j.results.WithRetentionLock(ctx, func(ctx context.Context) error {
		if err := j.sweepEvidence(ctx, started, &report); err != nil {
			return err
		}
		return j.sweepTimelines(ctx, started, &report)
	})
	if errors.Is(err, results.ErrLocked) {
		// Otra réplica está barriendo. No es un fallo.
		return report, nil
	}
	if err != nil {
		return report, err
	}

	report.Duration = j.clk.Now().Sub(started)
	if report.EvidenceDeleted+report.TimelinesDeleted > 0 || report.EvidenceFailed > 0 {
		j.log.Info("retención",
			"evidencia_borrada", report.EvidenceDeleted,
			"lineas_borradas", report.TimelinesDeleted,
			"fallos", report.EvidenceFailed,
			"sin_verificar", report.Unverified)
	}
	return report, nil
}

// Run barre periódicamente hasta que se cancele el contexto.
func (j *Job) Run(ctx context.Context) {
	ticker := time.NewTicker(j.cfg.Interval)
	defer ticker.Stop()

	// Una pasada al arrancar: si el proceso estuvo caído, hay atraso.
	if _, err := j.RunOnce(ctx); err != nil {
		j.log.Error("la retención falló al arrancar", "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := j.RunOnce(ctx); err != nil {
				j.log.Error("la retención falló", "err", err)
			}
		}
	}
}

func (j *Job) sweepEvidence(ctx context.Context, now time.Time, report *Report) error {
	expired, err := j.results.ExpiredEvidence(ctx, now, j.cfg.BatchSize)
	if err != nil {
		return err
	}

	for _, object := range expired {
		if err := j.evidence.Delete(ctx, object); err != nil {
			report.EvidenceFailed++
			j.log.Error("no se pudo borrar evidencia",
				"object_id", object.ObjectID, "key", object.ObjectKey, "err", err)
			continue
		}

		// Comprobar. Un borrado sin comprobar es un borrado que se supone.
		stillThere, err := j.evidence.Exists(ctx, object)
		verified := err == nil && !stillThere
		detail := ""
		switch {
		case err != nil:
			detail = "no se pudo verificar: " + err.Error()
			report.Unverified++
		case stillThere:
			detail = "el almacén sigue devolviendo el objeto"
			report.Unverified++
		}

		verifiedAt := now
		event := results.RetentionEvent{
			SessionID:  object.SessionID,
			TargetKind: "evidence_object",
			TargetID:   object.ObjectID,
			SHA256:     object.SHA256,
			Reason:     ReasonExpired,
			DeletedAt:  now,
			Verified:   verified,
			Detail:     detail,
		}
		if verified {
			event.VerifiedAt = &verifiedAt
		}
		if err := j.results.RecordRetention(ctx, event); err != nil {
			// El objeto ya no está pero el recibo no se pudo escribir. Se
			// deja el apunte para que el siguiente ciclo lo reintente: sin
			// recibo no hay prueba de nada.
			report.EvidenceFailed++
			j.log.Error("no se pudo escribir el recibo de borrado",
				"object_id", object.ObjectID, "err", err)
			continue
		}

		if err := j.results.DeleteEvidenceRow(ctx, object.ObjectID); err != nil {
			j.log.Error("no se pudo quitar el apunte", "object_id", object.ObjectID, "err", err)
			continue
		}
		report.EvidenceDeleted++
	}
	return nil
}

func (j *Job) sweepTimelines(ctx context.Context, now time.Time, report *Report) error {
	expired, err := j.results.ExpiredTimelines(ctx, now, j.cfg.BatchSize)
	if err != nil {
		return err
	}

	for _, ref := range expired {
		if err := j.results.DeleteTimeline(ctx, ref.SessionID); err != nil {
			j.log.Error("no se pudo borrar la línea de tiempo",
				"session_id", ref.SessionID, "err", err)
			continue
		}

		// Verificar: leerla otra vez tiene que dar "no está".
		_, err := j.results.Timeline(ctx, ref.SessionID)
		verified := errors.Is(err, results.ErrNotFound)
		verifiedAt := now

		event := results.RetentionEvent{
			SessionID:  ref.SessionID,
			TargetKind: "session_timeline",
			TargetID:   ref.SessionID,
			SHA256:     ref.SHA256,
			Reason:     ReasonExpired,
			DeletedAt:  now,
			Verified:   verified,
		}
		if verified {
			event.VerifiedAt = &verifiedAt
		} else {
			event.Detail = "la línea de tiempo sigue siendo legible tras el borrado"
			report.Unverified++
		}

		if err := j.results.RecordRetention(ctx, event); err != nil {
			return fmt.Errorf("retention: no se pudo escribir el recibo de %s: %w", ref.SessionID, err)
		}
		report.TimelinesDeleted++
	}
	return nil
}
