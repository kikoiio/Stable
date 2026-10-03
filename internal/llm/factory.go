package llm

import (
	"errors"
	"net/http"
	"strings"

	"stable/internal/appconfig"
)

func NewProvider(model appconfig.ModelConfig) (Provider, error) {
	return NewProviderWithConfig(Config{Model: model})
}

func NewProviderWithConfig(config Config) (Provider, error) {
	model := config.Model
	if strings.TrimSpace(model.Model) == "" {
		return nil, errors.New("model ID required")
	}
	if strings.TrimSpace(model.APIKey) == "" {
		return nil, errors.New("API key required")
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{}
	}
	if config.MaxTokens <= 0 {
		config.MaxTokens = 4096
	}
	switch model.Provider {
	case "anthropic":
		if model.BaseURL != "" {
			return nil, errors.New("base URL is only supported for openai-compatible provider")
		}
		return newAnthropic(config), nil
	case "openai":
		if model.BaseURL != "" {
			return nil, errors.New("base URL is only supported for openai-compatible provider")
		}
		return newOpenAI(config), nil
	case "openai-compatible":
		if err := appconfig.ValidateBaseURL(model.BaseURL); err != nil {
			return nil, err
		}
		return newCompatible(config), nil
	default:
		return nil, errors.New("streaming provider must be anthropic, openai, or openai-compatible")
	}
}
