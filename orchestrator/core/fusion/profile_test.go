package fusion

import (
	"errors"
	"strings"
	"testing"
)

// TestPerfilInvalido: un perfil incoherente no se carga.
//
// Es preferible seguir con el anterior que empezar a decidir con umbrales que
// nadie ha revisado.
func TestPerfilInvalido(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(string) string
		match string
	}{
		{
			name:  "sin versión",
			edit:  func(s string) string { return strings.Replace(s, `version: "v"`, "", 1) },
			match: "versión",
		},
		{
			name:  "los pesos no suman uno",
			edit:  func(s string) string { return strings.Replace(s, "pose_parallax: 0.18", "pose_parallax: 0.38", 1) },
			match: "suman",
		},
		{
			name:  "señal desconocida",
			edit:  func(s string) string { return strings.Replace(s, "pose_parallax:", "telepatia:", 1) },
			match: "desconocida",
		},
		{
			name:  "peso negativo",
			edit:  func(s string) string { return strings.Replace(s, "pose_parallax: 0.18", "pose_parallax: -0.18", 1) },
			match: "negativo",
		},
		{
			name: "las bandas se cruzan",
			edit: func(s string) string {
				return strings.Replace(s, "reject_max: 0.45", "reject_max: 0.85", 1)
			},
			match: "banda intermedia",
		},
		{
			name: "un suelo acusa con un motivo de calidad",
			edit: func(s string) string {
				return strings.Replace(s, "code: attack_flat_surface", "code: quality_capture", 1)
			},
			match: "motivo de calidad",
		},
		{
			name: "un suelo usa un motivo inventado",
			edit: func(s string) string {
				return strings.Replace(s, "code: attack_flat_surface", "code: motivo_inventado", 1)
			},
			match: "desconocido",
		},
		{
			name: "sin intentos posibles",
			edit: func(s string) string {
				return strings.Replace(s, "max_attempts: 3", "max_attempts: 0", 1)
			},
			match: "max_attempts",
		},
		{
			name: "referencia de nitidez imposible",
			edit: func(s string) string {
				return strings.Replace(s, "sharpness_reference: 45.0", "sharpness_reference: 0", 1)
			},
			match: "sharpness_reference",
		},
		{
			name: "rango de exposición al revés",
			edit: func(s string) string {
				return strings.Replace(s, "min_brightness: 0.20", "min_brightness: 0.95", 1)
			},
			match: "exposición",
		},
	}

	base := profileWith("v", "0.75", "0.45")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.edit(base)))
			if err == nil {
				t.Fatal("se aceptó un perfil inválido")
			}
			if !errors.Is(err, ErrInvalidProfile) {
				t.Errorf("errors.Is(ErrInvalidProfile) = false: %v", err)
			}
			if !strings.Contains(err.Error(), tc.match) {
				t.Errorf("el error no menciona %q: %v", tc.match, err)
			}
		})
	}
}

func TestPerfilValidoSeCarga(t *testing.T) {
	profile, err := Parse([]byte(profileWith("v1", "0.75", "0.45")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if profile.Version != "v1" {
		t.Errorf("versión %q", profile.Version)
	}
	if profile.Checksum() == "" {
		t.Error("sin resumen: no se podría probar qué bytes decidieron")
	}
	if got := profile.Weight(SignalFlashGradient3D); got != 0.20 {
		t.Errorf("peso de flash_gradient_3d = %v", got)
	}
	if _, ok := profile.FloorFor(SignalPoseCompliance); ok {
		t.Error("pose_compliance no debería tener suelo en este perfil")
	}
}

func TestElResumenCambiaConLosBytes(t *testing.T) {
	a, _ := Parse([]byte(profileWith("v1", "0.75", "0.45")))
	b, _ := Parse([]byte(profileWith("v1", "0.76", "0.45")))

	if a.Checksum() == b.Checksum() {
		t.Error("dos perfiles distintos con el mismo resumen: no se podría distinguir " +
			"qué bytes produjeron cada veredicto")
	}
}

func TestYamlIlegible(t *testing.T) {
	if _, err := Parse([]byte("esto: no: es: yaml: válido:")); err == nil {
		t.Error("se aceptó YAML inválido")
	}
}

func TestTaxonomiaCompleta(t *testing.T) {
	// Toda señal conocida se reconoce.
	for _, signal := range AllSignals {
		if !signal.Known() {
			t.Errorf("la señal %q no se reconoce a sí misma", signal)
		}
	}
	if Signal("inventada").Known() {
		t.Error("se reconoció una señal inventada")
	}

	// Todo motivo tiene familia, y ninguna familia es la cadena vacía.
	for _, code := range AllCodes() {
		if code.Family() == "" {
			t.Errorf("el motivo %q no tiene familia", code)
		}
		if !code.Known() {
			t.Errorf("el motivo %q no se reconoce a sí mismo", code)
		}
	}

	// Un motivo desconocido se trata como calidad: en la duda no se acusa.
	if got := Code("inventado").Family(); got != FamilyQuality {
		t.Errorf("un motivo desconocido cayó en la familia %q; debería ser de calidad", got)
	}
}

// TestPulsoNoAdmiteSuelo: el pulso puede absolver, nunca acusar.
//
// El SNR del rPPG depende del tono de piel tanto como de que haya latido: la
// melanina se interpone entre la cámara y el lecho capilar y atenúa los
// fotones que vuelven. Medido (Nowara et al., CVPRW 2020), POS da entre +0,05
// y +1,76 dB para Fitzpatrick I-V y -5,58 dB para el VI, con el suelo de ruido
// de la métrica en torno a -6,5. Para una piel muy oscura la salida es
// indistinguible de no haber medido.
//
// Consecuencia: no existe umbral que separe una máscara de una persona de piel
// oscura. Un suelo aquí rechazaría por tono de piel y lo llamaría fraude, y el
// perfil se cargaría sin que nadie lo notase.
func TestPulsoNoAdmiteSuelo(t *testing.T) {
	body := `
version: "test"
weights:
  rppg_snr: 0.5
  capture_quality: 0.5
floors:
  rppg_snr:
    min: 0.30
    code: attack_flat_surface
capture:
  sharpness_reference: 45.0
  min_brightness: 0.20
  max_brightness: 0.85
  max_highlight_saturation: 0.12
  min_face_coverage: 0.70
thresholds:
  pass_min: 0.75
  reject_max: 0.40
`
	if _, err := LoadFile(writeProfile(t, body)); err == nil {
		t.Fatal("se cargó un perfil con suelo sobre el pulso; debía rechazarse")
	} else if !errors.Is(err, ErrInvalidProfile) {
		t.Fatalf("error inesperado: %v", err)
	}
}
