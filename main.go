package main

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/novshi-tech/boid/cmd"
	"github.com/novshi-tech/boid/internal/sandbox"
)

func main() {
	command := sandbox.CommandFromArgv0(os.Args[0])
	if shouldRunBoidBuiltinShim(command) {
		resp, err := sandbox.RunBoidShim(os.Args[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if resp.Stdout != "" {
			os.Stdout.WriteString(resp.Stdout)
		}
		if resp.Stderr != "" {
			os.Stderr.WriteString(resp.Stderr)
		}
		os.Exit(resp.ExitCode)
	}
	if command != "boid" {
		if command == "git" {
			exitCode, err := sandbox.RunGitShim(os.Args[1:])
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(exitCode)
		}
		shimMain()
		return
	}

	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func shouldRunBoidBuiltinShim(command string) bool {
	return command == "boid" && os.Getenv("BOID_BUILTIN_SHIM") != ""
}

func shimMain() {
	brokerSocket := os.Getenv("BOID_BROKER_SOCKET")
	if brokerSocket == "" {
		fmt.Fprintf(os.Stderr, "boid shim: BOID_BROKER_SOCKET not set\n")
		os.Exit(1)
	}

	stdin := readStdinNonBlocking()

	resp, err := sandbox.ShimExec(brokerSocket, os.Args[0], os.Args[1:], stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boid shim: %v\n", err)
		os.Exit(1)
	}

	if resp.Stdout != "" {
		os.Stdout.WriteString(resp.Stdout)
	}
	if resp.Stderr != "" {
		os.Stderr.WriteString(resp.Stderr)
	}
	os.Exit(resp.ExitCode)
}

func readStdinNonBlocking() []byte {
	stat, err := os.Stdin.Stat()
	if err != nil {
		return nil
	}
	// No pipe connected
	if stat.Mode()&os.ModeNamedPipe == 0 {
		return nil
	}
	// Pipe present — read with timeout to avoid hanging
	ch := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(os.Stdin)
		ch <- data
	}()
	select {
	case data := <-ch:
		return data
	case <-time.After(100 * time.Millisecond):
		return nil
	}
}
