(function () {
  'use strict';
  var busy = false;

  function operationForm(form) {
    if (!form || form.tagName !== 'FORM' || form.method.toLowerCase() !== 'post') return false;
    var path = new URL(form.action, location.href).pathname;
    if (/^\/tasks\/[^/]+\/commands$/.test(path)) return true;
    if (/^\/tasks\/[^/]+\/action$/.test(path)) return form.elements.type && form.elements.type.value === 'go';
    return /^\/tasks\/[^/]+\/suggestion$/.test(path) && form.elements.verb && form.elements.verb.value === 'go' && form.elements.answer && form.elements.answer.value === 'accept';
  }

  function feedback(form, text, unknown) {
    var el = document.getElementById('operation-submit-feedback');
    if (!el) {
      el = document.createElement('div');
      el.id = 'operation-submit-feedback';
      el.className = 'operation-submit-feedback';
      el.setAttribute('role', 'status');
      el.setAttribute('aria-live', 'polite');
      el.tabIndex = -1;
    }
    // Keep feedback outside fragments that live updates replace.
    var anchor = document.getElementById('task-operations') || document.getElementById('task-live-status');
    if (anchor) anchor.insertAdjacentElement('afterend', el);
    else form.insertAdjacentElement('afterend', el);
    el.textContent = text;
    if (unknown) {
      var button = document.createElement('button');
      button.type = 'button';
      button.textContent = 'Refresh status';
      button.addEventListener('click', function () {
        var retry = document.getElementById('task-live-retry');
        if (retry) retry.click();
      });
      el.appendChild(document.createTextNode(' '));
      el.appendChild(button);
    }
    el.focus({ preventScroll: true });
    el.scrollIntoView({ block: 'nearest' });
    return el;
  }

  document.addEventListener('submit', async function (event) {
    var form = event.target;
    if (event.defaultPrevented || !operationForm(form)) return;
    event.preventDefault();
    if (busy) return;
    busy = true;
    // Capture submitter fields before disabling buttons; command keys live there.
    var data = new FormData(form);
    var instruction = form.elements.instruction;
    var sentInstruction = instruction ? instruction.value : null;
    if (event.submitter && event.submitter.name) data.append(event.submitter.name, event.submitter.value);
    var buttons = Array.from(form.querySelectorAll('button[type="submit"], input[type="submit"]'));
    var wasDisabled = buttons.map(function (button) { return button.disabled; });
    buttons.forEach(function (button) { button.disabled = true; });
    form.setAttribute('aria-busy', 'true');
    var notice = feedback(form, 'Sending…', false);
    var controller = new AbortController();
    var timer = setTimeout(function () { controller.abort(); }, 30000);
    try {
      var response = await fetch(form.action, {
        method: 'POST', body: new URLSearchParams(data), signal: controller.signal,
        credentials: 'same-origin', headers: { 'Accept': 'text/html' }
      });
      if (response.headers.get('X-Boid-Operation-Status') === 'not-submitted') {
        feedback(form, 'The operation was not submitted because its history could not be saved. Your input has been kept. Refresh the current state before trying again.', true);
        return;
      }
      var doc = new DOMParser().parseFromString(await response.text(), 'text/html');
      var result = doc.getElementById('task-operations');
      var target = document.getElementById('task-operations');
      var operationID = response.headers.get('X-Boid-Operation-ID') || new URL(response.url).searchParams.get('operation');
      var latest = result && operationID && result.querySelector('.operation-result[data-operation-id="' + CSS.escape(operationID) + '"]');
      if (!result || !target || !latest) throw new Error('No confirmed operation result');
      target.replaceWith(result);
      var outcome = latest.dataset.operationResult;
      if (instruction && instruction.value === sentInstruction && (outcome === 'accepted' || outcome === 'started')) {
        instruction.value = '';
      }
      notice.remove();
      latest.tabIndex = -1;
      latest.focus({ preventScroll: true });
      latest.scrollIntoView({ block: 'nearest' });
      document.dispatchEvent(new CustomEvent('boid:operation-result'));
    } catch (_) {
      feedback(form, 'The operation result could not be confirmed. Check the current status before trying again. Your input has been kept.', true);
    } finally {
      clearTimeout(timer);
      buttons.forEach(function (button, i) { button.disabled = wasDisabled[i]; });
      form.removeAttribute('aria-busy');
      busy = false;
    }
  });
})();
