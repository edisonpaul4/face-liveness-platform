/**
 * Captura de cámara a JPEG.
 *
 * 720p a 30 fps, que es lo que impone el servidor. La calidad JPEG cambia con
 * el reto activo: alta en los destellos, donde la señal es un matiz de color,
 * y baja en las poses, donde lo que importa es la geometría (ver quality.js).
 */

import { ENCODING_JPEG } from './protocol.js';

/**
 * Formato de compresión: JPEG, y se probó WebP.
 *
 * WebP comprime MUCHO mejor —a distorsión equivalente pesa la mitad— y aun así
 * está descartado, porque el cuello de botella no es la red sino la CPU del
 * cliente. Medido sobre un frame real de 720p:
 *
 *     JPEG q92    1,3 ms   158 KB
 *     JPEG q70    1,0 ms    63 KB
 *     WebP q60   28,2 ms    27 KB      ← 28 veces más lento
 *
 * A 30 fps el presupuesto por frame son 33 ms. WebP se los come casi enteros
 * él solo. Se probó en una sesión real y la cadencia se desplomó de 21 a
 * **7 fps**: menos bytes por frame, pero un tercio de los frames. Para el
 * pulso, que vive de tener muestras, eso es el peor cambio posible.
 *
 * El ahorro de banda se saca por el otro lado, bajando la calidad del JPEG:
 * q70 pesa un 60 % menos que q92 y cuesta un milisegundo (ver quality.js).
 *
 * Si algún día se quiere WebP de verdad, el camino es `OffscreenCanvas` en un
 * Worker para sacar la codificación del hilo principal. No es un cambio de una
 * línea, y hasta entonces JPEG gana.
 *
 * Un aviso que sobrevive al experimento: `canvas.toBlob` con un tipo que no
 * soporta **no falla** — devuelve PNG en silencio. Un PNG de 720p pesa un mega
 * y saturaría la subida sin que nada lo delatara. Por eso nunca se pide un
 * formato sin comprobar el `blob.type` que vuelve.
 */

/**
 * Resolución de captura por defecto. La impone el servidor en el saludo; esto
 * es sólo lo que se usa si no dice nada.
 *
 * 480p y no 720p, y la razón no es la banda sino la MEDIDA. A 720p el cliente
 * pedía 20,7 Mbit/s de subida —206 KB por frame— y un parón de 1,8 s del
 * enlace se llevó un reto de mirada entero. Al bajar no se pierde precisión:
 * medido sobre 24 frames reales con la cara cerca, a 480p el iris se mide
 * MEJOR —1 frame sin medir contra 4— por 58 KB en vez de 158, porque con la
 * cara ocupando medio encuadre a 720p le sobran píxeles.
 */
export const CAPTURE_WIDTH = 640;
export const CAPTURE_HEIGHT = 480;

/**
 * Captura con UNA tubería de un frame: se entrega al worker el frame de ahora
 * y se devuelve el ANTERIOR, que mientras tanto ya se ha codificado.
 *
 * Sin esto, mover la codificación al worker no compra nada: el bucle de
 * captura la esperaría igual y el reloj de pared por frame sería el mismo. Lo
 * que se gana es solapar la codificación de un frame con la captura del
 * siguiente.
 *
 * Cuesta un frame de latencia, unos 45 ms, y es un coste seguro:
 *
 *   · las dos medidas que dependen del instante —el reparto en fases de la
 *     mirada y la correlación del destello— BUSCAN su retardo, hasta 800 y
 *     600 ms, así que absorben un desplazamiento común;
 *   · y el sello de captura viaja con el frame, no se toca. La latencia sólo
 *     puede hacer que una respuesta parezca más LENTA, nunca más rápida, así
 *     que no puede provocar una acusación de `temporal_response_too_fast`.
 *
 * El primer frame de la sesión sale como `pipeline`: no se pierde, se entrega
 * en la llamada siguiente.
 */
