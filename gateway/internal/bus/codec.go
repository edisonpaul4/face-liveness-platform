package bus

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/edisonpaul4/biometrics/gateway/internal/analyzer"
)

// Formato de los mensajes del bus.
//
// Los frames van en binario compacto porque son el camino caliente: un JSON
// con el payload en base64 costaría un tercio más de red por cada frame. Todo
// lo demás —anuncios, leases, medidas, control— va en JSON, que es de bajo
// caudal y así el worker de Python lo lee sin generar código.
//
// PROVISIONAL: cuando haya codegen de protobuf (/proto/nats/v1) esto se
// sustituye por los tipos generados. El reparto binario/JSON se mantendrá.
const (
	// FrameTaskHeaderSize es la cabecera de un frame en el bus.
	FrameTaskHeaderSize = 32
	// FrameTaskVersion es la versión del formato.
	FrameTaskVersion = 1
)

// Flags de la cabecera.
const (
	// FlagClientClockTrusted indica que la deriva del reloj del cliente
	// resultó plausible y CapturedAtUS sirve para alinear.
	FlagClientClockTrusted uint16 = 1 << 0
)

// Errores de decodificación.
var (
	ErrShortFrame   = errors.New("bus: frame más corto que la cabecera")
	ErrFrameVersion = errors.New("bus: versión de frame no soportada")
	ErrFrameLength  = errors.New("bus: longitud declarada distinta del payload")
)

// FrameTask es un frame camino del analizador.
//
// No lleva el identificador de sesión: va en el subject. Y no lleva NADA del
// estado de la sesión —ni reto activo, ni umbrales, ni veredicto— porque el
// analizador mide y no decide (CLAUDE.md §3).
type FrameTask struct {
	Encoding uint8
	Flags    uint16
	Seq      uint64
	// CapturedAtUS es el reloj del CLIENTE. Sólo alinea; no puntúa.
	CapturedAtUS int64
	// ReceivedAtUS es el sello del SERVIDOR. Es el que puntúa.
	ReceivedAtUS int64
	Payload      []byte
}

// ClientClockTrusted indica si el sello del cliente es utilizable.
func (f FrameTask) ClientClockTrusted() bool { return f.Flags&FlagClientClockTrusted != 0 }

// EncodeFrameTask serializa el frame.
func EncodeFrameTask(f FrameTask) []byte {
	out := make([]byte, FrameTaskHeaderSize+len(f.Payload))
	out[0] = FrameTaskVersion
	out[1] = f.Encoding
	binary.BigEndian.PutUint16(out[2:4], f.Flags)
	binary.BigEndian.PutUint64(out[4:12], f.Seq)
	binary.BigEndian.PutUint64(out[12:20], uint64(f.CapturedAtUS))
	binary.BigEndian.PutUint64(out[20:28], uint64(f.ReceivedAtUS))
	binary.BigEndian.PutUint32(out[28:32], uint32(len(f.Payload)))
	copy(out[FrameTaskHeaderSize:], f.Payload)
	return out
}

// DecodeFrameTask deserializa el frame. El payload apunta al buffer de
// entrada: no se copia.
func DecodeFrameTask(b []byte) (FrameTask, error) {
	if len(b) < FrameTaskHeaderSize {
		return FrameTask{}, fmt.Errorf("%w: %d bytes", ErrShortFrame, len(b))
	}
	if b[0] != FrameTaskVersion {
		return FrameTask{}, fmt.Errorf("%w: %d", ErrFrameVersion, b[0])
	}
	f := FrameTask{
		Encoding:     b[1],
		Flags:        binary.BigEndian.Uint16(b[2:4]),
		Seq:          binary.BigEndian.Uint64(b[4:12]),
		CapturedAtUS: int64(binary.BigEndian.Uint64(b[12:20])),
		ReceivedAtUS: int64(binary.BigEndian.Uint64(b[20:28])),
	}
	declared := binary.BigEndian.Uint32(b[28:32])
	payload := b[FrameTaskHeaderSize:]
	if int(declared) != len(payload) {
		return FrameTask{}, fmt.Errorf("%w: declara %d, hay %d", ErrFrameLength, declared, len(payload))
	}
	f.Payload = payload
	return f, nil
}

// --- mensajes JSON -----------------------------------------------------------

// Announce es el anuncio periódico de capacidad de un worker.
type Announce struct {
	WorkerID string `json:"worker_id"`
	// Capacity es cuántas sesiones simultáneas admite.
	Capacity int `json:"capacity"`
	// Sessions es cuántas lleva ahora mismo.
	Sessions int `json:"sessions"`
	// InFlight son los frames pendientes de analizar.
	InFlight    int    `json:"in_flight"`
	Version     string `json:"version"`
	EmittedAtUS int64  `json:"emitted_at_us"`
}

// Load es la ocupación en [0,1]. Un worker sin capacidad declarada se
// considera lleno: mejor no mandarle nada que reventarlo.
func (a Announce) Load() float64 {
	if a.Capacity <= 0 {
		return 1
	}
	load := float64(a.Sessions) / float64(a.Capacity)
	if load > 1 {
		return 1
	}
	return load
}

// LeaseRequest pide a un worker que se haga cargo de una sesión.
type LeaseRequest struct {
	SessionID string `json:"session_id"`
	// DeadlineUS es hasta cuándo vive la sesión. Pasado ese punto el worker
	// puede soltar el estado caliente aunque no le llegue el cierre.
	DeadlineUS int64 `json:"deadline_us"`
}

