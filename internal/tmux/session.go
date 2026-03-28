package tmux

type TmuxManager interface {
	EnsureSession(name string) error
	NewWindow(session, windowName string) error
	SendKeys(session, window, keys string) error
	KillWindow(session, window string) error
	ListWindows(session string) ([]string, error)
	HasSession(name string) bool
	Attach(session string) error
}
