package fusion

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edisonpaul4/biometrics/orchestrator/core/clock"
)

// profileYAML arma un perfil mínimo con la banda de aprobado que se le pida.
func profileYAML(version string, passMin, rejectMax float64) string {
	return strings.ReplaceAll(`
version: "VERSION"
weights:
  flash_gradient_3d: 0.20
  pose_parallax: 0.18
  flash_screen_absence: 0.15
  flash_correlation: 0.15
  pose_continuity: 0.08
  pose_compliance: 0.07
  temporal_plausibility: 0.07
  pose_identity: 0.05
  capture_quality: 0.05
floors:
  flash_gradient_3d: {min: 0.30, code: attack_flat_surface}
thresholds:
  pass_min: PASSMIN
  reject_max: REJECTMAX
quality:
  min_signals: 3
  min_capture_quality: 0.35
  require_completed_challenges: true
capture:
  sharpness_reference: 45.0
  min_brightness: 0.20
  max_brightness: 0.85
  max_highlight_saturation: 0.12
  min_face_coverage: 0.70
retry:
  max_attempts: 3
  backoff: [5s, 30s]
  window: 30m
  escalate_on_exhaustion: true
`, "VERSION", version)
}

func profileWith(version string, passMin, rejectMax string) string {
	body := profileYAML(version, 0, 0)
	body = strings.ReplaceAll(body, "PASSMIN", passMin)
	return strings.ReplaceAll(body, "REJECTMAX", rejectMax)
}

// mediocreTimeline puntúa alrededor de 0,70: cae a un lado u otro según el
// umbral, que es justo lo que se quiere poder mover sin recompilar.
func mediocreTimeline() Timeline {
	v := ptr(0.70)
	return Timeline{
		SessionID: "01J0YAML",
		Windows: []Window{
			poseWindow(v, v, v, v),
			flashWindow(v, v, v),
		},
		Temporal:            accepted(3),
		Capture:             goodCapture(),
		ChallengesCompleted: true,
	}
}

// TestCambiarElYamlCambiaElResultadoSinRecompilar es el segundo criterio de
// aceptación: mover un umbral es una operación de producto, no un despliegue.
func TestCambiarElYamlCambiaElResultadoSinRecompilar(t *testing.T) {
	path := writeProfile(t, profileWith("estricto-1", "0.75", "0.45"))

	profile, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	engine, err := New(profile, clock.NewFake(time.Now()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	loader := NewLoader(path, engine, nil)

	timeline := mediocreTimeline()

	// Con el umbral estricto, ese score no llega a aprobado.
	strict := engine.Decide(timeline)
	t.Logf("perfil estricto: score %.3f → %s", strict.Score, strict.Outcome)
	if strict.Outcome != OutcomeRetry {
		t.Fatalf("con pass_min 0.75 se esperaba reintentar, salió %s", strict.Outcome)
	}
	if strict.ProfileVersion != "estricto-1" {
		t.Errorf("versión del perfil %q", strict.ProfileVersion)
	}

	// Se cambia el YAML. Nada se recompila.
	if err := os.WriteFile(path, []byte(profileWith("permisivo-1", "0.65", "0.40")), 0o600); err != nil {
		t.Fatalf("no se pudo reescribir el perfil: %v", err)
	}
	// mtime con granularidad de segundo en algunos sistemas: se fuerza.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	changed, err := loader.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !changed {
		t.Fatal("el recargador no vio el cambio")
	}

	relaxed := engine.Decide(timeline)
	t.Logf("perfil permisivo: score %.3f → %s", relaxed.Score, relaxed.Outcome)

	if relaxed.Outcome != OutcomePass {
		t.Errorf("con pass_min 0.65 se esperaba aprobado, salió %s", relaxed.Outcome)
	}
	if relaxed.ProfileVersion != "permisivo-1" {
		t.Errorf("versión del perfil %q, se esperaba permisivo-1", relaxed.ProfileVersion)
	}
	if relaxed.ProfileChecksum == strict.ProfileChecksum {
		t.Error("el resumen del perfil no cambió al cambiar el fichero")
	}
	// Misma entrada, mismo score: lo único que cambió fue el criterio.
	if relaxed.Score != strict.Score {
		t.Errorf("el score cambió (%.4f vs %.4f): sólo debía cambiar el umbral",
			relaxed.Score, strict.Score)
	}
}

// TestUnPerfilRotoNoSustituyeAlBueno: es preferible seguir decidiendo con el
// anterior que empezar a decidir con umbrales que nadie ha revisado.
func TestUnPerfilRotoNoSustituyeAlBueno(t *testing.T) {
	path := writeProfile(t, profileWith("bueno-1", "0.75", "0.45"))

	profile, _ := LoadFile(path)
	engine, _ := New(profile, clock.NewFake(time.Now()))
	loader := NewLoader(path, engine, nil)

	// Pesos que no suman uno: el score dejaría de significar nada.
	roto := strings.Replace(profileWith("roto-1", "0.75", "0.45"),
		"flash_gradient_3d: 0.20", "flash_gradient_3d: 0.90", 1)
	if err := os.WriteFile(path, []byte(roto), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(path, future, future)

	changed, err := loader.Reload()
	if err == nil {
		t.Fatal("se aceptó un perfil con pesos que no suman uno")
	}
	if changed {
		t.Error("el recargador dice que cambió, y no debía")
	}
	if engine.Profile().Version != "bueno-1" {
		t.Errorf("el perfil en vigor es %q; debía seguir siendo el bueno",
			engine.Profile().Version)
	}
	if !errors.Is(err, ErrInvalidProfile) {
		t.Errorf("err = %v", err)
	}
}

// TestReloadSinCambiosNoHaceNada evita recargar en cada tick.
func TestReloadSinCambiosNoHaceNada(t *testing.T) {
	path := writeProfile(t, profileWith("estable-1", "0.75", "0.45"))
	profile, _ := LoadFile(path)
	engine, _ := New(profile, clock.NewFake(time.Now()))
	loader := NewLoader(path, engine, nil)

	if changed, err := loader.Reload(); err != nil || !changed {
		t.Fatalf("primera carga: %v %v", changed, err)
	}
	if changed, err := loader.Reload(); err != nil || changed {
		t.Errorf("segunda carga sin cambios: %v %v", changed, err)
	}
}

func TestWatchRecargaSolo(t *testing.T) {
	path := writeProfile(t, profileWith("v1", "0.75", "0.45"))
	profile, _ := LoadFile(path)
	engine, _ := New(profile, clock.NewFake(time.Now()))
	loader := NewLoader(path, engine, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loader.Watch(ctx, 20*time.Millisecond)

	_ = os.WriteFile(path, []byte(profileWith("v2", "0.65", "0.40")), 0o600)
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(path, future, future)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if engine.Profile().Version == "v2" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("el vigilante no recargó; sigue en %q", engine.Profile().Version)
}

func TestLoadFileConRutaInexistente(t *testing.T) {
	if _, err := LoadFile("/no/existe/perfil.yaml"); err == nil {
		t.Error("se aceptó una ruta inexistente")
	}
}
