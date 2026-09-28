package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/edisonpaul4/biometrics/gateway/internal/api"
)

const origenCliente = "http://localhost:5173"

// El cliente de pruebas y el frontend viven en otro puerto que el gateway, así
// que sin esto el navegador ni siquiera llega a pedir la sesión.
func TestOrigenAutorizadoRecibeCabeceras(t *testing.T) {
	srv, _ := newTestServer(t, func(cfg *api.Config) {
		cfg.AllowedOrigins = []string{origenCliente}
	})

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/sessions", nil)
	req.Header.Set("Origin", origenCliente)
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()

	if got := res.Header.Get("Access-Control-Allow-Origin"); got != origenCliente {
		t.Errorf("Allow-Origin = %q, se esperaba %q", got, origenCliente)
	}
	if !strings.Contains(res.Header.Get("Vary"), "Origin") {
		t.Error("falta Vary: Origin — una caché serviría la respuesta de un origen a otro")
	}
}

// Devolver el origen que venga es como una API acaba abierta a cualquiera sin
// que nadie lo haya decidido.
func TestOrigenNoAutorizadoNoRecibeCabeceras(t *testing.T) {
	srv, _ := newTestServer(t, func(cfg *api.Config) {
		cfg.AllowedOrigins = []string{origenCliente}
	})

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/sessions", nil)
	req.Header.Set("Origin", "http://evil.example")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()

	if got := res.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q, no debería haber ninguna", got)
	}
}

// Sin lista configurada el gateway se comporta como siempre: mismo origen.
func TestSinListaNoHayCORS(t *testing.T) {
	srv, _ := newTestServer(t, nil)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/sessions", nil)
	req.Header.Set("Origin", origenCliente)
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()

	if got := res.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q sin lista configurada", got)
	}
}

func TestPreflight(t *testing.T) {
	srv, _ := newTestServer(t, func(cfg *api.Config) {
		cfg.AllowedOrigins = []string{origenCliente}
	})

	for _, c := range []struct {
		nombre string
		origen string
		quiero int
	}{
		{"autorizado", origenCliente, http.StatusNoContent},
		{"desconocido", "http://evil.example", http.StatusForbidden},
	} {
		t.Run(c.nombre, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/v1/sessions", nil)
			req.Header.Set("Origin", c.origen)
			req.Header.Set("Access-Control-Request-Method", "POST")
			res, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("OPTIONS: %v", err)
			}
			defer res.Body.Close()

			if res.StatusCode != c.quiero {
				t.Errorf("estado = %d, se esperaba %d", res.StatusCode, c.quiero)
			}
		})
	}
}
