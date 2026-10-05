package hooks

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	MaxFileBytes    = 256 * 1024
	MaxHooksPerFile = 100
)

type LoadResult struct {
	Hooks      []Hook
	Rejections []string
}

type fileDoc struct {
	Hooks []Hook `yaml:"hooks"`
}

func LoadFiles(userPath, projectPath string) LoadResult {
	user, userRej := loadOne(userPath, "user")
	project, projectRej := loadOne(projectPath, "project")
	merged, mergeRej := mergeHooks(user, project)
	return LoadResult{Hooks: merged, Rejections: append(append(userRej, projectRej...), mergeRej...)}
}

func loadOne(path, source string) ([]Hook, []string) {
	if path == "" {
		return nil, nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []string{fmt.Sprintf("%s: %v", path, err)}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, []string{fmt.Sprintf("%s: symbolic link refused", path)}
	}
	if info.Size() > MaxFileBytes {
		return nil, []string{fmt.Sprintf("%s: file exceeds %d bytes", path, MaxFileBytes)}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, []string{fmt.Sprintf("%s: %v", path, err)}
	}
	var doc fileDoc
	if err = yaml.Unmarshal(raw, &doc); err != nil {
		return nil, []string{fmt.Sprintf("%s: invalid YAML: %v", path, err)}
	}
	seen := map[string]bool{}
	var out []Hook
	var rejections []string
	for i, h := range doc.Hooks {
		n := i + 1
		if n > MaxHooksPerFile {
			rejections = append(rejections, fmt.Sprintf("%s: hook %d exceeds per-file limit %d", path, n, MaxHooksPerFile))
			continue
		}
		if strings.TrimSpace(h.ID) == "" {
			h.ID = fmt.Sprintf("%s:%d", source, n)
		} else if seen[h.ID] {
			rejections = append(rejections, fmt.Sprintf("%s: duplicate id %s skipped", path, h.ID))
			continue
		} else {
			seen[h.ID] = true
		}
		h.Source = source
		if err := Validate([]Hook{h}); err != nil {
			rejections = append(rejections, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		out = append(out, h)
	}
	return out, rejections
}

func mergeHooks(user, project []Hook) ([]Hook, []string) {
	projectIDs := map[string]bool{}
	for _, h := range project {
		projectIDs[h.ID] = true
	}
	var merged []Hook
	var rejections []string
	for _, h := range user {
		if projectIDs[h.ID] {
			rejections = append(rejections, fmt.Sprintf("id %s: project overrides user", h.ID))
			continue
		}
		merged = append(merged, h)
	}
	return append(merged, project...), rejections
}
