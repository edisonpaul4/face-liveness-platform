/**
 * Secuenciador de destellos.
 *
 * Dos cosas que no son negociables y una que parece un detalle:
 *
 * 1. **350 ms mínimo por color.** Es un requisito de accesibilidad por riesgo
 *    fotosensible, no una preferencia de producto: por debajo de ~3 Hz de
 *    cambio se entra en el rango que puede desencadenar crisis. El servidor ya
 *    genera tramos de 350-600 ms, así que esto es una red de seguridad; pero
 *    tiene que estar aquí, porque la seguridad de quien mira la pantalla no
 *    puede depender de que el servidor se porte bien.
 *
 * 2. **Transiciones suavizadas.** Un corte duro de blanco a azul es un
 *    parpadeo; una rampa de ~120 ms es un cambio de luz. Además de ser más
 *    seguro, se parece más a cómo cambia la luz en el mundo real.
 *
 * 3. **Se reporta el instante REAL de pintado**, el que devuelve el callback
 *    de render, no el momento en que se decidió pintar. Entre una cosa y otra
 *    hay un frame de composición y a veces mucho más si la pestaña va
 *    apretada. El analizador busca el retardo entre emitir y ver, y si el
 *    cliente miente sobre cuándo pintó, esa búsqueda arranca torcida.
 *
 * Sin DOM: el pintado se inyecta. En el navegador pinta la pantalla; en el
 * banco de pruebas, anota.
 */

/** Duración mínima por color. Accesibilidad, no negociable. */
export const MIN_SEGMENT_MS = 350;

/** Duración de la rampa entre colores. */
export const RAMP_MS = 120;

/** Colores de la paleta, en RGB de CSS. */
// Primarios SATURADOS, sin fuga en los otros dos canales.
//
// Un rojo con algo de verde y azul (rgb(255,40,40), que es lo que pide el
// instinto para suavizarlo) se mide como respuesta en los tres canales: el
// analizador normaliza contra la línea base del sujeto y amplifica, así que
// una fuga del 16 % basta para que el rostro parezca responder a todo. El
// servidor deja entonces de poder distinguir rojo de blanco y el reto de
// destello se queda sin contenido.
//
// Saturar tampoco encandila más: un rojo puro tiene MENOS luminancia que uno
// lavado con blanco.
export const PALETTE = {
  white: 'rgb(255, 255, 255)',
  red: 'rgb(255, 0, 0)',
  green: 'rgb(0, 255, 0)',
  blue: 'rgb(0, 0, 255)',
};

/**
 * Normaliza la secuencia que manda el servidor aplicando el mínimo.
 *
 * Devuelve también qué se ajustó: el cliente no puede cambiar la secuencia en
 * silencio, porque el analizador correlaciona contra lo que cree que se
 * pintó.
 */
export function enforceMinimum(sequence, minMs = MIN_SEGMENT_MS) {
  const adjustments = [];
  const safe = sequence.map((segment, index) => {
    const requested = Number(segment.duration_ms) || 0;
    if (requested >= minMs) return { ...segment, duration_ms: requested };
    adjustments.push({ index, color: segment.color, requested, applied: minMs });
    return { ...segment, duration_ms: minMs };
  });
  return { sequence: safe, adjustments };
}

/**
 * Pinta una secuencia de destellos.
 *
 * @param {Array} sequence tramos {color, duration_ms} tal y como los manda el servidor.
 * @param {(color: string, rampMs: number) => Promise<number>} painter
 *        pinta y resuelve con el instante REAL de pintado, en ms.
 * @param {object} options `alive` decide si esta secuencia sigue siendo la
 *        vigente; en cuanto deja de serlo se abandona sin tocar la pantalla.
 * @returns {Promise<{painted: Array, adjustments: Array, abandoned: boolean, stalled: boolean}>}
 */
export async function playSequence(sequence, painter, options = {}) {
  const {
    minMs = MIN_SEGMENT_MS,
    rampMs = RAMP_MS,
    now = () => performance.now(),
    sleep = defaultSleep,
    alive = () => true,
  } = options;

  const { sequence: safe, adjustments } = enforceMinimum(sequence, minMs);
  const painted = [];

  for (const segment of safe) {
    // Una secuencia que ya no es la vigente no puede pintar NADA, ni siquiera
    // apagarse: el servidor ya está midiendo el reto siguiente y cualquier
    // color que pongamos aquí se mide como respuesta a otra cosa.
    if (!alive()) return { painted, adjustments, abandoned: true, stalled: false };

    const decidedAt = now();
    const paintedAt = await painter(PALETTE[segment.color] ?? PALETTE.white, rampMs);

    // El pintor no pudo componer: pestaña oculta, o el navegador dejó de
    // animar. Inventar aquí un instante de pintado sería lo peor que se puede
    // hacer, porque el analizador correlaciona contra él.
    if (paintedAt === null) return { painted, adjustments, abandoned: true, stalled: true };

    painted.push({
      color: segment.color,
      duration_ms: segment.duration_ms,
      decided_at_ms: Math.round(decidedAt),
      // El que importa: cuándo se pintó de verdad.
      painted_at_ms: Math.round(paintedAt),
      // Lo que costó llegar de la decisión a la pantalla.
      paint_lag_ms: Math.round(paintedAt - decidedAt),
    });

    // El tramo se cuenta desde que se pintó, no desde que se decidió: si la
    // composición se retrasó, el color tiene que estar en pantalla su tiempo
    // completo igualmente.
    const elapsed = now() - paintedAt;
    await sleep(Math.max(0, segment.duration_ms - elapsed));
  }

  if (!alive()) return { painted, adjustments, abandoned: true, stalled: false };

  // Se apaga con la misma rampa: cortar en seco es otro parpadeo.
  await painter(null, rampMs);
  return { painted, adjustments, abandoned: false, stalled: false };
}

function defaultSleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
