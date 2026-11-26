// Command pullgate pulls a git repo, asks the owner on Telegram to apply one
// exact commit, and applies it as root. See DESIGN.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/code6x/pullgate"
	"github.com/code6x/pullgate/internal/bootstrap"
	"github.com/code6x/pullgate/internal/config"
	"github.com/code6x/pullgate/internal/doctor"
	"github.com/code6x/pullgate/internal/gate"
	"github.com/code6x/pullgate/internal/listener"
	"github.com/code6x/pullgate/internal/recipe"
	"github.com/code6x/pullgate/internal/selfupdate"
	"github.com/code6x/pullgate/internal/spool"
	"github.com/code6x/pullgate/internal/telegram"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: pullgate [--config-dir DIR] [--state-dir DIR] [--spool-dir DIR] COMMAND
       pullgate --version | -version   print the version (same as: pullgate version)
       pullgate --help
commands:
  run NAME       fetch, check, and ask (the timer runs it, as root)
  apply NAME     act on approvals (the path unit runs it, as root)
  status NAME [--ask-again] [--pause] [--resume]
                 --ask-again: ask about the last tip again; it also closes
                 an open question for it, so the next run sends a new one
  listen [--config FILE]   the Telegram listener (its own user)
  update [--config FILE]   look for a new release of pullgate, and install
                 one after a tap (pullgate-update.service runs it, as root)
  units          print the units this binary carries, as JSON
  recipe fetch URL [--gh PATH] [--bundle NAME] [--signer-workflow PATH]
                 [--dir DIR]   download a recipe from a GitHub release,
                 check its SHA-256 and build attestation, and unpack it,
                 as root; the last line is its folder
  recipe update NAME (--version X.Y.Z | --url URL) [--reinstall] [--gh PATH]
                 move an instance to a newer version of its recipe, with
                 the checks of the first install, as root (forward only)
  recipe use NAME X.Y.Z   switch an instance to a version that recipe
                 fetch unpacked; it counts from the next run
  doctor [NAME] [--telegram] [--config FILE] [--unit-dir DIR]
                 check the setup of the host, as root: one line per check,
                 and exit 1 on any problem. --telegram also checks the bot
                 token and the chat with Telegram
  bootstrap [--token-stdin] [--add] [--recipe URL [--gh PATH] [--bundle NAME]
                 [--signer-workflow PATH] [--replace-config]]
                 --instance NAME --repo URL [--branch B] [--apply PROGRAM]
                 [--known-hosts FILE] [--recipe-instance NAME] [--instance ...]
                 set up a host, as root: the bot token (with no echo),
                 the pairing with the owner's chat by a one-time code, the
                 deploy key and the host key of each instance. --recipe
                 sets the instances up from a checked recipe; --add adds
                 them to a paired host, with no token and no pairing
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
	showVersion := fs.Bool("version", false, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprintln(stdout, usage)
			return 0
		}
		return usageErr(stderr, err.Error())
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, "pullgate", version)
		return 0
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return usageErr(stderr, "give a command: run, apply, status, listen, update, units, bootstrap, doctor or version")
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
	case "bootstrap":
		o, err := bootstrap.Parse(rest[1:])
		if err != nil {
			return usageErr(stderr, err.Error())
		}
		o.EtcDir, o.Out, o.Err = *configDir, stdout, stderr
		o.Fetcher.UID = 0
		o.Setup = &bootstrap.Setup{
			StateDir: *stateDir, SpoolDir: *spoolDir, UnitDir: "/etc/systemd/system", UID: 0,
			Units: pullgate.Units(), Enable: pullgate.Enable,
			ListenerUser: o.ListenerGroup, Useradd: "/usr/sbin/useradd", Systemctl: doctor.Systemctl,
			Doctor: func(ctx context.Context, before bool) int {
				return doctor.Run(ctx, &doctor.Options{
					Version: version, ConfigDir: *configDir, StateDir: *stateDir, SpoolDir: *spoolDir,
					UnitDir: "/etc/systemd/system", Listener: filepath.Join(*configDir, "listener.toml"), UID: 0,
					Units: pullgate.Units(), Enable: pullgate.Enable, BeforeEnable: before,
					Systemctl: doctor.Systemctl, ReadAs: doctor.ReadAs, Out: stdout,
				})
			},
		}
		if euid != 0 {
			log.Error("bootstrap", "err", "it must run as root. Fix: run it with sudo, or as root")
			return 1
		}
		if err := bootstrap.Run(ctx, o); err != nil {
			log.Error("bootstrap", "err", err)
			return 1
		}
		return 0
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
	case doctor.ReadAsCommand: // started by pullgate doctor only
		if len(rest) != 2 {
			return usageErr(stderr, "pullgate "+doctor.ReadAsCommand+" takes one path")
		}
		if err := doctor.ReadOne(rest[1]); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	case "recipe":
		if len(rest) >= 3 && (rest[1] == "update" || rest[1] == "use") {
			return recipeUpdate(ctx, log, rest[1:], *configDir, euid, stdout, stderr)
		}
		if len(rest) < 3 || rest[1] != "fetch" {
			return usageErr(stderr, "give pullgate recipe fetch URL, recipe update NAME --version X.Y.Z, or recipe use NAME X.Y.Z")
		}
		rf := flag.NewFlagSet("recipe", flag.ContinueOnError)
		rf.SetOutput(io.Discard)
		f := &recipe.Fetcher{Dir: recipe.DefaultDir, UID: 0, Out: stdout}
		rf.StringVar(&f.GH, "gh", "/usr/bin/gh", "")
		rf.StringVar(&f.Bundle, "bundle", "", "")
		rf.StringVar(&f.Workflow, "signer-workflow", "", "")
		rf.StringVar(&f.Dir, "dir", f.Dir, "")
		if err := rf.Parse(rest[3:]); err != nil || rf.NArg() > 0 {
			return usageErr(stderr, "unknown arguments after pullgate recipe fetch URL")
		}
		if euid != 0 {
			log.Error("recipe", "err", "it must run as root: it writes to "+f.Dir+". Fix: run it with sudo, or as root")
			return 1
		}
		res, err := f.Fetch(ctx, rest[2])
		if err != nil {
			log.Error("recipe", "err", err)
			return 1
		}
		_, _ = fmt.Fprintln(stdout, res.Dir)
		return 0
	case "doctor":
		args := rest[1:]
		name := ""
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			name, args = args[0], args[1:]
		}
		df := flag.NewFlagSet("doctor", flag.ContinueOnError)
		df.SetOutput(io.Discard)
		tg := df.Bool("telegram", false, "")
		path := df.String("config", filepath.Join(*configDir, "listener.toml"), "")
		unitDir := df.String("unit-dir", "/etc/systemd/system", "")
		if err := df.Parse(args); err != nil || df.NArg() > 0 {
			return usageErr(stderr, "unknown arguments after pullgate doctor")
		}
		if name != "" && !config.ValidName(name) {
			return usageErr(stderr, fmt.Sprintf("%q is not an instance name", name))
		}
		if euid != 0 {
			log.Error("doctor", "err", "it must run as root, to read the files of root and to read the outbox as the listener user. Fix: run it with sudo, or as root")
			return 1
		}
		n := doctor.Run(ctx, &doctor.Options{
			Version: version, ConfigDir: *configDir, StateDir: *stateDir, SpoolDir: *spoolDir,
			UnitDir: *unitDir, Listener: *path, Name: name, Telegram: *tg, UID: 0,
			Units: pullgate.Units(), Enable: pullgate.Enable,
			Systemctl: doctor.Systemctl, ReadAs: doctor.ReadAs, Out: stdout,
		})
		if n > 0 {
			return 1
		}
		return 0
	case "units":
		if len(rest) > 1 {
			return usageErr(stderr, "pullgate units takes no arguments")
		}
		b, err := json.Marshal(selfupdate.UnitList{Units: pullgate.Units(), Enable: pullgate.Enable})
		if err != nil {
			log.Error("units", "err", err)
			return 1
		}
		_, _ = fmt.Fprintln(stdout, string(b))
		return 0
	case "update":
		uf := flag.NewFlagSet("update", flag.ContinueOnError)
		uf.SetOutput(io.Discard)
		path := uf.String("config", "/etc/pullgate/listener.toml", "")
		unitDir := uf.String("unit-dir", "/etc/systemd/system", "")
		if err := uf.Parse(rest[1:]); err != nil || uf.NArg() > 0 {
			return usageErr(stderr, "unknown arguments after pullgate update")
		}
		log = log.With("step", "update")
		return update(ctx, log, stderr, *path, *unitDir, *stateDir, *spoolDir, euid)
	default:
		return usageErr(stderr, fmt.Sprintf("unknown command %q", cmd))
	}
}

