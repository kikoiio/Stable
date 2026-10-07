package execution

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"stable/internal/memory"
)

// MemoryProvider exposes only the operations required by host memory tools.
// Implementations bind session IDs to the service's trusted project and user
// roots; tool arguments can never choose a filesystem path.
type MemoryProvider interface {
	List(ctx context.Context, sessionID string) ([]memory.MemoryHeader, error)
	Read(ctx context.Context, sessionID string, scope memory.MemoryScope, filename string) (memory.MemoryEntry, error)
	Save(ctx context.Context, sessionID string, change memory.MemoryChange) error
	Delete(ctx context.Context, sessionID string, scope memory.MemoryScope, filename string) error
}

func (e *toolRunExecutor) executeMemoryTool(ctx context.Context, callName string, args map[string]any) (string, string, string, string, error) {
	sessionID := e.request.Work.SessionID
	if e.deps.Memory == nil {
		return "", "", "", "", errors.New("memory service unavailable")
	}
	scope := memory.MemoryScope(stringArg(args, "scope"))
	var entry string
	var operation string
	var result string
	var err error
	switch callName {
	case "memory_list":
		operation, scope = "list", "both"
		var headers []memory.MemoryHeader
		headers, err = e.deps.Memory.List(ctx, sessionID)
		if err == nil {
			if len(headers) == 0 {
				result = "No memory entries."
			} else {
				var lines []string
				for _, h := range headers {
					lines = append(lines, fmt.Sprintf("%s\t%s\t%s\t%s\t%s", h.Scope, h.Type, h.Filename, h.Name, h.Description))
				}
				result = strings.Join(lines, "\n")
			}
		}
	case "memory_read":
		operation, entry = "read", stringArg(args, "filename")
		var record memory.MemoryEntry
		record, err = e.deps.Memory.Read(ctx, sessionID, scope, entry)
		if err == nil {
			result = fmt.Sprintf("Name: %s\nType: %s\nDescription: %s\n\n%s", record.Name, record.Type, record.Description, record.Body)
		}
	case "memory_save":
		operation, entry = "save", stringArg(args, "name")
		kind := memory.MemoryType(stringArg(args, "type"))
		if owner, ok := memory.ScopeForType(kind); !ok || owner != scope {
			err = errors.New("memory type does not belong to the requested scope")
		} else {
			err = e.deps.Memory.Save(ctx, sessionID, memory.MemoryChange{
				Action: memory.ActionUpsert, Scope: scope, Type: kind, Name: entry,
				Description: stringArg(args, "description"), Body: stringArg(args, "body"),
			})
			if err == nil {
				result = "Memory saved."
			}
		}
	case "memory_delete":
		operation, entry = "delete", stringArg(args, "filename")
		err = e.deps.Memory.Delete(ctx, sessionID, scope, entry)
		if err == nil {
			result = "Memory deleted."
		}
	default:
		return "", "", "", "", errors.New("unknown memory tool")
	}
	return result, operation, string(scope), entry, err
}

func stringArg(args map[string]any, key string) string {
	value, _ := args[key].(string)
	return value
}

func memoryActionTime(now func() time.Time) time.Time {
	if now == nil {
		return time.Now().UTC()
	}
	return now().UTC()
}
