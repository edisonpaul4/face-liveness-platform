package challenge

import "testing"

// Dos destellos seguidos nunca se pegan por el mismo color.
//
// Cada secuencia alterna por dentro, pero nada impedía que una acabara en rojo
// y la siguiente empezara en rojo. Medido en una sesión real: 355 ms + 424 ms
// de rojo continuo, la cámara se acomodó del todo y en el segundo destello la
// cara se DES-enrojeció durante su propio tramo rojo.
func TestDestellosSeguidosNoRepitenColorEnLaJuntura(t *testing.T) {
	p := DefaultPolicy()
	for seed := Seed(0); seed < 3000; seed++ {
		s, err := Generate(seed, p)
		if err != nil {
			t.Fatalf("semilla %d: %v", seed, err)
		}
		last := FlashUnspecified
		for {
			st, ok := s.Current()
			if !ok {
				break
			}
			if st.Kind == KindFlash {
				if st.Flash[0].Color == last {
					t.Fatalf("semilla %d: dos destellos pegados por %s", seed, last)
				}
				last = st.Flash[len(st.Flash)-1].Color
			}
			if !s.Advance() {
				break
			}
		}
	}
}
