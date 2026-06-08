package buildtrigger

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// ServeConfig is the top-level operator configuration parsed from YAML.
// It is loaded once at server startup and injected into the gateway via
// gateway.Options.
type ServeConfig struct {
	Build BuildSection `yaml:"build"`
}

// BuildSection groups all build-trigger operator configuration.
type BuildSection struct {
	Defaults      Defaults               `yaml:"defaults"`
	AWSConnectors map[string]AWSConnector `yaml:"aws_connectors"`
}

// Defaults are server-wide fallbacks applied when a trigger omits the
// corresponding field. Individual trigger settings take precedence.
type Defaults struct {
	// TokenTTL is the parsed duration; zero means "use the hardcoded default".
	TokenTTL    time.Duration `yaml:"-"`
	// TokenTTLRaw is the raw string from YAML (e.g. "15m") before parsing.
	TokenTTLRaw string        `yaml:"token_ttl"`
	// TokenScopes is a list of scope names (e.g. ["repo:read","lfs:read"]).
	TokenScopes []string      `yaml:"token_scopes"`
	// Audience is the optional audience claim for minted tokens.
	Audience    string        `yaml:"audience"`
}

// ParseServeConfig unmarshals YAML into a ServeConfig and post-processes the
// token_ttl string into a time.Duration. Returns an error if the YAML is
// malformed or token_ttl is not a valid Go duration string.
func ParseServeConfig(data []byte) (ServeConfig, error) {
	var cfg ServeConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return ServeConfig{}, fmt.Errorf("buildtrigger: parse config: %w", err)
	}
	if raw := cfg.Build.Defaults.TokenTTLRaw; raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return ServeConfig{}, fmt.Errorf("buildtrigger: config build.defaults.token_ttl %q: %w", raw, err)
		}
		cfg.Build.Defaults.TokenTTL = d
	}
	return cfg, nil
}
