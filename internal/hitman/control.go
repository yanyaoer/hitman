package hitman

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	launchAgentLabel  = "com.hitman.srv"
	launchDaemonLabel = "com.hitman.net"
	rootInstallDir    = "/Library/Application Support/hitman"
	defaultSocksAddr  = "127.0.0.1:2333"
	defaultHTTPAddr   = "127.0.0.1:2334"
	resolverFileMark  = "# hitman managed resolver"
)

func cmdBuild() error {
	if _, err := exec.LookPath("go"); err != nil {
		return fmt.Errorf("missing required tool: go")
	}
	if err := os.MkdirAll("bin", 0o755); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	ldflags := strings.Join([]string{
		"-X", "hitman/internal/hitman.version=dev",
		"-X", "hitman/internal/hitman.commit=local",
		"-X", "hitman/internal/hitman.date=" + now,
	}, " ")
	err := runInteractive("go", "build", "-tags", "with_gvisor", "-trimpath", "-ldflags", ldflags, "-o", "bin/hitman", "./cmd/hitman")
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "built bin/hitman (with_gvisor)")
	return nil
}

func cmdInit() error {
	if err := cmdInstall(); err != nil {
		return err
	}
	time.Sleep(time.Second)
	if err := cmdCATrust(); err != nil {
		return err
	}
	if err := cmdStatus(); err != nil {
		return err
	}
	if err := cmdSmoke(); err != nil {
		fmt.Fprintf(os.Stderr, "(smoke failed: %v)\n", err)
	}
	return nil
}

func cmdInstall() error {
	if err := cmdInstallUser(); err != nil {
		return err
	}
	return cmdInstallNet()
}

func cmdInstallUser() error {
	exe, err := currentExecutableForInstall()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	logDir := defaultLogDir()
	if err := os.MkdirAll(defaultStateDir(), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	dst := launchAgentPath(home)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	plist := renderLaunchdPlist(launchAgentLabel, []string{exe, "serve"},
		filepath.Join(logDir, "hitman.err.log"),
		filepath.Join(logDir, "hitman.out.log"),
		defaultStateDir(), home)
	if err := os.WriteFile(dst, []byte(plist), 0o644); err != nil {
		return err
	}
	if err := startUserService(dst); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "installed + started user service %s\n", launchAgentLabel)
	return nil
}

func cmdInstallNet() error {
	exe, err := currentExecutableForInstall()
	if err != nil {
		return err
	}
	if err := installRootDaemonBinary(exe); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	logDir := defaultLogDir()
	if err := os.MkdirAll(defaultStateDir(), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	plist := renderLaunchdPlist(launchDaemonLabel, []string{rootDaemonBinaryPath(), "netd"},
		filepath.Join(logDir, "hitman-net.err.log"),
		filepath.Join(logDir, "hitman-net.out.log"),
		defaultStateDir(), home)
	tmp, err := os.CreateTemp("", launchDaemonLabel+".*.plist")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString(plist); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := runInteractive("sudo", "mkdir", "-p", filepath.Dir(launchDaemonPath())); err != nil {
		return err
	}
	if err := runInteractive("sudo", "cp", tmpPath, launchDaemonPath()); err != nil {
		return err
	}
	if err := runInteractive("sudo", "chown", "root:wheel", launchDaemonPath()); err != nil {
		return err
	}
	if err := runInteractive("sudo", "chmod", "644", launchDaemonPath()); err != nil {
		return err
	}
	if err := startNetService(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "installed + started root service %s\n", launchDaemonLabel)
	return nil
}

func cmdUninstall() error {
	_ = runQuiet("launchctl", "bootout", guiLabel())
	_ = runQuiet("sudo", "launchctl", "bootout", "system/"+launchDaemonLabel)
	if err := os.Remove(launchAgentPathFromCurrentHome()); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = runInteractive("sudo", "rm", "-f", launchDaemonPath())
	_ = runInteractive("sudo", "rm", "-f", rootDaemonBinaryPath())
	if err := cleanupManagedResolverFiles(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "uninstalled %s and %s\n", launchAgentLabel, launchDaemonLabel)
	return nil
}

func cmdRestart() error {
	if err := startUserService(launchAgentPathFromCurrentHome()); err != nil {
		return err
	}
	if _, err := os.Stat(launchDaemonPath()); err == nil {
		exe, err := currentExecutableForInstall()
		if err != nil {
			return err
		}
		if err := installRootDaemonBinary(exe); err != nil {
			return err
		}
		if err := startNetService(); err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "restarted %s and %s\n", launchAgentLabel, launchDaemonLabel)
		return nil
	}
	fmt.Fprintf(os.Stdout, "restarted %s (netd is not installed)\n", launchAgentLabel)
	return nil
}

