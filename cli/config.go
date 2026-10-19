package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config is ~/.config/forseti/config.json (all fields optional; defaults apply
// on load). It is the CLI's own state — provider keys and pi/ttt settings stay
// in their native files (see configTargets).
type Config struct {
	Repo string `json:"repo,omitempty"`

	Herdr struct {
		Bin         string `json:"bin"`
		Agent       string `json:"agent"`
		EditorLabel string `json:"editor_label"`
	} `json:"herdr"`

	TTT struct {
		Bin  string `json:"bin"`
		Port int    `json:"port"`
	} `json:"ttt"`

	Pi struct {
		Bin string `json:"bin"`
	} `json:"pi"`

	Services struct {
		Memory bool `json:"memory"`
		Laya   bool `json:"laya"`
	} `json:"services"`

	Crew struct {
		Engine  string `json:"engine"`
		Host    string `json:"host"`
		Model   string `json:"model"`
		Default string `json:"default,omitempty"`
	} `json:"crew"`
}

func defaultConfig() Config {
	var c Config
	c.Herdr.Bin = "herdr"
	c.Herdr.Agent = "coder"
	c.Herdr.EditorLabel = "forseti"
	c.TTT.Bin = "ttt"
	c.TTT.Port = 4242
	c.Pi.Bin = "pi"
	c.Services.Memory = true
	c.Services.Laya = false
	c.Crew.Engine = "rust"
	c.Crew.Host = "herdr"
	return c
}

func configPath() string {
	if p := os.Getenv("FORSETI_CONFIG"); p != "" {
		return expandHome(p)
	}
	return filepath.Join(homeDir(), ".config", "forseti", "config.json")
}

// loadConfig reads the file (missing = defaults) and fills empty fields.
func loadConfig() Config {
	c := defaultConfig()
	data, err := os.ReadFile(configPath())
	if err != nil {
		return c
	}
	if err := json.Unmarshal(data, &c); err != nil {
		warn("config %s is not valid JSON (%v) — using defaults", configPath(), err)
		return defaultConfig()
	}
	applyDefaults(&c)
	return c
}

func applyDefaults(c *Config) {
	d := defaultConfig()
	if c.Herdr.Bin == "" {
		c.Herdr.Bin = d.Herdr.Bin
	}
	if c.Herdr.Agent == "" {
		c.Herdr.Agent = d.Herdr.Agent
	}
	if c.Herdr.EditorLabel == "" {
		c.Herdr.EditorLabel = d.Herdr.EditorLabel
	}
	if c.TTT.Bin == "" {
		c.TTT.Bin = d.TTT.Bin
	}
	if c.TTT.Port == 0 {
		c.TTT.Port = d.TTT.Port
	}
	if c.Pi.Bin == "" {
		c.Pi.Bin = d.Pi.Bin
	}
	if c.Crew.Engine == "" {
		c.Crew.Engine = d.Crew.Engine
	}
	if c.Crew.Host == "" {
		c.Crew.Host = d.Crew.Host
	}
}

func saveConfig(c Config) error {
	path := configPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// getKey reads one flattened config key.
func getKey(c Config, key string) (string, bool) {
	switch key {
	case "repo":
		return c.Repo, true
	case "herdr.bin":
		return c.Herdr.Bin, true
	case "herdr.agent":
		return c.Herdr.Agent, true
	case "herdr.editor_label":
		return c.Herdr.EditorLabel, true
	case "ttt.bin":
		return c.TTT.Bin, true
	case "ttt.port":
		return strconv.Itoa(c.TTT.Port), true
	case "pi.bin":
		return c.Pi.Bin, true
	case "services.memory":
		return strconv.FormatBool(c.Services.Memory), true
	case "services.laya":
		return strconv.FormatBool(c.Services.Laya), true
	case "crew.engine":
		return c.Crew.Engine, true
	case "crew.host":
		return c.Crew.Host, true
	case "crew.model":
		return c.Crew.Model, true
	case "crew.default":
		return c.Crew.Default, true
	}
	return "", false
}

var configKeys = []string{
	"repo",
	"herdr.bin", "herdr.agent", "herdr.editor_label",
	"ttt.bin", "ttt.port",
	"pi.bin",
	"services.memory", "services.laya",
	"crew.engine", "crew.host", "crew.model", "crew.default",
}

// setKey writes one flattened config key with validation.
func setKey(c *Config, key, value string) error {
	switch key {
	case "repo":
		c.Repo = expandHome(value)
	case "herdr.bin":
		c.Herdr.Bin = value
	case "herdr.agent":
		c.Herdr.Agent = value
	case "herdr.editor_label":
		c.Herdr.EditorLabel = value
	case "ttt.bin":
		c.TTT.Bin = value
	case "ttt.port":
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("ttt.port must be 1..65535")
		}
		c.TTT.Port = n
	case "pi.bin":
		c.Pi.Bin = value
	case "services.memory":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("services.memory must be true or false")
		}
		c.Services.Memory = b
	case "services.laya":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("services.laya must be true or false")
		}
		c.Services.Laya = b
	case "crew.engine":
		if value != "rust" && value != "go" {
			return fmt.Errorf("crew.engine must be rust or go")
		}
		c.Crew.Engine = value
	case "crew.host":
		if value != "herdr" && value != "ttt" {
			return fmt.Errorf("crew.host must be herdr or ttt")
		}
		c.Crew.Host = value
	case "crew.model":
		c.Crew.Model = value
	case "crew.default":
		c.Crew.Default = expandHome(value)
	default:
		return fmt.Errorf("unknown key %q (try: %s)", key, strings.Join(configKeys, ", "))
	}
	return nil
}

// configTargets maps a human target to the file it edits.
func configTargets(c Config) map[string]string {
	piDir := filepath.Join(homeDir(), ".pi", "agent")
	return map[string]string{
		"forseti":      configPath(),
		"pi-models":    filepath.Join(piDir, "models.json"),
		"pi-settings":  filepath.Join(piDir, "settings.json"),
		"pi-mcp":       filepath.Join(piDir, "mcp.json"),
		"ttt-settings": filepath.Join(homeDir(), ".config", "ttt", "settings.json"),
		"ttt-plugins":  filepath.Join(homeDir(), ".config", "ttt", "plugins", "forseti", "init.lua"),
		"ttt-registry": filepath.Join(homeDir(), ".config", "ttt", "plugins.ttt.json"),
		"crew":         filepath.Join(repoOrEmpty(c), "crew", "examples", "crew.yaml"),
		"tools":        filepath.Join(homeDir(), ".config", "forseti", "tools.d"),
	}
}

func repoOrEmpty(c Config) string {
	r, _ := findRepo(c)
	return r
}

func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}

func expandHome(p string) string {
	if p == "~" {
		return homeDir()
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(homeDir(), p[2:])
	}
	return p
}

func tttPluginDir() string {
	if v := os.Getenv("FORSETI_TTT_PLUGIN_DIR"); v != "" {
		return expandHome(v)
	}
	return filepath.Join(homeDir(), ".config", "ttt", "plugins", "forseti")
}

func piExtensionPath() string {
	if v := os.Getenv("FORSETI_PI_EXTENSION"); v != "" {
		return expandHome(v)
	}
	return filepath.Join(homeDir(), ".pi", "agent", "extensions", "forseti.ts")
}
