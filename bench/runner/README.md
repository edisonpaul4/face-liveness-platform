# runner

Ejecuta los casos de `bench/cases/` contra una instancia de la plataforma y
calcula las métricas PAD.

Lo que debe hacer cuando se implemente:

1. Abrir una sesión real por la API pública (nada de atajos internos: se mide
   el sistema completo, protocolo incluido).
2. Reproducir el material del caso como si fuera una cámara.
3. Registrar la traza WebSocket completa.
4. Verificar `expect_protocol`: que el servidor no filtre el guion. Un fallo
   aquí invalida el resto de métricas del caso.
5. Calcular APCER por familia, BPCER, ACER, tasa de inconclusive y latencias.

El runner es un cliente externo: no importa nada de `gateway/internal` ni de
`orchestrator/internal`.

SCAFFOLDING: sin implementación.
