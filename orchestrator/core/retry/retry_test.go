package retry

import (
	"testing"
	"time"

	"github.com/edisonpaul4/biometrics/orchestrator/core/clock"
	"github.com/edisonpaul4/biometrics/orchestrator/core/fusion"
)

var t0 = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

func policy() fusion.RetryPolicy {
	return fusion.RetryPolicy{
		MaxAttempts:          3,
		Backoff:              []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute},
		Window:               30 * time.Minute,
		EscalateOnExhaustion: true,
	}
}

func gate(t *testing.T, p fusion.RetryPolicy) (*Gate, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(t0)
	return NewGate(p, NewMemoryStore(p.Window), clk), clk
}

func result(outcome fusion.Outcome, code fusion.Code) fusion.Result {
	return fusion.Result{
		Outcome: outcome,
		Reasons: []fusion.Reason{{Code: code, Family: code.Family()}},
	}
}

func TestAprobarLimpiaElHistorico(t *testing.T) {
	g, _ := gate(t, policy())

	g.Evaluate("sujeto-1", result(fusion.OutcomeRetry, fusion.CodeQualityCapture))
	g.Evaluate("sujeto-1", result(fusion.OutcomeRetry, fusion.CodeQualityCapture))

	decision := g.Evaluate("sujeto-1", result(fusion.OutcomePass, fusion.CodePassed))

	if decision.Disposition != DispositionFinal {
		t.Errorf("disposición %s", decision.Disposition)
	}
	if got := g.Attempts("sujeto-1"); got != 0 {
		t.Errorf("quedan %d intentos apuntados; aprobar debería limpiarlos", got)
	}
}

// TestUnRechazoNoSeReintenta: repetir un ataque sólo le da al atacante otra
// tirada con un guion nuevo.
func TestUnRechazoNoSeReintenta(t *testing.T) {
	g, _ := gate(t, policy())

	decision := g.Evaluate("sujeto-1", result(fusion.OutcomeReject, fusion.CodeAttackFlatSurface))

	if decision.Disposition != DispositionFinal {
		t.Errorf("disposición %s, se esperaba final", decision.Disposition)
	}
	if decision.RetryAfter != 0 {
		t.Errorf("se ofreció esperar %v tras un rechazo", decision.RetryAfter)
	}
}

func TestElBackoffCreceConLosIntentos(t *testing.T) {
	g, _ := gate(t, policy())
	quality := result(fusion.OutcomeRetry, fusion.CodeQualityCapture)

	first := g.Evaluate("sujeto-1", quality)
	second := g.Evaluate("sujeto-1", quality)

	if first.Disposition != DispositionRetry || second.Disposition != DispositionRetry {
		t.Fatalf("disposiciones %s / %s", first.Disposition, second.Disposition)
	}
	if first.RetryAfter != 5*time.Second {
		t.Errorf("primera espera %v, se esperaba 5s", first.RetryAfter)
	}
	if second.RetryAfter != 30*time.Second {
		t.Errorf("segunda espera %v, se esperaba 30s", second.RetryAfter)
	}
	if first.Attempt != 1 || second.Attempt != 2 {
		t.Errorf("intentos %d y %d", first.Attempt, second.Attempt)
	}
	if second.Remaining != 1 {
		t.Errorf("quedan %d intentos, se esperaba 1", second.Remaining)
	}
}

// TestAlAgotarseEscalaARevisionManual es el requisito.
//
// Rechazar a alguien porque su cámara es mala tres veces seguidas sigue siendo
// rechazarlo por su cámara.
func TestAlAgotarseEscalaARevisionManual(t *testing.T) {
	g, _ := gate(t, policy())
	quality := result(fusion.OutcomeRetry, fusion.CodeQualityCapture)

	g.Evaluate("sujeto-1", quality)
	g.Evaluate("sujeto-1", quality)
	third := g.Evaluate("sujeto-1", quality)

	if third.Disposition != DispositionEscalate {
		t.Fatalf("disposición %s, se esperaba escalado", third.Disposition)
	}
	if third.Remaining != 0 {
		t.Errorf("quedan %d intentos", third.Remaining)
	}

	var found bool
	for _, reason := range third.Reasons {
		if reason.Code == fusion.CodePolicyManualReview {
			found = true
			if reason.Family != fusion.FamilyPolicy {
				t.Errorf("familia %q", reason.Family)
			}
		}
	}
	if !found {
		t.Errorf("no se explicó el escalado: %v", third.Reasons)
	}
	// Y el motivo original sigue ahí: quien revise el caso necesita saber
	// contra qué se estrelló.
	if third.Reasons[0].Code != fusion.CodeQualityCapture {
		t.Errorf("se perdió el motivo original: %v", third.Reasons)
	}
}

