package conn

import (
	"testing"
	"time"
)

// El pulso se mide sobre el tramo más largo SIN destellos. Un destello mueve
// el color de la piel órdenes de magnitud más que un latido, así que dentro de
// él la señal no está enterrada en ruido: está tapada.
func TestLongestUnlitSpan(t *testing.T) {
	base := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }

	cases := []struct {
		name  string
		first time.Time
		now   time.Time
		lit   []interval
		want  *interval // nil = no hay tramo suficiente
	}{
		{
			name:  "sin destellos, la sesión entera",
			first: at(0), now: at(30),
			want: &interval{from: at(0), to: at(30)},
		},
		{
			name:  "un destello en medio parte la sesión y gana el trozo mayor",
			first: at(0), now: at(30),
			lit:  []interval{{from: at(8), to: at(11)}},
			want: &interval{from: at(11), to: at(30)},
		},
		{
			name:  "el trozo mayor puede ser el de antes del destello",
			first: at(0), now: at(30),
			lit:  []interval{{from: at(22), to: at(25)}},
			want: &interval{from: at(0), to: at(22)},
		},
		{
			name:  "destellos desordenados se ordenan antes de medir",
			first: at(0), now: at(40),
			lit: []interval{
				{from: at(30), to: at(32)},
				{from: at(5), to: at(7)},
			},
			want: &interval{from: at(7), to: at(30)},
		},
		{
			name:  "destellos solapados no cuentan dos veces",
			first: at(0), now: at(30),
			lit: []interval{
				{from: at(5), to: at(9)},
				{from: at(7), to: at(12)},
			},
			want: &interval{from: at(12), to: at(30)},
		},
		{
			// Doce segundos troceados en cachos de cuatro no son doce
			// segundos: la resolución en frecuencia la fija el tramo
			// continuo más largo, no la suma.
			name:  "muchos destellos dejan sólo trozos cortos y no se pide nada",
			first: at(0), now: at(30),
			lit: []interval{
				{from: at(5), to: at(8)},
				{from: at(13), to: at(16)},
				{from: at(21), to: at(24)},
			},
			want: nil,
		},
		{
			name:  "una sesión corta no da para medir pulso",
			first: at(0), now: at(6),
			want: nil,
		},
		{
			// Sin un frame aceptado no hay cara que medir, aunque haya
			// pasado el tiempo.
			name: "sin frames no hay tramo",
			now:  at(30),
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Conn{firstFrameAt: tc.first, litIntervals: tc.lit}
			c.clk = fixedClock{now: tc.now}

			got, ok := c.longestUnlitSpan()
			if tc.want == nil {
				if ok {
					t.Fatalf("se pidió pulso sobre %v-%v y no debería", got.from, got.to)
				}
				return
			}
			if !ok {
				t.Fatal("no se eligió tramo y debería haberse elegido")
			}
			if !got.from.Equal(tc.want.from) || !got.to.Equal(tc.want.to) {
				t.Fatalf("tramo %v-%v, se esperaba %v-%v",
					got.from, got.to, tc.want.from, tc.want.to)
			}
		})
	}
}

type fixedClock struct{ now time.Time }

func (f fixedClock) Now() time.Time { return f.now }
