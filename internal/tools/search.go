// Search and edit tools: grep, glob and edit. Pure Go, no external binaries,
// so they behave the same on Windows, macOS and Linux.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	maxSearchFileSize = 5 << 20 // files above this are skipped by grep
	binarySniffBytes  = 8 << 10 // a NUL byte in this prefix marks a binary file

	// maxEditManyFiles caps a single edit_many call. It is high enough for a
	// project-wide refactor and low enough that a mistake stays bounded and
	// reviewable; bigger work is split into a few calls on purpose.
	maxEditManyFiles = 50
)

// Walk limits. Without them a grep over a home directory (130k+ files of
// browser caches) ran for many minutes and the turn sent nothing over ACP.
// When one is reached the tool returns what it found so far, with a note.
var (
	searchDeadline = 60 * time.Second
	// maxWalkFiles is a safety net far above any project; what normally ends
	// a broad search early is maxMatchFiles (the output could not show more
	// anyway) or the deadline.
	maxWalkFiles  = 500000
	maxMatchFiles = 1000
)

// errEnough is returned by a visit callback that already has all it can use.
var errEnough = errors.New("enough results")

// walkLimit is the partial-result note; empty when the walk finished.
type walkLimit string

func (l walkLimit) note() string {
	if l == "" {
		return ""
	}
	return fmt.Sprintf("\n[search stopped early: %s; results are partial — narrow path/glob]\n", string(l))
}

// skipDirs are never descended into by grep/glob.
var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	"dist":         true,
}

// globRegexp converts a glob pattern with `**` support into an anchored
// regexp over slash-separated relative paths:
//
//	**/   zero or more directories
//	**    anything, including '/'
//	*     anything except '/'
//	?     one character except '/'
//	[..]  character class (passed through)
func globRegexp(pattern string) (*regexp.Regexp, error) {
	p := filepath.ToSlash(strings.TrimSpace(pattern))
	p = strings.TrimPrefix(p, "./")
	var sb strings.Builder
	sb.WriteString("^")
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch c {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				if i+2 < len(p) && p[i+2] == '/' {
					sb.WriteString("(?:.*/)?")
					i += 2
				} else {
					sb.WriteString(".*")
					i++
				}
			} else {
				sb.WriteString("[^/]*")
			}
		case '?':
			sb.WriteString("[^/]")
		case '[':
			end := strings.IndexByte(p[i:], ']')
			if end < 0 {
				sb.WriteString(`\[`)
				continue
			}
			class := p[i+1 : i+end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			sb.WriteString("[" + class + "]")
			i += end
		default:
			sb.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	sb.WriteString("$")
	return regexp.Compile(sb.String())
}

// globMatcher returns a predicate over slash-separated relative paths. A
// pattern without '/' matches the base name at any depth ("*.go"), like
// ripgrep's --glob.
func globMatcher(pattern string) (func(rel string) bool, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return func(string) bool { return true }, nil
	}
	re, err := globRegexp(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid glob %q: %w", pattern, err)
	}
	if !strings.Contains(filepath.ToSlash(pattern), "/") {
		return func(rel string) bool { return re.MatchString(pathBase(rel)) }, nil
	}
	return re.MatchString, nil
}

func pathBase(rel string) string {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		return rel[i+1:]
	}
	return rel
}

// walkFiles visits every regular file under root (skipping skipDirs and
// cloud-only placeholders), giving the absolute path and the slash-separated
// path relative to root. It stops at searchDeadline or maxWalkFiles and says
// so in the returned walkLimit; a cancelled ctx is still an error.
func walkFiles(ctx context.Context, root string, visit func(abs, rel string, d fs.DirEntry) error) (walkLimit, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.Now().Add(searchDeadline)
	files := 0
	var limit walkLimit
	errStop := fmt.Errorf("stop")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil // unreadable entry: skip, keep walking
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			limit = walkLimit(fmt.Sprintf("time limit of %s reached after %d file(s)", searchDeadline, files))
			return errStop
		}
		if d.IsDir() {
			if p != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || cloudOnly(d) {
			return nil
		}
		if files >= maxWalkFiles {
			limit = walkLimit(fmt.Sprintf("file limit of %d reached", maxWalkFiles))
			return errStop
		}
		files++
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			rel = p
		}
		return visit(p, filepath.ToSlash(rel), d)
	})
	if err == errStop {
		err = nil
	}
	if errors.Is(err, errEnough) {
		limit = walkLimit(fmt.Sprintf("%d matching files found after %d file(s) scanned", maxMatchFiles, files))
		err = nil
	}
	return limit, err
}

