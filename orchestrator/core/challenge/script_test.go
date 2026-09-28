package challenge

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// walk recorre un guion de avance único y devuelve todos sus pasos.
func walk(t *testing.T, s *Script) []Step {
	t.Helper()
	var steps []Step
	for {
		step, ok := s.Current()
		if !ok {
			break
		}
		steps = append(steps, step)
		if !s.Advance() {
			break
		}
	}
	return steps
}

func mustGenerate(t *testing.T, seed Seed, p Policy) *Script {
	t.Helper()
	s, err := Generate(seed, p)
	if err != nil {
		t.Fatalf("Generate(%d): %v", seed, err)
	}
	return s
}

// TestSameSeedSameScript es el contrato con /bench: reproducibilidad exacta.
func TestSameSeedSameScript(t *testing.T) {
	p := DefaultPolicy()
	for seed := Seed(0); seed < 200; seed++ {
		a := walk(t, mustGenerate(t, seed, p))
		b := walk(t, mustGenerate(t, seed, p))
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("semilla %d produjo dos guiones distintos:\n%+v\n%+v", seed, a, b)
		}
	}
}

// TestDifferentSeedsDifferentScripts: el guion tiene que variar de verdad.
// No se exige que TODAS las parejas difieran (el espacio de guiones es
// finito), sino que la inmensa mayoría lo haga.
func TestDifferentSeedsDifferentScripts(t *testing.T) {
	p := DefaultPolicy()
	base := walk(t, mustGenerate(t, 1, p))
	equal := 0
	const n = 200
	for seed := Seed(2); seed < 2+n; seed++ {
		if reflect.DeepEqual(base, walk(t, mustGenerate(t, seed, p))) {
			equal++
		}
	}
	if equal > 2 {
		t.Errorf("%d de %d semillas distintas produjeron el mismo guion", equal, n)
	}
}

// TestScriptShape comprueba la forma del guion sobre muchas semillas.
func TestScriptShape(t *testing.T) {
	p := DefaultPolicy()
	for seed := Seed(0); seed < 500; seed++ {
		steps := walk(t, mustGenerate(t, seed, p))

		if n := retos(steps); n < p.MinSteps || n > p.MaxSteps {
			t.Fatalf("semilla %d: %d retos, se esperaban entre %d y %d (%v)",
				seed, n, p.MinSteps, p.MaxSteps, kinds(steps))
		}

		// Exactamente un tramo de quietud, y siempre antes de los destellos:
		// medir el pulso dentro de un destello es imposible, porque el
		// destello mueve el color de la piel órdenes de magnitud más que un
		// latido.
		holds, vistoDestello := 0, false
		for _, st := range steps {
			switch st.Kind {
			case KindFlash:
				vistoDestello = true
			case KindHold:
				holds++
				if vistoDestello {
					t.Fatalf("semilla %d: tramo de quietud después de un destello (%v)",
						seed, kinds(steps))
				}
				if st.Hold != p.PulseHold {
					t.Errorf("semilla %d: tramo de %v, se esperaba %v", seed, st.Hold, p.PulseHold)
				}
			}
		}
		if holds != 1 {
			t.Fatalf("semilla %d: %d tramos de quietud, se esperaba 1", seed, holds)
		}

		// El primer paso es siempre la calibración: 1.5 s de pantalla neutra.
		calib := steps[0]
		if calib.Kind != KindCalibration {
			t.Fatalf("semilla %d: el primer paso es %s, se esperaba calibración", seed, calib.Kind)
		}
		if calib.Hold != p.CalibrationHold {
			t.Errorf("semilla %d: calibración de %v, se esperaba %v", seed, calib.Hold, p.CalibrationHold)
		}
		if calib.Window.Min != p.CalibrationHold || calib.Window.Max != p.CalibrationHold+p.CalibrationSlack {
			t.Errorf("semilla %d: ventana de calibración %+v", seed, calib.Window)
		}

		flashes := 0
		seenPoses := map[PoseAction]bool{}
		ids := map[string]bool{}
		for i, st := range steps {
			if ids[st.ID] {
				t.Errorf("semilla %d: identificador repetido %q", seed, st.ID)
			}
			ids[st.ID] = true

			if i > 0 && st.Kind == KindCalibration {
				t.Errorf("semilla %d: calibración repetida en la posición %d", seed, i)
			}

			switch st.Kind {
			case KindPose:
				if st.Pose == PoseUnspecified {
					t.Errorf("semilla %d: pose sin acción", seed)
				}
				if seenPoses[st.Pose] {
					t.Errorf("semilla %d: pose repetida %s", seed, st.Pose)
				}
				seenPoses[st.Pose] = true
				// El "acércate" de antes del destello no lleva mínimo de
				// reacción: no tiene parámetro sorteado que adivinar, así que
				// responder deprisa no delata una respuesta pregrabada.
				wantMin := p.MinReaction
				if st.Pose == PoseMoveCloser {
					wantMin = 0
				}
				if st.Window.Min != wantMin || st.Window.Max != p.PoseDeadline {
					t.Errorf("semilla %d: ventana de pose %+v", seed, st.Window)
				}
				if len(st.Flash) != 0 {
					t.Errorf("semilla %d: una pose no lleva secuencia de destello", seed)
				}
			case KindFlash:
				flashes++
				checkFlash(t, seed, st, p)
			}
		}

		if flashes == 0 {
			t.Fatalf("semilla %d: ningún reto de destello; siempre debe haber al menos uno", seed)
		}
	}
}

