// mezze.sh's live robot: the robot of Mezze's picture, drawn with WebGL from /3d/robot.bin (written by
// build/mezze-robot.py), balancing its six plates. It sways a little at rest; the pointer is a hand that can touch
// it: the part it touches (an arm, the head, a plate, the body) is pushed the way the pointer moves, harder the faster
// it moves, and every joint springs back. The plates tilt, slide and wobble on their sticks but never fall: every
// angle and slide is clamped. The head looks towards a pointer nearby.
//
// It never blocks the page: it starts after the load event, when the browser is idle, and only when the picture is
// on screen. Until its first frame, and for good when there is no WebGL, when the reader prefers reduced motion, or
// without script, the picture (img/robot.webp, the same pose) stays.
import { SPIN } from "./plates.js"; // radians a second: the plates turn on their sticks

const browser = typeof document !== "undefined";
const stage = browser ? document.querySelector(".stage") : null;
const img = stage?.querySelector("img");
const reduce = browser ? matchMedia("(prefers-reduced-motion: reduce)") : { matches: true };

// ---------- small 4x4 matrix helpers (column-major, as WebGL takes them) ----------
const I = () => new Float32Array([1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1]);
function mul(a, b) {
  const o = new Float32Array(16);
  for (let c = 0; c < 4; c++) for (let r = 0; r < 4; r++) {
    let s = 0;
    for (let k = 0; k < 4; k++) s += a[k * 4 + r] * b[c * 4 + k];
    o[c * 4 + r] = s;
  }
  return o;
}
const T = (x, y, z) => { const m = I(); m[12] = x; m[13] = y; m[14] = z; return m; };
function R(ax, ay, az, t) {
  const l = Math.hypot(ax, ay, az) || 1, x = ax / l, y = ay / l, z = az / l, c = Math.cos(t), s = Math.sin(t), k = 1 - c;
  return new Float32Array([x * x * k + c, y * x * k + z * s, z * x * k - y * s, 0, x * y * k - z * s, y * y * k + c, z * y * k + x * s, 0,
    x * z * k + y * s, y * z * k - x * s, z * z * k + c, 0, 0, 0, 0, 1]);
}
const about = (p, m) => mul(T(p[0], p[1], p[2]), mul(m, T(-p[0], -p[1], -p[2])));
// The inverse of a rigid transform (rotation and translation only, which is all this file makes).
function rigidInverse(m) {
  const o = I();
  for (let r = 0; r < 3; r++) for (let c = 0; c < 3; c++) o[c * 4 + r] = m[r * 4 + c];
  for (let r = 0; r < 3; r++) o[12 + r] = -(o[r] * m[12] + o[4 + r] * m[13] + o[8 + r] * m[14]);
  return o;
}
const apply = (m, p, w = 1) => [0, 1, 2].map((r) => m[r] * p[0] + m[4 + r] * p[1] + m[8 + r] * p[2] + m[12 + r] * w);
const sub = (a, b) => [a[0] - b[0], a[1] - b[1], a[2] - b[2]];
const cross = (a, b) => [a[1] * b[2] - a[2] * b[1], a[2] * b[0] - a[0] * b[2], a[0] * b[1] - a[1] * b[0]];
const dot = (a, b) => a[0] * b[0] + a[1] * b[1] + a[2] * b[2];
const clamp = (v, lo, hi) => Math.min(hi, Math.max(lo, v));

// ---------- the model file ----------
export function parse(buf) {
  const dv = new DataView(buf);
  if (new TextDecoder().decode(new Uint8Array(buf, 0, 4)) !== "MZR1") throw new Error("not a robot file");
  const hlen = dv.getUint32(4, true);
  const head = JSON.parse(new TextDecoder().decode(new Uint8Array(buf, 8, hlen)).replace(/\0+$/, ""));
  let at = 8 + hlen;
  const take = (Type, n) => { const a = new Type(buf, at, n); at += Math.ceil((n * Type.BYTES_PER_ELEMENT) / 4) * 4; return a; };
  const { centre, scale } = head.quant;
  for (const p of head.parts) {
    p.pos = take(Int16Array, p.verts * 3);
    p.nor = take(Int8Array, p.verts * 3);
    p.col = take(Uint8Array, p.verts * 4);
    p.idx = take(Uint16Array, p.tris * 3);
    p.world = new Float32Array(p.verts * 3); // the rest pose, for picking
    for (let i = 0; i < p.verts * 3; i++) p.world[i] = centre[i % 3] + (p.pos[i] / 32767) * scale;
  }
  return head;
}

