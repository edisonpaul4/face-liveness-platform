/**
 * Política de calidad JPEG.
 *
 * La compresión agresiva destruye la señal cromática sutil que el analizador
 * necesita: el destello mueve la piel unos pocos niveles de gris, y un JPEG
 * apretado se come justo eso. Así que la calidad sube durante los destellos y
 * baja durante las poses, donde lo que importa es la geometría y el detalle
 * fino sobra.
 *
 * Detalle que parece menor y no lo es: la CALIBRACIÓN se captura con la misma
 * calidad que el destello. La línea base es contra lo que se mide la respuesta
 * cromática, y compararla con otra compresión metería una diferencia que no
 * viene de la piel sino del códec.
 *
 * La MIRADA va tan alta como el destello, por un motivo distinto: el iris
 * ocupa una docena de píxeles y lo que se mide es su desplazamiento dentro de
 * la órbita. Un JPEG apretado emborrona justo ese borde y convierte la señal
 * en ruido.
 *
 * Y el TRAMO DE QUIETUD va al máximo, que es donde se mide el pulso. Ahí la
 * señal es la más pequeña de todo el sistema: un latido cambia el color de la
 * piel un 0,25 %, contra el ~5 % que mueve un destello. Medido sobre un frame
 * real, el JPEG a calidad 0,7 mete un error del 0,024 % en la media del
 * rostro y a 0,95 baja al 0,006 %; hay margen, pero es el único sitio donde
 * el códec y la señal están en el mismo orden de magnitud.
 */

// La escala es de JPEG. Se probó WebP y se descartó: comprime la mitad pero
// tarda 28 ms por frame contra 1 ms, y hundió la cadencia de 21 a 7 fps en una
// sesión real (ver camera.js).
//
// Después se probó bajar la calidad del JPEG, apoyándose en medir cuánto
// distorsiona cada nivel la respuesta diferencial cara/fondo:
//
//     JPEG 0,92   166 KB   error 0,121 %
//     JPEG 0,75    74 KB   error 0,220 %
//     JPEG 0,60    56 KB   error 0,432 %
//
// El destello mueve la piel un 5 %, así que 0,22 % de error parecía sobrar por
// veinte. **Salió mal.** La sesión siguiente dio correlaciones de 0,000 y
// 0,195 —contra 1,0 y 0,88 antes— y la persona acabó rechazada.
//
// No quedó probado que la causa fuera la calidad; la grabación no permitió
// reproducirlo. Pero la lección se sostiene igual: **medir la distorsión del
// códec no predice si la señal sobrevive**. Son cosas distintas, y hasta poder
// medir la segunda, los pasos que deciden se quedan altos.
//
// Bajadas sólo donde no hay señal fina que perder: pose y reposo.
export const QUALITY = {
  /**
   * Destello: **no se baja de 0,92 sin medir la correlación**, no la
   * distorsión.
   *
   * Se bajó a 0,70 apoyándose en que el códec sólo distorsiona un 0,22 % y la
   * señal es del 5 %. El argumento parecía sólido y el resultado fue malo: la
   * siguiente sesión dio correlaciones de 0,000 y 0,195 —contra el 1,0 y 0,88
   * de las anteriores— y la persona acabó RECHAZADA por el suelo de
   * `flash_correlation`.
   *
   * No está probado que la causa fuera la calidad: la grabación no permitió
   * reproducirlo. Pero la señal que decide no es sitio para arriesgar por
   * ahorrar bytes, y el ahorro se saca igual en los pasos que no deciden.
   */
  flash: 0.92,
  /** Calibración: la misma que el destello, a propósito — se comparan entre sí. */
  calibration: 0.92,
  /** Mirada: el iris son pocos píxeles y se mide su BORDE, que es lo primero
   *  que emborrona un códec. Por eso va más alta que el destello. */
  gaze: 0.92,
  /** Pose: interesa la geometría, no el matiz. */
  pose: 0.55,
  /**
   * Tramo de quietud: aquí NO se baja.
   *
   * El pulso mueve el color de la piel un 0,25 %, veinte veces menos que el
   * destello. A calidad baja la distorsión del códec sería del orden de la
   * señal y se la comería entera.
   */
  hold: 0.95,
  /** Entre retos. */
  idle: 0.6,
};

/** Devuelve la calidad JPEG para el reto activo. */
export function qualityFor(challenge) {
  if (!challenge) return QUALITY.idle;
  switch (challenge.kind) {
    case 'flash':
      return QUALITY.flash;
    case 'calibration':
      return QUALITY.calibration;
    case 'gaze':
      return QUALITY.gaze;
    case 'pose':
      return QUALITY.pose;
    case 'hold':
      return QUALITY.hold;
    default:
      return QUALITY.idle;
  }
}
