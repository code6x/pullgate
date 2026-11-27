package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"golang.org/x/sys/windows"

	"github.com/code6x/pullgate/internal/config"
	"github.com/code6x/pullgate/internal/state"
	"github.com/code6x/pullgate/internal/telegram"
	"github.com/code6x/pullgate/internal/winsecure"
)

func installBinary(root string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	b, err := os.ReadFile(exe) //nolint:gosec // the installer verified this executable
	if err != nil {
		return err
	}
	return state.WriteFileAtomic(filepath.Join(root, "pullgate.exe"), b, 0o700)
}

func setupWindows(ctx context.Context, root string, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	name := flags.String("instance", "", "")
	repo := flags.String("repo", "", "")
	branch := flags.String("branch", "main", "")
	apply := flags.String("apply", "", "")
	key := flags.String("deploy-key", "", "")
	hosts := flags.String("known-hosts", "", "")
	owner := flags.Int64("owner-id", 0, "")
	chat := flags.Int64("chat-id", 0, "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || !config.ValidName(*name) || *name == "listener" ||
		*owner <= 0 || *chat != *owner || !strings.EqualFold(filepath.Ext(*apply), ".ps1") || !filepath.IsAbs(*apply) {
		return errors.New("invalid setup options. Fix: pass --instance NAME --repo URL --apply ABSOLUTE_SCRIPT.ps1 --owner-id ID --chat-id ID for a private chat")
	}
	if err := initialize(root); err != nil {
		return err
	}
	lock, err := (state.Dir{Path: root}).Lock(false)
	if err != nil {
		return fmt.Errorf("setup lock: %w. Fix: stop the pullgate task during setup", err)
	}
	defer func() { _ = lock.Close() }()
	cfgPath := filepath.Join(root, *name+".toml")
	listenerPath := filepath.Join(root, "listener.toml")
	dstScript := filepath.Join(root, *name+"-apply.ps1")
	tokenPath := filepath.Join(root, "bot-token.dpapi")
	for _, p := range []string{cfgPath, listenerPath, dstScript, tokenPath} {
		if _, err := os.Lstat(p); err == nil {
			return fmt.Errorf("config %s already exists. Fix: edit the protected config as an administrator", p)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	cfg := map[string]any{
		"repo":     map[string]any{"url": *repo, "branch": *branch, "deploy_key": *key, "known_hosts": *hosts},
		"commands": map[string]any{"apply": []string{dstScript}},
	}
	instance, err := encodeWindowsConfig(cfg)
	if err != nil {
		return err
	}
	parsed, err := config.Parse(cfgPath, *name, instance)
	if err != nil {
		return err
	}
	if strings.HasPrefix(parsed.Repo.URL, "ssh://") {
		for _, path := range []string{*key, *hosts} {
			if err := winsecure.CheckManaged(root, path, false); err != nil {
				return fmt.Errorf("check SSH files: %w. Fix: place the deploy key and known hosts under the protected root", err)
			}
		}
	}
	listenerCfg, err := encodeWindowsConfig(map[string]any{
		"owner_id": *owner, "chat_id": *chat, "instances": []string{*name},
		"updates": map[string]any{"check": false},
	})
	if err != nil {
		return err
	}
	script, err := os.ReadFile(*apply)
	if err != nil {
		return fmt.Errorf("read apply script: %w. Fix: give a readable .ps1 file", err)
	}
	token, err := promptToken(out)
	if err != nil {
		return err
	}
	defer clear(token)
	tctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tg := telegram.New("https://api.telegram.org", string(token))
	chatInfo, err := tg.GetChat(tctx, *chat)
	if err != nil || chatInfo.Type != "private" || chatInfo.ID != *owner {
		return errors.New("cannot verify the private bot chat. Fix: start the bot in Telegram and check the IDs and token")
	}
	protected, err := winsecure.ProtectToken(token)
	if err != nil {
		return err
	}
	var written []string
	for _, file := range []struct {
		path string
		data []byte
	}{
		{dstScript, script},
		{tokenPath, protected},
		{cfgPath, instance},
		{listenerPath, listenerCfg},
	} {
		if err := state.WriteFileAtomic(file.path, file.data, 0o600); err != nil {
			for _, path := range written {
				err = errors.Join(err, os.Remove(path))
			}
			return err
		}
		written = append(written, file.path)
	}
	_, err = fmt.Fprintln(out, "Setup complete. Run pullgate doctor before starting the task.")
	return err
}

func encodeWindowsConfig(v map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	err := toml.NewEncoder(&buf).Encode(v)
	return buf.Bytes(), err
}

func promptToken(out io.Writer) ([]byte, error) {
	h := windows.Handle(os.Stdin.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return nil, errors.New("token entry needs a Windows console. Fix: run setup directly in an elevated PowerShell console")
	}
	if err := windows.SetConsoleMode(h, mode&^windows.ENABLE_ECHO_INPUT); err != nil {
		return nil, errors.New("cannot hide console input. Fix: run setup in an elevated PowerShell console")
	}
	defer func() {
		_ = windows.SetConsoleMode(h, mode)
		_, _ = fmt.Fprintln(out)
	}()
	_, _ = fmt.Fprint(out, "Bot token: ")
	line, err := bufio.NewReaderSize(os.Stdin, 4096).ReadSlice('\n')
	if err != nil {
		clear(line)
		return nil, errors.New("could not read the bot token. Fix: enter the token on one line")
	}
	token := bytes.TrimSpace(line)
	if len(token) == 0 || bytes.ContainsAny(token, " /\r\n\t") {
		clear(line)
		return nil, errors.New("invalid bot token. Fix: enter the token from BotFather")
	}
	return token, nil
}
