package challenge

import "testing"

// TestPRNGIsDeterministic: el generador es el contrato de reproducibilidad
// con /bench. Estos valores son un vector fijo: si cambian, los guiones
// históricos dejan de reproducirse.
func TestPRNGIsDeterministic(t *testing.T) {
	want := []uint64{
		0xE220A8397B1DCDAF,
		0x6E789E6AA1B965F4,
		0x06C45D188009454F,
	}
	p := newPRNG(0)
	for i, w := range want {
		if got := p.next(); got != w {
			t.Errorf("next()[%d] = %#016X, se esperaba %#016X", i, got, w)
		}
	}

	a, b := newPRNG(42), newPRNG(42)
	for range 100 {
		if a.next() != b.next() {
			t.Fatal("dos generadores con la misma semilla divergen")
		}
	}
}

func TestIntnRange(t *testing.T) {
	p := newPRNG(1)
	for range 10000 {
		if v := p.intn(7); v < 0 || v >= 7 {
			t.Fatalf("intn(7) = %d", v)
		}
	}
}

// TestIntnDegenerate: intn no puede entrar en pánico ni devolver basura con
// argumentos degenerados.
func TestIntnDegenerate(t *testing.T) {
	p := newPRNG(1)
	for _, n := range []int{-5, 0, 1} {
		if v := p.intn(n); v != 0 {
			t.Errorf("intn(%d) = %d, se esperaba 0", n, v)
		}
	}
}

// TestIntnUniformity es una comprobación gruesa de sesgo: con 60000 tiradas
// sobre 6 valores, ninguna casilla debería desviarse un 15 %.
func TestIntnUniformity(t *testing.T) {
	const n, draws = 6, 60000
	counts := make([]int, n)
	p := newPRNG(20260822)
	for range draws {
		counts[p.intn(n)]++
	}
	expected := draws / n
	for i, c := range counts {
		if c < expected*85/100 || c > expected*115/100 {
			t.Errorf("valor %d salió %d veces, se esperaban ~%d", i, c, expected)
		}
	}
}

func TestDurationMS(t *testing.T) {
	p := newPRNG(3)
	for range 1000 {
		if v := p.durationMS(350, 600); v < 350 || v > 600 {
			t.Fatalf("durationMS(350,600) = %d", v)
		}
	}
	if v := p.durationMS(500, 500); v != 500 {
		t.Errorf("durationMS(500,500) = %d", v)
	}
	if v := p.durationMS(500, 100); v != 500 {
		t.Errorf("durationMS con máximo < mínimo = %d, se esperaba el mínimo", v)
	}
}

func TestShuffleIsDeterministicAndPermutes(t *testing.T) {
	build := func() []int { return []int{0, 1, 2, 3, 4, 5, 6, 7} }

	a, b := build(), build()
	newPRNG(9).shuffle(len(a), func(i, j int) { a[i], a[j] = a[j], a[i] })
	newPRNG(9).shuffle(len(b), func(i, j int) { b[i], b[j] = b[j], b[i] })
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("shuffle no es determinista: %v vs %v", a, b)
		}
	}

	seen := map[int]bool{}
	for _, v := range a {
		if seen[v] {
			t.Fatalf("shuffle duplicó elementos: %v", a)
		}
		seen[v] = true
	}
	if len(seen) != 8 {
		t.Fatalf("shuffle perdió elementos: %v", a)
	}

	// Y de verdad mezcla: alguna semilla tiene que alterar el orden.
	shuffled := false
	for seed := range uint64(20) {
		c := build()
		newPRNG(seed).shuffle(len(c), func(i, j int) { c[i], c[j] = c[j], c[i] })
		for i := range c {
			if c[i] != i {
				shuffled = true
			}
		}
	}
	if !shuffled {
		t.Error("shuffle nunca alteró el orden")
	}

	// Casos degenerados: no debe tocar nada ni entrar en pánico.
	newPRNG(1).shuffle(0, func(int, int) { t.Error("swap sobre 0 elementos") })
	newPRNG(1).shuffle(1, func(int, int) { t.Error("swap sobre 1 elemento") })
}