// ── grep ─────────────────────────────────────────────────────────────────

func specGrep() Spec {
	return Spec{
		Name: "grep",
		Description: "Search file contents using a regular expression (Go's RE2 syntax), recursively, in pure Go — " +
			"works identically on every OS. Use it to locate specific code, symbols, or text patterns, and narrow searches " +
			"with `path` or `include`. ALWAYS use this instead of shell with grep/findstr/git grep/Select-String. " +
			"Skips .git, node_modules, vendor, dist, binary files and files over 5 MiB. Returns matching file paths, " +
			"line numbers, and line previews; results are sorted by path. " +
			"Examples: {\"pattern\":\"func Send\\\\(\",\"include\":\"*.go\"}; {\"pattern\":\"TODO|FIXME\",\"path\":\"internal\",\"output\":\"files\"}; " +
			"{\"pattern\":\"timeout\",\"caseSensitive\":false,\"context\":2}.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern":          map[string]any{"type": "string", "description": "Go regexp (RE2), e.g. `func \\w+Handler` or `import \"fmt\"`; escape regex metacharacters for literal text"},
				"path":             map[string]any{"type": "string", "description": "directory or single file to search (default: cwd)"},
				"include":          map[string]any{"type": "string", "description": "filename filter, e.g. \"*.go\" or \"internal/**/*_test.go\"; a pattern without '/' matches the base name at any depth; brace sets like {a,b} are not supported"},
				"caseSensitive":    map[string]any{"type": "boolean", "description": "case-sensitive match (default true)"},
				"context":          map[string]any{"type": "integer", "description": "lines of context before and after each match (default 0; content output only)"},
				"output":           map[string]any{"type": "string", "enum": []string{"content", "files", "count"}, "description": "\"content\" (default): `path:line: text`; \"files\": matching paths only; \"count\": matches per file"},
			},
			"required": []string{"pattern"},
		},
		Exec: execGrep,
	}
}

