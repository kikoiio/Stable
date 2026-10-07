package sessionlog

import (
	"errors"
	"os"
)

// DeleteEphemeral removes only a session whose creation event marks it as
// temporary. The same path lock used by Append prevents deleting a transcript
// while another in-process append is active.
func DeleteEphemeral(root, id string) error {
	path, err := SessionPath(root, id)
	if err != nil {
		return err
	}
	mu := lockFor(path)
	mu.Lock()
	defer mu.Unlock()

	replay, err := replayFile(path, id)
	if err != nil {
		return err
	}
	if !replay.Session.Ephemeral {
		return errors.New("refusing to delete a persistent session")
	}
	if err = os.Remove(path); err != nil {
		return err
	}
	return nil
}
