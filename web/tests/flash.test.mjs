/**
 * Lo que se prueba aquí es la parte del cliente que puede equivocarse en
 * silencio: los tiempos del destello. Un color que dura menos de lo debido, o
 * una secuencia vieja que sigue pintando encima de la nueva, no se ven en la
 * pantalla y sí arruinan la medición.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { MIN_SEGMENT_MS, PALETTE, enforceMinimum, playSequence } from '../public/lib/flash.js';

/** Reloj y sueño falsos: el tiempo avanza sólo cuando alguien duerme. */
function fakeClock() {
  let t = 0;
  return {
    now: () => t,
    sleep: async (ms) => { t += ms; },
    advance: (ms) => { t += ms; },
  };
}

/** Pintor que registra qué se pintó y cuándo, y tarda en componer. */
function recorder(clock, compositeMs = 16) {
  const log = [];
  return {
    log,
    painter: async (color) => {
      clock.advance(compositeMs);
      log.push({ color, at: clock.now() });
      return clock.now();
    },
  };
}

test('un tramo por debajo del mínimo se alarga y se declara', () => {
  const { sequence, adjustments } = enforceMinimum([
    { color: 'white', duration_ms: 120 },
    { color: 'red', duration_ms: 500 },
  ]);
  assert.equal(sequence[0].duration_ms, MIN_SEGMENT_MS);
  assert.equal(sequence[1].duration_ms, 500);
  assert.deepEqual(adjustments, [{ index: 0, color: 'white', requested: 120, applied: MIN_SEGMENT_MS }]);
});

test('ningún color puede estar en pantalla menos del mínimo', async () => {
  const clock = fakeClock();
  const { painter, log } = recorder(clock);
  await playSequence(
    [{ color: 'white', duration_ms: 10 }, { color: 'red', duration_ms: 10 }],
    painter,
    { now: clock.now, sleep: clock.sleep },
  );
  // El último apagado no cuenta como color.
  const colores = log.filter((entry) => entry.color !== null);
  for (let i = 1; i < log.length; i++) {
    assert.ok(log[i].at - log[i - 1].at >= MIN_SEGMENT_MS,
      `el color ${colores[i - 1].color} duró ${log[i].at - log[i - 1].at} ms`);
  }
});

test('el tramo se cuenta desde que se pintó, no desde que se decidió', async () => {
  const clock = fakeClock();
  // Composición lentísima: si el tiempo se contase desde la decisión, el
  // color estaría en pantalla 400-300 = 100 ms.
  const { painter, log } = recorder(clock, 300);
  await playSequence([{ color: 'white', duration_ms: 400 }], painter,
    { now: clock.now, sleep: clock.sleep });
  assert.equal(log[1].at - log[0].at, 400 + 300);
});

test('devuelve el instante REAL de pintado, posterior a la decisión', async () => {
  const clock = fakeClock();
  const { painter } = recorder(clock, 25);
  const { painted } = await playSequence([{ color: 'white', duration_ms: 400 }], painter,
    { now: clock.now, sleep: clock.sleep });
  assert.equal(painted[0].paint_lag_ms, 25);
  assert.ok(painted[0].painted_at_ms > painted[0].decided_at_ms);
});

test('una secuencia que deja de ser la vigente no vuelve a tocar la pantalla', async () => {
  // Regresión. El apagado final de la secuencia anterior caía dentro del reto
  // siguiente y borraba su primer color: el servidor lo medía como un sujeto
  // que no responde al destello, y la sesión salía "inconclusive".
  const clock = fakeClock();
  const { painter, log } = recorder(clock);
  let vigente = true;

  const corriendo = playSequence(
    [{ color: 'white', duration_ms: 400 }, { color: 'red', duration_ms: 400 }],
    painter,
    { now: clock.now, sleep: clock.sleep, alive: () => vigente },
  );
  // Se cierra el reto nada más empezar el primer color.
  vigente = false;
  const { painted, abandoned } = await corriendo;

  assert.equal(abandoned, true);
  assert.equal(painted.length, 1, 'no debe pintar el segundo color');
  assert.deepEqual(log.map((e) => e.color), [PALETTE.white],
    'ni siquiera el apagado: lo hace el cierre de reto');
});

test('una secuencia completa sí se apaga sola', async () => {
  const clock = fakeClock();
  const { painter, log } = recorder(clock);
  const { abandoned } = await playSequence([{ color: 'blue', duration_ms: 400 }], painter,
    { now: clock.now, sleep: clock.sleep });
  assert.equal(abandoned, false);
  assert.equal(log.at(-1).color, null);
});

test('si la pantalla no llega a componer, no se inventa un instante de pintado', async () => {
  // Pestaña en segundo plano: requestAnimationFrame no corre y el pintor
  // devuelve null. Rellenar ahí un timestamp plausible sería mentirle al
  // analizador, que correlaciona su búsqueda de retardo contra este número.
  const clock = fakeClock();
  const { painted, abandoned, stalled } = await playSequence(
    [{ color: 'white', duration_ms: 400 }, { color: 'red', duration_ms: 400 }],
    async () => null,
    { now: clock.now, sleep: clock.sleep },
  );
  assert.equal(stalled, true);
  assert.equal(abandoned, true);
  assert.deepEqual(painted, []);
});
