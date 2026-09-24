//go:build windows

package hook

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// shellCommand runs command through cmd.exe exactly as the user wrote it.
// os/exec quotes each argument for CommandLineToArgvW, which cmd.exe does not
// parse, so a quoted path such as "C:\Program Files\x\y.exe" reached cmd as
// \"...\" and the hook failed. The command line is therefore written whole:
// /s makes cmd strip only the outer quote pair and run the rest verbatim, and
// /d skips any AutoRun command in the registry, as a hook runs one command.
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "cmd")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd /d /s /c "` + command + `"`}
	return cmd
}

// procGuard confines one hook run to a Windows Job Object so the deadline
// kills the whole process tree, not just the cmd.exe shell.
//
// Windows has no process group to signal, and TerminateProcess on cmd.exe
// leaves everything cmd.exe spawned running forever: WaitDelay only bounds
// papio's wait on the inherited pipes, it never ends a descendant. The job is
// created with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE and with neither breakaway
// limit set, so terminating the job kills the tree at once, closing the last
// handle collects any survivor even if papio dies first, and a descendant
// asking for CREATE_BREAKAWAY_FROM_JOB is refused rather than escaping.
type procGuard struct {
	// setupErr records a job that could not be created; confine reports it so
	// a run that cannot be confined fails loudly instead of leaking a tree.
	setupErr error

	mu        sync.Mutex
	job       windows.Handle
	assigned  bool
	cancelled bool
}

// newProcGuard creates the job and arranges for the shell to start suspended.
// A job assignment only covers descendants created after it, so cmd.exe must
// not run a single instruction before it is inside the job.
func newProcGuard(cmd *exec.Cmd) *procGuard {
	// Keep the command line shellCommand wrote; only add the suspension.
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	guard := &procGuard{}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		guard.setupErr = fmt.Errorf("create hook job object: %w", err)
		return guard
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		guard.setupErr = fmt.Errorf("set hook job kill-on-close: %w", err)
		return guard
	}
	guard.job = job
	return guard
}

// confine assigns the started shell to the job, then lets it run. It must be
// called between Start and Wait: the process must exist to be assigned, and
// must be assigned before it is resumed and can spawn anything.
func (g *procGuard) confine(cmd *exec.Cmd) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cancelled {
		// The deadline already fired and kill terminated the still-suspended
		// shell; resuming it would revive a corpse. Wait reaps it.
		return nil
	}
	if g.setupErr != nil {
		return g.setupErr
	}
	if cmd.Process == nil {
		return errors.New("hook shell not started")
	}
	// The shell is suspended and its handle is held by os/exec, so the pid
	// cannot have been reused between Start and this OpenProcess.
	pid := uint32(cmd.Process.Pid)
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return fmt.Errorf("open hook shell: %w", err)
	}
	defer func() { _ = windows.CloseHandle(proc) }()
	if err := windows.AssignProcessToJobObject(g.job, proc); err != nil {
		return fmt.Errorf("assign hook shell to job: %w", err)
	}
	g.assigned = true
	return resumeProcess(pid)
}

// kill terminates the job, which ends the shell and every descendant at once.
// It falls back to the shell alone when the job is not holding it yet - the
// deadline can fire while the shell is still suspended.
func (g *procGuard) kill(cmd *exec.Cmd) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cancelled = true
	if g.assigned && g.job != 0 {
		if err := windows.TerminateJobObject(g.job, 1); err == nil {
			return nil
		}
	}
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

// close releases the job handle on every exit path: clean exit, non-zero exit
// and a failed start. The handle is the job's lifetime - leaking it keeps the
// job alive for the life of the daemon, and closing it is what makes
// kill-on-close collect a descendant the shell left behind.
func (g *procGuard) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job == 0 {
		return
	}
	_ = windows.CloseHandle(g.job)
	g.job = 0
	g.assigned = false
}

// resumeProcess resumes every thread of pid. A process created suspended has
// exactly one, but its handle is not reachable: os/exec hands back only the
// pid, so the threads are enumerated instead of assumed.
func resumeProcess(pid uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("snapshot hook threads: %w", err)
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()
	var entry windows.ThreadEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	resumed := 0
	// TH32CS_SNAPTHREAD ignores the pid argument and snapshots every thread on
	// the system, so filter by owner.
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}
		thread, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if openErr != nil {
			return fmt.Errorf("open hook thread: %w", openErr)
		}
		_, resumeErr := windows.ResumeThread(thread)
		_ = windows.CloseHandle(thread)
		if resumeErr != nil {
			return fmt.Errorf("resume hook thread: %w", resumeErr)
		}
		resumed++
	}
	if err != nil && !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return fmt.Errorf("enumerate hook threads: %w", err)
	}
	if resumed == 0 {
		return errors.New("hook shell has no resumable thread")
	}
	return nil
}
