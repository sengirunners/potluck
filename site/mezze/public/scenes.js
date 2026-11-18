// mezze.sh's scenes below the robot (index.html, .scene): they move the way the robot does. Each plate is the robot's
// own plate (img/props/, drawn by build/mezze-props.mjs): it turns at the robot's speed, sways a little at rest, and
// wobbles on a spring when the pointer pushes it (harder the faster it moves), then settles. Each beat also plays one
// short scene on a loop while it is on screen; with nothing on screen, nothing runs.
//
// index.html loads this, as it loads robot.js, only when motion is welcome, and it starts after the page has loaded,
// when the browser is idle. A plate shows its still picture until its scene comes near; then its sheet of turns loads.
// If the reader turns reduced motion on, everything stops and goes back to as drawn; turned off again, it starts again.
import { COLS, SPIN, TURNS } from "./plates.js";

const clamp = (x, a, b) => Math.max(a, Math.min(b, x));
const ease = (x) => 1 - Math.pow(1 - clamp(x, 0, 1), 3);
const q = (sel, root = document) => root.querySelector(sel);
class Spring { constructor(k = 90, d = 7) { this.x = 0; this.v = 0; this.to = 0; this.k = k; this.d = d; }
  step(dt) { this.v += (-this.k * (this.x - this.to) - this.d * this.v) * dt; this.x += this.v * dt; return this.x; } }
const ROWS = Math.ceil(TURNS / COLS);
const reduce = matchMedia("(prefers-reduced-motion: reduce)");

// Every scene as drawn, to go back to when the reader turns reduced motion on.
const ATTRS = ["transform", "opacity", "stroke-dashoffset", "stroke", "class"];
const asDrawn = [...document.querySelectorAll(".scene, .scene *")].map((el) => ({ el, style: el.getAttribute("style"),
  attrs: ATTRS.map((a) => [a, el.getAttribute(a)]), text: el.classList.contains("htxt") ? el.textContent : null }));

// ---------- plates ----------
const plates = [...document.querySelectorAll(".dish")].map((el) => {
  const p = { el, tilt: new Spring(60, 4.5), bob: new Spring(140, 9), phase: Math.random() * 9, spin: Math.random() * 6.3,
    dir: Math.random() < 0.5 ? -1 : 1, sec: el.closest("section"), sheet: false };
  let last = null;
  el.addEventListener("pointermove", (e) => {
    if (last) { p.tilt.v += clamp((e.clientX - last.x) * 6, -160, 160); p.bob.v += clamp((e.clientY - last.y) * 4, -80, 80); }
    last = { x: e.clientX, y: e.clientY };
  });
  el.addEventListener("pointerleave", () => { last = null; });
  el.addEventListener("pointerdown", () => { p.tilt.v += p.dir * 140; p.bob.v -= 90; });
  return p;
});
function drawPlate(p, t) {
  const sway = (Math.sin(t * 1.05 + p.phase) + 0.6 * Math.sin(t * 2.4 + p.phase)) * 1.4;
  const a = clamp(p.tilt.x + sway, -14, 14);
  if (p.sheet) showTurn(p.el, p.spin);
  p.el.style.transform = `translateY(${clamp(p.bob.x, -8, 8).toFixed(2)}px) rotate(${a.toFixed(2)}deg)`;
}
// The turn of a sheet nearest an angle, as a background position.
function showTurn(el, angle) {
  const f = ((Math.round(angle / (Math.PI * 2) * TURNS) % TURNS) + TURNS) % TURNS;
  el.style.backgroundPosition = `${((f % COLS) / (COLS - 1)) * 100}% ${(Math.floor(f / COLS) / (ROWS - 1)) * 100}%`;
}
// Swaps a plate's still picture for its sheet of turns once the sheet has loaded.
function useSheet(p) {
  if (p.sheet || p.loading) return;
  p.loading = true;
  const src = `/img/props/plate-${p.el.dataset.food}-turns.webp`, img = new Image();
  img.onload = () => { p.loading = false; p.sheet = true; p.el.style.backgroundImage = `url(${src})`; p.el.style.backgroundSize = `${COLS * 100}% ${ROWS * 100}%`; };
  img.onerror = () => { p.loading = false; };
  img.src = src;
}

