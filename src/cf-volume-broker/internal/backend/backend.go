package backend

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate
//counterfeiter:generate -o ../../fakes/runner.go . Runner
type Runner interface{ Run(string, ...string) error }

type ExecRunner struct{}

func (ExecRunner) Run(path string, args ...string) error {
	cmd := exec.Command(path, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", path, strings.Join(args, " "), err)
	}
	return nil
}