// ---------- the joints: springs with limits ----------
// Each joint turns about its pivot by roll (about the world's y, which faces the camera: a sideways tip), pitch (about x)
// and yaw (about z). k is stiffness, c damping, max the clamp in radians. A plate also slides on its stick, as far as
// SLIDE and no further.
const DEG = Math.PI / 180;
export const JOINTS = {
  root: { k: 16, c: 3.0, max: 9 * DEG, inertia: 3.2 },
  kick: { k: 40, c: 5, max: 14 * DEG, inertia: 1.2 },
  body: { k: 30, c: 4.2, max: 12 * DEG, inertia: 2.0 },
  arm_l: { k: 38, c: 4.5, max: 20 * DEG, inertia: 0.9 },
  arm_r: { k: 38, c: 4.5, max: 20 * DEG, inertia: 0.9 },
  head: { k: 34, c: 5.0, max: 18 * DEG, inertia: 0.8 },
  plate: { k: 22, c: 2.2, max: 22 * DEG, inertia: 0.35 },
};
export const SLIDE = 0.09; // world units: a plate's centre stays this close to its stick's top, so it never comes off

export function joint(kind) {
  return { kind, j: JOINTS[kind], a: [0, 0, 0], v: [0, 0, 0], goal: [0, 0, 0], s: [0, 0], sv: [0, 0] };
}

const finite = (xs) => xs.every(Number.isFinite);

// One step of a joint's springs. A joint that has gone non-finite (NaN compares false against every limit) starts
// again from rest rather than drawing nothing.
export function step(J, dt) {
  const { k, c, max } = J.j;
  if (!finite(J.a) || !finite(J.v)) { J.a = [0, 0, 0]; J.v = [0, 0, 0]; }
  for (let i = 0; i < 3; i++) {
    J.v[i] += (-k * (J.a[i] - J.goal[i]) - c * J.v[i]) * dt;
    J.a[i] += J.v[i] * dt;
    if (Math.abs(J.a[i]) > max) { // the hard limit: never past it, and a small bounce back
      J.a[i] = Math.sign(J.a[i]) * max;
      if (J.v[i] * J.a[i] > 0) J.v[i] *= -0.3;
    }
  }
}

// One step of a plate's slide on its stick: downhill when tilted, a spring back to the stick, and never further than
// SLIDE from it, so the stick's top always stays under the plate.
export function slide(J, dt) {
  if (!finite(J.s) || !finite(J.sv)) { J.s = [0, 0]; J.sv = [0, 0]; }
  for (let i = 0; i < 2; i++) {
    J.sv[i] += (Math.sin(J.a[i === 0 ? 0 : 1]) * (i === 0 ? 2.4 : -2.4) - 26 * J.s[i] - 3.2 * J.sv[i]) * dt;
    J.s[i] += J.sv[i] * dt;
  }
  const l = Math.hypot(J.s[0], J.s[1]);
  if (l > SLIDE) { J.s[0] *= SLIDE / l; J.s[1] *= SLIDE / l; J.sv[0] *= -0.3; J.sv[1] *= -0.3; }
}

// ---------- start and stop ----------
// The page loads this module only when motion is welcome; it stops when the reader turns reduced motion on, and starts
// again when they turn it off.
let stop = null, booting = false, modelBuf = null;
if (stage && img) {
  const start = () => (window.requestIdleCallback ? requestIdleCallback(boot, { timeout: 2500 }) : setTimeout(boot, 200));
  if (!reduce.matches) {
    if (document.readyState === "complete") start();
    else addEventListener("load", start, { once: true });
  }
  reduce.addEventListener?.("change", () => {
    if (reduce.matches) stop?.();
    else if (!stop && !booting) start();
  });
}

