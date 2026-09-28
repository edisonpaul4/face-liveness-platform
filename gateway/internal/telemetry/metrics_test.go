package telemetry

import (
	"reflect"
	"strings"
	"sync"
	"testing"
)

// TestEveryCounterIsExposed: un contador que no sale en Snapshot es un
// contador que no existe para quien mira. Este test es la red que faltaba
// cuando se añadieron los del bus y se quedaron fuera del mapa.
func TestEveryCounterIsExposed(t *testing.T) {
	m := New()

	// Se pone cada contador a un valor distinto para poder localizarlo.
	v := reflect.ValueOf(m).Elem()
	typ := v.Type()
	expected := map[string]int64{}

	for i := range typ.NumField() {
		field := typ.Field(i)
		if field.Type.String() != "atomic.Int64" {
			continue
		}
		counter := v.Field(i).Addr().Interface().(interface{ Store(int64) })
		value := int64(i + 1)
		counter.Store(value)
		expected[field.Name] = value
	}

	if len(expected) == 0 {
		t.Fatal("no se encontró ningún contador")
	}

	snap := m.Snapshot()
	if len(snap) != len(expected) {
		t.Errorf("Snapshot expone %d contadores y la estructura tiene %d", len(snap), len(expected))
	}

	// Todo valor asignado tiene que aparecer exactamente una vez.
	seen := map[int64]int{}
	for _, got := range snap {
		seen[got]++
	}
	for name, want := range expected {
		if seen[want] != 1 {
			t.Errorf("el contador %s (valor %d) no aparece en Snapshot", name, want)
		}
	}
}

func TestSnapshotText(t *testing.T) {
	m := New()
	m.FramesReceived.Add(7)
	m.LeasesLost.Add(2)

	text := m.Snapshot().Text()
	for _, want := range []string{
		"liveness_gateway_frames_received 7",
		"liveness_gateway_leases_lost 2",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("falta %q en:\n%s", want, text)
		}
	}

	// Orden estable: se lee a ojo y se compara entre despliegues.
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := 1; i < len(lines); i++ {
		if lines[i-1] > lines[i] {
			t.Errorf("las líneas no están ordenadas: %q antes de %q", lines[i-1], lines[i])
		}
	}
}

func TestMetricsAreConcurrencySafe(t *testing.T) {
	m := New()
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.FramesReceived.Add(1)
			m.FramesDroppedBackpressure.Add(1)
			_ = m.Snapshot()
		}()
	}
	wg.Wait()

	if got := m.Snapshot()["frames_received"]; got != 50 {
		t.Errorf("frames_received = %d, se esperaba 50", got)
	}
}
