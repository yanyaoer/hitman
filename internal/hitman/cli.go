package hitman

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

var errPrintedUsage = errors.New("printed usage")

// Run is the single binary entrypoint used by cmd/hitman.
func Run(args []string) int {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("hitman ")

	if err := run(args); err != nil {
		if errors.Is(err, errPrintedUsage) {
			return 0
		}
		fmt.Fprintf(os.Stderr, "hitman: %v\n", err)
		return 1
	}
	return 0
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stdout)
		return errPrintedUsage
	}
	switch args[0] {
	case "", "-h", "--help", "help":
		printUsage(os.Stdout)
		return errPrintedUsage
	case "version", "--version":
		fmt.Fprintln(os.Stdout, versionLine())
		return nil
	case "serve":
		runServe()
		return nil
	case "netd":
		runNetd()
		return nil
	case "build":
		return cmdBuild()
	case "init":
		return cmdInit()
	case "install":
		return cmdInstall()
	case "uninstall":
		return cmdUninstall()
	case "restart":
		return cmdRestart()
	case "on":
		return cmdOn()
	case "off":
		return cmdOff()
	case "upstream", "egress":
		return cmdUpstream(args[1:])
	case "ca-trust":
		return cmdCATrust()
	case "ca-untrust":
		return cmdCAUntrust()
	case "status":
		return cmdStatus()
	case "logs":
		return cmdLogs()
	case "smoke":
		return cmdSmoke()
	case "smoke-mitm":
		return cmdSmokeMITM()
	case "update":
		return cmdUpdate(args[1:])
	default:
		printUsage(os.Stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func versionLine() string {
	return fmt.Sprintf("hitman %s (%s, %s)", version, commit, date)
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `hitman - Human In The Middle; watch the Agent Network

Usage: hitman <command>

  init             install/start services + trust CA + status + smoke
  install          install/start user MITM and root netd launchd services
  uninstall        stop/remove both launchd services and managed resolvers
  restart          kickstart installed services
  on               start capture using the installed binary
  off              stop netd and remove hitman-managed resolver files
  upstream         set upstream: socks [addr], http [addr], or system
  egress           alias for upstream
  ca-trust         add local CA to System keychain (sudo)
  ca-untrust       remove local CA from System keychain (sudo)
  status           show services, listeners, upstream, route, resolvers, CA
  logs             tail MITM and netd logs
  smoke            live DNS/TUN/netd smoke using a curl copy named codex
  smoke-mitm       direct MITM-only smoke using curl --connect-to
  update           replace this binary from the latest GitHub release
  version          print build version

Developer:
  build            go build -tags with_gvisor -> bin/hitman
  serve            run MITM service foreground
  netd             run root network daemon foreground
`)
}
