package retention_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/edisonpaul4/biometrics/orchestrator/core/clock"
	"github.com/edisonpaul4/biometrics/orchestrator/internal/retention"
	"github.com/edisonpaul4/biometrics/orchestrator/internal/store/evidence"
	"github.com/edisonpaul4/biometrics/orchestrator/internal/store/results"
	"github.com/edisonpaul4/biometrics/orchestrator/internal/storetest"
)

// session arma una sesión resuelta con su biometría, ya caducada o no.
func session(t *testing.T, res *results.Store, ev *evidence.Store, clk clock.Clock,
	expiry time.Time) (string, []results.EvidenceObject) {
	t.Helper()

	ctx := context.Background()
	sessionID := ulid.Make().String()
	now := clk.Now()

	err := res.SaveSession(ctx, results.SessionRecord{
		SessionID:       sessionID,
		SubjectID:       "sujeto-de-prueba",
		Outcome:         "pass",
		Score:           0.94,
		ReasonCodes:     []string{"passed"},
		Reasons:         json.RawMessage(`[{"code":"passed","family":"quality"}]`),
		Signals:         json.RawMessage(`[{"signal":"flash_gradient_3d","value":1}]`),
		ProfileVersion:  "2026-08-24.1",
		ProfileChecksum: "ed03b95e3cc33817",
		AnalyzerVersion: "analyzer-py-3",
		CreatedAt:       now.Add(-time.Minute),
		DecidedAt:       now,
	})
	if err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	timeline := json.RawMessage(`{"windows":[{"kind":"flash","score":0.99}],"frames":120}`)
	if _, err := res.SaveTimeline(ctx, sessionID, timeline, expiry); err != nil {
		t.Fatalf("SaveTimeline: %v", err)
	}

	var objects []results.EvidenceObject
	for _, kind := range []evidence.Kind{
		evidence.KindKeyFrame, evidence.KindReferenceFrame, evidence.KindClip,
	} {
		payload := []byte("contenido de prueba para " + string(kind) + " " + sessionID)
		object, err := ev.Put(ctx, sessionID, kind, "image/jpeg", payload, expiry)
		if err != nil {
			t.Fatalf("Put %s: %v", kind, err)
		}
		if err := res.RecordEvidence(ctx, object); err != nil {
			t.Fatalf("RecordEvidence: %v", err)
		}
		objects = append(objects, object)
	}
	return sessionID, objects
}

