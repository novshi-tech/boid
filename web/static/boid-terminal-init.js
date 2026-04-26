// Loaded with ?v=<BuildID> from terminal.templ and forwards the same version
// to the boid-terminal.js import so both files share a single cache key per
// daemon restart.
const v = new URL(import.meta.url).searchParams.get('v') || '';
const suffix = v ? '?v=' + encodeURIComponent(v) : '';
const mod = await import('/static/boid-terminal.js' + suffix);
document.querySelectorAll('.boid-terminal[data-job-id]').forEach(function (root) {
  mod.initBoidTerminal(root, { jobId: root.dataset.jobId, wsUrl: root.dataset.wsUrl });
});
