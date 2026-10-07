package sessionlog

import (
	"os"
	"strings"
	"sync"
	"testing"
)

func TestEphemeralListAndDelete(t *testing.T) {
	root := t.TempDir()
	ordinary, err := Create(root, "ordinary")
	if err != nil {
		t.Fatal(err)
	}
	ephemeral, err := CreateEphemeral(root, "temporary")
	if err != nil {
		t.Fatal(err)
	}
	list, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != ordinary.ID {
		t.Fatalf("List = %+v", list)
	}
	if err = DeleteEphemeral(root, ordinary.ID); err == nil {
		t.Fatal("persistent session was deleted")
	}
	if err = DeleteEphemeral(root, ephemeral.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(mustSessionPath(t, root, ephemeral.ID)); !os.IsNotExist(err) {
		t.Fatalf("ephemeral file still exists: %v", err)
	}
}

func TestDeleteEphemeralRejectsInvalidIDs(t *testing.T) {
	for _, id := range []string{"", "../outside", "bad/id"} {
		if err := DeleteEphemeral(t.TempDir(), id); err == nil {
			t.Errorf("DeleteEphemeral(%q) succeeded", id)
		}
	}
}

func TestConcurrentEphemeralAppendAndDeleteAreSerialized(t *testing.T) {
	root := t.TempDir()
	session, err := CreateEphemeral(root, "temporary")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	var appendErr, deleteErr error
	go func() {
		defer wg.Done()
		<-start
		_, appendErr = Append(root, session.ID, EventActivity, map[string]string{"kind": "test"})
	}()
	go func() {
		defer wg.Done()
		<-start
		deleteErr = DeleteEphemeral(root, session.ID)
	}()
	close(start)
	wg.Wait()
	if deleteErr != nil {
		t.Fatalf("delete failed: %v (append error: %v)", deleteErr, appendErr)
	}
	if appendErr != nil && !strings.Contains(appendErr.Error(), "first session event") && !os.IsNotExist(appendErr) {
		t.Fatalf("append returned unexpected error: %v", appendErr)
	}
	if _, err = os.Stat(mustSessionPath(t, root, session.ID)); !os.IsNotExist(err) {
		t.Fatalf("session file remains after delete: %v", err)
	}
}

func mustSessionPath(t *testing.T, root, id string) string {
	t.Helper()
	p, err := SessionPath(root, id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