func checkFlash(t *testing.T, seed Seed, st Step, p Policy) {
	t.Helper()

	if len(st.Flash) < p.MinFlashColors || len(st.Flash) > p.MaxFlashColors {
		t.Errorf("semilla %d: %d colores, se esperaban entre %d y %d",
			seed, len(st.Flash), p.MinFlashColors, p.MaxFlashColors)
	}
	if st.Flash[0].Color == FlashWhite {
		t.Errorf("semilla %d: la secuencia empieza por blanco, que ya no se emite", seed)
	}

	var total time.Duration
	for i, seg := range st.Flash {
		if seg.Duration < p.MinFlashSegment || seg.Duration > p.MaxFlashSegment {
			t.Errorf("semilla %d: color %d dura %v, fuera de [%v,%v]",
				seed, i, seg.Duration, p.MinFlashSegment, p.MaxFlashSegment)
		}
		if seg.Color == FlashUnspecified {
			t.Errorf("semilla %d: color sin especificar en la posición %d", seed, i)
		}
		if i > 0 && seg.Color == st.Flash[i-1].Color {
			t.Errorf("semilla %d: color repetido consecutivo %s en %d", seed, seg.Color, i)
		}
		total += seg.Duration
	}

	if st.FlashDuration() != total {
		t.Errorf("semilla %d: FlashDuration = %v, se esperaba %v", seed, st.FlashDuration(), total)
	}
	if st.Hold != total {
		t.Errorf("semilla %d: Hold = %v, se esperaba %v", seed, st.Hold, total)
	}
	if st.Window.Min != total || st.Window.Max != total+p.FlashSlack {
		t.Errorf("semilla %d: ventana de destello %+v con secuencia de %v", seed, st.Window, total)
	}
}

// TestPaletteCoverage: con suficientes semillas deben aparecer los cuatro
// colores y las cuatro poses. Si el generador se atasca en un subconjunto,
// el guion es más predecible de lo que dice ser.
func TestPaletteCoverage(t *testing.T) {
	p := DefaultPolicy()
	colors := map[FlashColor]bool{}
	poses := map[PoseAction]bool{}
	lengths := map[int]bool{}

	for seed := Seed(0); seed < 500; seed++ {
		steps := walk(t, mustGenerate(t, seed, p))
		lengths[retos(steps)] = true
		for _, st := range steps {
			if st.Kind == KindPose {
				poses[st.Pose] = true
			}
			for _, seg := range st.Flash {
				colors[seg.Color] = true
			}
		}
	}

	// Sólo se emiten los primarios saturados: el blanco existe como estado
	// detectable pero ya no se pinta nunca (ver chromaticPalette).
	for _, c := range chromaticPalette {
		if !colors[c] {
			t.Errorf("el color %s no salió nunca en 500 semillas", c)
		}
	}
	if colors[FlashWhite] {
		t.Error("salió un blanco: ya no se emite en ninguna secuencia")
	}
	for _, a := range poseCatalog {
		if !poses[a] {
			t.Errorf("la pose %s no salió nunca en 500 semillas", a)
		}
	}
	for n := p.MinSteps; n <= p.MaxSteps; n++ {
		if !lengths[n] {
			t.Errorf("ningún guion tuvo %d retos en 500 semillas", n)
		}
	}
}

