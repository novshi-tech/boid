package sandbox_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

func TestBroker_DropsStdinWhenNotAllowed(t *testing.T) {
	// AllowStdin=false の host command に stdin が渡された場合、broker は黙って
	// stdin を捨ててコマンドを実行する。シェルのパイプラインで親プロセスの
	// stdin が子コマンドに継承されてしまうケース (hook が `printf | hook.sh` で
	// 起動された後に hook 内から host command を呼ぶケース) で、関係のない
	// 子 host command 呼び出しまで巻き込んで失敗させないため。
	broker := &sandbox.Broker{}
	token := broker.Register(map[string]sandbox.CommandDef{
		"cat": {
			Name:            "cat",
			Path:            "/bin/cat",
			AllowedPatterns: []string{"*"},
		},
	}, nil, testCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "cat",
		Token:   token,
		Stdin:   []byte("ignored input"),
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if resp.Stdout != "" {
		t.Fatalf("stdout = %q, want empty (stdin dropped)", resp.Stdout)
	}
}

func TestBroker_AllowsStdinWhenConfigured(t *testing.T) {
	broker := &sandbox.Broker{}
	token := broker.Register(map[string]sandbox.CommandDef{
		"cat": {
			Name:            "cat",
			Path:            "/bin/cat",
			AllowedPatterns: []string{"*"},
			AllowStdin:      true,
		},
	}, nil, testCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "cat",
		Token:   token,
		Stdin:   []byte("hello stdin"),
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if resp.Stdout != "hello stdin" {
		t.Fatalf("stdout = %q, want %q", resp.Stdout, "hello stdin")
	}
}

