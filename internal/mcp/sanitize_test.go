package mcp

import "testing"

func TestSanitizeName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"keeps plain alphanumerics", "chromeDevtools2", "chromeDevtools2"},
		{"preserves case", "TakeScreenshot", "TakeScreenshot"},
		{"replaces dashes", "chrome-devtools", "chrome_devtools"},
		{"replaces dots, slashes and spaces", "a.b/c d", "a_b_c_d"},
		{"replaces every special character", "!@#$%", "_____"},
		{"handles empty string", "", ""},
	}
	for _, tt := range tests {
		if got := SanitizeName(tt.in); got != tt.want {
			t.Errorf("%s: SanitizeName(%q) = %q, want %q", tt.name, tt.in, got, tt.want)
		}
	}
}

func TestMCPToolNamePrefixSanitizesServer(t *testing.T) {
	const want = "mcp__chrome_devtools__"
	if got := MCPToolNamePrefix("chrome-devtools"); got != want {
		t.Fatalf("MCPToolNamePrefix: got %q, want %q", got, want)
	}
}

func TestToolNameSanitizesServerAndToolIndependently(t *testing.T) {
	tests := []struct {
		name   string
		server string
		tool   string
		want   string
	}{
		{"sanitizes both parts", "chrome-devtools", "take.screenshot", "mcp__chrome_devtools__take_screenshot"},
		{"clean names pass through", "linear", "create_issue", "mcp__linear__create_issue"},
		{"sanitizes empty server", "", "search", "mcp____search"},
	}
	for _, tt := range tests {
		if got := ToolName(tt.server, tt.tool); got != tt.want {
			t.Errorf("%s: ToolName(%q, %q) = %q, want %q", tt.name, tt.server, tt.tool, got, tt.want)
		}
	}
}
