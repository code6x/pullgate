package bootstrap

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/code6x/pullgate/internal/config"
)

func (o *Options) listenerPath() string { return filepath.Join(o.EtcDir, "listener.toml") }

// loadPaired is the listener.toml of a host that is paired already
// (--add). It must pass the checks of the listener: owner, mode, keys.
func (o *Options) loadPaired() (*config.Listener, error) {
	l, err := config.LoadListenerForUpdate(o.listenerPath(), uint32(os.Geteuid())) //nolint:gosec // a uid fits in uint32
	if err != nil {
		return nil, fmt.Errorf("--add needs a host that is paired already, with a good %s: %w. Nothing was written. Fix: run pullgate doctor and fix what it says, or run bootstrap without --add to pair again", o.listenerPath(), err)
	}
	if _, err := os.Lstat(filepath.Join(o.EtcDir, "bot-token")); err != nil {
		return nil, fmt.Errorf("--add: %s is missing: %w. Nothing was written. Fix: run bootstrap without --add to pair again", filepath.Join(o.EtcDir, "bot-token"), err)
	}
	return l, nil
}

// addInstances writes listener.toml again with the new instance names
// added. Every other key stays as it was: the token, the IDs and the
// other instances are kept.
func (o *Options) addInstances(gid int) error {
	p := o.listenerPath()
	l, err := o.loadPaired()
	if err != nil {
		return err
	}
	b, err := os.ReadFile(p) //nolint:gosec // root's own config folder, checked just above
	if err != nil {
		return err
	}
	var t map[string]any
	if _, err := toml.Decode(string(b), &t); err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	names := slices.Clone(l.Instances)
	for _, in := range o.Instances {
		if !slices.Contains(names, in.Name) {
			names = append(names, in.Name)
		}
	}
	t["instances"] = names
	var out bytes.Buffer
	fmt.Fprintf(&out, header, o.Now().Format("2006-01-02"))
	if err := toml.NewEncoder(&out).Encode(t); err != nil {
		return err
	}
	if err := writeOwned(p, out.Bytes(), 0o640, gid); err != nil {
		return err
	}
	o.say("Wrote %s: the instances are now %s.", p, strings.Join(names, ", "))
	return nil
}
