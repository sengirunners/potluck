(() => {
  // The machines on the blanket. An example pool: the numbers show what each kind of machine might have.
  const MACHINES = {
    "mini-mac": { who: "Ana", name: "Ana's Mac mini", sub: "Left on at night while Ana sleeps", cores: 10, gb: 32, lent: "Whenever Ana isn't using it",
      runs: "Fresh macOS and Linux VMs", now: "a coding agent building the iOS app, in a fresh macOS VM." },
    server: { who: "the team", name: "The rented server", sub: "Paid for anyway, so keep it busy", cores: 16, gb: 64, lent: "All the time",
      runs: "Linux sandboxes and VMs", now: "three coding agents, one sandbox each." },
    tower: { who: "Sam", name: "Sam's old gaming PC", sub: "Was gathering dust under a desk", cores: 8, gb: 16, lent: "All the time",
      runs: "Linux sandboxes", now: "make test for Sam's branch, in a sandbox." },
    cloud: { who: "Kai", name: "Kai's AWS VM", sub: "A cloud VM with spare hours", cores: 4, gb: 16, lent: "Weekdays, set in its driver",
      runs: "Linux sandboxes", now: "idle, waiting for work." },
    "mini-pc": { who: "Selica", name: "Selica's mini PC", sub: "Small, quiet, always on", cores: 8, gb: 32, lent: "All the time",
      runs: "Linux sandboxes and VMs", now: "a coding agent fixing a flaky test, in its own sandbox." },
    laptop: { who: "Leo", name: "Leo's MacBook Pro", sub: "Leo's own laptop, lent when he isn't on it", cores: 12, gb: 36, lent: "When Leo steps away",
      runs: "Fresh macOS and Linux VMs", now: "a coding agent's tests, in a fresh macOS VM." },
  };
  // potluck host shares half of a machine's cores and memory unless told otherwise (--share).
  const SHARE = 0.5;
  const $ = (id) => document.getElementById(id);
  const stage = $("stage"), scene = $("scene"), card = $("card"), leader = $("leader"), labels = $("labels"), turnRow = $("turn");
  const tags = [...labels.querySelectorAll(".tag[data-for]")];
  const buttons = tags.filter((t) => t.tagName === "BUTTON");
  const machines = [...scene.querySelectorAll(".m[data-id]")];
  const reduce = matchMedia("(prefers-reduced-motion: reduce)");
  let chosen = "mini-mac";

  function total() {
    let n = 0, cores = 0, gb = 0;
    for (const [id, m] of Object.entries(MACHINES)) {
      n++; cores += m.cores * SHARE; gb += m.gb * SHARE;
    }
    $("pool-total").textContent = `${n} machines, ${cores} cores, ${gb} GB`;
  }

  // Fill the card for one machine. `say` is a short line for screen readers, sent only when the reader acted.
  function show(id, say) {
    chosen = id;
    const m = MACHINES[id];
    for (const t of buttons) t.setAttribute("aria-pressed", String(t.dataset.for === id));
    for (const el of machines) el.classList.toggle("sel", el.dataset.id === id);
    $("dish-who").textContent = `Brought by ${m.who}`;
    $("dish-name").textContent = m.name;
    $("dish-sub").textContent = m.sub;
    $("dish-brings").textContent = `${m.cores * SHARE} of ${m.cores} cores, ${m.gb * SHARE} of ${m.gb} GB`;
    $("dish-lent").textContent = m.lent;
    $("dish-runs").textContent = m.runs;
    $("dish-now").innerHTML = `<b>Right now:</b> ${m.now}`;
    if (say) $("pick-status").textContent = say;
    layout();
  }

  // Labels sit in a flat layer over the scene. Each time the scene moves, find where every machine lands on screen,
  // then place labels front to back, each at the cheapest spot: covering another label, the card or the arrows costs
  // most; covering a machine's body (its own included) or a leader line, drawing its own leader through another
  // machine, another label or across another leader, or sitting much nearer another machine's pin than its own each
  // cost as much as a body; a person or the food a little, and every pixel from its pin a little more. Spots above
  // the pin are tried first.
  const pins = new Map([...scene.querySelectorAll("[data-pin]")].map((p) => [p.dataset.pin, p]));
  const props = [...scene.querySelectorAll(".m:not([data-id]) .cut")];
  const area = (a, b) => Math.max(0, Math.min(a.x + a.w, b.x + b.w) - Math.max(a.x, b.x)) * Math.max(0, Math.min(a.y + a.h, b.y + b.h) - Math.max(a.y, b.y));
  const grow = (r, p) => ({ x: r.x - p, y: r.y - p, w: r.w + 2 * p, h: r.h + 2 * p });
  // A machine's shape on screen, as convex parts: its box (the hull of the corners of its front and back faces) and
  // its lid, read from zero-size marks in those corners. A cut-out's shape is its box.
  const mark = (face) => ["0% 0%", "100% 0%", "100% 100%", "0% 100%"].map((at) => {
    const i = document.createElement("i");
    i.className = "corner";
    [i.style.left, i.style.top] = at.split(" ");
    face.append(i);
    return i;
  });
  const marks = machines.map((m) => [[...m.querySelectorAll(".body > .fr, .body > .bk")].flatMap(mark), ...[...m.querySelectorAll(".lid .scr")].map(mark)]
    .filter((group) => group.length));
  const bounds = (pts) => {
    const x = Math.min(...pts.map((p) => p[0])), y = Math.min(...pts.map((p) => p[1]));
    return { x, y, w: Math.max(...pts.map((p) => p[0])) - x, h: Math.max(...pts.map((p) => p[1])) - y };
  };
  const corners = (r) => [[r.x, r.y], [r.x + r.w, r.y], [r.x + r.w, r.y + r.h], [r.x, r.y + r.h]];
  // A convex polygon with its bounding box, which lets most tests stop early.
  const polygon = (pts) => ({ pts, box: bounds(pts) }), rectangle = (r) => ({ pts: corners(r), box: r });
  const hull = (pts) => {
    pts = [...pts].sort((a, b) => a[0] - b[0] || a[1] - b[1]);
    const turn = (o, a, b) => (a[0] - o[0]) * (b[1] - o[1]) - (a[1] - o[1]) * (b[0] - o[0]);
    const half = (ps) => {
      const out = [];
      for (const p of ps) { while (out.length > 1 && turn(out[out.length - 2], out[out.length - 1], p) <= 0) out.pop(); out.push(p); }
      return out.slice(0, -1);
    };
    return half(pts).concat(half(pts.reverse()));
  };
  // The part of the segment [x1, y1, x2, y2] inside a convex polygon, as [t0, t1] along it (Cyrus-Beck clipping),
  // or null.
  const clip = ([x1, y1, x2, y2], { pts: poly, box }) => {
    if (Math.max(x1, x2) < box.x || Math.min(x1, x2) > box.x + box.w || Math.max(y1, y2) < box.y || Math.min(y1, y2) > box.y + box.h) return null;
    let t0 = 0, t1 = 1, wind = 0;
    poly.forEach(([ax, ay], i) => { const [bx, by] = poly[(i + 1) % poly.length]; wind += ax * by - bx * ay; });
    for (let i = 0; i < poly.length; i++) {
      const [ax, ay] = poly[i], [bx, by] = poly[(i + 1) % poly.length];
      const f = (x, y) => Math.sign(wind) * ((bx - ax) * (y - ay) - (by - ay) * (x - ax));
      const f1 = f(x1, y1), f2 = f(x2, y2);
      if (f1 < 0 && f2 < 0) return null;
      if (f1 < 0) t0 = Math.max(t0, f1 / (f1 - f2));
      else if (f2 < 0) t1 = Math.min(t1, f1 / (f1 - f2));
      if (t0 >= t1) return null;
    }
    return [t0, t1];
  };
  // The parts of the segment inside any of the polygons, merged, and their length in pixels.
  const spans = (seg, polys) => {
    const out = [];
    for (const t of polys.map((p) => clip(seg, p)).filter(Boolean).sort((a, b) => a[0] - b[0])) {
      const last = out[out.length - 1];
      if (last && t[0] <= last[1]) last[1] = Math.max(last[1], t[1]); else out.push(t);
    }
    return out;
  };
  const length = (seg, ts) => ts.reduce((n, t) => n + t[1] - t[0], 0) * Math.hypot(seg[2] - seg[0], seg[3] - seg[1]);
  // How much of box r a convex polygon covers: the polygon clipped to the box (Sutherland-Hodgman), then its area.
  const cover = (r, { pts: poly, box }) => {
    if (!area(r, box)) return 0;
    for (const inside of [(p) => p[0] - r.x, (p) => r.x + r.w - p[0], (p) => p[1] - r.y, (p) => r.y + r.h - p[1]]) {
      const out = [];
      poly.forEach((p, i) => {
        const q = poly[(i + 1) % poly.length], a = inside(p), b = inside(q);
        if (a >= 0) out.push(p);
        if ((a >= 0) !== (b >= 0)) { const t = a / (a - b); out.push([p[0] + (q[0] - p[0]) * t, p[1] + (q[1] - p[1]) * t]); }
      });
      poly = out;
      if (!poly.length) return 0;
    }
    return Math.abs(poly.reduce((n, p, i) => { const q = poly[(i + 1) % poly.length]; return n + p[0] * q[1] - q[0] * p[1]; }, 0)) / 2;
  };
  // Whether two segments [x1, y1, x2, y2] cross (touching ends do not count).
  const crosses = (a, b) => {
    const side = (s, x, y) => Math.sign((s[2] - s[0]) * (y - s[1]) - (s[3] - s[1]) * (x - s[0]));
    return side(a, b[0], b[1]) * side(a, b[2], b[3]) < 0 && side(b, a[0], a[1]) * side(b, a[2], a[3]) < 0;
  };
  const nearest = (r, x, y) => ({ x: Math.max(r.x, Math.min(r.x + r.w, x)), y: Math.max(r.y, Math.min(r.y + r.h, y)) });
  const rel = (el, s) => { const r = el.getBoundingClientRect(); return { x: r.left - s.left, y: r.top - s.top, w: r.width, h: r.height }; };

  let queued = false;
  function layout() {
    if (queued) return;
    queued = true;
    requestAnimationFrame(() => { queued = false; place(); });
  }
  function place() {
    const s = stage.getBoundingClientRect();
    const sceneBox = rel(scene, s), cardBox = rel(card, s), turnBox = rel(turnRow, s);
    const propBoxes = props.map((el) => rel(el, s));
    const shapes = machines.map((m, k) => marks[k].length
      ? marks[k].map((group) => polygon(hull(group.map((i) => { const r = rel(i, s); return [r.x, r.y]; }))))
      : [rectangle(rel(m.querySelector(".cut"), s))]);
    const items = tags.map((el) => {
      const id = el.dataset.for, p = pins.get(id).getBoundingClientRect();
      const k = machines.findIndex((m) => m.dataset.id === id);
      return { el, id, ax: p.left + p.width / 2 - s.left, ay: p.top - s.top, body: bounds(shapes[k].flatMap((q) => q.pts)), shape: shapes[k] };
    });
    items.sort((a, b) => (b.body.y + b.body.h) - (a.body.y + a.body.h));
    for (const it of items) ({ width: it.w, height: it.h } = it.el.getBoundingClientRect());

    const parts = shapes.flat();
    // Labels keep 8px in from the scene's edge: on a phone the scene runs to the screen's edge.
    const minX = sceneBox.x + 8, maxX = sceneBox.x + sceneBox.w - 8, minY = sceneBox.y + 8, maxY = sceneBox.y + sceneBox.h - 8;
    const leaderOf = (it, box) => {
      const e = nearest(box, it.ax, it.ay);
      return Math.hypot(e.x - it.ax, e.y - it.ay) > 6 ? [e.x, e.y, it.ax, it.ay] : null;
    };
    // Place labels in this order, each at its cheapest spot given the ones before it.
    const solve = (order) => {
      const hard = [cardBox, turnBox], placed = [], lines = [], spots = new Map();
      let total = 0;
      for (const it of order) {
        const { ax, ay, w, h, body, shape } = it;
        const g = 8, row = h + 6;
        const top = Math.min(ay, body.y) - g - h;
        const xs = [0, -1, 1, -2, 2, -3, 3].map((k) => ax - w / 2 + k * w / 3).concat(body.x - w - g, body.x + body.w + g);
        const ys = [top, top - row, top - 2 * row, top - 3 * row, ay - h / 2, body.y + body.h + g];
        let best = null, bestCost = Infinity;
        const tryAt = (cx, cy) => {
          const r = { x: Math.round(Math.max(minX, Math.min(maxX - w, cx))), y: Math.round(Math.max(minY, Math.min(maxY - h, cy))), w, h };
          const pad = grow(r, 3), edge = grow(r, 2);
          // Cheap terms first; a spot stops counting once it costs more than the best so far.
          let cost = 0;
          for (const q of hard) if (area(pad, q) > 0) cost += 10000 + area(pad, q);
          const e = nearest(r, ax, ay), d = Math.hypot(e.x - ax, e.y - ay);
          cost += 2 * d + (r.y > ay ? 60 : 0);
          for (const q of propBoxes) cost += area(r, q) * .1;
          // A label much nearer another machine's pin than its own reads as that machine's.
          for (const o of items) if (o !== it && o.id) { const n = nearest(r, o.ax, o.ay); if (Math.hypot(n.x - o.ax, n.y - o.ay) < d * .6) { cost += 1000; break; } }
          if (cost >= bestCost) return;
          // Its leader must not run through another machine or label, or cross a leader; nor may the label sit on
          // a leader already drawn.
          for (const l of lines) if (clip(l, rectangle(edge))) cost += 1000;
          const seg = leaderOf(it, r);
          if (seg) {
            for (const q of placed) if (length(seg, spans(seg, [rectangle(q)])) > 2) cost += 1000;
            for (const l of lines) if (crosses(seg, l)) cost += 1000;
            if (cost >= bestCost) return;
            for (const o of shapes) { const t = o === shape ? 0 : length(seg, spans(seg, o)); if (t > 2) cost += 1000 + 10 * t; }
          }
          if (cost >= bestCost) return;
          for (const q of parts) { const a = cover(edge, q); if (a > 0) { cost += 1000 + a; if (cost >= bestCost) return; } }
          best = r; bestCost = cost;
        };
        for (const cy of ys) for (const cx of xs) tryAt(cx, cy);
        // Nothing clear and clearly its own nearby: search a wider grid round the pin.
        if (bestCost >= 400) {
          for (let j = -6; j <= 8; j++) for (let k = -8; k <= 8; k++) tryAt(ax - w / 2 + k * w / 3, ay - h / 2 + j * row / 2);
        }
        // Still covering a machine or crossing one: anywhere in the scene, with a longer leader, is better. Search it
        // coarsely, then finely round the best spot.
        if (bestCost >= 1000) {
          for (let y = minY; y <= maxY - h; y += 24) for (let x = minX; x <= maxX - w; x += 24) tryAt(x, y);
          const b = best;
          for (let dy = -18; dy <= 18; dy += 6) for (let dx = -18; dx <= 18; dx += 6) tryAt(b.x + dx, b.y + dy);
        }
        hard.push(best);
        placed.push(best);
        const seg = leaderOf(it, best);
        if (seg) lines.push(seg);
        spots.set(it, { box: best, cost: bestCost });
        total += bestCost;
      }
      return { order, spots, total };
    };
    // Front to back first. If a label is left covering a machine or with its leader through one, try again with
    // that label first, a few times, and keep the cheapest layout.
    let sol = solve(items);
    for (let n = 0; n < 3; n++) {
      const worst = sol.order.reduce((a, b) => (sol.spots.get(b).cost > sol.spots.get(a).cost ? b : a));
      if (sol.spots.get(worst).cost < 1000) break;
      const next = solve([worst, ...sol.order.filter((it) => it !== worst)]);
      if (next.total >= sol.total) break;
      sol = next;
    }
    const stems = [];
    items.forEach((it, i) => {
      const { box } = sol.spots.get(it);
      it.box = box;
      it.el.style.transform = `translate(${box.x}px,${box.y}px)`;
      it.el.style.zIndex = String(items.length - i);
      const seg = leaderOf(it, box);
      if (seg) stems.push(`<path class="stem" d="M${seg[0]} ${seg[1]}L${seg[2]} ${seg[3]}"/><circle cx="${seg[2]}" cy="${seg[3]}" r="2" fill="#3A4437"/>`);
    });
    // The card's notch points up at the chosen machine's label.
    const sel = items.find((it) => it.id === chosen);
    if (sel) card.style.setProperty("--notch", `${Math.max(24, Math.min(cardBox.w - 24, sel.box.x + sel.box.w / 2 - cardBox.x))}px`);
    leader.innerHTML = stems.join("");
  }

  for (const t of buttons) {
    t.addEventListener("click", () => show(t.dataset.for, `${MACHINES[t.dataset.for].name}.`));
    t.addEventListener("keydown", (ev) => {
      if (ev.key !== "ArrowRight" && ev.key !== "ArrowLeft") return;
      ev.preventDefault();
      const i = buttons.indexOf(t) + (ev.key === "ArrowRight" ? 1 : -1);
      buttons[(i + buttons.length) % buttons.length].focus();
    });
  }
  for (const m of machines) {
    if (m.dataset.id === "dish") m.addEventListener("click", () => { if (!dragged) location.hash = "dish"; });
    else m.addEventListener("click", () => { if (!dragged) show(m.dataset.id, `${MACHINES[m.dataset.id].name}.`); });
  }

  // Turn the lawn, all the way round if you like: drag it, or use the arrow buttons. Dragging right moves the front of
  // the lawn right, half a turn for a drag across the scene. A drag of a few pixels or more is not a click. `goal` is
  // where the lawn is heading, so quick clicks add up.
  let spin = -12, goal = -12, startX = 0, startSpin = 0, dragging = false, dragged = false, anim = 0;
  const arrows = [...turnRow.querySelectorAll("[data-turn]")];
  // `now`: place the labels in this frame (during an animation), not the next.
  const setSpin = (deg, now) => {
    spin = deg;
    scene.style.setProperty("--spin", `${spin}deg`);
    if (now) place(); else layout();
  };
  const turnTo = (target, ms) => {
    cancelAnimationFrame(anim);
    goal = target;
    if (reduce.matches || ms === 0) { setSpin(goal); return; }
    const from = spin, to = goal, t0 = performance.now();
    const step = (t) => {
      const k = Math.min(1, (t - t0) / ms);
      setSpin(from + (to - from) * (1 - Math.pow(1 - k, 3)), true);
      if (k < 1) anim = requestAnimationFrame(step);
    };
    anim = requestAnimationFrame(step);
  };
  scene.addEventListener("pointerdown", (ev) => {
    if (ev.button !== 0) return;
    cancelAnimationFrame(anim);
    dragging = true; dragged = false; startX = ev.clientX; startSpin = spin;
  });
  addEventListener("pointermove", (ev) => {
    if (!dragging) return;
    const dx = ev.clientX - startX;
    if (!dragged && Math.abs(dx) > 4) { dragged = true; scene.classList.add("dragging"); }
    if (dragged) { setSpin(startSpin - dx * 180 / scene.clientWidth); goal = spin; }
  });
  const end = () => { dragging = false; scene.classList.remove("dragging"); setTimeout(() => { dragged = false; }, 0); };
  addEventListener("pointerup", end);
  addEventListener("pointercancel", end);
  for (const b of arrows) {
    b.addEventListener("click", () => turnTo(goal + Number(b.dataset.turn) * 30, 500));
  }

  labels.hidden = false;
  turnRow.hidden = false;
  new ResizeObserver(layout).observe(stage);
  document.fonts?.ready.then(layout);

  // A slow turn into place on load, unless the reader asked for less motion.
  if (!reduce.matches) { setSpin(-32); turnTo(-12, 1600); }

  // The cost band.
  const minutes = $("minutes");
  const money = new Intl.NumberFormat("en-US", { style: "currency", currency: "USD", maximumFractionDigits: 0 });
  const count = new Intl.NumberFormat("en-US");
  const bill = () => {
    const n = Number(minutes.value);
    $("minutes-out").textContent = count.format(n);
    $("cost-gh").textContent = money.format(n * 0.062);
  };
  minutes.addEventListener("input", bill);

  // The install line fades at its right edge while more of it is out of sight.
  const line = document.querySelector(".install .line");
  const fade = () => line.classList.toggle("more", line.scrollLeft + line.clientWidth < line.scrollWidth - 1);
  line.addEventListener("scroll", fade, { passive: true });
  new ResizeObserver(fade).observe(line);
  document.fonts?.ready.then(fade);

  // Copy buttons, shown only with script. If the clipboard refuses, the command is selected for the keyboard.
  const copyKey = /Mac|iPhone|iPad/.test(navigator.userAgentData?.platform ?? navigator.platform) ? "⌘C" : "Ctrl+C";
  for (const b of document.querySelectorAll("[data-copy]")) {
    b.hidden = false;
    b.addEventListener("click", async () => {
      const text = $(b.dataset.copy).textContent;
      let said;
      try {
        await navigator.clipboard.writeText(text);
        b.textContent = said = "Copied";
      } catch {
        getSelection().selectAllChildren($(b.dataset.copy));
        b.textContent = `Press ${copyKey}`;
        said = `Selected. Press ${copyKey} to copy`;
      }
      $("copy-status").textContent = said;
      setTimeout(() => { b.textContent = "Copy"; $("copy-status").textContent = ""; }, 1800);
    });
  }

  // The talk-to-us form: sent in place, so the reader stays on the page. Without script it posts as a plain form and
  // the server answers with a page. "Ask to try it early" picks the Potluck Dish topic.
  const talk = $("talk"), said = $("talk-said");
  const WORDS = {
    missing: "Please add your name and a message.",
    email: "That email address doesn't look right.",
    topic: "Please pick what it's about.",
    long: "That's longer than the form takes. Please shorten it.",
    busy: "That's a lot of messages from one place. Please try again in an hour.",
  };
  talk.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    const send = talk.querySelector("button[type=submit]");
    send.disabled = true;
    talk.classList.remove("done");
    said.classList.remove("bad");
    said.textContent = "Sending…";
    let error;
    try {
      const res = await fetch(talk.action, { method: "POST", body: new FormData(talk), headers: { accept: "application/json" } });
      const out = await res.json().catch(() => ({ ok: false }));
      if (!out.ok) error = out.error;
    } catch {
      error = "network";
    }
    send.disabled = false;
    if (!error) {
      talk.reset();
      talk.classList.add("done");
      said.textContent = "Thanks. It's with us, and we'll write back to the address you gave.";
      return;
    }
    said.classList.add("bad");
    said.textContent = WORDS[error] ?? "It didn't send. Please try again, or write to ops@sengi.run.";
  });
  for (const a of document.querySelectorAll("a[data-topic]")) {
    a.addEventListener("click", () => { $("talk-topic").value = a.dataset.topic; });
  }

  show(chosen);
  total();
  bill();
})();

