(() => {
  "use strict";

  const reduceMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  const $ = (sel) => document.querySelector(sel);
  const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  const wait = (ms) => new Promise((r) => setTimeout(r, reduceMotion ? 0 : ms));

  /* ------------------------------------------------------------------
   * Install box: tabs and copy
   * ------------------------------------------------------------------ */
  const tabs = Array.from(document.querySelectorAll(".install__tab"));
  const installText = $("#install-text");
  const panel = $("#panel-install");
  const copyBtn = $("#install-copy");
  const copyLabel = copyBtn.querySelector(".install__copy-label");
  const copyStatus = $("#copy-status");

  function selectTab(tab, focus) {
    tabs.forEach((t) => {
      const on = t === tab;
      t.setAttribute("aria-selected", String(on));
      t.tabIndex = on ? 0 : -1;
    });
    renderCmd(tab.dataset.cmd);
    panel.setAttribute("aria-labelledby", tab.id);
    resetCopy();
    if (focus) tab.focus();
  }
  tabs.forEach((tab, i) => {
    tab.addEventListener("click", () => selectTab(tab));
    tab.addEventListener("keydown", (e) => {
      if (e.key === "ArrowRight" || e.key === "ArrowLeft") {
        e.preventDefault();
        const next = tabs[(i + (e.key === "ArrowRight" ? 1 : tabs.length - 1)) % tabs.length];
        selectTab(next, true);
      }
    });
  });

  // Wrap only between words, so a narrow screen never splits sohan-bhat at its hyphen.
  function renderCmd(cmd) {
    installText.textContent = "";
    cmd.split(" ").forEach((word, i) => {
      if (i) installText.appendChild(document.createTextNode(" "));
      const span = document.createElement("span");
      span.className = word.includes("://") ? "w w--url" : "w";
      // URLs may only break right after a slash, never inside a word
      (word.includes("://") ? word.split(/(?<=[^/]\/)(?=[^/])/) : [word]).forEach((part, j) => {
        if (j) span.appendChild(document.createElement("wbr"));
        const piece = document.createElement("span");
        piece.className = "w";
        piece.textContent = part;
        span.appendChild(piece);
      });
      installText.appendChild(span);
    });
  }
  renderCmd(tabs[0].dataset.cmd);

  let copyTimer;
  function resetCopy() {
    clearTimeout(copyTimer);
    copyBtn.classList.remove("is-copied");
    copyLabel.textContent = "Copy";
  }
  async function copyText(text) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch (_) {
      const ta = document.createElement("textarea");
      ta.value = text;
      ta.setAttribute("readonly", "");
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();
      let ok = false;
      try { ok = document.execCommand("copy"); } catch (_) { ok = false; }
      ta.remove();
      return ok;
    }
  }
  copyBtn.addEventListener("click", async () => {
    const current = tabs.find((t) => t.getAttribute("aria-selected") === "true");
    const ok = await copyText(current.dataset.cmd);
    copyLabel.textContent = ok ? "Copied" : "Select it";
    copyBtn.classList.toggle("is-copied", ok);
    copyStatus.textContent = ok ? "Install command copied to clipboard" : "Copy failed. Select the command and copy it manually.";
    clearTimeout(copyTimer);
    copyTimer = setTimeout(resetCopy, 2200);
  });

  /* ------------------------------------------------------------------
   * File demo: a tiny fake project, a fake shell and a fake Oopsy.
   * Output mirrors the real CLI (cmd/oops/files.go, internal/hooks/hook.go).
   * ------------------------------------------------------------------ */
  const API_COMMITTED = `export async function getUser(id) {
  return db.users.find(id);
}`;
  const API_WORK = `export async function getUser(id) {
  return db.users.find(id);
}

// two hours of uncommitted work
export async function createInvoice(user, items) {
  const total = items.reduce((sum, i) => sum + i.price, 0);
  return billing.charge(user, total);
}`;

  const SEED = [
    { path: "src", dir: true },
    { path: "src/index.ts", content: `import { app } from "./api";\napp.listen(3000);` },
    { path: "src/api.ts", content: API_WORK, committed: API_COMMITTED },
    { path: "src/db.ts", content: `export const db = connect(process.env.DATABASE_URL);` },
    { path: "tests", dir: true },
    { path: "tests/api.test.ts", content: `test("getUser", async () => {\n  expect(await getUser(1)).toBeTruthy();\n});` },
    { path: "README.md", content: `# acme-app\n\nThe app that pays the bills.` },
    { path: "package.json", content: `{\n  "name": "acme-app",\n  "private": true\n}` },
    { path: ".env", content: `DATABASE_URL=postgres://localhost/acme\nSTRIPE_KEY=sk_live_not_in_git`, untracked: true },
  ];
  const CWD = "~/acme-app";
  const HEAD = "9c41e2b";

  const treeEl = $("#tree");
  const outEl = $("#term-out");
  const form = $("#term-form");
  const input = $("#term-input");
  const chipsEl = $("#chips");
  const sw = $("#oopsy-switch");
  const swState = sw.querySelector(".switch__state");

  let nodes, order, snapshots, oopsyOn, history, histIdx, busy, nextId, hooked;

  function makeNode(path, props) {
    const name = path.split("/").pop();
    const parent = path.includes("/") ? path.slice(0, path.lastIndexOf("/")) : "";
    return Object.assign({
      path, name, parent, dir: false, content: "", committed: "", untracked: false,
      state: "present", ghostId: null, el: null,
    }, props);
  }

  function initState() {
    nodes = new Map();
    order = [];
    SEED.forEach((s) => {
      const n = makeNode(s.path, {
        dir: !!s.dir, content: s.content || "",
        committed: s.committed !== undefined ? s.committed : (s.content || ""),
        untracked: !!s.untracked,
      });
      nodes.set(n.path, n);
      order.push(n);
    });
    snapshots = [];
    oopsyOn = true;
    hooked = true;
    history = [];
    histIdx = 0;
    busy = false;
    // Real Oopsy numbers snapshots 1, 2, 3. Start partway in, like a machine that has used it for a while.
    nextId = 42;
  }

  const ICON_DIR = '<svg class="node__icon" viewBox="0 0 16 16" aria-hidden="true"><path d="M1.5 3.5h5l1.5 2h6.5v8h-13z" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linejoin="round"/></svg>';
  const ICON_FILE = '<svg class="node__icon" viewBox="0 0 16 16" aria-hidden="true"><path d="M3.5 1.5h6l3 3v10h-9z M9.5 1.5v3h3" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linejoin="round"/></svg>';

  function nodeEl(n) {
    const li = document.createElement("li");
    li.className = "node" + (n.dir ? " node--dir" : "");
    li.innerHTML =
      `<div class="node__row">${n.dir ? ICON_DIR : ICON_FILE}<span class="node__name">${esc(n.name)}${n.dir ? "/" : ""}</span>` +
      `<span class="node__badge" hidden>M</span><span class="node__ghost-tag"></span><span class="sr-only node__sr"></span></div>`;
    n.el = li;
    if (n.dir) {
      const ul = document.createElement("ul");
      ul.setAttribute("role", "list");
      li.appendChild(ul);
    }
    return li;
  }

  function listFor(parent) {
    return parent ? nodes.get(parent).el.querySelector("ul") : treeEl;
  }

  function buildTree() {
    treeEl.textContent = "";
    order.forEach((n) => listFor(n.parent).appendChild(nodeEl(n)));
    order.forEach(paintNode);
  }

  // Oopsy never overwrites on restore: a saved copy lands next to the file as name.oops-<id>.ext
  function addSibling(orig, id, content) {
    const dot = orig.name.lastIndexOf(".");
    const base = dot > 0 ? orig.name.slice(0, dot) : orig.name;
    const ext = dot > 0 ? orig.name.slice(dot) : "";
    const path = (orig.parent ? orig.parent + "/" : "") + `${base}.oops-${id}${ext}`;
    const n = makeNode(path, { content, committed: content, untracked: true });
    nodes.set(path, n);
    order.splice(order.indexOf(orig) + 1, 0, n);
    orig.el.after(nodeEl(n));
    paintNode(n);
    return n;
  }

  function isDirty(n) {
    return !n.dir && !n.untracked && n.state === "present" && n.content !== n.committed;
  }

  function paintNode(n) {
    const li = n.el;
    li.classList.toggle("is-ghost", n.state === "ghost");
    li.classList.toggle("is-gone", n.state === "gone");
    li.querySelector(".node__badge").hidden = !isDirty(n);
    li.querySelector(".node__ghost-tag").textContent = n.ghostId ? `#${n.ghostId}` : "";
    let sr = "";
    if (n.state === "ghost") sr = `, deleted, saved in snapshot ${n.ghostId}`;
    else if (isDirty(n)) sr = ", modified";
    li.querySelector(".node__sr").textContent = sr;
  }

  /* output */
  function line(html, cls) {
    const div = document.createElement("div");
    div.className = "ln" + (cls ? " " + cls : "");
    div.innerHTML = html;
    outEl.appendChild(div);
    return div;
  }
  function text(t, cls) { return line(esc(t), cls); }
  function gap() { line("", "ln--gap"); }
  function scrollOut() { outEl.scrollTop = outEl.scrollHeight; }

  // Same wording as the CLI's ago() and ui.Bytes()
  function ago(ts) {
    const m = Math.floor((Date.now() - ts) / 60000);
    if (m < 1) return "just now";
    if (m < 60) return `${m} min ago`;
    return `${Math.floor(m / 60)} h ago`;
  }
  function bytes(n) {
    if (n < 1024) return `${n} B`;
    return `${(n / 1024).toFixed(1)} KB`;
  }
  const plural = (n, word) => `${n} ${word}${n === 1 ? "" : "s"}`;
  const pad = (s, w) => String(s).padEnd(w);
  const home = (p) => `${CWD}/${p}`;

  /* path helpers */
  function norm(p) {
    let s = p.trim().replace(/^\.\//, "").replace(/\/+$/, "");
    if (s.startsWith(CWD + "/")) s = s.slice(CWD.length + 1);
    return s;
  }
  const live = (n) => n && n.state === "present";
  const childrenOf = (dir) => order.filter((n) => n.parent === dir && live(n));
  const descendantsOf = (dir) => order.filter((n) => n.path.startsWith(dir + "/") && live(n));
  function sizeOf(n) {
    if (!n.dir) return n.content.length + 1;
    return order.filter((d) => d.path.startsWith(n.path + "/") && !d.dir).reduce((a, d) => a + d.content.length + 1, 0);
  }

  /* ---------------- commands ---------------- */
  const COMMANDS = {
    help() {
      [
        ["ls [dir]", "list files"],
        ["cat <file>", "print a file"],
        ["rm -rf <path>", "delete something, for real"],
        ["git reset --hard", "throw away uncommitted work"],
        ['echo "text" > <file>', "overwrite a file"],
        ["git status", "see what changed"],
        ["oops", "undo the last destructive command"],
        ["oops list", "show recent snapshots"],
        ["oops undo <id>", "restore a specific snapshot"],
        ["clear", "clear the screen"],
      ].forEach(([c, d]) => line(`<span class="hl">${esc(c.padEnd(22))}</span><span class="d">${esc(d)}</span>`));
    },

    clear() { outEl.textContent = ""; },

    pwd() { text("/Users/you/acme-app"); },

    whoami() { text("you. Or an AI agent. Oopsy does not mind which."); },

    ls(args) {
      const all = args.some((a) => /^-\w*a/.test(a));
      const target = args.filter((a) => !a.startsWith("-"))[0];
      let dir = "";
      if (target) {
        const t = norm(target);
        const n = nodes.get(t);
        if (t === "" || t === ".") dir = "";
        else if (!live(n)) return text(`ls: ${target}: No such file or directory`, "ln--err");
        else if (!n.dir) return text(n.path);
        else dir = t;
      }
      const kids = childrenOf(dir).filter((n) => all || !n.name.startsWith("."));
      if (!kids.length) return;
      line(kids.map((n) => n.dir ? `<span class="hl">${esc(n.name)}/</span>` : esc(n.name)).join("   "));
    },

    cat(args) {
      if (!args.length) return text("usage: cat <file>", "ln--err");
      args.forEach((a) => {
        const n = nodes.get(norm(a));
        if (!live(n)) return text(`cat: ${a}: No such file or directory`, "ln--err");
        if (n.dir) return text(`cat: ${a}: Is a directory`, "ln--err");
        text(n.content);
      });
    },

    echo(args, raw) {
      const m = raw.match(/^echo\s+(.*?)\s*(>>?)\s*(\S+)\s*$/);
      if (!m) {
        text(raw.replace(/^echo\s*/, "").replace(/^["']|["']$/g, ""));
        return;
      }
      const [, body, op, file] = m;
      const value = body.replace(/^["']|["']$/g, "");
      const n = nodes.get(norm(file));
      if (live(n) && n.dir) return text(`zsh: is a directory: ${file}`, "ln--err");
      if (!live(n)) return text(`This demo only overwrites files that exist. Try echo "${value}" > README.md`, "ln--dim");
      if (op === ">") {
        if (oopsyOn) snapshot(raw, [{ node: n, content: n.content, size: sizeOf(n) }], false);
        n.content = value;
      } else {
        n.content = n.content + "\n" + value;
      }
      flash([n]);
      paintNode(n);
    },

    rm(args, raw) {
      const flags = args.filter((a) => a.startsWith("-")).join("");
      const recursive = /r|R/.test(flags);
      const targets = args.filter((a) => !a.startsWith("-"));
      if (!targets.length) return text("usage: rm [-f | -i] [-dIPRrvWx] file ...", "ln--err");

      const hit = new Set();
      for (const t of targets) {
        if (t === "/" || t === "/*" || t === "~" || t === "~/") {
          if (oopsyOn) {
            line(`<span class="d">oops: not saved: ${esc(t.startsWith("~") ? "~" : "/")} (too broad to snapshot (it contains the whole home directory or the snapshot store))</span>`, "ln--oops");
          }
          return text("This is a browser demo, so that folder lives to see another day. Try rm -rf *", "ln--dim");
        }
        if (t === "." || t === ".." || t === "./") return text(`rm: "." and ".." may not be removed`, "ln--err");
        let matched = [];
        if (t === "*" || t === "./*") matched = childrenOf("").filter((n) => !n.name.startsWith("."));
        else if (/\/\*$/.test(t)) matched = childrenOf(norm(t.slice(0, -2))).filter((n) => !n.name.startsWith("."));
        else {
          const n = nodes.get(norm(t));
          if (live(n)) matched = [n];
        }
        if (!matched.length) { if (!/f/.test(flags)) text(`rm: ${t}: No such file or directory`, "ln--err"); continue; }
        for (const n of matched) {
          if (n.dir && !recursive) { text(`rm: ${t}: is a directory`, "ln--err"); continue; }
          hit.add(n);
          if (n.dir) descendantsOf(n.path).forEach((d) => hit.add(d));
        }
      }
      if (!hit.size) return;
      const victims = order.filter((n) => hit.has(n));
      let id = null;
      if (oopsyOn) {
        const tops = victims.filter((n) => !hit.has(nodes.get(n.parent)));
        id = snapshot(raw, tops.map((n) => ({
          node: n, size: sizeOf(n),
          members: victims.filter((v) => v === n || v.path.startsWith(n.path + "/")),
        })), false);
      }
      erase(victims, id);
    },

    git(args) {
      const sub = args.join(" ");
      if (sub === "status") {
        const modified = order.filter(isDirty);
        const deleted = order.filter((n) => !n.dir && !n.untracked && n.state !== "present");
        const untracked = order.filter((n) => n.untracked && live(n) && n.name !== ".env");
        text("On branch main");
        if (!modified.length && !deleted.length && !untracked.length) return text("nothing to commit, working tree clean");
        if (modified.length || deleted.length) {
          text("Changes not staged for commit:");
          modified.forEach((n) => line(`        <span class="y">modified:   ${esc(n.path)}</span>`));
          deleted.forEach((n) => line(`        <span class="hl">deleted:    ${esc(n.path)}</span>`));
        }
        if (untracked.length) {
          text("Untracked files:");
          untracked.forEach((n) => line(`        <span class="hl">${esc(n.path)}</span>`));
        }
        return;
      }
      if (/^reset\s+--hard/.test(sub)) {
        const changed = order.filter(isDirty);
        const missing = order.filter((n) => !n.untracked && n.state !== "present");
        if (oopsyOn && (changed.length || missing.length)) {
          snapshot("git " + sub, changed.map((n) => ({ node: n, content: n.content, size: sizeOf(n) })), true);
        }
        changed.forEach((n) => { n.content = n.committed; });
        // git itself brings back tracked files that were deleted
        missing.forEach((n) => { n.state = "present"; n.ghostId = null; });
        flash(changed);
        if (missing.length) restoreAnim(missing);
        order.forEach(paintNode);
        text(`HEAD is now at ${HEAD} Add getUser endpoint`);
        return;
      }
      if (sub === "log" || sub === "log --oneline") return text(`${HEAD} Add getUser endpoint\n1f0a7d3 Initial commit`);
      text("This demo knows git status, git reset --hard and git log.", "ln--dim");
    },

    oops(args) {
      if (!oopsyOn) {
        text("zsh: command not found: oops", "ln--err");
        if (order.some((n) => n.state === "gone")) line(`<span class="d"># Gone for good. Turn Oopsy on and press Reset to try again.</span>`);
        return;
      }
      const sub = args[0];
      if (!sub) {
        const s = latestUndoable();
        if (!s) return text("Nothing to undo. Oopsy has no unrestored snapshots.");
        return confirmAndRestore(s);
      }
      if (sub === "list" || sub === "ls" || sub === "log") return list();
      if (sub === "undo") {
        if (!args[1]) return text("oops: accepts 1 arg(s), received 0", "ln--err");
        const id = args[1].replace(/^#/, "");
        if (!/^\d+$/.test(id)) return text(`oops: "${args[1]}" is not a snapshot id (see oops list)`, "ln--err");
        const s = snapshots.find((x) => x.id === Number(id));
        if (!s) return text(`oops: no snapshot ${id}`, "ln--err");
        if (s.restored) text(`Snapshot ${s.id} was already restored ${ago(s.restored)}; restoring again keeps any existing files.`, "ln--dim");
        return confirmAndRestore(s);
      }
      if (sub === "init") {
        line(`<span class="y"><b>Setting up Oopsy</b></span>`);
        line(`  <span class="d">exists</span> ${pad("zsh", 12)} ~/.zshrc`);
        line(`  <span class="d">exists</span> ${pad("Claude Code", 12)} ~/.claude/settings.json`);
        line(`  <span class="d">exists</span> ${pad("Cursor", 12)} ~/.cursor/hooks.json`);
        gap();
        line(`Try it: <b>oops explain rm -rf build</b> then <b>oops doctor</b>`);
        return;
      }
      if (sub === "db") return text("Try the Postgres demo further down the page.", "ln--dim");
      if (sub === "help" || sub === "--help" || sub === "-h") {
        text("Undo destructive commands, for people and AI coding agents");
        gap();
        text("Usage:\n  oops [command]\n\nThis demo knows: oops, oops list, oops undo <id>, oops init");
        return;
      }
      text(`Error: unknown command "${sub}" for "oops"`, "ln--err");
    },

    sudo() { text("Nice try. Oopsy would still have your back, though.", "ln--dim"); },
  };
  COMMANDS.dir = COMMANDS.ls;

  /* What the hook prints on stderr right before the command runs:
     oops: saved snapshot 42 (4 items, 1.1 KB). Run oops to undo. */
  function snapshot(cmd, entries, gitRef) {
    const id = nextId++;
    const items = entries.reduce((a, e) => a + (e.members ? e.members.length : 1), 0);
    const size = entries.reduce((a, e) => a + e.size, 0);
    const parts = [];
    if (entries.length) parts.push(`${plural(items, "item")}, ${bytes(size)}`);
    if (gitRef) parts.push("1 git ref");
    snapshots.push({ id, cmd, entries, gitRef, items, size, at: Date.now(), restored: null });
    line(`<span class="y">oops:</span> <span class="d">saved snapshot ${id} (${esc(parts.join(" and ") || "nothing restorable")}). Run</span> <span class="y">oops</span> <span class="d">to undo.</span>`, "ln--oops");
    return id;
  }

  function latestUndoable() {
    for (let i = snapshots.length - 1; i >= 0; i--) if (!snapshots[i].restored) return snapshots[i];
    return null;
  }

  // Mirrors confirmAndRestore() in cmd/oops/files.go, including the y/N prompt.
  function confirmAndRestore(s) {
    line(`<span class="y"><b>Snapshot ${s.id}</b></span>  <span class="d">${ago(s.at)}  from zsh</span>`);
    line(`  <span class="d">$</span> ${esc(s.cmd)}`);
    line(`  <span class="d">in</span> ${CWD}`);
    gap();
    if (!s.entries.length && !s.gitRef) { text("This snapshot has nothing that can be restored."); return; }
    text("This will bring back:");
    const rows = s.entries.map((e) => {
      const p = home(e.node.path);
      const what = e.node.dir ? `folder, ${e.members.length - 1} items, ${bytes(e.size)}` : `file, ${bytes(e.size)}`;
      return { p, what, exists: live(e.node) };
    });
    if (s.gitRef) rows.push({ p: CWD, what: `move HEAD back to ${HEAD} if it was moved by the reset`, exists: false });
    const w = Math.max(...rows.map((r) => r.p.length));
    rows.forEach((r) => line(`  ${esc(pad(r.p, w))}  <span class="d">${esc(r.what)}</span>${r.exists ? '  <span class="y">(exists now, both copies are kept)</span>' : ""}`));
    gap();
    const ask = line(`Restore it? <span class="d">[y/N]</span> `);
    busy = true;
    updateChips();
    setTimeout(() => {
      ask.insertAdjacentHTML("beforeend", "y");
      restore(s);
      busy = false;
      updateChips();
      scrollOut();
      input.focus({ preventScroll: true });
    }, reduceMotion ? 0 : 420);
  }

  function restore(s) {
    const back = [];
    let same = 0;
    s.entries.forEach((e) => {
      const p = home(e.node.path);
      if (e.node.state === "ghost" && e.node.ghostId === s.id) {
        e.members.forEach((m) => { if (m.state === "ghost") { m.state = "present"; m.ghostId = null; back.push(m); } });
        line(`  <span class="g">restored</span> ${esc(p)}`);
      } else if (!live(e.node)) {
        e.members.forEach((m) => { m.state = "present"; m.ghostId = null; back.push(m); });
        line(`  <span class="g">restored</span> ${esc(p)}`);
      } else if (!e.node.dir && e.content !== undefined && e.content !== e.node.content) {
        const copy = addSibling(e.node, s.id, e.content);
        back.push(copy);
        line(`  <span class="y">kept both</span> ${esc(p)} exists, saved copy put at ${esc(home(copy.path))}`);
      } else {
        same += e.members ? e.members.filter((m) => !m.dir).length : 1;
      }
    });
    if (s.gitRef) line(`  <span class="g">git</span> HEAD was not moved, nothing to do`);
    if (same) line(`  <span class="d">${plural(same, "file")} already matched and were left alone</span>`);
    restoreAnim(back);
    order.forEach(paintNode);
    s.restored = Date.now();
    line(`<span class="g">Restored snapshot ${s.id}.</span>`);
  }

  // Mirrors `oops list`: ID, WHEN, FROM, SIZE, COMMAND
  function list() {
    if (!snapshots.length) return text("No snapshots yet.");
    line(`<span class="d">${pad("ID", 4)}  ${pad("WHEN", 11)}  ${pad("FROM", 11)}  ${pad("SIZE", 9)}  COMMAND</span>`);
    [...snapshots].reverse().forEach((s) => {
      const id = `<span class="${s.restored ? "d" : "y"}">${pad(s.id, 4)}</span>`;
      const size = !s.entries.length && s.gitRef ? "git" : bytes(s.size);
      line(`${id}  ${esc(pad(ago(s.at), 11))}  ${pad("zsh", 11)}  ${esc(pad(size, 9))}  ${esc(s.cmd)}${s.restored ? ' <span class="d">(restored)</span>' : ""}`);
    });
  }

  /* motion that means something */
  function erase(victims, id) {
    victims.forEach((n, i) => {
      const li = n.el;
      li.style.setProperty("--d", `${Math.min(i, 8) * 45}ms`);
      li.classList.remove("is-restoring");
      li.classList.add("is-erasing");
    });
    const total = reduceMotion ? 0 : 420 + Math.min(victims.length, 8) * 45;
    victims.forEach((n) => {
      n.state = id ? "ghost" : "gone";
      n.ghostId = id;
    });
    setTimeout(() => {
      victims.forEach((n) => {
        n.el.classList.remove("is-erasing");
        paintNode(n);
      });
    }, total);
  }

  function restoreAnim(list) {
    list.forEach((n, i) => {
      const li = n.el;
      li.classList.remove("is-erasing", "is-changed");
      li.style.setProperty("--d", `${Math.min(i, 8) * 45}ms`);
      void li.offsetWidth;
      li.classList.add("is-restoring");
      setTimeout(() => li.classList.remove("is-restoring"), reduceMotion ? 0 : 1100 + Math.min(i, 8) * 45);
    });
  }

  function flash(list) {
    list.forEach((n) => {
      const li = n.el;
      li.classList.remove("is-changed");
      void li.offsetWidth;
      li.classList.add("is-changed");
      setTimeout(() => li.classList.remove("is-changed"), reduceMotion ? 0 : 900);
    });
  }

  /* run */
  function tokenize(s) {
    const out = [];
    s.replace(/"([^"]*)"|'([^']*)'|(\S+)/g, (_, a, b, c) => { out.push(a ?? b ?? c); return ""; });
    return out;
  }

  function run(raw) {
    const cmd = raw.trim();
    line(`<span class="p">$</span>${esc(cmd)}`, "ln--cmd");
    if (cmd) {
      history.push(cmd);
      histIdx = history.length;
      const parts = cmd.split(/\s*(?:&&|;)\s*/).filter(Boolean);
      for (const part of parts) {
        if (busy) break;
        const [name, ...args] = tokenize(part);
        const fn = COMMANDS[name];
        if (fn) fn(args, part);
        else text(`zsh: command not found: ${name}`, "ln--err");
      }
    }
    updateChips();
    scrollOut();
  }

  /* chips */
  const CHIPS = [
    { cmd: "ls" },
    { cmd: "rm -rf src", danger: true },
    { cmd: "oops", hero: true },
    { cmd: "git reset --hard", danger: true },
    { cmd: "oops list" },
    { cmd: "cat src/api.ts" },
    { cmd: 'echo "hi" > README.md', danger: true },
    { cmd: "rm -rf *", danger: true },
    { cmd: "help" },
  ];
  const chipEls = CHIPS.map((c) => {
    const b = document.createElement("button");
    b.type = "button";
    b.className = "chip" + (c.danger ? " chip--danger" : "");
    b.textContent = c.cmd;
    b.addEventListener("click", () => typeAndRun(c.cmd));
    chipsEl.appendChild(b);
    return { ...c, el: b };
  });

  function updateChips() {
    const canUndo = oopsyOn && !!latestUndoable();
    chipEls.forEach((c) => {
      if (c.hero) {
        c.el.classList.toggle("chip--hero", canUndo);
        c.el.setAttribute("aria-label", canUndo ? "oops, undo available" : "oops");
      }
      c.el.disabled = busy;
    });
  }

  async function typeAndRun(cmd) {
    if (busy) return;
    busy = true;
    updateChips();
    input.value = "";
    if (!reduceMotion) {
      const step = Math.max(10, Math.min(28, 360 / cmd.length));
      for (const ch of cmd) { input.value += ch; await wait(step); }
      await wait(90);
    }
    input.value = "";
    busy = false;
    run(cmd);
  }

  form.addEventListener("submit", (e) => {
    e.preventDefault();
    if (busy) return;
    const v = input.value;
    input.value = "";
    run(v);
  });

  const COMPLETE = ["ls", "cat ", "rm -rf ", "git reset --hard", "git status", "echo ", "oops", "oops list", "oops undo ", "help", "clear"];
  input.addEventListener("keydown", (e) => {
    if (e.key === "ArrowUp") {
      if (!history.length) return;
      e.preventDefault();
      histIdx = Math.max(0, histIdx - 1);
      input.value = history[histIdx] || "";
      requestAnimationFrame(() => input.setSelectionRange(input.value.length, input.value.length));
    } else if (e.key === "ArrowDown") {
      if (!history.length) return;
      e.preventDefault();
      histIdx = Math.min(history.length, histIdx + 1);
      input.value = history[histIdx] || "";
    } else if (e.key === "Tab" && input.value.trim()) {
      const v = input.value;
      const words = v.split(" ");
      const last = words[words.length - 1];
      let match = null;
      if (words.length > 1 && last) {
        const base = last.includes("/") ? last.slice(0, last.lastIndexOf("/") + 1) : "";
        const pool = order.filter((n) => live(n) && (n.parent ? n.parent + "/" : "") === base).map((n) => n.path + (n.dir ? "/" : ""));
        match = pool.find((p) => p.startsWith(last));
        if (match) match = words.slice(0, -1).join(" ") + " " + match;
      } else {
        match = COMPLETE.find((c) => c.startsWith(v) && c !== v);
      }
      if (match) { e.preventDefault(); input.value = match; }
    } else if (e.key === "l" && e.ctrlKey) {
      e.preventDefault();
      COMMANDS.clear();
    }
  });

  outEl.addEventListener("click", () => {
    if (!window.getSelection().toString()) input.focus({ preventScroll: true });
  });

  sw.addEventListener("click", () => {
    oopsyOn = !oopsyOn;
    sw.setAttribute("aria-pressed", String(oopsyOn));
    swState.textContent = oopsyOn ? "on" : "off";
    line(oopsyOn
      ? `<span class="d"># Oopsy is on. Destructive commands get snapshotted first.</span>`
      : `<span class="d"># Oopsy is off. Same commands, no safety net. Try rm -rf src now.</span>`);
    updateChips();
    scrollOut();
  });

  function intro() {
    line(`<span class="d"># api.ts has uncommitted work. .env is not in git.</span>`);
    line(`<span class="d"># Destroy something, then type</span> <span class="y">oops</span><span class="d">.</span>`);
    gap();
  }

  function resetDemo() {
    initState();
    sw.setAttribute("aria-pressed", "true");
    swState.textContent = "on";
    buildTree();
    outEl.textContent = "";
    intro();
    updateChips();
  }
  $("#term-reset").addEventListener("click", resetDemo);
  resetDemo();

  /* ------------------------------------------------------------------
   * Postgres demo. The proxy logs "snapshot before DROP TABLE on public.users"
   * (internal/pgproxy/proxy.go) and oops db undo describes the change
   * the way internal/pgproxy/undo.go does.
   * ------------------------------------------------------------------ */
  const ROWS = [
    { id: 1, email: "ada@acme.dev", plan: "pro" },
    { id: 2, email: "grace@acme.dev", plan: "team" },
    { id: 3, email: "linus@acme.dev", plan: "free" },
    { id: 4, email: "margaret@acme.dev", plan: "team" },
  ];
  const dbRowsEl = $("#db-rows");
  const dbTable = $("#db-table");
  const dbEmpty = $("#db-empty");
  const dbOut = $("#db-out");
  const dbChipsEl = $("#db-chips");
  let db, dbSnaps, dbBusy, dbSeq;

  function dbLine(html, cls) {
    const d = document.createElement("div");
    d.className = "ln" + (cls ? " " + cls : "");
    d.innerHTML = html;
    dbOut.appendChild(d);
    dbOut.scrollTop = dbOut.scrollHeight;
  }

  function dbRender(anim) {
    dbRowsEl.textContent = "";
    if (db.exists) {
      db.rows.forEach((r, i) => {
        const tr = document.createElement("tr");
        tr.innerHTML = `<td>${r.id}</td><td>${esc(r.email)}</td><td>${esc(r.plan)}</td>`;
        if (anim === "restore") { tr.style.setProperty("--d", `${i * 60}ms`); tr.classList.add("is-restoring"); }
        dbRowsEl.appendChild(tr);
      });
    }
    dbTable.classList.toggle("is-dropped", !db.exists);
    const thead = dbTable.querySelector("thead");
    if (!db.exists) {
      dbEmpty.hidden = false;
      dbEmpty.textContent = 'relation "users" does not exist';
      thead.setAttribute("aria-hidden", "true");
    } else {
      dbEmpty.hidden = db.rows.length > 0;
      dbEmpty.textContent = "(0 rows)";
      thead.removeAttribute("aria-hidden");
    }
  }

  function dbEraseRows() {
    const trs = Array.from(dbRowsEl.children);
    trs.forEach((tr, i) => { tr.style.setProperty("--d", `${i * 60}ms`); tr.classList.add("is-erasing"); });
    return wait(400 + trs.length * 60);
  }

  function dbSnap(stmt, kind, describe, done) {
    dbSeq += 1;
    dbSnaps.push({ id: dbSeq, stmt, kind, describe, done, rows: db.rows.map((r) => ({ ...r })), at: Date.now(), restored: false });
    const t = new Date().toTimeString().slice(0, 8);
    dbLine(`<span class="y">oops db:</span> <span class="d">${t} snapshot before ${esc(kind)} on public.users</span>`, "ln--proxy");
  }

  const NO_TABLE = 'ERROR:  relation "users" does not exist';
  const DB_CMDS = {
    "SELECT * FROM users;": async () => {
      if (!db.exists) return dbLine(NO_TABLE, "ln--err");
      dbLine(`(${plural(db.rows.length, "row")})`);
    },
    "UPDATE users SET plan = 'free';": async (s) => {
      if (!db.exists) return dbLine(NO_TABLE, "ln--err");
      const n = db.rows.length;
      dbSnap(s, "UPDATE", `restore the old values of ${n} rows in public.users`, `${n} rows in public.users set back to their old values`);
      db.rows = db.rows.map((r) => ({ ...r, plan: "free" }));
      dbRender();
      Array.from(dbRowsEl.querySelectorAll("td:last-child")).forEach((td) => td.classList.add("is-changed"));
      dbLine(`UPDATE ${n}`);
    },
    "DELETE FROM users;": async (s) => {
      if (!db.exists) return dbLine(NO_TABLE, "ln--err");
      const n = db.rows.length;
      dbSnap(s, "DELETE", `put back ${n} rows into public.users`, `${n} rows put back into public.users`);
      await dbEraseRows();
      db.rows = [];
      dbRender();
      dbLine(`DELETE ${n}`);
    },
    "DROP TABLE users;": async (s) => {
      if (!db.exists) return dbLine('ERROR:  table "users" does not exist', "ln--err");
      dbSnap(s, "DROP TABLE", `recreate table public.users with ${db.rows.length} rows, its indexes and constraints`, `table public.users recreated with ${db.rows.length} rows`);
      await dbEraseRows();
      db.exists = false;
      db.rows = [];
      dbRender();
      dbLine("DROP TABLE");
    },
    "oops db undo": async () => {
      let s = null;
      for (let i = dbSnaps.length - 1; i >= 0; i--) if (!dbSnaps[i].restored) { s = dbSnaps[i]; break; }
      if (!s) return dbLine("Nothing to undo.");
      s.restored = true;
      dbLine(`<span class="y"><b>Database change ${s.id}</b></span>  <span class="d">${ago(s.at)}</span>`);
      dbLine(`  <span class="d">&gt;</span> ${esc(s.stmt)}`);
      dbLine("", "ln--gap");
      dbLine("This will:");
      dbLine(`  ${esc(s.describe)}`);
      dbLine("", "ln--gap");
      dbLine(`Restore it? <span class="d">[y/N]</span> y`);
      await wait(380);
      db.exists = true;
      db.rows = s.rows.map((r) => ({ ...r }));
      dbRender("restore");
      dbLine(`  <span class="g">done</span> ${esc(s.done)}`);
      dbLine(`<span class="g">Restored database change ${s.id}.</span>`);
    },
  };

  const DB_CHIPS = [
    { cmd: "SELECT * FROM users;" },
    { cmd: "UPDATE users SET plan = 'free';", danger: true },
    { cmd: "DELETE FROM users;", danger: true },
    { cmd: "DROP TABLE users;", danger: true },
    { cmd: "oops db undo", hero: true },
  ];
  const dbChipEls = DB_CHIPS.map((c) => {
    const b = document.createElement("button");
    b.type = "button";
    b.className = "chip" + (c.danger ? " chip--danger" : "");
    b.textContent = c.cmd;
    b.addEventListener("click", () => dbRun(c.cmd));
    dbChipsEl.appendChild(b);
    return { ...c, el: b };
  });

  function dbUpdateChips() {
    const canUndo = dbSnaps.some((s) => !s.restored);
    dbChipEls.forEach((c) => {
      c.el.disabled = dbBusy;
      if (c.hero) c.el.classList.toggle("chip--hero", canUndo);
    });
  }

  async function dbRun(cmd) {
    if (dbBusy) return;
    dbBusy = true;
    dbUpdateChips();
    const shell = cmd.startsWith("oops");
    dbLine(`<span class="p">${shell ? "$" : "acme=&gt;"}</span>${esc(cmd)}`, "ln--sql");
    await DB_CMDS[cmd](cmd);
    dbBusy = false;
    dbUpdateChips();
  }

  function dbReset() {
    db = { exists: true, rows: ROWS.map((r) => ({ ...r })) };
    dbSnaps = [];
    dbBusy = false;
    dbSeq = 0;
    dbOut.textContent = "";
    dbLine(`<span class="d"># psql talks to Postgres through oops db proxy.</span>`);
    dbLine(`<span class="d"># Run something you will regret.</span>`);
    dbRender();
    dbUpdateChips();
  }
  $("#db-reset").addEventListener("click", dbReset);
  dbReset();
})();
