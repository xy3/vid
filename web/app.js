"use strict";

/* vid.1t.ie client.
   - GET /api/me decides landing vs app.
   - Uploads are chunked (see upload.go): create a session, PUT ~8 MB slices,
     then finish. Chunking is what keeps each request under Cloudflare's 100 MB
     body limit.
   - The library re-fetches on a timer while anything is still transcoding. */

const $ = (sel, el = document) => el.querySelector(sel);
const cloneTpl = (id) => $("#" + id).content.firstElementChild.cloneNode(true);

const VIDEO_EXT = /\.(mp4|mov|m4v|webm|mkv|avi|wmv|flv|mpe?g|ts|3gp|ogv|mts|m2ts)$/i;

async function api(path, opts = {}) {
  const res = await fetch(path, opts);
  const ct = res.headers.get("content-type") || "";
  const data = ct.includes("application/json") ? await res.json().catch(() => ({})) : null;
  if (!res.ok) {
    const msg = (data && data.error) || res.statusText || "request failed";
    const err = new Error(msg);
    err.status = res.status;
    err.data = data;
    throw err;
  }
  return data;
}
const jsonInit = (method, body) => ({
  method,
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify(body),
});

function humanBytes(n) {
  if (!n) return "0 B";
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n : n.toFixed(1)) + " " + u[i];
}

function expiresText(expUnix) {
  if (!expUnix) return "kept indefinitely";
  const left = expUnix * 1000 - Date.now();
  if (left <= 0) return "expiring now";
  const min = left / 60000;
  if (min < 60) return "deletes in " + Math.ceil(min) + " min";
  const hrs = min / 60;
  if (hrs < 48) return "deletes in " + Math.ceil(hrs) + " h";
  return "deletes in " + Math.floor(hrs / 24) + " days";
}

/* ---------------- boot ---------------- */

let ME = null;
const main = $("#main");

boot();
async function boot() {
  try {
    ME = await api("/api/me");
  } catch (e) {
    main.innerHTML = '<div class="loading">Couldn\'t reach the server. Reload in a moment.</div>';
    return;
  }
  main.replaceChildren(ME.signed_in ? buildApp() : cloneTpl("tpl-landing"));
}

/* ---------------- app view ---------------- */

let pollTimer = null;

function buildApp() {
  const root = cloneTpl("tpl-app");

  // header identity
  const who = $("#who");
  who.hidden = false;
  if (ME.picture) $("#avatar").src = ME.picture;
  $("#email").textContent = ME.email || "";

  // retention <select> for new uploads
  const retSel = $("#retention", root);
  fillRetention(retSel, ME.default_retention);

  // drop targets
  const zone = $("#dropzone", root);
  const pick = $("#filepick", root);
  $("#browse", root).addEventListener("click", () => pick.click());
  pick.addEventListener("change", () => { takeFiles(pick.files, retSel.value); pick.value = ""; });

  zone.addEventListener("dragover", (e) => { e.preventDefault(); zone.classList.add("hot"); });
  zone.addEventListener("dragleave", () => zone.classList.remove("hot"));
  zone.addEventListener("drop", (e) => {
    e.preventDefault();
    zone.classList.remove("hot");
    takeFiles(e.dataTransfer.files, retSel.value);
  });

  // whole-window drag veil
  const veil = $("#dropveil");
  let depth = 0;
  window.addEventListener("dragenter", (e) => {
    if (![...(e.dataTransfer.types || [])].includes("Files")) return;
    depth++; veil.classList.add("show");
  });
  window.addEventListener("dragleave", () => { depth = Math.max(0, depth - 1); if (!depth) veil.classList.remove("show"); });
  window.addEventListener("dragover", (e) => e.preventDefault());
  window.addEventListener("drop", (e) => {
    e.preventDefault();
    depth = 0; veil.classList.remove("show");
    if (e.dataTransfer.files && e.dataTransfer.files.length) takeFiles(e.dataTransfer.files, retSel.value);
  });

  loadVideos();
  return root;
}

function fillRetention(sel, selected) {
  sel.replaceChildren();
  for (const opt of ME.retention_options) {
    const o = document.createElement("option");
    o.value = opt.value;
    o.textContent = opt.label;
    if (opt.value === selected) o.selected = true;
    sel.appendChild(o);
  }
}

/* ---------------- uploads ---------------- */

const queueEl = () => $("#queue");
let activeUploads = 0;

function takeFiles(fileList, retention) {
  const files = [...fileList].filter((f) => f.type.startsWith("video/") || f.type === "" || VIDEO_EXT.test(f.name));
  const skipped = fileList.length - files.length;
  if (skipped > 0) addNotice(skipped + " non-video file" + (skipped > 1 ? "s" : "") + " skipped");
  for (const f of files) uploadOne(f, retention);
}

function addNotice(text) {
  const q = queueEl();
  q.hidden = false;
  const row = document.createElement("div");
  row.className = "q";
  row.innerHTML = '<div class="q-top"><span class="q-name"></span><span class="q-stat">skipped</span></div>';
  $(".q-name", row).textContent = text;
  q.appendChild(row);
  setTimeout(() => row.remove(), 6000);
}

function makeQueueRow(name) {
  const q = queueEl();
  q.hidden = false;
  const row = document.createElement("div");
  row.className = "q";
  row.innerHTML =
    '<div class="q-top"><span class="q-name"></span><span class="q-stat">…</span></div>' +
    '<div class="track"><i></i></div>';
  $(".q-name", row).textContent = name;
  q.appendChild(row);
  return {
    set(pct, label, isErr) {
      $(".track i", row).style.width = Math.max(0, Math.min(100, pct)) + "%";
      const st = $(".q-stat", row);
      st.textContent = label;
      st.classList.toggle("err", !!isErr);
      if (pct >= 100 && !isErr) $(".track", row).classList.add("done");
    },
    done() { setTimeout(() => { row.remove(); if (!queueEl().children.length) queueEl().hidden = true; }, 1500); },
  };
}