// recipeUpdate is "pullgate recipe update NAME ..." and "pullgate recipe
// use NAME X.Y.Z" (DESIGN.md section 21).
func recipeUpdate(ctx context.Context, log *slog.Logger, args []string, configDir string, euid uint32, stdout, stderr io.Writer) int {
	u := &bootstrap.Update{EtcDir: configDir, Name: args[1], Use: args[0] == "use", Out: stdout, Now: time.Now,
		Fetcher: &recipe.Fetcher{Dir: recipe.DefaultDir, UID: 0, Out: stdout}}
	flags := args[2:]
	if u.Use {
		if len(args) < 3 {
			return usageErr(stderr, "give pullgate recipe use NAME X.Y.Z")
		}
		u.Version, flags = args[2], args[3:]
	}
	rf := flag.NewFlagSet("recipe", flag.ContinueOnError)
	rf.SetOutput(io.Discard)
	rf.StringVar(&u.Fetcher.Dir, "dir", u.Fetcher.Dir, "")
	if !u.Use {
		rf.StringVar(&u.Version, "version", "", "")
		rf.StringVar(&u.URL, "url", "", "")
		rf.BoolVar(&u.Reinstall, "reinstall", false, "")
		rf.StringVar(&u.Fetcher.GH, "gh", "/usr/bin/gh", "")
	}
	if err := rf.Parse(flags); err != nil || rf.NArg() > 0 || (!u.Use && (u.Version == "") == (u.URL == "")) {
		return usageErr(stderr, "usage: "+strings.ReplaceAll(bootstrap.UpdateUsage, "\n", " or "))
	}
	if euid != 0 {
		log.Error("recipe", "err", "it must run as root: it writes "+configDir+" and "+u.Fetcher.Dir+". Fix: run it with sudo, or as root")
		return 1
	}
	if err := u.Run(ctx); err != nil {
		log.Error("recipe", "err", err)
		return 1
	}
	return 0
}

