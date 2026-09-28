package wsproto

// Códigos de cierre WebSocket de la aplicación.
//
// Se usa el rango privado 4000-4999 incluso para el cierre correcto: toda
// desconexión lleva un motivo explícito y comprobable. Un 1000 pelado no
// distingue "sesión completada" de "el navegador cerró la pestaña".
const (
	// CloseSessionComplete: la sesión terminó y el veredicto ya se envió.
	CloseSessionComplete = 4000
	// CloseSessionExpired: se agotó el presupuesto de la sesión.
	CloseSessionExpired = 4001
	// CloseProtocolViolation: mensaje mal formado, fuera de orden o de tipo
	// inesperado.
	CloseProtocolViolation = 4002
	// CloseFrameTooLarge: un frame superó el límite duro de tamaño.
	CloseFrameTooLarge = 4003
	// CloseRateLimited: el cliente ignoró el control de caudal de forma
	// sostenida.
	CloseRateLimited = 4004
	// CloseUnauthorized: token ausente, desconocido o ya consumido.
	CloseUnauthorized = 4005
	// CloseInternalError: fallo del servidor.
	CloseInternalError = 4006
	// CloseSlowClient: el cliente no consume lo que se le envía.
	CloseSlowClient = 4007
	// CloseClientAbort: el cliente abandonó.
	CloseClientAbort = 4008
	// CloseInfrastructureError: se cayó una pieza nuestra —el analizador— y
	// la sesión no puede continuar. El fallo no es del cliente y reintentar
	// tiene sentido.
	CloseInfrastructureError = 4009
)

// ErrorCode es el código de un mensaje server_error. Enum cerrado del
// contrato /proto/ws/v1.
type ErrorCode string

// Códigos de error del protocolo.
const (
	ErrorInvalidProtocol ErrorCode = "invalid_protocol"
	ErrorUnauthorized    ErrorCode = "unauthorized"
	ErrorSessionExpired  ErrorCode = "session_expired"
	ErrorRateLimited     ErrorCode = "rate_limited"
	ErrorCapacity        ErrorCode = "capacity"
	ErrorInternal        ErrorCode = "internal"
)