const CHUNK_RETRIES = 3;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function uploadOne(file, retention) {
  const row = makeQueueRow(file.name);
  activeUploads++;
  try {
    const start = await api("/api/uploads", jsonInit("POST", {
      name: file.name, size: file.size, retention,
    }));
    const chunk = start.chunk_size || 8 << 20;
    let offset = 0;

    while (offset < file.size) {
      const end = Math.min(offset + chunk, file.size);
      const blob = file.slice(offset, end);
      let attempt = 0;
      for (;;) {
        try {
          const res = await fetch("/api/uploads/" + start.upload_id + "?offset=" + offset, {
            method: "PUT",
            headers: { "Content-Type": "application/octet-stream" },
            body: blob,
          });
          if (res.status === 409) {
            const j = await res.json().catch(() => ({}));
            offset = typeof j.expected === "number" ? j.expected : offset;
            break;
          }
          if (!res.ok) {
            const j = await res.json().catch(() => ({}));
            throw new Error(j.error || ("chunk " + res.status));
          }
          const j = await res.json();
          offset = j.received;
          break;
        } catch (e) {
          if (++attempt > CHUNK_RETRIES) throw e;
          await sleep(500 * attempt);
        }
      }
      row.set((offset / file.size) * 100, "uploading " + Math.floor((offset / file.size) * 100) + "%");
    }

    row.set(100, "processing…");
    await api("/api/uploads/" + start.upload_id + "/finish", jsonInit("POST", {}));
    row.done();
    loadVideos();
  } catch (e) {
    row.set(0, e.message || "failed", true);
  } finally {
    activeUploads--;
  }
}

/* ---------------- library ---------------- */

async function loadVideos() {
  let vids;
  try {
    vids = await api("/api/videos");
  } catch (e) {
    return;
  }
  const wrap = $("#videos");
  if (!wrap) return;
  wrap.replaceChildren(...vids.map(renderVideo));
  $("#count").textContent = vids.length ? vids.length + (vids.length === 1 ? " video" : " videos") : "";
  $("#empty").hidden = vids.length > 0;

  const pending = vids.some((v) => v.status === "queued" || v.status === "processing") || activeUploads > 0;
  clearTimeout(pollTimer);
  if (pending) pollTimer = setTimeout(loadVideos, 2500);
}

function renderVideo(v) {
  const el = cloneTpl("tpl-video");
  el.dataset.id = v.id;

  $(".v-name", el).textContent = v.name;

  const pill = $(".pill", el);
  pill.textContent = v.status;
  pill.classList.add(v.status);

  const img = $(".v-thumb img", el);
  const openLinks = el.querySelectorAll(".v-open, .v-open2");
  if (v.status === "ready") {
    img.src = "/t/" + v.id + ".jpg";
    openLinks.forEach((a) => (a.href = v.url));
  } else {
    img.removeAttribute("src");
    openLinks.forEach((a) => a.removeAttribute("href"));
  }

  const prog = $(".v-progress", el);
  if (v.status === "processing" || v.status === "queued") {
    prog.hidden = false;
    $(".v-fill", el).style.width = (v.progress || 0) + "%";
  }

  const meta = $(".v-meta", el);
  if (v.status === "ready") {
    let s = humanBytes(v.output_bytes);
    if (v.source_bytes > v.output_bytes && v.output_bytes > 0) {
      const pct = Math.round((1 - v.output_bytes / v.source_bytes) * 100);
      s += ' · <span class="saved">saved ' + pct + "%</span>";
    }
    s += " · " + expiresText(v.expires_at);
    meta.innerHTML = s;
  } else if (v.status === "failed") {
    meta.textContent = v.error || "processing failed";
  } else {
    meta.textContent = "compressing " + (v.progress || 0) + "%";
  }

  // actions
  const copy = $(".v-copy", el);
  copy.addEventListener("click", () => {
    const done = () => { copy.textContent = "Copied"; copy.classList.add("copied"); setTimeout(() => { copy.textContent = "Copy link"; copy.classList.remove("copied"); }, 1400); };
    (navigator.clipboard?.writeText(v.url) || Promise.reject()).then(done, () => window.prompt("Copy this link:", v.url));
  });

  const retWrap = $(".v-retwrap", el);
  const retSel = $(".v-ret", el);
  if (v.status === "failed") {
    retWrap.remove();
  } else {
    const ph = document.createElement("option");
    ph.textContent = "change…";
    ph.disabled = true;
    ph.selected = true;
    retSel.appendChild(ph);
    for (const opt of ME.retention_options) {
      const o = document.createElement("option");
      o.value = opt.value;
      o.textContent = opt.label;
      retSel.appendChild(o);
    }
    retSel.addEventListener("change", async () => {
      try {
        await api("/api/videos/" + v.id, jsonInit("POST", { retention: retSel.value }));
        loadVideos();
      } catch (e) { retSel.selectedIndex = 0; }
    });
  }

  $(".v-del", el).addEventListener("click", async () => {
    if (!window.confirm("Delete “" + v.name + "”? The share link stops working.")) return;
    try {
      await api("/api/videos/" + v.id, { method: "DELETE" });
      loadVideos();
    } catch (e) {}
  });

  return el;
}
