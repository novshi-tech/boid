#!/usr/bin/env bash
# noop hook to satisfy the "executing requires a completed hook to advance to done"
# rule introduced when the state machine was simplified. The actual gate-driven
# subtask spawning happens in the exit gate.
exit 0
