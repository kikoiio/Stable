package conversation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"stable/internal/agent"
)

type delegationEventPublisher struct{ event agent.DelegationEvent }

func (p *delegationEventPublisher) PublishDelegation(_ string, event agent.DelegationEvent) error {
	p.event = event
	return nil
}

func (p *delegationEventPublisher) Start(context.Context, agent.ExecutionRequest) (*agent.RunHandle, error) {
	return nil, errors.New("unused")
}

func (p *delegationEventPublisher) Cancel(string) error { return nil }

func TestDelegationEventReporterRedactsAndBoundsText(t *testing.T) {
	publisher := &delegationEventPublisher{}
	service := &Service{deps: Deps{Runner: publisher, ProviderCredential: "secret-token"}}
	reporter := NewDelegationEventReporter()
	reporter.Bind(service)
	if err := reporter.Publish("parent", agent.DelegationEvent{
		BatchID: "batch", TaskID: "task", TaskName: "inspect secret-token",
		Status: agent.DelegationRunning, Stage: "search", Summary: strings.Repeat("x", (8<<10)+1), Error: "secret-token",
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(publisher.event.TaskName, "secret-token") || strings.Contains(publisher.event.Error, "secret-token") {
		t.Fatalf("credential was not redacted: %+v", publisher.event)
	}
	if len(publisher.event.Summary) > 8<<10 || len(publisher.event.TaskName) > 256 || len(publisher.event.Error) > 1024 {
		t.Fatalf("event text was not bounded: task=%d summary=%d error=%d", len(publisher.event.TaskName), len(publisher.event.Summary), len(publisher.event.Error))
	}
}
