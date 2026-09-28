package analyzer

import (
	"context"
	"encoding/json"
	"math"
	"time"
)

// Scene es lo que un frame sintético representa: la escena que la cámara
// estaría viendo. El banco de pruebas y el cliente de prueba la construyen;
// el stub la "analiza" como si fueran píxeles.
//
// Existe porque un analizador simulado NO puede saber qué reto está activo:
// eso rompería la frontera. Lo que sabe es lo que hay delante de la cámara, y
// eso viaja en el propio frame, igual que en la realidad.
type Scene struct {
	// Face indica si hay un rostro en el encuadre.
	Face bool `json:"face"`
	// YawDeg y PitchDeg son la orientación de la cabeza en grados.
	// Negativo = izquierda / abajo.
	YawDeg   float64 `json:"yaw_deg"`
	PitchDeg float64 `json:"pitch_deg"`
	// FaceAreaRatio es la fracción del encuadre ocupada por el rostro.
	FaceAreaRatio float64 `json:"face_area_ratio"`
	// ScreenColor es el color que la pantalla del cliente está emitiendo en
	// este instante: white, red, green, blue o "" si no emite.
	ScreenColor string `json:"screen_color,omitempty"`
	// Sharpness es la varianza del laplaciano, en las MISMAS unidades que el
	// analizador de verdad: una webcam decente ronda los cientos, no el 0-1.
	// Tenerlo en otra escala hacía que el perfil de decisión leyera todas las
	// sesiones de prueba como fuera de foco.
	Sharpness  float64 `json:"sharpness,omitempty"`
	Brightness float64 `json:"brightness,omitempty"`
	// GazeX es el desplazamiento del iris dentro de su órbita, en anchos de
	// ojo y en coordenadas de IMAGEN: positivo hacia la derecha de la imagen.
	// Igual que la pose, se mide desde la cámara aunque el reto se enuncie
	// desde el sujeto.
	GazeX float64 `json:"gaze_x,omitempty"`
	GazeY float64 `json:"gaze_y,omitempty"`
	// EyeOpenness es la apertura del párpado, alto/ancho del ojo. Es el canal
	// VERTICAL de la mirada: al mirar arriba el párpado se retrae y al mirar
	// abajo baja con el ojo.
	EyeOpenness float64 `json:"eye_openness,omitempty"`
	// EyesShut simula ojos cerrados o gafas con reflejo: la mirada no se
	// puede medir y no debe entrar en ninguna decisión.
	EyesShut bool `json:"eyes_shut,omitempty"`
	// Flat marca una superficie plana (foto o pantalla) en vez de un rostro
	// con volumen. Sirve para simular ataques en /bench.
	Flat bool `json:"flat,omitempty"`
}

// StubVersion identifica al analizador de pruebas en las trazas.
const StubVersion = "stub-synthetic-1"

// Stub es un analizador sintético: decodifica la escena que trae el frame y
// la traduce a magnitudes. Determinista y sin estado.
//
// NO ES EL ANALIZADOR. El de verdad está en Python detrás de NATS y mira
// píxeles. Este existe para cerrar el circuito de extremo a extremo mientras
// aquél no está.
type Stub struct {
	// Delay simula coste de proceso. Sirve para provocar backpressure en los
	// tests.
	Delay time.Duration
}

// NewStub crea un analizador sintético.
func NewStub() *Stub { return &Stub{} }

