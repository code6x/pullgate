package gate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ProcThreadAttributeValue(13, FALSE, TRUE, FALSE) from the Windows SDK.
const procThreadAttributeJobList = 0x0002000d

// Windows assigns the process to its job when creating the process. It
// never has a chance to start children outside the job.
func runInJob(ctx context.Context, prog string, args, env []string, dir string, out io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("create PowerShell process job: %w", err)
	}
	defer func() {
		if job != 0 {
			_ = windows.CloseHandle(job)
		}
	}()
	var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))) //nolint:gosec // documented Win32 job structure
	if err != nil {
		return fmt.Errorf("set PowerShell process job limits: %w", err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer func() { _ = stdin.Close() }()
	handles := make([]windows.Handle, 2)
	for i, f := range []*os.File{stdin, w} {
		process := windows.CurrentProcess()
		if err := windows.DuplicateHandle(process, windows.Handle(f.Fd()), process, &handles[i], 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
			return err
		}
		defer func() { _ = windows.CloseHandle(handles[i]) }()
	}
	pi, err := startInJob(job, handles, prog, args, env, dir)
	if err != nil {
		return fmt.Errorf("start PowerShell in its process job: %w. Fix: check the script and the task's permissions", err)
	}
	_ = windows.CloseHandle(pi.Thread)
	_ = w.Close()
	// The parent must close its copy before waiting for pipe EOF.
	_ = windows.CloseHandle(handles[1])
	handles[1] = 0
	defer func() { _ = windows.CloseHandle(pi.Process) }()
	drained := make(chan error, 1)
	go func() {
		_, err := io.Copy(out, r)
		drained <- err
	}()
	finished := make(chan error, 1)
	go func() {
		_, err := windows.WaitForSingleObject(pi.Process, windows.INFINITE)
		finished <- err
	}()
	cancelled := false
	select {
	case err = <-finished:
	case <-ctx.Done():
		err = ctx.Err()
		cancelled = true
	}
	// Closing the job stops the leader on cancellation and also stops
	// descendants it left running after a normal exit.
	if closeErr := windows.CloseHandle(job); closeErr != nil {
		_ = r.Close()
		return fmt.Errorf("close PowerShell process job: %w", closeErr)
	}
	job = 0
	if cancelled {
		<-finished
	}
	copyErr := <-drained
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(pi.Process, &code); err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("PowerShell exit status %d", code)
	}
	return copyErr
}

func startInJob(job windows.Handle, handles []windows.Handle, prog string, args, env []string, dir string) (windows.ProcessInformation, error) {
	var pi windows.ProcessInformation
	attrs, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		return pi, err
	}
	defer attrs.Delete()
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]), uintptr(len(handles))*unsafe.Sizeof(handles[0])); err != nil { //nolint:gosec // Windows receives only the two intended pipe handles
		return pi, err
	}
	if err := attrs.Update(procThreadAttributeJobList, unsafe.Pointer(&job), unsafe.Sizeof(job)); err != nil { //nolint:gosec // Windows receives one job handle
		return pi, err
	}
	app, err := windows.UTF16PtrFromString(prog)
	if err != nil {
		return pi, err
	}
	argv, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{prog}, args...)))
	if err != nil {
		return pi, err
	}
	cwd, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return pi, err
	}
	block, err := windowsEnvironment(env)
	if err != nil {
		return pi, err
	}
	si := windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{})), Flags: windows.STARTF_USESTDHANDLES,
			StdInput: handles[0], StdOutput: handles[1], StdErr: handles[1],
		},
		ProcThreadAttributeList: attrs.List(),
	}
	err = windows.CreateProcess(app, argv, nil, nil, true, windows.EXTENDED_STARTUPINFO_PRESENT|
		windows.CREATE_UNICODE_ENVIRONMENT|windows.CREATE_NO_WINDOW, &block[0], cwd, &si.StartupInfo, &pi)
	return pi, err
}

func windowsEnvironment(env []string) ([]uint16, error) {
	slices.SortFunc(env, func(a, b string) int { return strings.Compare(strings.ToUpper(a), strings.ToUpper(b)) })
	var block []uint16
	for _, entry := range env {
		s, err := windows.UTF16FromString(entry)
		if err != nil {
			return nil, errors.New("invalid PowerShell environment. Fix: check the host config")
		}
		block = append(block, s...)
	}
	return append(block, 0), nil
}