export async function openCamera({ width = CAPTURE_WIDTH, height = CAPTURE_HEIGHT, fps = 30, sharpnessFloor = 0 } = {}) {
  const stream = await navigator.mediaDevices.getUserMedia({
    video: {
      width: { ideal: width },
      height: { ideal: height },
      frameRate: { ideal: fps, max: 60 },
      facingMode: 'user',
      // Balance de blancos MANUAL, y es más importante que fijar la
      // exposición aunque la intuición diga lo contrario.
      //
      // El pulso se mide como una variación de décimas de punto en el color
      // de la piel. El método POS normaliza cada ventana en el tiempo, así
      // que cancela solo la deriva COMÚN a los tres canales — que es lo que
      // hace la auto-exposición. El balance de blancos mueve cada canal por
      // su lado, esa oscilación cromática cae dentro de la banda del pulso, y
      // se lee como latido.
      //
      // Medido sobre escena sintética con el POS real: con una deriva de
      // balance de blancos del 1 %, el AUC de vivo contra superficie plana
      // cae de 0,71 a 0,42 — por debajo de 0,5, o sea que la máscara puntúa
      // MÁS que el rostro vivo.
      //
      // Aquí es una PISTA y casi nunca se cumple: los navegadores ignoran
      // estas restricciones en `getUserMedia` sin decir nada. Medido en un
      // móvil real, se pedía `manual` y `getSettings()` devolvía
      // `continuous` en las tres sesiones que se miraron.
      //
      // Lo que de verdad fija la cámara es `fijarCamara()`, con
      // `applyConstraints` sobre la pista ya arrancada y COMPROBANDO lo que
      // vuelve. Esto se queda como preferencia inicial.
      whiteBalanceMode: 'manual',
    },
    audio: false,
  });

  const video = document.createElement('video');
  video.srcObject = stream;
  video.playsInline = true;
  video.muted = true;
  await video.play();

  // `requestVideoFrameCallback` avisa UNA vez por cada frame que entrega la
  // cámara. Sin él se muestrea por reloj y se copia lo que el <video> esté
  // mostrando, que a menudo es el frame anterior otra vez.
  //
  // Medido sobre una grabación real de 5,16 s: se enviaron 139 frames a 26,9
  // fps de los que **69 eran idénticos al anterior** — 13,6 fps de información
  // real, con rachas de hasta 4 copias del mismo frame. La cámara entregaba la
  // mitad de lo pedido (a 720p con poca luz alarga la exposición y baja el
  // ritmo sola) y nadie se enteraba.
  //
  // Los duplicados no son sólo banda desperdiciada. Cada copia lleva un sello
  // de captura NUEVO con píxeles de hasta 150 ms antes, así que el sello
  // miente; y ocupan las plazas de `MaxInflight` del analizador, con lo que el
  // frame realmente nuevo puede acabar rechazado sin llegar a enviarse.
  let framesDelivered = 0;
  let deliveredAtUs = 0;
  let duplicates = 0;
  let usedFrame = -1;
  const tracksFrames = typeof video.requestVideoFrameCallback === 'function';
  if (tracksFrames) {
    const onFrame = (now) => {
      framesDelivered += 1;
      // `now` va en el reloj de performance.now(); sumarle el origen lo pasa a
      // época, que es el reloj en el que el servidor valida la deriva.
      deliveredAtUs = Math.round((performance.timeOrigin + now) * 1000);
      video.requestVideoFrameCallback(onFrame);
    };
    video.requestVideoFrameCallback(onFrame);
  }

  // El lienzo toma la RELACIÓN DE ASPECTO REAL de la cámara, no la pedida.
  //
  // `drawImage` con un rectángulo de destino no recorta: escala. Y una cámara
  // de móvil en vertical entrega 480x640 aunque se le pidan 640x480, así que
  // se estaba metiendo un retrato en un lienzo apaisado y **cada frame salía
  // con la cara 1,78 veces más ancha de lo que es**.
  //
  // No es cosmético, y no afecta a todo por igual. Medido sobre grabaciones
  // reales, deshaciendo el estirón:
  //
  //     gaze_offset_x    x1,02   (invariante: el iris se normaliza por el
  //                               ancho del ojo, y los dos son horizontales)
  //     pose_yaw_deg     x1,141  (una cara ensanchada parece MENOS girada)
  //     pose_pitch_deg   x0,735  (y más inclinada)
  //     gaze_openness    x1,636  (alto entre ancho: el estirón la aplastaba)
  //
  // Y la corrección no sólo cambia los números: los hace más consistentes. La
  // contrarrotación del iris contra el giro de cabeza correlaciona a -0,85 o
  // mejor en 7 de 8 pasos de pose con la geometría corregida, contra 5 de 8
  // con la estirada.
  //
  // Lo peor del estirón era que dependía del DISPOSITIVO: una webcam de
  // portátil es apaisada y no lo sufre, un móvil en vertical sí. Los mismos
  // umbrales no podían valer para los dos.
  //
  // Se conserva el número de píxeles pedido, repartido según el aspecto real:
  // así el peso por frame no cambia y el campo de visión tampoco.
  const nativo = video.videoWidth > 0 && video.videoHeight > 0
    ? video.videoWidth / video.videoHeight
    : width / height;
  const area = width * height;
  const par = (v) => Math.max(2, Math.round(v / 2) * 2);
  const canvas = document.createElement('canvas');
  canvas.width = par(Math.sqrt(area * nativo));
  canvas.height = par(Math.sqrt(area / nativo));
  const ctx = canvas.getContext('2d');
  const codec = { mime: 'image/jpeg', code: ENCODING_JPEG };

  // Codificación fuera del hilo principal, si el navegador da las tres piezas.
  //
  // Si falta alguna se sigue por el camino de siempre: un cliente que no
  // arranca no mide nada, y este atajo sólo cambia DÓNDE se codifica.
  const puedeWorker =
    typeof Worker === 'function' &&
    typeof OffscreenCanvas === 'function' &&
    typeof createImageBitmap === 'function';
  let worker = null;
  const pendientes = new Map();
  let siguienteId = 0;
  // Frames ya entregados al worker, en orden de captura.
  const enVuelo = [];
  if (puedeWorker) {
    try {
      worker = new Worker(new URL('./encoder-worker.js', import.meta.url));
      worker.onmessage = ({ data }) => {
        const resolver = pendientes.get(data.id);
        if (!resolver) return;
        pendientes.delete(data.id);
        resolver(data);
      };
    } catch {
      worker = null;
    }
  }

  /**
   * Entrega el frame de ahora al worker y devuelve el ANTERIOR.
   *
   * Solapar es todo el propósito: sin la tubería, el bucle de captura
   * esperaría al worker igual que esperaba a `toBlob` y el reloj de pared por
   * frame no cambiaría. Ver la nota de `openCamera`.
   */
  async function grabConWorker(quality, capturedAtUs) {
    const bitmap = await createImageBitmap(video);
    const id = ++siguienteId;
    const respuesta = new Promise((resolve) => pendientes.set(id, resolve));
    worker.postMessage({ id, bitmap, width: canvas.width, height: canvas.height, quality, sharpnessFloor }, [bitmap]);
    enVuelo.push(
      respuesta.then((data) => {
        // El worker mide la nitidez sobre el bitmap que ya tiene, así que el
        // rechazo por frame movido vuelve por aquí en vez de decidirse antes.
        if (data.rejected) return { rejected: data.rejected };
        if (data.error || data.type !== codec.mime) return null;
        return { jpeg: data.bytes, capturedAtUs, encoding: codec.code };
      }),
    );

    // La tubería tiene un hueco: se devuelve el más viejo sólo cuando hay otro
    // detrás codificándose.
    if (enVuelo.length < 2) return { rejected: 'pipeline' };
    return enVuelo.shift();
  }

  return {
    video,
    stream,
    codec,
    settings: stream.getVideoTracks()[0]?.getSettings() ?? {},

    /**
     * ¿Hay un frame que no se haya enviado ya?
     *
     * Se consulta ANTES del filtro local: pasar MediaPipe sobre una copia del
     * frame anterior gasta el mismo CPU del cliente que es justo lo que limita
     * el ritmo de captura.
     */
    hasFreshFrame() {
      return !tracksFrames || framesDelivered !== usedFrame;
    },

    /** Cuántos frames entregó la cámara y cuántas copias se evitaron. */
    stats() {
      return { delivered: framesDelivered, duplicates, tracksFrames };
    },

    /**
     * Congela exposición, balance de blancos y enfoque.
     *
     * Es la diferencia entre medir el destello y no medirlo, y no es una
     * optimización: con los lazos automáticos activos **la cámara lucha
     * contra el estímulo**. Ve la escena teñirse de rojo y baja la ganancia
     * del rojo para compensar, que es literalmente su trabajo.
     *
     * Medido dentro de un tramo de destello real: la respuesta del rostro
     * sube hasta un máximo y luego decae sola, y en el tramo siguiente el
     * canal del color nuevo llega a irse un 12 % al lado CONTRARIO — la
     * cámara ya se había acomodado al color anterior y al cambiar se desanda.
     *
     * Con la habitación a oscuras el destello es tan grande que sobrevive a
     * esa compensación: correlaciones de 0,94 a 0,99. Con la habitación
     * clara se la come entera: 0,01 a 0,60, con la modulación cayendo de
     * 0,17-0,30 a 0,01-0,046. La luz ambiente no rompe la física del
     * destello; rompe lo que la cámara deja de él.
     *
     * Se llama cuando la persona ya está encuadrada y la cámara ha
     * convergido: congelar antes fijaría una exposición de otra escena.
     *
     * Devuelve qué se pidió y qué quedó DE VERDAD, porque pedirlo no es
     * conseguirlo: `getUserMedia` ignora estas restricciones en silencio, y
     * así fue como esto pasó desapercibido.
     */
    async lockExposure() {
      const track = stream.getVideoTracks()[0];
      if (!track?.applyConstraints) return { soportado: false, motivo: 'sin applyConstraints' };

      const caps = track.getCapabilities?.() ?? {};
      const antes = track.getSettings?.() ?? {};

      // Se pide el MODO **y el VALOR**, y ése fue el error la primera vez.
      //
      // Poner `exposureMode: 'manual'` a secas no congela nada: la cámara se
      // queda en `none` —ni automático ni un valor definido— y la exposición
      // se va a donde le parece. Medido en un móvil real: `exposureTime`
      // saltó de 200 a 36, la imagen salió lavada, el reflejo especular se
      // disparó a 1 y la calidad de captura tumbó la sesión.
      //
      // El valor que se fija es el que la cámara YA había elegido: la persona
      // está encuadrada y el lazo automático ha convergido, así que es la
      // mejor exposición disponible para esta escena. Sólo se le pide que
      // deje de moverla.
      const dentro = (v, rango) => {
        if (v == null || !rango) return v ?? undefined;
        return Math.min(rango.max ?? v, Math.max(rango.min ?? v, v));
      };
      const pedido = {};
      if (caps.exposureMode?.includes('manual') && antes.exposureTime != null) {
        pedido.exposureMode = 'manual';
        pedido.exposureTime = dentro(antes.exposureTime, caps.exposureTime);
        if (caps.iso && antes.iso != null) pedido.iso = dentro(antes.iso, caps.iso);
      }
      if (caps.whiteBalanceMode?.includes('manual')) {
        pedido.whiteBalanceMode = 'manual';
        // El color de referencia sólo se fija si la cámara da uno creíble:
        // este móvil devolvía 0 K, que no es una temperatura de color.
        if (caps.colorTemperature && antes.colorTemperature > 0) {
          pedido.colorTemperature = dentro(antes.colorTemperature, caps.colorTemperature);
        }
      }
      if (Object.keys(pedido).length === 0) {
        return { soportado: false, motivo: 'la cámara no ofrece exposición ni blancos manuales', caps };
      }

      try {
        await track.applyConstraints({ advanced: [pedido] });
      } catch (error) {
        return { soportado: false, motivo: String(error?.message ?? error), caps };
      }

      const ahora = track.getSettings?.() ?? {};
      const congelada =
        (pedido.exposureMode == null || ahora.exposureMode === 'manual') &&
        (pedido.whiteBalanceMode == null || ahora.whiteBalanceMode === 'manual');

      if (!congelada) {
        // **Se deshace.** Verificar y seguir adelante igual fue lo que dejó la
        // cámara en un estado peor que el de partida: `none` no es `manual`,
        // es "sin definir". Una sesión con auto-ajuste mide el destello con
        // dificultad; una con la exposición perdida no mide NADA.
        try {
          await track.applyConstraints({
            advanced: [{ exposureMode: 'continuous', whiteBalanceMode: 'continuous' }],
          });
        } catch {
          /* si tampoco se puede deshacer, no hay nada mejor que hacer */
        }
        return {
          soportado: false,
          motivo: `no cuajó (exposición ${ahora.exposureMode}, blancos ${ahora.whiteBalanceMode})`,
          pedido,
          caps,
        };
      }

      return {
        soportado: true,
        pedido,
        exposureMode: ahora.exposureMode,
        whiteBalanceMode: ahora.whiteBalanceMode,
        exposureTime: ahora.exposureTime,
        iso: ahora.iso,
        caps,
      };
    },

    stop() {
      worker?.terminate();
      for (const track of stream.getTracks()) track.stop();
    },
  };
}
