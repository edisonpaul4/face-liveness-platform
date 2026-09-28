package api_test

import (
	"errors"

	"github.com/coder/websocket"
)

func asCloseError(err error, target *websocket.CloseError) bool {
	return errors.As(err, target)
}
