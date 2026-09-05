(() => {
  "use strict";

  const POLL_MS = 2000;

  // DEFAULT_SCHEDULE pre-fills the schedule field for a new group so it is never
  // submitted empty; editing an existing group keeps that group's own value.
  const DEFAULT_SCHEDULE = "0 1 * * *";

  // serviceUnreachable tracks whether the last /healthz poll failed, so the
  // UI can show an "unreachable" state instead of stale data.
  let serviceUnreachable = false;
  // actionsAllowed mirrors /healthz's actionsAllowed: false when this client
  // sits outside the service's allowActionsFrom list, in which case every
  // control that would change something is hidden. Hiding is courtesy only -
  // the server refuses those requests whatever the page renders - so it
  // starts optimistic and is corrected by the first poll.
  let actionsAllowed = true;
  // dateLocale is the BCP-47 tag (e.g. "en-US", "de-DE") the server's config
  // sets for rendering run timestamps; refreshed from /healthz on every poll.
  let dateLocale = "en-US";
  const expandedHistoryRows = new Set();
  const groupsEl = document.getElementById("groups");
  const storageSummaryEl = document.getElementById("storage-summary");
  const detailEl = document.getElementById("detail");
  const detailNameEl = document.getElementById("detail-name");
  const detailConfigEl = document.getElementById("detail-config");
  const detailReposEl = document.getElementById("detail-repos");
  const detailLiveEl = document.getElementById("detail-live");
  const liveMetaEl = document.getElementById("live-meta");
  const liveTotalsEl = document.getElementById("live-totals");
  const liveBarEl = document.getElementById("live-bar");
  const liveRowsEl = document.getElementById("live-rows");
  const liveLogEl = document.getElementById("live-log");
  const historyRowsEl = document.getElementById("history-rows");
  const healthEl = document.getElementById("health");
  const readOnlyBadgeEl = document.getElementById("read-only-badge");
  const headerTitleEl = document.getElementById("header-title");
  const themeToggleEl = document.getElementById("theme-toggle");

  const newGroupEl = document.getElementById("new-group");
  const runAllEl = document.getElementById("run-all");
  const runAllLabelEl = document.getElementById("run-all-label");
  const openMenuEl = document.getElementById("open-menu");
  const actionsMenuEl = document.getElementById("actions-menu");
  const menuSepEl = document.getElementById("menu-sep");
  const detailEditEl = document.getElementById("detail-edit");
  const detailRunEl = document.getElementById("detail-run");
  const detailRunMsgEl = document.getElementById("detail-run-msg");
  const groupFormEl = document.getElementById("group-form");
  const groupFormFormEl = document.getElementById("group-form-el");
  const formTitleEl = document.getElementById("form-title");
  const formNameEl = document.getElementById("form-name");
  const formScheduleEl = document.getElementById("form-schedule");
  const formEnabledEl = document.getElementById("form-enabled");
  const formOutEl = document.getElementById("form-out");
  const formStageEl = document.getElementById("form-stage");
  const formReposEl = document.getElementById("form-repos");
  const formReposAddEl = document.getElementById("form-repos-add");
  const formErrorsEl = document.getElementById("form-errors");
  const schedulePresetEls = document.querySelectorAll(".schedule-presets [data-schedule]");
  const schedulePreviewEl = document.getElementById("schedule-preview");
  const formCloseEl = document.getElementById("form-close");
  const formCancelEl = document.getElementById("form-cancel");
  const formDeleteEl = document.getElementById("form-delete");
  const groupSaveEl = document.getElementById("group-save");
  const settingsSaveEl = document.getElementById("settings-save");

  const openRestoreEl = document.getElementById("open-restore");
  const restoreEl = document.getElementById("restore-form");
  const restoreFormEl = document.getElementById("restore-form-el");
  const restoreCloseEl = document.getElementById("restore-close");
  const restoreCancelEl = document.getElementById("restore-cancel");
  const restoreSubmitEl = document.getElementById("restore-submit");
  const restoreErrorsEl = document.getElementById("restore-errors");
  const restoreGroupEl = document.getElementById("restore-group");
  const restoreRepoEl = document.getElementById("restore-repo");
  const restoreArchiveEl = document.getElementById("restore-archive");
  const restoreDestEl = document.getElementById("restore-dest");
  const restoreOriginEl = document.getElementById("restore-origin");
  const restoreCloneEl = document.getElementById("restore-clone");
  const restoreForceEl = document.getElementById("restore-force");
  const restoreConfirmShallowEl = document.getElementById("restore-confirm-shallow");
  const restoreNoVerifyEl = document.getElementById("restore-no-verify");
  const restoreSkipChecksEl = document.getElementById("restore-skip-checks");
  const restoreLiveEl = document.getElementById("restore-live");
  const restoreLogEl = document.getElementById("restore-log");
  const restoreMetaEl = document.getElementById("restore-meta");
  const restoreBarEl = document.getElementById("restore-bar");
  const restoreResultEl = document.getElementById("restore-result");

  const openSettingsEl = document.getElementById("open-settings");
  const settingsEl = document.getElementById("settings-form");
  const settingsFormEl = document.getElementById("settings-form-el");
  const settingsCloseEl = document.getElementById("settings-close");
  const settingsCancelEl = document.getElementById("settings-cancel");
  const settingsErrorsEl = document.getElementById("settings-errors");
  const setTitleEl = document.getElementById("set-title");
  const setDateFormatEl = document.getElementById("set-date-format");
  const setListenEl = document.getElementById("set-listen");
  const setOutEl = document.getElementById("set-out");
  const setCloneDepthEl = document.getElementById("set-clone-depth");
  const setJobsEl = document.getElementById("set-jobs");
  const setRetriesEl = document.getElementById("set-retries");
  const setTimeoutEl = document.getElementById("set-timeout");
  const setHistoryLimitEl = document.getElementById("set-history-limit");
  const setFailFastEl = document.getElementById("set-fail-fast");
  const setSkipChecksEl = document.getElementById("set-skip-checks");

  let openGroup = null; // name of the group whose detail view is open, if any
  let lastDetail = null; // most recent /api/groups/:name response for openGroup, used to pre-fill the edit form
  let lastGroupsJson = null; // last-rendered /api/groups payload, so unchanged polls skip re-rendering the grid
  let lastDetailJson = null; // last-rendered detail+history payload for openGroup, so unchanged polls skip re-rendering
  let editingName = null; // null in create mode, the group's current name in edit mode
  let batchWasRunning = false; // true once this page has observed a "run all" batch in progress, so its ok/failed summary is only alerted once, on the transition to finished

  // setOverviewActionsVisible toggles the actions menu's "New group", "Run
  // all now", "Restore" and "Service settings" entries together, since all
  // four are refused server-side to a client outside allowActionsFrom - "Run
  // all now"/"Restore" because they change something, "Service settings"
  // because its GET response includes the listen address and backup root,
  // which is more than a LAN viewer needs.
  function setOverviewActionsVisible(visible) {
    visible = visible && actionsAllowed;
    newGroupEl.classList.toggle("hidden", !visible);
    runAllEl.classList.toggle("hidden", !visible);
    openRestoreEl.classList.toggle("hidden", !visible);
    openSettingsEl.classList.toggle("hidden", !visible);
    menuSepEl.classList.toggle("hidden", !visible);
  }

  // applyActionsAllowed hides every control that would change something when
  // this client is outside the service's allowActionsFrom list, and shows the
  // "read-only" badge that explains where they went.
  function applyActionsAllowed() {
    readOnlyBadgeEl.classList.toggle("hidden", actionsAllowed);
    detailEditEl.classList.toggle("hidden", !actionsAllowed);
    detailRunEl.classList.toggle("hidden", !actionsAllowed);
    groupSaveEl.classList.toggle("hidden", !actionsAllowed);
    settingsSaveEl.classList.toggle("hidden", !actionsAllowed);
    if (!actionsAllowed) setOverviewActionsVisible(false);
  }

  // setHeaderControlsVisible hides the three-dot actions menu button itself
  // while the service is unreachable (every entry needs a live service) or
  // this client is outside allowActionsFrom (every entry is now hidden or
  // refused anyway, so the button would only open an empty list).
  function setHeaderControlsVisible(visible) {
    if (!visible) setMenuOpen(false);
    openMenuEl.classList.toggle("hidden", !visible);
  }

  function setMenuOpen(open) {
    actionsMenuEl.classList.toggle("hidden", !open);
    openMenuEl.setAttribute("aria-expanded", open ? "true" : "false");
  }

  openMenuEl.addEventListener("click", (e) => {
    e.stopPropagation();
    setMenuOpen(actionsMenuEl.classList.contains("hidden"));
  });

  document.addEventListener("click", (e) => {
    if (!actionsMenuEl.contains(e.target)) setMenuOpen(false);
  });

  document.addEventListener("keydown", (e) => {
    if (e.key !== "Escape" || actionsMenuEl.classList.contains("hidden")) return;
    setMenuOpen(false);
    openMenuEl.focus();
  });

  // formIsOpen reports whether a full-page form has taken over the grid, so
  // the 2s poll does not put the overview actions back underneath it.
  function formIsOpen() {
    return !groupFormEl.classList.contains("hidden") ||
      !settingsEl.classList.contains("hidden") ||
      !restoreEl.classList.contains("hidden");
  }

  async function getJSON(url, opts) {
    const res = await fetch(url, opts);
    if (!res.ok) {
      const body = await res.json().catch(() => ({}));
      const err = new Error(body.error || `${res.status} ${res.statusText}`);
      err.status = res.status;
      err.fields = body.fields;
      throw err;
    }
    return res.status === 204 ? null : res.json();
  }

  function fmtTime(iso) {
    if (!iso || iso.startsWith("0001-01-01")) return "—";
    return new Date(iso).toLocaleString(dateLocale);
  }

  function fmtRelative(iso) {
    if (!iso || iso.startsWith("0001-01-01")) return "—";
    const ms = new Date(iso).getTime() - Date.now();
    const abs = Math.abs(ms);
    const mins = Math.round(abs / 60000);
    const when = mins < 1 ? "<1m" : mins < 60 ? `${mins}m` : `${Math.round(mins / 60)}h`;
    return ms >= 0 ? `in ${when}` : `${when} ago`;
  }

  // Decimal (SI) units, matching what GNOME Files, Finder and disk-property
  // dialogs show, so the "Disk usage" figure reads the same as the folder's
  // own properties rather than ~7% smaller (the 1024-vs-1000 gap).
  function fmtBytes(n) {
    if (!n) return "0 B";
    const units = ["B", "KB", "MB", "GB", "TB"];
    let i = 0, v = n;
    while (v >= 1000 && i < units.length - 1) { v /= 1000; i++; }
    return `${v < 10 && i > 0 ? v.toFixed(1) : Math.round(v)} ${units[i]}`;
  }

  function fmtDuration(startIso, endIso) {
    if (!startIso || !endIso || endIso.startsWith("0001-01-01")) return "—";
    const secs = Math.max(0, Math.round((new Date(endIso) - new Date(startIso)) / 1000));
    if (secs < 60) return `${secs}s`;
    if (secs < 3600) return `${Math.floor(secs / 60)}m${String(secs % 60).padStart(2, "0")}s`;
    return `${Math.floor(secs / 3600)}h${String(Math.floor((secs % 3600) / 60)).padStart(2, "0")}m`;
  }

  function fmtElapsedMs(ms) {
    const secs = Math.max(0, Math.round(ms / 1000));
    if (secs < 60) return `${secs}s`;
    if (secs < 3600) return `${Math.floor(secs / 60)}m${String(secs % 60).padStart(2, "0")}s`;
    return `${Math.floor(secs / 3600)}h${String(Math.floor((secs % 3600) / 60)).padStart(2, "0")}m`;
  }

  function fmtElapsed(startIso) {
    if (!startIso || startIso.startsWith("0001-01-01")) return "—";
    return fmtElapsedMs(Date.now() - new Date(startIso).getTime());
  }

  // updateElapsedTimes ticks every live repo row's elapsed-time badge each
  // second, independent of the 2s data poll, and flags one running far
  // longer than its currently-active peers - a heuristic (not exact: a
  // single active repo has nothing to compare against, so it is never
  // flagged), meant to catch a clone that looks stuck.
  function updateElapsedTimes() {
    const cells = Array.from(document.querySelectorAll("#live-rows .elapsed[data-started]"));
    if (cells.length === 0) return;
    const now = Date.now();
    const ms = cells.map((el) => now - new Date(el.dataset.started).getTime());
    const avg = ms.reduce((a, b) => a + b, 0) / ms.length;
    cells.forEach((el, i) => {
      el.textContent = fmtElapsedMs(ms[i]);
      el.classList.toggle("slow", cells.length > 1 && ms[i] > avg * 2.5 && ms[i] > 15000);
    });
  }

  // renderDiskBar renders a used/free split as a two-segment bar (red = used,
  // green = free), with a solid divider marking the boundary between them so
  // the split reads clearly even at a glance.
  function renderDiskBar(usedBytes, freeBytes) {
    const total = usedBytes + freeBytes || 1;
    const usedPct = Math.min(100, (usedBytes / total) * 100);
    return `
      <div class="disk-bar" title="${fmtBytes(usedBytes)} used · ${fmtBytes(freeBytes)} free">
        <div class="disk-bar-used" style="width:${usedPct}%"></div>
        <div class="disk-bar-free" style="width:${100 - usedPct}%"></div>
        <div class="disk-bar-divider" style="left:${usedPct}%"></div>
      </div>
      <div class="row muted small"><span>${fmtBytes(usedBytes)} used</span><span>${fmtBytes(freeBytes)} free</span></div>
    `;
  }

  // renderStorageSummary shows the one disk-usage figure the dashboard has:
  // the current on-disk size of the backup root (the config's top-level "out")
  // against the free space left on its volume. Nothing per-group is measured.
  // totalRepos is the sum of every configured group's repo count, shown
  // alongside the heading since this panel is the dashboard's summary strip.
  function renderStorageSummary(usedBytes, freeBytes, root, totalRepos) {
    if (freeBytes == null) {
      storageSummaryEl.innerHTML = "";
      return;
    }
    const label = root ? `<div class="muted small mono">${escapeHtml(root)}</div>` : "";
    const repoCount = totalRepos === 1 ? "1 repo" : `${totalRepos || 0} repos`;
    storageSummaryEl.innerHTML = `<h3>Disk usage <span class="muted small">${repoCount}</span></h3>${label}${renderDiskBar(usedBytes || 0, freeBytes)}`;
  }

  function renderGroups(groups) {
    groupsEl.innerHTML = "";
    for (const group of groups) {
      const card = document.createElement("div");
      card.className = "card";
      card.dataset.name = group.name;

      const stateBadge = group.running
        ? `<span class="badge running">running</span>`
        : `<span class="badge ${group.enabled ? "on" : "off"}">${group.enabled ? "enabled" : "disabled"}</span>`;

      const last = group.lastRun;
      const summaryLine = last
        ? `<span class="counts">
             <span class="ok">${last.ok} ok</span>${last.warn ? `<span class="warn">${last.warn} warn</span>` : ""}${last.failed ? `<span class="failed">${last.failed} failed</span>` : ""}
           </span>`
        : `<span class="muted">no runs yet</span>`;
      const repoCount = `<span class="muted"> · ${group.repoCount} repo${group.repoCount === 1 ? "" : "s"}</span>`;

      card.innerHTML = `
        <div class="card-head"><h3>${escapeHtml(group.name)}</h3>${stateBadge}</div>
        <div class="schedule"><svg width="13" height="13"><use href="#ic-clock"/></svg>${escapeHtml(group.schedule)}${repoCount}</div>
        <div class="row"><span class="muted">last run</span><span class="mono">${fmtTime(last ? last.started : null)}</span></div>
        <div class="row"><span class="muted">next run</span><span class="mono">${fmtTime(group.nextRun)}</span></div>
        <div class="row"><span class="muted">summary</span>${summaryLine}</div>
        <div class="card-actions">
          <button class="link details" type="button">Details<svg width="15" height="15"><use href="#ic-arrow"/></svg></button>
        </div>
      `;

      card.querySelector(".details").addEventListener("click", () => openDetail(group.name));

      groupsEl.appendChild(card);
    }
  }

  async function triggerRun(name, btn) {
    btn.disabled = true;
    btn.textContent = "Starting…";
    try {
      await getJSON(`/api/groups/${encodeURIComponent(name)}/run`, { method: "POST" });
    } catch (err) {
      if (err.status === 409) {
        detailRunMsgEl.textContent = "already running";
        detailRunMsgEl.classList.remove("hidden");
      } else {
        alert(`could not start ${name}: ${err.message}`);
      }
    }
    if (openGroup) refreshDetail(); else refreshGroups();
  }

  detailRunEl.addEventListener("click", () => {
    if (openGroup) triggerRun(openGroup, detailRunEl);
  });

  // runAllGroups just starts the batch server-side and lets refreshBatch
  // (polled on every tick, see below) reflect its progress - the sequencing
  // itself runs in the service, so it survives this page being closed.
  async function runAllGroups() {
    try {
      await getJSON("/api/run-all", { method: "POST" });
    } catch {
      // A 409 here just means a batch is already in progress (this page's
      // own click, another client's, or one resumed from before this page
      // loaded) - refreshBatch below still picks up its progress either way.
    }
    refreshBatch();
  }

  runAllEl.addEventListener("click", () => {
    setMenuOpen(false);
    runAllGroups();
  });

  // refreshBatch mirrors the server's "run all" progress onto the menu entry,
  // regardless of which view is currently open, so reopening the browser
  // mid-batch shows it on the very next poll rather than only after
  // starting a new one from this page.
  async function refreshBatch() {
    let status;
    try {
      status = await getJSON("/api/run-all");
    } catch {
      return;
    }

    runAllEl.disabled = status.running;
    runAllEl.classList.toggle("running", status.running);
    runAllLabelEl.textContent = status.running
      ? `Running ${Math.min(status.done + 1, status.total)}/${status.total}…`
      : "Run all now";

    if (status.running) {
      batchWasRunning = true;
      return;
    }
    if (!batchWasRunning) return;
    batchWasRunning = false;

    const problems = status.results.filter((r) => r.status !== "ok");
    if (problems.length) {
      alert(`some groups did not complete cleanly:\n${problems.map((r) => `${r.name}: ${r.status}${r.reason ? ` (${r.reason})` : ""}`).join("\n")}`);
    }
  }

  function renderRunButton(running) {
    detailRunMsgEl.classList.add("hidden");
    detailRunEl.disabled = running;
    detailRunEl.innerHTML = running
      ? "Running…"
      : `<svg width="9" height="9"><use href="#ic-play"/></svg>Run now`;
  }

  function openDetail(name) {
    openGroup = name;
    lastDetailJson = null;
    groupsEl.classList.add("hidden");
    groupFormEl.classList.add("hidden");
    settingsEl.classList.add("hidden");
    restoreEl.classList.add("hidden");
    detailEl.classList.remove("hidden");
    setOverviewActionsVisible(false);
    detailNameEl.textContent = name;
    renderRunButton(false);
    refreshDetail();
  }

  function showGroupsGrid() {
    openGroup = null;
    lastGroupsJson = null;
    detailEl.classList.add("hidden");
    groupFormEl.classList.add("hidden");
    settingsEl.classList.add("hidden");
    restoreEl.classList.add("hidden");
    groupsEl.classList.remove("hidden");
    setOverviewActionsVisible(true);
  }

  document.getElementById("detail-close").addEventListener("click", showGroupsGrid);

  function debounce(fn, ms) {
    let timer;
    return (...args) => {
      clearTimeout(timer);
      timer = setTimeout(() => fn(...args), ms);
    };
  }

  async function updateSchedulePreview() {
    const expr = formScheduleEl.value.trim();
    if (!expr) {
      schedulePreviewEl.textContent = "";
      schedulePreviewEl.classList.remove("error");
      formScheduleEl.removeAttribute("aria-invalid");
      return;
    }
    let data;
    try {
      data = await getJSON(`/api/cron/preview?expr=${encodeURIComponent(expr)}`);
    } catch {
      return; // network hiccup - leave the last preview in place, Save will still validate
    }
    if (data.valid) {
      schedulePreviewEl.textContent = `Next: ${data.nextRuns.map(fmtTime).join(", ")}`;
      schedulePreviewEl.classList.remove("error");
      formScheduleEl.removeAttribute("aria-invalid");
    } else {
      schedulePreviewEl.textContent = data.error;
      schedulePreviewEl.classList.add("error");
      formScheduleEl.setAttribute("aria-invalid", "true");
    }
  }

  const debouncedSchedulePreview = debounce(updateSchedulePreview, 300);
  formScheduleEl.addEventListener("input", debouncedSchedulePreview);
  schedulePresetEls.forEach((btn) => {
    btn.addEventListener("click", () => {
      formScheduleEl.value = btn.dataset.schedule;
      updateSchedulePreview();
    });
  });

  // A repo line prefixed with "#" is disabled: skipped by the scheduler and
  // by validation (repolist.ParseEntries), but kept in the list so its URL
  // survives a save. This marker is written and read only by this UI.
  function parseRepoLine(raw) {
    const trimmed = (raw || "").trim();
    const disabled = /^#\s*/.test(trimmed);
    return { url: trimmed.replace(/^#\s*/, ""), disabled };
  }

  function serializeRepoEntry(entry) {
    return entry.disabled ? `# ${entry.url}` : entry.url;
  }

  function makeRepoRow(entry) {
    const row = document.createElement("div");
    row.className = "repo-row" + (entry.disabled ? " disabled" : "");

    const checkbox = document.createElement("input");
    checkbox.type = "checkbox";
    checkbox.title = "Enable this repository";
    checkbox.checked = !entry.disabled;

    const input = document.createElement("input");
    input.type = "text";
    input.className = "repo-url";
    input.placeholder = "https://.../_git/repo  or  https://github.com/owner/repo";
    input.spellcheck = false;
    input.value = entry.url;

    checkbox.addEventListener("change", () => {
      row.classList.toggle("disabled", !checkbox.checked);
    });

    input.addEventListener("paste", (e) => {
      const text = (e.clipboardData || window.clipboardData).getData("text");
      if (!text || !text.includes("\n")) return;
      e.preventDefault();
      const lines = text.split("\n").map((s) => s.trim()).filter(Boolean);
      if (lines.length === 0) return;
      const first = parseRepoLine(lines[0]);
      input.value = first.url;
      checkbox.checked = !first.disabled;
      row.classList.toggle("disabled", first.disabled);
      let after = row;
      for (let i = 1; i < lines.length; i++) {
        const newRow = makeRepoRow(parseRepoLine(lines[i]));
        after.after(newRow);
        after = newRow;
      }
    });

    const removeBtn = document.createElement("button");
    removeBtn.type = "button";
    removeBtn.className = "repo-remove";
    removeBtn.setAttribute("aria-label", "Remove repository");
    removeBtn.textContent = "×";
    removeBtn.addEventListener("click", () => row.remove());

    row.append(checkbox, input, removeBtn);
    return row;
  }

  function renderRepoRows(entries) {
    formReposEl.innerHTML = "";
    entries.forEach((entry) => formReposEl.appendChild(makeRepoRow(entry)));
  }

  function collectRepoRows() {
    return Array.from(formReposEl.querySelectorAll(".repo-row")).map((row) => ({
      url: row.querySelector(".repo-url").value.trim(),
      disabled: !row.querySelector('input[type="checkbox"]').checked,
    }));
  }

  formReposAddEl.addEventListener("click", () => {
    const row = makeRepoRow({ url: "", disabled: false });
    formReposEl.appendChild(row);
    row.querySelector(".repo-url").focus();
  });

  function renderRepoView(el, repos) {
    el.innerHTML = "";
    (repos || []).forEach((raw) => {
      const { url, disabled } = parseRepoLine(raw);
      const row = document.createElement("div");
      row.className = "repo-row-view" + (disabled ? " disabled" : "");
      row.textContent = url;
      el.appendChild(row);
    });
  }

  function resetForm() {
    formNameEl.value = "";
    formScheduleEl.value = "";
    formEnabledEl.checked = true;
    formOutEl.value = "";
    formStageEl.value = "";
    renderRepoRows([]);
    renderFormErrors();
    schedulePreviewEl.textContent = "";
    schedulePreviewEl.classList.remove("error");
    formScheduleEl.removeAttribute("aria-invalid");
  }

  function openCreateForm() {
    editingName = null;
    formTitleEl.textContent = "New group";
    resetForm();
    formScheduleEl.value = DEFAULT_SCHEDULE;
    updateSchedulePreview();
    formDeleteEl.classList.add("hidden");
    groupsEl.classList.add("hidden");
    detailEl.classList.add("hidden");
    settingsEl.classList.add("hidden");
    restoreEl.classList.add("hidden");
    groupFormEl.classList.remove("hidden");
    setOverviewActionsVisible(false);
  }

  function openEditForm(name, detail) {
    editingName = name;
    formTitleEl.textContent = `Edit ${name}`;
    resetForm();
    formNameEl.value = name;
    formScheduleEl.value = detail.schedule || "";
    formEnabledEl.checked = !!detail.enabled;
    const cfg = detail.config || {};
    formOutEl.value = cfg.out || "";
    formStageEl.value = cfg.stage || "";
    renderRepoRows((cfg.repos || []).map(parseRepoLine));
    formDeleteEl.classList.remove("hidden");
    groupsEl.classList.add("hidden");
    detailEl.classList.add("hidden");
    settingsEl.classList.add("hidden");
    restoreEl.classList.add("hidden");
    groupFormEl.classList.remove("hidden");
    setOverviewActionsVisible(false);
    updateSchedulePreview();
  }

  function renderFormErrors(fields, message) {
    if ((!fields || fields.length === 0) && !message) {
      formErrorsEl.classList.add("hidden");
      formErrorsEl.innerHTML = "";
      return;
    }
    const items = (fields || [])
      .map((f) => `<li><span class="field-error">${escapeHtml(f.field)}</span>: ${escapeHtml(f.msg)}</li>`)
      .join("");
    formErrorsEl.innerHTML =
      (message ? `<div>${escapeHtml(message)}</div>` : "") + (items ? `<ul>${items}</ul>` : "");
    formErrorsEl.classList.remove("hidden");
  }

  newGroupEl.addEventListener("click", () => {
    setMenuOpen(false);
    openCreateForm();
  });
  detailEditEl.addEventListener("click", () => {
    if (openGroup && lastDetail) openEditForm(openGroup, lastDetail);
  });
  formCloseEl.addEventListener("click", showGroupsGrid);
  formCancelEl.addEventListener("click", showGroupsGrid);

  function renderSettingsErrors(fields, message) {
    if ((!fields || fields.length === 0) && !message) {
      settingsErrorsEl.classList.add("hidden");
      settingsErrorsEl.innerHTML = "";
      return;
    }
    const items = (fields || [])
      .map((f) => `<li><span class="field-error">${escapeHtml(f.field)}</span>: ${escapeHtml(f.msg)}</li>`)
      .join("");
    settingsErrorsEl.innerHTML =
      (message ? `<div>${escapeHtml(message)}</div>` : "") + (items ? `<ul>${items}</ul>` : "");
    settingsErrorsEl.classList.remove("hidden");
  }

  async function openSettingsForm() {
    let s;
    try {
      s = await getJSON("/api/settings");
    } catch (err) {
      alert(`could not load service settings: ${err.message}`);
      return;
    }
    const d = s.defaults || {};
    setTitleEl.value = s.title || "";
    setDateFormatEl.value = s.dateFormat || "";
    setListenEl.value = s.listen || "";
    setOutEl.value = s.outResolved || s.out || "";
    setCloneDepthEl.value = d.cloneDepth ?? 0;
    setJobsEl.value = d.jobs ?? 1;
    setRetriesEl.value = d.retries ?? 0;
    setTimeoutEl.value = d.timeout || "";
    setHistoryLimitEl.value = d.historyLimit ?? 1;
    setFailFastEl.checked = !!d.failFast;
    setSkipChecksEl.checked = !!d.skipChecks;
    renderSettingsErrors();

    openGroup = null;
    groupsEl.classList.add("hidden");
    detailEl.classList.add("hidden");
    groupFormEl.classList.add("hidden");
    restoreEl.classList.add("hidden");
    settingsEl.classList.remove("hidden");
    setOverviewActionsVisible(false);
  }

  openSettingsEl.addEventListener("click", () => {
    setMenuOpen(false);
    openSettingsForm();
  });
  settingsCloseEl.addEventListener("click", showGroupsGrid);
  settingsCancelEl.addEventListener("click", showGroupsGrid);

  function renderRestoreErrors(fields, message) {
    if ((!fields || fields.length === 0) && !message) {
      restoreErrorsEl.classList.add("hidden");
      restoreErrorsEl.innerHTML = "";
      return;
    }
    const items = (fields || [])
      .map((f) => `<li><span class="field-error">${escapeHtml(f.field)}</span>: ${escapeHtml(f.msg)}</li>`)
      .join("");
    restoreErrorsEl.innerHTML =
      (message ? `<div>${escapeHtml(message)}</div>` : "") + (items ? `<ul>${items}</ul>` : "");
    restoreErrorsEl.classList.remove("hidden");
  }

  // restoreArchives maps the repo <select>'s value back to the archive it was
  // built from, so the archive line can be shown without another round trip.
  let restoreArchives = [];

  // archiveKey is the value a repo <option> carries: project and repo joined
  // the same way the server's ArchiveInfo.Key does, so a repository name that
  // repeats across projects still selects unambiguously.
  function archiveKey(a) {
    return a.project ? `${a.project}/${a.repo}` : a.repo;
  }

  async function loadRestoreArchives(group) {
    restoreArchives = [];
    restoreRepoEl.innerHTML = "";
    restoreRepoEl.disabled = true;
    restoreArchiveEl.textContent = "";
    if (!group) return;

    try {
      restoreArchives = await getJSON(`/api/groups/${encodeURIComponent(group)}/archives`);
    } catch (err) {
      renderRestoreErrors(undefined, `could not list archives: ${err.message}`);
      return;
    }

    if (restoreArchives.length === 0) {
      restoreRepoEl.innerHTML = `<option value="">no archives in this group yet</option>`;
      return;
    }
    restoreRepoEl.innerHTML = restoreArchives
      .map((a) => `<option value="${escapeHtml(archiveKey(a))}">${escapeHtml(archiveKey(a))}</option>`)
      .join("");
    restoreRepoEl.disabled = false;
    renderRestoreArchive();
  }

  // renderRestoreArchive shows which archive the picked repository resolves
  // to: a group backs up into one directory, so this is simply what is there.
  function renderRestoreArchive() {
    const a = restoreArchives.find((x) => archiveKey(x) === restoreRepoEl.value);
    if (!a) {
      restoreArchiveEl.textContent = "";
      return;
    }
    restoreArchiveEl.innerHTML =
      `Archive: <span class="mono">${escapeHtml(a.path)}</span> · ` +
      `${fmtBytes(a.size)} · ${escapeHtml(fmtTime(a.modified))}`;
  }

  restoreGroupEl.addEventListener("change", () => loadRestoreArchives(restoreGroupEl.value));
  restoreRepoEl.addEventListener("change", renderRestoreArchive);

  async function openRestoreForm() {
    let groups;
    try {
      groups = await getJSON("/api/groups");
    } catch (err) {
      alert(`could not load groups: ${err.message}`);
      return;
    }

    restoreGroupEl.innerHTML = groups
      .map((g) => `<option value="${escapeHtml(g.name)}">${escapeHtml(g.name)}</option>`)
      .join("");
    renderRestoreErrors();
    restoreLiveEl.classList.add("hidden");
    restoreResultEl.innerHTML = "";

    openGroup = null;
    groupsEl.classList.add("hidden");
    detailEl.classList.add("hidden");
    groupFormEl.classList.add("hidden");
    settingsEl.classList.add("hidden");
    restoreEl.classList.remove("hidden");
    setOverviewActionsVisible(false);

    await loadRestoreArchives(restoreGroupEl.value);
    refreshRestore();
  }

  openRestoreEl.addEventListener("click", () => {
    setMenuOpen(false);
    openRestoreForm();
  });
  restoreCloseEl.addEventListener("click", showGroupsGrid);
  restoreCancelEl.addEventListener("click", showGroupsGrid);

  restoreFormEl.addEventListener("submit", async (e) => {
    e.preventDefault();
    const archive = restoreArchives.find((x) => archiveKey(x) === restoreRepoEl.value);
    if (!archive) {
      renderRestoreErrors([{ field: "repo", msg: "pick a repository" }]);
      return;
    }

    const body = {
      group: restoreGroupEl.value,
      project: archive.project || "",
      repo: archive.repo,
      dest: restoreDestEl.value.trim(),
      // Origin only applies with a working clone, and the server rejects
      // "bare" without one; keep the default whenever no clone is asked for.
      origin: restoreCloneEl.checked ? restoreOriginEl.value : "source",
      clone: restoreCloneEl.checked,
      force: restoreForceEl.checked,
      confirmShallow: restoreConfirmShallowEl.checked,
      noVerify: restoreNoVerifyEl.checked,
      skipChecks: restoreSkipChecksEl.checked,
    };

    renderRestoreErrors();
    try {
      const status = await getJSON("/api/restore", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      });
      renderRestore(status);
    } catch (err) {
      renderRestoreErrors(err.fields, err.fields && err.fields.length ? undefined : err.message);
    }
  });

  function renderRestore(status) {
    restoreSubmitEl.disabled = !!status.running;
    restoreSubmitEl.textContent = status.running ? "Restoring…" : "Restore";
    if (!status.started) {
      restoreLiveEl.classList.add("hidden");
      return;
    }
    restoreLiveEl.classList.remove("hidden");

    restoreBarEl.style.width = `${status.running ? status.pct || 0 : 100}%`;
    const meta = [`${escapeHtml(status.repo || "")}`];
    meta.push(status.running ? `running ${fmtElapsed(status.started)}` : `took ${fmtDuration(status.started, status.finished)}`);
    if (status.phase) meta.push(escapeHtml(status.phase));
    restoreMetaEl.innerHTML = meta.filter(Boolean).join(" · ") +
      (status.dest ? ` <span class="mono">→ ${escapeHtml(status.dest)}</span>` : "");

    if (!status.logTail || status.logTail.length === 0) {
      restoreLogEl.innerHTML = `<span class="muted">no output yet</span>`;
    } else {
      restoreLogEl.innerHTML = status.logTail.map((l) => {
        const level = l.level && l.level !== "info" ? ` ${l.level}` : "";
        return `<div class="line${level}">${escapeHtml(l.line)}</div>`;
      }).join("");
      restoreLogEl.scrollTop = restoreLogEl.scrollHeight;
    }

    if (status.err) {
      restoreResultEl.innerHTML = `<div class="failed">${escapeHtml(status.err)}</div>`;
      return;
    }
    const r = status.result;
    if (!r) {
      restoreResultEl.innerHTML = "";
      return;
    }
    const rows = [
      ["bare repository", r.barePath],
      ["working clone", r.cloned ? r.workPath : "—"],
      ["refs", String(r.refs)],
      ["origin", r.originUrl || "—"],
    ];
    if (r.lfsBytes) rows.push(["lfs restored", fmtBytes(r.lfsBytes)]);
    if (r.shallow) rows.push(["history", "truncated"]);
    if (r.warnings && r.warnings.length) rows.push(["warnings", r.warnings.join("; ")]);
    restoreResultEl.innerHTML = rows
      .map(([k, v]) => `<div class="row"><span class="muted">${escapeHtml(k)}</span><span class="mono">${escapeHtml(String(v))}</span></div>`)
      .join("");
  }

  async function refreshRestore() {
    try {
      renderRestore(await getJSON("/api/restore"));
    } catch {
      // A failed poll leaves the last rendered state alone; refreshHealth
      // already surfaces an unreachable service.
    }
  }

  settingsFormEl.addEventListener("submit", async (e) => {
    e.preventDefault();
    const body = {
      title: setTitleEl.value.trim(),
      dateFormat: setDateFormatEl.value.trim(),
      listen: setListenEl.value.trim(),
      defaults: {
        cloneDepth: Number(setCloneDepthEl.value),
        jobs: Number(setJobsEl.value),
        retries: Number(setRetriesEl.value),
        timeout: setTimeoutEl.value.trim(),
        failFast: setFailFastEl.checked,
        skipChecks: setSkipChecksEl.checked,
        historyLimit: Number(setHistoryLimitEl.value),
      },
    };

    try {
      await getJSON("/api/settings", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      });
      showGroupsGrid();
      refreshGroups();
      refreshHealth();
    } catch (err) {
      renderSettingsErrors(err.fields, err.fields && err.fields.length ? undefined : err.message);
    }
  });

  groupFormFormEl.addEventListener("submit", async (e) => {
    e.preventDefault();
    const body = {
      name: formNameEl.value.trim(),
      schedule: formScheduleEl.value.trim(),
      enabled: formEnabledEl.checked,
      out: formOutEl.value.trim(),
      stage: formStageEl.value.trim(),
      repos: collectRepoRows().filter((r) => r.url !== "").map(serializeRepoEntry),
    };

    const url = editingName ? `/api/groups/${encodeURIComponent(editingName)}` : "/api/groups";
    const method = editingName ? "PUT" : "POST";

    try {
      await getJSON(url, {
        method,
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      });
      showGroupsGrid();
      refreshGroups();
      refreshHealth();
    } catch (err) {
      renderFormErrors(err.fields, err.fields && err.fields.length ? undefined : err.message);
    }
  });

  formDeleteEl.addEventListener("click", async () => {
    if (!editingName) return;
    if (!confirm(`Delete group "${editingName}"? This cannot be undone.`)) return;
    try {
      await getJSON(`/api/groups/${encodeURIComponent(editingName)}`, { method: "DELETE" });
      showGroupsGrid();
      refreshGroups();
      refreshHealth();
    } catch (err) {
      alert(`could not delete ${editingName}: ${err.message}`);
    }
  });

  function renderConfig(detail) {
    const cfg = detail.config;
    if (!cfg) { detailConfigEl.innerHTML = ""; detailReposEl.innerHTML = ""; return; }
    const repos = cfg.repos || [];
    const enabledCount = repos.filter((r) => !parseRepoLine(r).disabled).length;
    const repoSummary = enabledCount === repos.length
      ? `${repos.length} repo(s)`
      : `${enabledCount} of ${repos.length} repo(s) enabled`;
    const rows = [
      ["schedule", escapeHtml(detail.schedule)],
      ["next run", fmtTime(detail.nextRun)],
      ["repositories", repoSummary],
      ["output dir", cfg.out || cfg.outResolved],
      ["concurrency", `${cfg.jobs} job(s)`],
      ["clone depth", cfg.cloneDepth > 0 ? String(cfg.cloneDepth) : "full history"],
      ["retries", String(cfg.retries)],
      ["timeout", cfg.timeout],
      ["fail fast", cfg.failFast ? "yes" : "no"],
      ["skip checks", cfg.skipChecks ? "yes" : "no"],
    ];
    if (cfg.stage) rows.splice(4, 0, ["stage dir", cfg.stage]);
    detailConfigEl.innerHTML = rows.map(([k, v]) => {
      return `<div class="row"><span class="muted">${escapeHtml(k)}</span><span class="mono">${escapeHtml(String(v))}</span></div>`;
    }).join("");
    renderRepoView(detailReposEl, repos);
  }


  // statusClass maps a finished repo's status to the color already used for
  // the same words elsewhere on the page (group cards, history table).
  function statusClass(status) {
    return status === "ok" || status === "warn" || status === "failed" || status === "skipped" ? status : "";
  }

  function renderLiveTotals(repos) {
    const counts = { ok: 0, warn: 0, failed: 0, skipped: 0 };
    let bytes = 0;
    for (const r of repos) {
      if (counts[r.status] !== undefined) counts[r.status]++;
      bytes += r.bytesDone || 0;
    }
    const parts = ["ok", "warn", "failed", "skipped"]
      .filter((k) => counts[k] > 0)
      .map((k) => `<span class="${k}">${counts[k]} ${k}</span>`);
    liveTotalsEl.innerHTML = (parts.length ? `<span class="counts">${parts.join("")}</span> · ` : "") +
      `${fmtBytes(bytes)} transferred so far`;
  }

  function renderLogTail(tail) {
    if (!tail || tail.length === 0) {
      liveLogEl.innerHTML = `<span class="muted">no output yet</span>`;
      return;
    }
    liveLogEl.innerHTML = tail.map((l) => {
      const repo = l.repo ? `<span class="repo">${escapeHtml(l.repo)}: </span>` : "";
      const level = l.level && l.level !== "info" ? ` ${l.level}` : "";
      return `<div class="line${level}">${repo}${escapeHtml(l.line)}</div>`;
    }).join("");
    liveLogEl.scrollTop = liveLogEl.scrollHeight;
  }

  function renderLive(live) {
    if (!live || !live.repos || live.repos.length === 0) {
      detailLiveEl.classList.add("hidden");
      return;
    }
    detailLiveEl.classList.remove("hidden");
    const pct = live.total > 0 ? Math.round((live.done / live.total) * 100) : 0;
    liveBarEl.style.width = `${pct}%`;

    const meta = [];
    if (live.started) meta.push(`running ${fmtElapsed(live.started)}`);
    if (live.jobs) meta.push(`${live.jobs} job(s)`);
    if (live.depth) meta.push(`depth ${live.depth}`);
    liveMetaEl.innerHTML = meta.join(" · ") + (live.out ? ` <span class="mono" title="${escapeHtml(live.out)}">→ ${escapeHtml(live.out)}</span>` : "");

    renderLiveTotals(live.repos);

    // live.repos is pre-sized to the full repo count, so a slot whose worker
    // hasn't picked it up yet is still a zero-valued entry - no project/repo,
    // no started time, no status. Those carry nothing worth showing, so they
    // are dropped rather than rendered as blank rows.
    const isStarted = (r) => r.started && !r.started.startsWith("0001-01-01");
    const visibleRepos = live.repos.filter((r) => r.status || isStarted(r));

    // In-progress repos (no final status yet) stay on top; completed ones sink
    // to the bottom, so the rows a user actually wants to watch don't scroll
    // out of view as more repos finish. Array.prototype.sort is stable, so
    // repos within each group keep their original relative order.
    const sortedRepos = visibleRepos.sort((a, b) => (a.status ? 1 : 0) - (b.status ? 1 : 0));

    liveRowsEl.innerHTML = sortedRepos.map((r) => {
      const phase = [r.phase || "", r.detail || ""].filter(Boolean).join(" ");
      const bytes = r.bytesTotal ? `<div class="muted small">${fmtBytes(r.bytesDone)} / ${fmtBytes(r.bytesTotal)}</div>` : "";
      const lastLine = r.lastLine ? `<div class="muted small" title="${escapeHtml(r.lastLine)}">${escapeHtml(r.lastLine)}</div>` : "";
      const active = !r.status;
      const elapsed = r.started && !r.started.startsWith("0001-01-01")
        ? active
          ? `<span class="elapsed" data-started="${r.started}"></span>`
          : `<span class="elapsed muted">took ${fmtElapsed(r.started)}</span>`
        : "";
      const status = r.retryAttempt
        ? `<span class="warn">retry ${r.retryAttempt}/${r.retryOf} in ${Math.round((r.retryWaitMs || 0) / 1000)}s: ${escapeHtml(r.retryReason || "")}</span>`
        : `<span class="${statusClass(r.status)}">${escapeHtml(r.status || "…")}</span>`;
      return `
        <tr>
          <td class="mono">${repoLabel(r.project, r.repo)} ${elapsed}${lastLine}</td>
          <td>${escapeHtml(phase)}${bytes}</td>
          <td>${r.pct || 0}%<div class="mini-bar"><div style="width:${r.pct || 0}%"></div></div></td>
          <td>${status}</td>
        </tr>
      `;
    }).join("");
    updateElapsedTimes();

    renderLogTail(live.logTail);
  }

  function renderHistory(history) {
    const ascending = [...history].reverse();
    historyRowsEl.innerHTML = ascending.map((r, i) => {
      const stuck = r.stuckDirs ? `<span class="badge warn-badge" title="${r.stuckDirs} stuck staging dir(s)">${r.stuckDirs} stuck</span>` : "";
      const expandable = r.repos && r.repos.length > 0;
      const rowId = `repo-detail-${i}`;
      const detailRow = expandable ? `
        <tr id="${rowId}" class="repo-detail${expandedHistoryRows.has(rowId) ? "" : " hidden"}">
          <td colspan="8">
            <table class="nested">
              <thead><tr><th>Repository</th><th>Status</th><th>Reason</th></tr></thead>
              <tbody>
                ${r.repos.map((ro) => `
                  <tr>
                    <td class="mono">${repoLabel(ro.project, ro.repo)}</td>
                    <td class="${ro.status === "failed" ? "failed" : "warn"}">${escapeHtml(ro.status)}</td>
                    <td>${escapeHtml(ro.reason || "")}</td>
                  </tr>
                `).join("")}
              </tbody>
            </table>
          </td>
        </tr>
      ` : "";
      return `
        <tr class="${expandable ? "expandable" : ""}" ${expandable ? `data-toggle="${rowId}"` : ""}>
          <td>${fmtTime(r.started)}</td>
          <td>${escapeHtml(r.trigger)}</td>
          <td>${fmtDuration(r.started, r.ended)}</td>
          <td class="ok">${r.ok}</td>
          <td class="warn">${r.warn || ""}</td>
          <td class="failed">${r.failed || ""}</td>
          <td>${fmtBytes(r.dirBytes)} ${stuck}</td>
          <td class="mono" title="${escapeHtml(r.logPath)}">${escapeHtml(r.err || r.out || "")}</td>
        </tr>
        ${detailRow}
      `;
    }).join("") || `<tr><td colspan="8" class="muted">no runs yet</td></tr>`;

    historyRowsEl.querySelectorAll("tr[data-toggle]").forEach((row) => {
      row.addEventListener("click", () => {
        const rowId = row.dataset.toggle;
        const hidden = document.getElementById(rowId).classList.toggle("hidden");
        if (hidden) expandedHistoryRows.delete(rowId); else expandedHistoryRows.add(rowId);
      });
    });
  }

  async function refreshDetail() {
    if (!openGroup) return;
    try {
      const [detail, history] = await Promise.all([
        getJSON(`/api/groups/${encodeURIComponent(openGroup)}`),
        getJSON(`/api/groups/${encodeURIComponent(openGroup)}/history`),
      ]);
      lastDetail = detail;
      const json = JSON.stringify([detail, history]);
      // Skip re-rendering when nothing changed: re-rendering on every poll
      // reflows the head row (detail-run-msg, detail-live), which can shift
      // "Edit"/"back" out from under an in-flight click.
      if (json === lastDetailJson) return;
      lastDetailJson = json;
      renderRunButton(detail.running);
      renderConfig(detail);
      renderLive(detail.live);
      renderHistory(history);
    } catch {
      historyRowsEl.innerHTML = "";
    }
  }

  async function refreshGroups() {
    try {
      const groups = await getJSON("/api/groups");
      const json = JSON.stringify(groups);
      // Skip re-rendering when nothing changed: renderGroups() rebuilds every
      // card (and its click listener) from scratch, so re-running it on an
      // unchanged poll can destroy the "Details" button under an in-flight click.
      if (json === lastGroupsJson) return;
      lastGroupsJson = json;
      renderGroups(groups);
    } catch {
      groupsEl.innerHTML = "";
      lastGroupsJson = null;
    }
  }

  async function refreshHealth() {
    try {
      const h = await getJSON("/healthz");
      serviceUnreachable = false;
      dateLocale = h.dateFormat || "en-US";
      actionsAllowed = h.actionsAllowed !== false;
      applyActionsAllowed();
      healthEl.classList.remove("unreachable");
      healthEl.innerHTML = `<span class="dot"></span>up ${escapeHtml(h.uptime)}`;
      headerTitleEl.textContent = h.title || "";
      headerTitleEl.classList.toggle("hidden", !h.title);
      document.title = h.title ? `clonezip service - ${h.title}` : "clonezip service";
      renderStorageSummary(h.storageBytes, h.freeBytes, h.backupRoot, h.totalRepos);
      setHeaderControlsVisible(actionsAllowed);
      if (!openGroup && !formIsOpen()) setOverviewActionsVisible(true);
    } catch {
      serviceUnreachable = true;
      healthEl.classList.add("unreachable");
      healthEl.innerHTML = `<span class="dot"></span>unreachable`;
      storageSummaryEl.innerHTML = "";
      if (openGroup || formIsOpen()) showGroupsGrid();
      groupsEl.innerHTML = "";
      lastGroupsJson = null;
      setOverviewActionsVisible(false);
      setHeaderControlsVisible(false);
    }
  }

  function effectiveTheme() {
    return document.documentElement.dataset.theme
      || (matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
  }

  function applyThemeIcon() {
    themeToggleEl.textContent = effectiveTheme() === "dark" ? "☀" : "☾";
  }

  themeToggleEl.addEventListener("click", () => {
    const next = effectiveTheme() === "dark" ? "light" : "dark";
    document.documentElement.dataset.theme = next;
    localStorage.setItem("theme", next);
    applyThemeIcon();
  });

  applyThemeIcon();

  function escapeHtml(s) {
    return String(s ?? "").replace(/[&<>"']/g, (c) => ({
      "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
    })[c]);
  }

  // repoLabel joins a project and repo as "project/repo" (Azure project, or
  // Bitbucket workspace); a github.com or codeberg.org entry has no project, so
  // it is just the repo name.
  function repoLabel(project, repo) {
    return escapeHtml(project ? `${project}/${repo}` : repo);
  }

  function tick() {
    refreshHealth();
    refreshBatch();
    if (!restoreEl.classList.contains("hidden")) refreshRestore();
    if (openGroup) {
      refreshDetail();
    } else {
      refreshGroups();
    }
  }

  tick();
  setInterval(tick, POLL_MS);
  // Live elapsed-time badges tick every second, independent of the coarser
  // data poll above.
  setInterval(updateElapsedTimes, 1000);
})();
