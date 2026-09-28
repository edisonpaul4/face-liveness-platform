/**
 * Objetivo de mirada a pantalla completa.
 *
 * El mismo cuidado que el destello con una cosa: se devuelve el instante REAL
 * en que el objetivo apareció, no cuando se decidió pintarlo. El servidor mide
 * el desplazamiento del iris dentro de una ventana que empieza ahí, y un
 * timestamp inventado desplazaría la ventana entera.
 *
 * También comparte la vigilancia: en una pestaña oculta `requestAnimationFrame`
 * no corre, y colgarse esperándolo mata la sesión sin decir por qué.
 */

export const PAINT_TIMEOUT_MS = 1000;

/**
 * @param {HTMLElement} surface capa a pantalla completa donde vive el objetivo.
 * @returns {(target: {x: number, y: number}|null) => Promise<number|null>}
 *          instante REAL de aparición, o null si no llegó a pintarse.
 */
export function createTargetPainter(surface, { timeoutMs = PAINT_TIMEOUT_MS } = {}) {
  return (target) =>
    new Promise((resolve) => {
      let done = false;
      const finish = (value) => {
        if (done) return;
        done = true;
        clearTimeout(guard);
        resolve(value);
      };
      const guard = setTimeout(() => {
        surface.style.opacity = '0';
        finish(null);
      }, timeoutMs);

      if (target === null) {
        surface.style.opacity = '0';
      } else {
        // Porcentajes del viewport: el servidor manda fracciones y la pantalla
        // de cada uno es la que es.
        surface.style.setProperty('--target-x', `${target.x * 100}%`);
        surface.style.setProperty('--target-y', `${target.y * 100}%`);
        surface.style.opacity = '1';
      }

      requestAnimationFrame(() => {
        requestAnimationFrame((paintedAt) => finish(paintedAt));
      });
    });
}
