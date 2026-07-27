package dispatcher

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// docs/examples/workspace-home-init.sh の helper に対する回帰テスト。
//
// あのファイルは「リファレンス実装」ではあるが、 実運用では各 workspace が
// ほぼそのままコピーして使う (docs/ja/guide/workspace-home.md) ので、 壊れると
// 実害が出る。 実際 2026-07-28 の dogfood で、 移行した home の
// node / npm / npx / codex / yarn / pnpm が sandbox 内で軒並み動かなくなった —
// volta が絶対 path で張った shim が旧 HOME を指したまま持ち込まれ、
// relativize_symlink は現 HOME prefix しか見ないので直せなかった。
//
// script は `BOID_INIT_SH_SOURCE_ONLY=1` を立てて source すると helper 定義だけ
// して main を走らせずに抜ける。 ここではそれを使い、 実際に bash で helper を
// 呼んで symlink の張り替え結果を見る (期待する挙動が shell の挙動そのものなので、
// Go 側で模写しても意味がない)。
const initScriptRelPath = "../../docs/examples/workspace-home-init.sh"

// runInitScriptHelper は init.sh の helper を読み込んだ上で body を bash で実行し、
// その stdout+stderr を返す。 HOME は tmp に差し替える。
func runInitScriptHelper(t *testing.T, home, body string) string {
	t.Helper()

	abs, err := filepath.Abs(initScriptRelPath)
	if err != nil {
		t.Fatalf("resolving %s: %v", initScriptRelPath, err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("the reference init.sh this test pins is missing: %v", err)
	}

	script := "set -euo pipefail\n" +
		"export BOID_INIT_SH_SOURCE_ONLY=1\n" +
		". '" + abs + "'\n" +
		body

	cmd := exec.Command("bash", "-c", script)
	// os.Environ() は継がない。 init container が script に渡す env は
	// BOID_WORKSPACE_SLUG / BOID_WORKSPACE_HOME / HOME の 3 つと image の PATH
	// だけ (script 冒頭の「実行環境」節) で、 それを模す。 実際これを継いでいた
	// 初版は、 開発機の VOLTA_HOME=$HOME/.volta を拾って tmp home ではなく
	// 実ユーザの home を見に行き、 テストが環境依存になっていた。
	cmd.Env = []string{
		"HOME=" + home,
		"BOID_WORKSPACE_HOME=" + home,
		"BOID_WORKSPACE_SLUG=inittest",
		"PATH=" + os.Getenv("PATH"),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper body failed: %v\n--- output ---\n%s", err, out)
	}
	return string(out)
}

// mustSymlink は target を指す symlink を作る (target の実在は問わない)。
func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", link, err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink %s -> %s: %v", link, target, err)
	}
}

// oldHomeTarget は「引っ越し前の HOME」に焼き込まれた絶対 path を模して返す。
//
// リテラルで書いてはいけない: このリポジトリの開発機には
// /run/user/<uid>/homes/default/.volta/bin/volta-shim も
// ~/.local/share/boid/homes/default/.volta/bin/volta-shim も **現に存在する**
// ので、 それを target にすると dangling のはずの link が解決してしまい、
// 「生きている link は触らない」の枝に落ちてテストが緑にならない
// (このテストの初版が実際にそうなった)。 t.TempDir() 配下の作っていない
// ディレクトリなら、 どのホストでも必ず解決しない。
func oldHomeTarget(t *testing.T, suffix string) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "old-home", suffix)
}

// mustFile は実体ファイルを作る。
func mustFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestInitScript_RehomeDanglingSymlink_RepointsAnOldHomeAbsoluteLink は本命の
// ケース: volta が旧 HOME の絶対 path で張った shim が、 現 HOME 配下の実体を
// 指す **相対** symlink に張り替わること。
//
// 相対であることまで見るのは、 絶対に戻すと次の引っ越しで同じことが起きるから
// (home は host dir → tmpfs → named volume と 3 回 path が変わっている)。
func TestInitScript_RehomeDanglingSymlink_RepointsAnOldHomeAbsoluteLink(t *testing.T) {
	home := t.TempDir()
	mustFile(t, filepath.Join(home, ".volta/bin/volta-shim"))
	link := filepath.Join(home, ".volta/bin/node")
	// 旧 HOME を焼き込んだ shim。 実機では
	// `/run/user/1000/homes/default/.volta/bin/volta-shim` の形だった
	// (2026-07-28 dogfood)。
	mustSymlink(t, oldHomeTarget(t, ".volta/bin/volta-shim"), link)

	runInitScriptHelper(t, home, "rehome_dangling_symlink \"$HOME/.volta/bin/node\"")

	got, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink after rehome: %v", err)
	}
	if got != "volta-shim" {
		t.Errorf("node shim -> %q, want %q (relative to its own directory)", got, "volta-shim")
	}
	if _, err := os.Stat(link); err != nil {
		t.Errorf("the rehomed link still does not resolve: %v", err)
	}
}

// TestInitScript_RehomeDanglingSymlink_LeavesResolvableLinksAlone: 生きている
// link に触ると、 意図して張られた relative link を壊しかねない。
func TestInitScript_RehomeDanglingSymlink_LeavesResolvableLinksAlone(t *testing.T) {
	home := t.TempDir()
	mustFile(t, filepath.Join(home, ".local/share/claude/versions/2.1.220"))
	link := filepath.Join(home, ".local/bin/claude")
	mustSymlink(t, "../share/claude/versions/2.1.220", link)

	runInitScriptHelper(t, home, "rehome_dangling_symlink \"$HOME/.local/bin/claude\"")

	got, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if got != "../share/claude/versions/2.1.220" {
		t.Errorf("a resolvable link was rewritten to %q; it must be left alone", got)
	}
}

