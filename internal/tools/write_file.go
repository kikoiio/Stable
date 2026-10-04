package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

type WriteFileTool struct {
	FileStateCache *FileStateCache
}

func (t *WriteFileTool) Name() string           { return "WriteFile" }
func (t *WriteFileTool) Description() string    { return WriteFileDescription }
func (t *WriteFileTool) Category() ToolCategory { return CategoryWrite }

func (t *WriteFileTool) Schema() map[string]any {
	return map[string]any{
		"name": t.Name(), "description": t.Description(),
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file_path": map[string]any{"type": "string", "description": "Path to the file to write"},
				"content":   map[string]any{"type": "string", "description": "Content to write to the file"},
			},
			"required": []string{"file_path", "content"},
		},
	}
}

func (t *WriteFileTool) Execute(_ context.Context, args map[string]any) ToolResult {
	filePath, _ := args["file_path"].(string)
	content, _ := args["content"].(string)
	if filePath == "" {
		return ToolResult{Output: "Error: file_path is required", IsError: true}
	}

	var oldContent string
	_, statErr := os.Stat(filePath)
	if statErr == nil {
		if t.FileStateCache != nil {
			if ok, errMsg := t.FileStateCache.Check(filePath); !ok {
				return ToolResult{Output: errMsg, IsError: true}
			}
		}
		data, err := os.ReadFile(filePath)
		if err != nil {
			return ToolResult{Output: fmt.Sprintf("Error reading file: %s", err), IsError: true}
		}
		oldContent = string(data)
	} else if !os.IsNotExist(statErr) {
		return ToolResult{Output: fmt.Sprintf("Error stating file: %s", statErr), IsError: true}
	}

	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return ToolResult{Output: fmt.Sprintf("Error creating directories: %s", err), IsError: true}
	}
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		return ToolResult{Output: fmt.Sprintf("Error writing file: %s", err), IsError: true}
	}
	if t.FileStateCache != nil {
		t.FileStateCache.Update(filePath)
	}
	if statErr == nil {
		diff := BuildDiff(oldContent, content)
		return ToolResult{Output: fmt.Sprintf("Successfully wrote to %s with %d addition%s and %d removal%s\n%s", filePath, diff.Additions, plural(diff.Additions), diff.Removals, plural(diff.Removals), diff.Text), Additions: diff.Additions, Removals: diff.Removals, DiffText: diff.Text}
	}
	return ToolResult{Output: fmt.Sprintf("Successfully wrote to %s", filePath)}
}
