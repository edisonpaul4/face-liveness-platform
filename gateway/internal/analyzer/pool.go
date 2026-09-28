package analyzer

import (
	"context"
	"errors"
	"time"
)

// Errores del puerto de análisis.
var (
	// ErrNoWorkers: no hay ningún analizador disponible al que asignar la
	// sesión.
	ErrNoWorkers = errors.New("analyzer: no hay analizadores disponibles")
	// ErrLost: el analizador asignado dejó de responder. La sesión no se
	// recupera: se aborta y se repite desde cero.
	ErrLost = errors.New("analyzer: el analizador dejó de responder")
	// ErrBusy: el analizador no admite más frames ahora mismo. El frame se
	// descarta; no se encola.
	ErrBusy = errors.New("analyzer: analizador ocupado")
	// ErrClosed: el canal de análisis ya está cerrado.
	ErrClosed = errors.New("analyzer: canal de análisis cerrado")
)

// Analysis es el canal de análisis de UNA sesión.
//
// Es asíncrono a propósito: enviar un frame no espera su resultado. Si
// esperase, un analizador lento o muerto congelaría la sesión, y lo que hay
// que hacer con un frame que no se analiza a tiempo es tirarlo, no aguardarlo.
//
// La afinidad de sesión vive aquí: un Analysis está atado a un worker
// concreto durante toda la sesión, porque el estado caliente (tracking,
// ventana de rPPG, flujo óptico) está en la memoria de ESE worker y no viaja
// con cada frame.
type Analysis interface {
	// Submit envía un frame. No bloquea y no espera respuesta.
	Submit(ctx context.Context, req Request) error
	// Features entrega las medidas conforme llegan, en orden de llegada.
	Features() <-chan Features
	// RequestWindow pide medir una ventana ya cerrada: pose, calibración o
	// destello. No bloquea; el resultado llega por Scores.
	//
	// Es el ÚNICO sitio donde Go le cuenta algo a Python, y va recortado a
	// propósito: eje, objetivo, margen e intervalo. Ni identificador de reto,
	// ni posición en el guion, ni umbral de aprobado, ni qué pasa si sale mal
	// (CLAUDE.md §3).
	RequestWindow(ctx context.Context, req WindowRequest) error
	// Scores entrega las puntuaciones de ventana conforme llegan.
	Scores() <-chan WindowScore
	// Lost avisa de que el analizador dejó de responder. Nunca emite nada si
	// todo va bien.
	Lost() <-chan error
	// WorkerID identifica al worker asignado, para trazas.
	WorkerID() string
	// Close libera el estado caliente del worker y suelta la asignación.
	Close(ctx context.Context) error
}

// Pool asigna un analizador a una sesión.
type Pool interface {
	// Acquire elige un worker y le asigna la sesión hasta deadline.
	Acquire(ctx context.Context, sessionID string, deadline time.Time) (Analysis, error)
}

// WindowKind distingue qué se pide medir.
type WindowKind string

// Tipos de ventana.
const (
	WindowPose        WindowKind = "pose"
	WindowCalibration WindowKind = "calibration"
	WindowFlash       WindowKind = "flash"
	// WindowPulse pide el pulso sanguíneo de un intervalo. No lleva
	// parámetros: el latido no depende de qué se le pidió al sujeto.
	WindowPulse WindowKind = "rppg"
)

// FlashSegment es un color mantenido durante una duración.
type FlashSegment struct {
	Color      string
	DurationMS int64
}

// WindowRequest es lo que Go le pide medir a Python.
//
// Lo que NO lleva es tan importante como lo que lleva: nada que revele el
// guion ni qué consecuencia tiene la medida.
type WindowRequest struct {
	ID   string
	Kind WindowKind

	// Sólo para pose.
	Axis         string
	TargetDeg    float64
	ToleranceDeg float64

	// Sólo para destello: la secuencia emitida, sin la cual no hay con qué
	// correlacionar.
	Sequence []FlashSegment

	StartedAt time.Time
	EndedAt   time.Time
}

// WindowScore es lo que Python devuelve: magnitudes con nombre y nada más.
//
// No trae veredicto. Si el análisis no pudo medir lo dice con
// QualitySufficient, que lleva a reintentar y nunca a rechazo.
type WindowScore struct {
	WindowID   string
	Kind       WindowKind
	Score      float64
	Submetrics map[string]*float64
	Raw        map[string]float64
	FramesUsed int

	QualitySufficient bool
	QualityReason     string
}
