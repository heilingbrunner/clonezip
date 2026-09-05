package procx

import (
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobs maps each Tree to its job object handle. It lives here rather than as a
// field on Tree because Tree is shared with the non-Windows build, where a job
// handle has no meaning.
var jobs sync.Map // map[*Tree]uintptr

// configure puts the child in its own process group so a console Ctrl-C does not
// reach it directly: clonezip handles the signal itself and decides when to tear the
// tree down, after the in-flight repository has been given a chance to finish.
func configure(cmd *exec.Cmd) {
	attr := cmd.SysProcAttr
	if attr == nil {
		attr = &windows.SysProcAttr{}
		cmd.SysProcAttr = attr
	}
	attr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
}

// adopt puts the started process into a fresh job object. Every process it spawns
// inherits the job, so terminating the job terminates the whole tree.
//
// The job is created and assigned after Start rather than before, because Go does
// not expose the child's initial thread handle that starting it suspended would
// need. The window between Start and assignment is microseconds while git takes
// milliseconds to spawn its first helper, so in practice the tree is covered.
// When assignment fails there is nothing useful to report: kill falls back to
// killing the process alone, which is what would have happened regardless.
func (t *Tree) adopt() {
	if t.cmd.Process == nil {
		return
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return
	}

	// KILL_ON_JOB_CLOSE means that if clonezip itself dies, Windows tears the tree
	// down too, rather than leaving clones running and the stage dir locked.
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return
	}

	handle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(t.cmd.Process.Pid),
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	if err := windows.AssignProcessToJobObject(job, handle); err != nil {
		_ = windows.CloseHandle(job)
		return
	}
	jobs.Store(t, uintptr(job))
}

func (t *Tree) kill() {
	if job, ok := jobs.Load(t); ok {
		// Terminates every process in the job, so git and its helpers go together and
		// release their handles on the staging directory. If this fails there is no
		// recovery: the caller is already tearing down.
		_ = windows.TerminateJobObject(windows.Handle(job.(uintptr)), 1)
		return
	}
	if t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
}

func (t *Tree) close() {
	if job, ok := jobs.LoadAndDelete(t); ok {
		_ = windows.CloseHandle(windows.Handle(job.(uintptr)))
	}
}