// ---------- helpers for the scenes ----------
const swings = [...document.querySelectorAll(".tk")].map((g) => ({ g, px: +g.dataset.px, py: +g.dataset.py, rest: +g.dataset.rest,
  s: new Spring(30, 2.2), phase: Math.random() * 9, sec: g.closest("section") }));
const drawSwing = (w, t, dy = 0) => w.g.setAttribute("transform", `translate(0 ${dy.toFixed(2)}) rotate(${(w.rest + w.s.x + Math.sin(t * 1.3 + w.phase) * 0.8).toFixed(2)} ${w.px} ${w.py})`);
// A pop: something that scales on a spring (x = -1 is gone, 0 is full size). SVG groups scale about data-cx/cy.
const pop = (el) => ({ el, s: new Spring(180, 11), svg: el instanceof SVGElement, draw() {
  const k = Math.max(0, 1 + this.s.x);
  if (this.svg) { const cx = +this.el.dataset.cx, cy = +this.el.dataset.cy; this.el.setAttribute("transform", `translate(${cx} ${cy}) scale(${k.toFixed(3)}) translate(${-cx} ${-cy})`); }
  else this.el.style.transform = `scale(${k.toFixed(3)})`;
} });
const onScreen = new Set();
const sections = [...new Set([...document.querySelectorAll(".scene")].map((s) => s.closest("section")))];
const scenes = [];
const scene = (name, fn) => { const sec = q(`[data-beat="${name}"]`); if (sec) scenes.push({ sec, fn, t0: null }); };
const swung = new Set();

// The pass: the heat lamps breathe.
scene("pass", (t, sec) => { q(".lamps", sec).setAttribute("opacity", (0.75 + 0.25 * Math.sin(t * 1.7)).toFixed(3)); });

// Mail: a new note drops onto the rail and knocks its neighbours; the idle plate gets its one line.
const mailSec = q('[data-beat="mail"]');
const mailSwings = swings.filter((w) => w.sec === mailSec);
mailSwings.forEach((w) => swung.add(w));
const newTk = mailSwings.find((w) => w.g.classList.contains("new"));
const inbox = pop(q(".inbox", mailSec)), drop = new Spring(120, 10);
const idle = plates.find((p) => p.el.classList.contains("idle"));
let mailStage = -1, knocked = false;
scene("mail", (t, sec, dt, fresh) => {
  if (fresh) mailStage = -1;
  const T = 7, u = t % T, stage = u < 1.3 ? 0 : u < 6.2 ? 1 : 2;
  if (stage !== mailStage) {
    mailStage = stage;
    if (stage === 0) { drop.x = -170; drop.v = 0; knocked = false; inbox.s.x = -1; inbox.s.v = 0; inbox.s.to = -1; }
    if (stage === 1) { inbox.s.to = 0; inbox.s.v = 4; idle.bob.v -= 70; idle.tilt.v += 60; }
    if (stage === 2) { inbox.s.to = -1; }
  }
  drop.step(dt);
  if (!knocked && drop.x > -4) { knocked = true; mailSwings.forEach((w) => { if (w !== newTk) w.s.v += (w.px < newTk.px ? -1 : 1) * 45; }); newTk.s.v += 30; }
  const fade = u > 6.2 ? 1 - ease((u - 6.2) / 0.6) : 1;
  newTk.g.setAttribute("opacity", fade.toFixed(3));
  inbox.s.step(dt); inbox.draw();
  mailSwings.forEach((w) => { w.s.step(dt); drawSwing(w, t, w === newTk ? drop.x - (1 - fade) * 30 : 0); });
});

