package sessionlog

import (
	"fmt"
	"os"
	"time"
)

func Create(root, title string) (SessionInfo, error) {
	return create(root, title, false)
}

// CreateEphemeral creates a session whose transcript is intended to be
// discarded after a one-shot run.
func CreateEphemeral(root, title string) (SessionInfo, error) {
	return create(root, title, true)
}

func create(root, title string, ephemeral bool) (SessionInfo, error) {
	if _, err := Prepare(root); err != nil {
		return SessionInfo{}, err
	}
	id, err := NewID()
	if err != nil {
		return SessionInfo{}, err
	}
	now := time.Now().UTC()
	info := SessionInfo{ID: id, Title: title, CreatedAt: now, UpdatedAt: now, Ephemeral: ephemeral}
	if info.Title == "" {
		info.Title = "新会话"
	}
	_, err = Append(root, id, EventSessionCreated, info)
	return info, err
}

func List(root string) ([]SessionInfo, error) {
	dir, err := Prepare(root)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []SessionInfo
	for _, entry := range entries {
		if entry.IsDir() || len(entry.Name()) != 38 || entry.Name()[32:] != ".jsonl" {
			continue
		}
		id := entry.Name()[:32]
		if ValidateID(id) != nil {
			continue
		}
		replay, e := Replay(root, id)
		if e != nil {
			return out, fmt.Errorf("load session %s: %w", id, e)
		}
		info := replay.Session
		if info.Ephemeral {
			continue
		}
		if len(replay.Events) > 0 {
			info.UpdatedAt = replay.Events[len(replay.Events)-1].At
		}
		out = append(out, info)
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].UpdatedAt.After(out[i].UpdatedAt) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}
