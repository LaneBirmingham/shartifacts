// "Connect an agent", shared by the apps hub and the artifacts workspace: this server's MCP
// address and the commands that connect common agents to it. Any [data-connect] element
// opens it. Also window.shartifactsCopy, a clipboard copy that works over plain-HTTP LAN
// addresses too, where browsers withhold navigator.clipboard.
(() => {
  async function copy(text) {
    try {
      if (navigator.clipboard && window.isSecureContext) {
        await navigator.clipboard.writeText(text);
        return true;
      }
    } catch {}
    const t = document.createElement("textarea");
    t.value = text;
    t.setAttribute("readonly", "");
    t.style.cssText = "position:fixed;top:0;left:0;opacity:0";
    (document.querySelector("dialog[open]") || document.body).append(t);
    t.select();
    let ok = false;
    try {
      ok = document.execCommand("copy");
    } catch {}
    t.remove();
    return ok;
  }
  window.shartifactsCopy = copy;

  const url = `${location.origin}/mcp`;
  const local = ["localhost", "127.0.0.1", "[::1]"].includes(location.hostname);
  const clients = [
    ["Claude Code", `claude mcp add --transport http --scope user shartifacts ${url}`],
    ["Codex", `codex mcp add shartifacts --url ${url}`],
    ["Cursor, Claude Code's .mcp.json and other mcpServers files", JSON.stringify({ mcpServers: { shartifacts: { type: "http", url } } }, null, 2)],
  ];

  const el = (tag, cls, text) => {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text != null) n.textContent = text;
    return n;
  };
  const copyButton = (text, label = "Copy") => {
    const b = el("button", "quiet", label);
    b.type = "button";
    b.onclick = async () => {
      b.textContent = (await copy(text)) ? "Copied" : "Select and copy";
      setTimeout(() => (b.textContent = label), 1600);
    };
    return b;
  };

  const dialog = el("dialog", "connect-dialog");
  dialog.id = "connect-dialog";
  dialog.setAttribute("aria-labelledby", "connect-title");
  const head = el("div", "dialog-heading");
  const close = el("button", "", "×");
  close.type = "button";
  close.dataset.close = "";
  close.setAttribute("aria-label", "Close");
  close.onclick = () => dialog.close();
  head.append(el("span", "eyebrow", "CONNECT AN AGENT"), close);
  const title = el("h2", "", "Give your agent a place to make things.");
  title.id = "connect-title";
  const intro = el("p", "", "Agents connect over MCP. Once connected, yours can make artifacts and apps here, change them when you ask, and hand you links to open on any of your devices.");

  const endpoint = el("label", "", "MCP endpoint (Streamable HTTP)");
  const row = el("div", "copy-row");
  const input = el("input");
  input.readOnly = true;
  input.value = url;
  row.append(input, copyButton(url));
  endpoint.append(row);

  const list = el("div", "connect-clients");
  for (const [name, snippet] of clients) {
    const item = el("div", "connect-client");
    const top = el("div", "connect-client-head");
    top.append(el("strong", "", name), copyButton(snippet));
    item.append(top, el("pre", "", snippet));
    list.append(item);
  }

  const next = el("p", "connect-next");
  next.append("Then ask it something like ", el("em", "", "“make me a packing checklist in shartifacts”"), " or ", el("em", "", "“build a small expenses app in shartifacts”"), ".");

  const note = el("p", "fineprint");
  note.append(
    local
      ? "This address works only on this computer. For agents on other devices, open shartifacts by this computer's LAN or Tailscale address (with Docker, set SHARTIFACTS_BIND in .env first) and connect them to that. "
      : "Anyone who can reach this address can read and change everything here and deploy apps, so keep it on your own network or tailnet. ",
  );
  const more = el("a", "", "Other clients");
  more.href = "https://github.com/LaneBirmingham/shartifacts/blob/main/docs/agents.md";
  more.target = "_blank";
  more.rel = "noopener";
  note.append(more, ".");

  dialog.append(head, title, intro, endpoint, list, next, note);
  document.body.append(dialog);
  dialog.addEventListener("click", (e) => {
    if (e.target === dialog) dialog.close();
  });
  document.addEventListener("click", (e) => {
    if (e.target.closest?.("[data-connect]")) dialog.showModal();
  });
})();
