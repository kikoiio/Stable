package appconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"stable/internal/platform/paths"
	"stable/internal/platform/secfile"
	"strconv"
	"strings"
)

type ModelConfig struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	BaseURL  string `json:"base_url,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
	// ContextWindowTokens overrides the model context window. Zero uses the
	// conservative default resolved by sessioncontext.EffectiveWindow.
	ContextWindowTokens int `json:"context_window_tokens,omitempty"`
}

// SnapshotQuota bounds candidate snapshot storage. Zero fields fall back to
// the approved defaults.
type SnapshotQuota struct {
	MaxProjectBytes          int64 `json:"max_project_bytes,omitempty"`
	MaxManifestsPerCandidate int   `json:"max_manifests_per_candidate,omitempty"`
}

const (
	// DefaultSnapshotProjectBytes is the default per-project blob budget (1 GiB).
	DefaultSnapshotProjectBytes int64 = 1 << 30
	// DefaultSnapshotManifests is the default per-candidate manifest budget.
	DefaultSnapshotManifests = 50
)

// ProjectBytes resolves the per-project snapshot budget.
func (q SnapshotQuota) ProjectBytes() int64 {
	if q.MaxProjectBytes <= 0 {
		return DefaultSnapshotProjectBytes
	}
	return q.MaxProjectBytes
}

// ManifestsPerCandidate resolves the per-candidate manifest budget.
func (q SnapshotQuota) ManifestsPerCandidate() int {
	if q.MaxManifestsPerCandidate <= 0 {
		return DefaultSnapshotManifests
	}
	return q.MaxManifestsPerCandidate
}

type AppConfig struct {
	Model        ModelConfig   `json:"model"`
	StateDir     string        `json:"state_dir,omitempty"`
	TemporalPort int           `json:"temporal_port,omitempty"`
	Snapshots    SnapshotQuota `json:"snapshots,omitempty"`
}

func ConfigPath() (string, error) {
	if p := os.Getenv("STABLE_CONFIG"); p != "" {
		return filepath.Abs(p)
	}
	base, err := paths.UserConfigHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "stable", "config.json"), nil
}

// UserSkillsDir returns the user-level skill directory: the same directory
// base as ConfigPath (XDG_CONFIG_HOME, or $HOME/.config when unset) plus
// stable/skills. STABLE_CONFIG does not affect it: that variable relocates
// only the config file itself, not the configuration directory tree.
func UserSkillsDir() (string, error) {
	base, err := paths.UserConfigHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "stable", "skills"), nil
}

// UserHooksPath returns the user-level hooks file in the same configuration
// directory tree as UserSkillsDir.
func UserHooksPath() (string, error) {
	base, err := UserSkillsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(base), "hooks.yaml"), nil
}

func StateDir() (string, error) {
	if p := os.Getenv("STABLE_STATE_DIR"); p != "" {
		return filepath.Abs(p)
	}
	base, err := paths.UserStateHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "stable"), nil
}

// UserCommandsDir returns the user-level custom command directory.
func UserCommandsDir() (string, error) {
	base, err := paths.UserConfigHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "stable", "commands"), nil
}

func Load() (AppConfig, error) {
	var c AppConfig
	p, err := ConfigPath()
	if err != nil {
		return c, err
	}
	if st, err := os.Stat(p); err == nil {
		private, privateErr := secfile.IsPrivatePath(p)
		if privateErr != nil {
			return c, privateErr
		}
		if !st.Mode().IsRegular() || !private {
			return c, fmt.Errorf("config file must be private (%s)", secfile.FixHint("file"))
		}
		dirPrivate, privateErr := secfile.IsPrivatePath(filepath.Dir(p))
		if privateErr != nil {
			return c, privateErr
		}
		if !dirPrivate {
			return c, fmt.Errorf("config directory must be private and owned by current user (%s)", secfile.FixHint("dir"))
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return c, err
		}
		if err := json.Unmarshal(data, &c); err != nil {
			return c, errors.New("invalid config JSON")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return c, err
	}
	if v := os.Getenv("STABLE_PROVIDER"); v != "" {
		if v != c.Model.Provider {
			c.Model.APIKey = ""
		}
		c.Model.Provider = v
	}
	if v := os.Getenv("STABLE_MODEL"); v != "" {
		c.Model.Model = v
	}
	if v := os.Getenv("STABLE_BASE_URL"); v != "" {
		c.Model.BaseURL = v
	}
	if v := os.Getenv("STABLE_TEMPORAL_PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return c, errors.New("invalid Temporal port")
		}
		c.TemporalPort = n
	}
	if v := os.Getenv("STABLE_CONTEXT_WINDOW_TOKENS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return c, errors.New("invalid context window tokens")
		}
		c.Model.ContextWindowTokens = n
	}
	if c.TemporalPort == 0 {
		c.TemporalPort = 7233
	}
	if c.StateDir == "" {
		c.StateDir, err = StateDir()
		if err != nil {
			return c, err
		}
	}
	if v := os.Getenv("STABLE_STATE_DIR"); v != "" {
		c.StateDir, err = filepath.Abs(v)
		if err != nil {
			return c, err
		}
	}
	keyEnv := map[string]string{"openai": "OPENAI_API_KEY", "anthropic": "ANTHROPIC_API_KEY", "gemini": "GEMINI_API_KEY", "openai-compatible": "STABLE_API_KEY"}
	if v := os.Getenv(keyEnv[c.Model.Provider]); v != "" {
		c.Model.APIKey = v
	}
	return c, nil
}

func (c AppConfig) Validate(requireKey bool) error {
	switch c.Model.Provider {
	case "openai", "anthropic", "gemini", "openai-compatible":
	default:
		return errors.New("provider must be openai, anthropic, gemini, or openai-compatible")
	}
	if strings.TrimSpace(c.Model.Model) == "" {
		return errors.New("model ID required")
	}
	if c.TemporalPort < 1 || c.TemporalPort > 65535 {
		return errors.New("Temporal port out of range")
	}
	if c.Model.Provider == "openai-compatible" {
		if err := ValidateBaseURL(c.Model.BaseURL); err != nil {
			return err
		}
	} else if c.Model.BaseURL != "" {
		return errors.New("base URL is only supported for openai-compatible provider")
	}
	if requireKey && c.Model.APIKey == "" {
		return fmt.Errorf("API key missing for %s", c.Model.Provider)
	}
	return nil
}

func ValidateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("invalid compatible base URL")
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme != "http" {
		return errors.New("compatible endpoint must use HTTPS or loopback HTTP")
	}
	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return nil
	}
	return errors.New("compatible endpoint must use HTTPS or loopback HTTP")
}

func (c AppConfig) Summary() string {
	host := "official"
	if c.Model.Provider == "openai-compatible" {
		if u, err := url.Parse(c.Model.BaseURL); err == nil {
			host = u.Host
		}
	}
	return fmt.Sprintf("provider=%s model=%s host=%s key_configured=%t", c.Model.Provider, c.Model.Model, host, c.Model.APIKey != "")
}

// Init creates the private config directory and a template config file if none exists.
func Init() (path string, created bool, err error) {
	path, err = ConfigPath()
	if err != nil {
		return "", false, err
	}
	dir := filepath.Dir(path)
	if err = secfile.MkdirAllPrivate(dir, 0700); err != nil {
		return "", false, err
	}
	template := []byte("{\n  \"model\": {\n    \"provider\": \"openai\",\n    \"model\": \"YOUR_MODEL_ID\",\n    \"api_key\": \"YOUR_PRIVATE_KEY\"\n  }\n}\n")
	f, err := secfile.OpenFilePrivate(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return path, false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	if _, err = f.Write(template); err != nil {
		return "", false, err
	}
	return path, true, nil
}
