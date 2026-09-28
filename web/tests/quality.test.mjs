/**
 * La calidad JPEG no es una preferencia estética: comprimir de más durante un
 * destello borra la señal cromática que el analizador mide.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { QUALITY, qualityFor } from '../public/lib/quality.js';

test('el destello se envía con más calidad que la pose', () => {
  assert.ok(qualityFor({ kind: 'flash' }) > qualityFor({ kind: 'pose' }));
});

test('la calibración va a la misma calidad que el destello', () => {
  // Si la línea base se captura con otro ajuste del códec, todo lo que se
  // mida contra ella lleva dentro la diferencia del compresor.
  assert.equal(qualityFor({ kind: 'calibration' }), qualityFor({ kind: 'flash' }));
});

test('sin reto activo se usa la calidad de reposo', () => {
  assert.equal(qualityFor(null), QUALITY.idle);
  assert.equal(qualityFor(undefined), QUALITY.idle);
});

test('un reto desconocido no rompe nada', () => {
  assert.ok(qualityFor({ kind: 'lo_que_sea' }) > 0);
});

test('los pasos que miden señal fina van altos; sólo baja lo que no decide', () => {
  // Tres necesidades distintas, y por eso tres calidades distintas.
  //
  // El DESTELLO mide una señal cromática del 5 %: con 0,3 % de distorsión del
  // códec le sobra margen quince veces.
  //
  // La MIRADA no mide color sino el BORDE del iris, que ocupa una docena de
  // píxeles y es lo primero que emborrona un códec. Necesita más, aunque su
  // señal sea más grande.
  //
  // Y el PULSO mueve la piel un 0,25 %, veinte veces menos que el destello.
  // Ahí la distorsión sería del orden de la señal, así que no se baja.
  // El PULSO es la señal más débil del sistema —mueve la piel un 0,25 %—, así
  // que va por encima de todo lo demás.
  assert.ok(qualityFor({ kind: 'hold' }) > qualityFor({ kind: 'flash' }));

  // DESTELLO y MIRADA se quedan altos: el primero porque bajarlo coincidió con
  // dos rechazos de personas legítimas, el segundo porque mide el BORDE del
  // iris, que son doce píxeles y es lo primero que emborrona un códec.
  assert.ok(qualityFor({ kind: 'flash' }) >= 0.9);
  assert.ok(qualityFor({ kind: 'gaze' }) >= 0.9);

  // Y la POSE sí baja: mide geometría, no matiz.
  assert.ok(qualityFor({ kind: 'pose' }) < qualityFor({ kind: 'flash' }));
});

test('la calidad del pulso y del destello no se bajan sin medir el RESULTADO', () => {
  // Guarda explícita, y con historia: bajar la del destello de 0,92 a 0,70
  // parecía seguro —el códec distorsiona un 0,22 % y la señal es del 5 %— y
  // coincidió con dos rechazos de personas legítimas.
  //
  // Medir la distorsión del códec no predice si la señal sobrevive. Quien
  // baje esto que traiga correlaciones medidas, no cuentas de error.
  assert.ok(QUALITY.hold >= 0.92);
  assert.ok(QUALITY.flash >= 0.9);
});
