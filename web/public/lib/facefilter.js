/**
 * Filtro de calidad local: rostro presente y encuadre nítido.
 *
 * **Esto NO puntúa nada.** Es un filtro de captura: sirve para no gastar red
 * mandando frames que el analizador va a descartar igualmente. La decisión de
 * si hay una persona real delante se toma en el servidor, con el analizador de
 * verdad, sobre los frames que sí llegan.
 *
 * Ponerlo a decidir sería regalarle la decisión al cliente, que es hostil por
 * definición: bastaría con parchear esta función para "aprobar" cualquier
 * cosa. Por eso lo único que puede hacer es tirar frames, nunca aprobarlos.
 *
 * Si MediaPipe no carga —sin red, CDN caída— el filtro se queda en el
 * detector de desenfoque y lo dice en el panel. Un instrumento que no arranca
 * sin internet no sirve como instrumento.
 */

const MODEL_URL =
  'https://storage.googleapis.com/mediapipe-models/face_detector/blaze_face_short_range/float16/1/blaze_face_short_range.tflite';
const WASM_URL = 'https://cdn.jsdelivr.net/npm/@mediapipe/tasks-vision@0.10.14/wasm';
const VISION_URL = 'https://cdn.jsdelivr.net/npm/@mediapipe/tasks-vision@0.10.14';

/** Por debajo de esta energía de gradiente, el frame está movido o desenfocado. */
export const SHARPNESS_FLOOR = 6.0;

