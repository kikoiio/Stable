package tui

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"stable/internal/commands"
)

type CompletionKind uint8

const (
	CommandCompletion CompletionKind = iota
	PathCompletion
)

type CompletionItem struct {
	Kind                      CompletionKind
	Label, Detail, InsertText string
}

type Overlay struct{ Width, Height int }

func (o Overlay) Render(items []CompletionItem, selected int) string {
	if o.Width < 1 || o.Height < 1 || len(items) == 0 {
		return ""
	}
	var lines []string
	limit := min(len(items), o.Height)
	start := 0
	if selected >= limit {
		start = selected - limit + 1
	}
	for i := start; i < min(len(items), start+limit); i++ {
		marker := "  "
		if i == selected {
			marker = "› "
		}
		lines = append(lines, marker+truncate(items[i].Label, o.Width-2))
	}
	return strings.Join(lines, "\n")
}

type PathCompleter interface {
	Complete(projectRoot, prefix string) ([]CompletionItem, error)
}

// CommandItems renders registry commands as completion entries. The insert
// text carries a trailing space exactly when the command documents an
// argument prompt, which reproduces the historical built-in list: bare view
// switches insert without a space, argument-taking commands with one.
func CommandItems(cmds []*commands.Command) []CompletionItem {
	items := make([]CompletionItem, 0, len(cmds))
	for _, c := range cmds {
		insert := "/" + c.Name
		if c.ArgPrompt != "" {
			insert += " "
		}
		items = append(items, CompletionItem{Kind: CommandCompletion, Label: "/" + c.Name, Detail: c.Description, InsertText: insert})
	}
	return items
}
func FilterCompletions(items []CompletionItem, prefix string) []CompletionItem {
	out := items[:0]
	for _, i := range items {
		if strings.HasPrefix(strings.ToLower(i.Label), strings.ToLower(prefix)) {
			out = append(out, i)
		}
	}
	return out
}

type projectPathCompleter struct{}

func (projectPathCompleter) Complete(root, prefix string) ([]CompletionItem, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
		root = resolved
	}
	needle := strings.TrimPrefix(prefix, "@")
	dirPart, base := filepath.Split(needle)
	dir := filepath.Join(root, filepath.FromSlash(dirPart))
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, nil
	}
	if !withinRoot(root, resolved) {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	var out []CompletionItem
	for _, e := range entries {
		if !strings.HasPrefix(strings.ToLower(e.Name()), strings.ToLower(base)) {
			continue
		}
		full := filepath.Join(dir, e.Name())
		real, er := filepath.EvalSymlinks(full)
		if er != nil || !withinRoot(root, real) {
			continue
		}
		rel, er := filepath.Rel(root, full)
		if er != nil {
			continue
		}
		text := "@" + filepath.ToSlash(rel)
		if e.IsDir() {
			text += "/"
		}
		out = append(out, CompletionItem{Kind: PathCompletion, Label: text, InsertText: text})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out, nil
}
func withinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