/* Step 1 figure: unfold the blanket as the figure scrolls through the viewport.
   Progress --bk-p runs 0 (figure top at the viewport bottom) to 1 (figure centred).
   The blanket is 288 x 160 units, cut into 4 x 2 cells of 72 x 80. Folded, it is three folds:
   right half over left (crease x=144), then the new right half over (x=72), then front over back (y=80).
   Each cell's 3D pose is those folds composed; a parallel projection makes each cell an affine
   matrix on the SVG group, so the gingham stays continuous and the moving panels get light and shade. */
(function () {
  var fig = document.querySelector(".bk-fig");
  if (!fig || !window.requestAnimationFrame) return;
  if (window.matchMedia && matchMedia("(prefers-reduced-motion: reduce)").matches) return;

  var E1 = [0.92, -0.2], E2 = [0.62, 0.36], O = [28, 165], K = 0.85; // screen = O + x*E1 + y*E2 - z*K (up)
  var T = 1.1; // cloth thickness, so stacked layers sort and show a little edge
  var VIEW = norm([-0.496, 0.736, 0.455]); // toward the viewer
  var LIGHT = norm([-0.35, 0.3, 1]);
  var svg = fig.querySelector(".bk-svg");
  var shadow = fig.querySelector(".bk-shadow");
  var group = fig.querySelector(".bk-cells");
  var cardBody = fig.querySelector(".bk-card-body");
  var cardShadow = fig.querySelector(".bk-card-shadow");
  var cells = [].slice.call(fig.querySelectorAll(".bk-cell")).map(function (el) {
    var c = +el.getAttribute("data-c"), r = +el.getAttribute("data-r");
    return { el: el, shade: el.querySelector(".bk-shade"), c: c, r: r, x0: c * 72, y0: r * 80 };
  });

  function norm(v) { var l = Math.hypot(v[0], v[1], v[2]); return [v[0] / l, v[1] / l, v[2] / l]; }
  function dot(a, b) { return a[0] * b[0] + a[1] * b[1] + a[2] * b[2]; }
  function sub(a, b) { return [a[0] - b[0], a[1] - b[1], a[2] - b[2]]; }
  function cross(a, b) { return [a[1] * b[2] - a[2] * b[1], a[2] * b[0] - a[0] * b[2], a[0] * b[1] - a[1] * b[0]]; }
  function clamp(v) { return v < 0 ? 0 : v > 1 ? 1 : v; }
  function ease(t) { return t < 0.5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2; }
  function span(p, a, b) { return ease(clamp((p - a) / (b - a))); }
  // Fold about a crease along y at x=c, height h (lifts +x side up and over to -x).
  function foldX(p, c, h, th) {
    var dx = p[0] - c, dz = p[2] - h, co = Math.cos(th), si = Math.sin(th);
    return [c + dx * co - dz * si, p[1], h + dx * si + dz * co];
  }
  function foldY(p, c, h, th) {
    var dy = p[1] - c, dz = p[2] - h, co = Math.cos(th), si = Math.sin(th);
    return [p[0], c + dy * co - dz * si, h + dy * si + dz * co];
  }
  function pose(cell, x, y, a1, a2, a3) {
    var p = [x, y, 0];
    if (cell.c >= 2) p = foldX(p, 144, T, a1);
    if (cell.c === 1 || cell.c === 2) p = foldX(p, 72, 2.5 * T, a2);
    if (cell.r === 1) p = foldY(p, 80, 6 * T, a3);
    return p;
  }
  function scr(p) { return [O[0] + p[0] * E1[0] + p[1] * E2[0], O[1] + p[0] * E1[1] + p[1] * E2[1] - p[2] * K]; }
  function f(n) { return Math.round(n * 1000) / 1000; }

  function draw(p) {
    fig.style.setProperty("--bk-p", p.toFixed(4));
    var a3 = Math.PI * (1 - span(p, 0.03, 0.34));
    var a2 = Math.PI * (1 - span(p, 0.32, 0.62));
    var a1 = Math.PI * (1 - span(p, 0.6, 0.9));
    var shadowPath = "";
    var flat = LIGHT[2];
    cells.forEach(function (cell) {
      var x0 = cell.x0, y0 = cell.y0;
      var P00 = pose(cell, x0, y0, a1, a2, a3), P10 = pose(cell, x0 + 72, y0, a1, a2, a3);
      var P01 = pose(cell, x0, y0 + 80, a1, a2, a3), P11 = pose(cell, x0 + 72, y0 + 80, a1, a2, a3);
      var S00 = scr(P00), S10 = scr(P10), S01 = scr(P01);
      var a = (S10[0] - S00[0]) / 72, b = (S10[1] - S00[1]) / 72;
      var c = (S01[0] - S00[0]) / 80, d = (S01[1] - S00[1]) / 80;
      var e = S00[0] - x0 * a - y0 * c, g = S00[1] - x0 * b - y0 * d;
      cell.el.setAttribute("transform", "matrix(" + [a, b, c, d, e, g].map(f).join(" ") + ")");
      var n = norm(cross(sub(P10, P00), sub(P01, P00)));
      var back = dot(n, VIEW) < 0;
      if (back) n = [-n[0], -n[1], -n[2]];
      var delta = dot(n, LIGHT) - flat - (back ? 0.05 : 0);
      var op = delta < 0 ? Math.min(0.34, -delta * 0.6) : 0;
      cell.shade.style.fill = delta < 0 ? "#3A0F08" : "#FFFFFF";
      cell.shade.style.fillOpacity = (delta < 0 ? op : Math.min(0.3, delta * 0.9)).toFixed(3);
      var zmax = Math.max(P00[2], P10[2], P01[2], P11[2]);
      cell.shade.style.strokeOpacity = Math.min(0.4, zmax / 12).toFixed(3);
      var mid = [(P00[0] + P11[0]) / 2, (P00[1] + P11[1]) / 2, (P00[2] + P11[2]) / 2];
      cell.depth = dot(mid, VIEW);
      // ground shadow: light from the back left, so lifted cloth throws shade forward and right
      shadowPath += "M" + [P00, P10, P11, P01].map(function (q) {
        var h = q[2] + 2.6;
        var s = scr([q[0] + h * 0.35, q[1] + h * 0.5, 0]);
        return f(s[0]) + " " + f(s[1]);
      }).join("L") + "Z";
    });
    shadow.setAttribute("d", shadowPath);
    cells.slice().sort(function (m, n) { return m.depth - n.depth; }).forEach(function (cell) { group.appendChild(cell.el); });
    var k = span(p, 0.86, 1);
    cardBody.style.transform = "translateY(" + f(-34 * (1 - k)) + "px)";
    cardBody.style.opacity = f(clamp(k * 1.6));
    cardShadow.style.opacity = f(0.22 * k);
  }

  var target = 0, shown = -1, ticking = false;
  function measure() {
    var r = fig.getBoundingClientRect(), vh = window.innerHeight || document.documentElement.clientHeight;
    target = clamp((vh - r.top) / (vh / 2 + r.height / 2));
  }
  function frame() {
    ticking = false;
    measure();
    var next = shown < 0 ? target : shown + (target - shown) * 0.2;
    if (Math.abs(target - next) < 0.0015) next = target;
    if (next !== shown) { shown = next; draw(shown); }
    if (shown !== target) request();
  }
  function request() { if (!ticking) { ticking = true; requestAnimationFrame(frame); } }

  fig.classList.add("bk-live");
  measure(); shown = target; draw(shown);
  window.addEventListener("scroll", request, { passive: true });
  window.addEventListener("resize", request, { passive: true });
})();

