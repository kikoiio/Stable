package execution

// HelperRequest is the single-line JSON request sent to the in-sandbox tool helper.
// Workspace is selected by the controlled executor and identifies the helper's
// already-mounted tool root; it is not a model-controlled mount boundary.
type HelperRequest struct {
	Tool      string         `json:"tool"`
	Args      map[string]any `json:"args"`
	Workspace string         `json:"workspace"`
}

// HelperResponse is the single-line JSON result returned by the in-sandbox tool helper.
type HelperResponse struct {
	Output    string `json:"output"`
	IsError   bool   `json:"is_error"`
	Additions int    `json:"additions,omitempty"`
	Removals  int    `json:"removals,omitempty"`
	DiffText  string `json:"diff_text,omitempty"`
}