export async function createFaceFilter({ sharpnessFloor = SHARPNESS_FLOOR } = {}) {
  const probe = document.createElement('canvas');
  probe.width = 128;
  probe.height = 96;
  const probeCtx = probe.getContext('2d', { willReadFrequently: true });

  let detector = null;
  // Cada cuánto se vuelve a preguntar si hay cara.
  //
  // Un segundo, y el número sale de una medida, no del gusto. Esta
  // comprobación es lo ÚNICO que queda del filtro en el hilo principal
  // —MediaPipe necesita el `<video>`— y cuesta 28 ms cada vez. A 4 veces por
  // segundo estiraba cuatro turnos de captura al año, y con ellos se perdían
  // 79 frames de los 486 que la cámara entregó en una sesión de 24 s.
  //
  // Lo que compraba: NADA, medido. En esa misma sesión el filtro rechazó cero
  // frames por falta de cara y cero por borrosos.
  //
  // Sigue existiendo porque si alguien se va de delante de la cámara hay que
  // dejar de enviar, y una cara no desaparece del encuadre en menos de un
  // segundo. Los que se cuelen los descarta el servidor, que decide igual.
  const FACE_CHECK_MS = 1000;
  let lastFaceCheckMs = -Infinity;
  let lastFaceSeen = true;
  let status = 'cargando';

  try {
    const vision = await import(/* @vite-ignore */ VISION_URL);
    const fileset = await vision.FilesetResolver.forVisionTasks(WASM_URL);
    detector = await vision.FaceDetector.createFromOptions(fileset, {
      baseOptions: { modelAssetPath: MODEL_URL, delegate: 'CPU' },
      runningMode: 'VIDEO',
      minDetectionConfidence: 0.5,
    });
    status = 'mediapipe';
  } catch (error) {
    // Degradación consciente: sin detector, sólo se filtra el desenfoque.
    status = `sólo nitidez (MediaPipe no cargó: ${error?.message ?? error})`;
  }

  return {
    get status() {
      return status;
    },

    /**
     * Decide si un frame merece la pena enviarse.
     * @returns {{ok: true, sharpness: number}|{ok: false, reason: 'noFace'|'blurry', sharpness: number}}
     */
    inspect(video, timestampMs) {
      const sharpness = measureSharpness(probeCtx, probe, video);
      if (sharpness < sharpnessFloor) {
        return { ok: false, reason: 'blurry', sharpness };
      }

      // El detector NO corre en cada frame, y eso es deliberado.
      //
      // Su trabajo es ahorrar subida no enviando frames sin cara. Pero corre
      // en el hilo principal, que es el mismo que captura y codifica, así que
      // lo que ahorra en bytes lo cobra en FRAMES — y los frames son la
      // señal: el pulso va con la raíz de su número y el destello se queda
      // sin correlación con pocas muestras por tramo.
      //
      // Medido en una sesión real: la cámara entregó 533 frames y el cliente
      // envió 383. Los 150 que faltan no los perdió la red —iba al 40 % de su
      // capacidad, sin un solo parón— sino este detector. Con la subida
      // holgada, el cambio sale a favor por goleada.
      //
      // Cada 250 ms basta: una cara no entra ni sale del encuadre más rápido
      // que eso, y los frames sin cara que se cuelen los descarta el servidor,
      // que es quien decide de todas formas (§6).
      if (detector && timestampMs - lastFaceCheckMs >= FACE_CHECK_MS) {
        lastFaceCheckMs = timestampMs;
        const { detections } = detector.detectForVideo(video, timestampMs);
        lastFaceSeen = Boolean(detections && detections.length > 0);
      }
      if (detector && !lastFaceSeen) {
        return { ok: false, reason: 'noFace', sharpness };
      }
      return { ok: true, sharpness };
    },

    /**
     * ¿Sigue habiendo una cara delante?
     *
     * Es lo único del filtro que queda en el hilo principal: MediaPipe
     * necesita el elemento `<video>`. La nitidez se mide en el worker, sobre
     * el bitmap que ya tiene para codificar.
     */
    faceStillThere(video, timestampMs) {
      if (!detector) return { ok: true };
      if (timestampMs - lastFaceCheckMs >= FACE_CHECK_MS) {
        lastFaceCheckMs = timestampMs;
        const { detections } = detector.detectForVideo(video, timestampMs);
        lastFaceSeen = Boolean(detections && detections.length > 0);
      }
      return lastFaceSeen ? { ok: true } : { ok: false, reason: 'noFace' };
    },

    /**
     * Dónde está la cara y cuánto ocupa, para guiar el encuadre ANTES de
     * empezar. No puntúa nada: sólo sirve para pedirle a la persona que se
     * acerque, igual que el filtro de nitidez sólo sirve para no enviar
     * frames movidos. Quien decide sigue siendo el servidor.
     *
     * @returns {{ok: true, area: number, cx: number, cy: number}|{ok: false}}
     */
    locate(video, timestampMs) {
      if (!detector) return { ok: false };

      const { detections } = detector.detectForVideo(video, timestampMs);
      const box = detections?.[0]?.boundingBox;
      if (!box) return { ok: false };

      const w = video.videoWidth || 1;
      const h = video.videoHeight || 1;
      return {
        ok: true,
        // Fracción del encuadre que ocupa la cara: la misma magnitud que el
        // servidor llama face_area_ratio, para poder comparar peras con peras.
        area: (box.width * box.height) / (w * h),
        cx: (box.originX + box.width / 2) / w,
        cy: (box.originY + box.height / 2) / h,
      };
    },
  };
}

/**
 * Energía de gradiente sobre una miniatura en gris.
 *
 * Es un estimador pobre comparado con la varianza del laplaciano que usa el
 * analizador, y da igual: aquí sólo hay que distinguir "movido" de "quieto",
 * y tiene que costar menos de un milisegundo para no comerse el presupuesto
 * de captura.
 */
function measureSharpness(ctx, canvas, video) {
  ctx.drawImage(video, 0, 0, canvas.width, canvas.height);
  const { data, width, height } = ctx.getImageData(0, 0, canvas.width, canvas.height);

  let total = 0;
  let count = 0;
  for (let y = 1; y < height - 1; y += 2) {
    for (let x = 1; x < width - 1; x += 2) {
      const i = (y * width + x) * 4;
      const here = data[i] * 0.299 + data[i + 1] * 0.587 + data[i + 2] * 0.114;
      const right = data[i + 4] * 0.299 + data[i + 5] * 0.587 + data[i + 6] * 0.114;
      const below = data[i + width * 4] * 0.299 + data[i + width * 4 + 1] * 0.587 + data[i + width * 4 + 2] * 0.114;
      total += Math.abs(here - right) + Math.abs(here - below);
      count += 1;
    }
  }
  return count > 0 ? total / count : 0;
}
