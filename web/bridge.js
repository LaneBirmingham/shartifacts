// Answers window.storage, window.fs and window.claude calls from artifact preview frames
// (see previewRuntime in internal/server/runtime.go). frameFor(window) returns what a
// message's source window may do: {id, thumbnail} for a frame this page shows, or nothing
// to ignore it. id is the artifact whose storage the frame may use. Thumbnails read
// storage and files; their storage writes appear to succeed but are not saved, and they
// cannot call Claude or report errors.
// Where a frame's artifact API lives: the workspace's, or a share link's read-only one.
function artifactAPI(frame) {
  return frame.base || `/api/artifacts/${encodeURIComponent(frame.id)}`;
}
function bridgeArtifactFrames(frameFor) {
  const post = async (path, body) => {
    const r = await fetch(path, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    const data = await r.json().catch(() => null);
    if (!r.ok) throw new Error(data?.error || `request failed (${r.status})`);
    return data;
  };
  // A Media library file by URL, ID or upload name; the latest upload wins a name.
  const readFile = async ({ path, encoding }) => {
    const r = await fetch("/api/media");
    if (!r.ok) throw new Error(`media library unavailable (${r.status})`);
    const { media } = await r.json();
    const base = path.split("/").pop();
    const item =
      media.find((m) => m.url === path || m.id === base) ||
      media
        .filter((m) => m.name === base)
        .sort((a, b) => b.uploaded_at.localeCompare(a.uploaded_at))[0];
    if (!item) throw new Error(`${path} is not in the Media library; upload it there first`);
    const bytes = new Uint8Array(await (await fetch(item.url)).arrayBuffer());
    return /^utf-?8$/i.test(encoding || "") ? new TextDecoder().decode(bytes) : bytes;
  };
  window.addEventListener("message", async (event) => {
    const d = event.data,
      frame = frameFor(event.source);
    if (!frame || typeof d?.type !== "string") return;
    if (d.type === "shartifacts-error") {
      // Only saved versions are recorded; draft previews are still being edited.
      const saved = frame.src?.match(/^\/preview\/([^/?]+)(?:\?version=|\/)(\d+)/);
      if (!frame.thumbnail && saved && typeof d.message === "string")
        post(`/api/artifacts/${saved[1]}/errors`, {
          version: Number(saved[2]),
          message: d.message,
        }).catch(() => {});
      return;
    }
    if (!["shartifacts-storage", "shartifacts-file", "shartifacts-complete"].includes(d.type))
      return;
    const reply = (message) =>
      event.source.postMessage({ type: "shartifacts-result", id: d.id, ...message }, "*");
    try {
      if (d.type === "shartifacts-complete") {
        if (frame.thumbnail) return reply({ error: "window.claude is off in thumbnails" });
        const { text } = await post("/api/complete", {
          prompt: d.prompt,
          messages: d.messages,
        });
        return reply({ result: text });
      }
      if (d.type === "shartifacts-file") return reply({ result: await readFile(d) });
      if (!frame.id) return reply({ error: "window.storage is unavailable here" });
      if (frame.share && (d.op === "set" || d.op === "delete")) return reply({ error: "this shared view is read-only" });
      if (frame.thumbnail && d.op === "set")
        return reply({ result: { key: d.key, value: d.value, shared: !!d.shared } });
      if (frame.thumbnail && d.op === "delete")
        return reply({ result: { key: d.key, deleted: false, shared: !!d.shared } });
      reply({
        result: await post(`${artifactAPI(frame)}/storage`, {
          op: d.op,
          key: d.key,
          value: d.value,
          shared: d.shared,
          prefix: d.prefix,
        }),
      });
    } catch (e) {
      reply({ error: e.message });
    }
  });
}

// Slide decks take arrow keys, but a click elsewhere leaves focus in this page, outside
// the deck's frame. Forward navigation keys to it while nothing here wants them.
function forwardDeckKeys(frame, isDeck) {
  const keys = ["ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown", "PageUp", "PageDown", "Home", "End"];
  document.addEventListener("keydown", (e) => {
    const target = document.activeElement;
    if (e.defaultPrevented || e.metaKey || e.ctrlKey || e.altKey || !keys.includes(e.key) || !isDeck()) return;
    if (target?.closest("input, textarea, select, [contenteditable], dialog")) return;
    frame()?.contentWindow?.postMessage({ type: "shartifacts-key", key: e.key }, "*");
    e.preventDefault();
  });
}

// ---- The capability runtime's host (web/runtime.js). ----------------------------------
// Each preview frame asks through `shartifacts-call` (one answer) and
// `shartifacts-subscribe` (a stream of events until unsubscribed). The person viewing is
// whoever this browser says it is (see viewerId); requests carry it to the server.
function viewerId() {
  try {
    return localStorage.getItem("shartifacts.viewer") || "";
  } catch {
    return "";
  }
}
function hostArtifactFrames(frameFor, hooks = {}) {
  const fail = (code, message, extra) => Object.assign({ code, message }, extra);
  const request = async (path, options = {}) => {
    const headers = { "X-Shartifacts-Viewer": viewerId(), ...options.headers };
    if (options.json !== undefined) {
      headers["Content-Type"] = "application/json";
      options = { ...options, body: JSON.stringify(options.json) };
    }
    const r = await fetch(path, { ...options, headers });
    const body = await r.json().catch(() => null);
    if (!r.ok) throw fail(body?.code || (r.status === 404 ? "not_found" : "upstream_error"), body?.error || body?.message || `request failed (${r.status})`, body?.live ? { live: body.live, paths: body.paths, changed: body.changed } : {});
    return body;
  };
  const states = new Set();
  const stateFor = (win, frame) => {
    for (const st of states) if (st.win === win) return st;
    const st = { win, peer: Array.from(crypto.getRandomValues(new Uint8Array(12)), (b) => "abcdefghijklmnopqrstuvwxyz0123456789"[b % 36]).join(""), watches: new Map(), roomSub: null, rooms: new Map(), aborts: new Map(), es: null };
    states.add(st);
    return st;
  };
  // A replaced frame's stream and subscriptions end with it.
  setInterval(() => {
    for (const st of states)
      if (!frameFor(st.win)) {
        st.es?.close();
        st.aborts.forEach((a) => a.abort());
        states.delete(st);
      }
  }, 5000);
  const versionOf = (frame) => Number((frame.src || "").match(/^\/preview\/[^/?]+(?:\?version=|\/)(\d+)/)?.[1] || 0);
  const live = (st, frame) => {
    if (st.es) return;
    st.es = new EventSource(`${artifactAPI(frame)}/live?peer=${st.peer}&viewer=${encodeURIComponent(viewerId())}`);
    st.es.onopen = async () => {
      // A reconnect re-joins the rooms this frame is in and re-asserts its presence.
      for (const [room, presence] of st.rooms) {
        await request(`/api/artifacts/${frame.id}/room`, { method: "POST", json: { peer: st.peer, room, op: "join" } }).catch(() => {});
        if (Object.keys(presence).length) request(`/api/artifacts/${frame.id}/room`, { method: "POST", json: { peer: st.peer, room, op: "presence", patch: presence } }).catch(() => {});
      }
      st.roomSub?.({ kind: "connection", connected: true });
      st.watches.forEach((w) => w.refresh());
    };
    st.es.onerror = () => st.roomSub?.({ kind: "connection", connected: false });
    st.es.onmessage = (m) => {
      let ev;
      try {
        ev = JSON.parse(m.data);
      } catch {
        return;
      }
      if (ev.type === "db") st.watches.forEach((w) => w.changed(ev.detail));
      else if (ev.type === "deleted") st.watches.forEach((w) => w.fail(fail("revoked", "the artifact was deleted")));
      else if (ev.type === "room") st.roomSub?.(ev);
    };
  };
  const send = (win, message) => {
    try {
      win.postMessage(message, "*");
    } catch {}
  };
  const artifactsDataTools = [
    { name: "get", description: "Read one document: {path}.", inputSchema: { type: "object", properties: { path: { type: "string" } }, required: ["path"] } },
    { name: "list", description: "Read a collection's documents: {collection}.", inputSchema: { type: "object", properties: { collection: { type: "string" } }, required: ["collection"] } },
    { name: "query", description: "Query a collection: {collection, where?: [[field, op, value]], order_by?, dir?, limit?}.", inputSchema: { type: "object", properties: { collection: { type: "string" } }, required: ["collection"] } },
    { name: "set", description: "Write a whole document: {path, data}.", inputSchema: { type: "object", properties: { path: { type: "string" }, data: { type: "object" } }, required: ["path", "data"] } },
    { name: "update", description: "Merge into an existing document: {path, data}.", inputSchema: { type: "object", properties: { path: { type: "string" }, data: { type: "object" } }, required: ["path", "data"] } },
    { name: "delete", description: "Delete one document: {path}.", inputSchema: { type: "object", properties: { path: { type: "string" } }, required: ["path"] } },
  ];
  const mediaFiles = async () => {
    const { media } = await request("/api/media");
    const byPath = new Map();
    for (const m of [...media].sort((a, b) => a.uploaded_at.localeCompare(b.uploaded_at))) byPath.set(m.name || m.id, m);
    return byPath;
  };
  const confirmSave = (name, size) => confirm(`Save “${name}” (${size >= 1048576 ? (size / 1048576).toFixed(1) + " MB" : Math.max(1, Math.round(size / 1024)) + " KB"}) from this artifact?`);
  const saveBlob = (blob, name) => {
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = name;
    document.body.append(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(a.href), 60000);
  };
  let limits;
  const sampleLimits = () => (limits ??= request("/api/sample/limits").catch(() => ({ available: false })));

  const calls = {
    runtime: {
      hello: async (frame, st) => {
        const me = await request(`${artifactAPI(frame)}/me`);
        const full = !frame.thumbnail;
        // Thumbnails and share links never sample, so they need not ask (a share host has no /api).
        const sample = full && (await sampleLimits()).available;
        return {
          peer: st.peer,
          thumbnail: !!frame.thumbnail,
          me,
          capabilities: { user: true, db: true, permissions: true, artifact: true, self: true, assets: full, comments: full, downloads: full || !!frame.share, files: full, mcp: full, room: full, sample },
        };
      },
    },
    user: {
      people: async () => Object.fromEntries((await request("/api/people")).people.map((p) => [p.id, { name: p.name, color: p.color }])),
    },
    db: {
      get: (frame, st, a) => request(`${artifactAPI(frame)}/db`, { method: "POST", json: { op: "get", path: a.path } }),
      query: (frame, st, a) => request(`${artifactAPI(frame)}/db`, { method: "POST", json: { op: "query", path: a.path, query: a.query } }),
      write: (frame, st, a) => {
        // A share visitor holds `view`: never writes. A thumbnail's writes are not saved.
        if (frame.share) throw fail("invalid_argument", "this shared view is read-only");
        return frame.thumbnail ? { ok: true } : request(`${artifactAPI(frame)}/db`, { method: "POST", json: { op: a.op, path: a.path, data: a.data } });
      },
      acquire: (frame, st, a) =>
        frame.thumbnail ? { acquired: false } : request(`/api/artifacts/${frame.id}/db`, { method: "POST", json: { op: "acquire", path: a.path, holder: a.holder, ttlMs: a.ttlMs, data: a.data } }),
    },
    assets: {
      upload: async (frame, st, a) => {
        try {
          return await request(`/api/artifacts/${frame.id}/assets`, { method: "POST", body: a.blob, headers: { "Content-Type": a.type } });
        } catch (e) {
          throw fail({ invalid_argument: "invalid_request", quota_exceeded: "quota_or_state" }[e.code] || e.code, e.message);
        }
      },
      list: async (frame) => {
        const r = await request(`/api/artifacts/${frame.id}/assets`);
        return { assets: r.assets.map(({ uploader, ...a }) => ({ ...a, createdAt: a.createdAt })), usage: r.usage };
      },
      delete: (frame, st, a) => request(`/api/artifacts/${frame.id}/assets/${encodeURIComponent(a.id)}`, { method: "DELETE" }),
    },
    artifact: {
      publish: async (frame, st, a) => {
        if (frame.thumbnail) throw fail("not_granted", "thumbnails cannot publish");
        // A frame that published files keeps running on the version it made.
        let base = st.base || versionOf(frame);
        if (!base) base = (await request(`/api/artifacts/${frame.id}`)).revisions.length;
        try {
          const r = await request(`/api/artifacts/${frame.id}/publish`, { method: "POST", json: { base_version: base, ...a } });
          if (a.files) st.base = Number(r.version);
          hooks.published?.(frame.id, Number(r.version), !!a.files);
          return r;
        } catch (e) {
          if (e.code === "conflict" && !e.paths) hooks.published?.(frame.id, Number(e.live), false);
          throw e;
        }
      },
      metadata: async (frame, st, a) => {
        // An archived artifact is still a real one its links open.
        const all = await request("/api/artifacts?archived=include");
        return Object.fromEntries(
          a.refs.map((ref) => {
            const id = String(ref).match(/(?:\/(?:a|artifact|view)\/)?([0-9a-f]{24})(?:[/?#]|$)/i)?.[1]?.toLowerCase() || "";
            const hit = all.find((x) => x.id === id);
            return [ref, { id: hit ? id : "", title: hit?.title || "", type: hit?.kind || null }];
          }),
        );
      },
    },
    comments: {
      openComposer: (frame, st, a) => {
        if (!hooks.compose) throw fail("unavailable", "open the artifact in the workspace to comment");
        return { opened: hooks.compose(a, frame) !== false };
      },
      create: async (frame, st, a) => {
        const author = (await calls.user.people())[viewerId()]?.name || "you";
        const c = await request(`/api/artifacts/${frame.id}/comments`, { method: "POST", json: { body: a.text, anchor: a.threadId ? undefined : a.anchor, parent: a.threadId, author, to_claude: !!a.toClaude } });
        hooks.commented?.();
        return { threadId: c.parent || c.id, commentId: c.id };
      },
      reply: async (frame, st, a) => {
        const author = (await calls.user.people())[viewerId()]?.name || "you";
        const c = await request(`/api/artifacts/${frame.id}/comments`, { method: "POST", json: { body: a.text, parent: a.threadId, author } }).catch((e) => {
          throw e.message.includes("parent") ? fail("not_found", "no such thread") : e;
        });
        hooks.commented?.();
        return { commentId: c.id };
      },
      resolve: async (frame, st, a) => {
        await request(`/api/artifacts/${frame.id}/comments/${encodeURIComponent(a.threadId)}`, { method: "PUT", json: { resolved: a.resolved } }).catch(() => {
          throw fail("not_found", "no such thread");
        });
        hooks.commented?.();
      },
      delete: async (frame, st, a) => {
        const r = await request(`/api/artifacts/${frame.id}/comments/${encodeURIComponent(a.threadId)}`, { method: "DELETE" });
        if (!r.deleted) throw fail("not_found", "no such thread");
        hooks.commented?.();
      },
    },
    downloads: {
      save: (frame, st, a) => {
        if (!confirmSave(a.filename, a.data.size)) throw fail("declined", "the viewer chose not to save it");
        saveBlob(a.data, a.filename);
        return { status: "saved" };
      },
    },
    files: {
      list: async (frame, st, a) => {
        if (a.path) throw fail("not_found", "the Media library has no folders");
        const entries = [...(await mediaFiles()).entries()].map(([path, m]) => ({ path, isDirectory: false, size: m.size, createdAt: m.uploaded_at }));
        return { entries };
      },
      read: async (frame, st, a) => {
        const m = (await mediaFiles()).get(a.path);
        if (!m) throw fail("not_found", `${a.path} is not in the Media library`);
        const buf = await (await fetch(m.url)).arrayBuffer();
        return { bytes: buf.slice(a.offset, a.offset + Math.min(a.length, 26214400)), size: buf.byteLength, revision: m.id };
      },
      download: async (frame, st, a) => {
        const m = (await mediaFiles()).get(a.path);
        if (!m) throw fail("not_found", `${a.path} is not in the Media library`);
        if (!confirmSave(a.path, m.size)) throw fail("declined", "the viewer chose not to save it");
        saveBlob(await (await fetch(m.url)).blob(), a.path.split("/").pop());
        return { status: "saved" };
      },
    },
    mcp: {
      listTools: async () => {
        const { servers } = await request("/api/connectors");
        return { servers: [{ server: "artifacts_data", kind: "artifact", authStatus: "connected", tools: artifactsDataTools.map(({ inputSchema, ...t }) => t) }, ...servers] };
      },
      describeTool: async (frame, st, a) => {
        if (a.server === "artifacts_data") {
          const t = artifactsDataTools.find((x) => x.name === a.tool);
          if (!t) throw fail("bad_request", "artifacts_data has no tool " + a.tool);
          return t;
        }
        return request("/api/connectors/describe", { method: "POST", json: { server: a.server, tool: a.tool } });
      },
      callTool: async (frame, st, a) => {
        if (a.server !== "artifacts_data") return request("/api/connectors/call", { method: "POST", json: { server: a.server, tool: a.tool, input: a.input } });
        const db = (json) => request(`${artifactAPI(frame)}/db`, { method: "POST", json });
        const i = a.input || {};
        let out;
        try {
          out = {
            get: () => db({ op: "get", path: i.path }),
            list: () => db({ op: "query", path: i.collection }),
            query: () => db({ op: "query", path: i.collection, query: { where: (i.where || []).map(([field, op, value]) => ({ field, op, value })), order_by: i.order_by, dir: i.dir, limit: i.limit } }),
            set: () => (frame.thumbnail ? { ok: true } : db({ op: "set", path: i.path, data: i.data })),
            update: () => (frame.thumbnail ? { ok: true } : db({ op: "update", path: i.path, data: i.data })),
            delete: () => (frame.thumbnail ? { ok: true } : db({ op: "delete", path: i.path })),
          }[a.tool];
          if (!out) throw fail("bad_request", "artifacts_data has no tool " + a.tool);
          out = await out();
        } catch (e) {
          throw e.code === "bad_request" ? e : fail("tool_error", JSON.stringify({ error: { code: e.code, message: e.message } }), { server: "artifacts_data" });
        }
        return { content: [{ type: "text", text: JSON.stringify(out) }], structuredContent: out };
      },
    },
    room: {
      send: async (frame, st, a) => {
        if (a.op === "join") st.rooms.set(a.room, {});
        if (a.op === "leave") st.rooms.delete(a.room);
        if (a.op === "presence") st.rooms.set(a.room, { ...st.rooms.get(a.room), ...a.patch });
        return request(`/api/artifacts/${frame.id}/room`, { method: "POST", json: { peer: st.peer, ...a } });
      },
      sendToClaude: async (frame, st, a) => {
        const r = await calls.comments.create(frame, st, {
          text: `${a.data.label || "Sent from the page"}\n\n${JSON.stringify(a.data, null, 2).slice(0, 3500)}${a.presence && Object.keys(a.presence).length ? "\n\nPresence: " + JSON.stringify(a.presence).slice(0, 400) : ""}`,
          toClaude: true,
        });
        return { to: "session", id: r.threadId };
      },
    },
    sample: {
      limits: async () => {
        const l = await sampleLimits();
        if (!l.available) throw fail("sampling_disabled", "Claude is off on this server");
        return { maxPromptBytes: l.maxPromptBytes, images: l.images, tools: l.tools };
      },
    },
  };

  const subscriptions = {
    db: {
      watch: (frame, st, a, emit) => {
        let last = "";
        const collection = a.query !== undefined;
        const fetchNow = async () => {
          try {
            const r = collection ? await calls.db.query(frame, st, a) : await calls.db.get(frame, st, a);
            const sig = JSON.stringify(collection ? r.docs.map((d) => [d.id, d.version]) : [r.exists, r.version]);
            if (sig !== last) {
              last = sig;
              emit(collection ? { docs: r.docs } : { doc: r });
            }
          } catch (e) {
            if (e.code === "invalid_argument" || e.code === "not_found") w.fail(e.code === "not_found" ? fail("revoked", "the artifact is gone") : e);
          }
        };
        const w = {
          refresh: fetchNow,
          changed: (path) => {
            if (collection ? path.slice(0, path.lastIndexOf("/")) === a.path : path === a.path) fetchNow();
          },
          fail: (e) => {
            emit({ error: e });
            st.watches.delete(w);
          },
        };
        st.watches.set(w, w);
        live(st, frame);
        fetchNow();
        return () => st.watches.delete(w);
      },
    },
    room: {
      events: (frame, st, a, emit) => {
        st.roomSub = emit;
        st.rooms.set("lobby", st.rooms.get("lobby") || {});
        if (st.es?.readyState === 1) {
          request(`/api/artifacts/${frame.id}/room`, { method: "POST", json: { peer: st.peer, room: "lobby", op: "join" } }).catch(() => {});
          emit({ kind: "connection", connected: true });
        }
        live(st, frame);
        return () => (st.roomSub = null);
      },
    },
    sample: {
      round: (frame, st, a, emit) => {
        const ctl = new AbortController();
        (async () => {
          try {
            const r = await fetch("/api/sample", { method: "POST", signal: ctl.signal, headers: { "Content-Type": "application/json", "X-Shartifacts-Viewer": viewerId() }, body: JSON.stringify(a) });
            if (!a.stream || !(r.headers.get("Content-Type") || "").startsWith("text/event-stream")) {
              const body = await r.json().catch(() => null);
              return emit(r.ok ? body : { type: "error", code: body?.code || "upstream_error", message: body?.error || "Claude is unavailable" });
            }
            const reader = r.body.getReader(),
              decoder = new TextDecoder();
            let buf = "";
            for (;;) {
              const { value, done } = await reader.read();
              if (done) break;
              buf += decoder.decode(value, { stream: true });
              let i;
              while ((i = buf.indexOf("\n\n")) >= 0) {
                const line = buf.slice(0, i);
                buf = buf.slice(i + 2);
                if (line.startsWith("data: ")) emit(JSON.parse(line.slice(6)));
              }
            }
          } catch (e) {
            if (!ctl.signal.aborted) emit({ type: "error", code: "upstream_error", message: e.message });
          }
        })();
        return () => ctl.abort();
      },
    },
  };

  window.addEventListener("message", async (event) => {
    const d = event.data,
      frame = frameFor(event.source);
    if (!frame || !d || typeof d.type !== "string" || !d.type.startsWith("shartifacts-")) return;
    const st = stateFor(event.source, frame);
    if (d.type === "shartifacts-call") {
      const f = calls[d.cap]?.[d.method];
      try {
        if (!f) throw fail("capability_removed", `${d.cap}.${d.method} is not available`);
        const result = await f(frame, st, d.args || {});
        send(event.source, { type: "shartifacts-reply", id: d.id, result });
      } catch (e) {
        send(event.source, { type: "shartifacts-reply", id: d.id, error: e && e.code ? { ...e } : fail("upstream_error", String(e?.message || e)) });
      }
    } else if (d.type === "shartifacts-subscribe") {
      const f = subscriptions[d.cap]?.[d.method];
      const emit = (ev) => send(event.source, { type: "shartifacts-event", sub: d.sub, event: ev });
      if (!f) return emit({ error: fail("capability_removed", `${d.cap}.${d.method} is not available`), type: "error", code: "capability_removed" });
      st.aborts.set(d.sub, { abort: f(frame, st, d.args || {}, emit) });
    } else if (d.type === "shartifacts-unsubscribe") {
      st.aborts.get(d.sub)?.abort();
      st.aborts.delete(d.sub);
    }
  });
}
