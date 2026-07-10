#!/usr/bin/env bash
set -euo pipefail

# This scenario must never actually run in a "run everything" (no scenario
# name given) invocation — its sibling `skip` marker file is supposed to
# exclude it entirely (see run.sh's scenario-collection loop and
# e2e/selftest/run_sh_skip_marker_test.sh, which asserts this exact string
# never appears in the captured output). Deliberately fails hard if it ever
# runs, so a regression here shows up as a real test failure rather than a
# silently-passing false negative.
e2e_fail "should-skip: this scenario ran despite having a skip marker — the skip mechanism is broken"
