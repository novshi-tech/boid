// Executes the actual captureSwapState/restoreSwapState/refreshHistoryHead
// source extracted from a rendered card detail page (by
// web_card_history_head_race_test.go) against a hand-built fake DOM, to
// reproduce two things a string/structural assertion cannot: a real async
// race, and a real splice-then-restore round trip.
//
// No real HTML parsing here — querySelector/querySelectorAll are stubbed to
// return pre-wired fake nodes for the exact selector strings the real
// source uses, so a selector typo is caught by
// TestCardDetail_LiveScript_HistoryHeadSelectorsMatchRenderedMarkup instead
// (a plain string mismatch there is cheaper and clearer than a stub miss
// here).

import { readFileSync } from "node:fs";

function makeNode(tag, id) {
	return {
		tagName: tag,
		id: id || "",
		attrs: {},
		parentNode: null,
		nextElementSibling: null,
		open: false,
		getAttribute(k) {
			return this.attrs[k];
		},
		closest(sel) {
			if (sel === "[id]") {
				let n = this;
				while (n) {
					if (n.id) return n;
					n = n.parentNode;
				}
				return null;
			}
			if (sel === "li") {
				let n = this;
				while (n) {
					if (n.tagName === "li") return n;
					n = n.parentNode;
				}
				return null;
			}
			return null;
		},
		querySelectorAll() {
			return [];
		},
		getBoundingClientRect() {
			return { top: 0 };
		},
	};
}

function makeList(children, stubs) {
	const list = {
		_children: children.slice(),
		get firstElementChild() {
			return this._children[0] || null;
		},
		querySelector(sel) {
			return (stubs.querySelector || {})[sel] || null;
		},
		querySelectorAll(sel) {
			return (stubs.querySelectorAll || {})[sel] || [];
		},
		removeChild(node) {
			const i = this._children.indexOf(node);
			if (i === -1) throw new Error("removeChild: stale node, not a current child");
			this._children.splice(i, 1);
			node.parentNode = null;
			return node;
		},
		insertBefore(node, ref) {
			const i = ref ? this._children.indexOf(ref) : this._children.length;
			if (ref && i === -1) throw new Error("insertBefore: stale reference node (BLOCKER 3 regression)");
			this._children.splice(i, 0, node);
			node.parentNode = this;
			return node;
		},
		getBoundingClientRect() {
			return { top: 0 };
		},
	};
	for (let i = 0; i < list._children.length; i++) {
		list._children[i].parentNode = list;
		list._children[i].nextElementSibling = list._children[i + 1] || null;
	}
	return list;
}

function runRaceScenario(src) {
	const itemA = makeNode("li", "card-item-summary-a");
	const itemB = makeNode("li", "card-item-summary-b");
	const boundaryLi = makeNode("li");
	const boundaryButton = makeNode("button");
	boundaryButton.attrs["hx-get"] = "/tasks/card-1/card-timeline?cursor=STALE-CURSOR&last_date=2026-01-02";
	boundaryButton.parentNode = boundaryLi;
	boundaryLi.closest = function (sel) {
		return sel === "li" ? boundaryLi : null;
	};

	const list = makeList([itemA, itemB, boundaryLi], {
		querySelector: { ".card-timeline-load-older button": boundaryButton },
		querySelectorAll: { "details[open]": [], details: [], ".card-timeline-date-sep": [] },
	});
	const container = {
		querySelector(sel) {
			if (sel === ".card-timeline-list") return list;
			return null;
		},
	};

	const fakeDocument = {
		getElementById(id) {
			return id === "card-timeline" ? container : null;
		},
		createElement(tag) {
			if (tag !== "template") throw new Error("unexpected createElement " + tag);
			return { content: { firstChild: null } };
		},
	};
	const fakeWindow = { location: { href: "http://localhost/tasks/card-1" } };

	// Simulates "Load older" completing (HTMX swaps the boundary <li> out of
	// the list) WHILE refreshHistoryHead()'s own fetch is still in flight —
	// the exact interleaving BLOCKER 3 identified.
	let fetchCalls = 0;
	let raceReplacement = null;
	const fakeFetch = () => {
		fetchCalls++;
		raceReplacement = makeNode("li");
		list.removeChild(boundaryLi);
		list.insertBefore(raceReplacement, null);
		return Promise.resolve({
			ok: true,
			text: () => Promise.resolve('<li id="card-item-summary-c"></li>'),
		});
	};

	const factory = new Function(
		"taskID",
		"document",
		"window",
		"URL",
		"fetch",
		src + "\nreturn { captureSwapState, restoreSwapState, refreshHistoryHead };",
	);
	const { refreshHistoryHead } = factory("card-1", fakeDocument, fakeWindow, URL, fakeFetch);

	refreshHistoryHead();
	if (fetchCalls !== 1) {
		throw new Error("expected exactly 1 fetch call, got " + fetchCalls);
	}

	return new Promise((resolve) => setTimeout(resolve, 20)).then(() => {
		const remaining = list._children.map((n) => n.id || "(boundary/new)");
		if (!list._children.includes(itemA) || !list._children.includes(itemB)) {
			throw new Error(
				"BLOCKER 3 regression: the list was emptied by a stale insertBefore reference after the boundary was detached mid-fetch. Remaining children: " +
					JSON.stringify(remaining),
			);
		}
		// The second symptom: the replacement control HTMX inserted must
		// survive too, or paging back is gone until a reload.
		if (!list._children.includes(raceReplacement)) {
			throw new Error(
				"BLOCKER 3 regression: the Load-older control HTMX swapped in was removed, so paging back is unreachable. Remaining children: " +
					JSON.stringify(remaining),
			);
		}
	});
}

