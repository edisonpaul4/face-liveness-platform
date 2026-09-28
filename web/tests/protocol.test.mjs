/**
 * El cliente escribe la cabecera binaria a mano. Si un desplazamiento se
 * mueve, el gateway lee basura y el fallo aparece muy lejos de aquí, así que
 * se comprueba byte a byte contra el reparto de gateway/internal/wsproto.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import {
  FRAME_HEADER_BYTES,
  FLAG_CLIENT_CLOCK_TRUSTED,
  MSG,
  clientHello,
  clientTelemetry,
  clientAbort,
  describeClose,
  encodeFrame,
} from '../public/lib/protocol.js';

const JPEG = new Uint8Array([0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10]);

test('la cabecera va donde el gateway la busca', () => {
  const out = encodeFrame({ seq: 42, jpeg: JPEG, capturedAtUs: 1787500000000000 });
  const view = new DataView(out.buffer);

  assert.equal(out.length, FRAME_HEADER_BYTES + JPEG.length);
  assert.equal(view.getUint8(0), 1, 'versión');
  assert.equal(view.getUint8(1), 1, 'codificación JPEG');
  assert.equal(view.getUint16(2, false), FLAG_CLIENT_CLOCK_TRUSTED);
  assert.equal(view.getBigUint64(4, false), 42n, 'seq');
  assert.equal(view.getBigInt64(12, false), 1787500000000000n, 'captura del cliente');
  assert.equal(view.getUint32(20, false), JPEG.length, 'longitud declarada');
  assert.deepEqual(out.slice(FRAME_HEADER_BYTES), JPEG);
});

test('sin reloj de confianza el flag se apaga', () => {
  const out = encodeFrame({ seq: 1, jpeg: JPEG, capturedAtUs: 0, clockTrusted: false });
  assert.equal(new DataView(out.buffer).getUint16(2, false), 0);
});

test('todo lo big-endian, nunca el orden de la máquina', () => {
  const out = encodeFrame({ seq: 0x0102030405060708n, jpeg: JPEG, capturedAtUs: 0 });
  assert.deepEqual([...out.slice(4, 12)], [1, 2, 3, 4, 5, 6, 7, 8]);
});

test('el saludo lleva el token y nada más que identifique a nadie', () => {
  const hello = JSON.parse(clientHello('tok-123'));
  assert.equal(hello.type, MSG.clientHello);
  assert.equal(hello.session_token, 'tok-123');
  assert.equal(hello.protocol_version, 1);
});

test('la telemetría y el aborto llevan su tipo', () => {
  assert.equal(JSON.parse(clientTelemetry({ actual_fps: 15 })).type, MSG.clientTelemetry);
  assert.equal(JSON.parse(clientAbort('camera_denied')).type, MSG.clientAbort);
});

test('los códigos de cierre se traducen a algo legible', () => {
  assert.match(describeClose(4000), /completa/i);
  assert.equal(typeof describeClose(4999), 'string');
});