// TestFlashesGoLastInOneBlock: los destellos van juntos y al final.
//
// No es cosmética: es lo que deja un tramo largo, seguido y con la
// iluminación estable donde medir el pulso sanguíneo. Con los destellos
// repartidos, la sesión queda troceada y el pulso —la única señal que una
// máscara no puede fingir— no se puede medir nunca.
func TestFlashesGoLastInOneBlock(t *testing.T) {
	p := DefaultPolicy()
	for seed := Seed(0); seed < 300; seed++ {
		steps := walk(t, mustGenerate(t, seed, p))
		// steps[0] es la calibración; los retos empiezan en 1.
		vistoDestello := false
		for _, step := range steps[1:] {
			if step.Kind == KindFlash {
				vistoDestello = true
				continue
			}
			if vistoDestello {
				t.Fatalf("semilla %d: un %s después de un destello", seed, step.Kind)
			}
		}
	}
}

// TestScriptStaysUnpredictable: agrupar los destellos cede el ORDEN de los
// tipos, y nada más.
//
// Es la contrapartida de TestFlashesGoLastInOneBlock y existe para acotarla:
// lo que rompe un vídeo grabado no es ignorar en qué orden vienen los tipos
// de reto, sino no poder responder a unos parámetros elegidos ahora. Este
// test vigila que esos parámetros sigan variando.
func TestScriptStaysUnpredictable(t *testing.T) {
	p := DefaultPolicy()

	primeros := map[Kind]int{}
	destellos := map[int]bool{}
	colores := map[string]bool{}
	lados := map[GazeTarget]bool{}
	poses := map[PoseAction]bool{}

	for seed := Seed(0); seed < 300; seed++ {
		steps := walk(t, mustGenerate(t, seed, p))
		primeros[steps[1].Kind]++

		n := 0
		for _, step := range steps[1:] {
			switch step.Kind {
			case KindFlash:
				n++
				for _, seg := range step.Flash {
					colores[seg.Color.String()] = true
				}
			case KindGaze:
				lados[step.Gaze] = true
			case KindPose:
				poses[step.Pose] = true
			}
		}
		destellos[n] = true
	}

	if len(primeros) < 2 {
		t.Error("el primer reto es siempre del mismo tipo")
	}
	if len(destellos) < 2 {
		t.Error("el número de destellos no varía: el bloque final es predecible")
	}
	if len(colores) < 2 {
		t.Error("los colores del destello no varían")
	}
	if len(lados) < 2 {
		t.Error("el lado de la mirada no varía")
	}
	if len(poses) < 2 {
		t.Error("la pose pedida no varía")
	}
}

// TestForwardOnly: el guion es de avance único. No hay forma de leer el
// futuro, y una vez agotado se queda agotado.
func TestForwardOnly(t *testing.T) {
	s := mustGenerate(t, 7, DefaultPolicy())

	first, ok := s.Current()
	if !ok {
		t.Fatal("Current() vacío en un guion recién generado")
	}
	again, _ := s.Current()
	if first.ID != again.ID {
		t.Error("Current() no es idempotente")
	}
	if s.Position() != 0 {
		t.Errorf("Position() = %d al principio", s.Position())
	}

	steps := 1
	for s.Advance() {
		steps++
		if s.Position() != steps-1 {
			t.Errorf("Position() = %d tras %d avances", s.Position(), steps-1)
		}
	}

	// Agotado: Advance sigue devolviendo false y Current ya no da nada.
	for range 3 {
		if s.Advance() {
			t.Error("Advance() = true sobre un guion agotado")
		}
	}
	if _, ok := s.Current(); ok {
		t.Error("Current() devuelve un paso sobre un guion agotado")
	}
}

// TestScriptStringDoesNotLeak: ni siquiera el %v del guion puede chivar su
// contenido; es fácil que acabe en un log.
func TestScriptStringDoesNotLeak(t *testing.T) {
	s := mustGenerate(t, 99, DefaultPolicy())
	step, _ := s.Current()
	got := s.String()
	if got != "Script{posición:0}" {
		t.Errorf("String() = %q", got)
	}
	if len(step.ID) > 0 && strings.Contains(got, step.ID) {
		t.Errorf("String() filtra el identificador del paso: %q", got)
	}
}

