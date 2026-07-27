#!/usr/bin/env bash
# workspace home 初期化スクリプト (リファレンス実装)
#
# docs/plans/home-workspace-volume.md の init.sh 契約に沿ったサンプル。
#
# 登録方法 (このファイルをコピー・編集した上で):
#   boid workspace set-init-script <slug> -f workspace-home-init.sh
#
# init.sh は daemon が保持している。 実体は daemon の
# ~/.config/boid/workspaces/<slug>/init.sh だが、これは **daemon 自身の**
# ファイルシステム上の path であり、container デプロイでは daemon の state volume
# の中なので、ホスト側の同名 path にコピーしても読まれない
# (PR9 / docs/plans/workspace-home-volume-persistence.md 論点 d)。
# 現在の内容は `boid workspace get-init-script <slug>`、
# その場で編集するなら `boid workspace edit-init-script <slug>`。
#
# なお 1 行目の shebang は **無視される**: boid は init.sh を exec せず、
# bytes を読んで使い捨て container 内の bash に渡す (下記「実行環境」)。
# ローカルで shellcheck / 直接実行するときのために残してある。
#
# 実行環境:
#   - boid が dispatch 前に起こす**使い捨て container** の中 (trusted)。
#     ホスト側での直接実行は PR5 (docs/plans/workspace-home-volume-persistence.md
#     論点c) で廃止された。 image は job container と同じ boid-runner base、
#     network は engine の既定 bridge (= 無制限 egress)
#   - HOME は workspace home に切替済み (以降の install はこの HOME 配下に着地する)。
#     しかもその path は **job が sandbox 内で見る $HOME と同一**なので、
#     $HOME 配下に絶対 path を焼き込んでも sandbox 内で壊れない
#     (PR5 以前はホスト側の実 path だったため絶対 symlink が dangling した —
#     下の link_sandbox_safe が存在する理由)
#   - env は **3 つだけ**: BOID_WORKSPACE_SLUG / BOID_WORKSPACE_HOME (= HOME) / HOME。
#     PATH は image の値。 ホストの PATH / USER / LOGNAME / LANG / LC_ALL / TERM は
#     **渡らない** (PR5 以前は渡っていた)
#   - 使えるコマンドは image が持っているもの (bash / coreutils / curl / git / gh 等)。
#     ホストにだけ入れてあるツールには到達できない
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
        # "in the init container", not "on host": since PR5 this script runs
        # inside boid's own runner image, so the fix is to add the tool to
        # build/container/Dockerfile (or install it from this script), not to
        # the machine the daemon happens to be running on.
        log "error: '$1' not found in the init container — add it to the boid runner image (build/container/Dockerfile) or install it here"
        exit 1
    fi
}