func cmdOn() error {
	return cmdInstall()
}

func cmdOff() error {
	_ = runQuiet("sudo", "launchctl", "bootout", "system/"+launchDaemonLabel)
	if err := cleanupManagedResolverFiles(); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "capture disabled (netd stopped, hitman resolver files removed)")
	return nil
}

func cmdUpstream(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: hitman upstream <socks [addr] | http [addr] | system>")
	}
	mode := strings.ToLower(strings.TrimSpace(args[0]))
	addr := ""
	if len(args) > 1 {
		addr = strings.TrimSpace(args[1])
	}
	upstreamMode := ""
	upstreamProxy := ""
	switch mode {
	case "socks", "socks5":
		upstreamMode = "proxy"
		if addr == "" {
			addr = defaultSocksAddr
		}
		upstreamProxy = normalizeProxy("socks5://" + addr)
	case "http", "mixed":
		upstreamMode = "proxy"
		if addr == "" {
			addr = defaultHTTPAddr
		}
		upstreamProxy = normalizeProxy("http://" + addr)
	case "system", "direct":
		upstreamMode = "system"
	default:
		return fmt.Errorf("usage: hitman upstream <socks [addr] | http [addr] | system>")
	}
	if err := writeUpstreamConfig(upstreamMode, upstreamProxy); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "upstream mode set to: %s\n", upstreamMode)
	if upstreamMode == "proxy" {
		fmt.Fprintf(os.Stdout, "upstream proxy set to: %s\n", upstreamProxy)
	} else if cfg, err := loadConfig(); err == nil {
		fmt.Fprintf(os.Stdout, "upstream system DNS: %s\n", cfg.UpstreamDNS)
	}
	fmt.Fprintf(os.Stdout, "config: %s\n", defaultConfigPath())
	if _, err := os.Stat(launchAgentPathFromCurrentHome()); err == nil {
		if err := cmdInstallUser(); err != nil {
			return err
		}
	}
	if _, err := os.Stat(launchDaemonPath()); err == nil {
		if err := cmdInstallNet(); err != nil {
			return err
		}
	}
	return nil
}

func cmdCATrust() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	caPem := filepath.Join(cfg.CADir, "hitman-ca.pem")
	if _, err := os.Stat(caPem); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("CA not found at %s (start the service once first: hitman install)", caPem)
		}
		return err
	}
	fmt.Fprintln(os.Stdout, "adding CA to System keychain (admin password required)...")
	if err := runInteractive("sudo", "security", "add-trusted-cert", "-d", "-r", "trustRoot", "-k", "/Library/Keychains/System.keychain", caPem); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "CA trusted")
	return nil
}

func cmdCAUntrust() error {
	_ = runQuiet("sudo", "security", "delete-certificate", "-c", "hitman CA", "/Library/Keychains/System.keychain")
	fmt.Fprintln(os.Stdout, "CA trust removed")
	return nil
}

