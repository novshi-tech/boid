//go:build linux

package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/novshi-tech/boid/internal/atomicfile"
	"github.com/spf13/cobra"
)

// hostPortsFileName names the file under hostModeConfigDir() that records
// which host ports this user's compose stack publishes.
const hostPortsFileName = "host-ports.json"

const (
	defaultHostCLIPort = 8442
	defaultHostWebPort = 8080
)

// hostPorts is the host side of compose.yml's port publish. The container
// side stays fixed; only these vary, so several users can share one host.
type hostPorts struct {
	CLI int `json:"cli_port,omitempty"`
	Web int `json:"web_port,omitempty"`
}

func defaultHostPorts() hostPorts {
	return hostPorts{CLI: defaultHostCLIPort, Web: defaultHostWebPort}
}

// cliAddr is the address the host CLI dials to reach its own daemon.
func (p hostPorts) cliAddr() string {
	return "127.0.0.1:" + strconv.Itoa(p.CLI)
}

// composeEnv returns the variables compose.yml interpolates into `ports:`.
func (p hostPorts) composeEnv() []string {
	return []string{
		"BOID_CLI_PORT=" + strconv.Itoa(p.CLI),
		"BOID_WEB_PORT=" + strconv.Itoa(p.Web),
	}
}

func (p hostPorts) validate() error {
	for _, f := range []struct {
		name string
		port int
	}{{"cli_port", p.CLI}, {"web_port", p.Web}} {
		if f.port < 1 || f.port > 65535 {
			return fmt.Errorf("%s %d is out of range (1-65535)", f.name, f.port)
		}
	}
	if p.CLI == p.Web {
		return fmt.Errorf("cli_port and web_port are both %d", p.CLI)
	}
	return nil
}

func hostPortsPath() (string, error) {
	dir, err := hostModeConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, hostPortsFileName), nil
}

// loadHostPorts reads the saved host ports, falling back to the defaults
// for a missing file or an omitted field.
func loadHostPorts() (hostPorts, error) {
	path, err := hostPortsPath()
	if err != nil {
		return hostPorts{}, err
	}
	ports := defaultHostPorts()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ports, nil
	}
	if err != nil {
		return hostPorts{}, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &ports); err != nil {
		return hostPorts{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := ports.validate(); err != nil {
		return hostPorts{}, fmt.Errorf("%s: %w", path, err)
	}
	return ports, nil
}

func saveHostPorts(p hostPorts) error {
	if err := p.validate(); err != nil {
		return err
	}
	path, err := hostPortsPath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicfile.WriteAtomic(path, 0o600, append(data, '\n')); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// resolveStartHostPorts applies `boid start`'s --cli-port/--web-port on top
// of the saved ports and persists the result so later commands dial it.
func resolveStartHostPorts(cmd *cobra.Command) (hostPorts, error) {
	ports, err := loadHostPorts()
	if err != nil {
		return hostPorts{}, err
	}
	changed := false
	for name, dst := range map[string]*int{"cli-port": &ports.CLI, "web-port": &ports.Web} {
		f := cmd.Flags().Lookup(name)
		if f == nil || !f.Changed {
			continue
		}
		v, err := strconv.Atoi(f.Value.String())
		if err != nil {
			return hostPorts{}, fmt.Errorf("--%s: %w", name, err)
		}
		*dst = v
		changed = true
	}
	if !changed {
		return ports, nil
	}
	if err := ports.validate(); err != nil {
		return hostPorts{}, fmt.Errorf("boid start: %w", err)
	}
	if err := saveHostPorts(ports); err != nil {
		return hostPorts{}, fmt.Errorf("boid start: %w", err)
	}
	return ports, nil
}
