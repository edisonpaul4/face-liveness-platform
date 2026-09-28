module github.com/edisonpaul4/biometrics/orchestrator

go 1.23

// Dependencias declaradas para fijar el stack. Todavía SIN USAR (scaffolding).
// Ejecuta `make deps` para poblar go.sum antes del primer build real.
require (
	github.com/go-chi/chi/v5 v5.1.0 // API REST de resultados
	github.com/jackc/pgx/v5 v5.7.1 // Postgres (resultados)
	github.com/minio/minio-go/v7 v7.0.80 // MinIO (evidencia)
	github.com/nats-io/nats.go v1.37.0 // bus NATS core
	github.com/oklog/ulid/v2 v2.1.0 // session_id
	github.com/prometheus/client_golang v1.20.5 // métricas
	github.com/redis/go-redis/v9 v9.7.0 // estado caliente de sesión
	google.golang.org/protobuf v1.35.2 // contratos del bus
)

require gopkg.in/yaml.v3 v3.0.1 // perfil de decisión

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-ini/ini v1.67.0 // indirect
	github.com/goccy/go-json v0.10.3 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/klauspost/compress v1.17.11 // indirect
	github.com/klauspost/cpuid/v2 v2.2.8 // indirect
	github.com/minio/md5-simd v1.1.2 // indirect
	github.com/rs/xid v1.6.0 // indirect
	golang.org/x/crypto v0.28.0 // indirect
	golang.org/x/net v0.30.0 // indirect
	golang.org/x/sys v0.26.0 // indirect
	golang.org/x/text v0.19.0 // indirect
)
