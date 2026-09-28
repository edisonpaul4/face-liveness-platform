// Package clock es la única fuente de tiempo del núcleo del orquestador.
//
// El núcleo nunca llama a time.Now(): recibe un Clock inyectado. Así toda la
// lógica temporal (ventanas de reacción, deadlines, expiración) es
// determinista en tests y reproducible en /bench.
package clock

import (
	"sync"
	"time"
)

// Clock entrega el instante actual.
type Clock interface {
	Now() time.Time
}

// System es el reloj real del proceso. Es el único punto del núcleo que toca
// el reloj del sistema operativo, y no debe usarse en tests.
type System struct{}

// Now devuelve la hora del sistema.
func (System) Now() time.Time { return time.Now() }

// Fake es un reloj manual, para tests y para la reproducción determinista de
// trazas del banco de pruebas. Es seguro para uso concurrente.
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

// NewFake crea un reloj manual detenido en start.
func NewFake(start time.Time) *Fake { return &Fake{now: start} }

// Now devuelve el instante actual del reloj manual.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance adelanta el reloj d y devuelve el nuevo instante. Una duración
// negativa retrocede el reloj: es útil para simular desorden de relojes, no
// para uso normal.
func (f *Fake) Advance(d time.Duration) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	return f.now
}

// Set fija el reloj en t.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t
}
