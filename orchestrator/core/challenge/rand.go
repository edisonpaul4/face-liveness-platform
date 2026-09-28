package challenge

// prng es un splitmix64: 20 líneas, sin dependencias y con un algoritmo
// especificado que no cambia entre versiones de Go.
//
// No se usa math/rand a propósito. El guion debe ser reproducible byte a byte
// a partir de la semilla durante años, para que /bench pueda replicar un caso
// exacto. La estabilidad del generador es parte del contrato; la de
// math/rand, no.
//
// No es criptográfico: no le hace falta. Lo que debe ser impredecible para el
// atacante es la SEMILLA, que se genera con crypto/rand en el borde y nunca
// sale del servidor.
type prng struct {
	state uint64
}

func newPRNG(seed uint64) *prng { return &prng{state: seed} }

// next devuelve el siguiente valor de 64 bits.
func (p *prng) next() uint64 {
	p.state += 0x9E3779B97F4A7C15
	z := p.state
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

// intn devuelve un entero uniforme en [0,n) sin sesgo de módulo.
// Para n <= 1 devuelve 0: los llamantes de este paquete pasan siempre
// constantes positivas ya validadas por Policy.Validate.
func (p *prng) intn(n int) int {
	if n <= 1 {
		return 0
	}
	un := uint64(n)
	// 2^64 mod un, calculado sin desbordar.
	threshold := (0 - un) % un
	for {
		v := p.next()
		if v >= threshold {
			return int(v % un)
		}
	}
}

// durationMS devuelve una duración uniforme en [minMS,maxMS] milisegundos.
// La granularidad de milisegundo es deliberada: los valores viajan al cliente
// y deben ser legibles y reproducibles.
func (p *prng) durationMS(minMS, maxMS int) int {
	if maxMS <= minMS {
		return minMS
	}
	return minMS + p.intn(maxMS-minMS+1)
}

// shuffle aplica Fisher-Yates descendente sobre n elementos.
func (p *prng) shuffle(n int, swap func(i, j int)) {
	for i := n - 1; i > 0; i-- {
		j := p.intn(i + 1)
		swap(i, j)
	}
}
