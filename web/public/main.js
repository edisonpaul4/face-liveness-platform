/**
 * Pegamento del navegador: cámara, filtro local, pantalla de destello y panel.
 *
 * La lógica de sesión no está aquí: está en lib/session.js, sin DOM, para que
 * el banco de pruebas ejercite el mismo código contra el gateway de verdad.
 * Lo que queda en este fichero es lo que sólo puede probarse con un navegador
 * y una cámara delante.
 */

import { openCamera } from './lib/camera.js';
import { createFaceFilter, SHARPNESS_FLOOR } from './lib/facefilter.js';
import { createPainter } from './lib/flashscreen.js';
import { createTargetPainter } from './lib/gazetarget.js';
import { FRAMING, TARGET_AREA, createFraming } from './lib/framing.js';
import { createRecorder } from './lib/recorder.js';
import { runSession } from './lib/session.js';

const el = (id) => document.getElementById(id);
const trace = [];

/** Qué decirle a la persona en cada estado del encuadre. */
const framingPrompts = {
  [FRAMING.noFace]: 'Ponte delante de la cámara.',
  [FRAMING.tooFar]: 'Acércate más a la cámara.',
  [FRAMING.tooClose]: 'Demasiado cerca: sepárate un poco.',
  [FRAMING.offCenter]: 'Céntrate en el óvalo.',
  [FRAMING.ok]: 'Así. No te muevas…',
};

const prompts = {
  calibration: 'Mira a la cámara y quédate quieto.',
  hold: 'Quédate quieto mirando a la cámara.',
  flash: 'Mira a la pantalla.',
  gaze: 'Mira al punto.',
  pose: {
    yaw_left: 'Gira la cabeza a tu izquierda.',
    yaw_right: 'Gira la cabeza a tu derecha.',
    pitch_up: 'Levanta un poco la barbilla.',
    move_closer: 'Acércate a la cámara.',
  },
};

function promptFor(challenge) {
  if (!challenge) return 'Espera…';
  if (challenge.kind === 'pose') {
    return prompts.pose[challenge.params?.action] ?? 'Sigue la indicación.';
  }
  if (challenge.kind === 'hold') {
    // Once segundos son muchos sin nada que mirar: sin cuenta atrás la gente
    // cree que se ha colgado, se mueve, y el movimiento estropea justo la
    // medida que este paso existe para hacer.
    const total = Math.round((challenge.params?.hold_ms ?? 0) / 1000);
    const van = Math.round((performance.now() - challenge.startedAt) / 1000);
    return total
      ? `Quédate quieto mirando a la cámara… ${Math.max(0, total - van)} s`
      : prompts.hold;
  }
  if (challenge.kind === 'gaze') {
    // Se dice también en palabras: el punto es amarillo y late, pero un
    // objetivo que sólo existe como color no lo ve todo el mundo.
    const izquierda = (challenge.params?.target_x ?? 0.5) < 0.5;
    return `Mira al punto, a la ${izquierda ? 'izquierda' : 'derecha'}.`;
  }
  return prompts[challenge.kind] ?? 'Sigue la indicación.';
}

function log(line) {
  trace.push(`${new Date().toISOString().slice(11, 23)}  ${line}`);
  el('trace').textContent = trace.slice(-60).join('\n');
}

/** Mediana y p95 de una lista de tiempos, para la cabecera de la grabación. */
function percentiles(v) {
  if (v.length === 0) return null;
  const s = [...v].sort((a, b) => a - b);
  const at = (q) => Math.round(s[Math.min(s.length - 1, Math.floor(s.length * q))] * 10) / 10;
  return { n: s.length, mediana: at(0.5), p95: at(0.95), max: at(1) };
}

