/**
 * Pintado de destellos a pantalla completa.
 *
 * Lo delicado aquí es UNA cosa: devolver el instante en que el color llegó de
 * verdad a la pantalla, no el instante en que se decidió pintarlo.
 *
 * `requestAnimationFrame` se ejecuta ANTES de componer el frame. Si se tomara
 * ese timestamp se estaría reportando la intención, no el hecho. Con dos rAF
 * encadenados, el segundo corre cuando el frame que llevaba el cambio ya está
 * compuesto: ese sí es el instante real, y es el que el analizador necesita
 * para que su búsqueda de retardo arranque de una medida y no de una
 * suposición.
 */

/**
 * Cuánto se espera a que el navegador componga antes de darlo por imposible.
 *
 * En una pestaña oculta `requestAnimationFrame` NO se ejecuta: la promesa se
 * quedaría pendiente para siempre y la sesión moriría por plazo, sin decir
 * por qué. Aquí se resuelve con null, que significa "no llegó a pintarse".
 */
export const PAINT_TIMEOUT_MS = 1000;

/**
 * @param {HTMLElement} surface la capa que cubre la pantalla.
 * @returns {(color: string|null, rampMs: number) => Promise<number|null>}
 *          el instante REAL de pintado, o null si no se pintó.
 */
export function createPainter(surface, { timeoutMs = PAINT_TIMEOUT_MS } = {}) {
  return (color, rampMs) =>
    new Promise((resolve) => {
      let listo = false;
      const acabar = (value) => {
        if (listo) return;
        listo = true;
        clearTimeout(guardia);
        resolve(value);
      };
      const guardia = setTimeout(() => {
        // No se compuso, así que el color no llegó a existir para nadie: se
        // deshace. Si no, al volver la pestaña al frente la pantalla aparece
        // encendida con un destello que ya no viene a cuento.
        surface.style.transition = '';
        surface.style.opacity = '0';
        acabar(null);
      }, timeoutMs);

      // Transición suavizada: un corte duro de blanco a azul es un parpadeo;
      // una rampa es un cambio de luz. Accesibilidad, no estética.
      surface.style.transition = `background-color ${rampMs}ms ease-in-out, opacity ${rampMs}ms ease-in-out`;

      if (color === null) {
        surface.style.opacity = '0';
      } else {
        surface.style.backgroundColor = color;
        surface.style.opacity = '1';
      }

      requestAnimationFrame(() => {
        // Segundo rAF: el frame con el cambio ya está compuesto.
        requestAnimationFrame((paintedAt) => acabar(paintedAt));
      });
    });
}
