package conn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/coder/websocket"

	"github.com/edisonpaul4/biometrics/gateway/internal/wsproto"
)

// Errores del saludo inicial.
var (
	ErrHandshakeTooLarge  = errors.New("conn: client_hello demasiado grande")
	ErrHandshakeType      = errors.New("conn: se esperaba client_hello")
	ErrHandshakeVersion   = errors.New("conn: versión de protocolo no soportada")
	ErrHandshakeMalformed = errors.New("conn: client_hello mal formado")
)

// Handshake lee el saludo inicial del cliente.
//
// Se ejecuta ANTES de arrancar las goroutines de la conexión, de forma
// secuencial: es la única lectura del socket que no hace readLoop, y por eso
// no rompe la regla de un solo lector.
//
// El token que trae este mensaje es lo que autoriza la sesión; hasta
// canjearlo no se genera guion ni se gasta nada.
func Handshake(ctx context.Context, ws *websocket.Conn, limits Limits) (wsproto.ClientHello, error) {
	ctx, cancel := context.WithTimeout(ctx, limits.HandshakeTimeout)
	defer cancel()

	typ, data, err := ws.Read(ctx)
	if err != nil {
		return wsproto.ClientHello{}, fmt.Errorf("conn: no llegó el saludo: %w", err)
	}
	if typ != websocket.MessageText {
		return wsproto.ClientHello{}, ErrHandshakeType
	}
	if len(data) > limits.MaxControlBytes {
		return wsproto.ClientHello{}, ErrHandshakeTooLarge
	}

	var hello wsproto.ClientHello
	if err := json.Unmarshal(data, &hello); err != nil {
		return wsproto.ClientHello{}, fmt.Errorf("%w: %v", ErrHandshakeMalformed, err)
	}
	if hello.Type != wsproto.TypeClientHello {
		return wsproto.ClientHello{}, ErrHandshakeType
	}
	if hello.ProtocolVersion != wsproto.ProtocolVersion {
		return wsproto.ClientHello{}, fmt.Errorf("%w: %d", ErrHandshakeVersion, hello.ProtocolVersion)
	}
	if hello.SessionToken == "" {
		return wsproto.ClientHello{}, fmt.Errorf("%w: sin token", ErrHandshakeMalformed)
	}
	return hello, nil
}