// TestStepIDIsOpaque: el identificador no puede delatar la posición.
func TestStepIDIsOpaque(t *testing.T) {
	steps := walk(t, mustGenerate(t, 12345, DefaultPolicy()))
	for i, st := range steps {
		if len(st.ID) != 16 {
			t.Errorf("paso %d: identificador de longitud %d", i, len(st.ID))
		}
		// El identificador no puede contener la posición ni parecerse a ella.
		if st.ID == strconv.Itoa(i) || strings.HasPrefix(st.ID, strconv.Itoa(i)+"-") {
			t.Errorf("paso %d: el identificador %q delata la posición", i, st.ID)
		}
	}
	// Semillas distintas dan identificadores distintos en la misma posición.
	a := walk(t, mustGenerate(t, 1, DefaultPolicy()))
	b := walk(t, mustGenerate(t, 2, DefaultPolicy()))
	if a[0].ID == b[0].ID {
		t.Error("el identificador del primer paso no depende de la semilla")
	}
}

func TestGenerateRejectsInvalidPolicy(t *testing.T) {
	p := DefaultPolicy()
	p.MaxSteps = 1 // < MinSteps
	if _, err := Generate(1, p); !errors.Is(err, ErrInvalidPolicy) {
		t.Errorf("se esperaba ErrInvalidPolicy, se obtuvo %v", err)
	}
}

// TestSingleStepPolicy: con MinSteps=MaxSteps=1 el único reto debe ser el
// destello garantizado.
func TestSingleStepPolicy(t *testing.T) {
	p := DefaultPolicy()
	p.MinSteps, p.MaxSteps = 1, 1
	for seed := Seed(0); seed < 50; seed++ {
		steps := walk(t, mustGenerate(t, seed, p))
		// Calibración + tramo de quietud + "acércate" + el reto. Sólo el
		// último es un reto: los otros tres son ventanas de medida y
		// encuadre, y por eso no cuentan para MinSteps ni MaxSteps.
		if len(steps) != 4 {
			t.Fatalf("semilla %d: %d pasos", seed, len(steps))
		}
		if retos(steps) != 1 || steps[len(steps)-1].Kind != KindFlash {
			t.Fatalf("semilla %d: el único reto debía ser destello, salió %v", seed, kinds(steps))
		}
	}
}

// TestManyStepsExhaustPoses: si se piden más retos que poses disponibles, el
// generador rellena con destellos en vez de repetir pose o quedarse corto.
func TestManyStepsExhaustPoses(t *testing.T) {
	p := DefaultPolicy()
	p.MinSteps, p.MaxSteps = 6, 6
	for seed := Seed(0); seed < 50; seed++ {
		steps := walk(t, mustGenerate(t, seed, p))
		if retos(steps) != 6 {
			t.Fatalf("semilla %d: %d retos, se esperaban 6 (%v)", seed, retos(steps), kinds(steps))
		}
		seen := map[PoseAction]bool{}
		for _, st := range steps {
			if st.Kind != KindPose {
				continue
			}
			if seen[st.Pose] {
				t.Fatalf("semilla %d: pose repetida %s", seed, st.Pose)
			}
			seen[st.Pose] = true
		}
	}
}

func TestWindowContains(t *testing.T) {
	w := Window{Min: 350 * time.Millisecond, Max: time.Second}
	cases := []struct {
		elapsed time.Duration
		want    bool
	}{
		{0, false},
		{349 * time.Millisecond, false},
		{350 * time.Millisecond, true}, // extremos incluidos
		{700 * time.Millisecond, true},
		{time.Second, true},
		{time.Second + time.Millisecond, false},
	}
	for _, c := range cases {
		if got := w.Contains(c.elapsed); got != c.want {
			t.Errorf("Contains(%v) = %v, se esperaba %v", c.elapsed, got, c.want)
		}
	}
}

