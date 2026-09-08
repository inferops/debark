package snapshot

import (
	"reflect"
	"testing"
)

func TestSummariseNil(t *testing.T) {
	if got := Summarise(nil); !reflect.DeepEqual(got, Summary{}) {
		t.Errorf("Summarise(nil) = %+v, want zero value", got)
	}
}

func TestSummariseFieldsFromCapture(t *testing.T) {
	s, _ := mustCapture(t, CaptureOptions{Root: "testdata/ubuntu2404-deb822", IncludeKeyrings: true})
	sum := Summarise(s)

	if sum.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %q", sum.SchemaVersion)
	}
	if sum.DistroID != "ubuntu" || sum.VersionID != "24.04" || sum.Codename != "noble" {
		t.Errorf("identity = %+v", sum)
	}
	if sum.InstalledCount != s.InstalledCount {
		t.Errorf("installed_count = %d, want %d", sum.InstalledCount, s.InstalledCount)
	}
	if !sum.HasMachineID {
		t.Error("has_machine_id should be true: the fixture carries one")
	}
	if sum.PhasedPolicy != PhasedTargetMachineID {
		t.Errorf("phased_policy = %q, want %q", sum.PhasedPolicy, PhasedTargetMachineID)
	}
	if len(sum.Sources) != len(s.APT.Sources) {
		t.Errorf("sources = %v", sum.Sources)
	}
	if len(sum.KeyFingerprints) != len(s.KeyringFingerprints) {
		t.Errorf("key_fingerprints count = %d, want %d", len(sum.KeyFingerprints), len(s.KeyringFingerprints))
	}
	if len(sum.Keyrings) != len(s.APT.Keyrings) {
		t.Errorf("keyrings = %v", sum.Keyrings)
	}
}

func TestSummariseRedactedSnapshotHasNoMachineID(t *testing.T) {
	s, _ := mustCapture(t, CaptureOptions{Root: "testdata/debian12-list", Redact: true})
	sum := Summarise(s)
	if sum.HasMachineID {
		t.Error("has_machine_id must be false once the machine id has been redacted")
	}
	if sum.PhasedPolicy != PhasedNeverInclude {
		t.Errorf("phased_policy = %q, want %q", sum.PhasedPolicy, PhasedNeverInclude)
	}
	if len(sum.Redactions) == 0 {
		t.Error("redactions should be reported")
	}
}

func TestSummariseToolString(t *testing.T) {
	s := &Snapshot{Tool: Tool{Name: "debark", Version: "1.2.3", Edition: "community"}}
	sum := Summarise(s)
	if sum.Tool != "debark/1.2.3 (community)" {
		t.Errorf("tool = %q", sum.Tool)
	}
}
