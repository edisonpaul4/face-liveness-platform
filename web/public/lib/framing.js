/**
 * Puerta de encuadre: exige que la cara esté suficientemente cerca y centrada
 * antes de dejar empezar.
 *
 * Por qué existe: la mirada se mide por el desplazamiento del iris dentro de
 * su órbita, y a media distancia el iris ocupa una docena de píxeles. Cada
 * centímetro que la persona se acerca es señal que el analizador gana gratis.
 * Lo mismo vale, en menor medida, para el destello y la pose.
 *
 * Lo que NO es: esto no puntúa. Es una condición de captura, del mismo tipo
 * que no enviar frames borrosos. El servidor no se entera de que existe y no
 * se fía de ella; si alguien parchea el cliente para saltársela, lo único que
 * consigue es que sus propias señales sean peores.
 *
 * Sin DOM a propósito: el estado se calcula aquí y lo pinta quien quiera.
 */

/** Fracción del encuadre que debe ocupar la cara para empezar. */
export const TARGET_AREA = 0.12;
/** Por debajo de esto se pide acercarse; por encima del techo, alejarse. */
export const MIN_AREA = 0.10;
export const MAX_AREA = 0.32;
/** Cuánto puede desviarse el centro de la cara del centro del encuadre. */
export const MAX_OFFSET = 0.18;
/** Frames seguidos en buena posición antes de dar el encuadre por bueno. */
export const STABLE_FRAMES = 8;

export const FRAMING = {
  noFace: 'noFace',
  tooFar: 'tooFar',
  tooClose: 'tooClose',
  offCenter: 'offCenter',
  ok: 'ok',
};

/**
 * Crea el evaluador de encuadre. Mantiene la racha de frames buenos.
 */
export function createFraming({ stableFrames = STABLE_FRAMES } = {}) {
  let streak = 0;

  return {
    /**
     * @param {{ok: boolean, area?: number, cx?: number, cy?: number}} located
     * @returns {{state: string, ready: boolean, progress: number, area: number}}
     */
    update(located) {
      const state = classify(located);
      streak = state === FRAMING.ok ? streak + 1 : 0;

      return {
        state,
        // La racha evita que un frame afortunado abra la puerta: hay que
        // sostener la posición, que es justo lo que hará falta después.
        ready: streak >= stableFrames,
        progress: Math.min(1, streak / stableFrames),
        area: located.ok ? located.area : 0,
      };
    },

    reset() {
      streak = 0;
    },
  };
}

function classify(located) {
  if (!located?.ok) return FRAMING.noFace;
  if (located.area < MIN_AREA) return FRAMING.tooFar;
  if (located.area > MAX_AREA) return FRAMING.tooClose;

  // Centrar importa porque una cara en una esquina mira a la cámara desde un
  // ángulo que contamina la medida de mirada: el iris se desplaza sin que la
  // mirada cambie.
  const offset = Math.hypot(located.cx - 0.5, located.cy - 0.5);
  if (offset > MAX_OFFSET) return FRAMING.offCenter;

  return FRAMING.ok;
}
