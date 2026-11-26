package doctor

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// RunPublic runs the checks that need no secret (Options.Public) and
// returns a short text for Telegram (code6x/pullgate#50): the problems and
// the notes, the number of checks that passed, and the summary. Root runs
// it after it reads a doctor request. Every line goes through Redact, so
// the text holds no IP address, no key or token, no host name of the
// system, and no path outside pullgate's own files.
func RunPublic(ctx context.Context, o *Options) string {
	var buf bytes.Buffer
	c := *o
	c.Public, c.Telegram, c.Out = true, false, &buf
	Run(ctx, &c)
	var keep []string
	oks := 0
	for _, l := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "ok "):
			oks++
		case strings.HasPrefix(l, "PROBLEM ") || strings.HasPrefix(l, "note "):
			keep = append(keep, l)
		case strings.HasPrefix(l, "All checks passed") || strings.Contains(l, "problem(s)"):
			keep = append(keep, fmt.Sprintf("ok       %d checks", oks), l)
		}
	}
	return Redact(strings.Join(keep, "\n"), PublicPaths(o))
}

// PublicPaths are the files of pullgate itself, which a reply may name.
func PublicPaths(o *Options) []string {
	p := []string{o.ConfigDir, o.StateDir, o.SpoolDir, "/usr/local/bin/pullgate", "/usr/local/lib/pullgate"}
	if o.UnitDir != "" {
		p = append(p, filepath.Join(o.UnitDir, "pullgate-*"))
	}
	return p
}

// hostname is os.Hostname; tests give a fake.
var hostname = os.Hostname

var (
	reURL    = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://\S+|[A-Za-z0-9._-]+@[A-Za-z0-9][A-Za-z0-9.-]*:\S+`)
	reSecret = regexp.MustCompile(`-----BEGIN[^-]*-----|\d{5,}:[A-Za-z0-9_-]{20,}|(^|[^A-Za-z0-9+/=_-])[A-Za-z0-9+=_-][A-Za-z0-9+/=_-]{31,}`)
	reIP     = regexp.MustCompile(`[0-9A-Fa-f:.]*[:.][0-9A-Fa-f:.]*`)
	rePath   = regexp.MustCompile(`(^|[\s"'(=])(/[^\s"'(),;<>]*)`)
)

// Redact removes from s what a Telegram reply must not show: URLs and
// git addresses (they name hosts), long runs that may be keys or tokens,
// IP addresses, every absolute path that is not one of the paths in own
// (a path, or a directory and all below it, or a prefix ending in "*"),
// and the host name of the system.
func Redact(s string, own []string) string {
	s = reURL.ReplaceAllString(s, "<address>")
	s = reSecret.ReplaceAllStringFunc(s, func(m string) string {
		if r := m[0]; !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/=_-", rune(r)) {
			return m[:1] + "<secret>"
		}
		return "<secret>"
	})
	s = reIP.ReplaceAllStringFunc(s, func(t string) string {
		if net.ParseIP(strings.Trim(t, ".")) != nil || net.ParseIP(strings.Trim(t, ":.")) != nil {
			return "<ip>"
		}
		return t
	})
	s = rePath.ReplaceAllStringFunc(s, func(m string) string {
		i := strings.Index(m, "/")
		pre, p := m[:i], m[i:]
		rest := ""
		for strings.HasSuffix(p, ".") || strings.HasSuffix(p, ":") {
			rest = p[len(p)-1:] + rest
			p = p[:len(p)-1]
		}
		if ownPath(p, own) {
			return m
		}
		return pre + "<path>" + rest
	})
	if h, err := hostname(); err == nil {
		s = redactHost(s, h)
	}
	return s
}

func ownPath(p string, own []string) bool {
	c := filepath.Clean(p)
	for _, o := range own {
		if o == "" {
			continue
		}
		if pre, ok := strings.CutSuffix(o, "*"); ok {
			if strings.HasPrefix(c, pre) && !strings.Contains(c[len(pre):], "/") {
				return true
			}
			continue
		}
		o = filepath.Clean(o)
		if c == o || strings.HasPrefix(c, o+"/") {
			return true
		}
	}
	return false
}

// redactHost replaces the host name h and its first label, as whole
// words, with <host>.
func redactHost(s, h string) string {
	for _, n := range []string{h, strings.SplitN(h, ".", 2)[0]} {
		if len(n) < 2 {
			continue
		}
		re := regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_-])` + regexp.QuoteMeta(n) + `($|[^A-Za-z0-9_-])`)
		// Twice: a match takes the character around it, so "a a" needs two.
		s = re.ReplaceAllString(re.ReplaceAllString(s, "${1}<host>${2}"), "${1}<host>${2}")
	}
	return s
}