func TestEnumStrings(t *testing.T) {
	kinds := map[Kind]string{
		KindUnspecified: "unspecified",
		KindCalibration: "calibration",
		KindPose:        "pose",
		KindFlash:       "flash",
		Kind(9):         "kind(9)",
	}
	for k, want := range kinds {
		if got := k.String(); got != want {
			t.Errorf("Kind(%d) = %q, se esperaba %q", uint8(k), got, want)
		}
	}
	poses := map[PoseAction]string{
		PoseUnspecified: "unspecified",
		PoseYawLeft:     "yaw_left",
		PoseYawRight:    "yaw_right",
		PosePitchUp:     "pitch_up",
		PoseMoveCloser:  "move_closer",
		PoseAction(9):   "pose(9)",
	}
	for p, want := range poses {
		if got := p.String(); got != want {
			t.Errorf("PoseAction(%d) = %q, se esperaba %q", uint8(p), got, want)
		}
	}
	colors := map[FlashColor]string{
		FlashUnspecified: "unspecified",
		FlashWhite:       "white",
		FlashRed:         "red",
		FlashGreen:       "green",
		FlashBlue:        "blue",
		FlashColor(9):    "color(9)",
	}
	for c, want := range colors {
		if got := c.String(); got != want {
			t.Errorf("FlashColor(%d) = %q, se esperaba %q", uint8(c), got, want)
		}
	}
}

func TestFlashDurationOnNonFlashStep(t *testing.T) {
	if got := (Step{Kind: KindPose}).FlashDuration(); got != 0 {
		t.Errorf("FlashDuration() = %v en una pose", got)
	}
}

// Excluir tipos de reto es de desarrollo: sirve para depurar el resto del
// recorrido mientras un análisis está a medio calibrar, o para que el maniquí
// del banco pueda jugar lo que sabe hacer. Lo que no puede es colarse en un
// guion normal.
func TestGuionSinDestello(t *testing.T) {
	p := DefaultPolicy()
	p.InsecureSkipKinds = []Kind{KindFlash}

	for seed := Seed(1); seed <= 50; seed++ {
		script, err := Generate(seed, p)
		if err != nil {
			t.Fatalf("semilla %d: %v", seed, err)
		}
		steps := walk(t, script)
		if len(steps) < 3 {
			t.Fatalf("semilla %d: sólo %d pasos con calibración", seed, len(steps))
		}
		for _, step := range steps {
			if step.Kind == KindFlash {
				t.Fatalf("semilla %d: apareció un destello con InsecureNoFlash", seed)
			}
		}
	}
}

// Y por defecto el destello sigue siendo obligatorio: es lo único que un
// vídeo grabado no puede responder.
func TestPorDefectoSiempreHayDestello(t *testing.T) {
	for seed := Seed(1); seed <= 50; seed++ {
		script, err := Generate(seed, DefaultPolicy())
		if err != nil {
			t.Fatalf("semilla %d: %v", seed, err)
		}
		destellos := 0
		for _, step := range walk(t, script) {
			if step.Kind == KindFlash {
				destellos++
			}
		}
		if destellos == 0 {
			t.Fatalf("semilla %d: guion sin ningún destello", seed)
		}
	}
}

// El guion tiene que ser largo: cada paso es una ventana temporal más que un
// vídeo grabado tiene que acertar, y un guion corto se supera por casualidad
// con más probabilidad de la que nadie querría.
func TestGuionTieneAlMenosCincoRetos(t *testing.T) {
	for seed := Seed(1); seed <= 200; seed++ {
		script, err := Generate(seed, DefaultPolicy())
		if err != nil {
			t.Fatalf("semilla %d: %v", seed, err)
		}
		steps := walk(t, script)
		// El primero es la calibración, que no cuenta como reto.
		if retos := len(steps) - 1; retos < 5 {
			t.Fatalf("semilla %d: %d retos, se esperaban al menos 5", seed, retos)
		}
	}
}

// Y los dos techos se respetan: las poses no se repiten —repetir es regalar
// una segunda oportunidad al mismo movimiento— y los destellos tienen su
// propio máximo.
func TestGuionRespetaLosTechos(t *testing.T) {
	for seed := Seed(1); seed <= 200; seed++ {
		script, err := Generate(seed, DefaultPolicy())
		if err != nil {
			t.Fatalf("semilla %d: %v", seed, err)
		}

		vistas := map[PoseAction]bool{}
		destellos := 0
		for _, step := range walk(t, script) {
			switch step.Kind {
			case KindPose:
				if vistas[step.Pose] {
					t.Fatalf("semilla %d: pose %s repetida", seed, step.Pose)
				}
				vistas[step.Pose] = true
			case KindFlash:
				destellos++
			}
		}
		if destellos > maxFlashSteps {
			t.Fatalf("semilla %d: %d destellos, el techo es %d", seed, destellos, maxFlashSteps)
		}
	}
}