// The claim: Leo's agent claims, claims again, stalls; the timer runs out; Ana's agent picks it up.
const leaseSec = q('[data-beat="lease"]');
const ring = q(".ring", leaseSec), C = 578;
const lp = plates.find((p) => p.el.classList.contains("lp"));
const holder = pop(q(".holder", leaseSec)), notice = pop(q(".notice", leaseSec));
const steps = [...leaseSec.querySelectorAll(".st")];
let leaseStage = -1;
const setHolder = (txt, dot, dashed) => { q(".htxt", leaseSec).textContent = txt; q(".hdot", leaseSec).style.background = dot; q(".holder", leaseSec).classList.toggle("dash", dashed); };
notice.s.x = -1; notice.s.to = -1; notice.draw();
scene("lease", (t, sec, dt, fresh) => {
  if (fresh) leaseStage = -1;
  const T = 11, u = t % T;
  let fill, stage;
  if (u < 0.4) { fill = ease(u / 0.4); stage = 0; }
  else if (u < 3) { fill = 1 - 0.45 * (u - 0.4) / 2.6; stage = 0; }
  else if (u < 3.35) { fill = 0.55 + 0.45 * ease((u - 3) / 0.35); stage = 1; }
  else if (u < 7) { fill = 1 - (u - 3.35) / 3.65; stage = 1; }
  else if (u < 8.2) { fill = 0; stage = 2; }
  else if (u < 8.7) { fill = ease((u - 8.2) / 0.5); stage = 3; }
  else { fill = 1 - 0.2 * (u - 8.7) / 2.3; stage = 3; }
  if (stage !== leaseStage) {
    leaseStage = stage;
    steps.forEach((r, i) => r.style.background = i === stage ? "#EADFF7" : "#fff");
    if (stage === 0) { setHolder("Leo's agent", "#10A66A", false); lp.el.classList.remove("grey"); holder.s.x = -0.6; notice.s.to = -1; }
    if (stage === 1) { lp.bob.v -= 120; lp.tilt.v += 60; holder.s.v = 3; }
    if (stage === 2) { setHolder("free to claim", "#CFC6B5", true); lp.el.classList.add("grey"); lp.tilt.v -= 160; holder.s.x = -0.4; notice.s.to = 0; notice.s.x = -1; }
    if (stage === 3) { setHolder("Ana's agent", "#2D6CE6", false); lp.el.classList.remove("grey"); lp.bob.v -= 140; holder.s.x = -0.7; }
  }
  if (leaseStage === 1 && u > 5.2) q(".htxt", leaseSec).textContent = "Leo's agent, stalled";
  ring.setAttribute("stroke-dashoffset", (C * (1 - fill)).toFixed(1));
  ring.setAttribute("stroke", fill < 0.2 && stage < 3 ? "#D91C4A" : "#8B3FD6");
  holder.s.step(dt); holder.draw(); notice.s.step(dt); notice.draw();
});

// The phone: the question arrives, a thumb taps Approve, the receipt prints, the stamp lands.
const askSec = q('[data-beat="ask"]');
const qcard = q(".q", askSec), thumb = q(".thumb", askSec), ripple = q(".ripple", askSec), receipt = q(".receipt", askSec);
const approve = pop(q(".approve", askSec)), stamp = pop(q(".stamp", askSec));
const qy = new Spring(120, 9), ry = new Spring(70, 9);
let askStage = -1;
scene("ask", (t, sec, dt, fresh) => {
  if (fresh) askStage = -1;
  const T = 9, u = t % T, stage = u < 1.9 ? 0 : u < 2.4 ? 1 : u < 4.3 ? 2 : u < 8.3 ? 3 : 4;
  if (stage !== askStage) {
    askStage = stage;
    if (stage === 0) { qy.x = 30; qy.v = 0; ry.x = -250; ry.to = -250; ry.v = 0; stamp.s.x = -1; stamp.s.to = -1; stamp.s.v = 0; }
    if (stage === 1) { approve.s.x = -0.12; approve.s.v = 0; }
    if (stage === 2) { ry.to = 0; }
    if (stage === 3) { stamp.s.x = 0.9; stamp.s.to = 0; stamp.s.v = 0; ry.v += 120; }
  }
  qy.step(dt); ry.step(dt); approve.s.step(dt); stamp.s.step(dt);
  const fin = stage === 4 ? 1 - ease((u - 8.3) / 0.6) : 1;
  qcard.style.transform = `translateY(${qy.x.toFixed(2)}px)`;
  qcard.style.opacity = (Math.min(1, u / 0.4) * fin).toFixed(3);
  thumb.style.opacity = u > 1.4 && u < 2.9 ? Math.min(1, (u - 1.4) / 0.3, (2.9 - u) / 0.3).toFixed(3) : 0;
  thumb.style.transform = `translate(${u < 2 ? ((2 - u) * 20).toFixed(1) : 0}px, ${u > 1.9 && u < 2.1 ? -4 : u < 1.9 ? ((1.9 - u) * 30).toFixed(1) : 0}px)`;
  const r = u > 2 && u < 2.8 ? (u - 2) / 0.8 : 0;
  ripple.style.transform = `scale(${(1 + r * 9).toFixed(2)})`; ripple.style.opacity = r ? (1 - r).toFixed(3) : 0;
  receipt.setAttribute("transform", `translate(0 ${ry.x.toFixed(2)})`); receipt.setAttribute("opacity", fin.toFixed(3));
  approve.draw(); stamp.draw(); stamp.el.setAttribute("opacity", (stage >= 3 ? fin : 0).toFixed(3));
});

