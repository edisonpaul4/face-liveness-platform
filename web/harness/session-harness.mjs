/**
 * Banco de pruebas del cliente.
 *
 * Importa EXACTAMENTE los mismos módulos que carga el navegador —protocolo,
 * política de calidad, secuenciador de destellos y conducción de sesión— y los
 * ejerce contra el gateway de verdad. Lo único que se sustituye es lo que un
 * proceso sin pantalla ni cámara no puede tener:
 *
 *   · la cámara, por los frames sintéticos de harness/fixtures;
 *   · la pantalla, por un pintor que anota cuándo "pintó" y con qué retardo.
 *
 * Así lo único que queda sin probar de forma automática es el pegamento del
 * navegador: getUserMedia, el canvas y MediaPipe WASM.
 *
 * El guion de retos NO se puede fijar desde aquí: la semilla vive en el
 * servidor y no viaja (CLAUDE.md §6). El banco, por tanto, juega las sesiones
 * que le toquen y repite hasta conseguir una que su maniquí sepa responder
 * entera; las que no, las declara en voz alta en vez de darlas por buenas.
 *
 *   node harness/session-harness.mjs [http://localhost:8080]
 */

import { readFileSync, existsSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import { runSession } from '../public/lib/session.js';
import { qualityFor, QUALITY } from '../public/lib/quality.js';
import { MIN_SEGMENT_MS, PALETTE } from '../public/lib/flash.js';

const BASE = (process.argv[2] ?? 'http://localhost:8080').replace(/\/$/, '');
const FIXTURES = join(dirname(fileURLToPath(import.meta.url)), 'fixtures');
const MAX_ATTEMPTS = 8;
const COMPOSITE_MS = 16; // lo que tarda un frame en llegar a la pantalla

/**
 * Retos que el maniquí NO sabe responder.
 *
 * `pitch_up`: una cabeza dibujada deja de ser detectable antes de que el pitch
 * medido llegue al umbral.
 *
 * `gaze`: MediaPipe sigue la pupila dibujada en horizontal, pero en vertical
 * apenas se mueve —medido: 0,05 de desvío como techo, contra los 0,06 que
 * pide el servidor—. Hacer que llegue exigiría un renderizador de ojos, y a
 * esas alturas ya no se estaría probando el producto.
 *
 * Son limitaciones del maniquí, no del producto: se declaran y se reintenta.
 * Para que el banco cubra el resto sin descartar casi todo, arranca el
 * gateway con `make gateway SKIP=gaze`.
 */
const OUT_OF_REACH = new Set(['pitch_up', 'gaze']);

const cache = new Map();
function fixture(name) {
  if (!cache.has(name)) {
    const path = join(FIXTURES, `${name}.jpg`);
    if (!existsSync(path)) {
      throw new Error(`falta ${name}.jpg — genera los frames con: python3 web/harness/make-fixtures.py`);
    }
    cache.set(name, new Uint8Array(readFileSync(path)));
  }
  return cache.get(name);
}

const COLOR_TO_FIXTURE = Object.fromEntries(
  Object.entries(PALETTE).map(([name, css]) => [css, `flash_${name}`]),
);

/** Lo que la "pantalla" está emitiendo ahora mismo. La cámara lo ve. */
let painting = null;
let grabSeq = 0;

/** Pintor de mentira: devuelve el instante real, como haría el doble rAF. */
async function painter(color) {
  painting = color;
  if (color === null) return performance.now();
  await new Promise((resolve) => setTimeout(resolve, COMPOSITE_MS));
  return performance.now();
}

/** Cámara de mentira: devuelve el frame que correspondería a lo que pasa. */
function makeSource(getChallenge) {
  return {
    async grab() {
      const name = pick(getChallenge());
      if (process.env.DEBUG_GRAB) {
        console.error(`GRAB seq=${++grabSeq} t=${Math.round(performance.now())} -> ${name}`);
      }
      return { jpeg: fixture(name), capturedAtUs: Date.now() * 1000 };
    },
  };
}

function pick(challenge) {
  if (painting !== null) return COLOR_TO_FIXTURE[painting] ?? 'flash_white';
  if (challenge?.kind !== 'pose') return 'flash_none';

  // Un cuello humano: llega al ángulo poco a poco, pasando por los
  // intermedios. Saltar directo al destino es justo lo que el analizador
  // penaliza como corte de vídeo.
  const progress = Math.min(1, (performance.now() - challenge.startedAt) / 900);
  const grados = Math.round((30 * progress) / 5) * 5;
  switch (challenge.params?.action) {
    // Ojo al signo, y ojo a de dónde sale. Los frames de yaw se nombran por
    // el ángulo MEDIDO, y el reto se enuncia desde el sujeto: girar hacia la
    // propia izquierda mueve la nariz a la derecha de la imagen, o sea ángulo
    // positivo. Copiar aquí el convenio del servidor en vez del físico haría
    // que el banco pasara con el servidor equivocado, que es exactamente lo
    // que pasó.
    case 'yaw_left':
      return yawFixture(grados);
    case 'yaw_right':
      return yawFixture(-grados);
    case 'move_closer':
      return `closer_${Math.min(4, Math.floor(progress * 5))}`;
    default:
      return 'pitch_up';
  }
}

/** Nombre del frame para un ángulo con signo. Cuidado con el cero: -0 < 0 es falso. */
function yawFixture(grados) {
  const signo = grados < 0 ? '-' : '+';
  return `yaw_${signo}${String(Math.abs(grados)).padStart(2, '0')}`;
}

async function playOne() {
  const response = await fetch(`${BASE}/v1/sessions`, { method: 'POST' });
  if (!response.ok) throw new Error(`POST /v1/sessions: ${response.status}`);
  const ticket = await response.json();

  const socket = new WebSocket(`${BASE.replace(/^http/, 'ws')}${ticket.ws_path ?? '/v1/liveness'}`);
  socket.binaryType = 'arraybuffer';

  const script = [];
  const qualityByKind = new Map();
  let current = null;
  let lastId = null;
  painting = null;
  grabSeq = 0;

  const session = runSession({
    socket,
    token: ticket.session_token,
    source: makeSource(() => current),
    painter,
    onEvent: (state) => {
      current = state.challenge;
      if (process.env.DEBUG_GRAB && state.challenge?.id !== lastId) {
        lastId = state.challenge?.id;
        console.error(`RETO t=${Math.round(performance.now())} ${state.challenge?.kind ?? 'ninguno'} ${JSON.stringify(state.challenge?.params ?? {})}`);
      }
      if (!state.challenge) return;
      qualityByKind.set(state.challenge.kind, state.quality);
      const label = state.challenge.params?.action ?? state.challenge.kind;
      if (script.at(-1) !== label) script.push(label);
    },
  });

  const started = performance.now();
  const final = await session.done;
  return { final, script, qualityByKind, seconds: (performance.now() - started) / 1000 };
}

async function main() {
  const skipped = [];
  let run = null;

  for (let attempt = 1; attempt <= MAX_ATTEMPTS; attempt++) {
    run = await playOne();
    const unreachable = run.script.filter((step) => OUT_OF_REACH.has(step));
    if (unreachable.length === 0) break;
    skipped.push({ attempt, steps: unreachable, decision: run.final.result?.decision });
    run = null;
  }

  for (const s of skipped) {
    console.log(`· intento ${s.attempt} descartado: el maniquí no sabe hacer ${s.steps.join(', ')}`);
  }
  if (!run) {
    console.error(`\nno salió ninguna sesión respondible en ${MAX_ATTEMPTS} intentos.`);
    process.exit(1);
  }

  report(run);
}

function report({ final, script, qualityByKind, seconds }) {
  const dropped = final.dropped.noFace + final.dropped.blurry + final.dropped.noFrame;
  console.log('\n┌─ panel de depuración ' + '─'.repeat(46));
  row('estado', final.phase);
  row('sesión', final.sessionId ?? '—');
  row('captura impuesta', `${final.capture?.width}x${final.capture?.height} @ ${final.capture?.fps} fps`);
  row('guion jugado', script.join(' → '));
  row('fps real', (final.sent / seconds).toFixed(1));
  row('frames enviados', String(final.sent));
  row('frames descartados', String(dropped));
  row('tamaño medio', `${Math.round(final.bytes / Math.max(1, final.sent) / 1024)} KiB`);
  row('latencia saludo', `${final.handshakeRttMs} ms`);
  row('calidad por reto', [...qualityByKind].map(([k, v]) => `${k}=${v}`).join(' '));
  if (final.paintEvents.length) {
    const lags = final.paintEvents.map((p) => p.paint_lag_ms);
    row('destellos pintados', String(final.paintEvents.length));
    row('retardo de pintado', `${Math.min(...lags)}–${Math.max(...lags)} ms (medido)`);
    row('duración por color', final.paintEvents.map((p) => `${p.color}:${p.duration_ms}ms`).join(' '));
  }
  row('tramos alargados', String(final.adjustments.length));
  if (final.gazeTargets.length) {
    row('objetivos de mirada', final.gazeTargets.map((t) => `(${t.x.toFixed(2)},${t.y.toFixed(2)})`).join(' '));
  }
  row('colores sin pintar', String(final.flashSkipped));
  row('resultado', final.result ? `${final.result.decision} · ${final.result.reason}` : '—');
  if (final.explanation) {
    row('score fusión', `${final.explanation.score.toFixed(3)} · perfil ${final.explanation.profile_version}`);
    console.log('│');
    console.log('│ detectores');
    for (const s of final.explanation.signals) {
      const marca = !s.measured ? '·' : s.passed ? '✓' : '✗';
      const valor = s.measured ? s.value.toFixed(3).padStart(6) : ' no med';
      const suelo = s.measured && s.floor > 0 ? `suelo ${s.floor.toFixed(2)}` : '';
      console.log(`│   ${marca} ${s.signal.padEnd(23)} ${valor}  peso ${s.weight.toFixed(2)}  ${suelo}`);
    }
    if (final.explanation.reasons.length) {
      console.log(`│   motivos: ${final.explanation.reasons.join(', ')}`);
    }
  }
  row('cierre', `${final.closeCode} ${final.closeReason}`);
  console.log('└' + '─'.repeat(68));

  const problems = [];
  if (final.closeCode !== 4000) problems.push(`cierre ${final.closeCode}, se esperaba 4000`);
  if (!final.result) problems.push('la sesión no llegó a un resultado');

  // El banco comprueba el CLIENTE, no la vivacidad. Y no puede comprobar la
  // vivacidad: su maniquí es un rostro dibujado, así que el clasificador de
  // textura lo marca como ataque y la calidad de captura de un render no
  // llega al mínimo. Que la fusión lo rechace es señal de que funciona.
  //
  // Lo que sí es responsabilidad del cliente es que los retos se cumplieran:
  // que pintara los colores a tiempo, que moviera la cabeza donde tocaba y
  // que mirara al punto. Eso son las señales de reto-respuesta.
  const respuesta = ['pose_compliance', 'flash_correlation', 'gaze_response'];
  for (const s of final.explanation?.signals ?? []) {
    if (!respuesta.includes(s.signal) || !s.measured) continue;
    if (s.value < 0.9) {
      problems.push(`${s.signal} = ${s.value.toFixed(3)}: el cliente no condujo bien el reto`);
    }
  }
  if (!final.explanation) {
    problems.push('sin desglose: arranca el gateway con GATEWAY_INSECURE_EXPLAIN_VERDICT=true');
  }
  if (final.flashSkipped > 0) problems.push(`${final.flashSkipped} color(es) del guion sin pintar`);
  if (final.sent < 20) problems.push(`sólo ${final.sent} frames enviados`);
  if (final.sent / seconds < 12) problems.push(`fps real ${(final.sent / seconds).toFixed(1)}, muy por debajo de 15`);

  for (const [kind, quality] of qualityByKind) {
    if (quality !== qualityFor({ kind })) problems.push(`calidad ${quality} en ${kind}`);
  }
  if (qualityByKind.has('flash') && qualityByKind.has('pose')
      && !(qualityByKind.get('flash') > qualityByKind.get('pose'))) {
    problems.push('la calidad no sube en el destello respecto a la pose');
  }
  for (const paint of final.paintEvents) {
    if (paint.duration_ms < MIN_SEGMENT_MS) problems.push(`tramo de ${paint.duration_ms} ms`);
    if (!(paint.painted_at_ms > paint.decided_at_ms)) problems.push('el instante de pintado no es posterior a la decisión');
  }

  if (problems.length) {
    console.log('\nPROBLEMAS:');
    for (const problem of problems) console.log('  ✗ ' + problem);
    process.exit(1);
  }
  console.log('\n✓ el cliente condujo la sesión entera y las métricas cuadran');
  process.exit(0);
}

function row(label, value) {
  console.log(`│ ${label.padEnd(22)} ${String(value)}`);
}

main().catch((error) => {
  console.error('el banco falló:', error);
  process.exit(1);
});
