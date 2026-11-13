"use strict";

const savedNoticeTime = 1600;
const copiedNoticeTime = 1800;
const reconnectDelay = 3000;
const scannerPage = document.body.dataset.page === "scanner";

async function post(url, values = {}) {
  try {
    const response = await fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body: new URLSearchParams(values),
    });
    if (response.status === 401) {
      location.href = "/signin";
      return { ok: false, message: "Please sign in again." };
    }
    const text = await response.text();
    try {
      return JSON.parse(text);
    } catch {
      return { ok: response.ok, message: text.trim() };
    }
  } catch {
    return { ok: false, message: "Scan Box can't be reached. Check the connection and try again." };
  }
}

function showMessage(text) {
  const message = document.querySelector("[data-message]");
  if (message) message.textContent = text;
}

// After a change, the Scanner page is redrawn by live updates; other pages load again.
function afterChange() {
  if (!scannerPage) location.reload();
}

// ---- Scanner page ----

function selectedColour() {
  const chosen = document.querySelector('[data-colour][aria-checked="true"]');
  return !chosen || chosen.dataset.colour === "true";
}

const scanActions = {
  one: colour => (colour ? "one_colour_page" : "one_grey_page"),
  add: colour => (colour ? "add_colour_page" : "add_grey_page"),
};

async function act(action) {
  const answer = await post("/api/action", { action });
  if (!answer.ok) showMessage(answer.message);
}

async function saveName(input) {
  const error = document.querySelector(`[data-error-for="${input.dataset.rename}"]`);
  const answer = await post(`/api/documents/${input.dataset.rename}/name`, { name: input.value });
  if (error) {
    error.hidden = answer.ok;
    error.textContent = answer.ok ? "" : answer.message;
  }
  if (answer.ok) input.defaultValue = input.value;
  return answer.ok;
}

// ---- Viewer: a scan's pages, large, with its name and Send next to them ----

const viewer = document.getElementById("viewer");

function viewerPart(name) {
  return viewer.querySelector(`[data-viewer-${name}]`);
}

function showPages(sources, captionFor, startAt) {
  const pages = viewerPart("pages");
  pages.replaceChildren(
    ...sources.map((source, index) => {
      const figure = document.createElement("figure");
      const picture = document.createElement("img");
      picture.src = source;
      picture.alt = captionFor(index + 1);
      picture.loading = "lazy";
      const caption = document.createElement("figcaption");
      caption.textContent = captionFor(index + 1);
      figure.append(picture, caption);
      return figure;
    }),
  );
  viewer.showModal();
  pages.scrollTop = 0;
  if (startAt > 1) pages.children[startAt - 1].scrollIntoView({ block: "start" });
}

function openViewer(trigger) {
  const { view: id, name, facts } = trigger.dataset;
  const count = Number(trigger.dataset.pages);
  const reviewing = trigger.dataset.reviewing === "true";
  const listInput = document.querySelector(`[data-rename="${id}"]`);
  viewer.dataset.id = id;
  viewerPart("naming").hidden = !reviewing;
  viewerPart("title").hidden = reviewing;
  viewerPart("title").textContent = name;
  viewerPart("send").hidden = !reviewing || trigger.dataset.canSend !== "true";
  viewerPart("error").hidden = true;
  viewerPart("facts").textContent = facts;
  viewerPart("pdf").hidden = false;
  viewerPart("pdf").href = `/documents/${id}.pdf`;
  viewerPart("download").hidden = false;
  viewerPart("download").href = `/documents/${id}/download`;
  const input = document.getElementById("viewer-name");
  input.value = input.defaultValue = listInput ? listInput.value : name;
  const sources = Array.from({ length: count }, (_, index) => `/documents/${id}/previews/${index + 1}.jpg`);
  showPages(sources, page => `Page ${page} of ${count}`, 1);
}

function openOpenDocumentViewer(trigger) {
  const count = Number(trigger.dataset.pages);
  viewer.dataset.id = "";
  viewerPart("naming").hidden = true;
  viewerPart("send").hidden = true;
  viewerPart("title").hidden = false;
  viewerPart("title").textContent = "Document in progress";
  viewerPart("facts").textContent = "";
  viewerPart("pdf").hidden = true;
  viewerPart("download").hidden = true;
  const sources = Array.from({ length: count }, (_, index) => `/open/previews/${index + 1}.jpg?v=${trigger.dataset.version}`);
  showPages(sources, page => `Page ${page} of ${count}`, Number(trigger.dataset.viewOpen));
}

