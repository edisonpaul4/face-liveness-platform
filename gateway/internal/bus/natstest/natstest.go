// Package natstest levanta un servidor NATS embebido para los tests.
//
// Sólo lo usan los tests. Existe para que probar el bus sea probar el bus de
// verdad —conexiones, subjects, request/reply— y no un doble de mentira que
// se comporta como uno quiere.
package natstest

import (
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// Start arranca un NATS core en un puerto libre y devuelve su URL. El
// servidor se apaga al terminar el test.
//
// Sin JetStream, igual que en producción: el camino de frames es efímero.
func Start(t *testing.T) string {
	t.Helper()

	srv, err := server.NewServer(&server.Options{
		Host:      "127.0.0.1",
		Port:      -1, // puerto libre
		NoLog:     true,
		NoSigs:    true,
		JetStream: false,
	})
	if err != nil {
		t.Fatalf("natstest: no se pudo crear el servidor: %v", err)
	}

	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		srv.Shutdown()
		t.Fatal("natstest: el servidor no arrancó a tiempo")
	}
	t.Cleanup(srv.Shutdown)

	return srv.ClientURL()
}

// Connect abre una conexión al servidor y la cierra al terminar el test.
func Connect(t *testing.T, url string) *nats.Conn {
	t.Helper()

	nc, err := nats.Connect(url,
		nats.Timeout(2*time.Second),
		nats.ReconnectWait(100*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("natstest: no se pudo conectar a %s: %v", url, err)
	}
	t.Cleanup(nc.Close)
	return nc
}
