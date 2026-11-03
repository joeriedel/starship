//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// killTreeOnCancel kills the command and everything it started when its context ends. The command runs in a job object, which catches descendants whose parent already exited; if the job can't be set up it falls back to taskkill, which walks the tree at kill time and can miss those. The returned started func must be called right after Start succeeds, and finish after the command ends; finish closes the job and, if the fallback killed anything, notes that on c.Stderr. When the command ends without being cancelled, finish first clears the job's kill-on-close flag so background children it left running survive, as they do on Unix.
func killTreeOnCancel(c *exec.Cmd, _ context.CancelFunc) (started func(), finish func() error) {
	var mu sync.Mutex
	var job windows.Handle
	var jobErr error
	usedFallback := false
	cancelled := false

	job, jobErr = newKillOnCloseJob()

	c.Cancel = func() error {
		mu.Lock()
		defer mu.Unlock()
		cancelled = true
		if jobErr == nil {
			return windows.TerminateJobObject(job, 1)
		}
		usedFallback = true
		if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(c.Process.Pid)).Run(); err != nil {
			if err := c.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				return err
			}
		}
		return nil
	}
	started = func() {
		mu.Lock()
		defer mu.Unlock()
		if jobErr != nil {
			return
		}
		jobErr = assignToJob(job, uint32(c.Process.Pid))
	}
	finish = func() error {
		mu.Lock()
		defer mu.Unlock()
		if job != 0 {
			// KILL_ON_JOB_CLOSE stays set until here as the guard if ensign dies mid-run; after an uncancelled run, leftovers are a success and must outlive the handle.
			if !cancelled && jobErr == nil {
				var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
				if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil && c.Stderr != nil {
					fmt.Fprintf(c.Stderr, "ensign: could not release job object (%v); leftover background processes may have been stopped\n", err)
				}
			}
			windows.CloseHandle(job)
			job = 0
		}
		if usedFallback && c.Stderr != nil {
			fmt.Fprintf(c.Stderr, "ensign: job object unavailable (%v); used taskkill, which can miss descendants whose parent already exited\n", jobErr)
		}
		return nil
	}
	return started, finish
}

// newKillOnCloseJob creates a job object whose processes are killed when its last handle closes.
func newKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("create job: %w", err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("configure job: %w", err)
	}
	return job, nil
}

// assignToJob puts the process with the given pid into job.
func assignToJob(job windows.Handle, pid uint32) error {
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return fmt.Errorf("open process: %w", err)
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		return fmt.Errorf("assign process: %w", err)
	}
	return nil
}
