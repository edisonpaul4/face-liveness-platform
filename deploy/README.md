# deploy

Infraestructura local y configuración de despliegue.

```
nats/nats.conf              NATS core, sin JetStream (a propósito)
postgres/initdb/            esquema: auditoría append-only + biometría con TTL
minio/create-buckets.sh     buckets de evidencia y bench
policy/decision-profile.yaml  perfil de decisión: pesos, umbrales y reintentos
local/policy.example.yaml   generación del guion de retos
```

## Retención

El esquema separa lo que se conserva de lo que caduca:

* `liveness.sessions` y `liveness.retention_events` son **append-only** de
  verdad: triggers que rechazan UPDATE y DELETE. Un resultado que se puede
  reescribir no sirve para auditar.
* `liveness.session_timelines` y `liveness.evidence_objects` son dato
  biométrico. Admiten DELETE —es el mecanismo de retención— pero no UPDATE,
  para que nadie altere una huella y luego "verifique" un borrado contra ella.

El job de retención del orquestador borra lo vencido cada 15 minutos y deja
recibo verificado de cada borrado. Ver `CLAUDE.md` §6bis.

El `docker-compose.yml` está en la raíz del repositorio y consume estos ficheros.

## Producción (pendiente)

Fuera del alcance del scaffolding. Notas de diseño a respetar cuando llegue:

- El analyzer escala añadiendo workers: se anuncian solos y el gateway les
  manda sesiones según carga. No hay que reconfigurar nada al añadir o quitar
  uno; el que deja de anunciarse deja de recibir sesiones.
- El gateway es stateless salvo por las conexiones vivas; el orchestrator
  guarda su estado en Redis.
- Buckets de evidencia con cifrado en reposo y ciclo de vida de retención.
