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
    installText.textContent = tab.dataset.cmd;
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
    const ok = await copyText(installText.textContent);
    copyLabel.textContent = ok ? "Copied" : "Select it";
    copyBtn.classList.toggle("is-copied", ok);
    copyStatus.textContent = ok ? "Install command copied to clipboard" : "Copy failed. Select the command and copy it manually.";
    clearTimeout(copyTimer);
    copyTimer = setTimeout(resetCopy, 2200);
  });

  /* ------------------------------------------------------------------
   * File demo: a tiny fake project, a fake shell and a fake Oopsy
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

  const treeEl = $("#tree");
  const outEl = $("#term-out");
  const form = $("#term-form");
  const input = $("#term-input");
  const chipsEl = $("#chips");
  const sw = $("#oopsy-switch");
  const swState = sw.querySelector(".switch__state");

  let nodes, order, snapshots, oopsyOn, history, histIdx, busy, startedAt, hexSeed;

  function newId() {
    hexSeed = (hexSeed * 1103515245 + 12345) & 0x7fffffff;
    return (hexSeed % 0xffff).toString(16).padStart(4, "0");
  }

  function initState() {
    nodes = new Map();
    order = [];
    SEED.forEach((s) => {
      const name = s.path.split("/").pop();
      const parent = s.path.includes("/") ? s.path.slice(0, s.path.lastIndexOf("/")) : "";
      const n = {
        path: s.path, name, parent, dir: !!s.dir,
        content: s.content || "", committed: s.committed !== undefined ? s.committed : (s.content || ""),
        untracked: !!s.untracked, state: "present", ghostId: null, el: null,
      };
      nodes.set(s.path, n);
      order.push(n);
    });
    snapshots = [];
    oopsyOn = true;
    history = [];
    histIdx = 0;
    busy = false;
    startedAt = Date.now();
    hexSeed = 20250718;
  }

  const ICON_DIR = '<svg class="node__icon" viewBox="0 0 16 16" aria-hidden="true"><path d="M1.5 3.5h5l1.5 2h6.5v8h-13z" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linejoin="round"/></svg>';
  const ICON_FILE = '<svg class="node__icon" viewBox="0 0 16 16" aria-hidden="true"><path d="M3.5 1.5h6l3 3v10h-9z M9.5 1.5v3h3" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linejoin="round"/></svg>';

  function buildTree() {
    treeEl.textContent = "";
    const lists = new Map([["", treeEl]]);
    order.forEach((n) => {
      const li = document.createElement("li");
      li.className = "node" + (n.dir ? " node--dir" : "");
      li.innerHTML =
        `<div class="node__row">${n.dir ? ICON_DIR : ICON_FILE}<span class="node__name">${esc(n.name)}${n.dir ? "/" : ""}</span>` +
        `<span class="node__badge" hidden>M</span><span class="node__ghost-tag"></span><span class="sr-only node__sr"></span></div>`;
      n.el = li;
      lists.get(n.parent).appendChild(li);
      if (n.dir) {
        const ul = document.createElement("ul");
        ul.setAttribute("role", "list");
        li.appendChild(ul);
        lists.set(n.path, ul);
      }
    });
    order.forEach(paintNode);
  }

  function isDirty(n) {
    return !n.dir && !n.untracked && n.state === "present" && n.content !== n.committed;
  }

  function paintNode(n) {
    const li = n.el;
    li.classList.toggle("is-ghost", n.state === "ghost");
    li.classList.toggle("is-gone", n.state === "gone");
    const badge = li.querySelector(".node__badge");
    badge.hidden = !isDirty(n);
    li.querySelector(".node__ghost-tag").textContent = n.ghostId ? `in ${n.ghostId}` : "";
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

  function ago(ts) {
    const s = Math.max(1, Math.round((Date.now() - ts) / 1000));
    if (s < 60) return `${s}s ago`;
    const m = Math.round(s / 60);
    return `${m}m ago`;
  }
  const plural = (n, word) => `${n} ${word}${n === 1 ? "" : "s"}`;

  /* path helpers */
  function norm(p) {
    let s = p.trim().replace(/^\.\//, "").replace(/\/+$/, "");
    if (s.startsWith("~/acme-app/")) s = s.slice(11);
    return s;
  }
  const live = (n) => n && n.state === "present";
  const childrenOf = (dir) => order.filter((n) => n.parent === dir && live(n));
  const descendantsOf = (dir) => order.filter((n) => n.path.startsWith(dir + "/") && live(n));

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
        ["oops list", "show snapshots"],
        ["oops undo <id>", "restore a specific snapshot"],
        ["clear", "clear the screen"],
      ].forEach(([c, d]) => line(`<span class="hl">${esc(c.padEnd(22))}</span><span class="d">${esc(d)}</span>`));
    },

    clear() { outEl.textContent = ""; },

    pwd() { text("/Users/you/acme-app"); },

    whoami() { text("you. Or an AI agent. Oopsy does not mind which."); },

    ls(args) {
      const all = args.includes("-a") || args.includes("-la") || args.includes("-al");
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
      const p = norm(file);
      let n = nodes.get(p);
      if (n && n.state === "present" && n.dir) return text(`zsh: is a directory: ${file}`, "ln--err");
      if (!n || n.state !== "present") {
        return text(`This demo only overwrites files that exist. Try echo "${value}" > README.md`, "ln--dim");
      }
      if (op === ">" ) {
        const prev = n.content;
        snapshot(raw, "content", [{ path: n.path, content: prev }], 1);
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
      if (!targets.length) return text("usage: rm [-rf] file ...", "ln--err");

      const hit = new Set();
      for (const t of targets) {
        if (t === "/" || t === "/*") return text(`rm: it is dangerous to operate recursively on '/'\nrm: use --no-preserve-root to override this failsafe`, "ln--err");
        if (t === "~" || t === "~/") return text("rm: refusing to delete your home directory in a browser demo. Try rm -rf *", "ln--dim");
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
      const files = victims.filter((n) => !n.dir);
      let id = null;
      if (oopsyOn) {
        id = snapshot(raw, "delete", victims.map((n) => ({ path: n.path })), files.length);
      }
      erase(victims, id);
    },

    git(args) {
      const sub = args.join(" ");
      if (sub === "status") {
        const modified = order.filter(isDirty);
        const deleted = order.filter((n) => !n.dir && !n.untracked && n.state !== "present");
        text("On branch main");
        if (!modified.length && !deleted.length) return text("nothing to commit, working tree clean");
        text("Changes not staged for commit:");
        modified.forEach((n) => line(`        <span class="y">modified:   ${esc(n.path)}</span>`));
        deleted.forEach((n) => line(`        <span class="hl">deleted:    ${esc(n.path)}</span>`));
        return;
      }
      if (/^reset\s+--hard/.test(sub) || sub === "checkout ." || sub === "restore .") {
        const changed = order.filter(isDirty);
        const missing = order.filter((n) => !n.untracked && n.state !== "present");
        if (changed.length && oopsyOn) {
          snapshot("git " + sub, "content", changed.map((n) => ({ path: n.path, content: n.content })), changed.length);
        }
        changed.forEach((n) => { n.content = n.committed; });
        // git brings back tracked files that were deleted
        missing.forEach((n) => { n.state = "present"; n.ghostId = null; });
        snapshots.forEach((s) => { if (s.kind === "delete" && missing.length) s.items = s.items.filter((it) => nodes.get(it.path).state !== "present"); });
        flash(changed);
        if (missing.length) restoreAnim(missing);
        order.forEach(paintNode);
        if (sub.startsWith("reset")) text("HEAD is now at 9c41e2b Add getUser endpoint");
        return;
      }
      if (sub === "log" || sub === "log --oneline") return text("9c41e2b Add getUser endpoint\n1f0a7d3 Initial commit");
      if (!sub) return text("usage: git <command>. This demo knows status, reset --hard and log.", "ln--dim");
      text(`This demo only knows git status, git reset --hard and git log.`, "ln--dim");
    },

    oops(args) {
      if (!oopsyOn) return text("zsh: command not found: oops", "ln--err");
      const sub = args[0];
      if (!sub) return undo(latestUndoable());
      if (sub === "list" || sub === "ls") return list();
      if (sub === "undo") {
        if (!args[1]) return undo(latestUndoable());
        const s = snapshots.find((x) => x.id === args[1]);
        if (!s) return text(`oops: no snapshot with id ${args[1]}. Run oops list to see them.`, "ln--err");
        if (s.restored) return text(`oops: snapshot ${s.id} is already restored.`, "ln--dim");
        return undo(s);
      }
      if (sub === "init") {
        line(`<span class="g">hooked</span> zsh            ~/.zshrc`);
        line(`<span class="g">hooked</span> Claude Code    ~/.claude/settings.json`);
        line(`<span class="g">hooked</span> Cursor         ~/.cursor/hooks.json`);
        text("Oopsy is on. Destructive commands get snapshotted first.");
        return;
      }
      if (sub === "db") return text("Try the Postgres demo further down the page.", "ln--dim");
      if (sub === "help" || sub === "--help" || sub === "-h") {
        text("usage: oops [list | undo <id> | init | db proxy | db undo]");
        return;
      }
      text(`oops: unknown command "${sub}". Try oops list.`, "ln--err");
    },

    sudo() { text("Nice try. Oopsy would still have your back, though.", "ln--dim"); },
  };
  COMMANDS.dir = COMMANDS.ls;

  function snapshot(cmd, kind, items, count) {
    const id = newId();
    snapshots.push({ id, cmd, kind, items, count, at: Date.now(), restored: false });
    line(`<span class="y">oopsy</span> <span class="d">saved ${plural(count, "file")} before</span> ${esc(cmd)} <span class="d">as</span> <span class="y">${id}</span><span class="d">. Undo with</span> <span class="y">oops</span>`, "ln--oops");
    return id;
  }

  function latestUndoable() {
    for (let i = snapshots.length - 1; i >= 0; i--) if (!snapshots[i].restored) return snapshots[i];
    return null;
  }

  function undo(s) {
    if (!s) return text("Nothing to undo. Oopsy has not had to save anything yet.", "ln--dim");
    s.restored = true;
    if (s.kind === "delete") {
      const back = s.items.map((it) => nodes.get(it.path)).filter((n) => n.state === "ghost");
      back.forEach((n) => { n.state = "present"; n.ghostId = null; });
      restoreAnim(back);
    } else {
      const back = s.items.map((it) => { const n = nodes.get(it.path); n.content = it.content; return n; });
      restoreAnim(back.filter((n) => n.state === "present"));
    }
    order.forEach(paintNode);
    line(`<span class="g">restored</span> ${plural(s.count, "file")} from <span class="y">${s.id}</span> <span class="d">(${esc(s.cmd)}, ${ago(s.at)})</span>`, "ln--ok");
    updateChips();
  }

  function list() {
    if (!snapshots.length) return text("No snapshots yet. Oopsy saves one the moment something destructive runs.", "ln--dim");
    line(`<span class="d">${"ID".padEnd(7)}${"WHEN".padEnd(10)}${"FILES".padEnd(7)}COMMAND</span>`);
    [...snapshots].reverse().forEach((s) => {
      line(`<span class="y">${s.id.padEnd(7)}</span>${esc(ago(s.at).padEnd(10))}${String(s.count).padEnd(7)}${esc(s.cmd)}${s.restored ? ' <span class="d">(restored)</span>' : ""}`);
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
    { cmd: "cat src/api.ts" },
    { cmd: "rm -rf src", danger: true },
    { cmd: "git reset --hard", danger: true },
    { cmd: 'echo "hi" > README.md', danger: true },
    { cmd: "rm -rf *", danger: true },
    { cmd: "oops", hero: true },
    { cmd: "oops list" },
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
    line(`<span class="d"># This is acme-app. src/api.ts has two hours of uncommitted work,</span>`);
    line(`<span class="d"># and .env is not in git at all. Oopsy is on.</span>`);
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
   * Postgres demo
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
    if (!db.exists) {
      dbEmpty.hidden = false;
      dbEmpty.textContent = 'relation "users" does not exist';
      dbTable.querySelector("thead").setAttribute("aria-hidden", "true");
    } else if (!db.rows.length) {
      dbEmpty.hidden = false;
      dbEmpty.textContent = "0 rows";
      dbTable.querySelector("thead").removeAttribute("aria-hidden");
    } else {
      dbEmpty.hidden = true;
      dbTable.querySelector("thead").removeAttribute("aria-hidden");
    }
  }

  function dbEraseRows() {
    const trs = Array.from(dbRowsEl.children);
    trs.forEach((tr, i) => { tr.style.setProperty("--d", `${i * 60}ms`); tr.classList.add("is-erasing"); });
    return wait(400 + trs.length * 60);
  }

  function dbSnap(stmt, rows, kind) {
    dbSeq += 1;
    const id = `db${dbSeq}`;
    dbSnaps.push({ id, stmt, kind, rows: rows.map((r) => ({ ...r })), existed: db.exists, at: Date.now(), restored: false });
    return id;
  }

  const DB_CMDS = {
    "SELECT * FROM users;": async () => {
      if (!db.exists) return dbLine('ERROR:  relation "users" does not exist', "ln--err");
      dbLine(`(${plural(db.rows.length, "row")})`);
    },
    "UPDATE users SET plan = 'free';": async (s) => {
      if (!db.exists) return dbLine('ERROR:  relation "users" does not exist', "ln--err");
      const id = dbSnap(s, db.rows, "update");
      dbLine(`oopsy proxy  saved ${plural(db.rows.length, "row")} of users before UPDATE as ${id}`, "ln--proxy");
      db.rows = db.rows.map((r) => ({ ...r, plan: "free" }));
      dbRender();
      Array.from(dbRowsEl.querySelectorAll("td:last-child")).forEach((td) => td.classList.add("is-changed"));
      dbLine(`UPDATE ${db.rows.length}`);
    },
    "DELETE FROM users;": async (s) => {
      if (!db.exists) return dbLine('ERROR:  relation "users" does not exist', "ln--err");
      const id = dbSnap(s, db.rows, "delete");
      dbLine(`oopsy proxy  saved ${plural(db.rows.length, "row")} of users before DELETE as ${id}`, "ln--proxy");
      const n = db.rows.length;
      await dbEraseRows();
      db.rows = [];
      dbRender();
      dbLine(`DELETE ${n}`);
    },
    "DROP TABLE users;": async (s) => {
      if (!db.exists) return dbLine('ERROR:  table "users" does not exist', "ln--err");
      const id = dbSnap(s, db.rows, "drop");
      dbLine(`oopsy proxy  saved table users (${plural(db.rows.length, "row")}, schema) before DROP as ${id}`, "ln--proxy");
      await dbEraseRows();
      db.exists = false;
      db.rows = [];
      dbRender();
      dbLine("DROP TABLE");
    },
    "oops db undo": async () => {
      let s = null;
      for (let i = dbSnaps.length - 1; i >= 0; i--) if (!dbSnaps[i].restored) { s = dbSnaps[i]; break; }
      if (!s) return dbLine("Nothing to undo. The proxy has not had to save anything yet.", "ln--dim");
      s.restored = true;
      db.exists = true;
      db.rows = s.rows.map((r) => ({ ...r }));
      dbRender("restore");
      dbLine(`<span class="g">restored</span> users, ${plural(s.rows.length, "row")}, from <span class="y">${s.id}</span> <span class="d">(${esc(s.stmt.replace(/;$/, ""))}, ${ago(s.at)})</span>`, "ln--ok");
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
    dbLine(`<span class="d"># Your app talks to Postgres through oopsy db proxy.</span>`);
    dbLine(`<span class="d"># Run something you will regret.</span>`);
    dbRender();
    dbUpdateChips();
  }
  $("#db-reset").addEventListener("click", dbReset);
  dbReset();
})();