func done(log *slog.Logger, res string, err error) int {
	if err != nil {
		log.Error("failed", "result", res, "err", err)
		return 1
	}
	log.Info("done", "result", res)
	return 0
}

// update is "pullgate update" (DESIGN.md section 15).
func update(ctx context.Context, log *slog.Logger, stderr io.Writer, path, unitDir, stateDir, spoolDir string, euid uint32) int {
	cfg, err := config.LoadListenerForUpdate(path, euid)
	if err != nil {
		log.Error("config", "err", err)
		return 78
	}
	owners, err := gate.ResolveOwners(cfg.Updates.ListenerUser)
	if err != nil {
		log.Error("setup", "err", err)
		return 78
	}
	bin, err := os.Executable()
	if err != nil || filepath.Base(bin) != "pullgate" || !filepath.IsAbs(bin) {
		log.Error("setup: cannot tell where this binary is, or it is not named pullgate. Fix: install it as /usr/local/bin/pullgate, as the README says", "path", bin)
		return 78
	}
	host, _ := os.Hostname()
	u := &selfupdate.Updater{
		Cfg:          cfg,
		StateDir:     filepath.Join(stateDir, spool.SelfName),
		Spool:        spool.Dirs{Base: cfg.SpoolDir, Name: spool.SelfName},
		Owners:       owners,
		UID:          euid,
		Bin:          bin,
		Version:      version,
		Script:       pullgate.InstallScript,
		UnitDir:      unitDir,
		Host:         host,
		Path:         selfupdate.RootPath,
		Arch:         runtime.GOARCH,
		Systemctl:    systemctl(log),
		Log:          log,
		Out:          stderr,
		ListenerWait: 5 * time.Second,
	}
	if spoolDir != "/var/spool/pullgate" {
		u.Spool.Base = spoolDir
	}
	res, err := u.Run(ctx)
	return done(log, string(res), err)
}

// systemctl runs systemctl, at most 2 minutes.
func systemctl(log *slog.Logger) selfupdate.Systemctl {
	return func(parent context.Context, args ...string) error {
		ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
		defer cancel()
		prog := "/usr/bin/systemctl"
		if _, err := os.Stat(prog); err != nil {
			prog = "/bin/systemctl"
		}
		cmd := exec.CommandContext(ctx, prog, args...) //nolint:gosec // systemctl of the system, with fixed arguments from pullgate
		cmd.Env = []string{"PATH=" + selfupdate.RootPath, "LC_ALL=C"}
		out, err := cmd.CombinedOutput()
		if err != nil && len(args) > 0 && args[0] != "is-enabled" && args[0] != "is-active" {
			log.Warn("systemctl failed", "args", args, "err", err, "output", string(out))
		}
		return err
	}
}
