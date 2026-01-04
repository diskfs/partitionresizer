package main

import (
	"reflect"
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
		"uuid:1234",
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

// Valid partition change formats
func TestParsePartitionChange_Valid(t *testing.T) {
	input := "name:sda1:20M"
	pc, err := parsePartitionChange(input)
	if err != nil {
		t.Fatalf("parsePartitionChange(%q) error: %v", input, err)
	}
	if pc.By() != resizer.IdentifierByName || pc.Value() != "sda1" {
		t.Errorf("parsePartitionChange(%q) identifier = (%v,%q), want (name,sda1)", input, pc.By(), pc.Value())
	}
	if got := pc.Size(); got != 20*1024*1024 {
		t.Errorf("parsePartitionChange(%q) size = %d, want %d", input, got, 20*1024*1024)
	}
}

// Invalid partition change formats
func TestParsePartitionChange_Invalid(t *testing.T) {
	inputs := []string{"badformat", "name:sda1", "name:sda1:XYZ"}
	for _, input := range inputs {
		if _, err := parsePartitionChange(input); err == nil {
			t.Errorf("parsePartitionChange(%q) expected error, got nil", input)
		}
	}
}

// Round-trip of multiple grow-partition values via Split
func TestGrowPartitionSlice(t *testing.T) {
	// ensure SliceVar unmarshals without panic
	cmd := rootCmd()
	if err := cmd.ParseFlags([]string{"--grow-partition=label:X:1G", "--grow-partition=name:Y:2G"}); err != nil {
		t.Fatalf("ParseFlags error: %v", err)
	}
	s, err := cmd.Flags().GetStringSlice("grow-partition")
	if err != nil {
		t.Fatalf("GetStringSlice error: %v", err)
	}
	if !reflect.DeepEqual(s, []string{"label:X:1G", "name:Y:2G"}) {
		t.Errorf("parsed grow-partition flags = %v, want %v", s, []string{"label:X:1G", "name:Y:2G"})
	}
}