function render(state, filterStatus) {
  const dropped = state.dropped.noFace + state.dropped.blurry + state.dropped.noFrame;
  const seconds = state.handshakeRttMs !== null ? (performance.now() - state.startedAt) / 1000 : 0;

  el('d-phase').textContent = state.phase;
  el('d-session').textContent = state.sessionId ?? '—';
  el('d-challenge').textContent = state.challenge
    ? `${state.challenge.kind}${state.challenge.params?.action ? '/' + state.challenge.params.action : ''}`
    : '—';
  el('d-fps').textContent = seconds > 0.5 ? (state.sent / seconds).toFixed(1) : '—';
  el('d-sent').textContent = String(state.sent);
  el('d-dropped').textContent =
    `${dropped} (sin rostro ${state.dropped.noFace}, borrosos ${state.dropped.blurry})`;
  el('d-quality').textContent = state.quality.toFixed(2);
  el('d-size').textContent = state.sent > 0 ? `${Math.round(state.bytes / state.sent / 1024)} KiB` : '—';
  el('d-rtt').textContent = state.handshakeRttMs !== null ? `${state.handshakeRttMs} ms` : '—';

  const lastPaint = state.paintEvents[state.paintEvents.length - 1];
  el('d-paint').textContent = lastPaint ? `${lastPaint.paint_lag_ms} ms (real)` : '—';
  // Si esto sube, el cliente va por detrás del guion y el servidor está
  // cerrando los retos antes de que la pantalla haya pintado sus colores.
  el('d-skipped').textContent = String(state.flashSkipped);
  el('d-gaze').textContent = state.gazeTargets.length
    ? `${state.gazeTargets.length} · último (${state.gazeTargets.at(-1).x.toFixed(2)}, ${state.gazeTargets.at(-1).y.toFixed(2)})`
    : '0';
  el('d-filter').textContent = filterStatus;
  el('d-result').textContent = state.result
    ? `${state.result.decision} · ${state.result.reason ?? ''}`
    : '—';
  showHint(state.result?.reason);
  renderDetectors(state.explanation);

  el('prompt').textContent = state.result
    ? `Resultado: ${state.result.decision}`
    : promptFor(state.challenge);
}

// El servidor estático dice a qué gateway apuntar. Si no lo dice —porque el
// estático lo sirve otra cosa— se queda lo que haya escrito en el campo.
fetch('/config.json')
  .then((r) => (r.ok ? r.json() : null))
  .then((config) => {
    if (config?.gateway) el('gateway').value = config.gateway;
  })
  .catch(() => {});

async function start() {
  el('start').disabled = true;
  el('warning').hidden = true;

  let camera;
  try {
    // La resolución se puede forzar desde la URL: ?capture=480 o ?capture=720.
    //
    // Es un interruptor de experimento, no una opción de producto. El destello
    // dejó de correlacionar al pasar a 720p —seis sesiones de seis— y la
    // sospecha es que a esa resolución el fondo también recibe el destello,
    // con lo que la medida diferencial cara/fondo lo cancela. Comparar las dos
    // en la MISMA habitación es la única forma de separar la resolución del
    // resto de variables de la escena.
    //
    // El servidor tiene su propio interruptor (`make gateway CAPTURE=480`).
    // Para un experimento limpio hay que mover los dos.
    const modo = new URLSearchParams(location.search).get('capture');
    const perfiles = {
      480: { width: 640, height: 480, fps: 30 },
      720: { width: 1280, height: 720, fps: 15 },
    };
    camera = await openCamera({ ...(perfiles[modo] ?? {}), sharpnessFloor: SHARPNESS_FLOOR });
    if (perfiles[modo]) {
      log(`captura forzada desde la URL: ${perfiles[modo].width}x${perfiles[modo].height} @ ${perfiles[modo].fps} fps`);
    }
  } catch (error) {
    el('warning').hidden = false;
    el('warning').textContent = `No se pudo abrir la cámara: ${error.message}`;
    el('start').disabled = false;
    return;
  }
  el('preview').srcObject = camera.stream;
  // La cadencia REAL que negoció la cámara, no la que se pidió.
  //
  // Son cosas distintas y confundirlas cuesta un diagnóstico: se le piden
  // 30 fps, `getSettings()` puede decir 30, y la cámara entregar 15 porque en
  // penumbra alarga la exposición. Sin este número, un tope de la cámara y un
  // tope del código dan exactamente el mismo síntoma.
  log(
    `cámara abierta: ${camera.settings.width}x${camera.settings.height} ` +
      `@ ${camera.settings.frameRate ?? '?'} fps (negociado)`,
  );

  const filter = await createFaceFilter();
  log(`filtro local: ${filter.status}`);
  if (!filter.status.startsWith('mediapipe')) {
    el('warning').hidden = false;
    el('warning').textContent =
      'MediaPipe no cargó: el filtro local sólo descarta frames borrosos. ' +
      'La sesión sigue siendo válida: quien puntúa es el servidor.';
  }

  await gateFraming(camera, filter);
}

