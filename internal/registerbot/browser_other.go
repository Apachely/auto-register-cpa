//go:build !windows

package registerbot

import "os/exec"

func hideWindow(cmd *exec.Cmd) {}
