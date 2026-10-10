package conversation

import (
	"errors"
	"sync"
	"unicode/utf8"

	"stable/internal/agent"
)

type DelegationEventReporter struct {
	mu      sync.RWMutex
	service *Service
}

func NewDelegationEventReporter() *DelegationEventReporter { return &DelegationEventReporter{} }

func (r *DelegationEventReporter) Bind(service *Service) {
	r.mu.Lock()
	r.service = service
	r.mu.Unlock()
}

func (r *DelegationEventReporter) Publish(parentRunID string, event agent.DelegationEvent) error {
	r.mu.RLock()
	service := r.service
	r.mu.RUnlock()
	if service == nil {
		return errors.New("delegation event service is unavailable")
	}
	if service.deps.AgentTasks != nil {
		event = service.deps.AgentTasks.sanitizeEvent(parentRunID, event)
	}
	credential := service.deps.ProviderCredential
	event.TaskName = truncateDelegationText(redactRunCredential(event.TaskName, credential), 256)
	event.Stage = truncateDelegationText(redactRunCredential(event.Stage, credential), 256)
	event.Summary = truncateDelegationText(redactRunCredential(event.Summary, credential), 8<<10)
	event.Error = truncateDelegationText(redactRunCredential(event.Error, credential), 1024)
	return service.publishDelegationEvent(parentRunID, event)
}

func truncateDelegationText(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