// Analyze mide el frame. Un payload que no describa una escena se trata como
// un frame ilegible: sin rostro, sin señales.
func (s *Stub) Analyze(ctx context.Context, req Request) (Features, error) {
	started := req.ReceivedAt

	if s.Delay > 0 {
		select {
		case <-ctx.Done():
			return Features{}, ctx.Err()
		case <-time.After(s.Delay):
		}
	}

	var scene Scene
	if err := json.Unmarshal(req.Payload, &scene); err != nil {
		return Features{
			Seq:     req.Seq,
			Signals: map[string]float64{"quality_sharpness": 0},
			Quality: Quality{FaceDetected: false},
			Version: StubVersion,
		}, nil
	}

	signals := map[string]float64{
		"pose_yaw_deg":           scene.YawDeg,
		"pose_pitch_deg":         scene.PitchDeg,
		"face_area_ratio":        scene.FaceAreaRatio,
		"quality_sharpness":      orDefault(scene.Sharpness, 600),
		"quality_brightness":     orDefault(scene.Brightness, 0.5),
		"quality_face_highlight": 0.01,
		"depth_plane_residual":   boolTo(scene.Flat, 0.02, 0.61),
		"texture_lbp_energy":     boolTo(scene.Flat, 0.21, 0.74),
		"rppg_snr_db":            boolTo(scene.Flat, 0.4, 7.2),
	}

	// Clasificador pasivo de textura: una superficie plana da probabilidad de
	// ataque alta; un rostro con volumen, baja.
	// Distintos a propósito entre modelos: el ancho vive del contexto y el
	// estrecho de la textura, así que ante un plano no reaccionan igual.
	signals["texture_pad_v2_a"] = boolTo(scene.Flat, 0.88, 0.02)
	signals["texture_pad_v2_b"] = boolTo(scene.Flat, 0.21, 0.01)
	signals["texture_pad_v1se_a"] = boolTo(scene.Flat, 0.64, 0.03)
	signals["texture_pad_v1se_b"] = boolTo(scene.Flat, 0.12, 0.01)

	if !scene.EyesShut {
		signals["gaze_offset_x"] = scene.GazeX
		signals["gaze_offset_y"] = scene.GazeY
		signals["gaze_agreement"] = 1.0
		signals["gaze_openness"] = orDefault(scene.EyeOpenness, 0.30)
	}

	r, g, b := colorResponse(scene)
	signals["color_response_r"] = r
	signals["color_response_g"] = g
	signals["color_response_b"] = b

	// Relación rostro/fondo por canal, que es lo que el gateway correlaciona
	// contra la secuencia emitida. Modulación deliberadamente PEQUEÑA: es el
	// orden de magnitud que deja una pantalla de portátil en una habitación
	// normal, y el punto de la correlación es funcionar justo ahí.
	rb, rg, rr := faceBackgroundRatio(scene)
	signals["surface_face_bg_b"] = rb
	signals["surface_face_bg_g"] = rg
	signals["surface_face_bg_r"] = rr
	signals["surface_face_bg_luminance_ratio"] = (rb + rg + rr) / 3

	elapsed := time.Duration(0)
	if !started.IsZero() {
		elapsed = time.Since(started)
	}

	return Features{
		Seq:     req.Seq,
		Signals: signals,
		Quality: Quality{
			FaceDetected: scene.Face,
			FaceCount:    boolToInt(scene.Face),
			Sharpness:    orDefault(scene.Sharpness, 0.8),
			Brightness:   orDefault(scene.Brightness, 0.5),
		},
		ProcessingMS: elapsed.Milliseconds(),
		Version:      StubVersion,
	}, nil
}

// colorResponse simula la respuesta cromática del rostro a la luz que emite
// la pantalla del cliente. El analizador no sabe qué color se pidió: sólo mide
// el que le llega reflejado.
func colorResponse(scene Scene) (r, g, b float64) {
	const base = 0.18 // reflejo ambiente
	if !scene.Face {
		return 0, 0, 0
	}
	r, g, b = base, base, base
	// Una superficie plana refleja peor y más uniforme.
	gain := 0.72
	if scene.Flat {
		gain = 0.24
	}
	switch scene.ScreenColor {
	case "white":
		r, g, b = base+gain, base+gain, base+gain
	case "red":
		r += gain
	case "green":
		g += gain
	case "blue":
		b += gain
	}
	return clamp01(r), clamp01(g), clamp01(b)
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

func orDefault(v, def float64) float64 {
	if v == 0 {
		return def
	}
	return v
}

func boolTo(cond bool, ifTrue, ifFalse float64) float64 {
	if cond {
		return ifTrue
	}
	return ifFalse
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// faceBackgroundRatio simula la relación rostro/fondo por canal, en orden BGR.
//
// El fondo está lejos de la pantalla y no recibe el destello, así que sólo
// sube el canal que la pantalla enciende — y poco: una modulación del 8 %, del
// orden de lo que se mide con una webcam en una habitación normal.
func faceBackgroundRatio(scene Scene) (b, g, r float64) {
	const rest = 0.74
	const depth = 0.08

	b, g, r = rest, rest, rest
	if !scene.Face {
		return b, g, r
	}
	switch scene.ScreenColor {
	case "white":
		b, g, r = rest*(1+depth), rest*(1+depth), rest*(1+depth)
	case "red":
		r = rest * (1 + depth)
	case "green":
		g = rest * (1 + depth)
	case "blue":
		b = rest * (1 + depth)
	}
	return b, g, r
}