/**
 * Puerta de encuadre: no se valida a nadie hasta que la cara está cerca.
 *
 * La mirada se mide por el desplazamiento del iris dentro de su órbita, y a
 * media distancia el iris ocupa una docena de píxeles. Cada centímetro que la
 * persona se acerca es señal que el analizador gana gratis.
 *
 * No puntúa nada y el servidor no se entera de que existe: es una condición
 * de captura, del mismo tipo que no enviar frames borrosos.
 */
async function gateFraming(camera, filter) {
  const guide = el('guide');
  const begin = el('begin');
  // El óvalo es un SVG: `hidden` como propiedad no hace nada ahí. Ver la
  // nota en styles.css.
  guide.removeAttribute('hidden');
  begin.hidden = false;
  begin.disabled = true;

  // Sin detector local no hay puerta que valga: se avisa y se deja pasar. El
  // servidor decide igual, sólo que con peor materia prima.
  if (!filter.status.startsWith('mediapipe')) {
    guide.setAttribute('hidden', '');
    begin.disabled = false;
    el('prompt').textContent = 'Sin detector local: colócate cerca y pulsa «validar».';
    begin.onclick = () => void validate(camera, filter);
    return;
  }

  const framing = createFraming();
  let stop = false;

  const tick = () => {
    if (stop) return;
    const status = framing.update(filter.locate(camera.video, performance.now()));

    guide.classList.toggle('ok', status.state === FRAMING.ok);
    guide.classList.toggle('near', status.state === FRAMING.tooClose);
    el('prompt').textContent = framingPrompts[status.state];
    el('d-framing').textContent =
      `${status.state} · ${(status.area * 100).toFixed(1)}% (objetivo ${(TARGET_AREA * 100).toFixed(0)}%)`;

    if (status.ready && begin.disabled) {
      begin.disabled = false;
      el('prompt').textContent = 'Listo. Pulsa «validar».';
      log(`encuadre listo: cara al ${(status.area * 100).toFixed(1)} % del encuadre`);
    } else if (!status.ready && !begin.disabled) {
      begin.disabled = true;
    }
    requestAnimationFrame(tick);
  };
  requestAnimationFrame(tick);

  begin.onclick = () => {
    stop = true;
    guide.setAttribute('hidden', '');
    begin.hidden = true;
    void validate(camera, filter);
  };
}

