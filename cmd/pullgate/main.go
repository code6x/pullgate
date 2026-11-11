// Command pullgate pulls a git repo, asks the owner on Telegram to apply one
// exact commit, and applies it as root. See DESIGN.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/code6x/pullgate/internal/config"
	"github.com/code6x/pullgate/internal/gate"
	"github.com/code6x/pullgate/internal/listener"
	"github.com/code6x/pullgate/internal/telegram"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: pullgate [--config-dir DIR] [--state-dir DIR] [--spool-dir DIR] COMMAND
commands:
  run NAME       fetch, check, and ask (the timer runs it, as root)
  apply NAME     act on approvals (the path unit runs it, as root)
  status NAME [--ask-again] [--pause] [--resume]
  listen [--config FILE]   the Telegram listener (its own user)
  version`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func usageErr(stderr io.Writer, msg string) int {
	_, _ = fmt.Fprintf(stderr, "pullgate: %s; see \"pullgate --help\" (exit 64)\n", msg)
	return 64
}

func run(args []string, stdout, stderr io.Writer) int {
	// pullgate sets the mode of its own files itself. 022 is what the
	// owner's apply command expects (DESIGN.md section 11).
	syscall.Umask(0o022)
	fs := flag.NewFlagSet("pullgate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configDir := fs.String("config-dir", "/etc/pullgate", "")
	stateDir := fs.String("state-dir", "/var/lib/pullgate", "")
	spoolDir := fs.String("spool-dir", "/var/spool/pullgate", "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprintln(stdout, usage)
			return 0
		}
		return usageErr(stderr, err.Error())
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return usageErr(stderr, "give a command: run, apply, status, listen or version")
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	euid := uint32(os.Geteuid()) //nolint:gosec // a uid fits in uint32

	switch cmd := rest[0]; cmd {
	case "version":
		_, _ = fmt.Fprintln(stdout, "pullgate", version)
		return 0
	case "run", "apply", "status":
		sf := flag.NewFlagSet(cmd, flag.ContinueOnError)
		sf.SetOutput(io.Discard)
		var flags gate.StatusFlags
		if cmd == "status" {
			sf.BoolVar(&flags.AskAgain, "ask-again", false, "")
			sf.BoolVar(&flags.Pause, "pause", false, "")
			sf.BoolVar(&flags.Resume, "resume", false, "")
		}
		if len(rest) < 2 {
			return usageErr(stderr, "give the instance name: pullgate "+cmd+" NAME")
		}
		name := rest[1]
		if err := sf.Parse(rest[2:]); err != nil || sf.NArg() > 0 {
			return usageErr(stderr, "unknown arguments after pullgate "+cmd+" "+name)
		}
		log = log.With("instance", name, "step", cmd)
		cfg, err := config.Load(*configDir, name, euid)
		if err != nil {
			log.Error("config", "err", err)
			return 78
		}
		env, err := gate.New(cfg, *stateDir, *spoolDir, euid, log, stderr)
		if err != nil {
			log.Error("setup", "err", err)
			return 78
		}
		switch cmd {
		case "run":
			res, err := env.Run(ctx)
			return done(log, string(res), err)
		case "apply":
			res, err := env.Apply(ctx)
			return done(log, string(res), err)
		default:
			if err := env.Status(stdout, flags); err != nil {
				log.Error("status", "err", err)
				return 1
			}
			return 0
		}
	case "listen":
		lf := flag.NewFlagSet("listen", flag.ContinueOnError)
		lf.SetOutput(io.Discard)
		path := lf.String("config", "/etc/pullgate/listener.toml", "")
		ownConfig := lf.Bool("trust-own-config", false, "")
		if err := lf.Parse(rest[1:]); err != nil || lf.NArg() > 0 {
			return usageErr(stderr, "unknown arguments after pullgate listen")
		}
		log = log.With("step", "listen")
		configUID := uint32(0)
		if *ownConfig {
			configUID = euid // tests only: the config belongs to the test user
		}
		cfg, err := config.LoadListener(*path, configUID)
		if err != nil {
			log.Error("config", "err", err)
			return 78
		}
		if euid == 0 {
			log.Error("the listener must not run as root (rule 4). Fix: run it as its own user, like the example unit")
			return 78
		}
		tok, err := cfg.ReadToken(euid)
		if err != nil {
			log.Error("token", "err", err)
			return 78
		}
		l := &listener.Listener{Cfg: cfg, TG: telegram.New(cfg.APIURL, tok), Log: log}
		if err := l.Load(); err != nil {
			log.Error("state", "err", err)
			return 1
		}
		log.Info("listening", "instances", cfg.Instances)
		if err := l.Run(ctx); err != nil {
			log.Error("listen", "err", err)
			return 1
		}
		return 0
	default:
		return usageErr(stderr, fmt.Sprintf("unknown command %q", cmd))
	}
}

func done(log *slog.Logger, res string, err error) int {
	if err != nil {
		log.Error("failed", "result", res, "err", err)
		return 1
	}
	log.Info("done", "result", res)
	return 0
}
