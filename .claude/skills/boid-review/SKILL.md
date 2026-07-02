---
name: boid-review
description: >
  Use when reviewing a boid PR or the current diff before merging, through the
  wiring-claim-and-test lens. Covers the classes that generic /code-review
  misses: (1) whether one end of a wiring seam was changed while the other end
  silently drifted out of sync — diffs touching adapter / Bindings / sandbox
  builder / spec hydrate / policy & builtin op / JobSpec / HarnessType
  propagation / session jsonl env strip / proxy & allowed_domains /
  embedded-skill bind / brokered git. (2) whether a claim such as "equivalent",
  "compatible", "passthrough", or "preserves the Phase N precondition" is
  actually backed by evidence in the diff. (3) whether new or changed behavior
  was shipped with no test exercising it and no stated reason for the absence.
  Reach for it on requests like "review this before merging", "check nothing in
  the wiring broke", "verify this compatible/equivalent claim is real", "look at
  the adapter/binding/HarnessType/allowed_domains PR", "it says it preserves the
  precondition — prove it from the diff", "worried the floor/bind got dropped",
  "did this feature ship without tests", "check the new op/handler has a test".
  Also the pre-merge check when integrating a child task's PR. Complements
  /code-review rather than replacing it.
---

# boid-review — pre-merge review for wiring, claims & tests

## What this skill catches

The Tier 0-2 mechanisms (govet / -race / arch guard / lint / integration tests) catch
**known** failure modes automatically. But one class is structurally beyond them:
**a change the author rewrote on purpose, whose justifying claim outruns what the diff
actually proves**. The compiler catches typos but never "is the author's claim true".
Lint sees shape but not "an as-yet-untested seam was rewritten under a wrong
'equivalent-to' claim".

This has actually happened. The 2026-06-29 binding regression (`d464581`, "add codex /
opencode adapter") slipped a change through `sandbox_builder.go` that **exclusively
replaced** the kit binding with the harness binding, justified as "equivalent to claude".
The three-part set: an intentional rewrite + a wrong claim + no test crossing that seam.
The 1-turn smoke passed; only another kit carrying `additional_bindings` died silently in
the crossfire. See the appendix of `docs/plans/quality-gates.md`.

**This skill is the reviewer lens for exactly that class** — and for its sibling, a feature
whose new behavior ships with no test and no stated reason (Lens 4), which the coverage
mechanisms don't enforce either. Both are dangers that live in the **absence** the diff
doesn't show, not in the lines it does. General bug / cleanup review is `/code-review`'s job
and this skill does not replace it. Run both before merging (order in Step 0 below).

## Step 0 — get the general review done first

Before applying the wiring and claim lenses, confirm the ordinary correctness review is
done. If it isn't, run `/code-review` first (effort medium–high depending on diff size) to
surface bugs and cleanups. This skill layers a second pass — wiring invariants, claim
discipline, and test-sync — on top of that. Don't mix the roles: general style nits are the
territory of lint and `/code-review`, and are not repeated here.

## Step 1 — gather the materials

1. **Get the diff.** For a PR, `gh pr diff <number>` (runs host-side — mind the sandbox
   gh/git quirks). For a local branch, `git diff origin/main...HEAD`.
2. **Extract the claims.** From the PR body + commit messages + code comments in the diff,
   list **verbatim** every assertion like "equivalent", "compatible", "passthrough",
   "unchanged", "Phase N precondition", "preserves …". These are the input to lens 2.
3. **Identify the seams touched.** Cross-reference the changed files against the catalog in
   `references/wiring-seams.md` and write down every seam that hits.
4. **List the new or changed behaviors.** For each, note whether the diff adds or updates a
   `_test.go` (or an e2e scenario) that exercises it. This is the input to lens 4.

If the materials can't be gathered (empty diff, unreadable PR body, etc.), report that
honestly. **Do not hand out a green GO with the evidence missing** — silence reads as
"covered".

## Lens 1 — wiring invariants

For each seam the diff touches (`references/wiring-seams.md`), a seam has **two or more
ends** and an **invariant**. Check that the diff kept both ends consistent, and that a
guard test covering them exists or was updated. The break is always the same shape:
**the author changed one end, the other end silently disagrees, and no test crosses both
ends**. Ask concretely:

> "This change touches end A of seam S. Is end B still consistent? Which test proves it?"

**Why it matters**: boid's fragile seams are **configuration wiring** that spans multiple
packages (hydrate → builder, op table → escape guard, etc.), and the compiler can't see
across that gap. A one-ended change compiles and passes the 1-turn smoke, but silently
drops the behavior for **the other end's case**. So distrust not "what changed" but "what
is claimed to be unchanged".

## Lens 2 — claim discipline

For each claim extracted in Step 1, demand that the diff **demonstrates the covered
scope**. Don't take a claim at face value. "Equivalent to claude" requires **an enumeration
of what must be equivalent** + **evidence each item still holds** (a test, or the relevant
lines in the diff). If the claim is broader than the proof, that is a NO-GO finding — which
is exactly the failure mechanism of `d464581`.

**Why it matters**: an equivalence claim is an assertion about the **complement** of the
diff — it declares "everything that must not change". A reviewer naturally looks at what
changed, but the danger hides on the side the author asserted "does not change" without
evidence. Same principle as "don't take a claim at face value".

## Lens 3 — memory / catalog sync

If the diff changed a wiring seam, the corresponding wiring-map memory **and** the relevant
entry in this skill's `references/wiring-seams.md` should be updated **in the same PR**.
Otherwise the catalog rots and the next reviewer trusts a stale invariant. Flag a diff that
changed a seam but didn't touch the corresponding doc (minor, but catch it).

## Lens 4 — test-sync (behavior shipped without a test)

The other lenses ask "is there a test?" only when a **catalog seam** was touched (Lens 1) or a
**claim** was made (Lens 2). A plain feature — a new builtin op and its handler, a new state
transition, a new API field with logic behind it — added with **no test and no stated reason**
slips past all of them. No other gate owns this hole: the coverage floors aren't enforced (CI's
`-coverprofile` only visualizes), `/code-review`'s charter is correctness + cleanup rather than
missing tests, and the TDD rule in `CLAUDE.md` isn't enforced at review time. This lens is that
gate — and it is the **same shape of absence** as Lens 1: the danger isn't in what the diff
shows, it's in the test that isn't there.

