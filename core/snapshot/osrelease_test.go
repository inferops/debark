package snapshot

import "testing"

func TestParseOSRelease(t *testing.T) {
	data := []byte(`PRETTY_NAME="Debian GNU/Linux 12 (bookworm)"
NAME="Debian GNU/Linux"
VERSION_ID="12"
VERSION_CODENAME=bookworm
ID=debian
# a comment
HOME_URL='https://www.debian.org/'
EMPTY=
`)
	vals := parseOSRelease(data)
	want := map[string]string{
		"PRETTY_NAME":      "Debian GNU/Linux 12 (bookworm)",
		"NAME":             "Debian GNU/Linux",
		"VERSION_ID":       "12",
		"VERSION_CODENAME": "bookworm",
		"ID":               "debian",
		"HOME_URL":         "https://www.debian.org/",
		"EMPTY":            "",
	}
	for k, w := range want {
		if got := vals[k]; got != w {
			t.Errorf("%s = %q, want %q", k, got, w)
		}
	}
	if _, ok := vals["# a comment"]; ok {
		t.Error("comment line was parsed as a field")
	}
}

func TestParseOSReleaseUbuntu(t *testing.T) {
	data := []byte(`PRETTY_NAME="Ubuntu 24.04.3 LTS"
NAME="Ubuntu"
VERSION_ID="24.04"
VERSION_CODENAME=noble
ID=ubuntu
ID_LIKE=debian
`)
	vals := parseOSRelease(data)
	if vals["ID"] != "ubuntu" || vals["VERSION_ID"] != "24.04" || vals["VERSION_CODENAME"] != "noble" {
		t.Errorf("unexpected parse: %+v", vals)
	}
}
