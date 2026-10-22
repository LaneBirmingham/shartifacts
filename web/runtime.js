// window.claude.use(name): the capability runtime of Claude's artifacts, for previews on
// this server. Loaded by every preview before the artifact's own code. The preview is an
// opaque-origin sandbox without network access, so each call goes to the page embedding
// it (web/bridge.js), which answers through this server's API. Namespaces follow the
// runtime contract 0.2.84: user, db, assets, artifact (and self), comments, downloads,
// permissions, files, mcp, room and sample. Opened on its own, use() resolves null.
(() => {
  "use strict";
  const embedded = parent !== window;
  const claude = (window.claude = window.claude || {});
  const pending = new Map(),
    subs = new Map();
  let seq = 0;
  addEventListener("message", (e) => {
    const d = e.data;
    if (e.source !== parent || !d) return;
    if (d.type === "shartifacts-reply") {
      const p = pending.get(d.id);
      if (!p) return;
      pending.delete(d.id);
      d.error ? p.reject(d.error) : p.resolve(d.result);
    } else if (d.type === "shartifacts-event") subs.get(d.sub)?.(d.event);
  });
  const err = (code, message, extra) => Object.assign({ code, message }, extra);
  const call = (cap, method, args, timeout = 0) =>
    new Promise((resolve, reject) => {
      const id = ++seq;
      pending.set(id, { resolve, reject });
      parent.postMessage({ type: "shartifacts-call", id, cap, method, args }, "*");
      if (timeout)
        setTimeout(() => {
          if (pending.delete(id)) reject(err("upstream_error", "the page did not answer"));
        }, timeout);
    });
  const subscribe = (cap, method, args, handler) => {
    const sub = "s" + ++seq;
    subs.set(sub, handler);
    parent.postMessage({ type: "shartifacts-subscribe", sub, cap, method, args }, "*");
    return () => {
      if (subs.delete(sub)) parent.postMessage({ type: "shartifacts-unsubscribe", sub }, "*");
    };
  };
  const later = (f) => Promise.resolve().then(f);
  const plain = (v) => v !== null && typeof v === "object" && Object.getPrototypeOf(v) === Object.prototype;
  const freezeDeep = (v) => {
    if (v && typeof v === "object" && !Object.isFrozen(v)) {
      Object.freeze(v);
      Object.values(v).forEach(freezeDeep);
    }
    return v;
  };
  const jsonBytes = (v) => new TextEncoder().encode(JSON.stringify(v ?? null)).length;
  let info = null;
  const hello = embedded
    ? Promise.race([
        call("runtime", "hello", {}).then((i) => (info = i)),
        new Promise((r) => setTimeout(() => r(null), 10000)),
      ]).catch(() => null)
    : Promise.resolve(null);

  // ---- user -------------------------------------------------------------------------
  const initials = (name) =>
    (name || "?")
      .split(/\s+/)
      .map((w) => [...w][0] || "")
      .join("")
      .slice(0, 2)
      .toUpperCase();
  const avatar = (name, color) =>
    "data:image/svg+xml," +
    encodeURIComponent(
      `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><rect width="64" height="64" rx="32" fill="${color || "#8f8882"}"/><text x="32" y="41" font-family="sans-serif" font-size="26" text-anchor="middle" fill="#fff">${name ? initials(name).replace(/[<&>]/g, "") : "•"}</text></svg>`,
    );
  const profileOf = (p, id) =>
    freezeDeep({
      id,
      name: p?.name || "",
      avatarUrl: avatar(p?.name, p?.color),
      color: p?.color || "#8f8882",
      email: null,
      isMe: !!info?.me?.id && id === info.me.id,
      guest: false,
    });
  const user = () => {
    const me = () => {
      const m = info.me;
      return freezeDeep({ id: m.id, name: m.name || "", avatarUrl: avatar(m.name, m.color), color: m.color || "#8f8882", email: null, isOwner: m.isOwner, canEdit: m.canEdit });
    };
    const canNames = { "data.write": true, "files.write": true, "assets.write": true, "people.add": false };
    return {
      isOwner: async () => !!info.me.isOwner,
      canEdit: async () => !!info.me.canEdit,
      can: async (name) => (info.thumbnail ? false : canNames[name] ?? false),
      me: async () => me(),
      id: async () => info.me.id,
      name: async () => info.me.name || "",
      avatarUrl: async () => (info.me.id ? me().avatarUrl : null),
      email: async () => null,
      profiles: async (ids) => {
        const list = [...new Set(typeof ids === "string" ? [ids] : Array.from(ids || []))];
        const people = await call("user", "people", {}).catch(() => ({}));
        return Object.fromEntries(list.map((id) => [id, profileOf(people[id], id)]));
      },
      search: async (q) => {
        const people = await call("user", "people", {}).catch(() => ({}));
        const query = String(q ?? "").toLowerCase();
        return Object.entries(people)
          .filter(([id, p]) => (query ? p.name.toLowerCase().includes(query) : id === info.me.id))
          .slice(0, 8)
          .map(([id, p]) => profileOf(p, id));
      },
    };
  };

  // ---- db ---------------------------------------------------------------------------
  const SEG = /^[A-Za-z0-9_\-.~:@+]+$/;
  const checkPath = (path, wantDoc) => {
    if (typeof path !== "string" || !path) throw new TypeError("db: a path must be a non-empty string");
    const segs = path.split("/");
    if (path.length > 1000 || segs.length > 16) throw new TypeError("db: at most 16 segments and 1000 bytes per path");
    for (const s of segs)
      if (!SEG.test(s) || s === "." || s === ".." || s.length > 200)
        throw new TypeError(`db: segment "${s}" must be letters, digits or _ - . ~ : @ +`);
    if ((segs.length % 2 === 0) !== wantDoc)
      throw new TypeError(`db: ${path} has ${segs.length} segments; a ${wantDoc ? "document" : "collection"} path needs an ${wantDoc ? "even" : "odd"} number`);
    return path;
  };
  const dbErr = (e) => (e && e.code ? e : err("unavailable", String(e?.message || e)));
  const meta = Object.freeze({ fromCache: false, hasPendingWrites: false });
  const docCache = new Map();
  const snapshotOf = (d) => {
    const key = d.path + "@" + (d.exists ? d.version : 0);
    let snap = docCache.get(key);
    if (!snap) {
      const data = d.exists ? freezeDeep(d.data) : undefined;
      snap = Object.freeze({ id: d.id, exists: !!d.exists, data: () => data, metadata: meta });
      docCache.set(key, snap);
      if (docCache.size > 5000) docCache.delete(docCache.keys().next().value);
    }
    return snap;
  };
  const randomId = () =>
    Array.from(crypto.getRandomValues(new Uint8Array(20)), (b) => "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"[b % 62]).join("");
  let liveSubs = 0;
  const db = () => {
    const write = (op, path, data) => {
      if (op !== "delete" && !plain(data)) return Promise.reject(err("invalid_argument", "a document body must be a plain object"));
      return call("db", "write", { op, path, data }).then(() => undefined, (e) => Promise.reject(dbErr(e)));
    };
    const docRef = (path) => {
      checkPath(path, true);
      const ref = {
        id: path.split("/").pop(),
        path,
        get: () => call("db", "get", { path }).then(snapshotOf, (e) => Promise.reject(dbErr(e))),
        set: (data) => write("set", path, data),
        update: (data) => write("update", path, data),
        delete: () => write("delete", path),
        acquire: (o) =>
          !o || typeof o.holder !== "string" || !o.holder
            ? Promise.reject(err("invalid_argument", "acquire needs {holder}"))
            : call("db", "acquire", { path, holder: o.holder, ttlMs: o.ttlMs || 0, data: o.data }).catch((e) => Promise.reject(dbErr(e))),
        onSnapshot: (next, error) => watch({ path }, next, error, (r) => snapshotOf(r.doc)),
        collection: (sub) => colRef(path + "/" + sub),
      };
      return Object.freeze(ref);
    };
    const queryOf = (path, q) => {
      const with_ = (patch) => queryOf(path, { ...q, ...patch });
      const query = {
        where: (field, op, value) => {
          if ((q.where || []).length >= 10) throw new TypeError("db: at most 10 filters");
          return with_({ where: [...(q.where || []), { field, op, value }] });
        },
        orderBy: (field, dir = "asc") => with_({ order_by: field, dir }),
        limit: (n) => {
          if (!(n >= 1 && n <= 1000)) throw new TypeError("db: limit must be 1-1000");
          return with_({ limit: n });
        },
        get: () => call("db", "query", { path, query: q }).then((r) => querySnap(r.docs, null), (e) => Promise.reject(dbErr(e))),
        onSnapshot: (next, error) => {
          let prev = null;
          return watch({ path, query: q }, next, error, (r) => (prev = querySnap(r.docs, prev)));
        },
      };
      return query;
    };
    const querySnap = (docs, prev) => {
      const snaps = docs.map(snapshotOf),
        before = prev ? prev.docs : [];
      const changes = [];
      const oldIndex = new Map(before.map((s, i) => [s.id, i]));
      snaps.forEach((s, i) => {
        if (!oldIndex.has(s.id)) changes.push({ type: "added", doc: s, oldIndex: -1, newIndex: i });
        else if (before[oldIndex.get(s.id)] !== s) changes.push({ type: "modified", doc: s, oldIndex: oldIndex.get(s.id), newIndex: i });
      });
      const ids = new Set(snaps.map((s) => s.id));
      before.forEach((s, i) => {
        if (!ids.has(s.id)) changes.push({ type: "removed", doc: s, oldIndex: i, newIndex: -1 });
      });
      const frozen = Object.freeze(changes.map(Object.freeze));
      return Object.freeze({ docs: Object.freeze(snaps), size: snaps.length, empty: !snaps.length, docChanges: () => frozen, metadata: meta });
    };
    const watch = (target, next, error, make) => {
      if (typeof next !== "function") throw new TypeError("onSnapshot needs a function");
      let dead = false;
      const die = (e) => {
        if (dead) return;
        dead = true;
        stop();
        liveSubs--;
        e = dbErr(e);
        if (typeof error === "function") error(e);
        else reportError(Object.assign(new Error(e.message), e));
      };
      if (++liveSubs > 64) {
        liveSubs--;
        later(() => typeof error === "function" ? error(err("resource_exhausted", "at most 64 subscriptions per view")) : 0);
        return () => {};
      }
      const stop = subscribe("db", "watch", target, (ev) => {
        if (dead) return;
        if (ev.error) return die(ev.error);
        next(make(ev));
      });
      return () => {
        if (!dead) {
          dead = true;
          liveSubs--;
          stop();
        }
      };
    };
    const colRef = (path) => {
      checkPath(path, false);
      return Object.freeze({
        ...queryOf(path, {}),
        path,
        doc: (id) => docRef(path + "/" + (id === undefined ? randomId() : id)),
        add: async (data) => {
          const ref = docRef(path + "/" + randomId());
          await ref.set(data);
          return ref;
        },
      });
    };
    return { doc: docRef, collection: colRef };
  };

  // ---- assets -----------------------------------------------------------------------
  const assets = () => ({
    upload: (blob, options = {}) => {
      if (!(blob instanceof Blob) || !blob.size) return Promise.reject(err("invalid_request", "upload takes a non-empty Blob or File"));
      const type = options.type || blob.type;
      if (!type) return Promise.reject(err("invalid_request", "pass options.type when blob.type is empty"));
      return call("assets", "upload", { blob, type });
    },
    list: () => call("assets", "list", {}),
    delete: (ref) =>
      typeof ref !== "string" || !ref
        ? Promise.reject(err("invalid_request", "delete takes an asset id or url"))
        : call("assets", "delete", { id: ref.replace(/^.*\/_blob\//, "") }),
  });

  // ---- artifact (and its legacy name, self) ------------------------------------------
  const artifact = () => ({
    publish: async (arg) => {
      if (typeof arg === "string") {
        if (!/^\s*<!doctype\s+html/i.test(arg)) throw err("invalid_content", "a page must begin with <!doctype html>");
        return call("artifact", "publish", { html: arg });
      }
      if (!plain(arg)) throw err("invalid_content", "publish takes an html string or a files object");
      const files = {};
      for (const [path, f] of Object.entries(arg)) {
        if (f === null || (plain(f) && f.delete === true)) files[path] = { delete: true, checked: plain(f) && "ifMatch" in f, if_match: f?.ifMatch ?? null };
        else if (typeof f === "string") files[path] = { content: f };
        else if (f instanceof Blob) files[path] = { content: await f.text() };
        else if (plain(f) && (typeof f.content === "string" || f.content instanceof Blob))
          files[path] = { content: typeof f.content === "string" ? f.content : await f.content.text(), checked: "ifMatch" in f, if_match: f.ifMatch ?? null };
        else throw err("invalid_content", `file ${path} is not a string, Blob or {content}`);
      }
      return call("artifact", "publish", { files });
    },
    edit: async () => {
      throw err("invalid_content", "this artifact is not a live doc");
    },
    sync: async () => {
      throw err("invalid_content", "this artifact is not a live doc");
    },
    metadata: (refs) => call("artifact", "metadata", { refs: Array.from(refs || []) }).catch(() => ({})),
  });

  // ---- comments ---------------------------------------------------------------------
  const cssPath = (el) => {
    const parts = [];
    for (let n = el; n && n.nodeType === 1 && n !== document.documentElement; n = n.parentElement) {
      if (n.id && /^[A-Za-z][\w-]*$/.test(n.id)) {
        parts.unshift("#" + n.id);
        break;
      }
      const same = n.parentElement ? [...n.parentElement.children].filter((c) => c.tagName === n.tagName) : [];
      parts.unshift(n.tagName.toLowerCase() + (same.length > 1 ? `:nth-of-type(${same.indexOf(n) + 1})` : ""));
    }
    return parts.join(" > ");
  };
  const commentText = (t) =>
    typeof t !== "string" || !t.trim() || new TextEncoder().encode(t).length > 4096 || /[\u0000-\u0008\u000b-\u001f]/.test(t)
      ? Promise.reject(err("invalid", "text must be 1 byte to 4 KiB of plain text"))
      : null;
  const comments = () => ({
    openComposer: (target) => {
      const el = target?.element,
        range = target?.range;
      if (!(el instanceof Element && el.isConnected) && !(range instanceof Range)) return Promise.reject(err("invalid", "target must be {element} or {range} in this document"));
      const quote = (range ? range.toString() : el.textContent || "").trim().slice(0, 500);
      return call("comments", "openComposer", { quote, anchor: cssPath(el || range.commonAncestorContainer.parentElement) });
    },
    anchorFor: async (el) => {
      if (!(el instanceof Element) || !el.isConnected) throw err("invalid", "anchorFor takes an element in this document");
      const r = el.getBoundingClientRect();
      return Object.freeze({ path: cssPath(el), x: Math.round(r.left + r.width / 2 + scrollX), y: Math.round(r.top + r.height / 2 + scrollY) });
    },
    create: (o) => commentText(o?.text) || call("comments", "create", { anchor: o.anchor?.path || "", text: o.text }),
    reply: (threadId, text) => commentText(text) || call("comments", "reply", { threadId, text }),
    sendToClaude: (o) =>
      commentText(o?.text) || call("comments", "create", { anchor: o.anchor?.path || "", threadId: o.threadId, text: o.text, toClaude: true }),
    canSendToClaude: async () => "available",
    resolve: (threadId, resolved) => call("comments", "resolve", { threadId, resolved: !!resolved }),
    delete: (threadId) => call("comments", "delete", { threadId }),
    customAnchors: async () => {
      throw err("not_granted", "customAnchors is not declared for this artifact");
    },
  });

  // ---- downloads ----------------------------------------------------------------------
  const DOWNLOAD_EXT = /\.(gif|png|jpe?g|webp|mp4|webm|mov|txt|json|md|docx|pptx|epub|csv|ttf|html|svg|pdf|xlsx|zip)$/i;
  const downloads = () => ({
    save: (req) => {
      if (!plain(req) || typeof req.filename !== "string" || !req.filename || req.filename.length > 512) return Promise.reject(err("bad_request", "save needs {filename, data}"));
      if (!DOWNLOAD_EXT.test(req.filename.trim())) return Promise.reject(err("rejected_extension", "that file type cannot be saved from an artifact"));
      if (req.request) return Promise.reject(err("request_unknown", "no export was requested"));
      let data = req.data;
      if (ArrayBuffer.isView(data)) data = new Blob([data]);
      if (data instanceof ArrayBuffer) data = new Blob([data]);
      if (typeof data === "string") data = new Blob([data]);
      if (!(data instanceof Blob) || !data.size) return Promise.reject(err("bad_request", "data must be non-empty"));
      return call("downloads", "save", { filename: req.filename.trim(), data });
    },
  });

  // ---- permissions ------------------------------------------------------------------
  const permissions = () => {
    const available = () => Object.fromEntries(Object.entries(info.capabilities).filter(([, on]) => on).map(([name]) => [name, "granted"]));
    return {
      state: async (name) => (name === undefined ? available() : available()[String(name).split(":")[0]] || "unavailable"),
      request: async (names) => {
        const all = available();
        return names === undefined ? all : Object.fromEntries(Array.from(names, (n) => [n, all[String(n).split(":")[0]] || "unavailable"]));
      },
      manage: async () => {
        throw err("unavailable", "permissions are managed by this server's operator; nothing to change here");
      },
    };
  };

  // ---- files: the Media library, as the project's files ----------------------------------
  const files = () => {
    const ns = {
      list: (o = {}) => call("files", "list", { path: o.path || "", recursive: !!o.recursive }),
      read: async (path, o = {}) => {
        const r = await call("files", "read", { path, offset: o.offset || 0, length: o.length || 26214400 });
        return { bytes: new Uint8Array(r.bytes), size: r.size, revision: r.revision };
      },
      download: (path) => call("files", "download", { path }),
      asSampleTools: async () => [
        {
          name: "list_project_files",
          description: "List the files in the project's Media library: path, size and creation time.",
          inputSchema: { type: "object", properties: {} },
          execute: async () => (await ns.list()).entries.map((e) => ({ path: e.path, size: e.size })),
        },
        {
          name: "read_project_file",
          description: "Read a text file from the project's Media library by path (up to about 12 KB).",
          inputSchema: { type: "object", properties: { path: { type: "string" } }, required: ["path"] },
          execute: async (input) => new TextDecoder().decode((await ns.read(String(input.path), { length: 12000 })).bytes),
        },
      ],
    };
    return ns;
  };

  // ---- mcp: the server's connectors, plus this artifact's own data ----------------------
  const mcp = () => {
    const cache = new Map(),
      watches = new Map(),
      keyOf = (s, t, i) => JSON.stringify([s, t, sortKeys(i ?? {})]);
    const sortKeys = (v) =>
      Array.isArray(v) ? v.map(sortKeys) : plain(v) ? Object.fromEntries(Object.keys(v).sort().map((k) => [k, sortKeys(v[k])])) : v;
    const withPayload = (r) => {
      let payload = r.structuredContent;
      if (payload === undefined) {
        const t = (r.content || []).find((c) => c.type === "text")?.text;
        try {
          payload = JSON.parse(t);
        } catch {
          payload = t;
        }
      }
      return { ...r, payload };
    };
    const callTool = (server, tool, input, options = {}) => {
      if (typeof server !== "string" || typeof tool !== "string") return Promise.reject(err("bad_request", "server and tool must be strings"));
      if (options.signal?.aborted) return Promise.reject(err("cancelled", "cancelled"));
      const key = keyOf(server, tool, input),
        c = options.cache;
      const hit = c && !c.refresh ? cache.get(key) : null;
      if (hit && Date.now() - hit.storedAt < Math.min(c.staleTime || 0, 300000)) return Promise.resolve({ ...hit.result, cache: { storedAt: hit.storedAt, revalidating: false } });
      const run = call("mcp", "callTool", { server, tool, input: input ?? {} }).then((r) => {
        const result = withPayload(r);
        if (c && (c.gcTime ?? 300000) > 0) cache.set(key, { result, storedAt: Date.now() });
        for (const w of watches.values()) if (w.key === key) w.deliver({ type: "data", result });
        return result;
      });
      if (!options.signal) return run;
      return new Promise((resolve, reject) => {
        options.signal.addEventListener("abort", () => reject(err("cancelled", "cancelled")), { once: true });
        run.then(resolve, reject);
      });
    };
    const listTools = (server) =>
      call("mcp", "listTools", {}).then((r) => (server === undefined ? r : { servers: r.servers.filter((s) => s.server === server) }));
    return {
      callTool,
      listTools,
      describeTool: (server, tool) => call("mcp", "describeTool", { server, tool }),
      invalidate: async (server, tool, input) => {
        if (input !== undefined && (server === undefined || tool === undefined)) throw err("bad_request", "input needs server and tool");
        for (const k of [...cache.keys()]) {
          const [s, t] = JSON.parse(k);
          if ((server === undefined || s === server) && (tool === undefined || t === tool) && (input === undefined || k === keyOf(server, tool, input))) cache.delete(k);
        }
        for (const w of watches.values()) {
          const [s, t] = JSON.parse(w.key);
          if ((server === undefined || s === server) && (tool === undefined || t === tool)) w.refetch();
        }
      },
      watchTool: (server, tool, input, handler, options = {}) => {
        if (typeof handler !== "function") throw new TypeError("watchTool needs a handler");
        const id = ++seq,
          key = keyOf(server, tool, input);
        let timer;
        const w = {
          key,
          deliver: (ev) => watches.has(id) && handler(ev),
          refetch: () =>
            callTool(server, tool, input, { cache: { gcTime: options.cache?.gcTime ?? 300000, refresh: true } }).catch((e) => w.deliver({ type: "error", error: e })),
        };
        watches.set(id, w);
        later(() => {
          const hit = cache.get(key);
          if (hit) w.deliver({ type: "data", result: { ...hit.result, cache: { storedAt: hit.storedAt, revalidating: true } } });
          w.refetch();
        });
        if (options.refetchInterval) timer = setInterval(() => document.visibilityState === "visible" && w.refetch(), Math.max(30000, options.refetchInterval));
        return () => {
          watches.delete(id);
          clearInterval(timer);
        };
      },
      server: async (name) => {
        const { servers } = await listTools(name);
        if (!servers.length || !servers[0].tools.length) throw err("server_not_connected", "no server named " + name, { server: name });
        if (servers[0].authStatus === "needs_reauth") throw err("needs_reauth", name + " needs reconnecting", { server: name });
        const handle = Object.create(null);
        for (const t of servers[0].tools)
          if (!["then", "toJSON"].includes(t.name) && !(t.name in Object.prototype))
            handle[t.name] = (input, options) => callTool(name, t.name, input, options).then((r) => r.payload ?? r);
        return Object.freeze(handle);
      },
    };
  };

  // ---- room ---------------------------------------------------------------------------
  const TOPIC = /^[a-z][a-z0-9_.-]{0,47}$/,
    ROOM = /^[a-z0-9][a-z0-9_.-]{0,47}$/;
  const room = () => {
    const rooms = new Map();
    let connected = false;
    const connListeners = new Set();
    const makeRoom = (name) => {
      const r = {
        name,
        peers: Object.freeze([]),
        byPeer: new Map(),
        topics: new Map(),
        peerListeners: new Set(),
        mine: {},
        sent: {},
        flush: null,
        left: false,
      };
      rooms.set(name, r);
      return r;
    };
    const lobby = makeRoom("lobby");
    const senderOf = (peer, by) => ({ peer, by: by ?? null, isMe: peer === info.peer || (!!by && by === info.me.id), sameTab: peer === info.peer, kind: "viewer", guest: false });
    const setPeers = (r, list) => {
      const next = new Map();
      for (const p of list) {
        const old = r.byPeer.get(p.peer);
        next.set(p.peer, old && old.updatedAt === p.updatedAt && JSON.stringify(old.presence) === JSON.stringify(p.presence) ? old : freezeDeep({ ...senderOf(p.peer, p.by), presence: p.presence || {}, updatedAt: Date.now() }));
      }
      const joined = [...next.values()].filter((p) => !r.byPeer.has(p.peer));
      const left = [...r.byPeer.values()].filter((p) => !next.has(p.peer));
      const updated = [...next.values()].filter((p) => r.byPeer.has(p.peer) && r.byPeer.get(p.peer) !== p);
      r.byPeer = next;
      r.peers = Object.freeze([...next.values()]);
      if (joined.length || left.length || updated.length) {
        const change = Object.freeze({ peers: r.peers, joined: Object.freeze(joined), left: Object.freeze(left), updated: Object.freeze(updated) });
        r.peerListeners.forEach((f) => f(change));
      }
    };
    subscribe("room", "events", {}, (ev) => {
      if (ev.kind === "connection") {
        connected = ev.connected;
        connListeners.forEach((f) => f(connected));
        return;
      }
      const r = rooms.get(ev.room);
      if (!r || r.left) return;
      if (ev.kind === "peers") setPeers(r, ev.peers);
      else if (ev.kind === "message") {
        const msg = Object.freeze({ ...senderOf(ev.peer, ev.by), topic: ev.topic, data: freezeDeep(ev.data) });
        r.topics.get(ev.topic)?.forEach((f) => f(msg));
      }
    });
    const api = (r) => ({
      emit: (topic, data) => {
        if (r.left) return Promise.reject(err("invalid_argument", "you left this room"));
        if (!TOPIC.test(topic)) return Promise.reject(err("invalid_argument", "topic must match ^[a-z][a-z0-9_.-]{0,47}$"));
        if (jsonBytes(data) > 4096) return Promise.reject(err("invalid_argument", "data is at most 4 KiB of JSON"));
        if (!connected) return Promise.resolve();
        return call("room", "send", { room: r.name, op: "emit", topic, data }).then(() => undefined);
      },
      on: (topic, handler, onError) => {
        if (typeof handler !== "function") throw new TypeError("on needs a handler");
        if (!TOPIC.test(topic)) {
          later(() => onError?.(err("invalid_argument", "topic must match ^[a-z][a-z0-9_.-]{0,47}$")));
          return () => {};
        }
        if (!r.topics.has(topic)) r.topics.set(topic, new Set());
        const f = (m) => handler(m);
        r.topics.get(topic).add(f);
        return () => r.topics.get(topic)?.delete(f);
      },
      presence: (patch) => {
        if (!plain(patch)) return Promise.reject(err("invalid_argument", "presence takes a plain object"));
        const merged = { ...r.mine };
        for (const [k, v] of Object.entries(patch)) v === null ? delete merged[k] : (merged[k] = v);
        if (jsonBytes(merged) > 4096) return Promise.reject(err("invalid_argument", "presence is at most 4 KiB of JSON"));
        r.mine = merged;
        // Applied here at once; sent coalesced, about 30 times a second.
        const self = r.byPeer.get(info.peer);
        if (self) setPeers(r, [...r.byPeer.values()].map((p) => (p.peer === info.peer ? { peer: p.peer, by: p.by, presence: merged } : { peer: p.peer, by: p.by, presence: p.presence, updatedAt: p.updatedAt })));
        if (!r.flush)
          r.flush = setTimeout(() => {
            r.flush = null;
            // Send the whole object, clearing fields removed since the last send.
            const patch = { ...r.mine };
            for (const k of Object.keys(r.sent)) if (!(k in r.mine)) patch[k] = null;
            r.sent = { ...r.mine };
            if (connected) call("room", "send", { room: r.name, op: "presence", patch }).catch(() => {});
          }, 33);
        return Promise.resolve();
      },
      peers: () => r.peers,
      onPeers: (handler, onError) => {
        if (typeof handler !== "function") throw new TypeError("onPeers needs a handler");
        r.peerListeners.add(handler);
        later(() => r.peerListeners.has(handler) && handler(Object.freeze({ peers: r.peers, joined: r.peers, left: Object.freeze([]), updated: Object.freeze([]) })));
        return () => r.peerListeners.delete(handler);
      },
      connected: () => connected,
      onConnection: (handler) => {
        if (typeof handler !== "function") throw new TypeError("onConnection needs a handler");
        connListeners.add(handler);
        later(() => connListeners.has(handler) && handler(connected));
        return () => connListeners.delete(handler);
      },
    });
    const ns = {
      ...api(lobby),
      join: async (name) => {
        if (!ROOM.test(name || "")) throw err("invalid_argument", "room names match ^[a-z0-9][a-z0-9_.-]{0,47}$");
        if (rooms.has(name) && !rooms.get(name).left) return rooms.get(name).handle;
        const r = makeRoom(name);
        await call("room", "send", { room: name, op: "join" });
        r.handle = Object.freeze({
          name,
          ...api(r),
          leave: async () => {
            if (r.left) return;
            r.left = true;
            rooms.delete(name);
            await call("room", "send", { room: name, op: "leave" }).catch(() => {});
          },
        });
        return r.handle;
      },
      onFollow: (handler) => {
        if (typeof handler !== "function") throw new TypeError("onFollow needs a handler");
        return () => {};
      },
      followControls: async () => "none",
      sendToClaudeSession: (data, options = {}) => {
        if (!plain(data) || !Object.keys(data).length || jsonBytes(data) > 4096) return Promise.reject(err("invalid_argument", "send a plain object of at most 4 KiB"));
        const presence = options.attachPresence === false ? undefined : lobby.mine;
        return call("room", "sendToClaude", { data, presence });
      },
      canSendToClaudeSession: async () => "available",
    };
    return ns;
  };

  // ---- sample -----------------------------------------------------------------------
  const sample = () => {
    const answers = new Map();
    const toBase64 = (blob) =>
      new Promise((resolve, reject) => {
        const fr = new FileReader();
        fr.onload = () => resolve(String(fr.result).split(",")[1]);
        fr.onerror = () => reject(err("image_rejected", "could not read an image"));
        fr.readAsDataURL(blob);
      });
    const parseJSON = (text) => {
      const tries = [text, (text.match(/```(?:json)?\s*\n?([\s\S]*?)```/) || [])[1]];
      const a = text.search(/[[{]/),
        b = Math.max(text.lastIndexOf("}"), text.lastIndexOf("]"));
      if (a >= 0 && b > a) tries.push(text.slice(a, b + 1));
      for (const t of tries) {
        if (typeof t !== "string") continue;
        try {
          return { ok: true, value: JSON.parse(t) };
        } catch {}
      }
      return { ok: false };
    };
    const run = async (input, options = {}, asJSON) => {
      if (!plain(options)) throw err("invalid_request", "options must be a plain object");
      const turns = typeof input === "string" ? [{ role: "user", content: input }] : input;
      if (!Array.isArray(turns) || !turns.length || (typeof input === "string" && !input.trim())) throw err("invalid_request", "input must be a prompt string or a list of turns");
      for (const t of turns)
        if (!t || !["user", "assistant"].includes(t.role) || typeof t.content !== "string" || !t.content) throw err("invalid_request", "each turn is {role: user|assistant, content: non-empty string}");
      if (turns[0].role !== "user" || turns.at(-1).role !== "user") throw err("invalid_request", "turns start and end with a user turn");
      if (new TextEncoder().encode(turns.map((t) => t.content).join("")).length > 262144) throw err("prompt_too_large", "input is over 256 KiB");
      const { signal, onText, tools = [], images, modelTier, model, effort } = options;
      if (signal !== undefined && !(signal instanceof AbortSignal)) throw err("invalid_request", "signal must be an AbortSignal (pass ctl.signal)");
      if (onText !== undefined && typeof onText !== "function") throw err("invalid_request", "onText must be a function");
      if (modelTier !== undefined && !["default", "quick", "complex"].includes(modelTier)) throw err("invalid_request", "modelTier is quick, default or complex");
      if (!Array.isArray(tools)) throw err("invalid_request", "tools must be an array");
      for (const t of tools)
        if (!t || !/^[A-Za-z0-9_-]{1,128}$/.test(t.name) || typeof t.description !== "string" || !t.description || typeof t.execute !== "function")
          throw err("invalid_request", "each tool is {name, description, inputSchema?, execute}");
      const cacheOpt = options.cache ?? true;
      if (tools.length && cacheOpt !== false && options.cache !== undefined) throw err("invalid_request", "calls with tools are never cached: omit cache");
      const pics = images === undefined ? [] : images instanceof Blob ? [images] : Array.from(images);
      if (pics.some((p) => !(p instanceof Blob))) throw err("invalid_request", "images must be Blobs");
      if (pics.some((p) => !["image/jpeg", "image/png", "image/webp", "image/gif"].includes(p.type) || p.size > 20e6)) throw err("image_rejected", "images are JPEG, PNG, WebP or GIF up to 20 MB");
      if (signal?.aborted) throw err("cancelled", "cancelled");
      const key = JSON.stringify([asJSON, turns, modelTier, model, effort]);
      const gc = cacheOpt === true ? 300000 : plain(cacheOpt) ? Math.min(cacheOpt.gcTime ?? 300000, 86400000) : 0;
      const cached = answers.get(key);
      if (gc && !tools.length && !pics.length && !(plain(cacheOpt) && cacheOpt.refresh) && cached && Date.now() - cached.at < gc) {
        await later(() => onText?.({ text: cached.result.text, delta: cached.result.text }));
        return cached.result;
      }
      const content = (text) => [{ type: "text", text }];
      const messages = turns.map((t) => ({ role: t.role, content: content(t.content) }));
      for (const p of pics) messages.at(-1).content.push({ type: "image", source: { type: "base64", media_type: p.type, data: await toBase64(p) } });
      const apiTools = tools.map((t) => ({ name: t.name, description: t.description, input_schema: t.inputSchema || { type: "object", properties: {} } }));
      // text is the whole answer so far: every round's words, a blank line between rounds.
      let text = "",
        result;
      const emit = (delta) => {
        text += delta;
        try {
          onText?.({ text, delta });
        } catch (e) {
          console.error(e);
        }
      };
      for (let round = 0; round < 8; round++) {
        let first = true;
        const done = await new Promise((resolve, reject) => {
          const stop = subscribe("sample", "round", { messages, tools: apiTools, model, model_tier: modelTier, effort, json: asJSON, stream: !!onText }, (ev) => {
            if (ev.type === "text") {
              emit((first && text ? "\n\n" : "") + ev.delta);
              first = false;
            } else if (ev.type === "done") {
              stop();
              resolve(ev);
            } else if (ev.type === "error") {
              stop();
              reject(err(ev.code || "upstream_error", ev.message || "Claude is unavailable", text && ev.code !== "refused" ? { text } : {}));
            }
          });
          signal?.addEventListener(
            "abort",
            () => {
              stop();
              reject(err("cancelled", "cancelled", text ? { text } : {}));
            },
            { once: true },
          );
        });
        if (done.stop_reason === "refusal") throw err("refused", "Claude declined this request");
        const blocks = done.message.content || [];
        const said = blocks.filter((b) => b.type === "text").map((b) => b.text).join("");
        // Without streaming, the round's words arrive whole with its result.
        if (said && !onText) text += (text ? "\n\n" : "") + said;
        messages.push(done.message);
        result = { text, truncated: done.stop_reason === "max_tokens", modelTierApplied: done.model_tier || modelTier || "default", ...(done.model ? { modelApplied: done.model } : {}) };
        const calls = blocks.filter((b) => b.type === "tool_use");
        if (done.stop_reason !== "tool_use" || !calls.length || !tools.length) break;
        const results = await Promise.all(
          calls.map(async (c) => {
            const tool = tools.find((t) => t.name === c.name);
            const ctl = new AbortController();
            const timer = setTimeout(() => ctl.abort(), 150000);
            signal?.addEventListener("abort", () => ctl.abort(), { once: true });
            try {
              if (!tool) throw new Error("no tool named " + c.name);
              const out = await tool.execute(c.input || {}, { signal: ctl.signal });
              const s = typeof out === "string" ? out : JSON.stringify(out ?? null);
              return { type: "tool_result", tool_use_id: c.id, content: s.slice(0, 32768) };
            } catch (e) {
              return { type: "tool_result", tool_use_id: c.id, content: "Error: " + (e?.message || String(e)), is_error: true };
            } finally {
              clearTimeout(timer);
            }
          }),
        );
        if (signal?.aborted) throw err("cancelled", "cancelled", text ? { text } : {});
        messages.push({ role: "user", content: results });
      }
      if (!result || !result.text.trim()) throw err("empty_completion", "Claude produced no text");
      result = Object.freeze(result);
      if (gc && !tools.length && !pics.length) answers.set(key, { result, at: Date.now() });
      return result;
    };
    const fn = (input, options) => run(input, options, false);
    fn.json = async (input, options) => {
      let r;
      try {
        r = await run(input, options, true);
      } catch (e) {
        throw e;
      }
      if (r.truncated) throw err("invalid_json", "the reply was cut short", { text: r.text });
      const parsed = parseJSON(r.text);
      if (!parsed.ok) {
        throw err("invalid_json", "the reply held no JSON value", { text: r.text });
      }
      return parsed.value;
    };
    fn.limits = () => call("sample", "limits", {});
    return fn;
  };

  const factories = { user, db, assets, artifact, self: artifact, comments, downloads, permissions, files, mcp, room, sample };
  const made = new Map();
  claude.use = (name) => {
    if (made.has(name)) return made.get(name);
    const p = hello.then((i) => {
      if (!i || !i.capabilities[name] || !factories[name]) return null;
      const ns = factories[name]();
      return typeof ns === "function" ? Object.freeze(ns) : Object.freeze(ns);
    });
    if (factories[name]) made.set(name, p);
    return p;
  };
})();