// El guion tiene que caber en el presupuesto de sesión del gateway. Un guion
// que no cabe se resuelve siempre por expiración, y el usuario no entiende
// por qué.
func TestGuionCabeEnElPresupuesto(t *testing.T) {
	const presupuesto = 60 * time.Second

	peor := time.Duration(0)
	for seed := Seed(1); seed <= 200; seed++ {
		script, err := Generate(seed, DefaultPolicy())
		if err != nil {
			t.Fatalf("semilla %d: %v", seed, err)
		}
		var total time.Duration
		for _, step := range walk(t, script) {
			total += step.Window.Max
		}
		peor = max(peor, total)
	}

	t.Logf("peor guion: %v de %v de presupuesto", peor.Round(time.Millisecond), presupuesto)
	if peor > presupuesto {
		t.Fatalf("el peor guion dura %v y el presupuesto es %v", peor, presupuesto)
	}
	// Margen para la reacción humana y la red: agotar el presupuesto en los
	// plazos máximos deja la sesión sin sitio para nada más.
	if peor > presupuesto*3/4 {
		t.Fatalf("el peor guion (%v) se come más de tres cuartos del presupuesto", peor)
	}
}

// Se pueden excluir varios tipos a la vez, y el guion sigue siendo válido
// mientras quede catálogo suficiente.
func TestGuionSinDestelloNiMirada(t *testing.T) {
	p := DefaultPolicy()
	p.InsecureSkipKinds = []Kind{KindFlash, KindGaze}
	p.MaxSteps = len(poseCatalog)
	p.MinSteps = 2

	for seed := Seed(1); seed <= 50; seed++ {
		script, err := Generate(seed, p)
		if err != nil {
			t.Fatalf("semilla %d: %v", seed, err)
		}
		for _, step := range walk(t, script) {
			if step.Kind == KindFlash || step.Kind == KindGaze {
				t.Fatalf("semilla %d: apareció %s pese a estar excluido", seed, step.Kind)
			}
		}
	}
}

// Una política que excluye tanto que no puede llegar a MinSteps es un error
// explícito, no un guion corto silencioso.
func TestPoliticaQueExcluyeDemasiadoNoValida(t *testing.T) {
	p := DefaultPolicy()
	p.InsecureSkipKinds = []Kind{KindFlash, KindGaze}
	if err := p.Validate(); err == nil {
		t.Fatal("se aceptó una política con MaxSteps 6 y sólo 4 poses disponibles")
	}
}

// La calibración no es un reto y no se puede quitar: es la línea base contra
// la que se mide todo lo demás.
func TestNoSePuedeOmitirLaCalibracion(t *testing.T) {
	if _, err := ParseKinds([]string{"calibration"}); err == nil {
		t.Fatal("se aceptó omitir la calibración")
	}
	if _, err := ParseKinds([]string{"destello"}); err == nil {
		t.Fatal("se aceptó un nombre de reto desconocido")
	}
	kinds, err := ParseKinds([]string{"flash", " gaze "})
	if err != nil {
		t.Fatalf("nombres válidos rechazados: %v", err)
	}
	if len(kinds) != 2 || kinds[0] != KindFlash || kinds[1] != KindGaze {
		t.Fatalf("ParseKinds = %v", kinds)
	}
}

// Dos objetivos de mirada seguidos nunca pueden pedir la misma esquina: si el
// objetivo no se mueve, el sujeto ya está mirando ahí y no hay nada que medir.
// Medido con cámara real: dos iguales seguidos dieron 0,000 de desplazamiento.
func TestMiradasSeguidasNoRepitenEsquina(t *testing.T) {
	p := DefaultPolicy()
	for seed := Seed(1); seed <= 300; seed++ {
		script, err := Generate(seed, p)
		if err != nil {
			t.Fatalf("semilla %d: %v", seed, err)
		}
		var prev *GazeTarget
		for {
			step, ok := script.Current()
			if !ok {
				break
			}
			if step.Kind == KindGaze {
				if prev != nil && prev.X == step.Gaze.X && prev.Y == step.Gaze.Y {
					t.Fatalf("semilla %d: dos miradas seguidas a (%.2f,%.2f)", seed, step.Gaze.X, step.Gaze.Y)
				}
				target := step.Gaze
				prev = &target
			}
			if !script.Advance() {
				break
			}
		}
	}
}

