package session

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/edisonpaul4/biometrics/orchestrator/core/clock"
)

var t0 = time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)

func TestIssueProducesUniqueTickets(t *testing.T) {
	r := NewRegistry(clock.NewFake(t0), time.Minute)

	ids := map[string]bool{}
	tokens := map[string]bool{}
	seeds := map[uint64]bool{}

	for range 200 {
		tk, err := r.Issue()
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		if ids[tk.SessionID] {
			t.Fatalf("session_id repetido: %s", tk.SessionID)
		}
		if tokens[tk.Token] {
			t.Fatalf("token repetido: %s", tk.Token)
		}
		if seeds[uint64(tk.Seed)] {
			t.Fatalf("semilla repetida: %d", tk.Seed)
		}
		ids[tk.SessionID] = true
		tokens[tk.Token] = true
		seeds[uint64(tk.Seed)] = true

		if !tk.ExpiresAt.After(tk.IssuedAt) {
			t.Fatalf("ticket caducado al nacer: %+v", tk)
		}
	}
}

// TestTicketIsSingleUse es la propiedad del paquete.
func TestTicketIsSingleUse(t *testing.T) {
	r := NewRegistry(clock.NewFake(t0), time.Minute)

	tk, err := r.Issue()
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if r.Consumed(tk.SessionID) {
		t.Error("el ticket figura como consumido antes de usarse")
	}

	got, err := r.Consume(tk.Token)
	if err != nil {
		t.Fatalf("primer canje: %v", err)
	}
	if got.SessionID != tk.SessionID || got.Seed != tk.Seed {
		t.Errorf("el canje devolvió otro ticket: %+v", got)
	}
	if !r.Consumed(tk.SessionID) {
		t.Error("el ticket no figura como consumido tras usarse")
	}

	for i := range 3 {
		if _, err := r.Consume(tk.Token); !errors.Is(err, ErrTicketConsumed) {
			t.Errorf("canje %d: err = %v, se esperaba ErrTicketConsumed", i+2, err)
		}
	}
}

// TestConcurrentConsumeAllowsExactlyOne: con varios gateways compitiendo por
// el mismo token, sólo uno puede ganar.
func TestConcurrentConsumeAllowsExactlyOne(t *testing.T) {
	r := NewRegistry(clock.NewFake(t0), time.Minute)
	tk, err := r.Issue()
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	const racers = 32
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		wins    int
		rejects int
	)
	start := make(chan struct{})

	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := r.Consume(tk.Token)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
			} else if errors.Is(err, ErrTicketConsumed) {
				rejects++
			}
		}()
	}
	close(start)
	wg.Wait()

	if wins != 1 {
		t.Errorf("%d canjes con éxito, se esperaba exactamente 1", wins)
	}
	if rejects != racers-1 {
		t.Errorf("%d rechazos, se esperaban %d", rejects, racers-1)
	}
}

func TestConsumeUnknownToken(t *testing.T) {
	r := NewRegistry(clock.NewFake(t0), time.Minute)
	if _, err := r.Consume("no existe"); !errors.Is(err, ErrUnknownTicket) {
		t.Errorf("err = %v, se esperaba ErrUnknownTicket", err)
	}
}

func TestTicketExpires(t *testing.T) {
	clk := clock.NewFake(t0)
	r := NewRegistry(clk, 30*time.Second)

	tk, err := r.Issue()
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	clk.Advance(29 * time.Second)
	if _, err := r.Consume(tk.Token); err != nil {
		t.Fatalf("dentro de plazo: %v", err)
	}

	other, err := r.Issue()
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	clk.Advance(31 * time.Second)
	if _, err := r.Consume(other.Token); !errors.Is(err, ErrTicketExpired) {
		t.Errorf("err = %v, se esperaba ErrTicketExpired", err)
	}
}

// TestSweepKeepsConsumedMarks: la limpieza puede tirar tickets sin usar, pero
// nunca la marca de consumido; si la tirara, un sessionId volvería a valer.
func TestSweepKeepsConsumedMarks(t *testing.T) {
	clk := clock.NewFake(t0)
	r := NewRegistry(clk, 10*time.Second)

	used, _ := r.Issue()
	unused, _ := r.Issue()

	if _, err := r.Consume(used.Token); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if got := r.Pending(); got != 1 {
		t.Errorf("Pending = %d, se esperaba 1", got)
	}

	clk.Advance(time.Hour)
	r.Sweep()

	if got := r.Pending(); got != 0 {
		t.Errorf("Pending = %d tras la limpieza, se esperaba 0", got)
	}
	if !r.Consumed(used.SessionID) {
		t.Error("la limpieza borró la marca de consumido: el sessionId volvería a valer")
	}
	if _, err := r.Consume(used.Token); !errors.Is(err, ErrTicketConsumed) {
		t.Errorf("tras la limpieza, err = %v, se esperaba ErrTicketConsumed", err)
	}
	if _, err := r.Consume(unused.Token); !errors.Is(err, ErrUnknownTicket) {
		t.Errorf("el ticket caducado sin usar debería haberse ido: %v", err)
	}
}

func TestNewRegistryDefaults(t *testing.T) {
	r := NewRegistry(nil, 0)
	tk, err := r.Issue()
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if got := tk.ExpiresAt.Sub(tk.IssuedAt); got != DefaultTicketTTL {
		t.Errorf("TTL por defecto = %v, se esperaba %v", got, DefaultTicketTTL)
	}
}