func execGrep(ctx context.Context, cwd string, raw json.RawMessage) (string, error) {
	var c struct {
		Pattern       string `json:"pattern"`
		Path          string `json:"path"`
		Include       string `json:"include"`
		CaseSensitive bool   `json:"caseSensitive"`
		Context       int    `json:"context"`
		Output        string `json:"output"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	if c.Pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}
	expr := c.Pattern
	if !c.CaseSensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return "", fmt.Errorf("invalid regexp: %w", err)
	}
	mode := strings.ToLower(strings.TrimSpace(c.Output))
	if mode == "" {
		mode = "content"
	}
	if mode != "content" && mode != "files" && mode != "count" {
		return "", fmt.Errorf("output must be \"content\", \"files\" or \"count\", got %q", c.Output)
	}
	if c.Context < 0 {
		c.Context = 0
	}
	match, err := globMatcher(c.Include)
	if err != nil {
		return "", err
	}
	root := resolvePath(cwd, c.Path)
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}

	type fileHit struct {
		display string
		lines   []string // rendered content lines (content mode)
		count   int
	}
	var hits []fileHit
	scan := func(abs, display string) {
		fi, err := os.Stat(abs)
		if err != nil || fi.Size() > maxSearchFileSize {
			return
		}
		b, err := os.ReadFile(abs)
		if err != nil {
			return
		}
		if bytes.IndexByte(b[:min(len(b), binarySniffBytes)], 0) >= 0 {
			return
		}
		text := strings.ReplaceAll(string(b), "\r\n", "\n")
		lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
		h := fileHit{display: display}
		lastPrinted := -1
		for i, line := range lines {
			if !re.MatchString(line) {
				continue
			}
			h.count++
			if mode != "content" {
				continue
			}
			from := max(0, i-c.Context)
			if lastPrinted >= 0 && from > lastPrinted+1 && c.Context > 0 {
				h.lines = append(h.lines, "--")
			}
			from = max(from, lastPrinted+1)
			to := min(len(lines)-1, i+c.Context)
			for j := from; j <= to; j++ {
				sep := "-"
				if re.MatchString(lines[j]) {
					sep = ":"
				}
				l := lines[j]
				if len(l) > 512 {
					l = l[:512] + "…"
				}
				h.lines = append(h.lines, fmt.Sprintf("%s:%d%s %s", display, j+1, sep, l))
			}
			lastPrinted = to
		}
		if h.count > 0 {
			hits = append(hits, h)
		}
	}
	var limit walkLimit
	if !info.IsDir() {
		scan(root, filepath.ToSlash(c.Path))
	} else {
		limit, err = walkFiles(ctx, root, func(abs, rel string, _ fs.DirEntry) error {
			if match(rel) {
				scan(abs, rel)
			}
			if len(hits) >= maxMatchFiles {
				return errEnough
			}
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].display < hits[j].display })
	if len(hits) == 0 {
		return "no matches" + limit.note(), nil
	}

	total := 0
	for _, h := range hits {
		total += h.count
	}
	var sb strings.Builder
	omitted := 0
	budget := outputCap - 256
	for i, h := range hits {
		var chunk string
		switch mode {
		case "files":
			chunk = h.display + "\n"
		case "count":
			chunk = fmt.Sprintf("%s: %d\n", h.display, h.count)
		default:
			chunk = strings.Join(h.lines, "\n") + "\n"
		}
		if sb.Len()+len(chunk) > budget {
			for _, rest := range hits[i:] {
				omitted += rest.count
			}
			fmt.Fprintf(&sb, "\n[output truncated: %d more match(es) in %d file(s) omitted; narrow path/glob/pattern]\n", omitted, len(hits)-i)
			break
		}
		sb.WriteString(chunk)
	}
	if omitted == 0 && mode != "files" {
		fmt.Fprintf(&sb, "(%d match(es) in %d file(s))\n", total, len(hits))
	}
	return truncate(sb.String(), outputCap-256) + limit.note(), nil
}

// ── glob ─────────────────────────────────────────────────────────────────

func specGlob() Spec {
	return Spec{
		Name: "glob",
		Description: "Search file paths using a glob pattern (examples: \"**/*.go\", \"internal/**/*_test.go\"), in pure Go — " +
			"works identically on every OS. Use it instead of shell with dir/find/ls to locate files. " +
			"Returns paths relative to the base dir, newest modification first. Skips .git, node_modules, vendor, dist. " +
			"(A pattern without '/' matches only at the top level of the base dir; use **/ to recurse.)",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string", "description": "glob over the relative path: * (no '/'), ** (any depth), ?, [abc]"},
				"path":    map[string]any{"type": "string", "description": "base directory (default: cwd)"},
			},
			"required": []string{"pattern"},
		},
		Exec: execGlob,
	}
}

func execGlob(ctx context.Context, cwd string, raw json.RawMessage) (string, error) {
	var c struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	if strings.TrimSpace(c.Pattern) == "" {
		return "", fmt.Errorf("pattern is required")
	}
	re, err := globRegexp(c.Pattern)
	if err != nil {
		return "", fmt.Errorf("invalid glob %q: %w", c.Pattern, err)
	}
	root := resolvePath(cwd, c.Path)
	type entry struct {
		rel string
		mod int64
	}
	var found []entry
	limit, err := walkFiles(ctx, root, func(_, rel string, d fs.DirEntry) error {
		if !re.MatchString(rel) {
			return nil
		}
		var mod int64
		if info, err := d.Info(); err == nil {
			mod = info.ModTime().UnixNano()
		}
		found = append(found, entry{rel, mod})
		if len(found) >= maxMatchFiles*10 {
			return errEnough
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(found) == 0 {
		return "no files match " + c.Pattern + limit.note(), nil
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].mod != found[j].mod {
			return found[i].mod > found[j].mod
		}
		return found[i].rel < found[j].rel
	})
	var sb strings.Builder
	budget := outputCap - 256
	for i, f := range found {
		if sb.Len()+len(f.rel)+1 > budget {
			fmt.Fprintf(&sb, "\n[output truncated: %d more file(s) omitted; narrow the pattern]\n", len(found)-i)
			return sb.String() + limit.note(), nil
		}
		sb.WriteString(f.rel + "\n")
	}
	fmt.Fprintf(&sb, "(%d file(s))\n", len(found))
	return sb.String() + limit.note(), nil
}

// ── edit ─────────────────────────────────────────────────────────────────

func specEdit() Spec {
	return Spec{
		Name: "edit",
		Description: "Edit the contents of a file by finding and replacing exact text. When editing text from read " +
			"output, preserve the exact indentation (tabs or spaces) and omit the line-number prefix, such as `1: `. " +
			"Never include the prefix in oldString or newString values: write line breaks as \\n and the file's " +
			"existing CRLF or LF line endings are matched and preserved. " +
			"By default oldString must appear EXACTLY ONCE (include enough surrounding lines to make it unique); " +
			"multiple matches FAIL unless replaceAll is true — set it to replace every occurrence, e.g. renaming a variable. " +
			"Prefer this over write for any change to an existing file: send only the changed region, never the whole file. " +
			"To create or overwrite a whole file use write.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":        map[string]any{"type": "string", "description": "file to edit"},
				"oldString":  map[string]any{"type": "string", "description": "exact text to replace (non-empty)"},
				"newString":  map[string]any{"type": "string", "description": "replacement text (may be empty to delete)"},
				"replaceAll": map[string]any{"type": "boolean", "description": "replace every occurrence (default false: exactly one must exist)"},
			},
			"required": []string{"path", "oldString", "newString"},
		},
		Exec: execEdit,
	}
}

func execEdit(_ context.Context, cwd string, raw json.RawMessage) (string, error) {
	var c struct {
		Path       string `json:"path"`
		OldString  string `json:"oldString"`
		NewString  string `json:"newString"`
		ReplaceAll bool   `json:"replaceAll"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	if c.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	if c.OldString == "" {
		return "", fmt.Errorf("oldString is empty; to create or overwrite a whole file use write")
	}
	if c.OldString == c.NewString {
		return "", fmt.Errorf("oldString and newString are identical; nothing to change")
	}
	p := resolvePath(cwd, c.Path)
	info, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	content := string(b)
	oldS, newS := c.OldString, c.NewString
	if strings.Contains(content, "\r\n") {
		oldS = toCRLF(oldS)
		newS = toCRLF(newS)
	}
	n := strings.Count(content, oldS)
	if n == 0 {
		return "", fmt.Errorf("oldString not found in %s; re-read the file and copy the text exactly (indentation, whitespace, no line-number prefixes)", c.Path)
	}
	if n > 1 && !c.ReplaceAll {
		return "", fmt.Errorf("oldString found %d times in %s; add more surrounding context to make it unique, or set replaceAll to true", n, c.Path)
	}
	if c.ReplaceAll {
		content = strings.ReplaceAll(content, oldS, newS)
	} else {
		content = strings.Replace(content, oldS, newS, 1)
	}
	if err := os.WriteFile(p, []byte(content), info.Mode().Perm()); err != nil {
		return "", err
	}
	return fmt.Sprintf("ok: %d replacement(s) in %s", n, c.Path), nil
}

// toCRLF turns bare LF into CRLF, leaving existing CRLF intact.
func toCRLF(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}

// ── edit_many: several exact replacements in several files ──────────────────

// UmEdicao is one replacement: the same contract as edit's arguments, plus the
// path. decrescente order has no meaning — every edit is independent.
type UmEdicao struct {
	Path       string `json:"path"`
	OldString  string `json:"oldString"`
	NewString  string `json:"newString"`
	ReplaceAll bool   `json:"replaceAll"`
}

func specEditMany() Spec {
	return Spec{
		Name: "edit_many",
		Description: "Applies several EXACT string replacements across several files in ONE call, the way a multi-file patch does. " +
			"Each edit has the same contract as the `edit` tool (oldString must appear exactly once in its file, unless " +
			"replaceAll is set; CRLF is handled). Use it for a refactor that touches the same region of many files — one call " +
			"instead of one `edit` per file, which is cheaper in tokens and rounds. Every edit is validated against the " +
			"file's CURRENT content before anything is written: if ANY edit fails its match, NOTHING is written and the " +
			"result names the edits that failed. To change one file only, prefer `edit`.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"edits": map[string]any{
					"type":        "array",
					"description": "the replacements to apply; order is irrelevant",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path":        map[string]any{"type": "string", "description": "file to edit"},
							"oldString":  map[string]any{"type": "string", "description": "exact text to replace (non-empty)"},
							"newString":  map[string]any{"type": "string", "description": "replacement text (may be empty to delete)"},
							"replaceAll": map[string]any{"type": "boolean", "description": "replace every occurrence (default false: exactly one must exist)"},
						},
						"required": []string{"path", "oldString", "newString"},
					},
				},
			},
			"required": []string{"edits"},
		},
		Exec: execEditMany,
	}
}

