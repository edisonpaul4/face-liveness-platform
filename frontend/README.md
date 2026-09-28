# frontend — Svelte

Aplicación cliente de liveness (SvelteKit + Svelte 5 + TypeScript).

```bash
cd frontend
npm install
npm run dev      # http://localhost:5173, con proxy /ws → gateway y /api → orchestrator
```

## Reglas del cliente

El cliente es **hostil por definición**: cualquier cosa que sepa, la sabe el
atacante. Por eso:

- No conoce el guion de retos: recibe uno a la vez (CLAUDE.md §6).
- No hay progreso "3 de 5": el cliente no sabe ni el 3 ni el 5.
- No decide nada: sólo captura, envía frames y muestra lo que el servidor
  indica.
- No contiene umbrales, pesos ni catálogos de decisión.
- El color de la iluminación activa lo impone el servidor en el reto; el
  cliente nunca lo elige ni lo anticipa.

## Estructura

```
src/routes/                 pantallas
src/lib/components/         CameraView, ChallengePrompt
src/lib/liveness/client.ts  protocolo WS v1 (tipos + conexión)
src/lib/stores/session.ts   estado permitido en el cliente
```

SCAFFOLDING: los componentes existen y documentan las reglas; no implementan
captura ni conexión.
