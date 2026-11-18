// `node build/mezze-props.mjs`: draws the props of mezze.sh's lower page and writes them to mezze/public/img/props/ as
// transparent WebP. Run it by hand after changing build/mezze-props.py or the robot's shapes (build/mezze_shapes.py),
// and commit what it writes. It needs Blender 5.2 (`blender` on the PATH), cwebp, and Google Chrome (or CHROME=<path>).
//
// Blender builds the props from the robot's own shapes (build/mezze-props.py); Chrome then draws them with robot.js's
// own fragment shader and the robot's lights from 3d/robot.bin, so they light exactly as the robot does. A plate is
// drawn twice: once still (plate-<food>.webp, what the page shows until scenes.js runs, and for good without it) and
// once at TURNS turns laid out COLS to a row (plate-<food>-turns.webp, plates.js), at 1.5 times its CSS size to keep
// the sheet light; scenes.js loads the sheet only when its scene comes near. Everything else is one picture at twice
// its CSS size. The anchors it prints are where the page places things on a prop, in the prop's CSS pixels.
import { spawn, spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { COLS, TURNS } from "../mezze/public/plates.js";
import { FS, parse } from "../mezze/public/robot.js";

const SITE_DIR = fileURLToPath(new URL("..", import.meta.url));
const OUT = join(SITE_DIR, "mezze/public/img/props");
const CHROME = process.env.CHROME || "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
// Errors are thrown, so the finally blocks below always stop Chrome and remove the temporary folder.
const fail = (msg) => { throw new Error(msg); };

// What to draw: the picture's CSS size, the height of the view in world units, where the camera looks, its elevation
// and turn in degrees, and how many turns (plates only).
const PLATE = { w: 220, h: 160, ortho: 1.1, at: [0, 0, 0.12], elev: 34 };
export const JOBS = [
  ...["bravas", "toast", "olives", "grapes", "cheese", "prawns"].flatMap((f) => [
    { prop: "plate-" + f, ...PLATE },
    { prop: "plate-" + f, out: `plate-${f}-turns`, ...PLATE, turns: TURNS, scale: 1.5, q: 80 },
  ]),
  { prop: "cloche", ...PLATE, at: [0, 0, 0.42] },
  { prop: "phone", w: 220, h: 400, ortho: 2.2, at: [0, 0, 0], elev: 10 },
  { prop: "laptop-leo", w: 240, h: 190, ortho: 1.5, at: [0, 0.1, 0.42], elev: 34, yaw: 18 },
  { prop: "laptop-ana", w: 240, h: 190, ortho: 1.5, at: [0, 0.1, 0.42], elev: 34, yaw: -18 },
  { prop: "home", w: 190, h: 230, ortho: 1.95, at: [0, 0, 0.78], elev: 34 },
  { prop: "pass", w: 640, h: 440, ortho: 4.3, at: [0, 0, 0.3], elev: 34 },
  { prop: "cazuela", w: 220, h: 140, ortho: 1.0, at: [0, 0, 0.12], elev: 34 },
  { prop: "blanket", w: 640, h: 360, ortho: 3.1, at: [0, 0, 0], elev: 34 },
];

// Runs in the page: draws one prop and returns its sheet as a PNG data URL and its anchors in CSS pixels.
const PAGE_SCRIPT = String.raw`
const VS = "attribute vec3 p; attribute vec3 n; attribute vec4 c; uniform mat4 M, VP; varying vec3 vn; varying vec4 vc;" +
  "void main(){ vec4 w = M * vec4(p, 1.0); vn = mat3(M) * n; vc = c; gl_Position = VP * w; }";
const mul = (a, b) => { const o = new Array(16).fill(0); for (let c = 0; c < 4; c++) for (let r = 0; r < 4; r++) for (let k = 0; k < 4; k++) o[c * 4 + r] += a[k * 4 + r] * b[c * 4 + k]; return o; };
const rotZ = (t) => { const c = Math.cos(t), s = Math.sin(t); return [c, s, 0, 0, -s, c, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1]; };
const nrm = (a) => { const l = Math.hypot(...a); return a.map((x) => x / l); };
function lookAt(eye, at) {
  const sub = (a, b) => a.map((x, i) => x - b[i]), dot = (a, b) => a.reduce((s, x, i) => s + x * b[i], 0);
  const cr = (a, b) => [a[1] * b[2] - a[2] * b[1], a[2] * b[0] - a[0] * b[2], a[0] * b[1] - a[1] * b[0]];
  const z = nrm(sub(eye, at)), x = nrm(cr([0, 0, 1], z)), y = cr(z, x);
  return [x[0], y[0], z[0], 0, x[1], y[1], z[1], 0, x[2], y[2], z[2], 0, -dot(x, eye), -dot(y, eye), -dot(z, eye), 1];
}
const ortho = (l, r, b, t, n, f) => [2 / (r - l), 0, 0, 0, 0, 2 / (t - b), 0, 0, 0, 0, -2 / (f - n), 0, -(r + l) / (r - l), -(t + b) / (t - b), -(f + n) / (f - n), 1];
window.draw = (prop, job, lights, FS, COLS) => {
  const SS = 2, X = job.scale || 2, OW = Math.round(job.w * X), OH = Math.round(job.h * X), W = OW * SS, H = OH * SS;
  const cv = document.createElement("canvas"); cv.width = W; cv.height = H;
  const gl = cv.getContext("webgl", { alpha: true, premultipliedAlpha: false, antialias: true, preserveDrawingBuffer: true });
  if (!gl) throw new Error("no WebGL");
  const sh = (t, s) => { const o = gl.createShader(t); gl.shaderSource(o, s); gl.compileShader(o); if (!gl.getShaderParameter(o, gl.COMPILE_STATUS)) throw new Error(gl.getShaderInfoLog(o)); return o; };
  const pr = gl.createProgram(); gl.attachShader(pr, sh(gl.VERTEX_SHADER, VS)); gl.attachShader(pr, sh(gl.FRAGMENT_SHADER, FS)); gl.linkProgram(pr); gl.useProgram(pr);
  const buf = (arr, name, size) => { gl.bindBuffer(gl.ARRAY_BUFFER, gl.createBuffer()); gl.bufferData(gl.ARRAY_BUFFER, new Float32Array(arr), gl.STATIC_DRAW); const l = gl.getAttribLocation(pr, name); gl.enableVertexAttribArray(l); gl.vertexAttribPointer(l, size, gl.FLOAT, false, 0, 0); };
  // Colour as robot.bin stores it (sRGB and the roughness byte), read the way robot.js reads it: bytes over 255.
  const col = prop.c.map((b) => b / 255);
  buf(prop.p, "p", 3); buf(prop.n, "n", 3); buf(col, "c", 4);
  const e = job.elev * Math.PI / 180, yw = (job.yaw || 0) * Math.PI / 180;
  const d = [Math.sin(yw) * Math.cos(e), -Math.cos(yw) * Math.cos(e), Math.sin(e)];
  const half = job.ortho / 2, aspect = job.w / job.h;
  const VP = mul(ortho(-half * aspect, half * aspect, -half, half, 0.1, 80), lookAt(job.at.map((x, i) => x + d[i] * 30), job.at));
  const U = (n) => gl.getUniformLocation(pr, n);
  gl.uniformMatrix4fv(U("VP"), false, new Float32Array(VP));
  gl.uniform3fv(U("L"), nrm(lights.sun)); gl.uniform3fv(U("F"), nrm(lights.fill)); gl.uniform3fv(U("V"), d);
  gl.uniform3fv(U("LC"), lights.sunColor); gl.uniform3fv(U("FC"), lights.fillColor); gl.uniform3fv(U("SKY"), lights.sky);
  gl.enable(gl.DEPTH_TEST); gl.viewport(0, 0, W, H);
  const turns = job.turns || 1, cols = turns > 1 ? COLS : 1;
  const sheet = document.createElement("canvas"); sheet.width = OW * cols; sheet.height = OH * Math.ceil(turns / cols);
  const sx = sheet.getContext("2d"); sx.imageSmoothingQuality = "high";
  for (let i = 0; i < turns; i++) {
    gl.uniformMatrix4fv(U("M"), false, new Float32Array(rotZ(i / turns * Math.PI * 2)));
    gl.clearColor(0, 0, 0, 0); gl.clear(gl.COLOR_BUFFER_BIT | gl.DEPTH_BUFFER_BIT);
    gl.drawArrays(gl.TRIANGLES, 0, prop.p.length / 3);
    sx.drawImage(cv, (i % cols) * OW, Math.floor(i / cols) * OH, OW, OH);
  }
  const anchors = {};
  for (const [k, a] of Object.entries(prop.anchors)) {
    const v = [0, 1, 2, 3].map((r) => VP[r] * a[0] + VP[4 + r] * a[1] + VP[8 + r] * a[2] + VP[12 + r]);
    anchors[k] = [+((v[0] / v[3] + 1) / 2 * job.w).toFixed(1), +((1 - v[1] / v[3]) / 2 * job.h).toFixed(1)];
  }
  return { png: sheet.toDataURL("image/png"), anchors };
};`;

// A page in a headless Chrome, driven over its DevTools protocol with Node's own WebSocket.
async function openPage(dir) {
  const chrome = spawn(CHROME, ["--headless=new", "--remote-debugging-port=0", `--user-data-dir=${dir}`, "--no-first-run",
    "--use-angle=swiftshader", "--enable-unsafe-swiftshader", "about:blank"], { stdio: ["ignore", "ignore", "pipe"] });
  const browserWs = await new Promise((resolve, reject) => {
    let err = "";
    chrome.stderr.on("data", (b) => { err += b; const m = err.match(/DevTools listening on (ws:\S+)/); if (m) resolve(m[1]); });
    chrome.on("exit", () => reject(new Error(`Chrome exited: ${err.slice(-400)}`)));
    chrome.on("error", (e) => reject(new Error(`could not start Chrome at ${CHROME} (${e.message}); set CHROME=<path>`)));
  });
  const port = new URL(browserWs).port;
  const pages = await (await fetch(`http://127.0.0.1:${port}/json/list`)).json();
  const ws = new WebSocket(pages.find((p) => p.type === "page").webSocketDebuggerUrl);
  await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
  let id = 0;
  const waiting = new Map();
  ws.onmessage = (m) => { const msg = JSON.parse(m.data); waiting.get(msg.id)?.(msg); waiting.delete(msg.id); };
  const evaluate = (expression) => new Promise((resolve, reject) => {
    const n = ++id;
    waiting.set(n, (msg) => {
      const r = msg.result;
      if (msg.error || r.exceptionDetails) reject(new Error(JSON.stringify(msg.error || r.exceptionDetails.exception?.description || r.exceptionDetails)));
      else resolve(r.result.value);
    });
    ws.send(JSON.stringify({ id: n, method: "Runtime.evaluate", params: { expression, returnByValue: true, awaitPromise: true } }));
  });
  // Closing waits for Chrome to exit, so its profile directory can be removed after.
  return { evaluate, close: () => new Promise((r) => {
    ws.close();
    if (chrome.exitCode !== null || chrome.signalCode !== null) return r();
    chrome.once("exit", r); chrome.kill();
  }) };
}

async function main() {
  const tmp = mkdtempSync(join(tmpdir(), "mezze-props-"));
  try {
    const json = join(tmp, "props.json");
    const b = spawnSync("blender", ["-b", "--python", join(SITE_DIR, "build/mezze-props.py"), "--", json], { encoding: "utf8" });
    if (b.status !== 0 || b.error) fail(`Blender failed: ${b.error?.message || b.stderr.slice(-1200) || b.stdout.slice(-1200)}`);
    const { props } = JSON.parse(readFileSync(json, "utf8"));
    const bin = readFileSync(join(SITE_DIR, "mezze/public/3d/robot.bin"));
    const { lights } = parse(bin.buffer.slice(bin.byteOffset, bin.byteOffset + bin.length));
    const page = await openPage(join(tmp, "chrome"));
    try {
      await page.evaluate(PAGE_SCRIPT);
      mkdirSync(OUT, { recursive: true });
      for (const job of JOBS) {
        if (!props[job.prop]) fail(`build/mezze-props.py made no prop "${job.prop}"`);
        const r = await page.evaluate(`draw(${JSON.stringify(props[job.prop])}, ${JSON.stringify(job)}, ${JSON.stringify(lights)}, ${JSON.stringify(FS)}, ${COLS})`);
        const name = job.out || job.prop, png = join(tmp, name + ".png");
        writeFileSync(png, Buffer.from(r.png.split(",")[1], "base64"));
        const w = spawnSync("cwebp", ["-quiet", "-q", String(job.q || 86), "-alpha_q", "100", "-m", "6", png, "-o", join(OUT, name + ".webp")]);
        if (w.status !== 0 || w.error) fail(`cwebp failed on ${name}: ${w.error?.message || w.stderr}`);
        console.log(name, Object.keys(r.anchors).length ? JSON.stringify(r.anchors) : "");
      }
    } finally {
      await page.close();
    }
  } finally {
    rmSync(tmp, { recursive: true, force: true });
  }
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  try {
    await main();
  } catch (e) {
    console.error(`mezze-props: ${e.message}`);
    process.exitCode = 1;
  }
}