func TestSinEscaladoSeAgotaYPunto(t *testing.T) {
	p := policy()
	p.EscalateOnExhaustion = false
	g, _ := gate(t, p)
	quality := result(fusion.OutcomeRetry, fusion.CodeQualityCapture)

	g.Evaluate("s", quality)
	g.Evaluate("s", quality)
	third := g.Evaluate("s", quality)

	if third.Disposition != DispositionFinal {
		t.Errorf("disposición %s", third.Disposition)
	}
	var found bool
	for _, reason := range third.Reasons {
		found = found || reason.Code == fusion.CodePolicyRetriesExhausted
	}
	if !found {
		t.Errorf("motivos %v", third.Reasons)
	}
}

// TestLosIntentosCaducan: quien lo intentó ayer no arrastra el tope de hoy.
func TestLosIntentosCaducan(t *testing.T) {
	g, clk := gate(t, policy())
	quality := result(fusion.OutcomeRetry, fusion.CodeQualityCapture)

	g.Evaluate("sujeto-1", quality)
	g.Evaluate("sujeto-1", quality)
	if got := g.Attempts("sujeto-1"); got != 2 {
		t.Fatalf("intentos %d", got)
	}

	clk.Advance(31 * time.Minute)

	fresh := g.Evaluate("sujeto-1", quality)
	if fresh.Attempt != 1 {
		t.Errorf("intento %d tras caducar la ventana, se esperaba 1", fresh.Attempt)
	}
	if fresh.Disposition != DispositionRetry {
		t.Errorf("disposición %s", fresh.Disposition)
	}
}

func TestElTopeEsPorIdentificador(t *testing.T) {
	g, _ := gate(t, policy())
	quality := result(fusion.OutcomeRetry, fusion.CodeQualityCapture)

	g.Evaluate("ana", quality)
	g.Evaluate("ana", quality)
	g.Evaluate("ana", quality)

	// Otro sujeto empieza de cero.
	decision := g.Evaluate("luis", quality)
	if decision.Disposition != DispositionRetry || decision.Attempt != 1 {
		t.Errorf("el tope de un sujeto afectó a otro: %+v", decision)
	}
}

func TestBackoffMasCortoQueLosIntentos(t *testing.T) {
	p := policy()
	p.MaxAttempts = 5
	p.Backoff = []time.Duration{time.Second, 2 * time.Second}
	g, _ := gate(t, p)
	quality := result(fusion.OutcomeRetry, fusion.CodeQualityCapture)

	g.Evaluate("s", quality)
	g.Evaluate("s", quality)
	third := g.Evaluate("s", quality)

	// Se repite la última: el backoff no puede desaparecer justo cuando más
	// falta hace.
	if third.RetryAfter != 2*time.Second {
		t.Errorf("espera %v, se esperaba repetir la última (2s)", third.RetryAfter)
	}
}

func TestSinBackoffConfiguradoNoSeEspera(t *testing.T) {
	p := policy()
	p.Backoff = nil
	g, _ := gate(t, p)

	decision := g.Evaluate("s", result(fusion.OutcomeRetry, fusion.CodeQualityCapture))
	if decision.RetryAfter != 0 {
		t.Errorf("espera %v", decision.RetryAfter)
	}
}

func TestResetBorraElHistorico(t *testing.T) {
	g, _ := gate(t, policy())
	g.Evaluate("s", result(fusion.OutcomeRetry, fusion.CodeQualityCapture))

	g.Reset("s")
	if got := g.Attempts("s"); got != 0 {
		t.Errorf("quedan %d intentos", got)
	}
}

func TestDisposicionString(t *testing.T) {
	cases := map[Disposition]string{
		DispositionUnknown:  "unknown",
		DispositionFinal:    "final",
		DispositionRetry:    "retry",
		DispositionEscalate: "escalate",
		Disposition(9):      "disposition(9)",
	}
	for d, want := range cases {
		if got := d.String(); got != want {
			t.Errorf("Disposition(%d) = %q, se esperaba %q", uint8(d), got, want)
		}
	}
}

func TestLaTiendaEnMemoriaEsSeguraEnConcurrencia(t *testing.T) {
	store := NewMemoryStore(time.Hour)
	done := make(chan struct{})

	for i := 0; i < 20; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				store.Record("s", t0)
				store.Attempts("s")
			}
		}()
	}
	for i := 0; i < 20; i++ {
		<-done
	}

	if count, _ := store.Attempts("s"); count != 1000 {
		t.Errorf("intentos %d, se esperaban 1000", count)
	}
}
