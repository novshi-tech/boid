#!/usr/bin/env bash
# workspace home 初期化スクリプト (リファレンス実装)
#
# docs/plans/home-workspace-volume.md の init.sh 契約に沿ったサンプル。
# workspace 側 (~/.config/boid/workspaces/<slug>/init.sh) にコピーして使う。
#
# 実行環境:
#   - ホスト側 (trusted) で boid が dispatch 前に呼ぶ
#   - HOME は workspace home に切替済み (以降の install はこの HOME 配下に着地する)
#   - env: BOID_WORKSPACE_SLUG / BOID_WORKSPACE_HOME (= HOME) が入る
#
# 契約:
#   - 冪等 (完了マーカー破損 or script 更新での再実行に耐える)
#   - 失敗時は非ゼロ終了 (boid が dispatch を fail させる)
#   - 対話操作は不可 (認証等は `boid agent claude` セッション内で行う)

set -euo pipefail

# ---- pin (workspace 側で必要なら override) ---------------------------------
: "${GO_VERSION:=1.25.7}"
: "${GO_ARCH:=linux-amd64}"
: "${NODE_VERSION:=lts}"          # volta で管理する node
: "${CODEX_PACKAGE:=@openai/codex}"

# ---- helpers ----------------------------------------------------------------
log() { printf '[init %s] %s\n' "${BOID_WORKSPACE_SLUG:-?}" "$*" >&2; }

require() {
    if ! command -v "$1" >/dev/null 2>&1; then
        log "error: '$1' not found on host — install it before dispatch"
        exit 1
    fi
}

# ---- Go ---------------------------------------------------------------------
# 公式 tarball を $HOME/.local/share/go に展開し、$HOME/.local/bin にシンボリック
# リンクを張る (host の layout と同じ)。
install_go() {
    local goroot="$HOME/.local/share/go"
    local want="go${GO_VERSION}"

    if [ -x "$goroot/bin/go" ]; then
        local have
        have=$("$goroot/bin/go" env GOVERSION 2>/dev/null || echo unknown)
        if [ "$have" = "$want" ]; then
            log "go ${GO_VERSION} already installed"
            _link_go_bins "$goroot"
            return 0
        fi
        log "replacing go ${have} with ${want}"
    else
        log "installing go ${GO_VERSION}"
    fi

    local tmp
    tmp=$(mktemp -d)
    # RETURN trap: 成功でも失敗でも一時 dir を掃除
    # shellcheck disable=SC2064
    trap "rm -rf '$tmp'" RETURN

    curl -fSL "https://go.dev/dl/go${GO_VERSION}.${GO_ARCH}.tar.gz" -o "$tmp/go.tgz"
    tar -C "$tmp" -xzf "$tmp/go.tgz"    # $tmp/go/... を展開

    mkdir -p "$(dirname "$goroot")"
    rm -rf "$goroot.new"
    mv "$tmp/go" "$goroot.new"
    rm -rf "$goroot"
    mv "$goroot.new" "$goroot"

    _link_go_bins "$goroot"
}

_link_go_bins() {
    local goroot="$1"
    mkdir -p "$HOME/.local/bin"
    ln -sf "$goroot/bin/go" "$HOME/.local/bin/go"
    ln -sf "$goroot/bin/gofmt" "$HOME/.local/bin/gofmt"
}

# ---- Volta ------------------------------------------------------------------
# node の toolchain 管理。--skip-setup で shell profile (.bashrc 等) を触らせない。
install_volta() {
    export VOLTA_HOME="$HOME/.volta"
    if [ -x "$VOLTA_HOME/bin/volta" ]; then
        log "volta already installed"
        return 0
    fi
    log "installing volta"
    curl -fsSL https://get.volta.sh | bash -s -- --skip-setup
}

# ---- Node via Volta (codex / opencode の実行基盤) ---------------------------
install_node() {
    export VOLTA_HOME="$HOME/.volta"
    if "$VOLTA_HOME/bin/volta" which node >/dev/null 2>&1; then
        log "node $($VOLTA_HOME/bin/node --version 2>/dev/null || echo unknown) already installed via volta"
        return 0
    fi
    log "installing node ${NODE_VERSION} via volta"
    "$VOLTA_HOME/bin/volta" install "node@${NODE_VERSION}"
}

# ---- Claude Code ------------------------------------------------------------
# Anthropic 公式インストーラ。~/.local/share/claude/versions/<X> に展開し、
# ~/.local/bin/claude シンボリックリンクを作る (host の layout と同じ)。
install_claude() {
    if [ -x "$HOME/.local/bin/claude" ]; then
        log "claude already installed ($($HOME/.local/bin/claude --version 2>/dev/null || echo unknown))"
        return 0
    fi
    log "installing claude code"
    curl -fsSL https://claude.ai/install.sh | bash
}

# ---- Codex (OpenAI) ---------------------------------------------------------
# npm パッケージなので volta 経由で入れる (バイナリは $VOLTA_HOME/bin/codex)。
install_codex() {
    export VOLTA_HOME="$HOME/.volta"
    if [ -x "$VOLTA_HOME/bin/codex" ]; then
        log "codex already installed"
        return 0
    fi
    log "installing codex (${CODEX_PACKAGE}) via volta"
    "$VOLTA_HOME/bin/volta" install "$CODEX_PACKAGE"
}

# ---- OpenCode ---------------------------------------------------------------
# 公式インストーラ。~/.opencode/bin/opencode か ~/.local/bin/opencode に着地する。
install_opencode() {
    if [ -x "$HOME/.opencode/bin/opencode" ] || [ -x "$HOME/.local/bin/opencode" ]; then
        log "opencode already installed"
        return 0
    fi
    log "installing opencode"
    curl -fsSL https://opencode.ai/install | bash
}

# ---- main -------------------------------------------------------------------
require curl
require tar
require bash

mkdir -p "$HOME/.local/bin" "$HOME/.local/share"

install_go
install_volta
install_node
install_claude
install_codex
install_opencode

log "workspace home init complete"
