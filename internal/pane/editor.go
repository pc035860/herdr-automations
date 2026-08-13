package pane

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// pagerCommand shows a captured run's output. A pager, not $EDITOR: this is
// a terminal transcript to scroll, not a file to change.
func pagerCommand(path string) *exec.Cmd {
	// $PAGER routinely carries flags ("less -R"), so it is a command line, not
	// an executable name.
	if pager := strings.Fields(os.Getenv("PAGER")); len(pager) > 0 {
		return exec.Command(pager[0], append(pager[1:], path)...)
	}
	if _, err := exec.LookPath("less"); err == nil {
		return exec.Command("less", "-R", path)
	}
	return exec.Command("cat", path)
}

// editorCommand opens the config file at line, using $VISUAL/$EDITOR and each
// editor's own way of jumping to a line. Unknown editors just get the path.
func editorCommand(path string, line int) *exec.Cmd {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	if line <= 0 {
		return exec.Command(editor, path)
	}

	switch filepath.Base(editor) {
	case "vi", "vim", "nvim", "nano", "emacs", "kak":
		return exec.Command(editor, "+"+strconv.Itoa(line), path)
	case "hx", "helix":
		return exec.Command(editor, path+":"+strconv.Itoa(line))
	case "code", "codium", "cursor", "zed":
		return exec.Command(editor, "--goto", path+":"+strconv.Itoa(line))
	default:
		return exec.Command(editor, path)
	}
}
