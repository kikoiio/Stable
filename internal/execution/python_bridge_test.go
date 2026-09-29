package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"proactive-agent/internal/core"
)

func TestBridgeProtocolAndUnknownOutcome(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "bridge.py")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(script, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	base := "import json,sys\nr=json.load(sys.stdin)\n"
	bridge := PythonBridge{Script: script, AllowedRoot: root, Timeout: time.Second}
	req := core.CapabilityRequest{ProtocolVersion: 1, OperationID: "op-1", Kind: "inspect_design", GoalID: "g"}
	write(base + "print(json.dumps({'protocol_version':1,'operation_id':r['operation_id'],'status':'observed','actual_artifact_id':'sha','evidence_paths':[],'postcondition':{}}))\n")
	got, err := bridge.Call(context.Background(), req)
	if err != nil || got.Status != "observed" {
		t.Fatalf("valid call %+v %v", got, err)
	}
	write(base + "print(json.dumps({'protocol_version':2,'operation_id':r['operation_id'],'status':'observed','evidence_paths':[]}))\n")
	if _, err = bridge.Call(context.Background(), req); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("version: %v", err)
	}
	write(base + "print(json.dumps({'protocol_version':1,'operation_id':'other','status':'observed','evidence_paths':[]}))\n")
	if _, err = bridge.Call(context.Background(), req); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("operation ID: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "report.json")
	if err = os.WriteFile(outside, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	write(base + "print(json.dumps({'protocol_version':1,'operation_id':r['operation_id'],'status':'observed','evidence_paths':['" + outside + "']}))\n")
	if _, err = bridge.Call(context.Background(), req); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("evidence path: %v", err)
	}
	write("import time\ntime.sleep(2)\n")
	bridge.Timeout = 10 * time.Millisecond
	if _, err = bridge.Call(context.Background(), req); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("timeout: %v", err)
	}
}
