module github.com/edisonpaul4/biometrics/gateway

go 1.23

// El gateway consume el NÚCLEO PÚBLICO del orquestador
// (orchestrator/core/...), nunca sus paquetes internal.
//
// Es un atajo consciente del vertical slice: en la arquitectura final la
// máquina de estados corre en el proceso orchestrator y el gateway habla con
// ella por NATS. Por eso la conexión sólo la usa a través de la interfaz
// conn.Engine, que es la costura por donde entrará el transporte.
require github.com/edisonpaul4/biometrics/orchestrator v0.0.0

replace github.com/edisonpaul4/biometrics/orchestrator => ../orchestrator

require (
	github.com/coder/websocket v1.8.12 // terminación WebSocket
	github.com/nats-io/nats.go v1.37.0 // bus con los workers de análisis
	github.com/oklog/ulid/v2 v2.1.0    // session_id
)

// Servidor NATS embebido: sólo lo usan los tests (internal/bus/natstest), para
// que probar el bus sea probar el bus de verdad y no un doble complaciente.
require github.com/nats-io/nats-server/v2 v2.10.22

// Dependencias declaradas para fijar el stack. Todavía SIN USAR.
require (
	github.com/prometheus/client_golang v1.20.5 // exposición de métricas
	github.com/redis/go-redis/v9 v9.7.0         // estado caliente de sesión
	google.golang.org/protobuf v1.35.2          // contratos del bus
)

require (
	github.com/klauspost/compress v1.17.11 // indirect
	github.com/minio/highwayhash v1.0.3 // indirect
	github.com/nats-io/jwt/v2 v2.5.8 // indirect
	github.com/nats-io/nkeys v0.4.7 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	go.uber.org/automaxprocs v1.6.0 // indirect
	golang.org/x/crypto v0.28.0 // indirect
	golang.org/x/sys v0.26.0 // indirect
	golang.org/x/time v0.7.0 // indirect
)
