package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/windows"

	"github.com/code6x/pullgate/internal/config"
	"github.com/code6x/pullgate/internal/gate"
	"github.com/code6x/pullgate/internal/listener"
	"github.com/code6x/pullgate/internal/state"
	"github.com/code6x/pullgate/internal/telegram"
	"github.com/code6x/pullgate/internal/winsecure"
)

func loadListener(root string) (*config.Listener, error) {
	if err := winsecure.Check(root, true); err != nil {
		return nil, err
	}
	c, err := config.LoadListener(filepath.Join(root, "listener.toml"), 0)
	if err != nil {
		return nil, err
	}
	if c.SpoolDir != filepath.Join(root, "spool") ||
		c.StateFile != filepath.Join(root, "listener-state.json") ||
		c.TokenFile != filepath.Join(root, "bot-token.dpapi") {
		return nil, errors.New("unsupported listener path. Fix: remove custom spool_dir, state_file and token_file settings")
	}
	return c, nil
}

func agent(ctx context.Context, root string, log *slog.Logger, out io.Writer) error {
	git, err := gitExecutable()
	if err != nil {
		return err
	}
	return runAgent(ctx, root, git, log, out)
}

func runAgent(ctx context.Context, root, git string, log *slog.Logger, out io.Writer) error {
	lock, err := (state.Dir{Path: root}).Lock(false)
	if err != nil {
		return fmt.Errorf("start agent: %w. Fix: check that a single pullgate task is running", err)
	}
	defer func() { _ = lock.Close() }()
	c, err := loadListener(root)
	if err != nil {
		return err
	}
	token, err := c.ReadToken(0)
	if err != nil {
		return err
	}
	l := &listener.Listener{Cfg: c, Log: log, TG: telegram.New(c.APIURL, token)}
	if err := winsecure.Check(c.StateFile, false); err != nil && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return err
	}
	if err := l.Load(); err != nil {
		return err
	}
	var gates []*gate.Env
	seen := make(map[string]bool)
	for _, name := range c.Instances {
		if seen[name] {
			return fmt.Errorf("duplicate instance %q. Fix: list each instance once in listener.toml", name)
		}
		seen[name] = true
		e, err := loadGate(root, name, git, log, out)
		if err != nil {
			return err
		}
		if err := e.Spool.Ensure(e.UID, e.Owners); err != nil {
			return err
		}
		gates = append(gates, e)
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	for _, e := range gates {
		wg.Go(func() { runInstance(ctx, e) })
	}
	return l.Run(ctx)
}

func runInstance(ctx context.Context, e *gate.Env) {
	poll := time.NewTicker(2 * time.Second)
	defer poll.Stop()
	var next time.Time
	for ctx.Err() == nil {
		entries, err := os.ReadDir(e.Spool.Inbox())
		if err != nil {
			e.Log.Error("read approval inbox", "err", err)
		} else if len(entries) > 0 {
			if _, err := e.Apply(ctx); err != nil {
				e.Log.Error("apply", "err", err)
			}
		}
		if time.Now().After(next) || e.Spool.HasCheck() {
			if _, err := e.Run(ctx); err != nil {
				e.Log.Error("run", "err", err)
			}
			next = time.Now().Add(checkInterval)
		}
		select {
		case <-ctx.Done():
		case <-poll.C:
		}
	}
}
