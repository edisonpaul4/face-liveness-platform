/**
 * Codifica frames fuera del hilo principal.
 *
 * El hilo principal es el mismo que atiende `requestVideoFrameCallback`, así
 * que mientras codifica NO puede recibir el frame siguiente de la cámara. Ese
 * es el cuello medido: la cámara entregaba 22,4 fps y el cliente enviaba 16,5.
 * Y los frames que faltan son señal — el pulso va con la raíz de su número, y
 * el destello se queda sin correlación con pocas muestras por tramo.
 *
 * Sigue siendo JPEG, a propósito. Lo que se mueve es DÓNDE se codifica, no con
 * qué: un códec distinto tocaría todas las señales a la vez y este repositorio
 * ya se quemó una vez razonando desde la distorsión del códec en vez de desde
 * la señal.
 */

let lienzo = null;
let ctx = null;
// Sonda de nitidez: el mismo tamaño y el mismo algoritmo que tenía el hilo
// principal, para que el comportamiento no cambie al mudarse.
let sonda = null;
let sondaCtx = null;

// Los mensajes se procesan EN ORDEN, encadenados.
//
// Un `await` dentro del manejador deja pasar el mensaje siguiente, así que dos
// frames podrían codificarse a la vez y volver desordenados. El gateway
// rechaza los frames fuera de secuencia, así que eso no es una ineficiencia:
// es perder frames.
let cadena = Promise.resolve();

self.onmessage = (evento) => {
  cadena = cadena.then(() => codificar(evento.data)).catch((error) => {
    self.postMessage({ id: evento.data?.id, error: String(error?.message ?? error) });
  });
};

async function codificar({ id, bitmap, width, height, quality, sharpnessFloor }) {
  if (!lienzo || lienzo.width !== width || lienzo.height !== height) {
    lienzo = new OffscreenCanvas(width, height);
    ctx = lienzo.getContext('2d');
    sonda = new OffscreenCanvas(128, 96);
    sondaCtx = sonda.getContext('2d', { willReadFrequently: true });
  }
  ctx.drawImage(bitmap, 0, 0, width, height);

  // La nitidez se mide AQUÍ, no en el hilo principal, y ése es el motivo de
  // que este worker exista tal como está.
  //
  // Medido en el móvil: el filtro costaba 21,7 ms de mediana y codificar 0,9.
  // El grueso no era comprimir sino `getImageData`, que es una lectura de
  // vuelta de la GPU: el hilo se queda parado esperando píxeles. Y mientras
  // está parado no puede atender a la cámara, así que entregaba 19,9 fps y
  // sólo se enviaban 16,3.
  //
  // Aquí el bitmap ya está en la mano, así que la sonda no cuesta un viaje
  // extra ni bloquea a nadie.
  const nitidez = medirNitidez(bitmap);
  bitmap.close();
  if (nitidez < sharpnessFloor) {
    self.postMessage({ id, rejected: 'blurry', sharpness: nitidez });
    return;
  }

  const blob = await lienzo.convertToBlob({ type: 'image/jpeg', quality });
  const buffer = await blob.arrayBuffer();
  // El tipo que VUELVE, no el que se pidió: `convertToBlob` con un formato no
  // soportado devuelve otro en silencio, y un PNG de 480p pesa un mega.
  self.postMessage({ id, bytes: new Uint8Array(buffer), type: blob.type }, [buffer]);
}

/**
 * Gradiente medio sobre una miniatura. Mismo algoritmo que usaba el hilo
 * principal: 128×96, saltando de dos en dos, luminancia Rec.601.
 */
function medirNitidez(bitmap) {
  sondaCtx.drawImage(bitmap, 0, 0, sonda.width, sonda.height);
  const { data, width, height } = sondaCtx.getImageData(0, 0, sonda.width, sonda.height);

  let total = 0;
  let count = 0;
  for (let y = 1; y < height - 1; y += 2) {
    for (let x = 1; x < width - 1; x += 2) {
      const i = (y * width + x) * 4;
      const here = data[i] * 0.299 + data[i + 1] * 0.587 + data[i + 2] * 0.114;
      const right = data[i + 4] * 0.299 + data[i + 5] * 0.587 + data[i + 6] * 0.114;
      const below =
        data[i + width * 4] * 0.299 +
        data[i + width * 4 + 1] * 0.587 +
        data[i + width * 4 + 2] * 0.114;
      total += Math.abs(here - right) + Math.abs(here - below);
      count += 1;
    }
  }
  return count === 0 ? 0 : total / count;
}
