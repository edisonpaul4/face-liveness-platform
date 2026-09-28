/**
 * Grabador de sesión para depuración.
 *
 * Guarda EXACTAMENTE los mismos JPEG que se enviaron al servidor, no un vídeo
 * recomprimido: lo que hay que poder reproducir es lo que el analizador vio,
 * y un H.264 por encima cambiaría justo las señales sutiles que se están
 * depurando —la respuesta cromática del destello y el borde del iris—.
 *
 * Junto a los frames va la línea de tiempo de retos, que es lo que convierte
 * la grabación en un caso con etiqueta: "aquí se le pidió mirar abajo a la
 * izquierda, y esto es lo que hizo su iris".
 *
 * ── Sobre el dato ──────────────────────────────────────────────────────────
 * Esto es biometría, categoría especial bajo la LOPDP (CLAUDE.md §6bis). Por
 * eso:
 *
 *   · va apagado por defecto y hay que marcarlo a mano en cada sesión;
 *   · no sale de la máquina: se envía al servidor estático local, el mismo
 *     que sirve la página, y se escribe en disco ahí;
 *   · el directorio de grabaciones NO se versiona.
 *
 * Formato: una línea JSON de cabecera, y después, por cada frame, 4 bytes
 * big-endian de longitud seguidos del JPEG.
 */

export function createRecorder({ enabled }) {
  if (!enabled) {
    return { record() {}, note() {}, async save() { return null; } };
  }

  const frames = [];
  const events = [];
  const startedAt = Date.now();

  return {
    /** Guarda un frame tal y como se envió. */
    record(seq, jpeg, capturedAtUs) {
      frames.push({ seq, capturedAtUs, jpeg });
    },

    /** Anota algo que pasó, con su instante. */
    note(kind, detail) {
      // ÉPOCA, no performance.now().
      //
      // Los frames se sellan con Date.now()/rVFC —época— y los eventos iban
      // con performance.now(), que cuenta desde que cargó la página. Dos
      // relojes sin nada que los reconcilie: la grabación no se podía alinear
      // y había que cuadrarla a ojo. Con un desfase de segundo y medio se
      // diagnosticó un retraso del gateway que no existía —medido después,
      // sus medidas volvían en 23 ms— y se persiguió durante dos sesiones.
      //
      // timeOrigin + now() da época con resolución de sub-milisegundo, que es
      // el mismo reloj de los frames.
      events.push({ at_ms: Math.round(performance.timeOrigin + performance.now()), kind, detail });
    },

    /**
     * Envía la grabación al servidor que sirvió esta página.
     *
     * Ruta RELATIVA a propósito: la grabación va al origen propio, no al
     * gateway. Son dos servidores distintos y confundirlos costó una
     * grabación perdida con un 404 que sólo se veía en la traza.
     *
     * @returns {Promise<string|null>} nombre del fichero escrito.
     */
    async save(meta) {
      if (frames.length === 0) return null;

      // Los sellos de captura van en la CABECERA, uno por frame.
      //
      // Sin ellos hay que inventarlos al reproducir, a la cadencia nominal —y
      // la real nunca coincide: el servidor pide 30 fps y llegan 14 a 21—.
      // Con esa deriva, la ventana de un destello cae donde no es y el
      // análisis mide ruido. Bloqueó cinco diagnósticos distintos antes de que
      // se arreglara: la señal salía a cero incluso en grabaciones que en vivo
      // habían correlacionado a 1,0.
      //
      // Son ocho bytes por frame en un fichero de decenas de megas. Baratos.
      const header = JSON.stringify({
        ...meta,
        started_at: new Date(startedAt).toISOString(),
        frames: frames.length,
        frame_captured_at_us: frames.map((f) => f.capturedAtUs),
        events,
      });

      const parts = [new TextEncoder().encode(header + '\n')];
      for (const frame of frames) {
        const prefix = new DataView(new ArrayBuffer(4));
        prefix.setUint32(0, frame.jpeg.length, false);
        parts.push(new Uint8Array(prefix.buffer), frame.jpeg);
      }

      const response = await fetch('/_recording', {
        method: 'POST',
        headers: { 'content-type': 'application/octet-stream' },
        body: new Blob(parts),
      });
      if (!response.ok) throw new Error(`el servidor rechazó la grabación: ${response.status}`);
      return (await response.json()).file;
    },
  };
}
