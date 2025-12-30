package partitionresizer

import (
	"os"
	"path/filepath"
	"reflect"
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

// TestFilterDisksByPartitions exercises matching by name, label, uuid.
func TestFilterDisksByPartitions(t *testing.T) {
	m := map[string][]partitionData{
		"d1": {{name: "p1", label: "L1", uuid: "U1"}},
		"d2": {{name: "p2", label: "L2", uuid: "U2"}},
	}
	id := NewPartitionIdentifier(IdentifierByLabel, "L1")
	got, err := filterDisksByPartitions(m, []PartitionIdentifier{id})
	if err != nil {
		t.Fatalf("filterDisksByPartitions error: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"d1"}) {
		t.Errorf("filterDisksByPartitions = %v, want [d1]", got)
	}
}

// TestFindDisks_Simple mocks a minimal sysfs tree and verifies findDisks.
func TestFindDisks_Simple(t *testing.T) {
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
	// start and size in bytes (blockSize=512)
	if pd.start != 2*512 || pd.size != 4*512 {
		t.Errorf("(start,size) = (%d,%d), want (%d,%d)",
			pd.start, pd.size, 2*512, 4*512)
	}
	// restrict to explicit disk
	single, err := findDisks("sdx", tmp)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := single["sdx"]; !ok {
		t.Errorf("findDisks(disk,…) failed to restrict to sdx: %v", single)
	}
}