async function saveViewerName() {
  const input = document.getElementById("viewer-name");
  if (input.value === input.defaultValue) return true;
  const answer = await post(`/api/documents/${viewer.dataset.id}/name`, { name: input.value });
  viewerPart("error").hidden = answer.ok;
  viewerPart("error").textContent = answer.ok ? "" : answer.message;
  if (!answer.ok) return false;
  input.defaultValue = input.value;
  const listInput = document.querySelector(`[data-rename="${viewer.dataset.id}"]`);
  if (listInput) listInput.value = listInput.defaultValue = input.value;
  return true;
}

if (viewer) {
  viewer.addEventListener("click", async event => {
    if (event.target === viewer || event.target.closest("[data-viewer-close]")) viewer.close();
    if (event.target.closest("[data-viewer-send]")) {
      if (!(await saveViewerName())) return;
      await post(`/api/documents/${viewer.dataset.id}/send`);
      viewer.close();
      afterChange();
    }
  });
  document.getElementById("viewer-name").addEventListener("change", saveViewerName);
  document.getElementById("viewer-name").addEventListener("keydown", event => {
    if (event.key === "Enter") saveViewerName();
  });
  viewer.addEventListener("close", () => viewerPart("pages").replaceChildren());
}

// ---- Edit: rename any scan ----

const editor = document.getElementById("editor");

function openEditor(trigger) {
  const input = document.getElementById("editor-name");
  editor.dataset.id = trigger.dataset.edit;
  input.value = input.defaultValue = trigger.dataset.name;
  editor.querySelector("[data-editor-note]").textContent = trigger.dataset.note;
  editor.querySelector("[data-editor-error]").hidden = true;
  editor.showModal();
  input.select();
}

if (editor) {
  editor.querySelector("[data-editor-form]").addEventListener("submit", async event => {
    event.preventDefault();
    const input = document.getElementById("editor-name");
    const error = editor.querySelector("[data-editor-error]");
    const answer = await post(`/api/documents/${editor.dataset.id}/name`, { name: input.value });
    error.hidden = answer.ok;
    error.textContent = answer.ok ? "" : answer.message;
    if (!answer.ok) return;
    editor.close();
    afterChange();
  });
  editor.querySelector("[data-editor-cancel]").addEventListener("click", () => editor.close());
}

// ---- Delete, after asking in the page's own dialog ----

const confirmation = document.getElementById("confirm");

function ask(title, text) {
  confirmation.querySelector("[data-confirm-title]").textContent = title;
  confirmation.querySelector("[data-confirm-text]").textContent = text;
  confirmation.querySelector("[data-confirm-error]").hidden = true;
  // Escape closes without an answer, so an earlier "yes" must not linger.
  confirmation.returnValue = "";
  confirmation.showModal();
  return new Promise(resolve => {
    confirmation.addEventListener("close", () => resolve(confirmation.returnValue === "yes"), { once: true });
  });
}

if (confirmation) {
  confirmation.addEventListener("click", event => {
    const answer = event.target.closest("[data-confirm-answer]");
    if (answer) confirmation.close(answer.dataset.confirmAnswer);
  });
}

async function deleteScan(trigger) {
  if (!(await ask(`Delete “${trigger.dataset.name}”?`, trigger.dataset.confirm))) return;
  const answer = await post(`/api/documents/${trigger.dataset.delete}/delete`);
  if (!answer.ok) {
    showMessage(answer.message);
    return;
  }
  afterChange();
}

document.addEventListener("click", event => {
  const trigger = event.target.closest("[data-view], [data-view-open]");
  if (!trigger || !viewer) return;
  event.preventDefault();
  if (trigger.dataset.view) openViewer(trigger);
  else openOpenDocumentViewer(trigger);
});

