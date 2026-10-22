package command

import (
	"os"
	"os/exec"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate
//counterfeiter:generate -o ../../fakes/runner.go . Runner
type Runner interface {
	Run(string, []string, []string, <-chan os.Signal) error
}

type ExecRunner struct{}

func (ExecRunner) Run(path string, args, env []string, signals <-chan os.Signal) error {
	cmd := exec.Command(path, args...)
	cmd.Env, cmd.Stdout, cmd.Stderr = env, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case sig := <-signals:
		_ = cmd.Process.Signal(sig)
		return <-done
	}
}
