/**
 * Conducción de una sesión de liveness.
 *
 * Sin DOM y sin cámara: la fuente de frames y el pintor se inyectan. Eso hace
 * que el banco de pruebas ejercite EXACTAMENTE este código contra el gateway
 * de verdad, y que lo único sin probar de forma automática sea el pegamento
 * del navegador.
 *
 * El cliente aquí no decide nada sobre la sesión: no dice haber cumplido un
 * reto, no conoce el guion y no sabe cuántos retos quedan. Manda frames y
 * pinta lo que le digan (CLAUDE.md §6).
 */

import { MSG, CHALLENGE, clientHello, clientTelemetry, clientAbort, encodeFrame, describeClose } from './protocol.js';
import { qualityFor } from './quality.js';
import { playSequence } from './flash.js';

/** Ritmo por defecto si el servidor no impone otro. */
export const DEFAULT_FPS = 30;

/**
 * @param {object} deps
 * @param {WebSocket} deps.socket
 * @param {string} deps.token
 * @param {{grab: (quality: number) => Promise<{jpeg: Uint8Array, capturedAtUs: number}|{rejected: string}>}} deps.source
 * @param {(color: string|null, rampMs: number) => Promise<number>} deps.painter
 * @param {(event: object) => void} [deps.onEvent] para el panel de depuración
 */
