package fusion

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edisonpaul4/biometrics/orchestrator/core/clock"
)

// profilePath es el perfil que se despliega de verdad. Los tests deciden con
// él, no con uno inventado: si alguien mueve un umbral en producción y rompe
// una expectativa, tiene que enterarse aquí.
const profilePath = "../../../deploy/policy/decision-profile.yaml"

func testEngine(t *testing.T) *Engine {
	t.Helper()

	profile, err := LoadFile(profilePath)
	if err != nil {
		t.Fatalf("no se pudo cargar el perfil desplegado: %v", err)
	}
	engine, err := New(profile, clock.NewFake(time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return engine
}

// ptr envuelve un valor medido. Un nil significa "no se pudo medir", que no es
// lo mismo que cero.
func ptr(v float64) *float64 { return &v }

// poseWindow arma una ventana de pose.
func poseWindow(compliance, continuity, parallax, identity *float64) Window {
	return Window{
		ID: "pose-1", Kind: WindowPose, QualitySufficient: true,
		Submetrics: map[string]*float64{
			"compliance": compliance, "continuity": continuity,
			"parallax": parallax, "identity": identity,
		},
	}
}

// flashWindow arma una ventana de destello.
func flashWindow(correlation, gradient, screenAbsence *float64) Window {
	return Window{
		ID: "flash-1", Kind: WindowFlash, QualitySufficient: true,
		Submetrics: map[string]*float64{
			"correlation": correlation, "gradient_3d": gradient,
			"screen_absence": screenAbsence,
		},
	}
}

// goodCapture son unas estadísticas de captura sin problemas.
func goodCapture() CaptureStats {
	return CaptureStats{
		Frames: 120, FramesWithFace: 118, MeanSharpness: 90,
		MeanBrightness: 0.52, MeanHighlightSaturation: 0.01,
	}
}

// accepted son n pasos respondidos a tiempo.
func accepted(n int) []TemporalEvent {
	events := make([]TemporalEvent, n)
	for i := range events {
		events[i] = TemporalEvent{StepID: "s", Kind: TemporalAccepted, ElapsedMS: 900}
	}
	return events
}

// baseline es una sesión completa y sana; cada caso cambia lo que le interesa.
func baseline() Timeline {
	return Timeline{
		SessionID:           "01J0TEST",
		Windows:             []Window{poseWindow(ptr(1), ptr(1), ptr(1), ptr(1)), flashWindow(ptr(1), ptr(1), ptr(1))},
		Temporal:            accepted(4),
		Capture:             goodCapture(),
		ChallengesCompleted: true,
	}
}

// TestDecisionTable es el criterio de aceptación: entradas conocidas,
// veredicto y motivos esperados.
func TestDecisionTable(t *testing.T) {
	engine := testEngine(t)

	cases := []struct {
		name        string
		mutate      func(*Timeline)
		wantOutcome Outcome
		wantCodes   []Code
		wantFamily  Family
	}{
		{
			name:        "rostro real: todo alto",
			mutate:      func(*Timeline) {},
			wantOutcome: OutcomePass,
			wantCodes:   []Code{CodePassed},
		},
		{
			name: "foto impresa: el destello no produce relieve",
			mutate: func(tl *Timeline) {
				// Cifras medidas en el banco del analizador para una foto.
				tl.Windows[1] = flashWindow(ptr(1.0), ptr(0.115), ptr(1.0))
			},
			wantOutcome: OutcomeReject,
			wantCodes:   []Code{CodeAttackFlatSurface},
			wantFamily:  FamilyAttack,
		},
		{
			name: "replay en pantalla: brilla y no sigue al destello",
			mutate: func(tl *Timeline) {
				tl.Windows[1] = flashWindow(ptr(0.0), ptr(0.226), ptr(0.187))
			},
			wantOutcome: OutcomeReject,
			wantCodes: []Code{
				CodeAttackNoColorResponse, CodeAttackEmissiveSurface, CodeAttackFlatSurface,
			},
			wantFamily: FamilyAttack,
		},
		{
			name: "foto girada: el paralaje dice que es un plano",
			mutate: func(tl *Timeline) {
				tl.Windows[0] = poseWindow(ptr(0.9), ptr(1.0), ptr(0.10), ptr(1.0))
			},
			wantOutcome: OutcomeReject,
			wantCodes:   []Code{CodeAttackPlanarMotion},
			wantFamily:  FamilyAttack,
		},
		{
			// Un cambio de cara a mitad de sesión YA NO SE RECHAZA, y hay que
			// tenerlo escrito en vez de descubrirlo.
			//
			// `pose_identity` perdió su suelo porque acusaba a personas reales:
			// sobre un sujeto legítimo dio 0 · 0,1178 · 0,2239 · 0,3335 ·
			// 0,3834 · 0,6582 · 0,7499 · 0,8301 · 0,9832, y con el suelo en
			// 0,35 rechazó dos sesiones auténticas por
			// `attack_identity_change`.
			//
			// No se arregla afinando el estadístico, y se intentó: en la
			// grabación de una de esas sesiones los frames a un lado y otro
			// del peor hueco se parecen 0,357 en una pose y 0,567 en una
			// ventana de QUIETUD. Una sustitución por otra cara real daría
			// valores del mismo orden — la variación legítima ya solapa con lo
			// que se quiere detectar. Este caso sintético usa vectores
			// ortogonales (similitud 0), que es más limpio que la realidad.
			//
			// Lo que defendía está declarado FUERA DE ALCANCE en §5: deepfakes
			// en tiempo real e inyección en el driver de cámara. Sigue pesando
			// 0,03 en la media, así que empuja hacia abajo; lo que no puede es
			// vetar.
			name: "cambio de cara a mitad de sesión: ya no se rechaza",
			mutate: func(tl *Timeline) {
				tl.Windows[0] = poseWindow(ptr(1), ptr(1), ptr(1), ptr(0.05))
			},
			wantOutcome: OutcomePass,
		},
		{
			name: "salto de trayectoria: corte de vídeo",
			mutate: func(tl *Timeline) {
				tl.Windows[0] = poseWindow(ptr(1), ptr(0.05), ptr(1), ptr(1))
			},
			wantOutcome: OutcomeReject,
			wantCodes:   []Code{CodeTemporalDiscontinuity},
			wantFamily:  FamilyTemporal,
		},
		{
			name: "respuesta imposiblemente rápida",
			mutate: func(tl *Timeline) {
				tl.Temporal = append(accepted(3), TemporalEvent{
					StepID: "s4", Kind: TemporalTooFast, ElapsedMS: 40,
				})
			},
			wantOutcome: OutcomeReject,
			wantCodes:   []Code{CodeTemporalTooFast},
			wantFamily:  FamilyTemporal,
		},
		{
			name: "luz ambiente aplastando el destello: NO es un ataque",
			mutate: func(tl *Timeline) {
				tl.Windows[1] = Window{
					ID: "flash-1", Kind: WindowFlash, QualitySufficient: false,
					QualityReason: "señal por debajo del ruido: luz ambiente",
					Submetrics: map[string]*float64{
						"correlation": nil, "gradient_3d": nil, "screen_absence": nil,
					},
				}
			},
			wantOutcome: OutcomeRetry,
			wantCodes:   []Code{CodeQualityInsufficientSignal},
			wantFamily:  FamilyQuality,
		},
		{
			name: "rostro fuera del encuadre",
			mutate: func(tl *Timeline) {
				tl.Capture.FramesWithFace = 40 // de 120
			},
			wantOutcome: OutcomeRetry,
			wantCodes:   []Code{CodeQualityNoFace},
			wantFamily:  FamilyQuality,
		},
		{
			name: "cámara borrosa",
			mutate: func(tl *Timeline) {
				tl.Capture.MeanSharpness = 4
			},
			wantOutcome: OutcomeRetry,
			wantCodes:   []Code{CodeQualityCapture},
			wantFamily:  FamilyQuality,
		},
		{
			name: "sesión sin terminar los retos",
			mutate: func(tl *Timeline) {
				tl.ChallengesCompleted = false
			},
			wantOutcome: OutcomeRetry,
			wantCodes:   []Code{CodeQualityIncomplete},
			wantFamily:  FamilyQuality,
		},
		{
			name: "señales insuficientes para decidir",
			mutate: func(tl *Timeline) {
				tl.Windows = []Window{poseWindow(ptr(0.9), nil, nil, nil)}
				tl.Temporal = nil
			},
			wantOutcome: OutcomeRetry,
			wantCodes:   []Code{CodeQualityNoSignals},
			wantFamily:  FamilyQuality,
		},
		{
			name: "el analizador se cayó",
			mutate: func(tl *Timeline) {
				tl.InfrastructureFailure = true
				tl.InfrastructureDetail = "lease perdido"
			},
			wantOutcome: OutcomeRetry,
			wantCodes:   []Code{CodeInfraAnalyzerUnavailable},
			wantFamily:  FamilyInfrastructure,
		},
		{
			name: "zona intermedia: sin evidencia para decidir",
			mutate: func(tl *Timeline) {
				tl.Windows[0] = poseWindow(ptr(0.6), ptr(0.6), ptr(0.6), ptr(0.6))
				tl.Windows[1] = flashWindow(ptr(0.6), ptr(0.6), ptr(0.6))
			},
			wantOutcome: OutcomeRetry,
			wantCodes:   []Code{CodeQualityInsufficientSignal},
			wantFamily:  FamilyQuality,
		},
		{
			name: "todo flojo pero nada por debajo de su suelo",
			mutate: func(tl *Timeline) {
				tl.Windows[0] = poseWindow(ptr(0.30), ptr(0.35), ptr(0.30), ptr(0.40))
				tl.Windows[1] = flashWindow(ptr(0.35), ptr(0.35), ptr(0.40))
				tl.Capture.MeanSharpness = 30 // captura mediocre pero aceptable
			},
			wantOutcome: OutcomeReject,
			wantCodes:   []Code{CodeAttackFusedScoreLow},
			wantFamily:  FamilyAttack,
		},
		{
			name: "un paso fuera de plazo, lo demás bien",
			mutate: func(tl *Timeline) {
				tl.Temporal = append(accepted(3), TemporalEvent{
					StepID: "s4", Kind: TemporalTimeout, ElapsedMS: 6000,
				})
			},
			wantOutcome: OutcomePass,
			wantCodes:   []Code{CodePassed},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			timeline := baseline()
			tc.mutate(&timeline)

			result := engine.Decide(timeline)

			if result.Outcome != tc.wantOutcome {
				t.Errorf("desenlace %s, se esperaba %s (score %.3f, motivos %v)",
					result.Outcome, tc.wantOutcome, result.Score, result.Reasons)
			}
			for _, code := range tc.wantCodes {
				if !result.HasCode(code) {
					t.Errorf("falta el motivo %q; llegaron %v", code, result.Reasons)
				}
			}
			if tc.wantFamily != "" && result.Primary().Family != tc.wantFamily {
				t.Errorf("familia del motivo principal %q, se esperaba %q",
					result.Primary().Family, tc.wantFamily)
			}

			// Todo veredicto llega firmado por el perfil que lo decidió.
			if result.ProfileVersion == "" || result.ProfileChecksum == "" {
				t.Error("el resultado no identifica el perfil que lo produjo")
			}
			if result.DecidedAt.IsZero() {
				t.Error("el resultado no dice cuándo se decidió")
			}
		})
	}
}

// TestTodoRechazoSeExplica: no se acusa a nadie sin decir con qué prueba.
func TestTodoRechazoSeExplica(t *testing.T) {
	engine := testEngine(t)

	rejections := []func(*Timeline){
		func(tl *Timeline) { tl.Windows[1] = flashWindow(ptr(1), ptr(0.1), ptr(1)) },
		func(tl *Timeline) { tl.Windows[1] = flashWindow(ptr(0.05), ptr(1), ptr(1)) },
		func(tl *Timeline) { tl.Windows[0] = poseWindow(ptr(1), ptr(1), ptr(0.05), ptr(1)) },
	}

	for i, mutate := range rejections {
		timeline := baseline()
		mutate(&timeline)
		result := engine.Decide(timeline)

		if result.Outcome != OutcomeReject {
			t.Fatalf("caso %d: se esperaba rechazo, salió %s", i, result.Outcome)
		}
		primary := result.Primary()
		if primary.Signal == "" {
			t.Errorf("caso %d: el rechazo no dice qué señal falló", i)
		}
		if primary.Threshold <= 0 {
			t.Errorf("caso %d: el rechazo no dice contra qué umbral se comparó", i)
		}
		if primary.Observed >= primary.Threshold {
			t.Errorf("caso %d: el valor observado (%.3f) no está por debajo del umbral (%.3f)",
				i, primary.Observed, primary.Threshold)
		}
		if len(result.Signals) == 0 {
			t.Errorf("caso %d: el resultado no guarda las señales que entraron", i)
		}
	}
}

// TestUnaSenalNoMedidaNoCuentaComoCero es la regla que separa "no se pudo
// medir" de "medido y malo".
func TestUnaSenalNoMedidaNoCuentaComoCero(t *testing.T) {
	engine := testEngine(t)

	timeline := baseline()
	// El paralaje no se pudo medir: el usuario no llegó a girar lo suficiente.
	timeline.Windows[0] = poseWindow(ptr(1), ptr(1), nil, ptr(1))

	result := engine.Decide(timeline)

	for _, signal := range result.Signals {
		if signal.Signal == SignalPoseParallax {
			t.Fatal("una señal no medida entró en la fusión")
		}
	}
	if result.HasCode(CodeAttackPlanarMotion) {
		t.Error("una señal no medida disparó una acusación")
	}
	if result.Outcome != OutcomePass {
		t.Errorf("desenlace %s: lo no medido no debería penalizar (score %.3f)",
			result.Outcome, result.Score)
	}
}

// TestSeQuedaConLaPeorVentana: si una ventana salió mal, otra buena no la
// desmiente.
func TestSeQuedaConLaPeorVentana(t *testing.T) {
	engine := testEngine(t)

	timeline := baseline()
	good := flashWindow(ptr(1), ptr(1), ptr(1))
	bad := flashWindow(ptr(1), ptr(0.10), ptr(1))
	bad.ID = "flash-2"
	timeline.Windows = []Window{timeline.Windows[0], good, bad}

	result := engine.Decide(timeline)

	if result.Outcome != OutcomeReject {
		t.Errorf("desenlace %s: una ventana buena no puede tapar una mala", result.Outcome)
	}
	if !result.HasCode(CodeAttackFlatSurface) {
		t.Errorf("motivos %v", result.Reasons)
	}
}

// TestLaCalidadNuncaAcusa: ningún camino de calidad puede acabar en rechazo.
func TestLaCalidadNuncaAcusa(t *testing.T) {
	engine := testEngine(t)

	qualityProblems := []func(*Timeline){
		func(tl *Timeline) { tl.Windows[1].QualitySufficient = false },
		func(tl *Timeline) { tl.Capture.FramesWithFace = 10 },
		func(tl *Timeline) { tl.Capture.MeanSharpness = 2 },
		func(tl *Timeline) { tl.Capture.MeanBrightness = 0.02 },
		func(tl *Timeline) { tl.Capture.MeanHighlightSaturation = 0.6 },
		func(tl *Timeline) { tl.ChallengesCompleted = false },
		func(tl *Timeline) { tl.InfrastructureFailure = true },
	}

	for i, mutate := range qualityProblems {
		timeline := baseline()
		mutate(&timeline)
		result := engine.Decide(timeline)

		if result.Outcome == OutcomeReject {
			t.Errorf("caso %d: un problema de calidad acabó en rechazo (%v)", i, result.Reasons)
		}
		if result.Primary().Family == FamilyAttack {
			t.Errorf("caso %d: un problema de calidad produjo una acusación (%v)", i, result.Primary())
		}
	}
}

// TestElVetoTemporalManda: un fallo temporal duro pesa más que un problema de
// calidad, porque se mide con el reloj del servidor y vale aunque la imagen no
// sirva.
func TestElVetoTemporalManda(t *testing.T) {
	engine := testEngine(t)

	timeline := baseline()
	timeline.Capture.MeanSharpness = 3 // calidad pésima
	timeline.Temporal = append(accepted(3), TemporalEvent{Kind: TemporalTooFast, ElapsedMS: 30})

	result := engine.Decide(timeline)

	if result.Outcome != OutcomeReject {
		t.Errorf("desenlace %s, se esperaba rechazo", result.Outcome)
	}
	if result.Primary().Code != CodeTemporalTooFast {
		t.Errorf("motivo principal %q", result.Primary().Code)
	}
}

// TestElPerfilDesplegadoSeparaLosTresCasosDelBanco ata el perfil a las cifras
// que mide el analizador de verdad.
func TestElPerfilDesplegadoSeparaLosTresCasosDelBanco(t *testing.T) {
	engine := testEngine(t)

	// Medidas en analyzer/tests/test_flash_acceptance.py.
	cases := []struct {
		name                                 string
		correlation, gradient, screenAbsence float64
		want                                 Outcome
	}{
		{"rostro real", 1.000, 1.000, 1.000, OutcomePass},
		{"foto impresa", 1.000, 0.115, 1.000, OutcomeReject},
		{"replay en pantalla", 0.000, 0.226, 0.187, OutcomeReject},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			timeline := baseline()
			timeline.Windows[1] = flashWindow(ptr(tc.correlation), ptr(tc.gradient), ptr(tc.screenAbsence))

			result := engine.Decide(timeline)
			t.Logf("score %.3f, motivos %v", result.Score, result.Reasons)

			if result.Outcome != tc.want {
				t.Errorf("desenlace %s, se esperaba %s", result.Outcome, tc.want)
			}
		})
	}
}