// Una mirada nunca va justo después de una pose: la pose deja la cabeza
// girada, y la referencia de mirada se toma de los frames anteriores al
// objetivo. Con la cabeza a medio volver, esa referencia describe una postura
// que ya no existe. Medido con una cámara real: cabeza aún a 20 grados del
// reto anterior y el reto de mirada irresoluble.
func TestMiradaNuncaVaJustoDespuesDeUnaPose(t *testing.T) {
	for seed := Seed(1); seed <= 300; seed++ {
		script, err := Generate(seed, DefaultPolicy())
		if err != nil {
			t.Fatalf("semilla %d: %v", seed, err)
		}
		steps := walk(t, script)
		for i := 1; i < len(steps); i++ {
			if steps[i].Kind == KindGaze && steps[i-1].Kind == KindPose {
				t.Fatalf("semilla %d: mirada justo después de una pose", seed)
			}
		}
	}
}

// El blanco no se emite en ninguna secuencia.
//
// No lleva información cromática —enciende los tres canales, el estado menos
// informativo que puede devolver un rostro— y su referencia ya la da la fase
// de calibración. Y medido con una cámara real: es el color que más adapta la
// exposición de la cámara, y esa adaptación se come los tramos siguientes,
// que son los que sí discriminan.
func TestLasSecuenciasNoLlevanBlanco(t *testing.T) {
	for seed := Seed(1); seed <= 300; seed++ {
		script, err := Generate(seed, DefaultPolicy())
		if err != nil {
			t.Fatalf("semilla %d: %v", seed, err)
		}
		for _, step := range walk(t, script) {
			if step.Kind != KindFlash {
				continue
			}
			for i, seg := range step.Flash {
				if seg.Color == FlashWhite {
					t.Fatalf("semilla %d: blanco en el tramo %d", seed, i+1)
				}
			}
		}
	}
}

// retos cuenta los pasos que de verdad piden algo al sujeto.
//
// La calibración, el tramo de quietud y el "acércate" de antes del destello no
// son retos: son ventanas de medida y encuadre. No hay nada que responder en
// ellas, así que no cuentan para MinSteps ni MaxSteps y no le quitan sitio a
// ningún reto real.
//
// El "acércate" se reconoce por su acción y no por su tipo, y es fiable porque
// PoseMoveCloser ya no se sortea: salió del catálogo justo por esto, así que
// una pose de acercarse SÓLO puede ser la que se coloca ante los destellos.
func retos(steps []Step) int {
	n := 0
	for _, s := range steps {
		switch {
		case s.Kind == KindCalibration, s.Kind == KindHold:
		case s.Kind == KindPose && s.Pose == PoseMoveCloser:
		default:
			n++
		}
	}
	return n
}

func kinds(steps []Step) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.Kind.String())
	}
	return out
}

// TestSiempreHayMiradaYDestello: las dos defensas de reto-respuesta atadas a
// ESTE instante van garantizadas, no sorteadas.
//
// Son las únicas que un vídeo grabado no puede superar: no puede responder a
// una secuencia de colores emitida ahora ni mirar al punto que acaba de
// aparecer. Sorteando la mirada, el 8,4 % de los guiones salía sin ninguna, y
// esas sesiones decidían sin una señal que pesa 0,11 y que tiene suelo propio.
// Dejar al azar si una defensa participa es no tenerla una de cada doce veces.
func TestSiempreHayMiradaYDestello(t *testing.T) {
	p := DefaultPolicy()
	for seed := Seed(0); seed < 1000; seed++ {
		steps := walk(t, mustGenerate(t, seed, p))
		miradas, destellos := 0, 0
		for _, st := range steps {
			switch st.Kind {
			case KindGaze:
				miradas++
			case KindFlash:
				destellos++
			}
		}
		if miradas == 0 {
			t.Fatalf("semilla %d: guion sin ninguna mirada (%v)", seed, kinds(steps))
		}
		if destellos == 0 {
			t.Fatalf("semilla %d: guion sin ningún destello (%v)", seed, kinds(steps))
		}
	}
}

