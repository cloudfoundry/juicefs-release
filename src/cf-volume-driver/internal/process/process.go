// Package process supplies the operating-system boundaries used by the driver.
package process

import (
	"os"
	"os/exec"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate
//counterfeiter:generate -o ../../fakes/process.go . Process
//counterfeiter:generate -o ../../fakes/factory.go . Factory
//counterfeiter:generate -o ../../fakes/mounts.go . Mounts

type Process interface {
	Start() error
	Wait() error
	Signal(os.Signal) error
	Kill() error
}

type Factory interface {
	New(string, ...string) Process
}

type Mounts interface {
	Mounted(string) (bool, error)
	Unmount(string) error
}

type FactoryImpl struct{}

func (FactoryImpl) New(path string, args ...string) Process {
	cmd := exec.Command(path, args...)
	cmd.Env, cmd.Stdout, cmd.Stderr = os.Environ(), os.Stdout, os.Stderr
	return &command{cmd: cmd}
}

type command struct{ cmd *exec.Cmd }

func (c *command) Start() error             { return c.cmd.Start() }
func (c *command) Wait() error              { return c.cmd.Wait() }
func (c *command) Signal(s os.Signal) error { return c.cmd.Process.Signal(s) }
func (c *command) Kill() error              { return c.cmd.Process.Kill() }
