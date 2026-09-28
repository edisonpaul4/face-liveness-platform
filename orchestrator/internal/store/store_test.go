package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/oklog/ulid/v2"

	"github.com/edisonpaul4/biometrics/orchestrator/core/clock"
	"github.com/edisonpaul4/biometrics/orchestrator/internal/store/evidence"
	"github.com/edisonpaul4/biometrics/orchestrator/internal/store/hot"
	"github.com/edisonpaul4/biometrics/orchestrator/internal/store/results"
	"github.com/edisonpaul4/biometrics/orchestrator/internal/storetest"
)

// --- Redis: estado caliente con TTL natural ----------------------------------

func TestTodoLoQueSeEscribeEnRedisTieneTTL(t *testing.T) {
	ctx := context.Background()
	store := storetest.Hot(t, 30*time.Second)
	sessionID := ulid.Make().String()

	if err := store.PutSession(ctx, sessionID, []byte(`{"state":"calibrando"}`)); err != nil {
		t.Fatalf("PutSession: %v", err)
	}

	ttl, err := store.KeyTTL(ctx, sessionID)
	if err != nil {
		t.Fatalf("KeyTTL: %v", err)
	}
	if ttl <= 0 || ttl > 30*time.Second {
		t.Errorf("TTL = %v, se esperaba algo positivo y no mayor de 30s", ttl)
	}
	t.Cleanup(func() { _ = store.Drop(ctx, sessionID) })
}

func TestElEstadoCalienteVaYVuelve(t *testing.T) {
	ctx := context.Background()
	store := storetest.Hot(t, time.Minute)
	sessionID := ulid.Make().String()
	t.Cleanup(func() { _ = store.Drop(ctx, sessionID) })

	state := []byte(`{"state":"reto_activo","step":2}`)
	if err := store.PutSession(ctx, sessionID, state); err != nil {
		t.Fatalf("PutSession: %v", err)
	}
	// El guion vive aquí y NO sale nunca al cliente (CLAUDE.md §6).
	if err := store.PutScript(ctx, sessionID, []byte(`{"steps":["flash","pose"]}`)); err != nil {
		t.Fatalf("PutScript: %v", err)
	}

	got, err := store.Session(ctx, sessionID)
	if err != nil || !bytes.Equal(got, state) {
		t.Fatalf("Session = %s, %v", got, err)
	}
	if _, err := store.Script(ctx, sessionID); err != nil {
		t.Fatalf("Script: %v", err)
	}

	if err := store.Drop(ctx, sessionID); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if _, err := store.Session(ctx, sessionID); !errors.Is(err, hot.ErrNotFound) {
		t.Errorf("tras soltarla, Session = %v", err)
	}
}

func TestUnaSesionCaducaSola(t *testing.T) {
	ctx := context.Background()
	store := storetest.Hot(t, 300*time.Millisecond)
	sessionID := ulid.Make().String()

	if err := store.PutSession(ctx, sessionID, []byte("x")); err != nil {
		t.Fatalf("PutSession: %v", err)
	}
	time.Sleep(600 * time.Millisecond)

	if _, err := store.Session(ctx, sessionID); !errors.Is(err, hot.ErrNotFound) {
		t.Errorf("la sesión sigue viva pasado su TTL: %v", err)
	}
}

// TestElTicketSeCanjeaUnaSolaVez: con varios gateways, un GET seguido de un
// DEL dejaría una ventana para canjearlo dos veces.
func TestElTicketSeCanjeaUnaSolaVez(t *testing.T) {
	ctx := context.Background()
	store := storetest.Hot(t, time.Minute)
	token := ulid.Make().String()

	if err := store.PutTicket(ctx, token, []byte(`{"session_id":"S1"}`), time.Minute); err != nil {
		t.Fatalf("PutTicket: %v", err)
	}

	if _, err := store.ConsumeTicket(ctx, token); err != nil {
		t.Fatalf("primer canje: %v", err)
	}
	if _, err := store.ConsumeTicket(ctx, token); !errors.Is(err, hot.ErrNotFound) {
		t.Errorf("segundo canje: %v; el ticket debería estar gastado", err)
	}
}