async function boot() {
  if (stop || booting || reduce.matches) return;
  booting = true;
  try {
    const canvas = document.createElement("canvas");
    canvas.setAttribute("aria-hidden", "true");
    const gl = canvas.getContext("webgl", { antialias: true, alpha: true, premultipliedAlpha: true, powerPreference: "low-power" });
    if (!gl) return; // the picture stays
    let model;
    try {
      if (!modelBuf) {
        const res = await fetch("/3d/robot.bin");
        if (!res.ok) return;
        modelBuf = await res.arrayBuffer();
      }
      model = parse(modelBuf.slice(0));
    } catch {
      return;
    }
    if (reduce.matches) return;
    stage.append(canvas);
    try {
      stop = run(gl, canvas, model);
    } catch {
      canvas.remove(); // the picture stays
    }
  } finally {
    booting = false;
  }
}

function shader(gl, type, src) {
  const s = gl.createShader(type);
  gl.shaderSource(s, src);
  gl.compileShader(s);
  if (!gl.getShaderParameter(s, gl.COMPILE_STATUS)) throw new Error(gl.getShaderInfoLog(s));
  return s;
}
function program(gl, vs, fs) {
  const p = gl.createProgram();
  gl.attachShader(p, shader(gl, gl.VERTEX_SHADER, vs));
  gl.attachShader(p, shader(gl, gl.FRAGMENT_SHADER, fs));
  gl.linkProgram(p);
  if (!gl.getProgramParameter(p, gl.LINK_STATUS)) throw new Error(gl.getProgramInfoLog(p));
  return p;
}

// Lit like the picture: a warm sun, a cool fill, a cream sky; flat facets on the robot, gloss on the jewel colours.
const VS = `attribute vec3 p; attribute vec3 n; attribute vec4 c;
uniform mat4 M, VP; uniform vec3 C; uniform float S;
varying vec3 vn; varying vec4 vc;
void main(){ vec4 w = M * vec4(C + p * S, 1.0); vn = mat3(M) * n; vc = c; gl_Position = VP * w; }`;
// The robot's light, also used to draw the lower page's props (build/mezze-props.mjs), so they light the same.
export const FS = `precision mediump float;
varying vec3 vn; varying vec4 vc;
uniform vec3 L, F, V, LC, FC, SKY;
vec3 lin(vec3 c){ return pow(c, vec3(2.2)); }
void main(){
  vec3 base = lin(vc.rgb);
  if (vc.a > 0.995) { gl_FragColor = vec4(pow(min(base * 0.55 + 0.55, 1.0), vec3(1.0 / 2.2)), 1.0); return; }
  float rough = clamp(vc.a * 255.0 / 200.0, 0.05, 1.0);
  vec3 N = normalize(vn);
  float d = max(dot(N, L), 0.0);
  vec3 amb = mix(vec3(0.62, 0.52, 0.42), SKY, N.z * 0.5 + 0.5) * 0.55;
  vec3 col = base * (amb + d * LC * 1.05 + max(dot(N, F), 0.0) * FC * 0.35);
  vec3 H = normalize(L + V);
  float sp = pow(max(dot(N, H), 0.0), mix(90.0, 6.0, rough)) * (1.0 - rough) * 0.75;
  col += sp * LC + pow(1.0 - max(dot(N, V), 0.0), 3.0) * 0.08;
  gl_FragColor = vec4(pow(clamp(col, 0.0, 1.0), vec3(1.0 / 2.2)), 1.0);
}`;
// Soft shadows on the ground: an ellipse each, under the foot, the cazuela and the napkin.
const SVS = `attribute vec2 q; uniform mat4 VP; uniform vec4 E; varying vec2 u;
void main(){ u = q; gl_Position = VP * vec4(E.xy + q * E.zw, 0.0, 1.0); }`;
const SFS = `precision mediump float; varying vec2 u; uniform float A;
void main(){ float a = A * (1.0 - smoothstep(0.15, 1.0, length(u))); gl_FragColor = vec4(vec3(0.36, 0.29, 0.16) * a, a); }`;

