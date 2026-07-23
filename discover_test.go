package partitionresizer

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestParseKeyValueLines verifies parsing of KEY=VALUE lines.
func TestParseKeyValueLines(t *testing.T) {
	data := []byte("A=1\nB=two\nINVALID\nC=3\r\n")
	got := parseKeyValueLines(data)
	want := map[string]string{"A": "1", "B": "two", "C": "3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseKeyValueLines = %v, want %v", got, want)
	}
}

// TestReadSysIntValue trims CR/LF and parses integers.
func TestReadSysIntValue(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "val")
	if err := os.WriteFile(p, []byte("123\n"), 0644); err != nil {
		t.Fatal(err)
	}
	v, err := readSysIntValue(p)
	if err != nil || v != 123 {
		t.Fatalf("readSysIntValue(123\n) = (%d,%v), want (123,nil)", v, err)
	}
	if err := os.WriteFile(p, []byte("456\r"), 0644); err != nil {
		t.Fatal(err)
	}
	v, err = readSysIntValue(p)
	if err != nil || v != 456 {
		t.Fatalf("readSysIntValue(456\r) = (%d,%v), want (456,nil)", v, err)
	}
}

// TestFilterDisks exercises matching by name, label, uuid.
func TestFilterDisks(t *testing.T) {
	m := map[string][]partitionData{
		"d1": {{name: "p1", label: "L1", uuid: "U1"}},
		"d2": {{name: "p2", label: "L2", uuid: "U2"}},
	}
	t.Run("ByLabel", func(t *testing.T) {
		id := NewPartitionIdentifier(IdentifierByLabel, "L1")
		got, err := filterDisksByPartitions(m, []PartitionIdentifier{id})
		if err != nil {
			t.Fatalf("filterDisksByPartitions error: %v", err)
		}
		if !reflect.DeepEqual(got, []string{"d1"}) {
			t.Errorf("filterDisksByPartitions = %v, want [d1]", got)
		}
	})
	t.Run("ByName", func(t *testing.T) {
		id := NewPartitionIdentifier(IdentifierByName, "p2")
		got, err := filterDisksByPartitions(m, []PartitionIdentifier{id})
		if err != nil {
			t.Fatalf("filterDisksByPartitions error: %v", err)
		}
		if !reflect.DeepEqual(got, []string{"d2"}) {
			t.Errorf("filterDisksByPartitions = %v, want [d2]", got)
		}
	})
	t.Run("ByUUID", func(t *testing.T) {
		id := NewPartitionIdentifier(IdentifierByUUID, "U1")
		got, err := filterDisksByPartitions(m, []PartitionIdentifier{id})
		if err != nil {
			t.Fatalf("filterDisksByPartitions error: %v", err)
		}
		if !reflect.DeepEqual(got, []string{"d1"}) {
			t.Errorf("filterDisksByPartitions = %v, want [d1]", got)
		}
	})
	t.Run("No match", func(t *testing.T) {
		id := NewPartitionIdentifier(IdentifierByLabel, "NOPE")
		got, err := filterDisksByPartitions(m, []PartitionIdentifier{id})
		if err != nil {
			t.Fatalf("filterDisksByPartitions error: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("filterDisksByPartitions = %v, want []", got)
		}
	})
}

// TestFindDisks verifies findDisks.
func TestFindDisks(t *testing.T) {
	tmp := t.TempDir()
	sys := filepath.Join(tmp, "class", "block")
	// create disk directory and queue/logical_block_size
	diskDir := filepath.Join(sys, "sdx")
	if err := os.MkdirAll(filepath.Join(diskDir, "queue"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(diskDir, "queue", "logical_block_size"), []byte("512\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// create one partition: sdx1
	part := filepath.Join(diskDir, "sdx1")
	if err := os.Mkdir(part, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(part, "partition"), []byte("1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(part, "start"), []byte("2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(part, "size"), []byte("4\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(part, "uevent"), []byte("PARTNAME=foo\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Run("all", func(t *testing.T) {
		disks, err := findDisks("", tmp)
		if err != nil {
			t.Fatalf("findDisks error: %v", err)
		}
		data, ok := disks["sdx"]
		if !ok || len(data) != 1 {
			t.Fatalf("unexpected disks map: %v", disks)
		}
		pd := data[0]
		if pd.name != "sdx1" {
			t.Errorf("pd.name = %q, want sdx1", pd.name)
		}
		if pd.label != "foo" {
			t.Errorf("pd.label = %q, want foo", pd.label)
		}
		// start, size, and end in bytes (blockSize=512). End is the
		// inclusive last byte of the partition, i.e.
		// (start_sector + size_sectors - 1) * blockSize.
		if pd.start != 2*512 || pd.size != 4*512 {
			t.Errorf("(start,size) = (%d,%d), want (%d,%d)",
				pd.start, pd.size, 2*512, 4*512)
		}
		expectedEnd := int64((2 + 4 - 1) * 512)
		if pd.end != expectedEnd {
			t.Errorf("pd.end = %d, want %d", pd.end, expectedEnd)
		}
	})
	t.Run("single", func(t *testing.T) {
		// restrict to explicit disk
		single, err := findDisks("sdx", tmp)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := single["sdx"]; !ok {
			t.Errorf("findDisks(disk,…) failed to restrict to sdx: %v", single)
		}
	})
	t.Run("none", func(t *testing.T) {
		_, err := findDisks("nosuchdisk", tmp)
		if err == nil || !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("expected fs.ErrNotExist for missing disk, got: %v", err)
		}
	})
	t.Run("disk image", func(t *testing.T) {
		// no need to copy, since we are only reading the disk image
		disks, err := findDisks(diskfullImg, tmp)
		if err != nil {
			t.Fatalf("findDisks error: %v", err)
		}
		if len(disks) != 1 {
			t.Fatalf("expected 1 disk from disk image, got %d", len(disks))
		}
		data, ok := disks[path.Base(diskfullImg)]
		if !ok || len(data) != 4 {
			t.Fatalf("unexpected disks map from disk image: %v", disks)
		}
	})
}

// TestFindDisksPopulatesPartUUID verifies the sysfs (block-device) discovery
// path fills in the partition GUID from the uevent PARTUUID key, so a caller can
// select a partition by IdentifierByUUID on a real device. Without it, uuid is
// empty and filterDisksByPartitions can never match a UUID identifier — a gap
// invisible to the image-file path, which already sets uuid from go-diskfs.
//
// The GUID is normalized to upper case to match go-diskfs's UUID(), which
// convert.go compares against when resolving the same identifier on the GPT; a
// lower-case value from the uevent would silently fail that later comparison.
func TestFindDisksPopulatesPartUUID(t *testing.T) {
	const (
		partUUIDLower = "a94e71c5-a294-4512-8974-b2d52289ea18"
		wantLabel     = "EFI System"
	)
	tmp := t.TempDir()
	diskDir := filepath.Join(tmp, "class", "block", "sdy")
	if err := os.MkdirAll(filepath.Join(diskDir, "queue"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(diskDir, "queue", "logical_block_size"), []byte("512\n"), 0644); err != nil {
		t.Fatal(err)
	}
	part := filepath.Join(diskDir, "sdy1")
	if err := os.Mkdir(part, 0755); err != nil {
		t.Fatal(err)
	}
	for name, val := range map[string]string{"partition": "1\n", "start": "2\n", "size": "4\n"} {
		if err := os.WriteFile(filepath.Join(part, name), []byte(val), 0644); err != nil {
			t.Fatal(err)
		}
	}
	uevent := "PARTNAME=" + wantLabel + "\nPARTUUID=" + partUUIDLower + "\n"
	if err := os.WriteFile(filepath.Join(part, "uevent"), []byte(uevent), 0644); err != nil {
		t.Fatal(err)
	}

	disks, err := findDisks("", tmp)
	if err != nil {
		t.Fatalf("findDisks error: %v", err)
	}
	data, ok := disks["sdy"]
	if !ok || len(data) != 1 {
		t.Fatalf("unexpected disks map: %v", disks)
	}
	pd := data[0]
	if pd.label != wantLabel {
		t.Errorf("pd.label = %q, want %q", pd.label, wantLabel)
	}
	want := strings.ToUpper(partUUIDLower)
	if pd.uuid != want {
		t.Errorf("pd.uuid = %q, want %q", pd.uuid, want)
	}
}
