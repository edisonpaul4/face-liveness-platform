/**
 * Estado de la sesión en el cliente.
 *
 * Contiene exactamente lo que el cliente tiene derecho a saber:
 * el reto activo, el estado de la cámara y el resultado final.
 *
 * NO contiene: longitud del guion, índice del reto, retos futuros, semilla,
 * scores parciales ni umbrales (CLAUDE.md §6).
 *
 * SCAFFOLDING: sin implementación.
 */

import type { ActiveChallenge, SessionResult } from '$lib/liveness/client';

export interface SessionState {
	phase: 'idle' | 'connecting' | 'streaming' | 'finished' | 'error';
	activeChallenge: ActiveChallenge | null;
	result: SessionResult | null;
}

export const initialState: SessionState = {
	phase: 'idle',
	activeChallenge: null,
	result: null
};
