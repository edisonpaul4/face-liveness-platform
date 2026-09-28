/**
 * Protocolo WebSocket v1. Espejo de /proto/ws/v1 y de
 * gateway/internal/wsproto.
 *
 * Sin DOM: este módulo corre igual en el navegador y en Node, y por eso el
 * banco de pruebas puede ejercitar exactamente el mismo código que se ejecuta
 * en la pestaña.
 */

/** Cabecera binaria de un frame: 24 bytes big-endian. */
export const FRAME_HEADER_BYTES = 24;
export const FRAME_VERSION = 1;

export const ENCODING_JPEG = 1;
export const ENCODING_WEBP = 2;

/** El sello de captura del cliente es utilizable para alinear. */
export const FLAG_CLIENT_CLOCK_TRUSTED = 1 << 0;

export const MSG = {
  clientHello: 'client_hello',
  clientTelemetry: 'client_telemetry',
  clientAbort: 'client_abort',
  serverHello: 'server_hello',
  serverChallenge: 'server_challenge',
  serverChallengeEnd: 'server_challenge_end',
  serverCaptureControl: 'server_capture_control',
  serverResult: 'server_result',
  serverError: 'server_error',
};

/** Tipos de reto que puede pedir el servidor. */
export const CHALLENGE = {
  calibration: 'calibration',
  pose: 'pose',
  flash: 'flash',
  gaze: 'gaze',
  hold: 'hold',
};

/**
 * Serializa un frame para el bus.
 *
 * `capturedAtUs` es el reloj del CLIENTE y el servidor no lo usa para
 * puntuar: sella cada frame con el suyo al recibirlo. Va igualmente porque
 * sirve para alinear frames entre sí.
 */
export function encodeFrame({
  seq,
  jpeg,
  capturedAtUs,
  clockTrusted = true,
  encoding = ENCODING_JPEG,
}) {
  const out = new Uint8Array(FRAME_HEADER_BYTES + jpeg.length);
  const view = new DataView(out.buffer);

  view.setUint8(0, FRAME_VERSION);
  view.setUint8(1, encoding);
  view.setUint16(2, clockTrusted ? FLAG_CLIENT_CLOCK_TRUSTED : 0, false);
  view.setBigUint64(4, BigInt(seq), false);
  view.setBigInt64(12, BigInt(Math.round(capturedAtUs)), false);
  view.setUint32(20, jpeg.length, false);

  out.set(jpeg, FRAME_HEADER_BYTES);
  return out;
}

/** Saludo inicial. El token es de un solo uso. */
export function clientHello(sessionToken, capabilities = {}) {
  return JSON.stringify({
    type: MSG.clientHello,
    protocol_version: 1,
    session_token: sessionToken,
    capabilities: { encodings: ['webp', 'jpeg'], max_fps: 30, max_width: 1280, max_height: 720, flash: true, ...capabilities },
  });
}

/**
 * Telemetría de captura.
 *
 * NO es un reporte de cumplimiento: el cliente nunca dice haber superado un
 * reto. Son pistas sobre la cámara, y quien decide es el servidor.
 */
export function clientTelemetry(payload) {
  return JSON.stringify({ type: MSG.clientTelemetry, ...payload });
}

export function clientAbort(reason) {
  return JSON.stringify({ type: MSG.clientAbort, reason });
}

/** Códigos de cierre de la aplicación. Ver /proto/ws/v1/README.md. */
export const CLOSE = {
  4000: 'sesión completada',
  4001: 'sesión expirada',
  4002: 'violación de protocolo',
  4003: 'frame demasiado grande',
  4004: 'exceso de mensajes de control',
  4005: 'no autorizado',
  4006: 'error interno',
  4007: 'cliente lento',
  4008: 'abandono del cliente',
  4009: 'error de infraestructura',
};

export function describeClose(code) {
  return CLOSE[code] ?? `código ${code}`;
}