// TestElPerfilDesplegadoEsValido: si alguien despliega un perfil roto, se
// entera aquí y no en producción.
func TestElPerfilDesplegadoEsValido(t *testing.T) {
	profile, err := LoadFile(profilePath)
	if err != nil {
		t.Fatalf("el perfil desplegado no carga: %v", err)
	}
	if err := profile.Validate(); err != nil {
		t.Fatalf("el perfil desplegado no valida: %v", err)
	}

	// Toda señal de la taxonomía tiene peso: una señal que se mide y no entra
	// en la decisión es trabajo tirado.
	for _, signal := range AllSignals {
		if profile.Weight(signal) <= 0 {
			t.Errorf("la señal %q no participa en la decisión", signal)
		}
	}
}

func TestPerfilNuloNoCreaMotor(t *testing.T) {
	if _, err := New(nil, nil); err == nil {
		t.Error("se aceptó un perfil nulo")
	}
}

// writeProfile escribe un perfil temporal y devuelve su ruta.
func writeProfile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "profile.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("no se pudo escribir el perfil: %v", err)
	}
	return path
}

// Una captura quemada no puede acusar por textura.
//
// El resumen de sesión de los clasificadores pasivos es el PEOR frame, así que
// un reflejo cenital basta para arrastrar la sesión entera. Rechazar por eso
// sería acusar de fingir a quien tiene una lámpara encima: calidad
// insuficiente lleva a reintentar, nunca a rechazo.
func TestPADNoVotaConCapturaQuemada(t *testing.T) {
	profile, err := LoadFile(profilePath)
	if err != nil {
		t.Fatalf("no se pudo cargar el perfil desplegado: %v", err)
	}

	malo := 0.05
	windows := []Window{{
		ID:                "texture",
		Kind:              WindowTexture,
		QualitySufficient: true,
		Submetrics:        map[string]*float64{"v2": &malo, "v1se": &malo},
	}}

	limite := profile.Capture.MaxHighlightSaturation
	base := CaptureStats{Frames: 40, FramesWithFace: 40, MeanSharpness: 60, MeanBrightness: 0.5}

	bien := base
	bien.MeanHighlightSaturation = limite / 2
	quemada := base
	quemada.MeanHighlightSaturation = limite * 1.5

	engine := &Engine{}
	conSenal, _ := engine.collect(Timeline{Windows: windows, Capture: bien}, profile)
	sinSenal, _ := engine.collect(Timeline{Windows: windows, Capture: quemada}, profile)

	if !mentions(conSenal, SignalTexturePADV2) {
		t.Fatal("con exposición correcta el clasificador debería votar")
	}
	for _, señal := range []Signal{SignalTexturePADV2, SignalTexturePADV1SE} {
		if mentions(sinSenal, señal) {
			t.Errorf("%s votó sobre una captura quemada", señal)
		}
	}
}

