package clock

import (
	"sync"
	"testing"
	"time"
)

func TestSystemAdvances(t *testing.T) {
	var c Clock = System{}
	first := c.Now()
	if first.IsZero() {
		t.Fatal("System.Now() devolvió el instante cero")
	}
	if second := c.Now(); second.Before(first) {
		t.Errorf("el reloj del sistema retrocedió: %v -> %v", first, second)
	}
}

func TestFakeIsFrozen(t *testing.T) {
	start := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	f := NewFake(start)
	for range 3 {
		if got := f.Now(); !got.Equal(start) {
			t.Fatalf("Now() = %v, se esperaba %v: el reloj falso no puede avanzar solo", got, start)
		}
	}
}

func TestFakeAdvanceAndSet(t *testing.T) {
	start := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	f := NewFake(start)

	got := f.Advance(1500 * time.Millisecond)
	want := start.Add(1500 * time.Millisecond)
	if !got.Equal(want) || !f.Now().Equal(want) {
		t.Errorf("Advance devolvió %v y Now() = %v, se esperaba %v", got, f.Now(), want)
	}

	f.Advance(-500 * time.Millisecond)
	if !f.Now().Equal(start.Add(time.Second)) {
		t.Errorf("Advance negativo: Now() = %v", f.Now())
	}

	f.Set(start)
	if !f.Now().Equal(start) {
		t.Errorf("Set: Now() = %v, se esperaba %v", f.Now(), start)
	}
}

func TestFakeIsConcurrencySafe(t *testing.T) {
	f := NewFake(time.Unix(0, 0))
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.Advance(time.Millisecond)
			_ = f.Now()
		}()
	}
	wg.Wait()
	if got := f.Now(); !got.Equal(time.Unix(0, 0).Add(50 * time.Millisecond)) {
		t.Errorf("Now() = %v tras 50 avances de 1 ms", got)
	}
}
