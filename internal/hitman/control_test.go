package hitman

import (
	"strings"
	"testing"
)

func TestRenderLaunchdPlistEscapesProgramArguments(t *testing.T) {
	plist := renderLaunchdPlist("com.hitman.test", []string{"/tmp/hitman&bin", "serve"}, "/tmp/err", "/tmp/out", "/tmp/work dir", "/Users/me")
	if !strings.Contains(plist, "<string>/tmp/hitman&amp;bin</string>") {
		t.Fatalf("plist did not XML-escape program argument:\n%s", plist)
	}
	if !strings.Contains(plist, "<string>/tmp/work dir</string>") {
		t.Fatalf("plist missing working directory:\n%s", plist)
	}
}

func TestRootDaemonBinaryPathIsSystemOwnedLocation(t *testing.T) {
	if got := rootDaemonBinaryPath(); got != "/Library/Application Support/hitman/bin/hitman" {
		t.Fatalf("rootDaemonBinaryPath = %q", got)
	}
}