func mentions(values []SignalValue, signal Signal) bool {
	for _, v := range values {
		if v.Signal == signal {
			return true
		}
	}
	return false
}

// Un pulso que no se pudo medir NO puede bloquear la sesión.
//
// Es la otra mitad de la regla de equidad. La del «sin suelo» impide que el
// pulso rechace; ésta impide que bloquee. Sin las dos, el pulso —que es
// estructuralmente no medible en piel muy oscura, porque la melanina está por
// encima del lecho capilar— dejaría a ese grupo reintentando en bucle. Un
// rechazo disfrazado de «inténtalo otra vez» sigue siendo un rechazo, y encima
// uno que no se puede recurrir porque nunca se declara.
//
// Salió de una sesión real: score 0,9154, doce de trece señales medidas, el
// destello correlacionando a 1,0, y resuelta en reintentar porque no se pudo
// medir el pulso.
func TestUnPulsoNoMedibleNoBloqueaLaSesion(t *testing.T) {
	engine := testEngineForFairness(t)

	bueno := 1.0
	base := Timeline{
		Windows: []Window{
			{
				ID: "pose", Kind: WindowPose, QualitySufficient: true,
				Submetrics: map[string]*float64{
					"compliance": &bueno, "continuity": &bueno,
					"parallax": &bueno, "identity": &bueno,
				},
			},
			{
				ID: "flash", Kind: WindowFlash, QualitySufficient: true,
				Submetrics: map[string]*float64{
					"correlation": &bueno, "gradient_3d": &bueno, "screen_absence": &bueno,
				},
			},
			{
				ID: "gaze", Kind: WindowGaze, QualitySufficient: true,
				Submetrics: map[string]*float64{"response": &bueno},
			},
		},
		Capture: CaptureStats{
			Frames: 400, FramesWithFace: 400, MeanSharpness: 80, MeanBrightness: 0.5,
		},
		ChallengesCompleted: true,
		Temporal: []TemporalEvent{
			{StepID: "pose", Kind: TemporalAccepted, ElapsedMS: 1200},
			{StepID: "flash", Kind: TemporalAccepted, ElapsedMS: 1100},
			{StepID: "gaze", Kind: TemporalAccepted, ElapsedMS: 900},
		},
	}

	// Con el pulso medido y bien: aprueba.
	conPulso := base
	conPulso.Windows = append(append([]Window(nil), base.Windows...), Window{
		ID: "rppg", Kind: WindowPulse, QualitySufficient: true,
		Submetrics: map[string]*float64{"snr": &bueno},
	})
	if got := engine.Decide(conPulso); got.Outcome != OutcomePass {
		t.Fatalf("con todo medido salió %s (score %.4f, motivos %v)",
			got.Outcome, got.Score, got.Reasons)
	}

	// Y con el pulso NO medible: tiene que seguir aprobando.
	sinPulso := base
	sinPulso.Windows = append(append([]Window(nil), base.Windows...), Window{
		ID: "rppg", Kind: WindowPulse, QualitySufficient: false,
		QualityReason: "señal insuficiente",
	})
	got := engine.Decide(sinPulso)
	if got.Outcome != OutcomePass {
		t.Errorf("un pulso no medible bloqueó la sesión: %s (score %.4f, motivos %v)",
			got.Outcome, got.Score, got.Reasons)
	}
}

func testEngineForFairness(t *testing.T) *Engine {
	t.Helper()
	profile, err := LoadFile(profilePath)
	if err != nil {
		t.Fatalf("no se pudo cargar el perfil: %v", err)
	}
	engine, err := New(profile, clock.NewFake(time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("no se pudo crear el motor: %v", err)
	}
	return engine
}
