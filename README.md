# Plataforma de liveness detection

Prueba de vida facial (face liveness / PAD) con retos activos, equivalente en
alcance a AWS Rekognition Face Liveness.

> **Lee `CLAUDE.md` antes de tocar nada.** Es el documento normativo:
> arquitectura, frontera Go/Python, modelo de amenaza, convenciones y la regla
> del guion de retos.

Estado: **scaffolding**. Sin lógica de negocio.

## Arranque

```bash
cp .env.example .env
make up      # NATS, Redis, Postgres, MinIO
make venv    # entorno del analyzer
make models  # modelos de detección (no se versionan)
make test
make down
```

## Estructura

| Ruta | Qué es |
|------|--------|
| `gateway/` | Go — WebSocket, sesión de transporte, publicación en el bus |
| `orchestrator/` | Go — FSM, retos, fusión, veredicto, persistencia, API de control |
| `analyzer/` | Python — visión por computador; hoy: detección de rostro y calidad |
| `proto/` | contratos compartidos (bus protobuf + protocolo cliente JSON Schema) |
| `frontend/` | aplicación Svelte (SvelteKit) |
| `web/` | cliente de prueba mínimo, sin framework |
| `bench/` | banco de pruebas de ataques y métricas PAD |
| `deploy/` | configuración de infraestructura |

## API de control

```
POST /v1/sessions                     crea sesión → sessionId + token de WebSocket
GET  /v1/sessions/{id}                resultado, motivos y estado de la biometría
GET  /v1/sessions/{id}/timeline       features (410 Gone si ya caducó)
GET  /v1/sessions/{id}/retention      recibos de borrado
GET  /v1/sessions?outcome=&subject_id=&reason_code=&from=&to=   listado
```

Todo bajo `Authorization: Bearer <clave>`.

## Las cuatro reglas que no se negocian

1. **Go decide, Python mide.** El analyzer no conoce sesiones, retos ni
   veredictos. Ver `CLAUDE.md` §3.
2. **El cliente nunca conoce el guion de retos.** Un reto a la vez, sin índice,
   sin total, sin futuro. Ver `CLAUDE.md` §6.
3. **El bus de frames es efímero.** NATS core, sin JetStream, sin reintentos.
   La sesión se ata a un worker con un lease; si el worker cae, se aborta y se
   reintenta desde cero. Ver `CLAUDE.md` §4.
4. **La biometría caduca; la auditoría se queda.** Frames y clip a 24 h,
   cifrados en cliente; features a 72 h. El veredicto y sus motivos se
   conservan sin la cara. Borrado automático y verificable. Ver `CLAUDE.md`
   §6bis.

## Targets

```
make up     levanta la infraestructura
make down   la baja
make test   tests de todos los componentes
make lint   linters de todos los componentes
make help   resto de targets
```

`test` y `lint` se saltan con aviso los componentes cuyo toolchain no esté
instalado, para que un clon limpio siempre pueda ejecutarlos.
