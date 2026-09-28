// Package storetest levanta las tiendas contra la infraestructura local.
//
// Sólo lo usan los tests. Se salta el test si no hay infraestructura a mano:
// `make up` la levanta. Probar la persistencia contra dobles no prueba nada —
// los triggers append-only, el TTL de Redis y el cifrado en el almacén son
// justo lo que hay que verificar de verdad.
package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/edisonpaul4/biometrics/orchestrator/core/clock"
	"github.com/edisonpaul4/biometrics/orchestrator/internal/store/evidence"
	"github.com/edisonpaul4/biometrics/orchestrator/internal/store/hot"
	"github.com/edisonpaul4/biometrics/orchestrator/internal/store/results"
)

// postgresCandidates son los sitios donde suele estar el Postgres local. El
// docker-compose usa 5433 cuando el 5432 está ocupado por otro proyecto.
var postgresCandidates = []string{
	"postgres://liveness:liveness@localhost:5433/liveness?sslmode=disable",
	"postgres://liveness:liveness@localhost:5432/liveness?sslmode=disable",
}

// Results abre la tienda de resultados, o se salta el test.
func Results(t *testing.T) *results.Store {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	for _, dsn := range postgresCandidates {
		store, err := results.Open(ctx, dsn)
		if err == nil {
			t.Cleanup(store.Close)
			return store
		}
	}
	t.Skip("no hay Postgres a mano; levántalo con `make up`")
	return nil
}

// Hot abre el estado caliente, o se salta el test.
func Hot(t *testing.T, ttl time.Duration) *hot.Store {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	store, err := hot.Open(ctx, "redis://localhost:6379/1", ttl)
	if err != nil {
		t.Skip("no hay Redis a mano; levántalo con `make up`")
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestKey es la clave AES de los tests. No sale de aquí.
var TestKey = []byte("clave-de-pruebas-de-32-bytes!!!!")

// Evidence abre el almacén de evidencia, o se salta el test.
func Evidence(t *testing.T, clk clock.Clock) *evidence.Store {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	store, err := evidence.Open(ctx, evidence.Config{
		Endpoint:  "localhost:9000",
		AccessKey: "minioadmin",
		SecretKey: "minioadmin",
		Bucket:    "liveness-evidence-test",
		Key:       TestKey,
		KeyID:     "test-key-1",
	}, clk)
	if err != nil {
		t.Skipf("no hay MinIO a mano (%v); levántalo con `make up`", err)
	}
	return store
}