func TestLosIntentosSeCuentanEnRedis(t *testing.T) {
	ctx := context.Background()
	store := storetest.Hot(t, time.Minute)
	subject := ulid.Make().String()
	t.Cleanup(func() { _ = store.ResetAttempts(ctx, subject) })

	for expected := int64(1); expected <= 3; expected++ {
		got, err := store.IncrementAttempts(ctx, subject, time.Minute)
		if err != nil || got != expected {
			t.Fatalf("intento %d: %d, %v", expected, got, err)
		}
	}

	if err := store.ResetAttempts(ctx, subject); err != nil {
		t.Fatalf("ResetAttempts: %v", err)
	}
	if got, _ := store.Attempts(ctx, subject); got != 0 {
		t.Errorf("tras el reset quedan %d intentos", got)
	}
}

// --- MinIO: la evidencia sale cifrada del proceso ----------------------------

func TestLaEvidenciaSeGuardaCifrada(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Now().UTC())
	store := storetest.Evidence(t, clk)

	res := storetest.Results(t)
	sessionID := saveMinimalSession(t, res, clk)

	secret := []byte("ESTO-ES-UNA-CARA-EN-CLARO-NO-DEBE-SALIR")
	object, err := store.Put(ctx, sessionID, evidence.KindReferenceFrame,
		"image/jpeg", secret, clk.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(ctx, object) })

	// Lo que hay en el almacén NO es lo que se guardó.
	raw := downloadRaw(t, object)
	if bytes.Contains(raw, secret) {
		t.Fatal("el almacén contiene el contenido en claro")
	}
	if object.Encryption != evidence.Algorithm {
		t.Errorf("cifrado apuntado como %q", object.Encryption)
	}
	if object.KeyID == "" {
		t.Error("no se apuntó con qué clave se cifró; sin eso no se pueden rotar")
	}

	// Y descifra bien.
	back, err := store.Get(ctx, object)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(back, secret) {
		t.Errorf("lo recuperado no coincide con lo guardado")
	}
}

func TestCadaObjetoSeCifraConUnNonceDistinto(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Now().UTC())
	store := storetest.Evidence(t, clk)
	res := storetest.Results(t)
	sessionID := saveMinimalSession(t, res, clk)

	payload := []byte("el mismo contenido dos veces")
	first, err := store.Put(ctx, sessionID, evidence.KindKeyFrame, "image/jpeg",
		payload, clk.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	second, err := store.Put(ctx, sessionID, evidence.KindKeyFrame, "image/jpeg",
		payload, clk.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(ctx, first); _ = store.Delete(ctx, second) })

	// Mismo claro, distinto cifrado: si coincidieran, se podría saber que dos
	// sesiones guardaron lo mismo sin descifrar nada.
	if first.SHA256 == second.SHA256 {
		t.Error("dos objetos con el mismo contenido dieron el mismo cifrado")
	}
}

func TestLaRutaDelObjetoSigueLaConvencion(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 8, 24, 15, 4, 5, 0, time.UTC))
	store := storetest.Evidence(t, clk)
	res := storetest.Results(t)
	sessionID := saveMinimalSession(t, res, clk)

	object, err := store.Put(ctx, sessionID, evidence.KindClip, "video/mp4",
		[]byte("clip"), clk.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(ctx, object) })

	// <yyyy>/<mm>/<dd>/<session_id>/<artefacto> (CLAUDE.md §7). La fecha
	// delante hace que borrar un día entero sea un prefijo.
	if !strings.HasPrefix(object.ObjectKey, "2026/08/24/"+sessionID+"/clip-") {
		t.Errorf("ruta %q", object.ObjectKey)
	}
}

func TestNoSeGuardaUnObjetoVacio(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Now().UTC())
	store := storetest.Evidence(t, clk)

	if _, err := store.Put(ctx, "S1", evidence.KindClip, "video/mp4", nil,
		clk.Now().Add(time.Hour)); err == nil {
		t.Error("se guardó un objeto vacío")
	}
}

