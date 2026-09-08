package policy

import (
	"reflect"
	"testing"
)

func TestParseMiniYAML(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want map[string]any
	}{
		{
			name: "scalars and booleans",
			in: "schema_version: debark.policy/v1\n" +
				"require_signed_publisher: true\n" +
				"allow_url_inputs: false\n" +
				"default_severity: deny\n",
			want: map[string]any{
				"schema_version":           "debark.policy/v1",
				"require_signed_publisher": true,
				"allow_url_inputs":         false,
				"default_severity":         "deny",
			},
		},
		{
			name: "block sequence",
			in: "allow_components:\n" +
				"  - main\n" +
				"  - universe\n" +
				"deny_flags:\n" +
				"  - network-postinst\n",
			want: map[string]any{
				"allow_components": []any{"main", "universe"},
				"deny_flags":       []any{"network-postinst"},
			},
		},
		{
			name: "flow sequence",
			in:   "deny_components: [multiverse, non-free, non-free-firmware]\n",
			want: map[string]any{
				"deny_components": []any{"multiverse", "non-free", "non-free-firmware"},
			},
		},
		{
			name: "comments and blank lines are ignored",
			in: "# a policy file\n" +
				"\n" +
				"require_signed_publisher: true  # inline comment\n" +
				"\n" +
				"# another comment\n" +
				"default_severity: warn\n",
			want: map[string]any{
				"require_signed_publisher": true,
				"default_severity":         "warn",
			},
		},
		{
			name: "quoted strings",
			in: "default_severity: \"warn\"\n" +
				"allow_packages:\n" +
				"  - 'lib*'\n" +
				"  - \"*-dbg\"\n",
			want: map[string]any{
				"default_severity": "warn",
				"allow_packages":   []any{"lib*", "*-dbg"},
			},
		},
		{
			name: "empty value is null",
			in:   "approved_keys:\n",
			want: map[string]any{
				"approved_keys": nil,
			},
		},
		{
			name: "url-shaped values keep their colon",
			in:   "deny_packages:\n  - not-a-url-but-has: colon\n",
			// This exercises that a colon not followed by whitespace/EOL
			// inside a list item scalar is not treated as a new key; the
			// list item is the whole remainder.
			want: map[string]any{
				"deny_packages": []any{"not-a-url-but-has: colon"},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseMiniYAML([]byte(c.in))
			if err != nil {
				t.Fatalf("parseMiniYAML: %v", err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("parseMiniYAML(%q)\n got: %#v\nwant: %#v", c.in, got, c.want)
			}
		})
	}
}

func TestParseMiniYAMLRejectsNesting(t *testing.T) {
	_, err := parseMiniYAML([]byte("outer:\n  inner: value\n"))
	// "inner: value" is indented under "outer" but is not a list item, so
	// collectBlockSequence stops without consuming it and the next top-level
	// pass sees it at indent>0, which is rejected.
	if err == nil {
		t.Fatal("expected an error for nested mapping (unsupported), got nil")
	}
}

func TestStripYAMLComment(t *testing.T) {
	cases := []struct{ in, want string }{
		{"foo: bar # trailing", "foo: bar "},
		{"foo: 'a # b'", "foo: 'a # b'"},
		{`foo: "a # b"`, `foo: "a # b"`},
		{"no comment here", "no comment here"},
		{"#full line comment", ""},
	}
	for _, c := range cases {
		if got := stripYAMLComment(c.in); got != c.want {
			t.Errorf("stripYAMLComment(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
