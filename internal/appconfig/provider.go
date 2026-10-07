package appconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"stable/internal/platform/secfile"
)

type ConfigSource string

const (
	SourceFile    ConfigSource = "file"
	SourceEnv     ConfigSource = "env"
	SourceDefault ConfigSource = "default"
)

type ModelSelection struct {
	Provider       string       `json:"provider"`
	Model          string       `json:"model"`
	BaseURL        string       `json:"base_url,omitempty"`
	ProviderSource ConfigSource `json:"provider_source"`
	ModelSource    ConfigSource `json:"model_source"`
	BaseURLSource  ConfigSource `json:"base_url_source"`
}

// LoadWithModelSources returns resolved model settings and their sources,
// without returning or logging the configured API key.
func LoadWithModelSources() (AppConfig, ModelSelection, error) {
	c, err := Load()
	if err != nil {
		return c, ModelSelection{}, err
	}
	selection := ModelSelection{Provider: c.Model.Provider, Model: c.Model.Model, BaseURL: c.Model.BaseURL}
	selection.ProviderSource = sourceFor("STABLE_PROVIDER", c.Model.Provider)
	selection.ModelSource = sourceFor("STABLE_MODEL", c.Model.Model)
	selection.BaseURLSource = sourceFor("STABLE_BASE_URL", c.Model.BaseURL)
	return c, selection, nil
}

func sourceFor(env, value string) ConfigSource {
	if os.Getenv(env) != "" {
		return SourceEnv
	}
	if value != "" {
		return SourceFile
	}
	return SourceDefault
}

// UpdateModelSelection atomically updates the private config file while
// preserving fields this version of Stable does not know about.
func UpdateModelSelection(provider, model, baseURL string) error {
	provider, model, baseURL = strings.TrimSpace(provider), strings.TrimSpace(model), strings.TrimSpace(baseURL)
	if provider != "openai" && provider != "anthropic" && provider != "openai-compatible" && provider != "gemini" {
		return errors.New("provider must be openai, anthropic, gemini, or openai-compatible")
	}
	if model == "" {
		return errors.New("model ID required")
	}
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err = secfile.MkdirAllPrivate(dir, 0700); err != nil {
		return err
	}
	var config map[string]json.RawMessage
	data, err := os.ReadFile(path)
	if err == nil {
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return statErr
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("config path must be a regular file")
		}
		private, pErr := secfile.IsPrivatePath(path)
		if pErr != nil {
			return pErr
		}
		if !private {
			return fmt.Errorf("config file must be private (%s)", secfile.FixHint("file"))
		}
		if err = json.Unmarshal(data, &config); err != nil {
			return errors.New("invalid config JSON")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if config == nil {
		config = map[string]json.RawMessage{}
	}
	var modelFields map[string]json.RawMessage
	if raw := config["model"]; len(raw) != 0 {
		if err = json.Unmarshal(raw, &modelFields); err != nil {
			return errors.New("invalid model config")
		}
	}
	if modelFields == nil {
		modelFields = map[string]json.RawMessage{}
	}
	var oldProvider string
	_ = json.Unmarshal(modelFields["provider"], &oldProvider)
	if provider == "openai-compatible" && baseURL == "" && oldProvider == provider {
		_ = json.Unmarshal(modelFields["base_url"], &baseURL)
	}
	if provider == "openai-compatible" {
		if err := ValidateBaseURL(baseURL); err != nil {
			return err
		}
	} else if baseURL != "" {
		return errors.New("base URL is only supported for openai-compatible provider")
	}
	if oldProvider != provider {
		delete(modelFields, "api_key")
		delete(modelFields, "base_url")
	}
	modelJSON, _ := json.Marshal(model)
	providerJSON, _ := json.Marshal(provider)
	modelFields["provider"], modelFields["model"] = providerJSON, modelJSON
	if provider == "openai-compatible" {
		modelFields["base_url"], _ = json.Marshal(baseURL)
	} else {
		delete(modelFields, "base_url")
	}
	config["model"], err = json.Marshal(modelFields)
	if err != nil {
		return err
	}
	updated, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-update-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if err = secfile.ChmodPrivate(tmpPath, 0600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpPath) }
	if _, err = tmp.Write(updated); err != nil {
		cleanup()
		return err
	}
	if err = tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err = tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if existing, statErr := os.Lstat(path); statErr == nil && (!existing.Mode().IsRegular() || existing.Mode()&os.ModeSymlink != 0) {
		_ = os.Remove(tmpPath)
		return errors.New("config path must be a regular file")
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		_ = os.Remove(tmpPath)
		return statErr
	}
	if err = os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}
