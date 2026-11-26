package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/code6x/pullgate/internal/winsecure"
)

func TestWindowsState(t *testing.T) {
	base := filepath.Join(t.TempDir(), "protected")
	if err := winsecure.Mkdir(base); err != nil {
		t.Fatal(err)
	}
	d := Dir{Path: filepath.Join(base, "instance")}
	if err := d.Ensure(); err != nil {
		t.Fatal(err)
	}
	lock, err := d.Lock(false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if other, err := d.Lock(false); !errors.Is(err, ErrBusy) {
		if other != nil {
			_ = other.Close()
		}
		t.Fatalf("second lock returned %v, want ErrBusy", err)
	}
	s, err := d.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{2, 3} {
		s.NextID = id
		if err := d.Save(s); err != nil {
			t.Fatal(err)
		}
	}
	if loaded, err := d.Load(); err != nil || loaded.NextID != 3 {
		t.Fatalf("could not reload replaced state: %v", err)
	}
	if made, err := d.PauseIfNone("operator"); err != nil || !made {
		t.Fatalf("first pause failed: %v", err)
	}
	if made, err := d.PauseIfNone("different"); err != nil || made {
		t.Fatalf("second pause replaced the first: %v", err)
	}
	if resumed, err := d.ResumeTelegram(0); err != nil || resumed {
		t.Fatalf("Telegram resumed an operator pause: %v", err)
	}
	if d.PauseReason() != "operator" {
		t.Fatal("operator pause was lost")
	}
	if err := os.Link(d.StateFile(), filepath.Join(d.Path, "second-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Load(); err == nil {
		t.Fatal("loaded state with multiple hard links")
	}
}