Scope it tightly — this is **not** a general coverage watchdog:

- **In scope**: new or changed *behavior* shipped with neither a test (new or existing) that
  exercises it nor a stated reason for the absence. Ask concretely:

  > "This diff adds/changes behavior B. Which test exercises B? If none, did the author say why?"

- **Out of scope** (never flag): pure refactors, renames, docs- or config-only changes, and
  trivial glue with no branching logic. If existing behavior stays covered by a test the diff
  updates, that counts as covered.
- **A stated reason is an acknowledgment only if it addresses coverage, not just local
  runnability.** Accept it when the author says *where* the behavior is exercised instead (an e2e
  scenario, an integration test) or why a unit test genuinely cannot exist. **Watch the common
  trap**: "this package imports sqlite, so it can't build in the sandbox" justifies not *running*
  the test locally — but CI runs `go test ./...` on a host runner, so `internal/db` and its
  dependents are still tested there; the test should be written and left for CI. "I can't run it
  here" is a deferral of verification to CI, **not** license to omit the test. Only when the
  reason truly accounts for the missing coverage is it an acknowledgment rather than a gap; a
  bare, silent omission always is.

**Why it matters**: the same principle as Lens 1 turned on the author's own change. A feature
compiles, passes the 1-turn smoke, and looks complete in the diff — while the behavior it added
has no regression guard at all. The next change that breaks it will do so invisibly. Report it
the way Lens 1 reports a missing guard: name the behavior, name the test that isn't there, one
sentence.

## Output format

A **GO / NO-GO** plus a list of evidence-backed findings. Each finding:

- `file:symbol` (function or type name, not a line number — lines rot)
- which lens (wiring / claim / sync / test-sync)
- the broken invariant or over-broad claim, in one sentence
- **the specific proof that is missing** (e.g. "no test for end B", "no evidence in the
  diff for equivalence item X")

Put NO-GO blockers first. If it's clean, **say so and list the seams you checked** (show
coverage, not silence). When you suspect a false positive, don't assert NO-GO — mark it
"needs confirmation" and give your reasoning, leaving room for a human maintainer to override.

## Growing the catalog

When you meet a new way a seam breaks (caught in review, or discovered after the fact), add
one entry to `references/wiring-seams.md`. That catalog is this skill's core asset — the
knowledge a generic reviewer doesn't have is concentrated there.