async function validate(camera, filter) {
  let lastChallengeId = null;
  const base = el('gateway').value.replace(/\/$/, '');
  let ticket;
  try {
    const response = await fetch(`${base}/v1/sessions`, { method: 'POST' });
    ticket = await response.json();
  } catch (error) {
    el('warning').hidden = false;
    el('warning').textContent = `No se pudo crear la sesión: ${error.message}`;
    el('start').disabled = false;
    camera.stop();
    return;
  }
  log(`sesión ${ticket.session_id}`);

  // Congelar la cámara ANTES de empezar: con los lazos automáticos activos,
  // el destello se cancela solo. Ver `lockExposure` en camera.js.
  const fijada = await camera.lockExposure();
  log(
    fijada.soportado
      ? `cámara fijada: exposición ${fijada.exposureMode}, blancos ${fijada.whiteBalanceMode}, enfoque ${fijada.focusMode}`
      : `cámara NO fijada (${fijada.motivo}): el destello competirá con el auto-ajuste`,
  );
  if (!fijada.soportado) {
    el('warning').hidden = false;
    el('warning').textContent =
      'Esta cámara no deja fijar exposición ni balance de blancos, así que se ' +
      'deja como estaba. Con mucha luz ambiente, el destello puede salir no medible.';
  }

  const socket = new WebSocket(`${base.replace(/^http/, 'ws')}${ticket.ws_path ?? '/v1/liveness'}`);
  socket.binaryType = 'arraybuffer';

  // Grabación de depuración: apagada salvo que se marque a mano. Lo que
  // guarda es biometría y se queda en esta máquina (CLAUDE.md §6bis).
  let ultimoRender = 0;
  const recorder = createRecorder({ enabled: el('record').checked });
  if (el('record').checked) log('grabando la sesión en local');

  // Cuánto cuesta cada etapa de capturar un frame, en el móvil de verdad.
  //
  // Hace falta medirlo y no deducirlo: la cámara entrega 22,4 fps y el cliente
  // envía 16,5, y esos seis frames que faltan son señal —el pulso va con la
  // raíz de su número y el destello se queda sin correlación con pocas
  // muestras por tramo—. Adelgazar el filtro de cara compró 0,8 fps, así que
  // el coste está en otro sitio y adivinar cuál ya ha fallado una vez.
  const coste = { filtro: [], codec: [] };
  const anota = (donde, ms) => {
    const v = coste[donde];
    v.push(ms);
    if (v.length > 600) v.shift();
  };

  const source = {
    async grab(quality) {
      // Antes que nada, ¿hay frame nuevo? Si la cámara no ha entregado uno,
      // lo que hay en el <video> es el que ya se envió.
      if (!camera.hasFreshFrame()) return { rejected: 'duplicate' };

      // El filtro local va ANTES de codificar: comprimir un frame que se va a
      // tirar es gastar CPU y batería para nada.
      // La NITIDEZ la mide el worker, sobre el bitmap que ya tiene. Aquí
      // sólo queda preguntar si sigue habiendo cara, y eso va cada 250 ms.
      //
      // Medido: la sonda de nitidez costaba 21,7 ms de mediana en el hilo
      // principal —un `getImageData` es una lectura de vuelta de la GPU, el
      // hilo se para a esperar píxeles— mientras codificar costaba 0,9. El
      // caro nunca fue el códec.
      const t0 = performance.now();
      const verdict = filter.faceStillThere(camera.video, t0);
      anota('filtro', performance.now() - t0);
      if (!verdict.ok) return { rejected: verdict.reason };

      const t1 = performance.now();
      const frame = await camera.grab(quality);
      anota('codec', performance.now() - t1);
      return frame;
    },
  };

  const session = runSession({
    socket,
    token: ticket.session_token,
    source,
    painter: createPainter(el('flash')),
    targetPainter: createTargetPainter(el('target')),
    // Se guarda lo mismo que se envía, byte a byte y con el mismo seq que
    // usa el servidor: sin eso la grabación no se puede alinear con el log.
    onFrameSent: (seq, jpeg, capturedAtUs) => recorder.record(seq, jpeg, capturedAtUs),
    onEvent: (state) => {
      // El panel se repinta a lo sumo cada 200 ms. Repintarlo en cada frame
      // gasta hilo principal —el mismo que captura— para enseñar números que
      // nadie puede leer a 20 por segundo.
      const ahora = performance.now();
      if (ahora - ultimoRender >= 200) {
        ultimoRender = ahora;
        render(state, filter.status);
      }
      // La línea de tiempo de retos es lo que convierte la grabación en un
      // caso con etiqueta: qué se pidió y cuándo.
      if (state.challenge && state.challenge.id !== lastChallengeId) {
        lastChallengeId = state.challenge.id;
        recorder.note('challenge', {
          kind: state.challenge.kind,
          params: state.challenge.params,
          deadline_ms: state.challenge.deadlineMs,
        });
      }
    },
  });
  session.state.startedAt = performance.now();

  el('abort').disabled = false;
  el('abort').onclick = () => session.abort('usuario');

  // Una pestaña en segundo plano no compone: el destello no llega a la
  // pantalla y el navegador además estrangula los temporizadores. No hay
  // sesión que valga, así que se abandona en cuanto pasa y se dice por qué.
  const onHidden = () => {
    if (document.hidden) session.abort('screen_hidden');
  };
  document.addEventListener('visibilitychange', onHidden);

  const final = await session.done;
  document.removeEventListener('visibilitychange', onHidden);
  log(`cierre ${final.closeCode}: ${final.closeReason}`);
  if (final.adjustments?.length) {
    log(`⚠ se alargaron ${final.adjustments.length} tramo(s) al mínimo de 350 ms`);
  }
  if (final.flashSkipped > 0) {
    log(`⚠ ${final.flashSkipped} color(es) del guion no llegaron a pintarse`);
  }
  for (const paint of final.paintEvents ?? []) {
    log(`  ${paint.color}: pintado con ${paint.paint_lag_ms} ms de retardo real`);
  }

  recorder.note('result', { decision: final.result?.decision, reason: final.result?.reason });
  try {
    const file = await recorder.save({
      session_id: final.sessionId,
      capture: final.capture,
      result: final.result,
      close_code: final.closeCode,
      paint_events: final.paintEvents,
      gaze_targets: final.gazeTargets,
      // Qué dice la cámara de sí misma y cuántos frames repetidos se
      // descartaron. Es lo que separa "el código va lento" de "la cámara no
      // da más", que desde los sellos de captura se ven igual.
      camera: { settings: camera.settings, stats: camera.stats(), fijada },
      // Por qué se saltó cada turno de captura. Sin esto, «se enviaron menos
      // frames de los que entregó la cámara» no dice si sobraba trabajo, si
      // el socket iba lleno o si simplemente no había frame nuevo.
      dropped: final.dropped,
      costes: {
        filtro_ms: percentiles(coste.filtro),
        codec_ms: percentiles(coste.codec),
      },
    });
    if (file) {
      log(`grabación guardada: web/harness/recordings/${file}`);
      el('warning').hidden = false;
      el('warning').textContent = `Grabación guardada: web/harness/recordings/${file}`;
    }
  } catch (error) {
    // A la vista, no sólo en la traza: una grabación que se pierde en
    // silencio hace perder la sesión entera.
    log(`no se pudo guardar la grabación: ${error.message}`);
    el('warning').hidden = false;
    el('warning').textContent = `No se pudo guardar la grabación: ${error.message}`;
  }

  camera.stop();
  el('start').disabled = false;
  el('abort').disabled = true;
}

