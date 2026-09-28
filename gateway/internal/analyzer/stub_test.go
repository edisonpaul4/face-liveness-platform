package analyzer

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func analyze(t *testing.T, s *Stub, scene Scene) Features {
	t.Helper()
	payload, err := json.Marshal(scene)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	f, err := s.Analyze(context.Background(), Request{
		SessionID: "test", Seq: 1, Payload: payload, ReceivedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	return f
}

// TestStubOnlyMeasures es la frontera del CLAUDE.md §3: el analizador no
// emite juicios, sólo magnitudes.
func TestStubOnlyMeasures(t *testing.T) {
	f := analyze(t, NewStub(), Scene{Face: true, FaceAreaRatio: 0.2})

	prohibidas := []string{"is_live", "spoof_score", "attack_detected", "liveness", "passed", "decision"}
	for name := range f.Signals {
		for _, mala := range prohibidas {
			if name == mala {
				t.Errorf("el analizador emitió la señal prohibida %q", name)
			}
		}
	}
	if len(f.Signals) == 0 {
		t.Error("el analizador no midió nada")
	}
	if f.Version != StubVersion {
		t.Errorf("versión %q", f.Version)
	}
}

func TestStubIsDeterministic(t *testing.T) {
	s := NewStub()
	scene := Scene{Face: true, YawDeg: -12, FaceAreaRatio: 0.25, ScreenColor: "red"}

	a := analyze(t, s, scene)
	b := analyze(t, s, scene)
	for name, va := range a.Signals {
		if vb := b.Signals[name]; va != vb {
			t.Errorf("la señal %q cambió entre dos análisis idénticos: %v vs %v", name, va, vb)
		}
	}
}

func TestStubColorResponse(t *testing.T) {
	s := NewStub()

	cases := map[string][3]bool{ // ¿qué canales se encienden?
		"white": {true, true, true},
		"red":   {true, false, false},
		"green": {false, true, false},
		"blue":  {false, false, true},
		"":      {false, false, false},
	}
	for color, want := range cases {
		f := analyze(t, s, Scene{Face: true, ScreenColor: color})
		got := [3]bool{
			f.SignalOr("color_response_r", 0) >= 0.5,
			f.SignalOr("color_response_g", 0) >= 0.5,
			f.SignalOr("color_response_b", 0) >= 0.5,
		}
		if got != want {
			t.Errorf("color %q: canales encendidos %v, se esperaba %v", color, got, want)
		}
	}
}

// TestStubFlatSurfaceRespondsPoorly: una foto o una pantalla no reflejan
// como una cara. El stub lo simula para que /bench pueda probar ataques.
func TestStubFlatSurfaceRespondsPoorly(t *testing.T) {
	s := NewStub()

	real := analyze(t, s, Scene{Face: true, ScreenColor: "red"})
	flat := analyze(t, s, Scene{Face: true, ScreenColor: "red", Flat: true})

	if flat.SignalOr("color_response_r", 0) >= real.SignalOr("color_response_r", 0) {
		t.Error("la superficie plana respondió igual o mejor que un rostro")
	}
	if flat.SignalOr("depth_plane_residual", 1) >= real.SignalOr("depth_plane_residual", 1) {
		t.Error("la superficie plana no se distingue en planaridad")
	}
	if flat.SignalOr("rppg_snr_db", 1) >= real.SignalOr("rppg_snr_db", 1) {
		t.Error("la superficie plana no se distingue en pulso")
	}
}

func TestStubNoFace(t *testing.T) {
	f := analyze(t, NewStub(), Scene{Face: false})
	if f.Quality.FaceDetected || f.Quality.FaceCount != 0 {
		t.Errorf("calidad = %+v", f.Quality)
	}
	if f.SignalOr("color_response_r", 1) != 0 {
		t.Error("hay respuesta cromática sin rostro")
	}
}

// TestStubUnreadablePayload: un payload que no describe una escena es un
// frame ilegible, no un error de sesión.
func TestStubUnreadablePayload(t *testing.T) {
	f, err := NewStub().Analyze(context.Background(), Request{
		SessionID: "test", Payload: []byte("esto no es una escena"), ReceivedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("un frame ilegible no debe ser un error: %v", err)
	}
	if f.Quality.FaceDetected {
		t.Error("se detectó rostro en un frame ilegible")
	}
}

func TestStubRespectsContextCancellation(t *testing.T) {
	s := &Stub{Delay: 5 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := s.Analyze(ctx, Request{Payload: []byte("{}")}); err == nil {
		t.Error("se esperaba error al cancelar el contexto")
	}
}

func TestFeaturesSignalHelpers(t *testing.T) {
	f := Features{Signals: map[string]float64{"a": 1.5}}
	if v, ok := f.Signal("a"); !ok || v != 1.5 {
		t.Errorf("Signal(a) = %v, %v", v, ok)
	}
	if _, ok := f.Signal("b"); ok {
		t.Error("Signal(b) existe")
	}
	if got := f.SignalOr("b", 9); got != 9 {
		t.Errorf("SignalOr(b) = %v", got)
	}
}