func cmdStatus() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	netCfg, err := loadNetConfig()
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "== launchd ==")
	printLaunchdState(guiLabel(), "user", false)
	printLaunchdState("system/"+launchDaemonLabel, "netd", false)

	fmt.Fprintln(os.Stdout, "== listeners ==")
	listenPort := addrPort(cfg.ListenAddr)
	dnsPort := addrPort(netCfg.DNSAddr)
	if tcpPortOpen(listenPort) {
		fmt.Fprintf(os.Stdout, "  mitm :%s listening\n", listenPort)
	} else {
		fmt.Fprintf(os.Stdout, "  mitm :%s NOT listening\n", listenPort)
	}
	if dnsPortOpen(dnsPort) {
		fmt.Fprintf(os.Stdout, "  dns  :%s listening\n", dnsPort)
	} else {
		fmt.Fprintf(os.Stdout, "  dns  :%s NOT listening\n", dnsPort)
	}

	fmt.Fprintln(os.Stdout, "== upstream ==")
	fmt.Fprintf(os.Stdout, "  mode: %s\n", cfg.UpstreamMode)
	if cfg.UpstreamMode == "system" || cfg.UpstreamMode == "direct" {
		fmt.Fprintf(os.Stdout, "  real-IP DNS: %s\n", cfg.UpstreamDNS)
	} else if cfg.UpstreamProxy == "" {
		fmt.Fprintln(os.Stdout, "  proxy: missing")
	} else {
		hp := proxyHostPort(cfg.UpstreamProxy)
		host, port, _ := net.SplitHostPort(hp)
		fmt.Fprintf(os.Stdout, "  proxy: %s\n", cfg.UpstreamProxy)
		if tcpAddrOpen(net.JoinHostPort(host, port)) {
			fmt.Fprintln(os.Stdout, "  reachable")
		} else {
			fmt.Fprintln(os.Stdout, "  NOT reachable")
		}
	}

	fmt.Fprintln(os.Stdout, "== config ==")
	fmt.Fprintf(os.Stdout, "  path: %s\n", defaultConfigPath())
	if _, err := os.Stat(defaultConfigPath()); err == nil {
		fmt.Fprintln(os.Stdout, "  source: file + defaults")
	} else {
		fmt.Fprintln(os.Stdout, "  source: defaults")
	}
	fmt.Fprintf(os.Stdout, "  state: %s\n", defaultStateDir())
	fmt.Fprintf(os.Stdout, "  logs: %s\n", defaultLogDir())

	fmt.Fprintln(os.Stdout, "== route ==")
	if out, err := commandOutput("netstat", "-rn", "-f", "inet"); err == nil {
		found := false
		for _, line := range strings.Split(out, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			if strings.HasPrefix(fields[0], "198.18.") || fields[0] == "198.18/15" || fields[0] == "198.18.0.0/15" {
				fmt.Fprintln(os.Stdout, "  "+line)
				found = true
			}
		}
		if !found {
			fmt.Fprintln(os.Stdout, "  fake-IP route not found")
		}
	} else {
		fmt.Fprintln(os.Stdout, "  netstat unavailable")
	}

	fmt.Fprintln(os.Stdout, "== resolvers ==")
	for _, domain := range netCfg.ResolverDomains {
		path := filepath.Join("/etc/resolver", domain)
		if b, err := os.ReadFile(path); err == nil && bytes.Contains(b, []byte(resolverFileMark)) {
			fmt.Fprintf(os.Stdout, "  %s -> hitman\n", domain)
		} else if err == nil {
			fmt.Fprintf(os.Stdout, "  %s -> exists but not hitman-managed\n", domain)
		} else {
			fmt.Fprintf(os.Stdout, "  %s -> missing\n", domain)
		}
	}

	fmt.Fprintln(os.Stdout, "== intercept targets ==")
	fmt.Fprintf(os.Stdout, "  domains: %s\n", strings.Join(netCfg.Domains, ","))
	fmt.Fprintf(os.Stdout, "  domain_suffixes: %s\n", strings.Join(netCfg.DomainSuffixes, ","))
	fmt.Fprintf(os.Stdout, "  processes: %s\n", strings.Join(netCfg.Processes, ","))

	fmt.Fprintln(os.Stdout, "== CA trust ==")
	if err := runQuiet("security", "find-certificate", "-c", "hitman CA", "/Library/Keychains/System.keychain"); err == nil {
		fmt.Fprintln(os.Stdout, "  trusted")
	} else {
		fmt.Fprintln(os.Stdout, "  NOT trusted")
	}
	return nil
}