// Sessions: the lid of the session that needs you rattles until you attach, then lifts; the running one steams.
const runSec = q('[data-beat="run"]');
const lid = q(".lid", runSec), puffs = q(".puffs", runSec);
const lidA = new Spring(70, 6), lidY = new Spring(70, 7), rattle = new Spring(500, 7);
const puffList = [];
let runStage = -1, nextPuff = 0, nextRattle = 0;
scene("run", (t, sec, dt, fresh) => {
  if (fresh) { runStage = -1; nextPuff = 0; nextRattle = 0; puffList.splice(0).forEach((p) => p.c.remove()); }
  const T = 7, u = t % T, stage = u < 2.4 ? 0 : u < 5.4 ? 1 : 2;
  if (stage !== runStage) { runStage = stage; lidA.to = stage === 1 ? -26 : 0; lidY.to = stage === 1 ? -26 : 0; if (stage === 2) rattle.v += 60; }
  if (stage === 0 && t > nextRattle) { nextRattle = t + 0.5; rattle.v += (Math.random() < 0.5 ? -1 : 1) * 90; }
  lidA.step(dt); lidY.step(dt); rattle.step(dt);
  lid.style.transform = `translate(${(lidY.x * 0.3).toFixed(2)}px, ${(lidY.x - Math.abs(rattle.x) * 0.25).toFixed(2)}px) rotate(${(lidA.x + rattle.x * 0.5).toFixed(2)}deg)`;
  if (t > nextPuff) { nextPuff = t + 0.7; const c = document.createElementNS("http://www.w3.org/2000/svg", "circle"); c.dataset.made = ""; puffs.appendChild(c); puffList.push({ c, b: t, x: (Math.random() - 0.5) * 10 }); }
  for (let i = puffList.length - 1; i >= 0; i--) {
    const p = puffList[i], a = t - p.b;
    if (a > 2.2 || a < 0) { p.c.remove(); puffList.splice(i, 1); continue; }
    p.c.setAttribute("cy", (118 - a * 28).toFixed(1)); p.c.setAttribute("cx", (212 + p.x + Math.sin(a * 3 + p.x) * 5).toFixed(1));
    p.c.setAttribute("r", (4 + a * 4).toFixed(1)); p.c.setAttribute("opacity", (0.9 * (1 - a / 2.2)).toFixed(3));
  }
});