// TestTodoGuionPermiteMedirTodo: cada señal de la fusión tiene su
// prerrequisito en TODOS los guiones.
//
// El guion es aleatorio, pero de ahí no se sigue que las defensas lo sean.
// Que una señal participe o no según la semilla equivale a no tenerla una
// parte de las veces, y encima sin que nadie se entere: el veredicto sale
// igual de convincente con once señales que con trece.
//
// Antes de este test, medido sobre 5000 semillas:
//
//   - al 8,4 % de los guiones les faltaba la mirada;
//   - al 22 % les faltaba una pose ANGULAR, y con ella el paralaje, que es de
//     las dos únicas señales que separan un rostro de una superficie plana.
//     Acercarse no vale: no es un ángulo y no produce ventana de pose.
//
// Si se añade una señal nueva a la fusión, su prerrequisito va aquí.
func TestTodoGuionPermiteMedirTodo(t *testing.T) {
	requisitos := []struct {
		señales string
		cumple  func([]Step) bool
	}{
		{"pose_compliance, pose_continuity, pose_parallax, pose_identity", func(ss []Step) bool {
			for _, s := range ss {
				if s.Kind == KindPose && s.Pose != PoseMoveCloser {
					return true
				}
			}
			return false
		}},
		{"flash_correlation, flash_gradient_3d, flash_screen_absence", func(ss []Step) bool {
			return contiene(ss, KindFlash)
		}},
		{"gaze_response", func(ss []Step) bool { return contiene(ss, KindGaze) }},
		{"rppg_snr", func(ss []Step) bool { return contiene(ss, KindHold) }},
	}

	p := DefaultPolicy()
	for seed := Seed(0); seed < 2000; seed++ {
		steps := walk(t, mustGenerate(t, seed, p))
		for _, r := range requisitos {
			if !r.cumple(steps) {
				t.Fatalf("semilla %d: el guion no permite medir %s (%v)", seed, r.señales, kinds(steps))
			}
		}
	}
}

func contiene(steps []Step, k Kind) bool {
	for _, s := range steps {
		if s.Kind == k {
			return true
		}
	}
	return false
}

// Las dos duraciones de una secuencia de destello tienen que diferir.
//
// Si son iguales, la secuencia es simétrica en el tiempo y la búsqueda de
// retardo del analizador puede desplazarla medio periodo y encajarla sobre su
// CONTRARIA. Entonces la correlación deja de significar «respondió a ESTOS
// colores» y pasa a significar «hubo un cambio de brillo con esta forma».
//
// El número está medido sobre 27 ventanas reales, comparando la secuencia
// emitida contra su inversa: con 27 y 29 ms de diferencia la inversa subía a
// 0,85 y 0,72; desde 34 ms no pasa de 0,35 mientras la real se queda en 0,94
// o más. La política pide 100, factor tres sobre ese punto.
//
// La otra salida —alargar los tramos por encima del rango de búsqueda— se
// probó y salió MAL: con tramos de 800-1400 ms la correlación de caras reales
// se hundió de 0,92-0,99 a 0,049 y 0,014, porque a esa duración la cámara ya
// ha compensado el color con su balance de blancos y lo que se mide es el
// lazo de la cámara, no la piel. Está anotado en MinFlashSegment.
func TestLasDuracionesDeUnDestelloSeparanBastante(t *testing.T) {
	p := DefaultPolicy()
	for seed := Seed(1); seed <= 2000; seed++ {
		script, err := Generate(seed, p)
		if err != nil {
			t.Fatalf("semilla %d: %v", seed, err)
		}
		for _, step := range walk(t, script) {
			if step.Kind != KindFlash {
				continue
			}
			for i, seg := range step.Flash {
				if seg.Duration < p.MinFlashSegment || seg.Duration > p.MaxFlashSegment {
					t.Fatalf("semilla %d: tramo %d dura %v, fuera de [%v, %v]",
						seed, i, seg.Duration, p.MinFlashSegment, p.MaxFlashSegment)
				}
				for j := range i {
					diff := seg.Duration - step.Flash[j].Duration
					if diff < 0 {
						diff = -diff
					}
					if diff < p.FlashDurationSpread {
						t.Fatalf("semilla %d: los tramos %d y %d difieren %v, menos que %v",
							seed, j, i, diff, p.FlashDurationSpread)
					}
				}
			}
		}
	}
}
