/**
 * Servidor de ficheros para el cliente de pruebas. Sin dependencias, porque
 * este cliente es desechable y no merece un árbol de node_modules.
 *
 * getUserMedia sólo funciona en un origen seguro: localhost cuenta como tal,
 * así que servir en http://localhost basta y no hace falta certificado. Desde
 * otra máquina de la red, NO: ahí hace falta HTTPS.
 *
 *   node web/serve.mjs [puerto] [--gateway http://localhost:8080]
 */

import { createServer } from 'node:http';
import { mkdir, readFile, writeFile, rm } from 'node:fs/promises';
import { execFile } from 'node:child_process';
import { tmpdir } from 'node:os';
import { extname, join, normalize } from 'node:path';
import { fileURLToPath } from 'node:url';
import { dirname } from 'node:path';

const HERE = dirname(fileURLToPath(import.meta.url));
const ROOT = join(HERE, 'public');
const REPO = join(HERE, '..');
const PORT = Number(process.argv.find((a) => /^\d+$/.test(a)) ?? 5173);
const gatewayFlag = process.argv.indexOf('--gateway');
const GATEWAY = gatewayFlag > -1 ? process.argv[gatewayFlag + 1] : 'http://localhost:8080';

const TYPES = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.json': 'application/json; charset=utf-8',
  '.wasm': 'application/wasm',
};

const RECORDINGS = join(HERE, 'harness', 'recordings');

/**
 * Recibe una grabación de sesión y la escribe en disco.
 *
 * Es biometría (CLAUDE.md §6bis): se queda en esta máquina, en un directorio
 * que no se versiona, y sólo llega aquí si la persona marcó la casilla. Este
 * servidor es el de depuración local; no tiene nada que hacer en producción.
 */
async function saveRecording(req, res) {
  const chunks = [];
  let bytes = 0;
  try {
    for await (const chunk of req) {
      bytes += chunk.length;
      // Tope de cordura: una sesión son ~20 s a 720p y 30 fps, unos 60 MB.
      // Trescientos es que algo ha ido mal.
      if (bytes > 300 * 1024 * 1024) {
        res.writeHead(413).end('demasiado grande');
        return;
      }
      chunks.push(chunk);
    }
  } catch (err) {
    // Subida interrumpida: se descarta lo recibido. Media grabación no sirve
    // para reproducir nada, y guardarla dejaría biometría a medias en disco.
    console.error(`grabación interrumpida tras ${(bytes / 1e6).toFixed(1)} MB: ${err.code ?? err.message}`);
    if (!res.headersSent) res.writeHead(400).end('subida interrumpida');
    return;
  }

  await mkdir(RECORDINGS, { recursive: true });
  const body = Buffer.concat(chunks);

  // El nombre lleva el session_id, no sólo la hora.
  //
  // Con nombre por hora no se puede emparejar una grabación con su sesión del
  // log del gateway, que es justo lo que hace falta para diagnosticar: hubo
  // dos ficheros de 20 MB con horas distintas y el MISMO contenido byte a
  // byte —una subida lenta reintentada— y no había forma de saber cuál era
  // cuál. Un fichero que no se sabe de qué sesión es no sirve de prueba.
  let sid = '';
  try {
    const nl = body.indexOf(0x0a);
    if (nl > 0) sid = JSON.parse(body.subarray(0, nl).toString()).session_id ?? '';
  } catch {
    // Cabecera ilegible: se guarda igual, sólo que sin identificar.
  }
  const stamp = new Date().toISOString().replace(/[:.]/g, '-');
  const file = sid ? `session-${sid}-${stamp}.bin` : `session-${stamp}.bin`;
  await writeFile(join(RECORDINGS, file), body);

  console.log(`grabación guardada: harness/recordings/${file} (${(bytes / 1e6).toFixed(1)} MB)`);
  res.writeHead(200, { 'content-type': TYPES['.json'] });
  res.end(JSON.stringify({ file }));
}

/**
 * Analiza una foto o un vídeo con los detectores PASIVOS.
 *
 * Existe porque un ataque grabado no puede responder a un guion aleatorio: no
 * mira al punto ni gira cuando se le pide. Eso lo deja fuera del recorrido
 * normal, y a la vez es justo el material que hay que medir.
 *
 * El archivo se escribe en un temporal, se pasa al analizador y se borra. No
 * se guarda: si es biometría de alguien, no tiene por qué quedarse (§6bis).
 */
