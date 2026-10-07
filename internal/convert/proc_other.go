//go:build !windows

package convert

import "os/exec"

func hideWindow(*exec.Cmd) {}