// A history item's own <details> fold (e.g. a closed child's spec) must
// re-open after refreshHistoryHead()'s splice, the same way refresh()
// already does for #task-status/#task-pinned — BLOCKER 1's fix reuses
// captureSwapState/restoreSwapState around the splice instead of leaving it
// bare. No Load-older boundary in this scenario (an already-fully-loaded
// history list, matching the real "no button, no 'No more history.' li"
// state) — refreshHistoryHead's fetch-then-splice path replaces the whole
// list, exercising capture (before) / restore (after) around that.
function runDetailsPreservationScenario(src) {
	const oldItemWrapper = makeNode("div", "card-child-c1");
	const oldDetails = makeNode("details");
	oldDetails.className = "detail-children-spec";
	oldDetails.open = true;
	oldDetails.parentNode = oldItemWrapper;

	const newItemWrapper = makeNode("div", "card-child-c1");
	const newDetails = makeNode("details");
	newDetails.className = "detail-children-spec";
	newDetails.open = false;
	newDetails.parentNode = newItemWrapper;

	const itemLi = makeNode("li", "card-item-child-c1");
	const list = makeList([itemLi], {
		querySelector: {},
		querySelectorAll: {
			"details[open]": [oldDetails],
			details: [newDetails],
			".card-timeline-date-sep": [],
		},
	});
	const container = {
		querySelector(sel) {
			return sel === ".card-timeline-list" ? list : null;
		},
	};
	const fakeDocument = {
		getElementById(id) {
			return id === "card-timeline" ? container : null;
		},
		createElement(tag) {
			if (tag !== "template") throw new Error("unexpected createElement " + tag);
			return { content: { firstChild: null } };
		},
	};
	const fakeWindow = { location: { href: "http://localhost/tasks/card-1" } };
	const fakeFetch = () =>
		Promise.resolve({ ok: true, text: () => Promise.resolve("<li>ignored in this scenario</li>") });

	const factory = new Function(
		"taskID",
		"document",
		"window",
		"URL",
		"fetch",
		src + "\nreturn { captureSwapState, restoreSwapState, refreshHistoryHead };",
	);
	const { refreshHistoryHead } = factory("card-1", fakeDocument, fakeWindow, URL, fakeFetch);

	refreshHistoryHead();

	return new Promise((resolve) => setTimeout(resolve, 20)).then(() => {
		if (newDetails.open !== true) {
			throw new Error(
				"BLOCKER 1 regression: a history item's <details> that was open before refreshHistoryHead()'s splice did not re-open on the freshly-fetched item sharing its id",
			);
		}
	});
}