func cmdLogs() error {
	logDir := defaultLogDir()
	return runInteractive("tail", "-f",
		filepath.Join(logDir, "hitman.err.log"),
		filepath.Join(logDir, "hitman.out.log"),
		filepath.Join(logDir, "hitman-net.err.log"),
		filepath.Join(logDir, "hitman-net.out.log"))
}

func cmdSmoke() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	netCfg, err := loadNetConfig()
	if err != nil {
		return err
	}
	caPem := filepath.Join(cfg.CADir, "hitman-ca.pem")
	if _, err := os.Stat(caPem); err != nil {
		return fmt.Errorf("CA not found at %s", caPem)
	}
	dnsPort := addrPort(netCfg.DNSAddr)
	if !dnsPortOpen(dnsPort) {
		return fmt.Errorf("netd DNS is not listening on :%s (run hitman on)", dnsPort)
	}
	token, err := codexToken()
	if err != nil {
		return err
	}
	curlPath, err := exec.LookPath("curl")
	if err != nil {
		return fmt.Errorf("missing required tool: curl")
	}
	tmp, err := os.MkdirTemp("", "hitman-smoke-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	codexCurl := filepath.Join(tmp, "codex")
	if err := copyFile(curlPath, codexCurl, 0o755); err != nil {
		return err
	}
	body := filepath.Join(tmp, "body")
	fmt.Fprintln(os.Stdout, "sending /backend-api/codex/models through DNS/TUN/netd as process name 'codex'...")
	out, err := commandOutputEnv([]string{"CODEX_TOKEN=" + token}, codexCurl,
		"-sS", "--max-time", "60", "--cacert", caPem,
		"-o", body, "-w", "status=%{http_code} bytes=%{size_download}\n",
		"https://chatgpt.com/backend-api/codex/models",
		"-H", "Authorization: Bearer "+token,
		"-H", "User-Agent: codex_cli_rs/0.142.5 (hitman smoke)")
	if err != nil && strings.TrimSpace(out) == "" {
		return err
	}
	if curlStatusLineOK(out) {
		fmt.Fprintf(os.Stdout, "SMOKE PASS: DNS/TUN/netd -> hitman relayed HTTP response (%s)\n", strings.TrimSpace(out))
		return nil
	}
	return fmt.Errorf("Codex models request failed: %s", strings.TrimSpace(out))
}

func cmdSmokeMITM() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	caPem := filepath.Join(cfg.CADir, "hitman-ca.pem")
	if _, err := os.Stat(caPem); err != nil {
		return fmt.Errorf("CA not found at %s", caPem)
	}
	token, err := codexToken()
	if err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil {
		return err
	}
	payload := `{"model":"gpt-5.5","stream":true,"store":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Reply with the single word ok"}]}],"reasoning":{"effort":"low"}}`
	fmt.Fprintln(os.Stdout, "sending gpt-5.5 stream directly to MITM listener with curl --connect-to...")
	out, err := commandOutput("curl",
		"-sN", "--max-time", "300",
		"--connect-to", "chatgpt.com:443:"+host+":"+port,
		"--cacert", caPem,
		"https://chatgpt.com/backend-api/codex/responses",
		"-H", "Authorization: Bearer "+token,
		"-H", "Content-Type: application/json",
		"-H", "User-Agent: codex_cli_rs/0.142.5 (hitman smoke-mitm)",
		"-d", payload)
	if err != nil && strings.TrimSpace(out) == "" {
		return err
	}
	if strings.Contains(out, "proxy_rounds") || strings.Contains(out, "response.completed") {
		fmt.Fprintln(os.Stdout, "SMOKE PASS: MITM relayed a terminal response.completed")
		return nil
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	return fmt.Errorf("no terminal event seen. Last lines: %s", strings.Join(lines, "\n"))
}

