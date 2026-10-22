package cli

import (
	"fmt"
	"os/exec"
	"strings"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate
//counterfeiter:generate -o ../../fakes/runner.go . Runner
type Runner interface {
	Output([]string, ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Output(env []string, args ...string) ([]byte, error) {
	path, err := exec.LookPath("cf")
	if err != nil {
		return nil, fmt.Errorf("locate vendored cf CLI: %w", err)
	}
	cmd := exec.Command(path, args...)
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("cf %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}