# sandbox-safe な symlink を張るヘルパー。
#
# 背景 (docs/plans/home-workspace-volume.md):
#   PR5 以前、このスクリプトは host 側で走り、 workspace HOME は
#   `~/.local/share/boid/homes/<slug>/` という host の実 path だった。
#   一方 sandbox 内ではそれが `/home/<user>` に bind mount 経由で見えるだけなので、
#   絶対 path で張った symlink は sandbox 内で dangling した。
#   PR5 (docs/plans/workspace-home-volume-persistence.md 論点c) で init が
#   container 実行になり、 `$HOME` は **job が見るのと同じ path** になったので
#   この不一致は解消している。 それでもこのヘルパーを残しているのは、
#   相対 symlink なら home ごと別の path へ移しても壊れないからで、
#   移行中の環境や将来の path 変更に対する保険として引き続き推奨される。
#   (PR6 で workspace HOME の実体は docker named volume
#   `boid-ws-home-<installID8>-<slug>` になった。 container 内から見える path は
#   変わっていないが、「home は別の入れ物に移りうる」という前提が現実になった。)
#
# $1: target (絶対 path でも相対 path でも可)
# $2: link path
link_sandbox_safe() {
    local target="$1" link="$2"
    local link_dir
    link_dir=$(dirname "$link")
    local rel="$target"
    case "$target" in
        "$HOME"/*)
            # `$HOME/foo/bar` を `link_dir` からの相対に置き換える。
            # `realpath --relative-to` は coreutils 8.23+ で使える。
            rel=$(realpath --relative-to="$link_dir" "$target")
            ;;
    esac
    ln -sfn "$rel" "$link"
}

# claude installer など、 制御外の installer が絶対 symlink を作った場合の
# 事後 relative 化。 対象 link が `$HOME/foo/bar` の絶対 symlink なら
# `link_dir` からの相対に張り替える。
relativize_symlink() {
    local link="$1"
    [ -L "$link" ] || return 0
    local target
    target=$(readlink "$link")
    case "$target" in
        "$HOME"/*)
            link_sandbox_safe "$target" "$link"
            log "rewrote $link -> $(readlink "$link") (was $target)"
            ;;
    esac
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
    link_sandbox_safe "$goroot/bin/go" "$HOME/.local/bin/go"
    link_sandbox_safe "$goroot/bin/gofmt" "$HOME/.local/bin/gofmt"
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
# 注意: volta の shim (`$VOLTA_HOME/bin/node`) は volta install 直後から
# symlink として存在するが、 default node が設定されるまで実行時に
# 「Node is not available」で失敗する。 単純な存在チェックや
# `volta which node` では判定できないので、 実際に `node --version` を
# 走らせて exit 0 で判定する。
install_node() {
    export VOLTA_HOME="$HOME/.volta"
    if [ -x "$VOLTA_HOME/bin/node" ] && "$VOLTA_HOME/bin/node" --version >/dev/null 2>&1; then
        log "node $("$VOLTA_HOME/bin/node" --version 2>/dev/null || echo unknown) already installed via volta"
        return 0
    fi
    log "installing node ${NODE_VERSION} via volta"
    "$VOLTA_HOME/bin/volta" install "node@${NODE_VERSION}"
}

# ---- Claude Code ------------------------------------------------------------
# Anthropic 公式インストーラ。~/.local/share/claude/versions/<X> に実バイナリ、
# ~/.local/bin/claude をシンボリックリンクとして張る。 installer は絶対 path
# で symlink を張るので、 sandbox 内で dangling しないように事後で relative
# 化する (link_sandbox_safe / relativize_symlink の doc 参照)。
install_claude() {
    local claude_bin="$HOME/.local/bin/claude"
    # 既存の symlink が dangling してる可能性があるので、 存在チェックだけでなく
    # 実行が通るかで判定する (`-x` は dangling symlink でも true になるケースがある)。
    if [ -e "$claude_bin" ] && "$claude_bin" --version >/dev/null 2>&1; then
        log "claude already installed ($($claude_bin --version 2>/dev/null || echo unknown))"
        relativize_symlink "$claude_bin"
        return 0
    fi
    log "installing claude code"
    curl -fsSL https://claude.ai/install.sh | bash
    relativize_symlink "$claude_bin"
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
# claude 同様、 installer が絶対 symlink を張る可能性があるので事後で relative 化。
install_opencode() {
    local opencode_bin_direct="$HOME/.opencode/bin/opencode"
    local opencode_bin_link="$HOME/.local/bin/opencode"
    if [ -e "$opencode_bin_direct" ] && "$opencode_bin_direct" --version >/dev/null 2>&1; then
        log "opencode already installed"
        relativize_symlink "$opencode_bin_link"
        return 0
    fi
    if [ -e "$opencode_bin_link" ] && "$opencode_bin_link" --version >/dev/null 2>&1; then
        log "opencode already installed"
        relativize_symlink "$opencode_bin_link"
        return 0
    fi
    log "installing opencode"
    curl -fsSL https://opencode.ai/install | bash
    relativize_symlink "$opencode_bin_link"
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
