package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/edisonpaul4/biometrics/orchestrator/core/clock"
	"github.com/edisonpaul4/biometrics/orchestrator/internal/api"
	"github.com/edisonpaul4/biometrics/orchestrator/internal/retention"
	"github.com/edisonpaul4/biometrics/orchestrator/internal/store/evidence"
	"github.com/edisonpaul4/biometrics/orchestrator/internal/store/results"
	"github.com/edisonpaul4/biometrics/orchestrator/internal/storetest"
)

const apiKey = "clave-de-pruebas"

type rig struct {
	server   *httptest.Server
	results  *results.Store
	evidence *evidence.Store
	clock    *clock.Fake
}

func newRig(t *testing.T) *rig {
	t.Helper()

	clk := clock.NewFake(time.Now().UTC())
	res := storetest.Results(t)
	ev := storetest.Evidence(t, clk)
	hotStore := storetest.Hot(t, time.Minute)

	server := httptest.NewServer(api.NewServer(api.Config{
		Results:      res,
		Hot:          hotStore,
		Clock:        clk,
		APIKeys:      []string{apiKey},
		TicketTTL:    2 * time.Minute,
		WebSocketURL: "ws://gateway.test/v1/liveness",
	}).Handler())
	t.Cleanup(server.Close)

	return &rig{server: server, results: res, evidence: ev, clock: clk}
}

func (r *rig) do(t *testing.T, method, path string, body io.Reader) (*http.Response, []byte) {
	t.Helper()

	request, err := http.NewRequest(method, r.server.URL+path, body)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()

	data, _ := io.ReadAll(response.Body)
	return response, data
}

// persistSession guarda una sesión completa: veredicto, línea de tiempo y las
// tres piezas de evidencia.
func (r *rig) persistSession(t *testing.T, outcome string, expiry time.Time) string {
	t.Helper()
	ctx := context.Background()
	sessionID := ulid.Make().String()

	err := r.results.SaveSession(ctx, results.SessionRecord{
		SessionID:   sessionID,
		SubjectID:   "sujeto-api",
		Outcome:     outcome,
		Score:       0.91,
		ReasonCodes: []string{"passed"},
		Reasons: json.RawMessage(
			`[{"code":"passed","family":"quality","observed":0.91,"threshold":0.75}]`),
		Signals: json.RawMessage(
			`[{"signal":"flash_gradient_3d","value":1,"weight":0.2,"floor":0.3}]`),
		ProfileVersion:  "2026-08-24.1",
		ProfileChecksum: "ed03b95e3cc33817",
		AnalyzerVersion: "analyzer-py-3",
		CreatedAt:       r.clock.Now().Add(-30 * time.Second),
		DecidedAt:       r.clock.Now(),
	})
	if err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	timeline := json.RawMessage(`{"windows":[{"kind":"flash","score":0.99}],"frames":118}`)
	if _, err := r.results.SaveTimeline(ctx, sessionID, timeline, expiry); err != nil {
		t.Fatalf("SaveTimeline: %v", err)
	}

	for _, kind := range []evidence.Kind{
		evidence.KindKeyFrame, evidence.KindReferenceFrame, evidence.KindClip,
	} {
		object, err := r.evidence.Put(ctx, sessionID, kind, "image/jpeg",
			[]byte("contenido "+string(kind)), expiry)
		if err != nil {
			t.Fatalf("Put %s: %v", kind, err)
		}
		if err := r.results.RecordEvidence(ctx, object); err != nil {
			t.Fatalf("RecordEvidence: %v", err)
		}
	}
	return sessionID
}

