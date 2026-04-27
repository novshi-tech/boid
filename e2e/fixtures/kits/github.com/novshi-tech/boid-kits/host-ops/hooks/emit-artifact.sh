#!/usr/bin/env bash
set -euo pipefail

# broker 経由で hostcmd を呼ぶ (hook policy が Gate と同等であることの検証)
# 成果物 (artifact) は同 kit の host-ops gate が exclusive に書き込むため、
# このフックは payload_patch を出力しない (exclusive trait の二重書き衝突を避ける)。
fake-hook-cmd --hook-ran
