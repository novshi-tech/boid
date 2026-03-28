package testutil

import "github.com/novshi-tech/boid/internal/sandbox"

type MockSandbox struct {
	SetupCalled bool
	Config      sandbox.SandboxConfig
	ShellCalls  []string
	CleanedUp   bool
}

func (m *MockSandbox) Setup(cfg sandbox.SandboxConfig) error {
	m.SetupCalled = true
	m.Config = cfg
	return nil
}

func (m *MockSandbox) Shell(windowName string, cmd string) (string, error) {
	m.ShellCalls = append(m.ShellCalls, cmd)
	return cmd, nil
}

func (m *MockSandbox) Cleanup() error {
	m.CleanedUp = true
	return nil
}
