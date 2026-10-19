// Clipboard images for the full screen: Ctrl+V with a screenshot in the
// clipboard inserts an [Image N] placeholder in the input box and the image
// goes to the model with the message, like OpenCode.
//
// The terminal never delivers image bytes: Windows Terminal < 1.25 sends an
// EMPTY bracketed paste when the clipboard holds only an image, and some
// terminals send a raw Ctrl+V (0x16). In both cases bh reads the clipboard
// from the OS itself.
package cli

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
)

// clipboardResult is what the OS clipboard held: an image saved to the
// requested path (image == true), or text.
type clipboardResult struct {
	image bool
	text  string
}

// clipboardRead reads the OS clipboard, saving an image as PNG at dest. A
// variable so tests never touch the real clipboard.
var clipboardRead = readOSClipboard

// errNoClipboard: the clipboard has neither an image nor text.
var errNoClipboard = errors.New("clipboard is empty")

// winClipboardScript saves the clipboard image as PNG at $env:BH_PASTE_DEST
// and prints IMAGE; without an image it prints TEXT: and the clipboard text.
// The path goes through the environment so it needs no quoting. Clipboard
// access requires an STA thread (-STA).
const winClipboardScript = `$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$img = [System.Windows.Forms.Clipboard]::GetImage()
if ($img -ne $null) {
  $img.Save($env:BH_PASTE_DEST, [System.Drawing.Imaging.ImageFormat]::Png)
  [Console]::Write('IMAGE')
  exit 0
}
if ([System.Windows.Forms.Clipboard]::ContainsText()) {
  [Console]::Write('TEXT:' + [System.Windows.Forms.Clipboard]::GetText())
  exit 0
}
exit 1`

// readOSClipboard is the best-effort reader per platform: PowerShell on
// Windows, pngpaste/osascript on macOS, wl-paste/xclip on Linux.
func readOSClipboard(dest string) (clipboardResult, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return clipboardResult{}, err
	}
	switch goruntime.GOOS {
	case "windows":
		cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-STA", "-Command", winClipboardScript)
		cmd.Env = append(os.Environ(), "BH_PASTE_DEST="+dest)
		out, err := cmd.Output()
		s := string(out)
		switch {
		case strings.HasPrefix(s, "IMAGE") && fileHasData(dest):
			return clipboardResult{image: true}, nil
		case strings.HasPrefix(s, "TEXT:"):
			return clipboardResult{text: strings.TrimPrefix(s, "TEXT:")}, nil
		case err != nil && cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 1:
			return clipboardResult{}, errNoClipboard
		case err != nil:
			return clipboardResult{}, fmt.Errorf("powershell: %w", err)
		}
		return clipboardResult{}, errNoClipboard
	case "darwin":
		if _, err := exec.LookPath("pngpaste"); err == nil {
			if exec.Command("pngpaste", dest).Run() == nil && fileHasData(dest) {
				return clipboardResult{image: true}, nil
			}
		} else {
			script := []string{
				"-e", fmt.Sprintf("set f to open for access POSIX file %q with write permission", dest),
				"-e", "write (the clipboard as «class PNGf») to f",
				"-e", "close access f",
			}
			if exec.Command("osascript", script...).Run() == nil && fileHasData(dest) {
				return clipboardResult{image: true}, nil
			}
			_ = os.Remove(dest) // osascript leaves an empty file when there is no image
		}
		return clipboardText(exec.Command("pbpaste"))
	default:
		var img, txt *exec.Cmd
		if os.Getenv("WAYLAND_DISPLAY") != "" {
			if _, err := exec.LookPath("wl-paste"); err == nil {
				img = exec.Command("wl-paste", "--type", "image/png")
				txt = exec.Command("wl-paste", "--no-newline")
			}
		}
		if img == nil {
			if _, err := exec.LookPath("xclip"); err != nil {
				return clipboardResult{}, errors.New("install wl-paste or xclip to paste images")
			}
			img = exec.Command("xclip", "-selection", "clipboard", "-t", "image/png", "-o")
			txt = exec.Command("xclip", "-selection", "clipboard", "-o")
		}
		if out, err := img.Output(); err == nil && bytes.HasPrefix(out, []byte("\x89PNG")) {
			if err := os.WriteFile(dest, out, 0o644); err != nil {
				return clipboardResult{}, err
			}
			return clipboardResult{image: true}, nil
		}
		return clipboardText(txt)
	}
}

