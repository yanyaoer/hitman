package hitman

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

type fileConfig struct {
	ListenAddr    string   `json:"listen"`
	UpstreamMode  string   `json:"upstreamMode"`
	UpstreamProxy string   `json:"upstreamProxy"`
	UpstreamDNS   string   `json:"upstreamDNS"`
	CADir         string   `json:"caDir"`
	AuditDir      string   `json:"auditDir"`
	AllowHosts    []string `json:"allowHosts"`

	MarkerText     string `json:"markerText"`
	TruncationStep *int   `json:"truncationStep"`
	MaxTierN       *int   `json:"maxTierN"`
	MaxContinue    *int   `json:"maxContinue"`
	DebugLog       *bool  `json:"debug"`
	AuditBodies    *bool  `json:"auditBodies"`

	NetDNSAddr      string   `json:"netDNS"`
	MITMAddr        string   `json:"mitmAddr"`
	FakeIPCIDR      string   `json:"fakeIPCIDR"`
	TunAddress      string   `json:"tunAddress"`
	InterfaceName   string   `json:"tunName"`
	Domains         []string `json:"domains"`
	DomainSuffixes  []string `json:"domainSuffixes"`
	Processes       []string `json:"processes"`
	ProcessPaths    []string `json:"processPaths"`
	ResolverDomains []string `json:"resolverDomains"`
}

func defaultConfigPath() string {
	if path := strings.TrimSpace(os.Getenv("HITMAN_CONFIG")); path != "" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return filepath.Join(".config", "hitman", "config.json")
	}
	return filepath.Join(home, ".config", "hitman", "config.json")
}

func defaultStateDir() string {
	if path := strings.TrimSpace(os.Getenv("HITMAN_STATE_DIR")); path != "" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return filepath.Join(".hitman")
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "hitman")
	}
	return filepath.Join(home, ".local", "state", "hitman")
}

func defaultLogDir() string {
	if path := strings.TrimSpace(os.Getenv("HITMAN_LOG_DIR")); path != "" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return filepath.Join(defaultStateDir(), "logs")
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Logs", "hitman")
	}
	return filepath.Join(defaultStateDir(), "logs")
}

func resolveStatePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return defaultStateDir()
	}
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(defaultStateDir(), path)
}

func loadFileConfig() (fileConfig, error) {
	path := defaultConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fileConfig{}, nil
		}
		return fileConfig{}, err
	}
	var cfg fileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fileConfig{}, err
	}
	return cfg, nil
}

func configString(envKey, fileValue, def string) string {
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		return v
	}
	if v := strings.TrimSpace(fileValue); v != "" {
		return v
	}
	return def
}

func configList(envKey string, fileValue []string, defCSV string) []string {
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		return splitCSV(v)
	}
	if fileValue != nil {
		out := make([]string, 0, len(fileValue))
		for _, value := range fileValue {
			if v := strings.TrimSpace(value); v != "" {
				out = append(out, v)
			}
		}
		return out
	}
	return splitCSV(defCSV)
}

func configInt(envKey string, fileValue *int, def int) int {
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	if fileValue != nil {
		return *fileValue
	}
	return def
}

func configBool(envKey string, fileValue *bool, def bool) bool {
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	if fileValue != nil {
		return *fileValue
	}
	return def
}

func configUpstreamProxy(cfg fileConfig) string {
	if v := strings.TrimSpace(os.Getenv("HITMAN_UPSTREAM_PROXY")); v != "" {
		return normalizeProxy(v)
	}
	if v := strings.TrimSpace(os.Getenv("HITMAN_SOCKS")); v != "" {
		return normalizeProxy(v)
	}
	return normalizeProxy(cfg.UpstreamProxy)
}

func configUpstreamMode(cfg fileConfig, upstreamProxy string) string {
	if v := strings.TrimSpace(os.Getenv("HITMAN_UPSTREAM_MODE")); v != "" {
		return normalizeUpstreamMode(v)
	}
	if v := strings.TrimSpace(cfg.UpstreamMode); v != "" {
		return normalizeUpstreamMode(v)
	}
	if upstreamProxy != "" {
		return "proxy"
	}
	return defaultUpstreamMode
}
