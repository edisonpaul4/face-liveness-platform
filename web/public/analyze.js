/**
 * Banco de ataques: análisis pasivo de un archivo.
 *
 * Sube la foto o el vídeo al servidor de depuración, que lo pasa por el mismo
 * Pipeline del analizador y devuelve las señales. No se guarda: el archivo se
 * borra en cuanto se analiza.
 */

const el = (id) => document.getElementById(id);

/**
 * Cómo leer cada señal.
 *
 * `alto` dice si un valor ALTO delata un ataque. Se usa sólo para colorear:
 * quien concluye sigue siendo Go, y aquí ni siquiera hay veredicto que dar.
 */
// Los umbrales salen de bench/runner/bench_dataset.py sobre el banco de
// ataques, no de redondear el softmax a 0,5. La clase "ataque" de MiniFASNet
// vive por debajo de la milésima: con un corte en 0,5 esta página no marcaría
// jamás un ataque y quien la usa concluiría que su ataque funcionó.
//
// `surface_specular_fraction` no aparece a propósito: deja pasar entre el
// 62 % y el 91 % de los ataques medidos. Enseñarlo como detector sería
// anunciar una defensa que no existe.
const LECTURA = {
  texture_pad_v2_a: { alto: true, umbral: 0.0085, que: 'modelo PAD v2' },
  texture_pad_v2_b: { alto: true, umbral: 0.0085, que: 'modelo PAD v2 (2ª familia)' },
  texture_pad_v1se_a: { alto: true, umbral: 0.0016, que: 'modelo PAD v1se' },
  texture_pad_v1se_b: { alto: true, umbral: 0.0016, que: 'modelo PAD v1se (2ª familia)' },
  surface_moire_index: { alto: true, umbral: 0.017, que: 'rejilla de píxeles: delata pantalla' },
  surface_banding_index: { alto: true, umbral: 0.040, que: 'bandeo por refresco: delata pantalla' },
};

el('file').addEventListener('change', () => {
  el('run').disabled = !el('file').files.length;
  el('estado').textContent = el('file').files.length
    ? `Listo: ${el('file').files[0].name}`
    : 'Elige una foto o un vídeo.';
});

el('run').addEventListener('click', async () => {
  const file = el('file').files[0];
  if (!file) return;

  el('run').disabled = true;
  el('resultado').hidden = true;
  el('estado').textContent = `Analizando ${file.name}… (un vídeo tarda)`;

  try {
    const response = await fetch('/_analyze', {
      method: 'POST',
      headers: { 'x-filename': encodeURIComponent(file.name) },
      body: file,
    });
    const report = await response.json();
    if (report.error) throw new Error(report.error);
    render(report, file.name);
    el('estado').textContent = `Analizado: ${file.name}`;
  } catch (error) {
    el('estado').textContent = `No se pudo analizar: ${error.message}`;
  } finally {
    el('run').disabled = false;
  }
});

function render(report, name) {
  el('resultado').hidden = false;

  const resumen = el('resumen');
  resumen.textContent = '';
  fila(resumen, 'archivo', name);
  fila(resumen, 'frames analizados', String(report.frames));
  fila(resumen, 'frames con rostro', String(report.frames_with_face));
  fila(resumen, 'recorrido de yaw', `${report.yaw_span_deg}°`);

  // Qué detectores lo señalan. No es un veredicto: es quién levanta la mano.
  const acusan = [];
  for (const [nombre, stats] of Object.entries(report.signals)) {
    const lectura = LECTURA[nombre];
    if (!lectura) continue;
    const valor = lectura.alto ? stats.max : stats.min;
    const sospecha = lectura.alto ? valor >= lectura.umbral : valor <= lectura.umbral;
    if (sospecha) acusan.push(`${nombre} (${valor.toFixed(3)})`);
  }
  fila(resumen, 'detectores que lo señalan', acusan.length ? acusan.join(' · ') : 'ninguno');

  const cuerpo = el('senales').querySelector('tbody');
  cuerpo.textContent = '';
  for (const [nombre, stats] of Object.entries(report.signals)) {
    const lectura = LECTURA[nombre];
    const tr = document.createElement('tr');
    if (lectura) {
      const valor = lectura.alto ? stats.max : stats.min;
      const sospecha = lectura.alto ? valor >= lectura.umbral : valor <= lectura.umbral;
      tr.className = sospecha ? 'fail' : '';
      tr.title = lectura.que;
    }
    for (const texto of [nombre, stats.min.toFixed(4), stats.median.toFixed(4), stats.max.toFixed(4)]) {
      const td = document.createElement('td');
      td.textContent = texto;
      tr.appendChild(td);
    }
    cuerpo.appendChild(tr);
  }

  const pose = report.pose_window;
  if (!pose) {
    el('pose').textContent = report.pose_window_reason ?? 'sin ventana de pose';
    return;
  }
  // El paralaje es lo que separa una superficie plana de un rostro con
  // volumen, y sólo se puede medir si la cabeza se movió.
  const sub = Object.entries(pose.submetrics ?? {})
    .map(([k, v]) => `${k}: ${v === null ? 'no medida' : v}`)
    .join(' · ');
  el('pose').textContent = `score ${pose.score} · ${sub}`;
}

function fila(dl, termino, valor) {
  const dt = document.createElement('dt');
  dt.textContent = termino;
  const dd = document.createElement('dd');
  dd.textContent = valor;
  dl.append(dt, dd);
}