export function runSession({
  socket,
  token,
  source,
  painter,
  targetPainter = async () => null,
  // Sumidero opcional de frames enviados, para grabar la sesión. Se inyecta
  // como el pintor: este módulo no sabe qué se hace con ellos.
  onFrameSent = () => {},
  onEvent = () => {},
  now = () => performance.now(),
}) {
  const state = {
    phase: 'conectando',
    challenge: null,
    // Colores del guion que no se llegaron a pintar porque el servidor
    // cerró el reto antes. Es una métrica del panel: si sube, el cliente va
    // por detrás del guion y el análisis se queda sin tramos.
    //
    // Que se abandone sólo el apagado final NO cuenta: el servidor cierra el
    // reto justo cuando el último color cumple su tiempo, así que llegar
    // tarde a apagar es lo normal y lo apaga el cierre de reto.
    flashSkipped: 0,
    // Objetivos de mirada mostrados, con el instante REAL de aparición. El
    // servidor no los consume todavía; existen para poder contrastar su
    // ventana contra la realidad en vez de contra una suposición.
    gazeTargets: [],
    // Qué midió cada detector y si pasó su suelo. Null salvo en modo de
    // depuración del servidor.
    explanation: null,
    quality: qualityFor(null),
    seq: 0,
    sent: 0,
    bytes: 0,
    dropped: { noFace: 0, blurry: 0, noFrame: 0 },
    paintEvents: [],
    adjustments: [],
    handshakeRttMs: null,
    result: null,
    closeCode: null,
  };

  let pump = null;
  let stopPump = () => clearTimeout(pump);
  let helloAt = null;
  let telemetryTimer = null;
  let resolveDone;
  const done = new Promise((resolve) => { resolveDone = resolve; });

  const emit = () => onEvent({ ...state, dropped: { ...state.dropped } });

  function setPhase(phase) {
    state.phase = phase;
    emit();
  }

  // Bytes del último frame enviado. Es la unidad en la que se mide si el
  // socket va atascado: "¿cabe otro frame o ya hay uno esperando?".
  let lastBytes = 0;

  // Envíos del último segundo, para no pasar del presupuesto de subida.
  let recientes = [];

  /**
   * ¿Cabe otro frame dentro del caudal que el enlace aguanta?
   *
   * El recurso escaso NO son los fotogramas por segundo: son los bits. Y
   * pasarse no se paga en frames perdidos sino en frames TARDÍOS, que es
   * mucho peor — un frame que llega tarde miente sobre cuándo pasó lo que
   * muestra, y todas las medidas que dependen del instante (el reparto en
   * fases de la mirada, la correlación del destello, la plausibilidad
   * temporal) miden entonces otra cosa.
   *
   * Medido en el mismo móvil, la misma habitación y el mismo día:
   *
   *     7,2 - 8,7 Mbit/s   retardo hallado 250-350 ms   todo se midió
   *    16,5 Mbit/s         retardo hallado 800 ms       dos miradas falladas
   *
   * A 16,5 la cámara daba 53 fps y se enviaban 28: más muestras y ninguna
   * utilizable. Por eso el tope va en caudal y no en cadencia — si los frames
   * comprimen mejor, entran más, que es exactamente lo que se quiere.
   *
   * Esto NO sustituye a `socketAtascado`: aquel mira la cola del navegador,
   * que es la única que se puede consultar. Por debajo hay las de TCP y las
   * de la red, invisibles desde aquí, y son las que acumularon los 800 ms.
   * Un presupuesto de caudal es la única forma de no llenarlas.
   */
  const PRESUPUESTO_BITS = 9_000_000;
  function cabeEnElCaudal() {
    if (lastBytes === 0) return true;
    const ahora = now();
    recientes = recientes.filter((e) => ahora - e.at < 1000);
    const bits = recientes.reduce((n, e) => n + e.bytes, 0) * 8;
    return bits + lastBytes * 8 <= PRESUPUESTO_BITS;
  }

  /**
   * ¿El socket está al día, o hay un frame entero esperando a salir?
   *
   * Sin esta comprobación el cliente encola a ciegas y el retraso crece sin
   * tope: medido en una sesión real desde un móvil, se pedían **18,5 Mbit/s**
   * de subida —155 KB por frame a 17,9 fps— y el enlace daba menos. El
   * síntoma no fue perder frames sino que llegaran TARDE, y cada vez más: en
   * 5,4 s de sesión el retraso creció **2,0 s**, medido comparando el sello
   * de cada mensaje en el cliente con la hora a la que el gateway lo mandó.
   *
   * Y un frame que llega tarde no es un frame perdido: es un frame que MIENTE
   * sobre cuándo pasó lo que muestra. El reto de mirada parte sus dos fases
   * por el reloj del servidor, así que con los frames 1-2 s atrasados la fase
   * de salida se llenó con la mirada de llegada y el avance salió cero. La
   * persona había obedecido —reprocesado con su propio reloj da 6,16°— y la
   * sesión la mandó a reintentar.
   *
   * Es el mismo principio del buzón de un solo hueco del gateway (§6), que
   * faltaba en este lado: nunca hay cola que crezca. Con el enlace justo, el
   * efecto es que la cadencia baja a lo que el enlace da, en vez de mantener
   * los fps a cambio de latencia.
   */
  function socketAtascado() {
    const pendientes = socket.bufferedAmount ?? 0;
    return lastBytes > 0 && pendientes > lastBytes;
  }

  async function grabAndSend() {
    if (socketAtascado()) {
      state.dropped.backpressure = (state.dropped.backpressure ?? 0) + 1;
      emit();
      return;
    }
    if (!cabeEnElCaudal()) {
      state.dropped.caudal = (state.dropped.caudal ?? 0) + 1;
      emit();
      return;
    }
    const frame = await source.grab(state.quality);
    if (!frame || frame.rejected) {
      // El filtro local descarta frames que no sirven para medir. Es calidad,
      // no puntuación: NUNCA decide nada sobre la sesión.
      const reason = frame?.rejected ?? 'noFrame';
      state.dropped[reason] = (state.dropped[reason] ?? 0) + 1;
      emit();
      return;
    }

    state.seq += 1;
    socket.send(encodeFrame({
      seq: state.seq,
      jpeg: frame.jpeg,
      capturedAtUs: frame.capturedAtUs,
      encoding: frame.encoding,
    }));
    state.sent += 1;
    state.bytes += frame.jpeg.length;
    lastBytes = frame.jpeg.length;
    recientes.push({ at: now(), bytes: frame.jpeg.length });
    onFrameSent(state.seq, frame.jpeg, frame.capturedAtUs);
    emit();
  }

  function startPump(fps) {
    // Se SONDEA al doble del ritmo impuesto, y no es lo mismo que capturar al
    // doble: si no hay frame nuevo el turno se salta y cuesta cero.
    //
    // El ritmo real de una cámara no es el que se le pide, y casi nunca es un
    // divisor del nuestro. Medido en un móvil: la cámara entregaba cada 51 ms
    // y se sondeaba cada 33, así que un frame se pillaba y el siguiente
    // llegaba justo después del tic — los frames salían a 66 ms, uno sí y uno
    // no, y se enviaban 16,7 de los 19,7 que la cámara daba. No se perdían por
    // trabajo: se perdían por batimiento entre dos relojes que no encajan.
    //
    // Sondear más deprisa no puede saltarse el tope del servidor: no se puede
    // enviar más frames de los que la cámara entrega, y el filtro de
    // duplicados garantiza uno por frame de cámara como mucho.
    const interval = 1000 / Math.min(fps || DEFAULT_FPS, 60);
    const sondeo = interval / 2;

    // Bucle que se reprograma solo, NO un setInterval con guardia de
    // ocupado. La diferencia no es de estilo.
    //
    // Con `setInterval` cada 33 ms y un `if (busy) return`, una captura que
    // tarde 34 ms hace perder el turno ENTERO y la cadencia cae de 30 a 15.
    // No degrada: se parte por la mitad. Medido en una sesión real a 720p,
    // todos los pasos salían entre 15,2 y 17,7 fps con el servidor pidiendo
    // 30, y el escalón era exacto.
    //
    // Reprogramando desde el final de cada captura se obtiene lo que la
    // máquina dé —25 fps si el JPEG cuesta 40 ms— en vez de redondear hacia
    // abajo al siguiente múltiplo del intervalo. Al pulso eso le importa
    // mucho: su relación señal/ruido va con la raíz del número de muestras.
    let stopped = false;
    const tick = async () => {
      if (stopped) return;
      const started = now();
      try {
        await grabAndSend();
      } finally {
        if (!stopped) {
          const rest = Math.max(0, sondeo - (now() - started));
          pump = setTimeout(tick, rest);
        }
      }
    };
    pump = setTimeout(tick, 0);
    stopPump = () => {
      stopped = true;
      clearTimeout(pump);
    };
  }

  function startTelemetry() {
    telemetryTimer = setInterval(() => {
      const seconds = (now() - helloAt) / 1000;
      socket.send(clientTelemetry({
        dropped_frames: state.dropped.noFace + state.dropped.blurry + state.dropped.noFrame,
        actual_fps: seconds > 0 ? Number((state.sent / seconds).toFixed(2)) : 0,
        camera_state: 'ok',
      }));
    }, 2000);
  }

  // Generación de destello. Cada reto nuevo, y cada cierre de reto, invalida
  // la secuencia que estuviera sonando: si no, su apagado final cae dentro del
  // reto siguiente y le borra el primer color. El servidor lo mide como un
  // sujeto que no responde al destello.
  let flashGeneration = 0;

  async function onChallenge(message) {
    const generation = ++flashGeneration;

    state.challenge = {
      id: message.challenge_id,
      kind: message.kind,
      params: message.params ?? {},
      deadlineMs: message.deadline_ms,
      startedAt: now(),
    };
    state.quality = qualityFor(state.challenge);
    setPhase(`reto: ${message.kind}`);

    if (message.kind === CHALLENGE.gaze) {
      // DOS puntos: el objetivo aparece a un lado, se queda `dwell_ms`, y
      // salta al otro. El servidor mide el viaje entre las dos posiciones y no
      // contra un reposo, así que la permanencia del primero es parte de la
      // medida y no un adorno.
      const desde = { x: message.params?.from_x ?? 0.5, y: message.params?.from_y ?? 0.5 };
      const hasta = { x: message.params?.target_x ?? 0.5, y: message.params?.target_y ?? 0.5 };
      const dwellMs = Math.max(0, Number(message.params?.dwell_ms ?? 0));

      const primero = await targetPainter(desde);
      if (primero === null) {
        abort('screen_not_painting');
        return;
      }
      state.gazeTargets.push({ x: desde.x, y: desde.y, painted_at_ms: Math.round(primero) });
      emit();

      // Se espera desde el instante REAL de pintado, no desde que se decidió:
      // el servidor cuenta la permanencia desde que reveló el paso, y sumar
      // aquí el retardo de composición desplazaría las dos fases.
      const restante = dwellMs - (now() - primero);
      if (restante > 0) await new Promise((r) => setTimeout(r, restante));
      if (generation !== flashGeneration) return;

      const segundo = await targetPainter(hasta);
      if (segundo === null) {
        abort('screen_not_painting');
        return;
      }
      state.gazeTargets.push({ x: hasta.x, y: hasta.y, painted_at_ms: Math.round(segundo) });
      emit();
      return;
    }

    if (message.kind !== CHALLENGE.flash) return;

    const sequence = message.params?.sequence ?? [];
    const { painted, adjustments, abandoned, stalled } = await playSequence(sequence, painter, {
      now,
      alive: () => generation === flashGeneration,
    });

    // Se ACUMULAN, no se sustituyen.
    //
    // Con la asignación, cada destello borraba los pintados del anterior: una
    // sesión con dos destellos guardaba dos eventos de cuatro, y justo los del
    // primero —que es el que suele fallar— se perdían. Eso deja la grabación
    // sin el dato que hace falta para situar su ventana al reproducirla, que
    // es el mismo agujero que ya costó cinco diagnósticos con los sellos por
    // frame.
    state.paintEvents = [...state.paintEvents, ...painted];
    state.adjustments = adjustments;
    if (abandoned) state.flashSkipped += sequence.length - painted.length;
    emit();

    // La pantalla dejó de componer —lo normal es que la pestaña pasara a
    // segundo plano—. Sin destello no hay nada que medir, así que se abandona
    // diciendo por qué en vez de dejar morir la sesión por plazo.
    if (stalled) {
      abort('screen_not_painting');
      return;
    }

    // Se le cuenta al servidor cuándo se pintó DE VERDAD cada color. Hoy el
    // gateway no lo consume, pero es lo que permitirá contrastar su búsqueda
    // de retardo contra la realidad en vez de contra una suposición.
    socket.send(clientTelemetry({
      paint_events: painted,
      flash_adjustments: adjustments,
      flash_skipped: sequence.length - painted.length,
    }));
  }

  /** Abandona la sesión diciendo por qué. */
  function abort(reason) {
    setPhase(`abandonada: ${reason}`);
    try {
      socket.send(clientAbort(reason));
    } catch {
      // El socket ya no está: da igual, se cierra igualmente.
    }
    socket.close(1000, 'abandonado');
  }

  socket.addEventListener('open', () => {
    helloAt = now();
    setPhase('saludando');
    socket.send(clientHello(token));
  });

  socket.addEventListener('message', (event) => {
    if (typeof event.data !== 'string') return;
    let message;
    try {
      message = JSON.parse(event.data);
    } catch {
      return;
    }

    switch (message.type) {
      case MSG.serverHello:
        state.handshakeRttMs = Math.round(now() - helloAt);
        state.sessionId = message.session_id;
        state.capture = message.capture;
        setPhase('capturando');
        startPump(message.capture?.fps);
        startTelemetry();
        break;

      case MSG.serverChallenge:
        void onChallenge(message);
        break;

      case MSG.serverChallengeEnd: {
        // El objetivo de mirada se apaga con el reto: dejarlo puesto haría
        // que la persona siguiera mirando ahí durante el siguiente.
        if (state.challenge?.kind === CHALLENGE.gaze) void targetPainter(null);
        // El servidor cierra el reto. No dice si se superó, y el cliente
        // tampoco lo pregunta.
        const eraDestello = state.challenge?.kind === CHALLENGE.flash;
        flashGeneration += 1;
        state.challenge = null;
        state.quality = qualityFor(null);
        setPhase('entre retos');
        // Apagado autoritativo: la secuencia abandonada ya no puede pintar,
        // así que la pantalla se apaga aquí y no se queda encendida.
        if (eraDestello) void painter(null, 0);
        break;
      }

      case MSG.serverResult:
        state.result = { decision: message.decision, reason: message.reason_key };
        // Desglose por detector. Sólo llega si el servidor corre con la
        // bandera insegura puesta; en producción no existe.
        state.explanation = message.explanation ?? null;
        setPhase(`resultado: ${message.decision}`);
        break;

      case MSG.serverError:
        state.error = message;
        setPhase(`error: ${message.code}`);
        break;
    }
  });

  socket.addEventListener('close', (event) => {
    stopPump();
    clearInterval(telemetryTimer);
    state.closeCode = event.code;
    state.closeReason = describeClose(event.code);
    setPhase(`cerrada (${state.closeReason})`);
    resolveDone({ ...state });
  });

  return { done, state, abort };
}