/* Skyline: machines drop onto the blanket one after another, once, when the figure scrolls into view. */
(function () {
  var fig = document.querySelector('.sk-fig');
  if (!fig || !('IntersectionObserver' in window)) return;
  if (window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches) return;
  var r = fig.getBoundingClientRect();
  if (r.top < window.innerHeight && r.bottom > 0) return; // already on screen: leave it drawn
  fig.classList.add('sk-armed');
  var io = new IntersectionObserver(function (es) {
    es.forEach(function (e) {
      if (e.isIntersecting) { fig.classList.add('sk-in'); io.disconnect(); }
    });
  }, { threshold: 0.4 });
  io.observe(fig);
})();

/* Step 3 figure: once it scrolls into view, the pointer moves to "Wind down", clicks it, and the jobs finish:
   the in-use count ticks from 6 to 0, then holds. Reduced motion (or no script): the menu stays as drawn, with
   "Wind down" highlighted. */
(function () {
  var fig = document.querySelector('.mb-fig');
  if (!fig) return;
  var reduce = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  if (reduce || !('IntersectionObserver' in window)) return;

  function q(name) { return fig.querySelector('[data-mb="' + name + '"]'); }
  var title = q('title'), use = q('use'), keep = q('keep'), wind = q('wind');
  var cores = fig.querySelectorAll('.mb-cores i');

  // The starting state: still lending, nothing highlighted, the pointer off on the desktop.
  wind.classList.remove('mb-hi');
  fig.classList.add('mb-start');

  function useLine(n) {
    if (n === 0) return 'No jobs left';
    var jobs = n > 3 ? 2 : 1;
    return n + (n === 1 ? ' core' : ' cores') + ' in use by ' + jobs + (jobs === 1 ? ' job' : ' jobs');
  }

  function play() {
    setTimeout(function () { fig.classList.remove('mb-start'); }, 250);
    setTimeout(function () { wind.classList.add('mb-hi'); }, 950);
    setTimeout(function () { wind.classList.add('mb-press'); }, 1450);
    setTimeout(function () {
      wind.classList.remove('mb-press');
      keep.querySelector('.mb-ck').textContent = '';
      wind.querySelector('.mb-ck').textContent = '✓';
      title.textContent = 'Winding down';
    }, 1600);
    var n = 6;
    function tick() {
      n -= 1;
      cores[n].className = 'mb-lent';
      use.textContent = useLine(n);
      if (n > 0) { setTimeout(tick, 360); return; }
      setTimeout(function () {
        title.textContent = 'Not lending';
        for (var i = 0; i < 6; i++) cores[i].className = '';
      }, 500);
    }
    setTimeout(tick, 2000);
  }

  var io = new IntersectionObserver(function (entries) {
    if (entries[0].isIntersecting) { io.disconnect(); play(); }
  }, { threshold: 0.6 });
  io.observe(fig);
})();
