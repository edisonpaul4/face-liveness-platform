package challenge

import (
	"errors"
	"testing"
	"time"
)

func TestDefaultPolicyIsValid(t *testing.T) {
	if err := DefaultPolicy().Validate(); err != nil {
		t.Fatalf("la política por defecto no valida: %v", err)
	}
}

// TestDefaultPolicyMatchesSpec fija los números del diseño para que un cambio
// accidental salte aquí y no en producción.
func TestDefaultPolicyMatchesSpec(t *testing.T) {
	p := DefaultPolicy()
	cases := []struct {
		name      string
		got, want any
	}{
		{"CalibrationHold", p.CalibrationHold, 1500 * time.Millisecond},
		{"MinSteps", p.MinSteps, 5},
		{"MaxSteps", p.MaxSteps, 6},
		{"MinFlashColors", p.MinFlashColors, 2},
		{"MaxFlashColors", p.MaxFlashColors, 2},
		// El mínimo tiene que quedar por encima del rango de búsqueda de
		// retardo del analizador (600 ms), o el buscador desliza el estímulo
		// un tramo entero y encaja una secuencia sobre su contraria.
		{"MinFlashSegment", p.MinFlashSegment, 350 * time.Millisecond},
		{"MaxFlashSegment", p.MaxFlashSegment, 600 * time.Millisecond},
		{"FlashDurationSpread", p.FlashDurationSpread, 100 * time.Millisecond},
		{"MinReaction", p.MinReaction, 350 * time.Millisecond},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %v, se esperaba %v", c.name, c.got, c.want)
		}
	}
}

func TestPolicyValidate(t *testing.T) {
	cases := []struct {
		name  string
		mutar func(*Policy)
	}{
		{"calibración cero", func(p *Policy) { p.CalibrationHold = 0 }},
		{"calibración negativa", func(p *Policy) { p.CalibrationHold = -time.Second }},
		{"holgura de calibración negativa", func(p *Policy) { p.CalibrationSlack = -time.Second }},
		{"MinSteps cero", func(p *Policy) { p.MinSteps = 0 }},
		{"MinSteps negativo", func(p *Policy) { p.MinSteps = -1 }},
		{"MaxSteps < MinSteps", func(p *Policy) { p.MinSteps, p.MaxSteps = 3, 2 }},
		{"MaxSteps desorbitado", func(p *Policy) { p.MaxSteps = 99 }},
		{"MinFlashColors cero", func(p *Policy) { p.MinFlashColors = 0 }},
		{"MaxFlashColors < MinFlashColors", func(p *Policy) { p.MinFlashColors, p.MaxFlashColors = 5, 4 }},
		{"segmento cero", func(p *Policy) { p.MinFlashSegment = 0 }},
		{"segmento máximo menor que el mínimo", func(p *Policy) { p.MaxFlashSegment = time.Millisecond }},
		{"holgura de destello negativa", func(p *Policy) { p.FlashSlack = -time.Second }},
		{"reacción mínima negativa", func(p *Policy) { p.MinReaction = -time.Millisecond }},
		{"plazo de pose por debajo de la reacción mínima", func(p *Policy) { p.PoseDeadline = 10 * time.Millisecond }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := DefaultPolicy()
			c.mutar(&p)
			err := p.Validate()
			if err == nil {
				t.Fatal("se esperaba un error de validación")
			}
			if !errors.Is(err, ErrInvalidPolicy) {
				t.Errorf("errors.Is(ErrInvalidPolicy) = false: %v", err)
			}
			if _, gerr := Generate(1, p); !errors.Is(gerr, ErrInvalidPolicy) {
				t.Errorf("Generate con política inválida: %v", gerr)
			}
		})
	}
}

// TestZeroPolicyIsInvalid: la política cero no puede colarse como válida.
func TestZeroPolicyIsInvalid(t *testing.T) {
	if err := (Policy{}).Validate(); !errors.Is(err, ErrInvalidPolicy) {
		t.Errorf("la política cero debería ser inválida, err=%v", err)
	}
}