func currentExecutableForInstall() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if strings.Contains(exe, string(filepath.Separator)+"go-build") {
		return "", fmt.Errorf("installing from go run is not supported; run `go build -tags with_gvisor -trimpath -o bin/hitman ./cmd/hitman` and execute bin/hitman")
	}
	info, err := os.Stat(exe)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", exe)
	}
	return exe, nil
}

func installRootDaemonBinary(exe string) error {
	if err := runInteractive("sudo", "install", "-d", "-o", "root", "-g", "wheel", "-m", "755", filepath.Dir(rootDaemonBinaryPath())); err != nil {
		return err
	}
	return runInteractive("sudo", "install", "-o", "root", "-g", "wheel", "-m", "755", exe, rootDaemonBinaryPath())
}

func renderLaunchdPlist(label string, args []string, stderrPath, stdoutPath, workDir, home string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>`)
	b.WriteString(xmlString(home))
	b.WriteString(`</string>
		<key>PATH</key>
		<string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
	</dict>
	<key>KeepAlive</key>
	<true/>
	<key>Label</key>
	<string>`)
	b.WriteString(xmlString(label))
	b.WriteString(`</string>
	<key>ProgramArguments</key>
	<array>
`)
	for _, arg := range args {
		b.WriteString("		<string>")
		b.WriteString(xmlString(arg))
		b.WriteString("</string>\n")
	}
	b.WriteString(`	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>StandardErrorPath</key>
	<string>`)
	b.WriteString(xmlString(stderrPath))
	b.WriteString(`</string>
	<key>StandardOutPath</key>
	<string>`)
	b.WriteString(xmlString(stdoutPath))
	b.WriteString(`</string>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>WorkingDirectory</key>
	<string>`)
	b.WriteString(xmlString(workDir))
	b.WriteString(`</string>
</dict>
</plist>
`)
	return b.String()
}

func xmlString(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func startUserService(plistPath string) error {
	_ = runQuiet("launchctl", "bootout", guiLabel())
	var last string
	for range 5 {
		if out, err := commandOutput("launchctl", "bootstrap", "gui/"+strconv.Itoa(os.Getuid()), plistPath); err == nil {
			break
		} else {
			last = out
		}
		if launchdLoaded(guiLabel(), false) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !launchdLoaded(guiLabel(), false) {
		return fmt.Errorf("bootstrap %s failed: %s", launchAgentLabel, strings.TrimSpace(last))
	}
	return runInteractive("launchctl", "kickstart", "-k", guiLabel())
}

func startNetService() error {
	_ = runQuiet("sudo", "launchctl", "bootout", "system/"+launchDaemonLabel)
	var last string
	for range 5 {
		if out, err := commandOutput("sudo", "launchctl", "bootstrap", "system", launchDaemonPath()); err == nil {
			break
		} else {
			last = out
		}
		if launchdLoaded("system/"+launchDaemonLabel, true) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !launchdLoaded("system/"+launchDaemonLabel, true) {
		return fmt.Errorf("bootstrap %s failed: %s", launchDaemonLabel, strings.TrimSpace(last))
	}
	return runInteractive("sudo", "launchctl", "kickstart", "-k", "system/"+launchDaemonLabel)
}

func printLaunchdState(label, name string, sudo bool) {
	out, err := launchdPrint(label, sudo)
	if err != nil {
		fmt.Fprintf(os.Stdout, "  %s: not loaded\n", name)
		return
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "state = ") {
			fmt.Fprintf(os.Stdout, "  %s: %s\n", name, line)
			return
		}
	}
	fmt.Fprintf(os.Stdout, "  %s: loaded\n", name)
}

func launchdLoaded(label string, sudo bool) bool {
	_, err := launchdPrint(label, sudo)
	return err == nil
}

func launchdPrint(label string, sudo bool) (string, error) {
	if sudo {
		return commandOutput("sudo", "launchctl", "print", label)
	}
	return commandOutput("launchctl", "print", label)
}

func cleanupManagedResolverFiles() error {
	netCfg, err := loadNetConfig()
	if err != nil {
		return err
	}
	for _, domain := range netCfg.ResolverDomains {
		path := filepath.Join("/etc/resolver", domain)
		b, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			if grepErr := runQuiet("sudo", "grep", "-qF", resolverFileMark, path); grepErr != nil {
				fmt.Fprintf(os.Stderr, "warning: skip resolver cleanup for %s: %v\n", path, err)
				continue
			}
		} else if !bytes.Contains(b, []byte(resolverFileMark)) {
			continue
		}
		if err := runInteractive("sudo", "rm", "-f", path); err != nil {
			return err
		}
	}
	return nil
}

func writeUpstreamConfig(upstreamMode, upstreamProxy string) error {
	path := defaultConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	cfg := map[string]any{}
	if data, err := os.ReadFile(path); err == nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	cfg["upstreamMode"] = upstreamMode
	if upstreamMode == "proxy" {
		cfg["upstreamProxy"] = upstreamProxy
	} else {
		delete(cfg, "upstreamProxy")
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func codexToken() (string, error) {
	if token := strings.TrimSpace(os.Getenv("CODEX_TOKEN")); token != "" {
		return token, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		return "", fmt.Errorf("no token: set CODEX_TOKEN or ensure ~/.codex/auth.json has .tokens.access_token")
	}
	var root struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return "", err
	}
	if strings.TrimSpace(root.Tokens.AccessToken) == "" {
		return "", fmt.Errorf("no token: set CODEX_TOKEN or ensure ~/.codex/auth.json has .tokens.access_token")
	}
	return root.Tokens.AccessToken, nil
}

func guiLabel() string {
	return "gui/" + strconv.Itoa(os.Getuid()) + "/" + launchAgentLabel
}

func launchAgentPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
}

func launchAgentPathFromCurrentHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("Library", "LaunchAgents", launchAgentLabel+".plist")
	}
	return launchAgentPath(home)
}

func launchDaemonPath() string {
	return filepath.Join("/Library/LaunchDaemons", launchDaemonLabel+".plist")
}

func rootDaemonBinaryPath() string {
	return filepath.Join(rootInstallDir, "bin", "hitman")
}

func addrPort(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err == nil {
		return port
	}
	if i := strings.LastIndex(addr, ":"); i >= 0 && i+1 < len(addr) {
		return addr[i+1:]
	}
	return addr
}

func proxyHostPort(proxy string) string {
	proxy = strings.TrimPrefix(proxy, "socks5://")
	proxy = strings.TrimPrefix(proxy, "socks://")
	proxy = strings.TrimPrefix(proxy, "http://")
	return proxy
}

func tcpPortOpen(port string) bool {
	return tcpAddrOpen(net.JoinHostPort("127.0.0.1", port)) || lsofPortOpen("TCP", port)
}

func dnsPortOpen(port string) bool {
	return tcpPortOpen(port) || lsofPortOpen("UDP", port)
}

func tcpAddrOpen(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func lsofPortOpen(proto, port string) bool {
	if proto == "TCP" {
		if err := runQuiet("lsof", "-nP", "-iTCP:"+port, "-sTCP:LISTEN"); err == nil {
			return true
		}
		return runQuiet("sudo", "-n", "lsof", "-nP", "-iTCP:"+port, "-sTCP:LISTEN") == nil
	}
	if err := runQuiet("lsof", "-nP", "-iUDP:"+port); err == nil {
		return true
	}
	return runQuiet("sudo", "-n", "lsof", "-nP", "-iUDP:"+port) == nil
}

func curlStatusLineOK(out string) bool {
	for _, field := range strings.Fields(out) {
		if !strings.HasPrefix(field, "status=") {
			continue
		}
		code, err := strconv.Atoi(strings.TrimPrefix(field, "status="))
		return err == nil && code >= 100 && code <= 599
	}
	return false
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func runInteractive(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runQuiet(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	return cmd.Run()
}

func commandOutput(name string, args ...string) (string, error) {
	return commandOutputEnv(nil, name, args...)
}

func commandOutputEnv(env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}