// clipboardText runs the platform's text paste as the fallback.
func clipboardText(cmd *exec.Cmd) (clipboardResult, error) {
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return clipboardResult{}, errNoClipboard
	}
	return clipboardResult{text: string(out)}, nil
}

func fileHasData(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Size() > 0
}

// attachmentPath is where pasted image n is saved:
// <config dir>/attachments/<timestamp>-<n>.png.
func attachmentPath(n int) string {
	stamp := time.Now().Format("20060102-150405.000")
	stamp = strings.ReplaceAll(stamp, ".", "")
	return filepath.Join(config.Dir(), "attachments", fmt.Sprintf("%s-%d.png", stamp, n))
}

// pasteClipboard reads the clipboard for image number n: the saved path when
// it held an image, otherwise its text.
func pasteClipboard(n int) (path, text string, err error) {
	dest := attachmentPath(n)
	res, err := clipboardRead(dest)
	if err != nil {
		return "", "", err
	}
	if res.image {
		return dest, "", nil
	}
	return "", res.text, nil
}

// pastedImage is an image pasted into the input box, shown as [Image n].
type pastedImage struct {
	n    int
	path string
}

func imageLabel(n int) string { return "[Image " + strconv.Itoa(n) + "]" }

// placeholderRe matches the [Image N] placeholders in the typed text.
var placeholderRe = regexp.MustCompile(`\[Image \d+\]`)

// selectImages returns the paths of the images whose [Image N] placeholder is
// still in the text, in placeholder order (a deleted placeholder drops its image).
func selectImages(text string, imgs []pastedImage) []string {
	var out []string
	used := map[int]bool{}
	for _, m := range placeholderRe.FindAllString(text, -1) {
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(m, "[Image "), "]"))
		if err != nil || used[n] {
			continue
		}
		for _, im := range imgs {
			if im.n == n {
				used[n] = true
				out = append(out, im.path)
				break
			}
		}
	}
	return out
}

// pasteText normalizes a text paste for the single-line input box: line
// breaks become spaces (a pasted newline must not send the message) and other
// control characters are dropped.
func pasteText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case r < 32 || r == 127:
			return -1
		}
		return r
	}, s)
}

// highlightImages paints the attachment tokens the way OpenCode does: fg
// background.base on bg feedback.warning, bold — the inverted "chip" used for
// [Image N], [PDF N] and [Pasted ~N lines].
func highlightImages(s, base string) string {
	locs := placeholderRe.FindAllStringIndex(s, -1)
	if len(locs) == 0 || !colorsOn {
		return color(base, s)
	}
	var sb strings.Builder
	prev := 0
	for _, l := range locs {
		if l[0] > prev {
			sb.WriteString(color(base, s[prev:l[0]]))
		}
		sb.WriteString(boldf(bgWarning + colorText + s[l[0]:l[1]]))
		prev = l[1]
	}
	if prev < len(s) {
		sb.WriteString(color(base, s[prev:]))
	}
	return sb.String()
}

// clipboardWrite puts text on the OS clipboard. A variable so tests never
// touch the real clipboard. The terminal clipboard (OSC 52) is written by the
// screen itself; this is the native half of OpenCode's "all-available".
var clipboardWrite = writeOSClipboard

// writeOSClipboard writes text with the platform tool, feeding it on stdin:
// PowerShell Set-Clipboard on Windows, pbcopy on macOS, wl-copy or xclip on
// Linux.
func writeOSClipboard(text string) error {
	var cmd *exec.Cmd
	switch goruntime.GOOS {
	case "windows":
		cmd = exec.Command("powershell", "-NoProfile", "-NonInteractive", "-STA", "-Command",
			"[Console]::InputEncoding = [System.Text.Encoding]::UTF8; $t = [Console]::In.ReadToEnd(); Set-Clipboard -Value $t")
	case "darwin":
		cmd = exec.Command("pbcopy")
	default:
		if _, err := exec.LookPath("wl-copy"); err == nil && os.Getenv("WAYLAND_DISPLAY") != "" {
			cmd = exec.Command("wl-copy")
		} else if _, err := exec.LookPath("xclip"); err == nil {
			cmd = exec.Command("xclip", "-selection", "clipboard")
		} else {
			return errors.New("install wl-copy or xclip to copy")
		}
	}
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}

// osc52 is the terminal escape that sets the system clipboard (OSC 52), the
// way OpenCode writes it alongside the native clipboard.
func osc52(text string) string {
	return "\033]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\a"
}
