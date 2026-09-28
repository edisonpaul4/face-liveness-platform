/**
 * Cliente del protocolo WebSocket v1.
 *
 * Contrato: /proto/ws/v1/client_protocol.schema.json
 *
 * Reglas (CLAUDE.md §6):
 *  · Sólo existe un reto activo. No hay cola, ni buffer, ni prefetch de retos.
 *  · El cliente no informa de cumplimiento de retos: sólo envía frames.
 *  · Ningún umbral ni constante de negocio vive aquí.
 *
 * SCAFFOLDING: sin implementación.
 */

export type ChallengeKind = 'calibration' | 'pose' | 'flash';

export type PoseAction = 'yaw_left' | 'yaw_right' | 'pitch_up' | 'move_closer';

/** Paleta de destello: blanco de referencia + primarios saturados. */
export type FlashColor = 'white' | 'red' | 'green' | 'blue';

export interface FlashSegment {
	color: FlashColor;
	duration_ms: number;
}

export type ChallengeParams =
	| { hold_ms: number }
	| { action: PoseAction }
	| { sequence: FlashSegment[] };

/**
 * El único reto que el cliente conoce: el activo.
 *
 * `deadline_ms` es el plazo máximo. El servidor NO manda la ventana mínima de
 * reacción: saber a partir de qué milisegundo una respuesta deja de ser
 * sospechosa es exactamente lo que un atacante necesita.
 */
export interface ActiveChallenge {
	challenge_id: string;
	kind: ChallengeKind;
	params?: ChallengeParams;
	deadline_ms: number;
	prompt_key?: string;
}

export type Decision = 'live' | 'spoof' | 'inconclusive';

export interface SessionResult {
	session_id: string;
	decision: Decision;
	reason_key?: string;
	reference_id?: string;
}

/** Cabecera binaria de frame: 16 bytes big-endian. Ver /proto/ws/v1/README.md */
export const FRAME_HEADER_BYTES = 16;

export function connect(_url: string, _sessionToken: string): never {
	throw new Error('scaffolding: cliente WebSocket no implementado');
}
