package config

import (
	"fmt"
	"os"
	"runtime"

	"gopkg.in/yaml.v3"
)

// DefaultPath is where stretchy looks for its configuration on Linux.
const DefaultPath = "/etc/stretchy/config.yaml"

type Config struct {
	Server  Server  `yaml:"server"`
	Auth    Auth    `yaml:"auth"`
	Storage Storage `yaml:"storage"`
	Logging Logging `yaml:"logging"`
}

type Server struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

// Auth enables HTTP basic auth when both fields are set.
type Auth struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type Storage struct {
	DataDir string `yaml:"data_dir"`
}

type Logging struct {
	// Dir receives stretchy.log; empty means stderr only.
	Dir   string `yaml:"dir"`
	Level string `yaml:"level"`
}

func Defaults() *Config {
	cfg := &Config{
		Server:  Server{Host: "127.0.0.1", Port: 9200},
		Storage: Storage{DataDir: "/var/lib/stretchy"},
		Logging: Logging{Dir: "/var/log/stretchy", Level: "info"},
	}
	if runtime.GOOS != "linux" {
		// Development fallback outside the target platform.
		cfg.Storage.DataDir = "data"
		cfg.Logging.Dir = ""
	}
	return cfg
}

// Load reads the YAML config at path. A missing file at the default
// location is not an error: defaults are used so a plain `stretchy`
// still starts during development.
func Load(path string) (*Config, error) {
	cfg := Defaults()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && path == DefaultPath {
			return cfg, nil
		}
		return nil, err
	}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Server.Port <= 0 || cfg.Server.Port > 65535 {
		return nil, fmt.Errorf("invalid server.port %d", cfg.Server.Port)
	}
	if cfg.Storage.DataDir == "" {
		return nil, fmt.Errorf("storage.data_dir must not be empty")
	}
	return cfg, nil
}
