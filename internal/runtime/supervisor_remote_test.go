package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"stable/internal/remote"
)

type fakeRemoteController struct {
	status      remote.RemoteStatus
	startConfig remote.Config
	startErr    error
	stopErr     error
	pairErr     error
	pairedToken string
	stopCalls   int
	startCalls  int
	pairCalls   int
	lastStopCtx context.Context
}

func (f *fakeRemoteController) Start(_ context.Context, cfg remote.Config) (remote.RemoteStatus, error) {
	f.startCalls++
	f.startConfig = cfg
	if f.startErr != nil {
		return remote.RemoteStatus{}, f.startErr
	}
	f.status = remote.RemoteStatus{Running: true, ListenAddr: cfg.EffectiveListenAddress(), TLS: cfg.CertFile != ""}
	return f.status, nil
}

func (f *fakeRemoteController) Stop(ctx context.Context) error {
	f.stopCalls++
	f.lastStopCtx = ctx
	if f.stopErr != nil {
		return f.stopErr
	}
	f.status = remote.RemoteStatus{}
	return nil
}

func (f *fakeRemoteController) Status() remote.RemoteStatus { return f.status }

func (f *fakeRemoteController) IssuePairingToken() (string, time.Time, error) {
	f.pairCalls++
	if f.pairErr != nil {
		return "", time.Time{}, f.pairErr
	}
	return f.pairedToken, time.Now().Add(time.Minute), nil
}

func TestDispatchSupervisorControlRemoteLifecycle(t *testing.T) {
	manager := &fakeRemoteController{pairedToken: "one-use-code"}
	status := Status{Running: true, PID: 42}

	response, exit := dispatchSupervisorControl(`{"action":"start","listen_addr":"127.0.0.1:9000","tls_cert_file":"cert.pem","tls_key_file":"key.pem"}`, status, manager)
	started, ok := response.(RemoteControlResult)
	if !ok || exit || !started.Running || started.ListenAddr != "127.0.0.1:9000" {
		t.Fatalf("start response = %#v, exit=%v", response, exit)
	}
	if manager.startCalls != 1 || manager.startConfig.ListenAddress != "127.0.0.1:9000" || manager.startConfig.CertFile != "cert.pem" || manager.startConfig.KeyFile != "key.pem" {
		t.Fatalf("start config/calls = %#v / %d", manager.startConfig, manager.startCalls)
	}

	response, exit = dispatchSupervisorControl(`{"action":"status"}`, status, manager)
	remoteStatus, ok := response.(RemoteControlResult)
	if !ok || exit || !remoteStatus.Running || remoteStatus.ListenAddr != "127.0.0.1:9000" {
		t.Fatalf("status response = %#v, exit=%v", response, exit)
	}

	response, exit = dispatchSupervisorControl(`{"action":"pair"}`, status, manager)
	paired, ok := response.(RemoteControlResult)
	if !ok || exit || paired.PairingToken != "one-use-code" || manager.pairCalls != 1 {
		t.Fatalf("pair response = %#v, exit=%v, calls=%d", response, exit, manager.pairCalls)
	}

	response, exit = dispatchSupervisorControl(`{"action":"stop"}`, status, manager)
	stopped, ok := response.(RemoteControlResult)
	if !ok || exit || stopped.Running || manager.stopCalls != 1 {
		t.Fatalf("stop response = %#v, exit=%v, stop calls=%d", response, exit, manager.stopCalls)
	}
}

func TestDispatchSupervisorDownStopsRemoteBeforeReply(t *testing.T) {
	manager := &fakeRemoteController{status: remote.RemoteStatus{Running: true, ListenAddr: "127.0.0.1:8765"}}
	response, exit := dispatchSupervisorControl("down", Status{Running: true}, manager)
	status, ok := response.(Status)
	if !ok || !exit || !status.Running {
		t.Fatalf("down response = %#v, exit=%v", response, exit)
	}
	if manager.stopCalls != 1 || manager.Status().Running {
		t.Fatalf("remote was not stopped before returning: calls=%d status=%#v", manager.stopCalls, manager.Status())
	}
}

func TestDispatchSupervisorControlReturnsErrorsAndKeepsStatusProtocol(t *testing.T) {
	manager := &fakeRemoteController{startErr: errors.New("bind failed")}
	response, exit := dispatchSupervisorControl(`{"action":"start"}`, Status{Running: true}, manager)
	failed, ok := response.(RemoteControlResult)
	if !ok || exit || failed.ErrorCode != "operation_failed" || failed.Error != "bind failed" {
		t.Fatalf("failed start response = %#v, exit=%v", response, exit)
	}

	response, exit = dispatchSupervisorControl(`{"action":"start","tls_cert_file":"only-cert.pem"}`, Status{Running: true}, manager)
	failed, ok = response.(RemoteControlResult)
	if !ok || exit || failed.ErrorCode != "invalid_request" {
		t.Fatalf("invalid request response = %#v, exit=%v", response, exit)
	}

	want := Status{Running: true, PID: 42}
	response, exit = dispatchSupervisorControl("status", want, manager)
	got, ok := response.(Status)
	if !ok || exit || got != want {
		t.Fatalf("legacy status response = %#v, exit=%v", response, exit)
	}
}
