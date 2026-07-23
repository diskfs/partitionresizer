package main

import (
	"testing"

	resizer "github.com/diskfs/partitionresizer"
)

// Valid partition identifier formats
func TestParsePartitionIdentifier_Valid(t *testing.T) {
	tests := []struct {
		input string
		by    resizer.Identifier
		val   string
	}{
		{"name:sda1", resizer.IdentifierByName, "sda1"},
		{"label:EFI System", resizer.IdentifierByLabel, "EFI System"},
		{"uuid:AD6871EE-31F9-4CF3-9E09-6F7A25C30056", resizer.IdentifierByUUID, "AD6871EE-31F9-4CF3-9E09-6F7A25C30056"},
	}
	for _, tt := range tests {
		pi, err := parsePartitionIdentifier(tt.input)
		if err != nil {
			t.Errorf("parsePartitionIdentifier(%q) error: %v", tt.input, err)
			continue
		}
		if pi.By() != tt.by || pi.Value() != tt.val {
			t.Errorf("parsePartitionIdentifier(%q) = (%v, %q), want (%v, %q)",
				tt.input, pi.By(), pi.Value(), tt.by, tt.val)
		}
	}
}

// Invalid inputs for partition identifier
func TestParsePartitionIdentifier_Invalid(t *testing.T) {
	inputs := []string{
		"no-delimiter",
		"bogus:value", // unknown identifier type
	}
	for _, input := range inputs {
		if _, err := parsePartitionIdentifier(input); err == nil {
			t.Errorf("parsePartitionIdentifier(%q) expected error, got nil", input)
		}
	}
}

// Valid size parsing
func TestParseSize_Valid(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{"100", 100},
		{"1B", 1},
		{"2k", 2 * 1024},
		{"3M", 3 * 1024 * 1024},
		{"4G", 4 * 1024 * 1024 * 1024},
		{"5T", 5 * 1024 * 1024 * 1024 * 1024},
	}
	for _, tt := range tests {
		got, err := parseSize(tt.input)
		if err != nil {
			t.Errorf("parseSize(%q) error: %v", tt.input, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseSize(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

// Invalid size strings
func TestParseSize_Invalid(t *testing.T) {
	inputs := []string{"XYZ", "12X", "--5M"}
	for _, input := range inputs {
		if _, err := parseSize(input); err == nil {
			t.Errorf("parseSize(%q) expected error, got nil", input)
		}
	}
}

// A create/GUID spec and a match (grow) spec both parse, and their fields land
// where expected.
func TestParsePartitionSpec_Valid(t *testing.T) {
	t.Run("guid create", func(t *testing.T) {
		spec, err := parsePartitionSpec("guid=AD..56,minsize=2G,label=EFI System,type=C12A,index=7,fs=fat32")
		if err != nil {
			t.Fatalf("parsePartitionSpec error: %v", err)
		}
		if spec.GUID != "AD..56" || spec.MinSize != 2*1024*1024*1024 {
			t.Errorf("guid/minsize = (%q,%d)", spec.GUID, spec.MinSize)
		}
		if spec.Label != "EFI System" || spec.TypeGUID != "C12A" || spec.Index != 7 || spec.FS != resizer.FSFAT32 {
			t.Errorf("unexpected spec %+v", spec)
		}
		if spec.Match != nil {
			t.Errorf("Match should be nil for a guid spec, got %+v", spec.Match)
		}
	})
	t.Run("match grow", func(t *testing.T) {
		spec, err := parsePartitionSpec("match=label:Data,minsize=100G")
		if err != nil {
			t.Fatalf("parsePartitionSpec error: %v", err)
		}
		if spec.Match == nil || spec.Match.By() != resizer.IdentifierByLabel || spec.Match.Value() != "Data" {
			t.Errorf("unexpected Match %+v", spec.Match)
		}
		if spec.MinSize != 100*1024*1024*1024 {
			t.Errorf("minsize = %d", spec.MinSize)
		}
	})
}

// Missing required fields and unknown keys are rejected.
func TestParsePartitionSpec_Invalid(t *testing.T) {
	inputs := []string{
		"guid=AD..56",                   // no minsize
		"minsize=2G",                    // neither guid nor match
		"match=label:X,size=2G",         // unknown key size (want minsize)
		"guid=AD..56,minsize=2G,fs=zfs", // unknown fs
		"noequals",                      // not key=value
	}
	for _, in := range inputs {
		if _, err := parsePartitionSpec(in); err == nil {
			t.Errorf("parsePartitionSpec(%q) expected error, got nil", in)
		}
	}
}

// Shrink parses with an explicit size and, without one, requests shrink-to-fit
// (Size 0).
func TestParseShrinkSpec(t *testing.T) {
	t.Run("explicit size", func(t *testing.T) {
		s, err := parseShrinkSpec("label:P3:200M")
		if err != nil {
			t.Fatalf("parseShrinkSpec error: %v", err)
		}
		if s.ID.By() != resizer.IdentifierByLabel || s.ID.Value() != "P3" || s.Size != 200*1024*1024 {
			t.Errorf("unexpected shrink %+v (size %d)", s.ID, s.Size)
		}
	})
	t.Run("to-fit", func(t *testing.T) {
		s, err := parseShrinkSpec("uuid:AD..59")
		if err != nil {
			t.Fatalf("parseShrinkSpec error: %v", err)
		}
		if s.ID.By() != resizer.IdentifierByUUID || s.Size != 0 {
			t.Errorf("want to-fit (size 0), got %+v (size %d)", s.ID, s.Size)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		for _, in := range []string{"onlyone", "label:P3:XYZ"} {
			if _, err := parseShrinkSpec(in); err == nil {
				t.Errorf("parseShrinkSpec(%q) expected error, got nil", in)
			}
		}
	})
}