// Draws and animates until the returned function is called, which takes everything down again.
function run(gl, canvas, model) {
  const bye = new AbortController(), on = { signal: bye.signal };
  const prog = program(gl, VS, FS), sprog = program(gl, SVS, SFS);
  const loc = (p, n) => gl.getUniformLocation(p, n);
  const attr = { p: gl.getAttribLocation(prog, "p"), n: gl.getAttribLocation(prog, "n"), c: gl.getAttribLocation(prog, "c") };
  const parts = model.parts;
  const byName = Object.fromEntries(parts.map((p) => [p.name, p]));
  for (const p of parts) {
    const buf = (data, target = gl.ARRAY_BUFFER) => { const b = gl.createBuffer(); gl.bindBuffer(target, b); gl.bufferData(target, data, gl.STATIC_DRAW); return b; };
    p.gl = { pos: buf(p.pos), nor: buf(p.nor), col: buf(p.col), idx: buf(p.idx, gl.ELEMENT_ARRAY_BUFFER) };
    p.joint = p.name.startsWith("plate:") ? joint("plate") : JOINTS[p.name] ? joint(p.name) : null;
    if (p.plate) p.spin = Math.random() * Math.PI * 2;
  }
  const quad = gl.createBuffer();
  gl.bindBuffer(gl.ARRAY_BUFFER, quad);
  gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([-1, -1, 1, -1, -1, 1, 1, 1]), gl.STATIC_DRAW);

  // The picture's camera: orthographic, 6.5 units tall, looking down at 18 degrees.
  const view = new Float32Array(model.camera.view);
  const invView = rigidInverse(view);
  const [W0, H0] = model.size, aspect = W0 / H0, half = model.camera.ortho / 2;
  const NEAR = 5, FAR = 40;
  // An orthographic projection of the window x0..x1, y0..y1 (camera units). VP0 is the picture's own window, which
  // the labels are placed in; the canvas overhangs the picture (site.css) so a tipped plate is never cut off, and
  // VP is the canvas's wider window, set in resize().
  const ortho = (x0, x1, y0, y1) => new Float32Array([2 / (x1 - x0), 0, 0, 0, 0, 2 / (y1 - y0), 0, 0, 0, 0, -2 / (FAR - NEAR), 0,
    -(x1 + x0) / (x1 - x0), -(y1 + y0) / (y1 - y0), -(FAR + NEAR) / (FAR - NEAR), 1]);
  const VP0 = mul(ortho(-half * aspect, half * aspect, -half, half), view);
  let VP = VP0, win = [-half * aspect, half * aspect, -half, half];
  const camFwd = apply(invView, [0, 0, -1], 0), camRight = apply(invView, [1, 0, 0], 0), camUp = apply(invView, [0, 1, 0], 0);
  const lights = model.lights;
  const toCam = camFwd.map((x) => -x);

  // Each part's matrix: its parent's, then its own turn about its pivot (a plate: tilt, slide, spin).
  function matrices(t) {
    for (const p of parts) {
      let m = p.parent ? byName[p.parent].m : I();
      const J = p.joint;
      if (J) {
        const r = mul(R(0, 0, 1, J.a[2]), mul(R(0, 1, 0, J.a[0]), R(1, 0, 0, J.a[1])));
        m = mul(m, about(p.pivot, r));
      }
      if (p.plate) {
        const { centre, axis } = p.plate;
        // Slide in the plate's own plane: two directions across its axis.
        const u = cross(axis, [0, 0, 1]), ul = Math.hypot(...u) || 1, uu = u.map((x) => x / ul), vv = cross(axis, uu);
        m = mul(m, T(uu[0] * J.s[0] + vv[0] * J.s[1], uu[1] * J.s[0] + vv[1] * J.s[1], uu[2] * J.s[0] + vv[2] * J.s[1]));
        m = mul(m, about(centre, R(axis[0], axis[1], axis[2], p.spin)));
      }
      p.m = m;
    }
  }

  // ---------- the pointer: where it touches, and how fast it moves ----------
  // Events come to the figure (the picture's box), not the canvas, which overhangs it and lets clicks through.
  const ptr = { x: 0, y: 0, in: false, vx: 0, vy: 0, t: 0, near: false };
  const rect = () => canvas.getBoundingClientRect();
  function ray(clientX, clientY) {
    const b = rect();
    const fx = (clientX - b.left) / b.width, fy = (clientY - b.top) / b.height;
    return { o: apply(invView, [win[0] + fx * (win[1] - win[0]), win[3] - fy * (win[3] - win[2]), 0]), d: camFwd };
  }
  // The nearest triangle under the ray, in each part's rest pose (the ray is taken into it), or null.
  function pick(r) {
    let best = null;
    for (const p of parts) {
      if (p.name === "ground") continue;
      const inv = rigidInverse(p.m);
      const o = apply(inv, r.o), d = apply(inv, r.d, 0);
      const bc = p.bound, oc = sub(o, bc), along = dot(oc, d);
      if (dot(oc, oc) - along * along > bc[3] * bc[3]) continue;
      const w = p.world, ix = p.idx;
      for (let i = 0; i < ix.length; i += 3) {
        const a = ix[i] * 3, b = ix[i + 1] * 3, c = ix[i + 2] * 3;
        const e1 = [w[b] - w[a], w[b + 1] - w[a + 1], w[b + 2] - w[a + 2]], e2 = [w[c] - w[a], w[c + 1] - w[a + 1], w[c + 2] - w[a + 2]];
        const pv = cross(d, e2), det = dot(e1, pv);
        if (Math.abs(det) < 1e-9) continue;
        const tv = [o[0] - w[a], o[1] - w[a + 1], o[2] - w[a + 2]];
        const uu = dot(tv, pv) / det;
        if (uu < 0 || uu > 1) continue;
        const qv = cross(tv, e1), vv = dot(d, qv) / det;
        if (vv < 0 || uu + vv > 1) continue;
        const t = dot(e2, qv) / det;
        if (t > 0 && (!best || t < best.t)) best = { t, part: p, at: apply(p.m, [o[0] + d[0] * t, o[1] + d[1] * t, o[2] + d[2] * t]) };
      }
    }
    return best;
  }

  // A push at world point `at` with velocity `vel` (world units a second, across the screen): a turn for the part it
  // touched about its pivot, and a share for the parts above it. An arm pushed tips the robot that way and its body
  // leans back the other way to keep its balance; a plate pushed wobbles, and the other plates feel it a little.
  const PUSH = 0.055, MAX_KICK = 3.5;
  function push(hit, vel) {
    const kick = (p, share) => {
      const J = p.joint;
      if (!J) return;
      const piv = apply(p.parent ? byName[p.parent].m : I(), p.pivot);
      const tq = cross(sub(hit.at, piv), vel);
      const g = (PUSH * share) / J.j.inertia;
      J.v[0] = clamp(J.v[0] + tq[1] * g, -MAX_KICK, MAX_KICK);
      J.v[1] = clamp(J.v[1] + tq[0] * g, -MAX_KICK, MAX_KICK);
      if (p.name === "head") J.v[2] = clamp(J.v[2] + tq[2] * g, -MAX_KICK, MAX_KICK);
      if (p.plate) { J.sv[0] += dot(vel, camRight) * 0.02 * share; J.sv[1] += dot(vel, camUp) * 0.02 * share; }
    };
    let p = hit.part.name.startsWith("stick:") ? byName[hit.part.parent] : hit.part;
    kick(p, 1);
    const touched = p;
    for (let share = 0.25; p.parent; share *= 0.6) { p = byName[p.parent]; kick(p, share); }
    if (touched.name === "arm_l" || touched.name === "arm_r" || touched.name === "kick") {
      // The balance reflex: the body leans against the tip.
      const B = byName.body.joint, side = Math.sign(byName.root.joint.v[0] || 1);
      B.v[0] = clamp(B.v[0] - side * 0.5 * Math.abs(byName.root.joint.v[0]), -MAX_KICK, MAX_KICK);
    }
    for (const q of parts) if (q.plate && q !== touched) { q.joint.v[0] += (Math.random() - 0.5) * 0.4 * Math.hypot(...vel) * 0.05; q.joint.v[1] += (Math.random() - 0.5) * 0.4 * Math.hypot(...vel) * 0.05; }
  }

  const toWorldVel = (vx, vy) => {
    const b = rect(), k = (win[3] - win[2]) / b.height; // world units per CSS pixel
    return [0, 1, 2].map((i) => camRight[i] * vx * k - camUp[i] * vy * k);
  };
  stage.addEventListener("pointermove", (e) => {
    const now = e.timeStamp / 1000, dt = Math.max(1 / 240, now - (ptr.t || now - 1 / 60));
    const vx = (e.clientX - ptr.x) / dt, vy = (e.clientY - ptr.y) / dt;
    if (ptr.in) { ptr.vx = ptr.vx * 0.5 + vx * 0.5; ptr.vy = ptr.vy * 0.5 + vy * 0.5; }
    ptr.x = e.clientX; ptr.y = e.clientY; ptr.t = now; ptr.in = true;
    if (e.pointerType !== "mouse" || !stage.classList.contains("live")) return; // touch: the tap is the contact (pointerdown)
    const hit = pick(ray(e.clientX, e.clientY));
    if (hit) {
      // A brush: a push for the distance moved since the last event, in the direction moved.
      const vel = toWorldVel(ptr.vx, ptr.vy);
      const speed = Math.hypot(...vel);
      if (speed > 0.2) push(hit, vel.map((x) => (x * Math.min(dt, 0.05)) / 0.016 / Math.max(1, speed / 25)));
    }
  }, on);
  stage.addEventListener("pointerleave", () => { ptr.in = false; ptr.vx = ptr.vy = 0; }, on);
  stage.addEventListener("pointerdown", (e) => {
    ptr.x = e.clientX; ptr.y = e.clientY; ptr.t = e.timeStamp / 1000; ptr.in = e.pointerType === "mouse";
    if (!stage.classList.contains("live")) return;
    const hit = pick(ray(e.clientX, e.clientY));
    if (!hit) return;
    // A tap or a click: a push away from the part's middle, as a finger would give it.
    const p = hit.part, centre = apply(p.m, p.bound);
    const away = sub(hit.at, centre);
    const across = [0, 1, 2].map((i) => camRight[i] * dot(away, camRight) + camUp[i] * dot(away, camUp));
    const l = Math.hypot(...across) || 1;
    push(hit, across.map((x) => (x / l) * 45));
  }, on);
  // Near the picture, the head turns to look at the pointer: where it is, worked out here so the frame reads no layout.
  function lookAt(e) {
    const b = rect(), m = 160;
    ptr.near = e.pointerType === "mouse" && e.clientX > b.left - m && e.clientX < b.right + m && e.clientY > b.top - m && e.clientY < b.bottom + m;
    ptr.hx = ((e.clientX - b.left) / b.width - 0.55) * 2;
    ptr.hy = ((e.clientY - b.top) / b.height - 0.3) * 2;
  }
  addEventListener("pointermove", lookAt, { passive: true, signal: bye.signal });

  // ---------- the labels follow their plates ----------
  const tags = [...stage.querySelectorAll(".tags li[data-part]")];
  const project = (pt) => { const c = apply(VP0, pt); return [(c[0] + 1) / 2, (1 - c[1]) / 2]; };
  matrices(0);
  // Each label keeps its place in the page (left and top, as in the picture) and moves by a transform: as far as its
  // part has moved from the picture's pose. Transforms need no layout, so a frame never forces one.
  for (const tag of tags) {
    const p = byName[tag.dataset.part];
    tag.rest = project(p.plate ? p.plate.centre : p.bound);
  }

  // ---------- drawing ----------
  const U = { M: loc(prog, "M"), VP: loc(prog, "VP"), C: loc(prog, "C"), S: loc(prog, "S"), L: loc(prog, "L"), F: loc(prog, "F"), V: loc(prog, "V"), LC: loc(prog, "LC"), FC: loc(prog, "FC"), SKY: loc(prog, "SKY") };
  const SU = { VP: loc(sprog, "VP"), E: loc(sprog, "E"), A: loc(sprog, "A") };
  const sq = gl.getAttribLocation(sprog, "q");
  // The canvas's window and size, measured only when the figure changes size (a ResizeObserver), never in a frame.
  const size = { picW: 1, picH: 1 };
  function measure() {
    const b = rect(), pic = img.getBoundingClientRect(), dpr = Math.min(devicePixelRatio || 1, 2);
    if (!pic.width || !pic.height) return;
    const ux = (2 * half * aspect) / pic.width, uy = (2 * half) / pic.height; // camera units per CSS pixel
    win = [-half * aspect - (pic.left - b.left) * ux, half * aspect + (b.right - pic.right) * ux, -half - (b.bottom - pic.bottom) * uy, half + (pic.top - b.top) * uy];
    VP = mul(ortho(...win), view);
    size.picW = pic.width; size.picH = pic.height;
    const w = Math.round(b.width * dpr), h = Math.round(b.height * dpr);
    if (canvas.width !== w || canvas.height !== h) { canvas.width = w; canvas.height = h; }
    gl.viewport(0, 0, canvas.width, canvas.height);
  }
  measure();
  const sizes = new ResizeObserver(measure);
  sizes.observe(stage);
  function draw() {
    gl.clearColor(0, 0, 0, 0);
    gl.clear(gl.COLOR_BUFFER_BIT | gl.DEPTH_BUFFER_BIT);
    gl.disable(gl.DEPTH_TEST);
    gl.enable(gl.BLEND);
    gl.blendFunc(gl.ONE, gl.ONE_MINUS_SRC_ALPHA);
    gl.useProgram(sprog);
    gl.uniformMatrix4fv(SU.VP, false, VP);
    gl.bindBuffer(gl.ARRAY_BUFFER, quad);
    gl.enableVertexAttribArray(sq);
    gl.vertexAttribPointer(sq, 2, gl.FLOAT, false, 0, 0);
    for (const s of model.shadows) {
      gl.uniform4f(SU.E, s.at[0], s.at[1], s.r[0], s.r[1]);
      gl.uniform1f(SU.A, s.a);
      gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
    }
    gl.disableVertexAttribArray(sq);
    gl.disable(gl.BLEND);
    gl.enable(gl.DEPTH_TEST);
    gl.useProgram(prog);
    gl.uniformMatrix4fv(U.VP, false, VP);
    gl.uniform3fv(U.C, model.quant.centre);
    gl.uniform1f(U.S, model.quant.scale);
    gl.uniform3fv(U.L, lights.sun);
    gl.uniform3fv(U.F, lights.fill);
    gl.uniform3fv(U.V, toCam);
    gl.uniform3fv(U.LC, lights.sunColor);
    gl.uniform3fv(U.FC, lights.fillColor);
    gl.uniform3fv(U.SKY, lights.sky);
    for (const k of Object.values(attr)) gl.enableVertexAttribArray(k);
    for (const p of parts) {
      gl.uniformMatrix4fv(U.M, false, p.m);
      gl.bindBuffer(gl.ARRAY_BUFFER, p.gl.pos); gl.vertexAttribPointer(attr.p, 3, gl.SHORT, true, 0, 0);
      gl.bindBuffer(gl.ARRAY_BUFFER, p.gl.nor); gl.vertexAttribPointer(attr.n, 3, gl.BYTE, true, 0, 0);
      gl.bindBuffer(gl.ARRAY_BUFFER, p.gl.col); gl.vertexAttribPointer(attr.c, 4, gl.UNSIGNED_BYTE, true, 0, 0);
      gl.bindBuffer(gl.ELEMENT_ARRAY_BUFFER, p.gl.idx);
      gl.drawElements(gl.TRIANGLES, p.idx.length, gl.UNSIGNED_SHORT, 0);
    }
    for (const tag of tags) {
      const p = byName[tag.dataset.part];
      const at = project(apply(p.m, p.plate ? p.plate.centre : p.bound));
      const dx = (at[0] - tag.rest[0]) * size.picW, dy = (at[1] - tag.rest[1]) * size.picH;
      tag.style.transform = `translate(calc(-50% + ${dx.toFixed(1)}px), ${dy.toFixed(1)}px)`;
    }
  }

  // ---------- the loop ----------
  let t = 0, last = 0, raf = 0, shown = false, visible = true;
  function physics(dt) {
    const root = byName.root.joint, head = byName.head.joint;
    // At rest: a slow balance sway, never the same twice.
    root.goal[0] = (1.6 * Math.sin(t * 1.05) + 0.6 * Math.sin(t * 2.4 + 1)) * DEG;
    root.goal[1] = 0.5 * Math.sin(t * 0.8 + 2) * DEG;
    byName.body.joint.goal[0] = -0.5 * root.goal[0];
    // The head looks towards a pointer nearby.
    if (ptr.near) {
      head.goal[2] = clamp(ptr.hx * 0.35, -0.3, 0.3);
      head.goal[1] = clamp(ptr.hy * 0.2, -0.2, 0.2);
    } else head.goal[1] = head.goal[2] = 0;
    for (const p of parts) {
      const J = p.joint;
      if (!J) continue;
      step(J, dt);
      if (p.plate) {
        slide(J, dt);
        p.spin += SPIN * dt * (1 + Math.abs(J.v[0]) * 0.4);
      }
    }
    // The plates lag behind their carriers: a sway felt through the sticks.
    const sway = byName.root.joint.v[0] + byName.body.joint.v[0];
    for (const p of parts) if (p.plate) p.joint.v[0] -= sway * 0.9 * dt;
  }
  function frame(now) {
    raf = 0;
    const dt = Math.min(0.05, last ? (now - last) / 1000 : 1 / 60);
    last = now;
    t += dt;
    for (let n = 0, h = dt / 3; n < 3; n++) physics(h);
    matrices(t);
    draw();
    if (!shown) {
      shown = true;
      stage.classList.add("live");
      stage.title = "Touch the robot";
    }
    if (visible && !document.hidden) raf = requestAnimationFrame(frame);
  }
  let gone = false;
  const go = () => { if (!gone && !raf && visible && !document.hidden) { last = 0; raf = requestAnimationFrame(frame); } };
  const seen = new IntersectionObserver((es) => { visible = es[0].isIntersecting; go(); });
  seen.observe(stage);
  document.addEventListener("visibilitychange", go, on);
  // Take it all down: the picture comes back, and nothing of the robot is left running or held.
  function teardown() {
    if (gone) return;
    gone = true;
    if (raf) cancelAnimationFrame(raf);
    bye.abort();
    seen.disconnect();
    sizes.disconnect();
    stage.classList.remove("live");
    stage.removeAttribute("title");
    for (const tag of tags) tag.style.transform = "";
    if (!gl.isContextLost()) {
      for (const p of parts) for (const b of Object.values(p.gl)) gl.deleteBuffer(b);
      gl.deleteBuffer(quad);
      gl.deleteProgram(prog);
      gl.deleteProgram(sprog);
    }
    canvas.remove();
    stop = null;
  }
  canvas.addEventListener("webglcontextlost", (e) => { e.preventDefault(); teardown(); }, on);
  go();
  return teardown;
}
