package gitx

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/code6x/pullgate/internal/safedir"
)

// Limits cap the size of an exported tree.
type Limits struct {
	MaxFiles int
	MaxBytes int64
}

// entry is one line of git ls-tree -r -l -z.
type entry struct {
	mode string
	typ  string
	oid  string
	size int64
	path string
}

func parseLsTree(out []byte) ([]entry, error) {
	var es []entry
	for _, rec := range strings.Split(string(out), "\x00") {
		if rec == "" {
			continue
		}
		meta, path, ok := strings.Cut(rec, "\t")
		if !ok {
			return nil, fmt.Errorf("git ls-tree: bad line %q", meta)
		}
		f := strings.Fields(meta)
		if len(f) != 4 {
			return nil, fmt.Errorf("git ls-tree: bad line %q", meta)
		}
		e := entry{mode: f[0], typ: f[1], oid: f[2], path: path}
		if !ValidSHA(e.oid) {
			return nil, fmt.Errorf("git ls-tree: bad object name %q", e.oid)
		}
		if f[3] != "-" {
			n, err := strconv.ParseInt(f[3], 10, 64)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("git ls-tree: bad size %q", f[3])
			}
			e.size = n
		}
		es = append(es, e)
	}
	return es, nil
}

// CheckPath refuses a tree path that is absolute, empty, or has a part that
// is empty, ".", "..", or ".git" in any case.
func CheckPath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) {
		return fmt.Errorf("the commit has a bad path %q", p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return fmt.Errorf("the commit has a bad path %q (part %q)", p, part)
		}
	}
	return nil
}

// Tree is the result of an export.
type Tree struct {
	Submodules []string  // paths of submodules, which were skipped
	Symlinks   []Symlink // symlinks that point out of the tree
}

// Symlink is a symlink of the tree whose target is absolute or holds "..".
type Symlink struct{ Path, Target string }

// Outside reports whether a symlink target may point out of the tree.
func Outside(target string) bool {
	if strings.HasPrefix(target, "/") {
		return true
	}
	for _, p := range strings.Split(target, "/") {
		if p == ".." {
			return true
		}
	}
	return false
}

// Export writes the files of commit sha to dest, which must not exist.
// It never runs a checkout or git archive, so no filter, no export-subst
// and no export-ignore of .gitattributes applies. Submodules are skipped
// and returned. When the tree is complete, every directory is made
// read-only, and dest gets mode 0750 and group gid (gid < 0: no change).
// The files are written to a temporary name first, and get the name dest
// only when the export is complete.
func (g *Git) Export(ctx context.Context, sha, dest string, lim Limits, gid int) (tree *Tree, err error) {
	if !ValidSHA(sha) {
		return nil, errors.New("export: not a full SHA")
	}
	var submodules []string
	out, err := g.Run(ctx, "ls-tree", "-r", "-l", "-z", "--full-tree", sha)
	if err != nil {
		return nil, err
	}
	es, err := parseLsTree(out)
	if err != nil {
		return nil, err
	}
	if lim.MaxFiles > 0 && len(es) > lim.MaxFiles {
		return nil, fmt.Errorf("the commit has %d files, more than max_files = %d. Fix: raise [limits] max_files in the config, or make the repo smaller", len(es), lim.MaxFiles)
	}
	var total int64
	var blobs []entry
	for _, e := range es {
		if err := CheckPath(e.path); err != nil {
			return nil, err
		}
		switch {
		case e.mode == "160000" && e.typ == "commit":
			submodules = append(submodules, e.path)
		case e.typ == "blob" && (e.mode == "100644" || e.mode == "100755" || e.mode == "120000"):
			total += e.size
			blobs = append(blobs, e)
		default:
			return nil, fmt.Errorf("the commit has %q with an unknown mode %s", e.path, e.mode)
		}
	}
	if lim.MaxBytes > 0 && total > lim.MaxBytes {
		return nil, fmt.Errorf("the commit has %d bytes of files, more than max_tree_bytes = %d. Fix: raise [limits] max_tree_bytes in the config", total, lim.MaxBytes)
	}
	if err := safedir.CheckParents(filepath.Dir(dest), g.UID); err != nil {
		return nil, fmt.Errorf("export refused: %w", err)
	}
	if _, err := os.Lstat(dest); err == nil {
		return nil, fmt.Errorf("the tree %s exists already", dest)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".tmp-")
	if err != nil {
		return nil, fmt.Errorf("make a tree in %s: %w", filepath.Dir(dest), err)
	}
	defer func() {
		if err != nil {
			_ = RemoveTree(tmp)
		}
	}()
	links, err := g.writeBlobs(ctx, tmp, blobs)
	if err != nil {
		return nil, err
	}
	if err := seal(tmp, gid); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return nil, fmt.Errorf("name the tree %s: %w", dest, err)
	}
	return &Tree{Submodules: submodules, Symlinks: links}, nil
}

