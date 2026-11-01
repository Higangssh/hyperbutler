// hyperbutler GUI page. It talks to the host only through window.chrome.webview
// (internal/gui/bridge). Text from containers, dockerd or the guest is only ever put in
// the document with textContent: the page never assigns HTML (the CSP's Trusted Types
// rule makes every HTML sink throw), never evaluates strings and loads nothing from the
// network (the CSP allows only this origin, and the host answers every request).
"use strict";

(() => {
  const wv = window.chrome && window.chrome.webview;
  let M = {};
  let LANG = "en";
  let INIT = {};
  const pending = new Map();
  let nextId = 1;

  // --- bridge ---------------------------------------------------------------------------
  function call(op, args) {
    const id = nextId++;
    return new Promise((resolve, reject) => {
      pending.set(id, { resolve, reject });
      wv.postMessage({ id, op, args: args || {} });
    });
  }
  wv.addEventListener("message", (e) => {
    const m = e.data;
    if (!m || typeof m !== "object") return;
    if (m.event) { onEvent(m); return; }
    const p = pending.get(m.id);
    if (!p) return;
    pending.delete(m.id);
    if (m.ok) p.resolve(m.data);
    else p.reject(m.error || { code: "HB-GUI-001", detail: "", ref: 0 });
  });
  window.addEventListener("error", (e) => {
    try { wv.postMessage({ id: nextId++, op: "log", args: { text: String(e.message) + " @" + String(e.lineno) } }); } catch (_) { /* nothing to do */ }
  });

  // --- messages (REQ-UX-013) -------------------------------------------------------------
  function t(key, params) {
    let s = M[key];
    if (s === undefined) return "⟦" + key + "⟧"; // shows a missing key, never silently
    if (params) s = s.replace(/\{(\w+)\}/g, (all, k) => (k in params ? String(params[k]) : all));
    return s;
  }
  let plural = null;
  function tp(key, n, params) {
    const form = plural.select(n);
    const k = (key + "#" + form) in M ? key + "#" + form : key + "#other";
    return t(k, Object.assign({ n: fmtInt(n) }, params || {}));
  }
  function fmtInt(n) { return new Intl.NumberFormat(LANG).format(n); }
  // listText joins a list as the language does ("a, b and c"; "unit": "a, b").
  function listText(items, type) {
    const lang = LANG === "pseudo" ? "en" : LANG;
    return new Intl.ListFormat(lang, { style: "long", type: type || "conjunction" }).format(items.map(String));
  }
  function fmt1(n) { return new Intl.NumberFormat(LANG, { maximumFractionDigits: 1, minimumFractionDigits: n < 10 ? 1 : 0 }).format(n); }
  // bytes returns [number, unit] for a big-number card.
  function bytes(b) {
    if (b >= 1 << 30) return [fmt1(b / (1 << 30)), t("unit.gb")];
    if (b >= 1 << 20) return [fmtInt(Math.round(b / (1 << 20))), t("unit.mb")];
    return [fmtInt(Math.round(b / 1024)), t("unit.kb")];
  }
  function bytesText(b) { const [n, u] = bytes(b); return t("unit.value", { n, unit: u }); }
  function duration(sec) {
    if (sec < 60) return tp("time.seconds", sec);
    const m = Math.floor(sec / 60);
    if (m < 60) return tp("time.minutes", m);
    const h = Math.floor(m / 60);
    if (h < 48) return t("time.hoursMinutes", { h: fmtInt(h), m: fmtInt(m % 60) });
    return tp("time.days", Math.floor(h / 24));
  }

  // --- DOM helpers: text only ----------------------------------------------------------
  const $ = (id) => document.getElementById(id);
  function h(tag, props, ...kids) {
    const el = document.createElement(tag);
    for (const [k, v] of Object.entries(props || {})) {
      if (v === undefined || v === null || v === false) continue;
      if (k === "class") el.className = v;
      else if (k === "text") el.textContent = v;
      else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
      else el.setAttribute(k, v === true ? "" : String(v));
    }
    for (const c of kids.flat()) {
      if (c === undefined || c === null || c === false) continue;
      el.append(typeof c === "string" ? document.createTextNode(c) : c);
    }
    return el;
  }
  function clear(el) { while (el.firstChild) el.removeChild(el.firstChild); }
  function translateStatic() {
    document.documentElement.lang = LANG;
    for (const el of document.querySelectorAll("[data-k]")) el.textContent = t(el.dataset.k);
    for (const el of document.querySelectorAll("[data-k-aria]")) el.setAttribute("aria-label", t(el.dataset.kAria));
    for (const el of document.querySelectorAll("[data-k-alt]")) el.setAttribute("alt", t(el.dataset.kAlt));
    document.title = t("app.title");
  }
  // setLanguage switches the page to a language and its catalog (at the start, and when
  // gui.language changes: the host's answer carries both, so nothing reloads).
  function setLanguage(lang, messages) {
    LANG = lang;
    M = messages;
    plural = new Intl.PluralRules(LANG === "pseudo" ? "en" : LANG);
    translateStatic();
    $("about").textContent = t("help.about", { version: INIT.version });
  }
  function busy(btn, on) {
    if (!btn) return;
    btn.disabled = on;
    if (on) btn.setAttribute("aria-busy", "true"); else btn.removeAttribute("aria-busy");
  }

  // --- errors: code, translation, English original ---------------------------------------
  let toastRef = 0;
  function showError(err, what) {
    const code = (err && err.code) || "HB-GUI-001";
    const msg = ("error." + code) in M ? t("error." + code) : t("error.unknown");
    $("toast").className = "toast";
    $("toast-title").textContent = what || t("error.title");
    $("toast-text").textContent = msg;
    $("toast-code").textContent = t("error.code", { code });
    toastRef = (err && err.ref) || 0;
    $("toast-copy").hidden = !toastRef;
    $("toast").hidden = false;
  }
  // showInfo shows a note; one that stays (keep) is not hidden after a while.
  function showInfo(text, keep) {
    $("toast").className = "toast info";
    $("toast-title").textContent = text;
    $("toast-text").textContent = "";
    $("toast-code").textContent = "";
    $("toast-copy").hidden = true;
    $("toast").hidden = false;
    if (!keep) setTimeout(() => { if ($("toast").classList.contains("info") && $("toast-title").textContent === text) $("toast").hidden = true; }, 4000);
  }
  function copyEnglish(ref, btn) {
    if (!ref) return;
    call("error.copy", { ref }).then(() => { if (btn) btn.textContent = t("error.copied"); }, (e) => showError(e));
  }

  // --- confirmation, with the REQ-UX-017 list ------------------------------------------
  function confirmDialog(title, body, okLabel, list) {
    return new Promise((resolve) => {
      const d = $("confirm");
      $("confirm-title").textContent = title;
      $("confirm-body").textContent = body;
      $("confirm-ok").textContent = okLabel;
      const ul = $("confirm-list");
      clear(ul);
      for (const li of list || []) ul.append(li);
      ul.hidden = !(list && list.length);
      d.returnValue = "cancel";
      d.addEventListener("close", () => resolve(d.returnValue === "ok"), { once: true });
      d.showModal();
      $("confirm-cancel").focus();
    });
  }
  // stopConfirm asks before something stops the running containers: what is "engine.stop",
  // "engine.restart" or "dockerd". It lists them and what happens to each (REQ-UX-017).
  // It returns the host's confirmation token, or "" when the user cancels. The host
  // computes the list and issues the token together, and refuses the operation without
  // it (internal/gui/bridge/confirm.go).
  const confirmAction = { "engine.stop": "engine.stop", "engine.restart": "engine.restart", dockerd: "engine.restartDockerd" };
  async function stopConfirm(what) {
    let c;
    try { c = await call("confirm", { action: confirmAction[what] }); } catch (e) { showError(e); return ""; }
    const p = c.preview || { running: true, unknown: true, containers: [], counts: {} };
    const items = (p.containers || []).map((c) => h("li", {}, h("span", { class: "name", text: c.name }),
      h("span", { class: "sub", text: c.image }), h("span", { class: "sub", text: t("stop.then." + c.then) })));
    let body;
    if (!p.running) body = t("confirm." + what + ".body.stopped");
    else if (p.unknown) body = t("stop.unknown");
    else if (!items.length) body = t("stop.none");
    else body = tp("stop.summary", items.length, { back: fmtInt(p.counts.back || 0), stays: fmtInt((p.counts.stays || 0) + (p.counts.maybe || 0)) });
    return (await confirmDialog(t("confirm." + what + ".title"), body, t("confirm." + what + ".ok"), items)) ? c.token : "";
  }
  // confirmToken shows a plain confirmation and returns the host's token for action.
  async function confirmToken(action, id, title, body, okLabel) {
    if (!(await confirmDialog(title, body, okLabel, []))) return "";
    try { return (await call("confirm", id ? { action, id } : { action })).token; } catch (e) { showError(e); return ""; }
  }

  // --- state ----------------------------------------------------------------------------
  let status = null;
  let containers = null;
  let tab = "home";
  let engineBusy = "";
  let selected = "";

  async function refreshStatus(auto) {
    try {
      status = await call("status", { auto: !!auto });
    } catch (e) {
      if (e.code !== "HB-GUI-004" && !auto) showError(e);
      return;
    }
    render();
    if (status.running && (tab === "home" || tab === "containers")) refreshContainers(auto);
  }
  async function refreshContainers(auto) {
    if (!status || !status.running || status.phase !== "running") { containers = null; renderContainers(); renderHome(); return; }
    try {
      containers = await call("containers", { auto: !!auto });
    } catch (e) {
      if (e.code !== "HB-GUI-004" && !auto) showError(e);
      if (e.code === "HB-ENG-004") containers = null;
    }
    renderContainers();
    renderHome();
  }

  function render() {
    if (picking) { $("first").hidden = true; $("win").hidden = true; return; }
    if (!status) return;
    const first = status.firstStart && !firstDismissed;
    $("first").hidden = !first;
    $("win").hidden = first;
    if (first) { renderFirst(); return; }
    renderHome();
    if (tab === "containers") renderContainers();
  }

  // --- engine actions ---------------------------------------------------------------------
  async function engineAction(op, btn, token) {
    if (engineBusy) return;
    if (!token && op !== "engine.start") {
      token = await stopConfirm({ "engine.stop": "engine.stop", "engine.restart": "engine.restart", "engine.restartDockerd": "dockerd" }[op]);
      if (!token) return;
    }
    engineBusy = op;
    busy(btn, true);
    renderHome();
    try {
      await call(op, token ? { token } : {});
      showInfo(t("done." + op));
    } catch (e) {
      showError(e, t("fail." + op));
    } finally {
      engineBusy = "";
      busy(btn, false);
      await refreshStatus(false);
    }
  }

  // --- first open: the language, asked once ----------------------------------------------
  // Shown before anything else while gui.language was never chosen (INIT.pickLanguage).
  // Each language is named in itself, the Windows display language is preselected, and
  // the picker's own texts follow the one pointed at. Continue saves it through
  // config.set; the answer carries the catalog, so the page goes on in it at once.
  let picking = false;
  function showPicker() {
    picking = true;
    render();
    const list = $("lang-list");
    clear(list);
    const langs = INIT.languages || [];
    const pick = langs.some((l) => l.code === INIT.suggest) ? INIT.suggest : (langs[0] || {}).code;
    const pickTexts = (code) => {
      const l = langs.find((x) => x.code === code);
      const tx = (l && l.texts) || {};
      const say = (key) => (key in tx ? tx[key] : t(key));
      $("lang").lang = code;
      $("lang-title").textContent = say("language.pick.title");
      $("lang-body").textContent = say("language.pick.body");
      $("lang-ok").textContent = say("language.pick.ok");
    };
    for (const l of langs) {
      const input = h("input", { type: "radio", name: "lang", value: l.code, checked: l.code === pick });
      input.addEventListener("change", () => { if (input.checked) pickTexts(l.code); });
      list.append(h("label", { lang: l.code }, input, h("span", { text: l.name })));
    }
    $("lang").hidden = false;
    const checked = list.querySelector("input:checked");
    if (checked) checked.focus();
    return new Promise((resolve) => {
      const ok = $("lang-ok");
      async function choose() {
        const c = list.querySelector("input:checked");
        if (!c) return;
        busy(ok, true);
        try {
          const r = await call("config.set", { key: "gui.language", value: c.value });
          ok.removeEventListener("click", choose);
          $("lang").hidden = true;
          picking = false;
          if (r.lang && r.messages) setLanguage(r.lang, r.messages);
          resolve();
        } catch (e) {
          showError(e, t("fail.setting", { name: t("setting.gui.language") }));
        } finally {
          busy(ok, false);
        }
      }
      ok.addEventListener("click", choose);
    });
  }
  // relanguage redraws what the page drew in the old language, after gui.language
  // changed from Settings; the other screens draw again when shown.
  function relanguage() {
    $("toast").hidden = true;
    clear($("doctor-list"));
    $("diag-result").textContent = "";
    $("daemon-result").hidden = true;
    if (daemonReadOnly) daemonResult(false, t("error.HB-DMN-003"));
    renderDaemonPath();
    render();
    renderSettings();
  }

  // --- first run ------------------------------------------------------------------------
  let firstDismissed = false;
  let firstChecked = false;
  function renderFirst() {
    if (firstChecked) return;
    firstChecked = true;
    const ul = $("first-checks");
    clear(ul);
    ul.append(h("li", { class: "chk" }, h("span", { class: "sub", text: t("first.checking") })));
    call("doctor").then((r) => {
      clear(ul);
      for (const id of ["vm-platform", "hyperv-admins", "host-disk", "docker-cli"]) {
        const c = r.checks.find((x) => x.id === id);
        if (c) ul.append(checkItem(c, false));
      }
      if (!ul.firstChild) for (const c of r.checks.slice(0, 3)) ul.append(checkItem(c, false));
    }, () => { clear(ul); });
  }

  // --- home -----------------------------------------------------------------------------
  function phaseKey() {
    if (engineBusy === "engine.start") return "starting";
    if (engineBusy === "engine.stop") return "stopping";
    if (engineBusy === "engine.restart" || engineBusy === "engine.restartDockerd") return "restarting";
    if (status.paused) return "paused";
    return status.phase;
  }
  function renderHome() {
    if (!status) return;
    const ph = phaseKey();
    const st = $("hero-state");
    st.className = "state " + ({ running: "running", failed: "failed", starting: "busy", stopping: "busy", restarting: "busy" }[ph] || "");
    st.textContent = t("home.state", { phase: t("phase." + ph) });
    const running = containers ? containers.running : 0;
    let title, sub;
    if (ph === "running") {
      title = containers ? tp("home.serving", running) : t("home.title.running");
      sub = status.upSeconds ? t("home.sub.running", { time: duration(status.upSeconds) }) : t("home.sub.runningNoTime");
    } else {
      title = t("home.title." + ph);
      sub = t("home.sub." + ph);
    }
    $("hero-title").textContent = title;
    $("hero-sub").textContent = sub;
    $("hero-mascot").className = "mascot" + (ph === "failed" ? " sad" : "");

    const btns = $("hero-buttons");
    clear(btns);
    const idle = !engineBusy;
    if (ph === "running" || ph === "paused") {
      btns.append(h("button", { class: "btn outline", disabled: !idle, onclick: (e) => engineAction("engine.stop", e.currentTarget) }, t("action.stop")));
      btns.append(h("button", { class: "btn primary", disabled: !idle, onclick: (e) => engineAction("engine.restart", e.currentTarget) }, t("action.restart")));
    } else if (ph === "stopped" || ph === "failed" || ph === "notsetup") {
      btns.append(h("button", { class: "btn primary", id: "home-start", disabled: !idle, onclick: (e) => engineAction("engine.start", e.currentTarget) }, t("action.start")));
    } else {
      btns.append(h("button", { class: "btn primary", disabled: true, "aria-busy": "true" }, t("phase." + ph)));
    }

    // Error state (the mockup's Error screen): the last start failed.
    const err = ph === "failed" && status.error;
    $("error-card").hidden = !err;
    if (err) {
      const code = status.error.code;
      $("hero-title").textContent = ("error." + code + ".title") in M ? t("error." + code + ".title") : t("home.title.failed");
      $("hero-sub").textContent = ("error." + code) in M ? t("error." + code) : t("home.sub.failed");
      const ol = $("error-steps");
      clear(ol);
      let i = 1;
      while (("error." + code + ".step" + i) in M) {
        ol.append(h("li", { class: "step" }, h("span", { class: "n", "aria-hidden": "true", text: String(i) }), h("div", { class: "grow", text: t("error." + code + ".step" + i) })));
        i++;
      }
      if (i === 1) ol.append(h("li", { class: "step" }, h("span", { class: "n", "aria-hidden": "true", text: "1" }), h("div", { class: "grow", text: t("error.generic.step1") })));
      $("error-detail").textContent = status.error.detail || "";
      $("error-code").textContent = t("error.code", { code });
      $("error-copy").textContent = t("error.copy");
    }

    const [mn, mu] = status.memoryMB ? bytes(status.memoryMB * (1 << 20)) : ["-", ""];
    $("st-mem").textContent = mn;
    $("st-mem-unit").textContent = mu;
    $("st-mem-sub").textContent = t(status.running ? "home.memory.sub.running" : "home.memory.sub.stopped");
    $("st-cpu").textContent = status.cpus ? fmtInt(status.cpus) : "-";
    $("st-cpu-sub").textContent = INIT.hostCpus ? tp("home.cpus.sub", INIT.hostCpus) : "";
    if (status.diskMax) {
      const [dn, du] = bytes(status.diskUsed || 0);
      $("st-disk").textContent = dn;
      $("st-disk-unit").textContent = du;
      $("st-disk-sub").textContent = t("home.disk.sub", { max: bytesText(status.diskMax), free: bytesText(status.hostFree || 0) });
    } else {
      $("st-disk").textContent = "-";
      $("st-disk-unit").textContent = "";
      $("st-disk-sub").textContent = t("home.disk.none");
    }
    if (containers) {
      $("st-ctr").textContent = fmtInt(containers.running);
      $("st-ctr-unit").textContent = t("home.containers.total", { n: fmtInt(containers.total) });
      $("st-ctr-sub").textContent = t("home.containers.sub");
    } else {
      $("st-ctr").textContent = "-";
      $("st-ctr-unit").textContent = "";
      $("st-ctr-sub").textContent = t("home.containers.unknown");
    }

    // Running containers by project.
    const box = $("home-project-list");
    clear(box);
    let shown = 0;
    if (containers && ph === "running") {
      for (const g of containers.groups) {
        const run = g.containers.filter((c) => c.state === "running");
        if (!run.length || shown >= 4) continue;
        shown++;
        const sec = h("div", { class: "proj" }, h("h3", { class: "strong", text: g.project ? t("home.project", { name: g.project }) : t("containers.noProject") }));
        for (const c of run.slice(0, 6)) {
          sec.append(h("div", { class: "proj-row" }, h("span", { class: "name", text: c.name }), h("span", { class: "sub", text: c.image }),
            h("span", { class: "sub", text: listText(c.ports.slice(0, 2), "unit") })));
        }
        box.append(sec);
      }
    }
    $("home-projects").hidden = shown === 0;
  }

  // --- containers -----------------------------------------------------------------------
  function initial(name) { return (name || "?").replace(/^[^\p{L}\p{N}]+/u, "").slice(0, 1).toUpperCase() || "?"; }
  function findContainer(id) {
    if (!containers) return null;
    for (const g of containers.groups) for (const c of g.containers) if (c.id === id) return c;
    return null;
  }
  function renderContainers() {
    const box = $("ctr-groups");
    clear(box);
    const empty = $("ctr-empty");
    clear(empty);
    if (!status || status.phase !== "running" || !containers) {
      box.append(h("p", { class: "sub", text: t("containers.engineOff") }));
      $("ctr-selected").hidden = true;
      empty.hidden = false;
      empty.append(h("span", { text: t("containers.engineOff") }));
      if (status && (status.phase === "stopped" || status.phase === "failed")) {
        empty.append(h("button", { class: "btn primary", disabled: !!engineBusy, onclick: (e) => engineAction("engine.start", e.currentTarget) }, t("action.start")));
      }
      return;
    }
    if (!containers.total) {
      box.append(h("div", { class: "empty" }, h("span", { text: t("containers.none") }), h("code", { class: "mono block", text: "docker run hello-world" })));
    }
    for (const g of containers.groups) {
      const head = h("div", { class: "group-head" }, h("h3", { text: g.project || t("containers.noProject") }),
        h("span", { class: "sub", text: g.running === g.containers.length && g.running ? tp("containers.allRunning", g.running) : tp("containers.someRunning", g.containers.length, { running: fmtInt(g.running) }) }));
      box.append(head);
      for (const c of g.containers) {
        const on = c.state === "running";
        box.append(h("button", { class: "item" + (on ? "" : " off"), "aria-current": c.id === selected ? "true" : "false", onclick: () => selectContainer(c.id) },
          h("span", { class: "av" + (on ? "" : " off"), "aria-hidden": "true", text: initial(c.name) }),
          h("div", { class: "grow" }, h("div", { text: c.name }), h("div", { class: "sub small", text: t("containers.imageStatus", { image: c.image, status: c.status }) })),
          c.ports.length ? h("span", { class: "sub small", text: c.ports[0] }) : null));
      }
    }
    if (containers.truncated) box.append(h("p", { class: "sub small", text: tp("containers.truncated", containers.total) }));
    const c = findContainer(selected);
    $("ctr-selected").hidden = !c;
    empty.hidden = !!c;
    if (!c) { empty.append(h("span", { text: t("containers.pick") })); return; }
    $("ctr-av").textContent = initial(c.name);
    $("ctr-av").className = "av" + (c.state === "running" ? "" : " off");
    $("ctr-name").textContent = c.name;
    $("ctr-status").textContent = t("containers.imageStatus", { image: c.image, status: c.status });
    const acts = $("ctr-actions");
    clear(acts);
    if (c.state === "running") {
      acts.append(h("button", { class: "btn ghost small", onclick: (e) => containerAction(c, "restart", e.currentTarget) }, t("action.restart")));
      acts.append(h("button", { class: "btn ghost small", onclick: (e) => containerAction(c, "stop", e.currentTarget) }, t("action.stop")));
    } else {
      acts.append(h("button", { class: "btn primary small", onclick: (e) => containerAction(c, "start", e.currentTarget) }, t("action.start")));
    }
  }
  function selectContainer(id) {
    if (selected !== id) { selected = id; $("log").textContent = ""; $("log-note").textContent = ""; }
    renderContainers();
    loadLogs();
  }
  async function containerAction(c, action, btn) {
    let token = "";
    if (action !== "start") {
      token = await confirmToken("container." + action, c.id, t("confirm.container." + action + ".title", { name: c.name }), t("confirm.container." + action + ".body"), t("action." + action));
      if (!token) return;
    }
    busy(btn, true);
    try {
      await call("container.action", token ? { id: c.id, action, token } : { id: c.id, action });
      showInfo(t("done.container." + action, { name: c.name }));
    } catch (e) {
      showError(e, t("fail.container." + action, { name: c.name }));
    } finally {
      busy(btn, false);
      await refreshContainers(false);
    }
  }
  async function loadLogs() {
    if (!selected) return;
    const id = selected;
    const btn = $("log-refresh");
    busy(btn, true);
    try {
      const r = await call("container.logs", { id, tail: Number($("log-tail").value) });
      if (id !== selected) return;
      $("log").textContent = r.text || t("containers.logs.empty"); // text, never HTML
      $("log-note").textContent = r.truncated ? tp("containers.logs.truncated", r.lines) : tp("containers.logs.lines", r.lines);
      $("log").scrollTop = $("log").scrollHeight;
    } catch (e) {
      showError(e, t("fail.logs"));
    } finally {
      busy(btn, false);
    }
  }

  // --- storage (read-only in this release) ------------------------------------------------
  async function loadStorage() {
    let s;
    try { s = await call("storage"); } catch (e) { showError(e); return; }
    const [n, u] = bytes(s.diskUsed || 0);
    $("sto-used").textContent = s.diskMax ? n : "-";
    $("sto-used-unit").textContent = s.diskMax ? u : "";
    $("sto-max").textContent = s.diskMax ? t("storage.max", { max: bytesText(s.diskMax) }) : t("home.disk.none");
    const total = Math.max(1, s.imagesSize + s.volumesSize + s.buildCache);
    $("sto-seg-img").style.width = (100 * s.imagesSize / total) + "%";
    $("sto-seg-vol").style.width = (100 * s.volumesSize / total) + "%";
    $("sto-seg-cache").style.width = (100 * s.buildCache / total) + "%";
    const leg = $("sto-legend");
    clear(leg);
    const parts = [["", "storage.images", s.imagesSize], ["vol", "storage.volumes", s.volumesSize], ["cache", "storage.cache", s.buildCache]];
    $("sto-bar").setAttribute("aria-label", listText(parts.map((p) => t(p[1], { size: bytesText(p[2]) })), "unit"));
    for (const [cls, key, size] of parts) leg.append(h("span", { class: cls, text: t("storage.legendItem", { text: t(key, { size: bytesText(size) }) }) }));
    $("sto-list-title").textContent = s.engine ? t("storage.counts", { images: tp("storage.imageCount", s.imageCount), volumes: tp("storage.volumeCount", s.volumeCount) }) : t("containers.engineOff");
    const list = $("sto-list");
    clear(list);
    for (const im of s.images) {
      list.append(h("div", { class: "row" }, h("span", { class: "name", text: im.name }),
        h("span", { class: "tag" + (im.containers > 0 ? " used" : ""), text: im.containers > 0 ? t("storage.inUse") : t("storage.unused") }),
        h("span", { class: "sub size", text: bytesText(im.size) })));
    }
    for (const v of s.volumes) {
      list.append(h("div", { class: "row" }, h("span", { class: "name", text: v.name }),
        h("span", { class: "tag" + (v.inUse ? " used" : ""), text: v.inUse ? t("storage.inUse") : t("storage.unused") }),
        h("span", { class: "sub size", text: v.size >= 0 ? bytesText(v.size) : t("storage.unknownSize") })));
    }
  }

  // --- settings ---------------------------------------------------------------------------
  const groups = {
    resources: ["memory", "cpus", "disk.size", "data.dir"],
    connection: ["hostGateway.enabled", "hostGateway.ports", "updateCheck", "context.manage"],
    app: ["gui.language", "gui.startOnOpen", "autostart"],
  };
  const applyClass = { live: "live", "restart-vm": "vm", "restart-dockerd": "dockerd", "first-start": "once" };
  let settings = [];
  const pendingRestart = new Set();

  function settingName(it) { return ("setting." + it.key) in M ? t("setting." + it.key) : it.key; }
  function settingHelp(it) { return ("setting." + it.key + ".help") in M ? t("setting." + it.key + ".help") : it.description; }

  async function loadSettings() {
    try { settings = (await call("config.list")).keys; } catch (e) { showError(e); return; }
    renderSettings();
  }
  function renderSettings() {
    const byKey = Object.fromEntries(settings.map((s) => [s.key, s]));
    const used = new Set();
    for (const [g, keys] of Object.entries(groups)) {
      const box = $("set-" + g);
      clear(box);
      for (const k of keys) if (byKey[k]) { box.append(settingRow(byKey[k])); used.add(k); }
    }
    const adv = $("set-advanced");
    clear(adv);
    adv.append(h("p", { class: "sub small", text: t("settings.advanced.note") }));
    for (const s of settings) if (!used.has(s.key)) adv.append(settingRow(s));
    renderBanner();
  }
  function whenPill(it) { return h("span", { class: "when " + (applyClass[it.apply] || "once"), text: t("apply." + it.apply) }); }
  function settingRow(it) {
    const id = "set-" + it.key.replace(/\./g, "-");
    const label = h("label", { for: id, class: "visually-hidden", text: settingName(it) });
    let control;
    if (!it.editable) {
      // Read-only: the value goes under the name, so a long path wraps in place.
      return h("div", { class: "set" }, h("div", { class: "k" }, h("b", { text: settingName(it) }), h("span", { class: "sub small", text: settingHelp(it) }),
        h("span", { class: "val", id, text: it.value || t("settings.empty") })), whenPill(it));
    }
    if (it.type === "bool") {
      control = h("button", { class: "tg", role: "switch", id, "aria-checked": String(it.value === "true"), "aria-label": settingName(it),
        onclick: (e) => setSetting(it, e.currentTarget.getAttribute("aria-checked") === "true" ? "false" : "true", e.currentTarget) });
      return h("div", { class: "set" }, h("div", { class: "k" }, h("b", { text: settingName(it) }), h("span", { class: "sub small", text: settingHelp(it) })), control, whenPill(it));
    }
    if (it.key === "memory" || it.key === "cpus" || it.key === "gui.language") {
      const opts = selectOptions(it);
      control = h("select", { class: "field", id, onchange: (e) => setSetting(it, e.currentTarget.value, e.currentTarget) },
        opts.map(([v, text, lang]) => h("option", { value: v, selected: v === it.value, lang }, text)));
    } else {
      const input = h("input", { class: "field", id, value: it.value, autocomplete: "off", spellcheck: "false" });
      control = h("span", { class: "row-buttons" }, input,
        h("button", { class: "btn ghost small", onclick: (e) => setSetting(it, input.value, e.currentTarget) }, t("action.save")));
    }
    return h("div", { class: "set" }, h("div", { class: "k" }, h("b", { text: settingName(it) }), h("span", { class: "sub small", text: settingHelp(it) })), label, control, whenPill(it));
  }
  function selectOptions(it) {
    // Each language named in itself, from its own catalog (INIT.languages).
    if (it.key === "gui.language") return [["auto", t("language.auto")]].concat((INIT.languages || []).map((l) => [l.code, l.name, l.code]));
    if (it.key === "cpus") {
      const n = Math.max(1, INIT.hostCpus || 8);
      const out = [];
      for (let i = 1; i <= n; i++) out.push([String(i), tp("unit.cpus", i)]);
      if (!out.some((o) => o[0] === it.value)) out.push([it.value, it.value]);
      return out;
    }
    const out = [2, 3, 4, 6, 8, 12, 16, 24, 32].map((g) => [g + "GiB", t("unit.value", { n: fmtInt(g), unit: t("unit.gb") })]);
    if (!out.some((o) => o[0] === it.value)) out.unshift([it.value, it.value]);
    return out;
  }
  // settingValue is a value as the confirmation names it: on/off for a switch.
  function settingValue(it, value) {
    if (it.type === "bool") return t(value === "true" ? "setting.value.on" : "setting.value.off");
    return value === "" ? t("settings.empty") : value;
  }
  async function setSetting(it, value, ctl) {
    // Some settings open this PC to the containers or reach outside the window: the page
    // asks first and the host refuses the change without a token for this key and value.
    let token = "";
    if (it.confirm) {
      const ok = await confirmDialog(t("confirm.setting.title", { name: settingName(it), value: settingValue(it, value) }),
        t("confirm.setting." + it.key + ".body"), t("confirm.setting.ok"), []);
      if (!ok) { renderSettings(); return; }
      try { token = (await call("confirm", { action: "config.set", key: it.key, value })).token; } catch (e) { showError(e); return; }
    }
    busy(ctl, true);
    try {
      const r = await call("config.set", token ? { key: it.key, value, token } : { key: it.key, value });
      const i = settings.findIndex((s) => s.key === it.key);
      if (i >= 0 && r.item && r.item.key) settings[i] = r.item;
      if (r.running && (it.apply === "restart-vm" || it.apply === "restart-dockerd")) pendingRestart.add(it.key);
      if (it.key === "gui.language" && r.lang && r.messages) {
        setLanguage(r.lang, r.messages);
        relanguage();
        showInfo(t("settings.languageChanged"));
      } else showInfo(t("settings.saved", { name: settingName(it) }));
    } catch (e) {
      showError(e, t("fail.setting", { name: settingName(it) }));
    } finally {
      busy(ctl, false);
      renderSettings();
    }
  }
  function renderBanner() {
    const keys = [...pendingRestart];
    $("set-banner").hidden = !keys.length;
    if (!keys.length) return;
    const vm = keys.some((k) => (settings.find((s) => s.key === k) || {}).apply === "restart-vm");
    const names = listText(keys.map((k) => settingName(settings.find((s) => s.key === k) || { key: k })));
    $("set-banner-text").textContent = t(vm ? "settings.pending.vm" : "settings.pending.dockerd", { names });
    $("set-banner-apply").dataset.op = vm ? "engine.restart" : "engine.restartDockerd";
  }

  // daemon.json
  let daemonReadOnly = false;
  let daemonView = null;
  function renderDaemonPath() {
    const d = daemonView;
    $("daemon-path").textContent = d ? t(d.exists ? "daemon.path" : "daemon.path.none", { path: d.path }) : "";
  }
  async function loadDaemon() {
    let d;
    try { d = await call("daemon.get"); } catch (e) { showError(e); return; }
    const ta = $("daemon-text");
    ta.value = d.text || (d.exists ? "" : "{\n}\n");
    daemonReadOnly = d.readOnly;
    ta.readOnly = d.readOnly;
    daemonView = { exists: d.exists, path: d.path };
    renderDaemonPath();
    $("daemon-save").disabled = d.readOnly;
    $("daemon-apply").disabled = false;
    if (d.readOnly) daemonResult(false, t("error.HB-DMN-003"));
    else $("daemon-result").hidden = true;
  }
  function daemonResult(ok, text, detail) {
    const r = $("daemon-result");
    clear(r);
    r.className = "result " + (ok ? "ok" : "bad");
    r.append(h("span", { class: "strong", text }));
    if (detail) r.append(h("span", { class: "mono", text: detail }));
    r.hidden = false;
  }
  async function daemonValidate() {
    const r = await call("daemon.validate", { text: $("daemon-text").value });
    if (r.ok) daemonResult(true, t("daemon.valid"));
    else daemonResult(false, t("daemon.invalid"), r.detail);
    return r.ok;
  }
  // daemonSave writes daemon.json. With ask it shows its own confirmation; Apply asks
  // once, in the dockerd dialog, and then only fetches the save's token.
  async function daemonSave(ask) {
    if (daemonReadOnly) return false;
    let token;
    if (ask) token = await confirmToken("daemon.save", "", t("confirm.daemon.save.title"), t("confirm.daemon.save.body"), t("daemon.save"));
    else {
      try { token = (await call("confirm", { action: "daemon.save" })).token; } catch (e) { showError(e); return false; }
    }
    if (!token) return false;
    try {
      await call("daemon.save", { text: $("daemon-text").value, token });
      daemonResult(true, t("daemon.saved"));
      return true;
    } catch (e) {
      daemonResult(false, ("error." + e.code) in M ? t("error." + e.code) : t("daemon.invalid"), e.detail);
      return false;
    }
  }
  async function daemonApply(btn) {
    if (!status || !status.running) {
      if (await daemonSave(true)) daemonResult(true, t("daemon.appliesAtStart"));
      return;
    }
    // One dialog: the dockerd restart's, with the containers it stops.
    const token = await stopConfirm("dockerd");
    if (!token) return;
    if (!daemonReadOnly && !(await daemonSave(false))) return;
    await engineAction("engine.restartDockerd", btn, token);
  }

  // --- help -------------------------------------------------------------------------------
  function checkItem(c, detail) {
    const mark = { ok: "✓", info: "i", warn: "!", fail: "×" }[c.status] || "i";
    const title = ("doctor." + c.id) in M ? t("doctor." + c.id) : c.id;
    const txt = h("div", { class: "txt" }, h("span", { text: title }), h("span", { class: "sub small", text: c.summary }));
    if (detail && c.fix && c.status !== "ok") txt.append(h("span", { class: "sub small", text: c.fix }));
    return h("li", { class: "chk" }, h("span", { class: "mark " + c.status, "aria-label": t("check." + c.status), role: "img", text: mark }), txt);
  }
  async function runDoctor() {
    const btn = $("doctor-run");
    const ul = $("doctor-list");
    busy(btn, true);
    clear(ul);
    ul.append(h("li", { class: "chk sub", text: t("help.doctor.running") }));
    try {
      const r = await call("doctor");
      clear(ul);
      for (const c of r.checks) ul.append(checkItem(c, true));
    } catch (e) {
      clear(ul);
      showError(e);
    } finally {
      busy(btn, false);
    }
  }
  async function runDiagnose(btn) {
    busy(btn, true);
    $("diag-result").textContent = t("help.diagnose.running");
    try {
      const r = await call("diagnose");
      $("diag-result").textContent = t("help.diagnose.done", { path: r.file });
      $("diag-reveal").hidden = false;
    } catch (e) {
      $("diag-result").textContent = "";
      showError(e, t("fail.diagnose"));
    } finally {
      busy(btn, false);
    }
  }

  // --- import from Docker Desktop (REQ-UX-023) ---------------------------------------------
  // The import itself is another package; until it is in the build the host answers
  // HB-IMP-001 and the dialog says so. Already imported items are shown but cannot be
  // chosen. Progress arrives as {"event":"import"} from the host: nothing polls.
  let importing = false;
  function importStatusText(st) { return t("import.status." + st); }
  // The import's reasons are English: the page shows its own sentence for each (a
  // detail key with lists for its placeholders) and only a button that copies the
  // English original the host kept (by ref), as for errors.
  function copyButton(ref) {
    if (!ref) return null;
    return h("button", { type: "button", class: "link inline-copy", onclick: (e) => { e.preventDefault(); copyEnglish(ref, e.currentTarget); } }, t("error.copy"));
  }
  function detailText(it) {
    const params = {};
    for (const [k, l] of Object.entries(it.args || {})) params[k] = listText(l);
    return t(it.detail, params);
  }
  // importNote shows a sentence in the dialog, with the copy button when ref is set.
  function importNote(text, ref) {
    const n = $("import-note");
    clear(n);
    n.append(text);
    const b = copyButton(ref);
    if (b) n.append(b);
  }
  function errorText(e) { return ("error." + e.code) in M ? t("error." + e.code) : t("error.unknown"); }
  async function openImport() {
    const d = $("import");
    const ul = $("import-list");
    clear(ul);
    $("import-progress").hidden = true;
    $("import-cancel").hidden = true;
    $("import-force").hidden = true;
    $("import-start").hidden = false;
    $("import-start").disabled = true;
    importNote(t("import.loading"));
    d.showModal();
    let plan;
    try {
      plan = await call("import.plan");
    } catch (e) {
      importNote(errorText(e), e.ref);
      $("import-start").hidden = true;
      return;
    }
    if (!plan.items.length) { importNote(t("import.nothing")); $("import-start").hidden = true; return; }
    importNote(t("import.pick"));
    if (plan.notes) {
      const n = $("import-note");
      n.append(h("span", { class: "line" }, tp("import.notes", plan.notes), copyButton(plan.notesRef)));
    }
    for (const it of plan.items) {
      const can = it.status === "new" || it.status === "partial" || it.status === "exists";
      const box = h("input", { type: "checkbox", value: it.id, checked: it.status === "new", disabled: !can });
      box.addEventListener("change", updateImportStart);
      const note = it.detail ? h("span", { class: "sub small note-line" }, h("span", { text: detailText(it) }), copyButton(it.noteRef)) : null;
      ul.append(h("li", { class: can ? "" : "done" }, h("label", {}, box,
        h("span", { class: "name", text: it.name }), h("span", { class: "sub small", text: ("import.kind." + it.kind) in M ? t("import.kind." + it.kind) : it.kind }),
        h("span", { class: "tag " + it.status, text: importStatusText(it.status) }),
        h("span", { class: "sub small", text: it.size >= 0 ? bytesText(it.size) : t("storage.unknownSize") }),
        note)));
    }
    updateImportStart();
  }
  function selectedImport() { return [...$("import-list").querySelectorAll("input:checked:not(:disabled)")].map((x) => x.value); }
  function updateImportStart() { $("import-start").disabled = importing || selectedImport().length === 0; }
  // startImport asks, then starts. force is import --force-space, offered only after the
  // host's free-space check said the copy may not fit.
  async function startImport(force) {
    const ids = selectedImport();
    if (!ids.length) return;
    $("import-force").hidden = true;
    const dlg = $("import");
    dlg.close();
    const token = await confirmToken("import.run", "", t("confirm.import.title"), tp("confirm.import.body", ids.length), t("import.start"));
    dlg.showModal();
    if (!token) return;
    importing = true;
    updateImportStart();
    for (const x of $("import-list").querySelectorAll("input")) x.disabled = true;
    $("import-progress").hidden = false;
    $("import-cancel").hidden = false;
    $("import-progress-text").textContent = t("import.starting");
    try {
      await call("import.run", force ? { ids, token, force: true } : { ids, token });
    } catch (e) {
      importing = false;
      $("import-cancel").hidden = true;
      $("import-progress").hidden = true;
      for (const x of $("import-list").querySelectorAll("input")) x.disabled = x.closest("li").classList.contains("done");
      updateImportStart();
      if (e.code === "HB-IMP-004") {
        importNote(t("error.HB-IMP-004"), e.ref); // the sizes, in English, behind the copy
        $("import-force").hidden = false;
      } else {
        showError(e);
      }
    }
  }
  function onImportProgress(p) {
    const total = Math.max(1, p.total || 1);
    $("import-bar").style.width = (100 * (p.done || 0) / total) + "%";
    if (p.running) {
      $("import-progress-text").textContent = t("import.progress", { done: fmtInt(p.done || 0), total: fmtInt(p.total || 0), name: p.current || "" });
      return;
    }
    importing = false;
    $("import-cancel").hidden = true;
    $("import-start").hidden = true;
    loadStorage();
    // One sentence per line: what stopped it, the counts, and Docker Desktop's check.
    const r = p.result || {};
    const lines = [];
    if (p.canceled) lines.push(t("import.cancelled"));
    if (p.error) lines.push(t("error.HB-IMP-002"));
    else lines.push(t("import.result", { imported: fmtInt(r.imported || 0), already: fmtInt(r.already || 0), refused: fmtInt(r.refused || 0), failed: fmtInt(r.failed || 0) }));
    const out = $("import-progress-text");
    clear(out);
    for (const l of lines) out.append(h("span", { class: "line", text: l }));
    if (r.sourceUnchanged) out.append(h("span", { class: "line", text: t("import.sourceUnchanged") }));
    else out.append(h("span", { class: "line" }, tp("import.sourceChanged", r.diffCount || 0), copyButton(r.diffRef)));
    if (p.error) showError(p.error, t("import.failed"));
  }

  // --- tabs (keyboard: arrows, Home, End) -------------------------------------------------
  const tabs = ["home", "containers", "storage", "settings", "help"];
  function showTab(name, focus) {
    if (tab === "containers" && name !== "containers") {
      // A log of up to 256 KiB stays in the document otherwise; it is fetched again
      // when a container is picked.
      selected = "";
      $("log").textContent = "";
      $("log-note").textContent = "";
    }
    tab = name;
    for (const n of tabs) {
      const b = $("tab-" + n);
      const on = n === name;
      b.setAttribute("aria-selected", String(on));
      b.tabIndex = on ? 0 : -1;
      $("view-" + n).hidden = !on;
      if (on && focus) b.focus();
    }
    if (name === "containers") { renderContainers(); refreshContainers(false); }
    if (name === "storage") loadStorage();
    if (name === "settings") { loadSettings(); loadDaemon(); }
    if (name === "help" && !$("doctor-list").firstChild) runDoctor();
    if (name === "home") refreshStatus(false);
  }
  function wireTabs() {
    for (const n of tabs) $("tab-" + n).addEventListener("click", () => showTab(n, false));
    $("tabs").addEventListener("keydown", (e) => {
      const i = tabs.indexOf(tab);
      let j = -1;
      if (e.key === "ArrowRight") j = (i + 1) % tabs.length;
      else if (e.key === "ArrowLeft") j = (i + tabs.length - 1) % tabs.length;
      else if (e.key === "Home") j = 0;
      else if (e.key === "End") j = tabs.length - 1;
      if (j >= 0) { e.preventDefault(); showTab(tabs[j], true); }
    });
  }

  // --- refresh: on focus and user action; a light timer only while visible ---------------
  // REQ-UX-011: nothing polls while the window is hidden or minimized; the host refuses a
  // timer's refresh then (HB-GUI-004) as well.
  let timer = 0;
  function schedule() {
    clearInterval(timer);
    timer = 0;
    if (document.visibilityState === "visible") timer = setInterval(() => refreshStatus(true), 5000);
  }
  document.addEventListener("visibilitychange", () => {
    schedule();
    if (document.visibilityState === "visible") refreshStatus(false);
  });
  function onEvent(m) {
    if (m.event === "activated" && document.visibilityState === "visible") refreshStatus(false);
    if (m.event === "import" && m.data) onImportProgress(m.data);
    // The window is closing during an import: the host stopped it and waits for its
    // cleanup before it closes.
    if (m.event === "closing") {
      $("import-cancel").disabled = true;
      $("import-progress-text").textContent = t("import.stopping");
      showInfo(t("import.stopping"), true);
    }
  }

  function wire() {
    wireTabs();
    $("toast-close").addEventListener("click", () => { $("toast").hidden = true; });
    $("toast-copy").addEventListener("click", (e) => copyEnglish(toastRef, e.currentTarget));
    $("error-copy").addEventListener("click", (e) => copyEnglishStatus(e.currentTarget));
    $("error-retry").addEventListener("click", (e) => engineAction("engine.start", e.currentTarget));
    $("error-diagnose").addEventListener("click", (e) => { showTab("help", true); runDiagnose($("diag-run")); void e; });
    $("error-help").addEventListener("click", () => showTab("help", true));
    $("ctr-refresh").addEventListener("click", () => refreshContainers(false));
    $("log-refresh").addEventListener("click", loadLogs);
    $("log-tail").addEventListener("change", loadLogs);
    $("set-banner-apply").addEventListener("click", async (e) => {
      const op = e.currentTarget.dataset.op;
      await engineAction(op, e.currentTarget);
      pendingRestart.clear();
      renderBanner();
    });
    $("daemon-validate").addEventListener("click", () => daemonValidate().catch(showError));
    $("daemon-save").addEventListener("click", () => daemonSave(true));
    $("daemon-apply").addEventListener("click", (e) => daemonApply(e.currentTarget));
    $("doctor-run").addEventListener("click", runDoctor);
    $("import-open").addEventListener("click", openImport);
    $("import-start").addEventListener("click", () => startImport(false));
    $("import-force").addEventListener("click", () => startImport(true));
    $("import-cancel").addEventListener("click", () => call("import.cancel").catch(showError));
    $("import").addEventListener("cancel", (e) => { if (importing) e.preventDefault(); });
    $("import-close").addEventListener("click", (e) => { if (importing) e.preventDefault(); });
    $("diag-run").addEventListener("click", (e) => runDiagnose(e.currentTarget));
    $("diag-reveal").addEventListener("click", () => call("diagnose.reveal").catch(showError));
    $("ladder-dockerd").addEventListener("click", (e) => engineAction("engine.restartDockerd", e.currentTarget));
    $("ladder-vm").addEventListener("click", (e) => engineAction("engine.restart", e.currentTarget));
    $("first-start").addEventListener("click", async (e) => {
      firstDismissed = true;
      render();
      showTab("home", false);
      await engineAction("engine.start", e.currentTarget);
    });
    $("first-settings").addEventListener("click", () => { firstDismissed = true; render(); showTab("settings", true); });
  }
  // The failed start's English original stays with the host, which gave it a ref.
  function copyEnglishStatus(btn) {
    if (status && status.error) copyEnglish(status.error.ref, btn);
  }

  // --- layout check (--check-layout, run with the pseudo-locale) --------------------------
  // REQ-UX-013: layouts leave room for text about 30% longer. Each screen is shown and
  // every visible element is checked: nothing may stick out of the window, clip its text,
  // or show a missing message key.
  function describe(el) {
    return el.tagName.toLowerCase() + (el.id ? "#" + el.id : "") + (el.className && typeof el.className === "string" ? "." + el.className.split(" ")[0] : "");
  }
  function layoutIssues(where) {
    const out = [];
    const vw = document.documentElement.clientWidth;
    if (document.documentElement.scrollWidth > vw + 1) out.push(where + ": the page scrolls sideways");
    for (const el of document.querySelectorAll("body *")) {
      if (el.closest("[hidden]") || el.closest("dialog:not([open])") || el.closest(".visually-hidden")) continue;
      const r = el.getBoundingClientRect();
      if (!r.width || !r.height) continue;
      if (r.right > vw + 1) out.push(where + ": " + describe(el) + " goes past the window edge");
      const cs = getComputedStyle(el);
      const scrolls = cs.overflowX === "auto" || cs.overflowX === "scroll";
      if (!scrolls && el.tagName !== "TEXTAREA" && el.tagName !== "SELECT" && el.tagName !== "INPUT" && el.scrollWidth > el.clientWidth + 1 && el.clientWidth > 0) {
        out.push(where + ": " + describe(el) + " clips its content");
      }
    }
    if (document.body.innerText.includes("⟦")) out.push(where + ": a missing message key is shown");
    return out;
  }
  async function checkLayout() {
    const wait = (ms) => new Promise((r) => setTimeout(r, ms));
    let issues = [];
    if (picking) {
      // The picker is checked as shown, then left without saving anything.
      issues = issues.concat(layoutIssues("language"));
      picking = false;
      $("lang").hidden = true;
    }
    await opened;
    render();
    if (!$("first").hidden) issues = issues.concat(layoutIssues("first-run"));
    firstDismissed = true;
    render();
    for (const n of tabs) {
      showTab(n, false);
      await wait(2500);
      issues = issues.concat(layoutIssues(n));
    }
    showTab("home", false);
    const text = issues.length ? "overflow " + issues.length + ": " + issues.slice(0, 20).join(" | ") : "ok";
    await call("layout", { text: text.slice(0, 2000) });
  }

  // --- start ----------------------------------------------------------------------------
  let opened = Promise.resolve();
  async function start() {
    INIT = await call("init");
    setLanguage(INIT.lang, INIT.messages);
    wire();
    if (INIT.pickLanguage) {
      // The picker is the first paint. The layout check does not answer it: it checks
      // it and goes on (checkLayout), with the rest opened meanwhile.
      const chosen = showPicker();
      firstPaint();
      if (INIT.checkLayout) { opened = openMain(); return; }
      await chosen;
    }
    opened = openMain();
    await opened;
    if (!INIT.pickLanguage) firstPaint();
  }
  async function openMain() {
    if (INIT.starting) engineBusy = "engine.start";
    try { status = await call("status", { auto: false }); } catch (e) { showError(e); }
    render();
    if (status && !status.firstStart) showTab("home", false);
    schedule();
    if (INIT.starting) {
      // The host started the engine (gui.startOnOpen); follow it until it is up.
      const until = Date.now() + 6 * 60 * 1000;
      const poll = setInterval(async () => {
        await refreshStatus(false);
        if (!status || status.phase === "running" || status.phase === "failed" || Date.now() > until) { engineBusy = ""; clearInterval(poll); renderHome(); }
      }, 5000);
    }
  }
  // firstPaint reports the first paint with content (the mascot decoded, then two
  // frames) and starts the layout check when asked.
  function firstPaint() {
    const img = $("hero-mascot");
    (img.decode ? img.decode() : Promise.resolve()).catch(() => {}).then(() =>
      requestAnimationFrame(() => requestAnimationFrame(() => {
        call("ready").catch(() => {});
        if (INIT.checkLayout) checkLayout().catch((e) => call("layout", { text: "error " + String(e && (e.message || e.code)) }));
      })));
  }
  start().catch((e) => { try { wv.postMessage({ id: nextId++, op: "log", args: { text: "start: " + String(e && (e.message || e.code)) } }); } catch (_) { /* nothing */ } });
})();
