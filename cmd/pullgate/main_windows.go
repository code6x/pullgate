// Command pullgate asks the owner to approve an exact git commit.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"github.com/code6x/pullgate/internal/config"
	"github.com/code6x/pullgate/internal/gate"
	"github.com/code6x/pullgate/internal/winsecure"
)

var version = "dev"

const usage = `usage: pullgate COMMAND
  version        print the version
  init           create the protected ProgramData directory
  install-binary copy this verified executable into ProgramData
  setup          set up one instance and enter the bot token without echo
  agent          run the listener and check for updates (Task Scheduler)
  doctor [NAME]  check the Windows setup
  status NAME [--ask-again] [--pause] [--resume]`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runWindows(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "pullgate:", err)
		os.Exit(1)
	}
}

func runWindows(ctx context.Context, args []string, out, errOut io.Writer) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		_, err := fmt.Fprintln(out, usage)
		return err
	}
	if args[0] == "version" || args[0] == "--version" || args[0] == "-version" {
		_, err := fmt.Fprintln(out, "pullgate", version)
		return err
	}
	root, err := winsecure.ProgramRoot()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(errOut, nil))
	switch args[0] {
	case "init", "install-binary":
		if len(args) != 1 {
			return errors.New("unexpected arguments. Fix: run pullgate --help")
		}
		if err := initialize(root); err != nil {
			return err
		}
		if args[0] == "install-binary" {
			return installBinary(root)
		}
		return nil
	case "setup":
		return setupWindows(ctx, root, args[1:], out)
	case "agent":
		if len(args) != 1 {
			return errors.New("unexpected arguments. Fix: run pullgate agent")
		}
		return agent(ctx, root, log, errOut)
	case "doctor":
		if len(args) > 2 {
			return errors.New("unexpected arguments. Fix: run pullgate doctor [NAME]")
		}
		return doctorWindows(root, args[1:], out)
	case "status":
		if len(args) < 2 {
			return errors.New("instance name required. Fix: run pullgate status NAME")
		}
		fs := flag.NewFlagSet("status", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		var f gate.StatusFlags
		fs.BoolVar(&f.AskAgain, "ask-again", false, "")
		fs.BoolVar(&f.Pause, "pause", false, "")
		fs.BoolVar(&f.Resume, "resume", false, "")
		if err := fs.Parse(args[2:]); err != nil || fs.NArg() != 0 {
			return errors.New("invalid status flags. Fix: run pullgate --help")
		}
		e, err := loadGate(root, args[1], "", log, errOut)
		if err != nil {
			return err
		}
		return e.Status(out, f)
	default:
		return errors.New("unknown command. Fix: run pullgate --help")
	}
}

func initialize(root string) error {
	for _, path := range []string{root, filepath.Join(root, "state"), filepath.Join(root, "spool")} {
		if err := winsecure.Mkdir(path); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return err
		}
		if err := winsecure.Check(path, true); err != nil {
			return err
		}
	}
	return nil
}

func loadGate(root, name, git string, log *slog.Logger, out io.Writer) (*gate.Env, error) {
	if err := winsecure.Check(root, true); err != nil {
		return nil, err
	}
	c, err := config.Load(root, name, 0)
	if err != nil {
		return nil, err
	}
	return gate.NewWindows(c, root, git, log.With("instance", name), out)
}

func gitExecutable() (string, error) {
	dir, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
	if err != nil {
		return "", fmt.Errorf("find Program Files: %w. Fix: check the Windows installation", err)
	}
	path := filepath.Join(dir, "Git", "cmd", "git.exe")
	if err := winsecure.CheckSystemPath(path, false); err != nil {
		return "", fmt.Errorf("check Git for Windows: %w. Fix: install Git for Windows for all users in Program Files", err)
	}
	return path, nil
}

func doctorWindows(root string, names []string, out io.Writer) error {
	for _, path := range []string{"state", "spool"} {
		if err := winsecure.CheckManaged(root, filepath.Join(root, path), true); err != nil {
			return err
		}
	}
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return fmt.Errorf("find Task Scheduler: %w. Fix: check the Windows installation", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task := exec.CommandContext(ctx, filepath.Join(system, "schtasks.exe"), "/Query", "/TN", `\pullgate`) //nolint:gosec // built-in Windows utility and fixed arguments
	if err := task.Run(); err != nil {
		return fmt.Errorf("cannot find the pullgate boot task: %w. Fix: run install.ps1 as an administrator", err)
	}
	_, _ = fmt.Fprintln(out, "OK protected data directories and boot task")
	_, err = gitExecutable()
	if err != nil {
		return err
	}
	c, err := loadListener(root)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		names = c.Instances
	}
	if _, err := c.ReadToken(0); err != nil {
		return err
	}
	for _, name := range names {
		if _, err := loadGate(root, name, "", slog.New(slog.NewTextHandler(io.Discard, nil)), io.Discard); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(out, "OK", name, "config and PowerShell script ACL")
	}
	_, err = fmt.Fprintln(out, "OK Git for Windows, listener config and protected token")
	return err
}

const checkInterval = time.Minute