async function analyzeMedia(req, res) {
  const name = decodeURIComponent(req.headers['x-filename'] ?? 'media.bin')
    .replace(/[^\w.-]/g, '_')
    .slice(-80);

  const chunks = [];
  let bytes = 0;
  for await (const chunk of req) {
    bytes += chunk.length;
    if (bytes > 200 * 1024 * 1024) {
      res.writeHead(413).end('demasiado grande');
      return;
    }
    chunks.push(chunk);
  }

  const path = join(tmpdir(), `liveness-${Date.now()}-${name}`);
  await writeFile(path, Buffer.concat(chunks));

  try {
    const report = await runAnalyzer(path);
    res.writeHead(200, { 'content-type': TYPES['.json'] });
    res.end(report);
  } catch (error) {
    res.writeHead(500, { 'content-type': TYPES['.json'] });
    res.end(JSON.stringify({ error: String(error.message ?? error) }));
  } finally {
    await rm(path, { force: true });
  }
}

function runAnalyzer(path) {
  const python = join(REPO, 'analyzer', '.venv', 'bin', 'python');
  const script = join(REPO, 'bench', 'runner', 'analyze_media.py');
  return new Promise((resolve, reject) => {
    execFile(python, [script, path, '--json'], { maxBuffer: 32 * 1024 * 1024, timeout: 180_000 },
      (error, stdout, stderr) => {
        if (error) {
          // El analizador escupe avisos de MediaPipe por stderr aunque vaya
          // bien; sólo importa si además no produjo salida.
          reject(new Error(stderr.split('\n').filter(Boolean).at(-1) ?? error.message));
          return;
        }
        resolve(stdout.trim());
      });
  });
}

// Una petición que se corta a mitad NO puede matar el servidor.
//
// Las grabaciones pesan 20-27 MB y suben por un túnel. Si la conexión se cae
// —el móvil cambia de red, el túnel se reinicia, la persona cierra la
// pestaña—, el stream de la petición RECHAZA con `ECONNRESET` y ese rechazo no
// lo captura nadie: Node se lleva el proceso entero por delante.
//
// Pasó varias veces, siempre justo después de una sesión, y el síntoma era un
// error de ngrok en el móvil mientras el gateway seguía perfectamente vivo.
// Costaba un rato entender que lo caído era el estático y no el túnel.
process.on('uncaughtException', (err) => {
  console.error(`petición abortada, el servidor sigue: ${err.code ?? err.message}`);
});
process.on('unhandledRejection', (err) => {
  console.error(`promesa sin capturar, el servidor sigue: ${err?.code ?? err}`);
});

const server = createServer(async (req, res) => {
  if (req.method === 'POST' && req.url === '/_analyze') {
    await analyzeMedia(req, res);
    return;
  }
  if (req.method === 'POST' && req.url === '/_recording') {
    await saveRecording(req, res);
    return;
  }

  // El cliente pregunta a dónde apuntar en vez de llevarlo escrito: así el
  // mismo estático sirve para el gateway local y para uno remoto.
  if (req.url === '/config.json') {
    res.writeHead(200, { 'content-type': TYPES['.json'] });
    res.end(JSON.stringify({ gateway: GATEWAY }));
    return;
  }

  const path = normalize(decodeURIComponent(new URL(req.url, 'http://x').pathname));
  if (path.includes('..')) {
    res.writeHead(403).end('no');
    return;
  }

  try {
    const file = join(ROOT, path === '/' ? 'index.html' : path);
    const body = await readFile(file);
    res.writeHead(200, {
      'content-type': TYPES[extname(file)] ?? 'application/octet-stream',
      // Nada de caché: se está depurando, no sirviendo producción.
      'cache-control': 'no-store',
    });
    res.end(body);
  } catch {
    res.writeHead(404, { 'content-type': 'text/plain; charset=utf-8' }).end('no está');
  }
});

server.listen(PORT, () => {
  console.log(`cliente de pruebas en http://localhost:${PORT}  (gateway: ${GATEWAY})`);
});
