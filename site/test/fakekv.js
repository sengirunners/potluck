// A KV namespace in memory: get, put with metadata, and list with prefix and cursor pages. `fail` names the
// calls that throw.
export function fakeKV(pageSize = 1000, fail = []) {
  const store = new Map();
  const check = (op) => {
    if (fail.includes(op)) throw new Error(`KV ${op}() failed.`);
  };
  return {
    store,
    async get(k) {
      check("get");
      return store.get(k)?.value ?? null;
    },
    async put(k, value, opts = {}) {
      check("put");
      store.set(k, { value, metadata: opts.metadata });
    },
    async list({ prefix = "", cursor, limit = pageSize } = {}) {
      check("list");
      const names = [...store.keys()].filter((k) => k.startsWith(prefix)).sort();
      const start = cursor ? Number(cursor) : 0;
      const n = Math.min(limit, pageSize);
      const slice = names.slice(start, start + n);
      const done = start + n >= names.length;
      return {
        keys: slice.map((name) => ({ name, metadata: store.get(name).metadata })),
        list_complete: done,
        cursor: done ? undefined : String(start + n),
      };
    },
  };
}
