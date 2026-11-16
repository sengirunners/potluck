import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";

// The step 2 figure: ten machines, and a bracket per pool under them. A pool's members are written once, on its
// bracket (data-members); its ticks, its ends and each machine's accessible name must agree with them.
const HTML = readFileSync(new URL("../public/index.html", import.meta.url), "utf8");
const fig = HTML.slice(HTML.indexOf('<figure class="sk-fig"'), HTML.indexOf("</figure>", HTML.indexOf('<figure class="sk-fig"')));
const unescape = (s) => s.replace(/&#39;/g, "'");

const machines = [...fig.matchAll(/<g class="sk-m" style="--i:(\d+)"[^>]*aria-label="([^"]+)"[^>]*><g transform="translate\(([\d.]+) 189\)"><rect class="sk-hit"(?: rx="[\d.]+")? x="([-\d.]+)" y="[-\d.]+" width="([\d.]+)"/g)]
  .map((m) => ({ i: +m[1], label: unescape(m[2]), left: +m[3] + +m[4], right: +m[3] + +m[4] + +m[5] }));
const pools = [...fig.matchAll(/<button class="sk-pool [^"]+" type="button" data-members="([\d ]+)" style="--l:(\d+);--r:(\d+);--at:(\d+);--row:\d" aria-label="([^"]+)">(.*?)<span class="sk-lab">([^<]+)<\/span><\/button>/g)]
  .map((m) => ({ members: m[1].split(" ").map(Number), l: +m[2], r: +m[3], label: unescape(m[5]),
    ticks: [...m[6].matchAll(/<i style="--x:(\d+)"><\/i>/g)].map((t) => +t[1]), name: unescape(m[7]) }));

test("the skyline has ten named machines, each with a name under it", () => {
  assert.deepEqual(machines.map((m) => m.i), [...Array(10).keys()]);
  assert.equal((fig.match(/class="sk-m"[^>]*role="img"[^>]*aria-label="[^"]+"/g) || []).length, 10);
  for (let i = 0; i < 10; i++) assert.match(fig, new RegExp(`<span class="sk-[ab][^"]*" data-m="${i}"`));
});

test("each pool bracket ticks exactly its members, ends at the first and last, and names them", () => {
  assert.deepEqual(pools.map((p) => p.name), ["Open source", "Startup", "Friends' project"]);
  for (const p of pools) {
    assert.equal(p.ticks.length, p.members.length, p.name);
    assert.equal(p.ticks[0], p.l, p.name);
    assert.equal(p.ticks.at(-1), p.r, p.name);
    p.members.forEach((m, k) => {
      const box = machines[m];
      assert.ok(p.ticks[k] >= box.left && p.ticks[k] <= box.right, `${p.name}: tick ${k} points at machine ${m}`);
    });
    const names = p.members.map((m) => machines[m].label.split(", in the ")[0]);
    assert.equal(p.label, `${p.name} pool: ${names.join(", ")}`);
  }
});

test("each machine's accessible name lists the pools whose brackets include it", () => {
  for (const m of machines) {
    const ins = pools.filter((p) => p.members.includes(m.i)).map((p) => p.name);
    assert.ok(ins.length > 0, m.label);
    assert.ok(m.label.endsWith(`, in the ${ins.join(" and ")} pool${ins.length > 1 ? "s" : ""}`), m.label);
  }
});
