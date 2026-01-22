package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"xhark/internal/ui"
)

func main() {
	var (
		baseURL  string
		specURL  string
		specFile string
	)

	flag.StringVar(&baseURL, "base-url", "", "Base URL for executing requests (e.g. http://localhost:8000)")
	flag.StringVar(&specURL, "spec-url", "", "OpenAPI spec URL (http/https)")
	flag.StringVar(&specFile, "spec-file", "", "Path to local OpenAPI spec file")
	flag.Parse()

	// CLI args take precedence over env.
	spec := strings.TrimSpace(specURL)
	if spec == "" {
		spec = strings.TrimSpace(specFile)
		if spec != "" {
			if abs, err := filepath.Abs(spec); err == nil {
				spec = abs
			}
			// Use explicit marker for local file.
			spec = "@" + spec
		}
	}
	if spec == "" {
		// Env fallback.
		if envSpecFile := strings.TrimSpace(os.Getenv("XHARK_SPEC_FILE")); envSpecFile != "" {
			if abs, err := filepath.Abs(envSpecFile); err == nil {
				envSpecFile = abs
			}
			spec = "@" + envSpecFile
		} else {
			spec = strings.TrimSpace(os.Getenv("XHARK_SPEC_URL"))
		}
	}

	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("XHARK_BASE_URL"))
	}

	app := ui.NewApp(os.Stdin, os.Stdout)
	if spec != "" {
		app.SetSpec(spec)
	}
	if baseURL != "" {
		app.SetBaseURL(baseURL)
	}
	if err := app.Init(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := app.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