func TestUnaClaveQueNoEsDe32BytesNoAbre(t *testing.T) {
	_, err := evidence.Open(context.Background(), evidence.Config{
		Endpoint: "localhost:9000", Key: []byte("corta"),
	}, nil)
	if !errors.Is(err, evidence.ErrBadKey) {
		t.Errorf("err = %v", err)
	}
}

// --- Postgres: append-only de verdad -----------------------------------------

func TestLaAuditoriaNoSePuedeReescribir(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Now().UTC())
	res := storetest.Results(t)

	sessionID := saveMinimalSession(t, res, clk)

	// Insertar dos veces la misma sesión falla por clave primaria: un
	// resultado no se sobrescribe ni por accidente.
	err := res.SaveSession(ctx, results.SessionRecord{
		SessionID: sessionID, Outcome: "reject", Score: 0.1,
		ProfileVersion: "v", ProfileChecksum: "c",
		CreatedAt: clk.Now(), DecidedAt: clk.Now(),
	})
	if err == nil {
		t.Error("se pudo reescribir un resultado ya guardado")
	}
}

func TestLaSesionSeGuardaEntera(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Now().UTC())
	res := storetest.Results(t)

	sessionID := ulid.Make().String()
	record := results.SessionRecord{
		SessionID:       sessionID,
		SubjectID:       "sujeto-7",
		Outcome:         "reject",
		Score:           0.42,
		ReasonCodes:     []string{"attack_flat_surface", "attack_emissive_surface"},
		Reasons:         json.RawMessage(`[{"code":"attack_flat_surface","signal":"flash_gradient_3d","observed":0.115,"threshold":0.3}]`),
		Signals:         json.RawMessage(`[{"signal":"flash_gradient_3d","value":0.115,"weight":0.2}]`),
		ProfileVersion:  "2026-08-24.1",
		ProfileChecksum: "ed03b95e3cc33817",
		AnalyzerVersion: "analyzer-py-3",
		CreatedAt:       clk.Now().Add(-time.Minute),
		DecidedAt:       clk.Now(),
	}
	if err := res.SaveSession(ctx, record); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	view, err := res.Session(ctx, sessionID)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if view.Outcome != "reject" || view.Score != 0.42 {
		t.Errorf("veredicto %s %.2f", view.Outcome, view.Score)
	}
	if len(view.ReasonCodes) != 2 {
		t.Errorf("motivos %v", view.ReasonCodes)
	}
	// La explicación completa sobrevive: sin ella, un rechazo no se puede
	// recurrir.
	if !bytes.Contains(view.Reasons, []byte("flash_gradient_3d")) {
		t.Errorf("se perdió el detalle del motivo: %s", view.Reasons)
	}
	if view.ProfileVersion != "2026-08-24.1" || view.ProfileChecksum == "" {
		t.Errorf("no se puede saber qué perfil decidió: %+v", view.ProfileVersion)
	}
}

func TestSesionInexistente(t *testing.T) {
	res := storetest.Results(t)
	if _, err := res.Session(context.Background(), "NO-EXISTE"); !errors.Is(err, results.ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

// --- ayudas -------------------------------------------------------------------

func saveMinimalSession(t *testing.T, res *results.Store, clk clock.Clock) string {
	t.Helper()
	sessionID := ulid.Make().String()
	err := res.SaveSession(context.Background(), results.SessionRecord{
		SessionID: sessionID, Outcome: "pass", Score: 0.9,
		ProfileVersion: "test", ProfileChecksum: "test",
		CreatedAt: clk.Now(), DecidedAt: clk.Now(),
	})
	if err != nil {
		t.Fatalf("saveMinimalSession: %v", err)
	}
	return sessionID
}

// downloadRaw baja el objeto sin pasar por el descifrado del almacén.
func downloadRaw(t *testing.T, object results.EvidenceObject) []byte {
	t.Helper()

	client, err := minio.New("localhost:9000", &minio.Options{
		Creds: credentials.NewStaticV4("minioadmin", "minioadmin", ""),
	})
	if err != nil {
		t.Fatalf("cliente crudo: %v", err)
	}
	reader, err := client.GetObject(context.Background(), object.Bucket, object.ObjectKey,
		minio.GetObjectOptions{})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return data
}
