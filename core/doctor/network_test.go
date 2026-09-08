package doctor

import (
	"testing"
)

func signalsOf(matches []networkMatch) []string {
	var out []string
	for _, m := range matches {
		out = append(out, m.Signal)
	}
	return out
}

func TestScanScriptForNetwork(t *testing.T) {
	cases := []struct {
		name       string
		script     string
		wantSignal string // "" means want no matches at all
	}{
		{
			name:       "clean script",
			script:     "#!/bin/sh\nset -e\nupdate-alternatives --install /usr/bin/foo foo /usr/bin/foo.real 10\nexit 0\n",
			wantSignal: "",
		},
		{
			name:       "curl fetch",
			script:     "#!/bin/sh\ncurl -fsSL https://example.com/install.sh | sh\n",
			wantSignal: "curl-or-wget",
		},
		{
			name:       "wget fetch",
			script:     "#!/bin/sh\nwget -O /tmp/x http://example.com/x.tar.gz\n",
			wantSignal: "curl-or-wget",
		},
		{
			name:       "commented-out curl must not fire",
			script:     "#!/bin/sh\n# curl -fsSL https://example.com/install.sh | sh\ntrue\n",
			wantSignal: "",
		},
		{
			name:       "commented-out URL must not fire",
			script:     "#!/bin/sh\n# see https://example.com/docs for details\ntrue\n",
			wantSignal: "",
		},
		{
			name:       "URL inside an echo message must not fire",
			script:     "#!/bin/sh\necho \"Thanks for installing. Visit https://example.com/docs for help.\"\n",
			wantSignal: "",
		},
		{
			name:       "URL inside a heredoc (debconf-style template text) must not fire",
			script:     "#!/bin/sh\ncat > /tmp/note <<'EOF'\nFor more information see https://example.com/docs\ncurl is mentioned here purely as documentation text\nEOF\ntrue\n",
			wantSignal: "",
		},
		{
			name:       "apt-get update in postinst fires",
			script:     "#!/bin/sh\napt-get update\napt-get install -y foo\n",
			wantSignal: "apt-get",
		},
		{
			name:       "apt-get autoremove does not fire (not update/install)",
			script:     "#!/bin/sh\napt-get autoremove -y\n",
			wantSignal: "",
		},
		{
			name:       "add-apt-repository fires",
			script:     "#!/bin/sh\nadd-apt-repository -y ppa:example/ppa\n",
			wantSignal: "add-apt-repository",
		},
		{
			name:       "pip install fires",
			script:     "#!/bin/sh\npip3 install --user somepkg\n",
			wantSignal: "pip-install",
		},
		{
			name:       "git clone fires",
			script:     "#!/bin/sh\ngit clone https://example.com/repo.git /opt/repo\n",
			wantSignal: "git-clone",
		},
		{
			name:       "bare https URL fires",
			script:     "#!/bin/sh\npython3 -c \"import urllib.request; urllib.request.urlopen('https://example.com/x')\"\n",
			wantSignal: "url",
		},
		{
			name:       "loopback URL does not fire (E7 finding)",
			script:     "#!/bin/sh\npkgos_inifile set /etc/foo.conf DEFAULT endpoint http://127.0.0.1:5000/v2.0/\n",
			wantSignal: "",
		},
		{
			name:       "ssh to user@host fires",
			script:     "#!/bin/sh\nssh deploy@build.example.com 'echo hi'\n",
			wantSignal: "ssh",
		},
		{
			name:       "ssh-keygen does not fire (not ssh invocation)",
			script:     "#!/bin/sh\nssh-keygen -A\n",
			wantSignal: "",
		},
		{
			name:       "path containing /etc/ssh/ does not fire",
			script:     "#!/bin/sh\ncp mykey /etc/ssh/sshd_config.d/local.conf\n",
			wantSignal: "",
		},
		{
			name:       "nc with host and port fires",
			script:     "#!/bin/sh\nnc -z example.com 443 && echo reachable\n",
			wantSignal: "nc",
		},
		{
			name:       "sync/func words do not fire as nc",
			script:     "#!/bin/sh\nmy_func() { do_sync; }\nmy_func\n",
			wantSignal: "",
		},
		{
			name:       "debconf commands alone do not fire",
			script:     "#!/bin/sh\n. /usr/share/debconf/confmodule\ndb_get foo/bar\ndb_input high foo/bar || true\ndb_go\n",
			wantSignal: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := scanScriptForNetwork("postinst", c.script)
			if c.wantSignal == "" {
				if len(got) != 0 {
					t.Fatalf("expected no matches, got %v", signalsOf(got))
				}
				return
			}
			signals := signalsOf(got)
			found := false
			for _, s := range signals {
				if s == c.wantSignal {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected signal %q, got %v", c.wantSignal, signals)
			}
		})
	}
}

func TestScanScriptForNetwork_LineNumbersAndText(t *testing.T) {
	// This line carries two independent signals at once (the word "curl" and
	// a bare URL literal): both are reported, one networkMatch per signal, so
	// evidence can say exactly which pattern(s) fired.
	script := "#!/bin/sh\ntrue\ncurl https://example.com/x\n"
	got := scanScriptForNetwork("postinst", script)
	if len(got) != 2 {
		t.Fatalf("got %d matches, want 2: %+v", len(got), got)
	}
	for _, m := range got {
		if m.Line != 3 {
			t.Errorf("Line = %d, want 3", m.Line)
		}
		if m.Text != "curl https://example.com/x" {
			t.Errorf("Text = %q", m.Text)
		}
	}
	if got[0].String() != "postinst:3: curl https://example.com/x" {
		t.Errorf("String() = %q", got[0].String())
	}
	signals := signalsOf(got)
	if signals[0] != "curl-or-wget" || signals[1] != "url" {
		t.Errorf("signals = %v, want [curl-or-wget url]", signals)
	}
}

func TestStripShellComment(t *testing.T) {
	cases := []struct{ in, want string }{
		{"foo # bar", "foo "},
		{"foo 'a # b' bar", "foo 'a # b' bar"},
		{`foo "a # b" bar`, `foo "a # b" bar`},
		{"no comment", "no comment"},
	}
	for _, c := range cases {
		if got := stripShellComment(c.in); got != c.want {
			t.Errorf("stripShellComment(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