el('start').addEventListener('click', () => void start());


/**
 * Pinta el desglose por detector.
 *
 * Sólo hay algo que pintar si el gateway corre con la bandera insegura. En
 * producción el cliente recibe `try_again` y nada más, a propósito: decirle a
 * un atacante qué detector le pilló y por cuánto es entregarle el bucle de
 * realimentación que necesita (CLAUDE.md §6).
 */
function renderDetectors(explanation) {
  const box = el('detectors');
  if (!explanation) {
    box.hidden = true;
    return;
  }
  box.hidden = false;
  el('d-score').textContent =
    `${explanation.score.toFixed(3)} · perfil ${explanation.profile_version}`;

  const body = el('d-signals').querySelector('tbody');
  body.textContent = '';
  for (const s of explanation.signals) {
    const tr = document.createElement('tr');
    // Tres estados, no dos. "No medido" no es "fallado": una señal que no se
    // pudo medir no entra en la fusión, ni a favor ni en contra.
    if (!s.measured) tr.className = 'unmeasured';
    else if (!s.passed) tr.className = 'fail';

    const cells = s.measured
      ? [s.signal, s.value.toFixed(3), s.floor > 0 ? s.floor.toFixed(2) : '—',
         s.weight.toFixed(2), s.passed ? '✓' : '✗']
      : [s.signal, 'no medido', '—', '—', '·'];

    for (const [i, text] of cells.entries()) {
      const td = document.createElement('td');
      td.textContent = text;
      if (i === 4) td.className = 'mark';
      tr.appendChild(td);
    }
    body.appendChild(tr);
  }

  if (explanation.reasons?.length) {
    log(`motivos: ${explanation.reasons.join(', ')}`);
  }
}


/**
 * Pistas de captura, en palabras que se puedan seguir.
 *
 * Sólo se traducen los motivos sobre el ENTORNO de la persona, que puede
 * arreglar. Un rechazo nunca se explica: decirle a un atacante por qué se le
 * rechazó es enseñarle qué corregir (CLAUDE.md §6). Pero dejar a un usuario
 * legítimo reintentando a ciegas tampoco es aceptable.
 */
const hints = {
  too_much_ambient_light:
    'Hay demasiada luz a tu espalda: el destello de la pantalla no llega a ' +
    'iluminarte. Baja la persiana o ponte de espaldas a la pared, y sube el ' +
    'brillo de la pantalla.',
  infrastructure_error: 'Fallo nuestro, no tuyo. Vuelve a intentarlo.',
};

function showHint(reason) {
  const text = hints[reason];
  if (!text) return;
  el('warning').hidden = false;
  el('warning').textContent = text;
}
