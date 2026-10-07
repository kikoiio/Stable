package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"stable/internal/appconfig"
	"stable/internal/platform/paths"
	"stable/internal/runtime"
)

func runProvider(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: stable provider list|show|use")
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return errors.New("usage: stable provider list")
		}
		fmt.Fprintln(stdout, "openai               streaming chat")
		fmt.Fprintln(stdout, "anthropic            streaming chat")
		fmt.Fprintln(stdout, "openai-compatible    streaming chat (requires --base-url)")
		fmt.Fprintln(stdout, "gemini               target decisions only; print is unsupported")
		return nil
	case "show":
		if len(args) != 1 {
			return errors.New("usage: stable provider show")
		}
		config, selection, err := appconfig.LoadWithModelSources()
		if err != nil {
			return err
		}
		if err = config.Validate(false); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "provider: %s (%s)\nmodel: %s (%s)\n", selection.Provider, selection.ProviderSource, selection.Model, selection.ModelSource)
		if selection.BaseURL != "" {
			fmt.Fprintf(stdout, "base_url: %s (%s)\n", selection.BaseURL, selection.BaseURLSource)
		}
		return nil
	case "use":
		return providerUse(args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown provider command %q; use list|show|use", args[0])
	}
}

func providerUse(args []string, stdout, stderr io.Writer) error {
	if len(args) < 1 {
		return errors.New("usage: stable provider use <provider> --model <model> [--base-url URL]")
	}
	provider := args[0]
	var model, baseURL string
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--model":
			i++
			if i >= len(args) {
				return errors.New("--model requires a value")
			}
			model = args[i]
		case "--base-url":
			i++
			if i >= len(args) {
				return errors.New("--base-url requires a value")
			}
			baseURL = args[i]
		default:
			return fmt.Errorf("unknown provider use argument %q", args[i])
		}
	}
	if model == "" {
		return errors.New("provider use requires --model")
	}
	c, err := appconfig.Load()
	if err != nil {
		return err
	}
	p, err := paths.Resolve(c.StateDir)
	if err != nil {
		return err
	}
	status, err := providerRuntimeControl(p, "status")
	if err == nil && status.Running {
		return errors.New("runtime is running; run `stable down` before changing provider")
	}
	if err != nil && !strings.Contains(err.Error(), "runtime is not running") {
		return err
	}
	if err = appconfig.UpdateModelSelection(provider, model, baseURL); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "saved provider=%s model=%s\n", provider, model)
	var overrides []string
	for _, name := range []string{"STABLE_PROVIDER", "STABLE_MODEL", "STABLE_BASE_URL"} {
		if strings.TrimSpace(getenv(name)) != "" {
			overrides = append(overrides, name)
		}
	}
	if len(overrides) != 0 {
		fmt.Fprintf(stderr, "environment overrides remain active: %s\n", strings.Join(overrides, ", "))
	}
	return nil
}

var getenv = func(key string) string { return os.Getenv(key) }

// Kept indirect so providerUse can be exercised without starting supervisor
// processes in unit tests. Production always uses the local runtime control socket.
var providerRuntimeControl = runtime.Control