document.addEventListener("click", async event => {
  const target = event.target.closest("button");
  if (!target || target.closest("dialog")) return;

  if (target.dataset.colour) {
    target.parentElement.querySelectorAll("[data-colour]").forEach(other => other.setAttribute("aria-checked", other === target));
    await post("/api/settings", { field: "colour", value: target.dataset.colour });
  } else if (target.dataset.scan) {
    await act(scanActions[target.dataset.scan](selectedColour()));
  } else if (target.dataset.act) {
    await act(target.dataset.act);
  } else if (target.dataset.move) {
    const answer = await post(`/api/open/pages/${target.dataset.move}/move`, { direction: target.dataset.direction });
    if (!answer.ok) showMessage(answer.message);
  } else if (target.hasAttribute("data-remove-last")) {
    const answer = await post("/api/remove-last");
    if (!answer.ok) showMessage(answer.message);
  } else if (target.dataset.send) {
    const input = document.querySelector(`[data-rename="${target.dataset.send}"]`);
    if (input && input.value !== input.defaultValue && !(await saveName(input))) return;
    target.disabled = true;
    await post(`/api/documents/${target.dataset.send}/send`);
    afterChange();
  } else if (target.dataset.edit) {
    openEditor(target);
  } else if (target.dataset.delete) {
    await deleteScan(target);
  } else if (target.hasAttribute("data-send-now")) {
    await post("/api/send-waiting");
  } else if (target.dataset.retry) {
    target.disabled = true;
    const answer = await post(`/api/documents/${target.dataset.retry}/retry`);
    if (!answer.ok) showMessage(answer.message);
    afterChange();
  } else if (target.hasAttribute("data-test-paperless")) {
    await testPaperless(target);
  } else if (target.classList.contains("copy")) {
    await copySnippet(target);
  }
});

document.addEventListener("change", async event => {
  const input = event.target;
  if (input.dataset.rename) await saveName(input);
  if (input.dataset.field) await saveSetting(input);
});

document.addEventListener("keydown", event => {
  if (event.key === "Enter" && event.target.dataset && event.target.dataset.rename) event.target.blur();
});

// "/" goes to the archive's filter beside the page, unless someone is typing.
document.addEventListener("keydown", event => {
  const typing = event.target instanceof Element && event.target.closest("input, textarea, select, [contenteditable]");
  const search = document.querySelector("[data-archive-filter]");
  if (event.key !== "/" || typing || !search || search.offsetParent === null) return;
  event.preventDefault();
  search.focus();
  search.select();
});

// ---- Archive: filter, and keep what was open across live updates ----

const archiveFilter = document.querySelector("[data-archive-filter]");

function filterArchive() {
  const words = archiveFilter.value.toLowerCase().split(/\s+/).filter(Boolean);
  for (const item of document.querySelectorAll(".tree-doc, .tree-link")) {
    const text = (item.title + " " + item.textContent).toLowerCase();
    item.hidden = words.some(word => !text.includes(word));
  }
  if (!words.length) return;
  for (const node of document.querySelectorAll(".tree-node")) {
    const matches = node.querySelector(".tree-doc:not([hidden]), .tree-link:not([hidden])");
    node.hidden = !matches;
    if (matches) node.open = true;
  }
}

if (archiveFilter) {
  archiveFilter.addEventListener("input", () => {
    if (!archiveFilter.value) document.querySelectorAll(".tree-node[hidden]").forEach(node => (node.hidden = false));
    filterArchive();
  });
  archiveFilter.addEventListener("keydown", event => {
    if (event.key === "Enter" && archiveFilter.value.trim()) location.href = "/scans?q=" + encodeURIComponent(archiveFilter.value.trim());
  });
}

function openArchiveKeys(container) {
  return new Set([...container.querySelectorAll(".tree-node[open]")].map(node => node.dataset.key));
}

function restoreArchive(container, openKeys, scroll) {
  for (const node of container.querySelectorAll(".tree-node")) node.open = openKeys.has(node.dataset.key);
  const tree = container.querySelector(".tree");
  if (tree) tree.scrollTop = scroll;
  if (archiveFilter && archiveFilter.value) filterArchive();
}

// ---- Live updates: parts of every page are redrawn as things change. A part waits
// while someone is typing or choosing in it.

const pendingParts = {};

function editing(container) {
  const active = document.activeElement;
  return container.contains(active) && ["INPUT", "SELECT", "TEXTAREA"].includes(active.tagName);
}

function applyLive(parts) {
  for (const container of document.querySelectorAll("[data-live]")) {
    const name = container.dataset.live;
    if (!(name in parts)) continue;
    if (name === "review" && container.dataset.key === parts.reviewKey) continue;
    if (editing(container)) {
      pendingParts[name] = parts;
      continue;
    }
    if (name === "review") container.dataset.key = parts.reviewKey;
    if (name === "archive") {
      const openKeys = openArchiveKeys(container);
      const tree = container.querySelector(".tree");
      const scroll = tree ? tree.scrollTop : 0;
      container.innerHTML = parts[name];
      restoreArchive(container, openKeys, scroll);
      continue;
    }
    container.innerHTML = parts[name];
  }
}