func (g *Git) writeBlobs(ctx context.Context, dest string, blobs []entry) ([]Symlink, error) {
	if len(blobs) == 0 {
		return nil, nil
	}
	if err := safedir.CheckTree(g.Dir, g.UID); err != nil {
		return nil, fmt.Errorf("git refused: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, g.timeout())
	defer cancel()
	cmd := g.command(ctx, "cat-file", "--batch")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("git cat-file: %w", err)
	}
	werr := make(chan error, 1)
	go func() {
		w := bufio.NewWriter(stdin)
		for _, b := range blobs {
			if _, err := w.WriteString(b.oid + "\n"); err != nil {
				werr <- err
				return
			}
		}
		err := w.Flush()
		_ = stdin.Close()
		werr <- err
	}()
	r := bufio.NewReader(stdout)
	made := map[string]bool{dest: true}
	var rerr error
	var links []Symlink
	for _, b := range blobs {
		var target string
		if target, rerr = writeOne(r, dest, b, made); rerr != nil {
			break
		}
		if b.mode == "120000" && Outside(target) {
			links = append(links, Symlink{Path: b.path, Target: target})
		}
	}
	if rerr != nil {
		_ = cmd.Cancel()
	}
	_ = discard(r, 1<<62)
	waitErr := cmd.Wait()
	if rerr != nil {
		return nil, rerr
	}
	if err := <-werr; err != nil {
		return nil, fmt.Errorf("git cat-file: %w", err)
	}
	if waitErr != nil {
		return nil, fmt.Errorf("git cat-file: %w", waitErr)
	}
	return links, nil
}

func writeOne(r *bufio.Reader, dest string, b entry, made map[string]bool) (target string, err error) {
	head, err := r.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("git cat-file: %w", err)
	}
	f := strings.Fields(head)
	if len(f) != 3 || f[0] != b.oid || f[1] != "blob" {
		return "", fmt.Errorf("git cat-file: unexpected answer %q", strings.TrimSpace(head))
	}
	size, err := strconv.ParseInt(f[2], 10, 64)
	if err != nil || size != b.size {
		return "", fmt.Errorf("git cat-file: wrong size for %q", b.path)
	}
	full, err := mkParents(dest, b.path, made)
	if err != nil {
		return "", err
	}
	if b.mode == "120000" {
		if size > 4096 {
			return "", fmt.Errorf("the symlink %q is too long", b.path)
		}
		buf := make([]byte, size)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", fmt.Errorf("git cat-file: %w", err)
		}
		if strings.ContainsRune(string(buf), 0) {
			return "", fmt.Errorf("the symlink %q holds a NUL", b.path)
		}
		target = string(buf)
		if err := os.Symlink(target, full); err != nil {
			return "", fmt.Errorf("write %q: %w", b.path, err)
		}
	} else {
		fh, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscallNoFollow, 0o600) //nolint:gosec // path checked by CheckPath and mkParents
		if err != nil {
			return "", fmt.Errorf("write %q: %w", b.path, err)
		}
		_, cerr := io.CopyN(fh, r, size)
		mode := os.FileMode(0o444)
		if b.mode == "100755" {
			mode = 0o555
		}
		if cerr == nil {
			cerr = fh.Chmod(mode)
		}
		if err := fh.Close(); cerr == nil {
			cerr = err
		}
		if cerr != nil {
			return "", fmt.Errorf("write %q: %w", b.path, cerr)
		}
	}
	if nl, err := r.ReadByte(); err != nil || nl != '\n' {
		return "", errors.New("git cat-file: bad answer end")
	}
	return target, nil
}

// mkParents makes the directories above path inside dest. Each one must
// be made in this export, so no symlink of the tree can be on the way.
func mkParents(dest, path string, made map[string]bool) (string, error) {
	parts := strings.Split(path, "/")
	dir := dest
	for _, p := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, p)
		if made[dir] {
			continue
		}
		if err := os.Mkdir(dir, 0o700); err != nil {
			return "", fmt.Errorf("write %q: %w", path, err)
		}
		made[dir] = true
	}
	full := filepath.Join(dir, parts[len(parts)-1])
	if !made[filepath.Dir(full)] {
		return "", fmt.Errorf("write %q: parent not made by this export", path)
	}
	return full, nil
}

// seal makes every directory of the tree read-only (0555), and the top
// directory 0750 with group gid.
func seal(dest string, gid int) error {
	var dirs []string
	err := filepath.WalkDir(dest, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && p != dest {
			dirs = append(dirs, p)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("seal the tree: %w", err)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, d := range dirs {
		if err := os.Chmod(d, 0o555); err != nil {
			return fmt.Errorf("seal the tree: %w", err)
		}
	}
	if gid >= 0 && gid != os.Getegid() {
		if err := os.Lchown(dest, -1, gid); err != nil {
			return fmt.Errorf("seal the tree: %w", err)
		}
	}
	if err := os.Chmod(dest, 0o750); err != nil {
		return fmt.Errorf("seal the tree: %w", err)
	}
	return nil
}

// RemoveTree removes an exported tree, whose directories are read-only.
func RemoveTree(dest string) error {
	if _, err := os.Lstat(dest); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	_ = filepath.WalkDir(dest, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700) //nolint:gosec // a tree under root-only directories; WalkDir does not follow its symlinks
		}
		return nil
	})
	return os.RemoveAll(dest)
}
