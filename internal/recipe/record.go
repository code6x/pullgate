package recipe

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"github.com/code6x/pullgate/internal/config"
)

// InstanceRecord is /etc/pullgate/NAME.recipe: the recipe an instance was
// set up from, and the local values its config was made with. pullgate
// status shows it.
type InstanceRecord struct {
	URL            string `toml:"url"`
	Repo           string `toml:"repo"` // OWNER/REPO
	Version        string `toml:"version"`
	File           string `toml:"file"`
	SHA256         string `toml:"sha256"`
	Dir            string `toml:"dir"`      // the version folder
	Instance       string `toml:"instance"` // the instance of the manifest
	Bundle         string `toml:"bundle,omitempty"`
	SignerWorkflow string `toml:"signer_workflow,omitempty"`
	Host           string `toml:"host"` // the value of @HOST@
	Installed      string `toml:"installed"`
}

// RecordPath is the record of the instance name in the config folder.
func RecordPath(etcDir, name string) string { return filepath.Join(etcDir, name+".recipe") }

// Encode is the text of the record.
func (r InstanceRecord) Encode() []byte {
	var b bytes.Buffer
	b.WriteString("# Written by pullgate: the recipe of this instance. pullgate owns this file.\n")
	_ = toml.NewEncoder(&b).Encode(r)
	return b.Bytes()
}

// ReadInstanceRecord reads a record. An unknown key is an error.
func ReadInstanceRecord(path string) (*InstanceRecord, error) {
	b, err := os.ReadFile(path) //nolint:gosec // in the config folder of root
	if err != nil {
		return nil, err
	}
	var r InstanceRecord
	md, err := toml.Decode(string(b), &r)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := config.ExactKeys(md); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if u := md.Undecoded(); len(u) > 0 {
		return nil, fmt.Errorf("%s: unknown key %q", path, u[0].String())
	}
	if !versionRE.MatchString(r.Version) || r.Repo == "" {
		return nil, fmt.Errorf("%s: no repo or no version X.Y.Z", path)
	}
	return &r, nil
}
