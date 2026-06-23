// boid-paste-attach.js
//
// Wires the clipboard-paste-to-attachment UX for the task create form and the
// task ask answer form. When a user pastes an image (or one of the allowed
// text formats) into a textarea marked with `data-paste-attach`, the file is
// stashed in a hidden FileList that rides on the form's existing
// multipart/form-data submit, and a reference path is inserted at the
// caret position. The agent sees that path inside its sandbox at
// `~/.boid/attachments/<filename>` (the dispatcher binds the per-task
// attachments dir there).
//
// Keep the limits / extensions in sync with internal/api/attachments.go.
(function () {
    "use strict";

    var MAX_FILE_BYTES = 10 * 1024 * 1024;
    var MAX_TOTAL_BYTES = 30 * 1024 * 1024;
    var EXT_BY_MIME = {
        "image/png": ".png",
        "image/jpeg": ".jpg",
        "image/webp": ".webp",
        "text/plain": ".txt",
        "text/markdown": ".md",
        "application/json": ".json",
    };
    var ALLOWED_EXTS = [".png", ".jpg", ".jpeg", ".webp", ".txt", ".md", ".json", ".log"];

    // ASCII-only, regex-friendly basename. Server rejects anything outside
    // ^[A-Za-z0-9._-]+$ so we mirror that here.
    var SAFE_NAME_RE = /^[A-Za-z0-9._-]+$/;

    function init() {
        document.querySelectorAll("textarea[data-paste-attach]").forEach(function (ta) {
            attach(ta);
        });
    }

    function attach(textarea) {
        var form = textarea.form;
        if (!form) return;
        // Hidden file input keeps the pasted blobs around so the form's normal
        // submit (multipart/form-data) ships them as the "attachments" field.
        // Each paste appends to a DataTransfer that owns the FileList.
        var dt = new DataTransfer();
        var hidden = form.querySelector('input[type="file"][name="attachments"][data-paste-attach-stash]');
        if (!hidden) {
            hidden = document.createElement("input");
            hidden.type = "file";
            hidden.name = "attachments";
            hidden.multiple = true;
            hidden.style.display = "none";
            hidden.setAttribute("data-paste-attach-stash", "");
            form.appendChild(hidden);
        }

        var listEl = form.querySelector('[data-paste-attach-list]');
        var counter = 0;

        textarea.addEventListener("paste", function (e) {
            var items = e.clipboardData && e.clipboardData.items;
            if (!items || items.length === 0) return;
            var added = [];
            for (var i = 0; i < items.length; i++) {
                var item = items[i];
                if (item.kind !== "file") continue;
                var file = item.getAsFile();
                if (!file) continue;
                var ext = pickExt(file);
                if (!ext) {
                    notifyError(textarea, "貼り付けられたファイル形式は許可されていない: " + (file.type || "unknown"));
                    e.preventDefault();
                    return;
                }
                if (file.size > MAX_FILE_BYTES) {
                    notifyError(textarea, file.name + " は 10MB 上限を超えてる");
                    e.preventDefault();
                    return;
                }
                if (totalBytes(dt) + file.size > MAX_TOTAL_BYTES) {
                    notifyError(textarea, "合計サイズが 30MB 上限を超えるよ");
                    e.preventDefault();
                    return;
                }
                counter += 1;
                var name = buildName(file, ext, counter);
                var renamed = renameFile(file, name);
                dt.items.add(renamed);
                added.push(name);
            }
            if (added.length === 0) return;
            e.preventDefault();
            hidden.files = dt.files;
            var prefix = textarea.getAttribute("data-paste-attach-prefix") || "~/.boid/attachments/";
            insertAtCaret(textarea, added.map(function (n) { return prefix + n; }).join("\n"));
            added.forEach(function (n, idx) {
                renderListEntry(listEl, dt, n, hidden);
            });
        });
    }

    function totalBytes(dt) {
        var s = 0;
        for (var i = 0; i < dt.files.length; i++) s += dt.files[i].size;
        return s;
    }

    function pickExt(file) {
        var m = (file.type || "").toLowerCase();
        if (EXT_BY_MIME[m]) return EXT_BY_MIME[m];
        // Fall back to the file's own extension (textarea paste from a file
        // manager sometimes lacks an explicit MIME type for .log / .md).
        var nm = (file.name || "").toLowerCase();
        var dot = nm.lastIndexOf(".");
        if (dot < 0) return null;
        var ext = nm.substring(dot);
        if (ALLOWED_EXTS.indexOf(ext) >= 0) return ext;
        return null;
    }

    function buildName(file, ext, counter) {
        // Prefer the original filename when it survives sanitization (so a
        // user dragging "diagram.png" sees that name on the agent side). Fall
        // back to the pasted-<ts>-<i> pattern for clipboard images, which
        // arrive without a usable name.
        var orig = (file.name || "").trim();
        if (orig) {
            var base = orig.substring(0, orig.length - ext.length);
            // Slugify the base: keep allowed chars only.
            base = base.replace(/[^A-Za-z0-9._-]+/g, "-").replace(/^-+|-+$/g, "");
            if (base && SAFE_NAME_RE.test(base + ext)) {
                return base + ext;
            }
        }
        var ts = Date.now();
        return "pasted-" + ts + "-" + counter + ext;
    }

    function renameFile(file, name) {
        try {
            return new File([file], name, { type: file.type, lastModified: file.lastModified });
        } catch (e) {
            // Older browsers — fall back to a Blob wrapper; we lose the name
            // attribute but the upload still works.
            return file;
        }
    }

    function insertAtCaret(textarea, text) {
        var start = textarea.selectionStart;
        var end = textarea.selectionEnd;
        var before = textarea.value.substring(0, start);
        var after = textarea.value.substring(end);
        var insert = text;
        if (before && !/\s$/.test(before)) insert = "\n" + insert;
        if (after && !/^\s/.test(after)) insert = insert + "\n";
        textarea.value = before + insert + after;
        var pos = start + insert.length;
        textarea.selectionStart = pos;
        textarea.selectionEnd = pos;
        textarea.dispatchEvent(new Event("input", { bubbles: true }));
    }

    function renderListEntry(listEl, dt, name, hidden) {
        if (!listEl) return;
        var li = document.createElement("li");
        li.className = "paste-attach-list-item";
        var label = document.createElement("span");
        label.textContent = name;
        label.className = "paste-attach-list-name";
        var remove = document.createElement("button");
        remove.type = "button";
        remove.className = "paste-attach-list-remove";
        remove.textContent = "×";
        remove.setAttribute("aria-label", "Remove " + name);
        remove.addEventListener("click", function () {
            for (var i = 0; i < dt.files.length; i++) {
                if (dt.files[i].name === name) {
                    dt.items.remove(i);
                    break;
                }
            }
            hidden.files = dt.files;
            li.parentNode && li.parentNode.removeChild(li);
        });
        li.appendChild(label);
        li.appendChild(remove);
        listEl.appendChild(li);
    }

    function notifyError(textarea, msg) {
        // Lightweight feedback — alert is the lowest-friction signal in the
        // current Web UI (no toast system yet). Replace with a styled
        // notification when the UI grows one.
        // eslint-disable-next-line no-alert
        alert(msg);
        textarea.focus();
    }

    if (document.readyState === "loading") {
        document.addEventListener("DOMContentLoaded", init);
    } else {
        init();
    }
})();