// LeaseReply es la respuesta del worker.
type LeaseReply struct {
	Accepted bool   `json:"accepted"`
	WorkerID string `json:"worker_id"`
	Reason   string `json:"reason,omitempty"`
}

// SessionHeartbeat renueva el lease. Mientras llegue, el gateway da al worker
// por vivo.
type SessionHeartbeat struct {
	WorkerID    string `json:"worker_id"`
	SessionID   string `json:"session_id"`
	InFlight    int    `json:"in_flight"`
	EmittedAtUS int64  `json:"emitted_at_us"`
}

// Quality describe si el frame servía para medir.
type Quality struct {
	FaceDetected bool    `json:"face_detected"`
	FaceCount    int     `json:"face_count"`
	Sharpness    float64 `json:"sharpness"`
	Brightness   float64 `json:"brightness"`
}

// FrameFeatures son las medidas de un frame.
//
// Magnitudes con nombre, nunca decisiones: aquí no cabe un is_live ni un
// spoof_score (CLAUDE.md §3).
type FrameFeatures struct {
	Seq          uint64             `json:"seq"`
	Signals      map[string]float64 `json:"signals"`
	Quality      Quality            `json:"quality"`
	ProcessingMS int64              `json:"processing_ms"`
	Version      string             `json:"version"`
	AnalyzedAtUS int64              `json:"analyzed_at_us"`
}

// Tipos de mensaje de control.
const (
	// ControlSessionClose pide al worker que suelte el estado caliente.
	ControlSessionClose = "session_close"
)

// SessionControl es higiene de recursos, no semántica de negocio.
type SessionControl struct {
	Kind      string `json:"kind"`
	SessionID string `json:"session_id"`
}

// windowRequestWire es el JSON que espera el analizador.
//
// Lo que NO aparece aquí es la parte importante: ni identificador de reto, ni
// posición en el guion, ni umbral de aprobado, ni qué pasa si sale mal. Python
// mide un intervalo; qué signifique es de Go (CLAUDE.md §3).
type windowRequestWire struct {
	Kind     string `json:"kind"`
	WindowID string `json:"window_id"`

	Axis         string  `json:"axis,omitempty"`
	TargetDeg    float64 `json:"target_deg,omitempty"`
	ToleranceDeg float64 `json:"tolerance_deg,omitempty"`

	Sequence []flashSegmentWire `json:"sequence,omitempty"`

	StartedAtUS int64 `json:"started_at_us"`
	EndedAtUS   int64 `json:"ended_at_us"`
}

type flashSegmentWire struct {
	Color      string `json:"color"`
	DurationMS int64  `json:"duration_ms"`
}

// EncodeWindowRequest serializa la petición de medida.
func EncodeWindowRequest(req analyzer.WindowRequest) ([]byte, error) {
	w := windowRequestWire{
		Kind:         string(req.Kind),
		WindowID:     req.ID,
		Axis:         req.Axis,
		TargetDeg:    req.TargetDeg,
		ToleranceDeg: req.ToleranceDeg,
		StartedAtUS:  req.StartedAt.UnixMicro(),
		EndedAtUS:    req.EndedAt.UnixMicro(),
	}
	for _, seg := range req.Sequence {
		w.Sequence = append(w.Sequence, flashSegmentWire{Color: seg.Color, DurationMS: seg.DurationMS})
	}
	return json.Marshal(w)
}

// challengeScoreWire es lo que devuelve el analizador.
type challengeScoreWire struct {
	WindowID   string              `json:"window_id"`
	Kind       string              `json:"kind"`
	Axis       string              `json:"axis"`
	Score      float64             `json:"score"`
	Submetrics map[string]*float64 `json:"submetrics"`
	Raw        map[string]float64  `json:"raw"`
	FramesUsed int                 `json:"frames_used"`

	// Los dos siguientes sólo vienen en el destello. Ausentes significan
	// medible: una ventana de pose que llega es una ventana que se midió.
	QualitySufficient *bool  `json:"quality_sufficient"`
	QualityReason     string `json:"quality_reason"`
}

// DecodeChallengeScore lee la medida de una ventana.
func DecodeChallengeScore(b []byte) (analyzer.WindowScore, error) {
	var w challengeScoreWire
	if err := json.Unmarshal(b, &w); err != nil {
		return analyzer.WindowScore{}, fmt.Errorf("bus: medida de ventana ilegible: %w", err)
	}
	if w.WindowID == "" {
		return analyzer.WindowScore{}, fmt.Errorf("bus: medida de ventana sin window_id")
	}

	// El tipo no siempre viaja: una ventana de pose trae `axis` y ninguna
	// marca. Se deduce en vez de exigirlo, para no romper por un campo que el
	// analizador no necesita emitir.
	kind := analyzer.WindowKind(w.Kind)
	if kind == "" {
		if w.Axis != "" {
			kind = analyzer.WindowPose
		} else {
			kind = analyzer.WindowCalibration
		}
	}

	quality := true
	if w.QualitySufficient != nil {
		quality = *w.QualitySufficient
	}

	return analyzer.WindowScore{
		WindowID:          w.WindowID,
		Kind:              kind,
		Score:             w.Score,
		Submetrics:        w.Submetrics,
		Raw:               w.Raw,
		FramesUsed:        w.FramesUsed,
		QualitySufficient: quality,
		QualityReason:     w.QualityReason,
	}, nil
}
