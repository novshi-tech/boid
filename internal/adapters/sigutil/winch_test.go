package sigutil

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// The launcher deliberately does not forward SIGWINCH to its child.
func TestWinchHelper(t *testing.T) {
	switch os.Getenv("BOID_TEST_WINCH_HELPER") {
	case "launcher":
		cmd := exec.Command(os.Args[0], "-test.run=^TestWinchHelper$")
		cmd.Env = []string{"BOID_TEST_WINCH_HELPER=receiver"}
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
	case "receiver":
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGWINCH)
		defer signal.Stop(ch)
		fmt.Println("ready")
		select {
		case <-ch:
			fmt.Println("resized")
		case <-time.After(3 * time.Second):
			t.Fatal("resize signal never reached the terminal application")
		}
	}
}

func TestForwardAndWait_ResizeReachesApplication(t *testing.T) {
	for _, isolated := range []bool{false, true} {
		t.Run(fmt.Sprintf("isolated=%v", isolated), func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestWinchHelper$")
			role := "receiver"
			if isolated {
				role = "launcher"
				cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			}
			cmd.Env = []string{"BOID_TEST_WINCH_HELPER=" + role}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			scanner := bufio.NewScanner(stdout)
			if !scanner.Scan() || scanner.Text() != "ready" {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				t.Fatal("application did not become ready")
			}
			done := make(chan struct{})
			// Retry until the forwarding loop has installed its signal handler.
			go func() {
				ticker := time.NewTicker(20 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-done:
						return
					case <-ticker.C:
						_ = syscall.Kill(os.Getpid(), syscall.SIGWINCH)
					}
				}
			}()
			code, stopped, err := ForwardAndWait(cmd, "test application")
			close(done)
			if err != nil || code != 0 || stopped {
				t.Fatalf("resize forwarding: exit=%d stopped=%v err=%v", code, stopped, err)
			}
		})
	}
}