// TestUnaSesionCompletaQuedaPersistidaYConsultable es el criterio de
// aceptación, la primera mitad.
func TestUnaSesionCompletaQuedaPersistidaYConsultable(t *testing.T) {
	rig := newRig(t)

	// 1. Crear sesión: devuelve identificador y token de WebSocket.
	response, body := rig.do(t, http.MethodPost, "/v1/sessions",
		strings.NewReader(`{"subject_id":"sujeto-api"}`))
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/sessions: %d %s", response.StatusCode, body)
	}

	var created api.CreateSessionResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("respuesta ilegible: %v", err)
	}
	if created.SessionID == "" || created.SessionToken == "" {
		t.Fatalf("ticket incompleto: %+v", created)
	}
	if created.WebSocketURL == "" {
		t.Error("no se devolvió a dónde conectarse")
	}
	if !created.ExpiresAt.After(rig.clock.Now()) {
		t.Error("el ticket nace caducado")
	}
	// La semilla del guion NO sale en la respuesta: quien la tenga puede
	// regenerar el guion entero (CLAUDE.md §6).
	if strings.Contains(strings.ToLower(string(body)), "seed") {
		t.Errorf("la respuesta filtra la semilla: %s", body)
	}

	// 2. La sesión se resuelve y se persiste.
	sessionID := rig.persistSession(t, "pass", rig.clock.Now().Add(24*time.Hour))

	// 3. Consultarla.
	response, body = rig.do(t, http.MethodGet, "/v1/sessions/"+sessionID, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET sesión: %d %s", response.StatusCode, body)
	}

	var session api.SessionResponse
	if err := json.Unmarshal(body, &session); err != nil {
		t.Fatalf("sesión ilegible: %v", err)
	}
	if session.Outcome != "pass" || session.Score != 0.91 {
		t.Errorf("veredicto %s %.2f", session.Outcome, session.Score)
	}
	if session.ProfileVersion != "2026-08-24.1" || session.ProfileChecksum == "" {
		t.Error("no se puede saber qué perfil decidió")
	}
	if len(session.ReasonCodes) == 0 || !strings.Contains(string(session.Reasons), "threshold") {
		t.Errorf("el resultado no es explicable: %s", body)
	}
	if !session.Evidence.TimelineAvailable || session.Evidence.Objects != 3 {
		t.Errorf("estado de la biometría: %+v", session.Evidence)
	}

	// 4. La línea de tiempo, mientras no caduque.
	response, body = rig.do(t, http.MethodGet, "/v1/sessions/"+sessionID+"/timeline", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET línea de tiempo: %d %s", response.StatusCode, body)
	}
	if !strings.Contains(string(body), "windows") {
		t.Errorf("línea de tiempo inesperada: %s", body)
	}

	// 5. Y aparece en los listados.
	response, body = rig.do(t, http.MethodGet,
		"/v1/sessions?outcome=pass&subject_id=sujeto-api&limit=50", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET listado: %d %s", response.StatusCode, body)
	}
	if !strings.Contains(string(body), sessionID) {
		t.Errorf("la sesión no aparece en el listado filtrado")
	}
}

// TestTrasLaRetencionQuedaLaAuditoriaYNoLaBiometria cierra el criterio por la
// vía de la API: lo que ve el backoffice cuando el plazo vence.
func TestTrasLaRetencionQuedaLaAuditoriaYNoLaBiometria(t *testing.T) {
	rig := newRig(t)

	// Una sesión cuya biometría ya venció.
	sessionID := rig.persistSession(t, "reject", rig.clock.Now().Add(-time.Hour))

	job := retention.New(rig.results, rig.evidence, retention.DefaultConfig(), rig.clock, nil)
	if _, err := job.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// El veredicto sigue consultable.
	response, body := rig.do(t, http.MethodGet, "/v1/sessions/"+sessionID, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET sesión: %d %s", response.StatusCode, body)
	}
	var session api.SessionResponse
	_ = json.Unmarshal(body, &session)
	if session.Outcome != "reject" {
		t.Errorf("se perdió el veredicto: %q", session.Outcome)
	}
	if session.Evidence.TimelineAvailable || session.Evidence.Objects != 0 {
		t.Errorf("la biometría sigue apuntada: %+v", session.Evidence)
	}

	// La línea de tiempo responde 410 Gone, no 404: existió y se borró a
	// propósito. La diferencia importa para quien audita.
	response, body = rig.do(t, http.MethodGet, "/v1/sessions/"+sessionID+"/timeline", nil)
	if response.StatusCode != http.StatusGone {
		t.Errorf("línea de tiempo caducada: %d %s, se esperaba 410", response.StatusCode, body)
	}
	if !strings.Contains(string(body), "evidence_expired") {
		t.Errorf("el 410 no explica por qué: %s", body)
	}

	// Y están los recibos de borrado.
	response, body = rig.do(t, http.MethodGet, "/v1/sessions/"+sessionID+"/retention", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET recibos: %d %s", response.StatusCode, body)
	}
	var receipts struct {
		Events []results.RetentionEvent `json:"events"`
	}
	if err := json.Unmarshal(body, &receipts); err != nil {
		t.Fatalf("recibos ilegibles: %v", err)
	}
	if len(receipts.Events) != 4 {
		t.Fatalf("%d recibos, se esperaban 4 (3 objetos + 1 línea)", len(receipts.Events))
	}
	for _, event := range receipts.Events {
		if !event.Verified {
			t.Errorf("recibo sin verificar: %+v", event)
		}
	}
}

