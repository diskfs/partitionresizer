package partitionresizer

import (
	"os"
	"path/filepath"
	"testing"
)

// buildFakeSysfs writes a minimal sysfs tree with one disk (disk) holding one
// partition (part) that carries the given label and PARTUUID, and returns the
// sysfs root. It mirrors what findDisks reads for a block device.
func buildFakeSysfs(t *testing.T, disk, part, label, partUUID string) string {
	t.Helper()
	root := t.TempDir()
	diskDir := filepath.Join(root, "class", "block", disk)
	if err := os.MkdirAll(filepath.Join(diskDir, "queue"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(diskDir, "queue", "logical_block_size"), []byte("512\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pd := filepath.Join(diskDir, part)
	if err := os.Mkdir(pd, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, val := range map[string]string{"partition": "1\n", "start": "2\n", "size": "4\n"} {
		if err := os.WriteFile(filepath.Join(pd, name), []byte(val), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	uevent := "PARTNAME=" + label + "\nPARTUUID=" + partUUID + "\n"
	if err := os.WriteFile(filepath.Join(pd, "uevent"), []byte(uevent), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestResolveDiskAndNames exercises the full front-end of Apply against a fake
// sysfs: auto-discovering the disk from an identifier, resolving a by-name
// identifier to the partition UUID, and doing both together.
func TestResolveDiskAndNames(t *testing.T) {
	const (
		label     = "EFI System"
		uuidLower = "a94e71c5-a294-4512-8974-b2d52289ea18"
		uuidUpper = "A94E71C5-A294-4512-8974-B2D52289EA18"
	)

	t.Run("auto-discover disk from label", func(t *testing.T) {
		sys := buildFakeSysfs(t, "sdx", "sdx1", label, uuidLower)
		desired := []PartitionSpec{{Match: NewPartitionIdentifier(IdentifierByLabel, label), MinSize: 1}}
		disk, _, _, err := resolveDiskAndNames("", desired, nil, sys)
		if err != nil {
			t.Fatalf("resolveDiskAndNames: %v", err)
		}
		if disk != "/dev/sdx" {
			t.Errorf("discovered disk = %q, want /dev/sdx", disk)
		}
	})

	t.Run("resolve by-name grow to uuid", func(t *testing.T) {
		sys := buildFakeSysfs(t, "sdx", "sdx1", label, uuidLower)
		desired := []PartitionSpec{{Match: NewPartitionIdentifier(IdentifierByName, "sdx1"), MinSize: 1}}
		disk, outDesired, _, err := resolveDiskAndNames("/dev/sdx", desired, nil, sys)
		if err != nil {
			t.Fatalf("resolveDiskAndNames: %v", err)
		}
		if disk != "/dev/sdx" {
			t.Errorf("disk = %q, want /dev/sdx", disk)
		}
		if outDesired[0].Match.By() != IdentifierByUUID || outDesired[0].Match.Value() != uuidUpper {
			t.Errorf("by-name Match not resolved: %v:%q, want uuid:%s", outDesired[0].Match.By(), outDesired[0].Match.Value(), uuidUpper)
		}
	})

	t.Run("discover and resolve a by-name shrink", func(t *testing.T) {
		sys := buildFakeSysfs(t, "sdx", "sdx1", label, uuidLower)
		shrink := &ShrinkSpec{ID: NewPartitionIdentifier(IdentifierByName, "sdx1"), Size: 5}
		disk, _, outShrink, err := resolveDiskAndNames("", nil, shrink, sys)
		if err != nil {
			t.Fatalf("resolveDiskAndNames: %v", err)
		}
		if disk != "/dev/sdx" {
			t.Errorf("discovered disk = %q, want /dev/sdx", disk)
		}
		if outShrink.ID.By() != IdentifierByUUID || outShrink.ID.Value() != uuidUpper {
			t.Errorf("by-name shrink not resolved: %v:%q", outShrink.ID.By(), outShrink.ID.Value())
		}
	})

	t.Run("no disk and no identifiers is an error", func(t *testing.T) {
		if _, _, _, err := resolveDiskAndNames("", nil, nil, t.TempDir()); err == nil {
			t.Error("expected error when no disk and no identifiers, got nil")
		}
	})
}

// TestMustExistIdentifiers returns Match (grow) and shrink identifiers but not a
// create's GUID, which does not exist yet and so cannot select a disk.
func TestMustExistIdentifiers(t *testing.T) {
	desired := []PartitionSpec{
		{Match: NewPartitionIdentifier(IdentifierByLabel, "Data"), MinSize: 1 * GiB},
		{GUID: espBGUID, MinSize: 2 * GiB}, // create by GUID: excluded
	}
	shrink := &ShrinkSpec{ID: NewPartitionIdentifier(IdentifierByName, "sda3")}
	got := mustExistIdentifiers(desired, shrink)
	if len(got) != 2 {
		t.Fatalf("got %d identifiers, want 2 (the Match and the shrink): %+v", len(got), got)
	}
	if got[0].By() != IdentifierByLabel || got[0].Value() != "Data" {
		t.Errorf("first identifier = %v:%q, want label:Data", got[0].By(), got[0].Value())
	}
	if got[1].By() != IdentifierByName || got[1].Value() != "sda3" {
		t.Errorf("second identifier = %v:%q, want name:sda3", got[1].By(), got[1].Value())
	}
}

func TestAnyNameIdentifier(t *testing.T) {
	byLabel := []PartitionSpec{{Match: NewPartitionIdentifier(IdentifierByLabel, "Data"), MinSize: 1}}
	byName := []PartitionSpec{{Match: NewPartitionIdentifier(IdentifierByName, "sda1"), MinSize: 1}}
	if anyNameIdentifier(byLabel, nil) {
		t.Error("label-only specs should not report a name identifier")
	}
	if !anyNameIdentifier(byName, nil) {
		t.Error("a by-name Match should report a name identifier")
	}
	if !anyNameIdentifier(nil, &ShrinkSpec{ID: NewPartitionIdentifier(IdentifierByName, "sda3")}) {
		t.Error("a by-name shrink should report a name identifier")
	}
}

// TestResolveNameIdentifiers rewrites by-name Match/shrink identifiers to the
// partition's uuid using the discovered data, leaving label/uuid identifiers
// untouched, and errors on a name with no match.
func TestResolveNameIdentifiers(t *testing.T) {
	parts := []partitionData{
		{name: "sda1", uuid: "UUID-1"},
		{name: "sda3", uuid: "UUID-3"},
	}
	desired := []PartitionSpec{
		{Match: NewPartitionIdentifier(IdentifierByName, "sda1"), MinSize: 1},
		{Match: NewPartitionIdentifier(IdentifierByLabel, "Data"), MinSize: 1}, // left as-is
	}
	shrink := &ShrinkSpec{ID: NewPartitionIdentifier(IdentifierByName, "sda3"), Size: 5}

	outDesired, outShrink, err := resolveNameIdentifiers(parts, desired, shrink)
	if err != nil {
		t.Fatalf("resolveNameIdentifiers: %v", err)
	}
	if outDesired[0].Match.By() != IdentifierByUUID || outDesired[0].Match.Value() != "UUID-1" {
		t.Errorf("sda1 not resolved to uuid: %v:%q", outDesired[0].Match.By(), outDesired[0].Match.Value())
	}
	if outDesired[1].Match.By() != IdentifierByLabel {
		t.Errorf("label Match should be untouched, got %v", outDesired[1].Match.By())
	}
	if outShrink.ID.By() != IdentifierByUUID || outShrink.ID.Value() != "UUID-3" || outShrink.Size != 5 {
		t.Errorf("shrink not resolved correctly: %v:%q size %d", outShrink.ID.By(), outShrink.ID.Value(), outShrink.Size)
	}
	// caller's specs must be untouched (copies returned)
	if desired[0].Match.By() != IdentifierByName {
		t.Error("resolveNameIdentifiers mutated the caller's desired slice")
	}

	// a name absent from the discovered data is an error
	if _, _, err := resolveNameIdentifiers(parts, []PartitionSpec{{Match: NewPartitionIdentifier(IdentifierByName, "nope"), MinSize: 1}}, nil); err == nil {
		t.Error("expected error for an unknown partition name, got nil")
	}
}