document.addEventListener("focusout", () => {
  setTimeout(() => {
    for (const [name, parts] of Object.entries(pendingParts)) {
      delete pendingParts[name];
      applyLive({ [name]: parts[name], reviewKey: parts.reviewKey });
    }
  });
});

function listen() {
  const source = new EventSource("/live" + location.search);
  source.onmessage = event => applyLive(JSON.parse(event.data));
  source.onerror = () => {
    source.close();
    setTimeout(listen, reconnectDelay);
  };
}

if (document.querySelector("[data-live]")) listen();

// ---- Settings page ----

function noticeFor(input) {
  const field = input.closest(".field");
  const panel = input.closest(".panel");
  return {
    saved: (field && field.querySelector(".saved")) || (panel && panel.querySelector(".saved")),
    error: (field && field.querySelector("[data-error]")) || null,
  };
}

// After a date or time change, every example in the section is written again by the server.
async function refreshDateTime() {
  const response = await fetch("/api/date-time");
  if (!response.ok) return;
  const view = await response.json();
  for (const [field, options] of [["date_style", view.dates], ["time_style", view.times]]) {
    const menu = document.getElementById(field);
    options.forEach((choice, index) => (menu.options[index].textContent = choice.label));
  }
  document.querySelector("[data-zone-note]").textContent = view.zoneNote;
  for (const part of ["heading", "row", "recent", "name"]) {
    document.querySelector(`[data-preview="${part}"]`).textContent = view[part];
  }
}

// The server lists European zones; the browser adds every other zone it knows.
function addOtherZones() {
  const menu = document.getElementById("time_zone");
  if (!menu || !Intl.supportedValuesOf) return;
  const listed = new Set([...menu.options].map(choice => choice.value));
  const group = document.createElement("optgroup");
  group.label = "Everywhere else";
  for (const zone of Intl.supportedValuesOf("timeZone")) {
    if (listed.has(zone)) continue;
    const now = new Intl.DateTimeFormat("en-GB", { timeZone: zone, hour: "2-digit", minute: "2-digit", hourCycle: "h23" }).format(new Date());
    group.append(new Option(`${zone} (now ${now})`, zone));
  }
  menu.append(group);
}
addOtherZones();

async function saveSetting(input) {
  const value = input.type === "checkbox" ? String(input.checked) : input.value;
  const answer = await post("/api/settings", { field: input.dataset.field, value });
  const { saved, error } = noticeFor(input);
  if (error) {
    error.hidden = answer.ok;
    error.textContent = answer.ok ? "" : answer.message;
  }
  if (!answer.ok && !error) showMessage(answer.message);
  if (answer.ok && saved) {
    saved.classList.add("shown");
    setTimeout(() => saved.classList.remove("shown"), savedNoticeTime);
  }
  if (answer.ok && input.hasAttribute("data-date-time")) await refreshDateTime();
  return answer.ok;
}

// A typed address is saved first; one that is refused isn't tested, or the test would
// report on the address saved before it.
async function testPaperless(button) {
  const address = document.getElementById("paperless_url");
  if (address && address.value !== address.defaultValue && !(await saveSetting(address))) return;
  const result = document.querySelector("[data-test-result]");
  button.disabled = true;
  result.hidden = false;
  result.className = "result small muted";
  result.textContent = "Checking…";
  const answer = await post("/api/paperless/test");
  result.className = "result small " + (answer.ok ? "ok" : "error");
  result.textContent = answer.message;
  button.disabled = false;
}

// The browser allows the clipboard only on https or localhost; elsewhere the text is selected.
async function copySnippet(button) {
  const text = button.parentElement.querySelector("pre");
  try {
    await navigator.clipboard.writeText(text.textContent);
    button.textContent = "Copied";
  } catch {
    const range = document.createRange();
    range.selectNodeContents(text);
    getSelection().removeAllRanges();
    getSelection().addRange(range);
    button.textContent = "Press Ctrl+C";
  }
  setTimeout(() => (button.textContent = "Copy"), copiedNoticeTime);
}


// Search filters that are menus apply as soon as they change.
document.querySelectorAll("select[data-submit]").forEach(menu => menu.addEventListener("change", () => menu.form.submit()));