// A card with no history yet — "No history yet." rendered, no
// .card-timeline-list at all — must still pick up a child that closes
// after page load, instead of staying stuck until a manual reload.
function runBootstrapScenario(src) {
	let appendedUL = null;
	let emptyRemoved = false;
	const emptyPara = {
		remove() {
			emptyRemoved = true;
		},
	};
	const container = {
		querySelector(sel) {
			if (sel === ".card-timeline-list") return null;
			if (sel === ".tab-empty") return emptyPara;
			return null;
		},
		appendChild(node) {
			appendedUL = node;
		},
	};
	const fakeDocument = {
		getElementById(id) {
			return id === "card-timeline" ? container : null;
		},
		createElement(tag) {
			if (tag === "template") {
				const children = [makeNode("li", "card-item-summary-new")];
				return {
					content: {
						get firstChild() {
							return children.shift() || null;
						},
					},
				};
			}
			if (tag === "ul") {
				return {
					className: "",
					_children: [],
					appendChild(node) {
						this._children.push(node);
						node.parentNode = this;
					},
				};
			}
			throw new Error("unexpected createElement " + tag);
		},
	};
	const fakeWindow = { location: { href: "http://localhost/tasks/card-1" } };
	let fetchedURL = null;
	const fakeFetch = (url) => {
		fetchedURL = url;
		return Promise.resolve({ ok: true, text: () => Promise.resolve('<li id="card-item-summary-new"></li>') });
	};

	const factory = new Function(
		"taskID",
		"document",
		"window",
		"URL",
		"fetch",
		src + "\nreturn { refreshHistoryHead };",
	);
	const { refreshHistoryHead } = factory("card-1", fakeDocument, fakeWindow, URL, fakeFetch);

	refreshHistoryHead();

	return new Promise((resolve) => setTimeout(resolve, 20)).then(() => {
		if (!fetchedURL || !fetchedURL.includes("frontier=")) {
			throw new Error("bootstrap fetch missing a frontier= param: " + fetchedURL);
		}
		if (!appendedUL) {
			throw new Error(
				'nice-to-have 1 regression: no <ul class="card-timeline-list"> was appended when history had never loaded before',
			);
		}
		if (appendedUL.className !== "card-timeline-list") {
			throw new Error("appended list has the wrong className: " + appendedUL.className);
		}
		if (!emptyRemoved) {
			throw new Error('the "No history yet." placeholder was not removed');
		}
	});
}

// A successful empty history clears entries that moved to current work.
// A failed fetch preserves the last known history instead.
function runEmptyResponseScenario(src, failed = false) {
	const itemA = makeNode("li", "card-item-summary-a");
	const list = makeList([itemA], {
		querySelector: {},
		querySelectorAll: { "details[open]": [], details: [], ".card-timeline-date-sep": [] },
	});
	const container = {
		querySelector(sel) {
			return sel === ".card-timeline-list" ? list : null;
		},
	};
	const fakeDocument = {
		getElementById(id) {
			return id === "card-timeline" ? container : null;
		},
		createElement() {
			return { content: { firstChild: null }, innerHTML: "" };
		},
	};
	const fakeWindow = { location: { href: "http://localhost/tasks/card-1" } };
	const fakeFetch = () => Promise.resolve({ ok: !failed, text: () => Promise.resolve("") });

	const factory = new Function(
		"taskID",
		"document",
		"window",
		"URL",
		"fetch",
		src + "\nreturn { refreshHistoryHead };",
	);
	const { refreshHistoryHead } = factory("card-1", fakeDocument, fakeWindow, URL, fakeFetch);

	refreshHistoryHead();

	return new Promise((resolve) => setTimeout(resolve, 20)).then(() => {
		if (list._children.includes(itemA) !== failed) {
			throw new Error(failed ? "failed refresh removed history" : "empty history retained a stale entry");
		}
	});
}

const chunkPath = process.argv[2];
const src = readFileSync(chunkPath, "utf8");

Promise.resolve()
	.then(() => runRaceScenario(src))
	.then(() => runDetailsPreservationScenario(src))
	.then(() => runBootstrapScenario(src))
	.then(() => runEmptyResponseScenario(src))
	.then(() => runEmptyResponseScenario(src, true))
	.then(() => {
		console.log("OK");
		process.exit(0);
	})
	.catch((err) => {
		console.error("FAIL: " + err.message);
		process.exit(1);
	});