// TestLaRetencionBorraLaBiometriaYDejaLaAuditoria es el criterio de aceptación.
func TestLaRetencionBorraLaBiometriaYDejaLaAuditoria(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Now().UTC())

	res := storetest.Results(t)
	ev := storetest.Evidence(t, clk)

	// Una sesión cuya biometría ya venció.
	sessionID, objects := session(t, res, ev, clk, clk.Now().Add(-time.Hour))

	// Antes: todo está.
	view, err := res.Session(ctx, sessionID)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if !view.TimelineAvailable || view.EvidenceObjects != 3 {
		t.Fatalf("antes de la retención: línea=%v objetos=%d",
			view.TimelineAvailable, view.EvidenceObjects)
	}
	for _, object := range objects {
		if exists, _ := ev.Exists(ctx, object); !exists {
			t.Fatalf("el objeto %s no llegó a subirse", object.ObjectKey)
		}
	}

	// El barrendero pasa.
	job := retention.New(res, ev, retention.DefaultConfig(), clk, nil)
	report, err := job.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	t.Logf("retención: %d objetos, %d líneas de tiempo, %d sin verificar",
		report.EvidenceDeleted, report.TimelinesDeleted, report.Unverified)

	if report.Unverified != 0 {
		t.Errorf("%d borrados sin verificar", report.Unverified)
	}

	// 1. La biometría ya no está en el almacén.
	for _, object := range objects {
		exists, err := ev.Exists(ctx, object)
		if err != nil {
			t.Fatalf("Exists: %v", err)
		}
		if exists {
			t.Errorf("el objeto %s sigue en el almacén", object.ObjectKey)
		}
	}

	// 2. La línea de tiempo tampoco.
	if _, err := res.Timeline(ctx, sessionID); !errors.Is(err, results.ErrNotFound) {
		t.Errorf("la línea de tiempo sigue siendo legible: %v", err)
	}

	// 3. Pero el registro de auditoría sigue entero.
	view, err = res.Session(ctx, sessionID)
	if err != nil {
		t.Fatalf("la auditoría desapareció con la biometría: %v", err)
	}
	if view.Outcome != "pass" || view.Score != 0.94 {
		t.Errorf("el veredicto cambió: %s %.2f", view.Outcome, view.Score)
	}
	if view.ProfileVersion != "2026-08-24.1" {
		t.Errorf("se perdió la versión del perfil: %q", view.ProfileVersion)
	}
	if len(view.ReasonCodes) == 0 {
		t.Error("se perdieron los motivos")
	}
	if view.TimelineAvailable || view.EvidenceObjects != 0 {
		t.Errorf("la sesión sigue diciendo que tiene biometría: línea=%v objetos=%d",
			view.TimelineAvailable, view.EvidenceObjects)
	}

	// 4. Y hay recibo de cada borrado, verificado.
	events, err := res.RetentionEvents(ctx, sessionID)
	if err != nil {
		t.Fatalf("RetentionEvents: %v", err)
	}
	if len(events) != 4 { // 3 objetos + 1 línea de tiempo
		t.Fatalf("%d recibos, se esperaban 4: %+v", len(events), events)
	}
	for _, event := range events {
		if !event.Verified {
			t.Errorf("recibo sin verificar: %+v", event)
		}
		if event.VerifiedAt == nil {
			t.Errorf("recibo sin fecha de verificación: %+v", event)
		}
		if event.SHA256 == "" {
			t.Errorf("recibo sin huella de lo borrado: %+v", event)
		}
		if event.Reason != retention.ReasonExpired {
			t.Errorf("motivo %q", event.Reason)
		}
	}
}

// TestLaRetencionNoTocaLoQueNoHaVencido es la otra mitad: borrar de más sería
// tan grave como borrar de menos.
func TestLaRetencionNoTocaLoQueNoHaVencido(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Now().UTC())

	res := storetest.Results(t)
	ev := storetest.Evidence(t, clk)

	sessionID, objects := session(t, res, ev, clk, clk.Now().Add(24*time.Hour))

	job := retention.New(res, ev, retention.DefaultConfig(), clk, nil)
	if _, err := job.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if _, err := res.Timeline(ctx, sessionID); err != nil {
		t.Errorf("se borró una línea de tiempo vigente: %v", err)
	}
	for _, object := range objects {
		if exists, _ := ev.Exists(ctx, object); !exists {
			t.Errorf("se borró evidencia vigente: %s", object.ObjectKey)
		}
	}

	events, _ := res.RetentionEvents(ctx, sessionID)
	if len(events) != 0 {
		t.Errorf("se emitieron %d recibos sin haber borrado nada", len(events))
	}

	// Limpieza: esta sesión no ha vencido, así que se borra a mano.
	clk.Advance(48 * time.Hour)
	if _, err := job.RunOnce(ctx); err != nil {
		t.Fatalf("limpieza: %v", err)
	}
}

// TestLosPlazosPorDefectoSonCortos: si nadie configura nada, la biometría
// tiene que caducar pronto igualmente.
func TestLosPlazosPorDefectoSonCortos(t *testing.T) {
	cfg := retention.DefaultConfig()

	if cfg.EvidenceTTL > 48*time.Hour {
		t.Errorf("la evidencia vive %v por defecto; es demasiado", cfg.EvidenceTTL)
	}
	if cfg.TimelineTTL > 7*24*time.Hour {
		t.Errorf("la línea de tiempo vive %v por defecto; es demasiado", cfg.TimelineTTL)
	}
	if cfg.Interval > time.Hour {
		t.Errorf("el barrendero pasa cada %v; debería pasar más a menudo", cfg.Interval)
	}
}
