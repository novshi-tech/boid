package dtmux

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
)

type RealTmux struct{}

func sessionTarget(name string) string {
	return name + ":"
}

func (t *RealTmux) EnsureSession(name string) error {
	if t.HasSession(name) {
		return nil
	}
	return exec.Command("tmux", "new-session", "-d", "-s", name).Run()
}

func (t *RealTmux) NewWindow(session, windowName string) error {
	return exec.Command("tmux", "new-window", "-t", sessionTarget(session), "-n", windowName).Run()
}

func (t *RealTmux) RunInWindow(session, windowName, command string) error {
	return exec.Command("tmux", "new-window", "-t", sessionTarget(session), "-n", windowName, "sh", "-c", command).Run()
}

func (t *RealTmux) SendKeys(session, window, keys string) error {
	return exec.Command("tmux", "send-keys", "-t", session+":"+window, keys, "Enter").Run()
}

func (t *RealTmux) KillWindow(session, window string) error {
	return exec.Command("tmux", "kill-window", "-t", session+":"+window).Run()
}

func (t *RealTmux) ListWindows(session string) ([]string, error) {
	out, err := exec.Command("tmux", "list-windows", "-t", session, "-F", "#{window_name}").Output()
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return lines, nil
}

func (t *RealTmux) HasSession(name string) bool {
	return exec.Command("tmux", "has-session", "-t", name).Run() == nil
}

func (t *RealTmux) SwitchClient(session, window string) error {
	return exec.Command("tmux", "switch-client", "-t", session+":"+window).Run()
}

func (t *RealTmux) Attach(session string) error {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		return err
	}
	return syscall.Exec(tmuxPath, []string{"tmux", "attach-session", "-t", session}, os.Environ())
}
