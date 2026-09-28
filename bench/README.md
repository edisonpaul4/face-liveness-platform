# bench — banco de pruebas de ataques de presentación

Mide si el sistema distingue una persona real de los ataques del modelo de
amenaza (CLAUDE.md §5), y si sigue dejando pasar a los usuarios legítimos.

## Métricas (ISO/IEC 30107-3)

| Métrica | Qué mide | Objetivo |
|---------|----------|----------|
| **APCER** | % de ataques clasificados como reales (falsos aceptados) | mínimo, por familia de ataque |
| **BPCER** | % de presentaciones reales rechazadas (fricción legítima) | bajo, o la gente no pasa |
| **ACER**  | media de ambas | resumen |
| **Inconclusive rate** | % de sesiones sin veredicto | vigilado: esconder fallos aquí es trampa |
| **p95 de latencia de sesión** | tiempo hasta veredicto | presupuesto de producto |

Regla: **APCER se reporta por familia** (A1, A2, A3), nunca agregado. Un
agregado bueno puede ocultar que A3 pasa siempre.

## Estructura

```
cases/      escenarios declarativos: qué ataque, con qué material, qué se espera
fixtures/   material de prueba (NO se commitean datos biométricos reales)
runner/     ejecutor: reproduce el caso contra el sistema y calcula métricas
```

## Reglas de datos

- Nada de rostros reales de terceros en el repositorio.
- `fixtures/` contiene material sintético o con consentimiento explícito
  documentado en el propio caso.
- Los ficheros pesados van al bucket `liveness-bench` de MinIO, referenciados
  por clave desde el caso, no versionados aquí.

SCAFFOLDING: los casos describen el diseño experimental; el runner no está
implementado.