func TestSinClaveNoSeConsultaNada(t *testing.T) {
	rig := newRig(t)

	for _, path := range []string{"/v1/sessions", "/v1/sessions/X", "/v1/sessions/X/timeline"} {
		response, err := http.Get(rig.server.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s sin clave: %d, se esperaba 401", path, response.StatusCode)
		}
	}
}

func TestUnaClaveEquivocadaNoVale(t *testing.T) {
	rig := newRig(t)

	request, _ := http.NewRequest(http.MethodGet, rig.server.URL+"/v1/sessions", nil)
	request.Header.Set("Authorization", "Bearer clave-inventada")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("%d, se esperaba 401", response.StatusCode)
	}
}

func TestSesionInexistenteDaCuatrocientosCuatro(t *testing.T) {
	rig := newRig(t)

	response, _ := rig.do(t, http.MethodGet, "/v1/sessions/NO-EXISTE", nil)
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("%d, se esperaba 404", response.StatusCode)
	}
}

func TestLosFiltrosDelListadoFuncionan(t *testing.T) {
	rig := newRig(t)

	subject := "sujeto-" + ulid.Make().String()
	ctx := context.Background()
	for _, outcome := range []string{"pass", "reject", "retry"} {
		err := rig.results.SaveSession(ctx, results.SessionRecord{
			SessionID: ulid.Make().String(), SubjectID: subject,
			Outcome: outcome, Score: 0.5,
			ReasonCodes:    []string{"code_" + outcome},
			ProfileVersion: "v", ProfileChecksum: "c",
			CreatedAt: rig.clock.Now(), DecidedAt: rig.clock.Now(),
		})
		if err != nil {
			t.Fatalf("SaveSession: %v", err)
		}
	}

	cases := []struct {
		query string
		want  int
	}{
		{fmt.Sprintf("subject_id=%s", subject), 3},
		{fmt.Sprintf("subject_id=%s&outcome=reject", subject), 1},
		{fmt.Sprintf("subject_id=%s&reason_code=code_retry", subject), 1},
		{fmt.Sprintf("subject_id=%s&reason_code=no_existe", subject), 0},
	}

	for _, tc := range cases {
		response, body := rig.do(t, http.MethodGet, "/v1/sessions?"+tc.query, nil)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("GET ?%s: %d", tc.query, response.StatusCode)
		}
		var listing struct {
			Count int `json:"count"`
		}
		_ = json.Unmarshal(body, &listing)
		if listing.Count != tc.want {
			t.Errorf("?%s devolvió %d, se esperaban %d", tc.query, listing.Count, tc.want)
		}
	}
}

func TestUnFiltroInventadoSeRechaza(t *testing.T) {
	rig := newRig(t)

	response, body := rig.do(t, http.MethodGet, "/v1/sessions?outcome=telepatia", nil)
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("%d %s, se esperaba 400", response.StatusCode, body)
	}
}
