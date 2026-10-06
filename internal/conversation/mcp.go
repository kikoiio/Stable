package conversation

import (
	"fmt"
	"sort"
	"strings"

	"stable/internal/agent"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/sessionlog"
)

func (s *Service) ensureMCPFresh(sessionID string) error {
	if s.mcp == nil {
		return nil
	}
	beforeRevision := s.mcp.Revision()
	beforeCount := len(s.mcp.Status())
	if err := s.mcp.EnsureFresh(); err != nil {
		return err
	}
	s.refreshMCPTooling()
	if s.mcp.Revision() == beforeRevision || sessionID == "" || s.deps.ProjectRoot == "" {
		return nil
	}
	return s.appendMCPReload(sessionID, beforeCount, "auto")
}

func (s *Service) listMCP(c ClientMsg) ([]ServerMsg, error) {
	if s.mcp == nil {
		return nil, fmt.Errorf("MCP 通道不可用")
	}
	if err := s.ensureMCPFresh(c.SessionID); err != nil {
		return nil, err
	}
	return []ServerMsg{{Type: "mcp_list", MCPList: &MCPListMsg{
		Servers: s.mcp.Status(), Rejections: s.mcp.Rejections(),
	}}}, nil
}

func (s *Service) reloadMCP(c ClientMsg) ([]ServerMsg, error) {
	if s.mcp == nil {
		return nil, fmt.Errorf("MCP 通道不可用")
	}
	before, after, rejections, err := s.mcp.Reload()
	s.refreshMCPTooling()
	report := MCPReportMsg{Before: before, After: after, Rejections: append([]string(nil), rejections...)}
	if err != nil {
		report.Error = err.Error()
	}
	if c.SessionID != "" && s.deps.ProjectRoot != "" {
		appendErr := s.appendMCPReload(c.SessionID, before, "manual")
		if appendErr != nil && err == nil {
			err = appendErr
			report.Error = err.Error()
		}
	}
	if err != nil {
		return []ServerMsg{{Type: "mcp_report", MCPReport: &report}}, nil
	}
	return []ServerMsg{{Type: "mcp_report", MCPReport: &report}}, nil
}

// refreshMCPTooling keeps the provider-facing tool list aligned with a
// manager reload. StreamingRunner exposes this as an optional capability so
// tests and alternate Runner implementations do not need to support dynamic
// schema updates.
func (s *Service) refreshMCPTooling() {
	if s.mcp == nil || s.deps.ExecutorFactory == nil || s.deps.Runner == nil {
		return
	}
	setter, ok := s.deps.Runner.(interface {
		SetTooling(agent.ExecutorFactory, []llm.ToolSchema)
	})
	if !ok {
		return
	}
	base := make([]llm.ToolSchema, 0, len(s.deps.ToolSchemas))
	for _, schema := range s.deps.ToolSchemas {
		if schema.Name == "mcp_call" || schema.Name == "tool_search" || strings.HasPrefix(schema.Name, "mcp__") {
			continue
		}
		base = append(base, schema)
	}
	if s.mcp.Configured() {
		for _, schema := range execution.MCPToolSchemas() {
			name, _ := schema["name"].(string)
			description, _ := schema["description"].(string)
			input, _ := schema["input_schema"].(map[string]any)
			base = append(base, llm.ToolSchema{Name: name, Description: description, InputSchema: input})
		}
		for _, schema := range s.mcp.EagerSchemas() {
			base = append(base, llm.ToolSchema{Name: schema.Name, Description: schema.Description, InputSchema: schema.InputSchema})
		}
	}
	sort.SliceStable(base, func(i, j int) bool { return base[i].Name < base[j].Name })
	setter.SetTooling(s.deps.ExecutorFactory, base)
}

func (s *Service) appendMCPReload(sessionID string, beforeCount int, trigger string) error {
	if s.mcp == nil || s.deps.ProjectRoot == "" {
		return nil
	}
	statuses := s.mcp.Status()
	rejections := s.mcp.Rejections()
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	_, err := sessionlog.Append(s.deps.ProjectRoot, sessionID, sessionlog.EventMCPReload, sessionlog.MCPReload{
		Before: beforeCount, After: len(statuses), Rejections: rejections, Trigger: trigger,
	})
	if err != nil {
		return err
	}
	for _, status := range statuses {
		if _, err = sessionlog.Append(s.deps.ProjectRoot, sessionID, sessionlog.EventMCPServer, sessionlog.MCPServer{
			Name: status.Name, Source: status.Source, State: status.State, Error: status.Error, ToolCount: status.ToolCount,
		}); err != nil {
			return err
		}
	}
	return nil
}
