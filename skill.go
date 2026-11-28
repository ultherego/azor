package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

//go:embed claude/azor/SKILL.md
var skillText []byte

// skillDir is where Claude Code looks for this skill. CLAUDE_CONFIG_DIR moves
// Claude Code's whole configuration, so it is followed too.
func skillDir() string {
	base := os.Getenv("CLAUDE_CONFIG_DIR")
	if base == "" {
		base = filepath.Join(os.Getenv("HOME"), ".claude")
	}
	return filepath.Join(base, "skills", "azor")
}

// cmdInstallSkill writes the skill built into this binary as a plain file.
// A link left by an older installation is replaced by the file; its target
// is not touched. A SKILL.md that is not Azor's is never overwritten.
func cmdInstallSkill(w io.Writer) error {
	dir := skillDir()
	p := filepath.Join(dir, "SKILL.md")
	if i, err := os.Lstat(dir); err == nil && i.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(dir); err != nil { // the link only
			return err
		}
		fmt.Fprintln(w, "removed the old link", short(dir))
	} else if err == nil && !i.IsDir() {
		return fmt.Errorf("%s is not a directory; nothing was changed", short(dir))
	}
	if i, err := os.Lstat(p); err == nil {
		if !i.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file; nothing was changed", short(p))
		}
		old, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if bytes.Equal(old, skillText) {
			fmt.Fprintln(w, short(p), "is up to date")
			printHook(w)
			return nil
		}
		if !isAzorSkill(old) {
			return fmt.Errorf("%s is not Azor's skill; nothing was changed", short(p))
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, skillText, 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	fmt.Fprintln(w, "installed", short(p))
	printHook(w)
	return nil
}

// isAzorSkill reads only the front matter: "name: azor".
func isAzorSkill(b []byte) bool {
	s := string(b)
	if !strings.HasPrefix(s, "---\n") {
		return false
	}
	head, _, ok := strings.Cut(s[4:], "\n---")
	if !ok {
		return false
	}
	for _, l := range strings.Split(head, "\n") {
		if strings.TrimSpace(l) == "name: azor" {
			return true
		}
	}
	return false
}

// printHook shows the hook to add by hand: settings.json is the user's file
// and is never edited here.
func printHook(w io.Writer) {
	bin, err := os.Executable()
	if err != nil {
		bin = "azor"
	} else if r, err := filepath.EvalSymlinks(bin); err == nil {
		bin = r
	}
	fmt.Fprintf(w, `
For one line of the current state when a session starts, add this hook to
~/.claude/settings.json yourself (Azor does not edit it):

  "hooks": { "SessionStart": [ { "matcher": "startup|resume",
    "hooks": [ { "type": "command", "command": %q, "timeout": 3 } ] } ] }
`, fmt.Sprintf("%q brief 2>/dev/null || true", bin))
}