// editResult is one planned write: the file, its new content and the number of
// replacements that produced it.
type editResult struct {
	path string
	body string
	mode fs.FileMode
	n    int
}

func execEditMany(_ context.Context, cwd string, raw json.RawMessage) (string, error) {
	var c struct {
		Edits []UmEdicao `json:"edits"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	if len(c.Edits) == 0 {
		return "", fmt.Errorf("edits is empty; to change a single file use the `edit` tool")
	}
	if len(c.Edits) > maxEditManyFiles {
		return "", fmt.Errorf("%d edits in one call exceeds the limit of %d; split the work into a few edit_many calls", len(c.Edits), maxEditManyFiles)
	}

	// ── phase 1: plan every write, against the CURRENT content ────────────
	// The edits are grouped per file so two edits on the same file compose
	// (the second sees the first's result), and an unmatched edit on file #12
	// leaves files #1..#11 untouched.
	order := make([]string, 0, len(c.Edits))
	pending := map[string][]UmEdicao{}
	for i, e := range c.Edits {
		e.Path = strings.TrimSpace(e.Path)
		if e.Path == "" {
			return "", fmt.Errorf("edit %d: path is required", i)
		}
		if e.OldString == "" {
			return "", fmt.Errorf("edit %d (%s): oldString is empty; to create or overwrite a whole file use write", i, e.Path)
		}
		if _, dup := pending[e.Path]; !dup {
			order = append(order, e.Path)
		}
		pending[e.Path] = append(pending[e.Path], e)
	}

	type umResultado struct {
		idx int
		n   int
	}
	planned := map[string]*editResult{}
	contagem := map[string][]umResultado{}
	for _, path := range order {
		p := resolvePath(cwd, path)
		info, err := os.Stat(p)
		if err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("%s is a directory, not a file", path)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
		content := string(b)
		var count []umResultado
		for j, e := range pending[path] {
			oldS, newS := e.OldString, e.NewString
			if strings.Contains(content, "\r\n") {
				oldS, newS = toCRLF(oldS), toCRLF(newS)
			}
			if oldS == newS {
				return "", fmt.Errorf("edit %d (%s): oldString and newString are identical; nothing to change", len(count)+j, e.Path)
			}
			n := strings.Count(content, oldS)
			switch {
			case n == 0:
				return "", fmt.Errorf("edit %d (%s): oldString not found; re-read the file and copy the text exactly (indentation, whitespace, no line-number prefixes)", len(count)+j, e.Path)
			case n > 1 && !e.ReplaceAll:
				return "", fmt.Errorf("edit %d (%s): oldString found %d times; add surrounding context to make it unique, or set replaceAll", len(count)+j, e.Path, n)
			}
			if e.ReplaceAll {
				content = strings.ReplaceAll(content, oldS, newS)
			} else {
				content = strings.Replace(content, oldS, newS, 1)
			}
			count = append(count, umResultado{idx: len(count) + j, n: n})
		}
		planned[path] = &editResult{path: path, body: content, mode: info.Mode().Perm()}
		contagem[path] = count
	}

	// ── phase 2: every edit matched; write ────────────────────────────────
	// Validation of EVERYTHING before writing ANYTHING is the point of the
	// tool: a half-applied refactor is worse than a failed one, because the
	// transcript no longer describes the code.
	var sb strings.Builder
	total := 0
	for _, path := range order {
		r := planned[path]
		if err := os.WriteFile(resolvePath(cwd, path), []byte(r.body), r.mode); err != nil {
			// A failure in the middle leaves the earlier writes in place (a
			// rename over a locked file, a disk error). The message says so
			// plainly: the log must not claim a state that is not real.
			fmt.Fprintf(&sb, "ok: %d file(s) written, then FAILED on %s: %v\n", total, path, err)
			return sb.String(), nil
		}
		n := 0
		for _, a := range contagem[path] {
			n += a.n
		}
		total++
		fmt.Fprintf(&sb, "ok: %d replacement(s) in %s\n", n, path)
	}
	fmt.Fprintf(&sb, "(%d file(s) changed)\n", total)
	return sb.String(), nil
}
