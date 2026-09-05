// Package procx starts child processes so that cancelling one kills its whole
// tree.
//
// This matters more than it looks. Killing git alone orphans the helpers it
// spawned - git-remote-https and git-lfs - and those keep open handles on the
// staging directory. The next step in the pipeline is to delete that directory,
// which then fails for as long as the orphan lives. So a timeout or a Ctrl-C has
// to take the descendants with it, not just the process that was launched.
package procx

import "os/exec"

// Tree kills a process together with everything it spawned.
type Tree struct {
	cmd *exec.Cmd
}

// Start prepares cmd for group termination, starts it, and returns the Tree that
// controls it. On failure the returned Tree is nil.
func Start(cmd *exec.Cmd) (*Tree, error) {
	configure(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	t := &Tree{cmd: cmd}
	t.adopt()
	return t, nil
}

// Kill terminates the process and everything it spawned. It is safe to call on a
// process that has already exited, and safe to call more than once.
func (t *Tree) Kill() {
	if t == nil || t.cmd == nil {
		return
	}
	t.kill()
}

// Close releases any handle held for the tree. It does not terminate anything;
// call it after Wait.
func (t *Tree) Close() {
	if t == nil {
		return
	}
	t.close()
}
