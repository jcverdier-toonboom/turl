package tui

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/jcverdier/turl/internal/runner"
)

var humanSize = runner.HumanSize

var copyCmds = [][]string{{"pbcopy"}, {"wl-copy"}, {"xclip", "-selection", "clipboard"}, {"xsel", "--clipboard", "--input"}}
var pasteCmds = [][]string{{"pbpaste"}, {"wl-paste", "--no-newline"}, {"xclip", "-selection", "clipboard", "-o"}, {"xsel", "--clipboard", "--output"}}

func copyClipboard(s string) error {
	for _, c := range copyCmds {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Stdin = strings.NewReader(s)
		return cmd.Run()
	}
	// OSC 52 works over ssh in most modern terminals.
	fmt.Fprintf(os.Stderr, "\x1b]52;c;%s\x07", base64.StdEncoding.EncodeToString([]byte(s)))
	return nil
}

func pasteClipboard() (string, error) {
	for _, c := range pasteCmds {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		out, err := exec.Command(c[0], c[1:]...).Output()
		return string(out), err
	}
	return "", errors.New("no clipboard tool found (pbpaste, wl-paste, xclip, xsel)")
}
