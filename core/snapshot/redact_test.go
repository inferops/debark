package snapshot

import (
	"strings"
	"testing"
)

func TestRedactProxyBytesBlanketDirectives(t *testing.T) {
	in := `Acquire::http::Proxy "http://user:secret@proxy.example.com:3128/";
Acquire::https::Proxy "https://proxy.example.com:3128/";
`
	out, changed := redactProxyBytes([]byte(in))
	if !changed {
		t.Fatal("expected a change")
	}
	s := string(out)
	if strings.Contains(s, "secret") || strings.Contains(s, "proxy.example.com") {
		t.Errorf("proxy host/credentials leaked: %s", s)
	}
	if !strings.Contains(s, `Acquire::http::Proxy "REDACTED";`) {
		t.Errorf("http proxy not blanket-redacted: %s", s)
	}
	if !strings.Contains(s, `Acquire::https::Proxy "REDACTED";`) {
		t.Errorf("https proxy not blanket-redacted: %s", s)
	}
}

func TestRedactProxyBytesDirectIsLeftAlone(t *testing.T) {
	in := `Acquire::http::Proxy "DIRECT";` + "\n"
	out, changed := redactProxyBytes([]byte(in))
	if changed || string(out) != in {
		t.Errorf("an explicit DIRECT (no proxy) carries no secret and should be left alone: changed=%v out=%q", changed, out)
	}
}

func TestRedactProxyBytesPerHostOverrideOnlyCredentials(t *testing.T) {
	in := `Acquire::http::Proxy::internal.example.com "http://user:secret@internal-proxy:3128/";` + "\n"
	out, changed := redactProxyBytes([]byte(in))
	if !changed {
		t.Fatal("expected a change")
	}
	s := string(out)
	if strings.Contains(s, "secret") {
		t.Errorf("credentials leaked: %s", s)
	}
	if !strings.Contains(s, "internal-proxy") || !strings.Contains(s, "internal.example.com") {
		t.Errorf("host information should survive a credential-only redaction: %s", s)
	}
}

func TestRedactProxyBytesPerHostNoCredentialsUntouched(t *testing.T) {
	in := `Acquire::http::Proxy::internal.example.com "http://internal-proxy:3128/";` + "\n"
	out, changed := redactProxyBytes([]byte(in))
	if changed || string(out) != in {
		t.Errorf("a proxy override with no credentials should be left byte-for-byte alone: changed=%v out=%q", changed, out)
	}
}

func TestRedactProxyBytesGenericCredentialedValueAnyKey(t *testing.T) {
	in := `Acquire::ftp::Proxy "ftp://bob:hunter2@ftp-proxy.example.com/";` + "\n"
	out, changed := redactProxyBytes([]byte(in))
	if !changed || strings.Contains(string(out), "hunter2") {
		t.Errorf("credentials on a non-http(s) proxy key leaked: changed=%v out=%s", changed, out)
	}
}

func TestRedactProxyBytesPreservesFormattingAndComments(t *testing.T) {
	in := `// comment above
Acquire::http::Proxy "http://user:secret@host/"; // trailing comment
APT::Install-Recommends "false";
`
	out, changed := redactProxyBytes([]byte(in))
	if !changed {
		t.Fatal("expected a change")
	}
	s := string(out)
	if !strings.Contains(s, "// comment above") || !strings.Contains(s, "// trailing comment") {
		t.Errorf("comments were not preserved: %s", s)
	}
	if !strings.Contains(s, `APT::Install-Recommends "false";`) {
		t.Errorf("unrelated key was altered: %s", s)
	}
}

func TestRedactProxyBytesNoProxyNoChange(t *testing.T) {
	in := `APT::Install-Recommends "false";` + "\n"
	out, changed := redactProxyBytes([]byte(in))
	if changed || string(out) != in {
		t.Errorf("a file with no proxy directive must be untouched: changed=%v", changed)
	}
}

func TestDoRedactMachineIDAndLabels(t *testing.T) {
	s := &Snapshot{
		Target: Target{MachineID: "abcdef0123456789abcdef0123456789"},
		Labels: map[string]string{"site": "hq"},
	}
	fs := &FileSet{Bytes: map[string][]byte{}}
	doRedact(s, fs, RedactMachineID, RedactLabels)

	if s.Target.MachineID != "" {
		t.Errorf("machine id not cleared: %q", s.Target.MachineID)
	}
	if s.Labels != nil {
		t.Errorf("labels not cleared: %v", s.Labels)
	}
	want := []string{RedactLabels, RedactMachineID} // sorted
	if !equalStrings(s.Redactions, want) {
		t.Errorf("Redactions = %v, want %v", s.Redactions, want)
	}
}

func TestDoRedactRecordsKindEvenWhenNothingToRemove(t *testing.T) {
	s := &Snapshot{} // MachineID already empty
	doRedact(s, &FileSet{Bytes: map[string][]byte{}}, RedactMachineID)
	if len(s.Redactions) != 1 || s.Redactions[0] != RedactMachineID {
		t.Errorf("Redactions = %v, want [%s] even though there was nothing to remove", s.Redactions, RedactMachineID)
	}
}

func TestDoRedactProxiesUpdatesDigestAndFlag(t *testing.T) {
	confPath := "/etc/apt/apt.conf.d/99proxy"
	ap := toArchivePath(confPath)
	orig := []byte(`Acquire::http::Proxy "http://user:secret@proxy:3128/";` + "\n")

	s := &Snapshot{APT: APT{Conf: []File{{
		Path:        confPath,
		ArchivePath: ap,
		Size:        int64(len(orig)),
		SHA256:      sha256Hex(orig),
	}}}}
	fs := &FileSet{Bytes: map[string][]byte{ap: append([]byte(nil), orig...)}}

	doRedact(s, fs, RedactProxies)

	if !s.APT.Conf[0].Redacted {
		t.Error("File.Redacted not set")
	}
	if strings.Contains(string(fs.Bytes[ap]), "secret") {
		t.Error("secret still present in file bytes")
	}
	if s.APT.Conf[0].SHA256 == sha256Hex(orig) {
		t.Error("digest was not recomputed after redaction")
	}
	if got := sha256Hex(fs.Bytes[ap]); got != s.APT.Conf[0].SHA256 {
		t.Errorf("recorded digest %s does not match the stored bytes' actual digest %s", s.APT.Conf[0].SHA256, got)
	}
	if len(s.Redactions) != 1 || s.Redactions[0] != RedactProxies {
		t.Errorf("Redactions = %v", s.Redactions)
	}
}

func TestDoRedactNilSafe(t *testing.T) {
	doRedact(nil, nil, RedactMachineID) // must not panic
	doRedact(&Snapshot{}, nil, RedactProxies)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
