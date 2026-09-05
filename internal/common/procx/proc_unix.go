//go:build !windows

package procx

import (
	"os/exec"
	"syscall"
	"time"
)

// killGrace is how long the tree gets to exit on SIGTERM before SIGKILL. git and
// git-lfs both clean up their temporary files on SIGTERM, so it is worth asking
// politely first; the Windows job-object path has no equivalent.
const killGrace = 5 * time.Second

// configure puts the child in a new process group, which is what makes it
// possible to signal it together with every helper it spawns.
func configure(cmd *exec.Cmd) {
	attr := cmd.SysProcAttr
	if attr == nil {
		attr = &syscall.SysProcAttr{}
		cmd.SysProcAttr = attr
	}
	attr.Setpgid = true
}

// adopt has nothing to do here: the process group was established at Start by the
// Setpgid attribute, and the group id equals the child's pid.
func (t *Tree) adopt() {}

func (t *Tree) kill() {
	if t.cmd.Process == nil {
		return
	}
	// Negating the pid addresses the whole process group, so git-lfs and
	// git-remote-https go down with git and release the staging directory.
	pgid := -t.cmd.Process.Pid

	if err := syscall.Kill(pgid, syscall.SIGTERM); err != nil {
		// Already gone, or never a group leader; nothing left to do politely.
		_ = syscall.Kill(pgid, syscall.SIGKILL)
		return
	}

	go func() {
		time.Sleep(killGrace)
		// Harmless once the group has exited: the pgid is gone and Kill simply
		// reports ESRCH.
		_ = syscall.Kill(pgid, syscall.SIGKILL)
	}()
}

// close has no handle to release on this platform.
func (t *Tree) close() {}
