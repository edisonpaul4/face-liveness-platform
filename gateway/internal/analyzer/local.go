package analyzer

import (
	"context"
	"sync"
	"time"
)

// LocalPool ejecuta el análisis dentro del propio proceso.
//
// Es el camino de desarrollo y el de los tests que no necesitan bus: mismo
// puerto, sin NATS. No hay descubrimiento ni lease porque no hay nada que
// pueda caerse por separado.
type LocalPool struct {
	analyzer Analyzer
	// Queue es cuántos frames caben esperando análisis. Uno por defecto: lo
	// que no se analiza a tiempo se tira, no se encola.
	Queue int
}

// NewLocalPool crea un pool en proceso sobre el analizador dado.
func NewLocalPool(a Analyzer) *LocalPool {
	if a == nil {
		a = NewStub()
	}
	return &LocalPool{analyzer: a, Queue: 1}
}

// Acquire abre un canal de análisis local.
func (p *LocalPool) Acquire(_ context.Context, sessionID string, _ time.Time) (Analysis, error) {
	queue := p.Queue
	if queue <= 0 {
		queue = 1
	}
	la := &localAnalysis{
		sessionID: sessionID,
		analyzer:  p.analyzer,
		in:        make(chan Request, queue),
		out:       make(chan Features, 8),
		scores:    make(chan WindowScore, 8),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	go la.run()
	return la, nil
}

type localAnalysis struct {
	sessionID string
	analyzer  Analyzer

	in     chan Request
	out    chan Features
	scores chan WindowScore
	stop   chan struct{}
	done   chan struct{}

	closeOnce sync.Once
}

func (l *localAnalysis) run() {
	defer close(l.done)
	for {
		select {
		case <-l.stop:
			return
		case req := <-l.in:
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				select {
				case <-l.stop:
					cancel()
				case <-ctx.Done():
				}
			}()
			features, err := l.analyzer.Analyze(ctx, req)
			cancel()
			if err != nil {
				continue
			}
			select {
			case l.out <- features:
			case <-l.stop:
				return
			default:
				// El consumidor va por detrás: se tira, no se acumula.
			}
		}
	}
}

// Submit encola el frame si hay hueco. Si no lo hay, el analizador va por
// detrás y el frame se descarta.
func (l *localAnalysis) Submit(_ context.Context, req Request) error {
	select {
	case <-l.done:
		return ErrClosed
	default:
	}
	select {
	case l.in <- req:
		return nil
	default:
		return ErrBusy
	}
}

func (l *localAnalysis) Features() <-chan Features { return l.out }

// Lost devuelve un canal que nunca emite: un analizador en proceso no se cae
// por su cuenta, se cae con todo lo demás.
func (l *localAnalysis) Lost() <-chan error { return nil }

func (l *localAnalysis) WorkerID() string { return "local" }

func (l *localAnalysis) Close(context.Context) error {
	l.closeOnce.Do(func() { close(l.stop) })
	<-l.done
	return nil
}

// RequestWindow puntúa la ventana con el mismo estimador sintético que usa el
// stub para los frames. No mide nada real: sirve para que el recorrido
// completo se pueda ejercitar sin Python.
func (l *localAnalysis) RequestWindow(_ context.Context, req WindowRequest) error {
	score := WindowScore{
		WindowID:          req.ID,
		Kind:              req.Kind,
		Score:             1,
		QualitySufficient: true,
		Submetrics:        map[string]*float64{},
		Raw:               map[string]float64{},
	}
	for _, name := range submetricsFor(req.Kind) {
		v := 0.9
		score.Submetrics[name] = &v
	}

	select {
	case l.scores <- score:
	case <-l.stop:
	}
	return nil
}

func (l *localAnalysis) Scores() <-chan WindowScore { return l.scores }

// submetricsFor son las sub-métricas que Python emite para cada tipo de
// ventana. El stub las rellena todas para que la fusión se ejercite entera.
func submetricsFor(kind WindowKind) []string {
	switch kind {
	case WindowPose:
		return []string{"compliance", "continuity", "parallax", "identity"}
	case WindowFlash:
		return []string{"correlation", "gradient_3d", "screen_absence"}
	default:
		return nil
	}
}