// Machines: little plates (notes, claims, sessions) travel the paths between the computers; the code card swings.
const machSec = q('[data-beat="mach"]');
const arcs = ["#arcA", "#arcB", "#arcC"].map((s) => q(s, machSec));
const lens = arcs.map((a) => a.getTotalLength());
const box = q(".packets", machSec);
// The little plates are made when the scene starts and removed when everything goes back to as drawn.
const packets = [];
let lastKick = 0;
scene("mach", (t, sec, dt, fresh) => {
  if (fresh) lastKick = 0;
  if (!packets.length) ["olives", "cheese", "grapes", "olives", "grapes", "cheese"].forEach((food, i) => {
    const el = document.createElement("div"); el.className = "at dish"; el.dataset.food = food; el.dataset.made = "";
    el.style.cssText = `--w:52;--h:37.8;opacity:0;pointer-events:none;background-image:url(/img/props/plate-${food}-turns.webp);background-size:${COLS * 100}% ${ROWS * 100}%`;
    box.appendChild(el);
    packets.push({ el, arc: i % 3, off: i / 6, speed: 0.12 + (i % 2) * 0.03, spin: i });
  });
  packets.forEach((p) => {
    const f = (t * p.speed + p.off) % 1, pt = arcs[p.arc].getPointAtLength(f * lens[p.arc]);
    p.el.style.setProperty("--x", (pt.x - 26).toFixed(1)); p.el.style.setProperty("--y", (pt.y - 19.5 + Math.sin(t * 6 + p.off * 9) * 1.5).toFixed(1));
    p.el.style.opacity = Math.min(1, f * 10, (1 - f) * 10).toFixed(3);
    p.spin += SPIN * dt * 1.5; showTurn(p.el, p.spin);
  });
  if (Math.floor(t / 5) !== lastKick) { lastKick = Math.floor(t / 5); swings.forEach((w) => { if (w.sec === sec) w.s.v += 50; }); }
});

// ---------- the loop: runs only while a scene is on screen and motion is welcome ----------
let raf = 0, prev = 0;
function frame(now) {
  raf = 0;
  if (reduce.matches) return asBefore(); // as well as on the change event, which not every browser sends
  if (!onScreen.size) return;
  const dt = Math.min(0.05, (now - prev) / 1000); prev = now;
  const t = now / 1000;
  for (const p of plates) {
    if (!onScreen.has(p.sec)) continue;
    p.tilt.step(dt); p.bob.step(dt);
    p.spin += SPIN * p.dir * dt * (1 + Math.min(2, Math.abs(p.tilt.v) * 0.01));
    drawPlate(p, t);
  }
  for (const s of scenes) {
    if (!onScreen.has(s.sec)) { s.t0 = null; continue; }
    const fresh = s.t0 === null;
    if (fresh) s.t0 = t;
    s.fn(t - s.t0, s.sec, dt, fresh);
  }
  swings.forEach((w) => { if (!swung.has(w) && onScreen.has(w.sec)) { w.s.step(dt); drawSwing(w, t); } });
  raf = requestAnimationFrame(frame);
}
function wake() {
  if (raf || reduce.matches || !onScreen.size) return;
  prev = performance.now();
  raf = requestAnimationFrame(frame);
}

// Back to as drawn: every attribute and style the scenes changed, the stills instead of the sheets, nothing made.
function asBefore() {
  cancelAnimationFrame(raf); raf = 0;
  for (const { el, style, attrs, text } of asDrawn) {
    if (style === null) el.removeAttribute("style"); else el.setAttribute("style", style);
    for (const [a, v] of attrs) if (v === null) el.removeAttribute(a); else el.setAttribute(a, v);
    if (text !== null) el.textContent = text;
  }
  document.querySelectorAll(".scene [data-made]").forEach((el) => el.remove());
  packets.length = 0; puffList.length = 0;
  for (const p of plates) { p.sheet = false; p.tilt.x = p.tilt.v = p.bob.x = p.bob.v = 0; }
  for (const w of swings) w.s.x = w.s.v = 0;
  for (const s of scenes) s.t0 = null;
}

// What is near loads its sheets; what is on screen moves.
const near = new IntersectionObserver((es) => es.forEach((e) => {
  if (e.isIntersecting && !reduce.matches) plates.forEach((p) => { if (p.sec === e.target) useSheet(p); });
}), { rootMargin: "150px 0px" });
const seen = new IntersectionObserver((es) => {
  es.forEach((e) => e.isIntersecting ? onScreen.add(e.target) : onScreen.delete(e.target));
  wake();
}, { rootMargin: "80px 0px" });
reduce.addEventListener("change", () => {
  if (reduce.matches) return asBefore();
  for (const sec of sections) { near.unobserve(sec); near.observe(sec); }
  wake();
});
const start = () => sections.forEach((sec) => { near.observe(sec); seen.observe(sec); });
const whenIdle = (fn) => (window.requestIdleCallback ? requestIdleCallback(fn, { timeout: 1500 }) : setTimeout(fn, 200));
if (document.readyState === "complete") whenIdle(start);
else addEventListener("load", () => whenIdle(start), { once: true });