// TestInitScript_RehomeDanglingSymlink_LeavesRelativeDanglingLinksAlone:
// 相対 dangling は「引っ越しで壊れた」のではなく単に対象が無いだけなので、
// suffix 探索で別物に繋ぎ替えると害のほうが大きい。
func TestInitScript_RehomeDanglingSymlink_LeavesRelativeDanglingLinksAlone(t *testing.T) {
	home := t.TempDir()
	// 探索が走ってしまうと当たってしまう囮を置く。
	mustFile(t, filepath.Join(home, "bin/tool"))
	link := filepath.Join(home, ".local/bin/tool")
	mustSymlink(t, "../../bin/tool-that-is-gone", link)

	runInitScriptHelper(t, home, "rehome_dangling_symlink \"$HOME/.local/bin/tool\"")

	got, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if got != "../../bin/tool-that-is-gone" {
		t.Errorf("a relative dangling link was rewritten to %q; it must be left alone", got)
	}
}

// TestInitScript_RehomeDanglingSymlink_LeavesUnmatchedLinksAlone: 直せないものを
// 適当な別物に繋ぎ替えない。 warning は出す (診断のため)。
func TestInitScript_RehomeDanglingSymlink_LeavesUnmatchedLinksAlone(t *testing.T) {
	home := t.TempDir()
	link := filepath.Join(home, ".local/bin/nope")
	target := oldHomeTarget(t, "nope")
	mustSymlink(t, target, link)

	out := runInitScriptHelper(t, home, "rehome_dangling_symlink \"$HOME/.local/bin/nope\"")

	got, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if got != target {
		t.Errorf("an unmatchable link was rewritten to %q; it must be left alone", got)
	}
	if !strings.Contains(out, "dangling") {
		t.Errorf("no warning was logged for an unmatchable dangling link; output was:\n%s", out)
	}
}

// TestInitScript_RepairVoltaShims_FixesEveryShimInOneGo: dogfood で壊れていたのは
// node だけではなく shim 一式 (node/npm/npx/codex/yarn/pnpm)。 1 本ずつ直す口が
// あるだけでは足りないので、 まとめて直る側を pin する。
func TestInitScript_RepairVoltaShims_FixesEveryShimInOneGo(t *testing.T) {
	home := t.TempDir()
	mustFile(t, filepath.Join(home, ".volta/bin/volta-shim"))
	// volta 本体は symlink ではなく実体。 触られないことも確かめる。
	mustFile(t, filepath.Join(home, ".volta/bin/volta"))

	shims := []string{"node", "npm", "npx", "codex", "yarn", "pnpm"}
	oldShim := oldHomeTarget(t, ".volta/bin/volta-shim")
	for _, name := range shims {
		mustSymlink(t, oldShim, filepath.Join(home, ".volta/bin", name))
	}

	runInitScriptHelper(t, home, "repair_volta_shims")

	for _, name := range shims {
		link := filepath.Join(home, ".volta/bin", name)
		got, err := os.Readlink(link)
		if err != nil {
			t.Errorf("readlink %s: %v", name, err)
			continue
		}
		if got != "volta-shim" {
			t.Errorf("shim %s -> %q, want %q", name, got, "volta-shim")
		}
		if _, err := os.Stat(link); err != nil {
			t.Errorf("shim %s still does not resolve: %v", name, err)
		}
	}
	if fi, err := os.Lstat(filepath.Join(home, ".volta/bin/volta")); err != nil {
		t.Errorf("lstat volta: %v", err)
	} else if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("the real volta binary was turned into a symlink")
	}
}

// TestInitScript_LinkVoltaBins_ExposesShimsOnPath: shim を直しても
// $HOME/.volta/bin は PATH に無い (PATH は image の値 + $HOME/.local/bin)。
// .local/bin に同名で張ることで初めて `node` が PATH から引ける。
func TestInitScript_LinkVoltaBins_ExposesShimsOnPath(t *testing.T) {
	home := t.TempDir()
	mustFile(t, filepath.Join(home, ".volta/bin/volta-shim"))
	for _, name := range []string{"node", "npm"} {
		mustSymlink(t, "volta-shim", filepath.Join(home, ".volta/bin", name))
	}

	runInitScriptHelper(t, home, "_link_volta_bins node npm npx")

	for _, name := range []string{"node", "npm"} {
		link := filepath.Join(home, ".local/bin", name)
		got, err := os.Readlink(link)
		if err != nil {
			t.Errorf("%s was not linked into .local/bin: %v", name, err)
			continue
		}
		if filepath.IsAbs(got) {
			t.Errorf("%s -> %q is absolute; it must be relative to survive the next home move", name, got)
		}
		if _, err := os.Stat(link); err != nil {
			t.Errorf("%s does not resolve through .local/bin: %v", name, err)
		}
	}
	// npx は shim が無いので張られない (存在しない target への link を残さない)。
	if _, err := os.Lstat(filepath.Join(home, ".local/bin/npx")); !os.IsNotExist(err) {
		t.Errorf("npx was linked even though no shim exists for it (err=%v)", err)
	}
}
