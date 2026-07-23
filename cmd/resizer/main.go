package main

import (
	"fmt"
	"log"
	"strconv"
	"strings"

	resizer "github.com/diskfs/partitionresizer"
	"github.com/spf13/cobra"
)

// rootCmd is the single resizer command: it declaratively reconciles a disk to a
// desired set of partitions, growing or creating each to at least its size and
// optionally shrinking one to free space.
func rootCmd() *cobra.Command {
	var (
		partitions []string
		shrinkStr  string
		fixErrors  bool
		dryRun     bool
	)
	cmd := &cobra.Command{
		Use:   "resizer [disk]",
		Short: "Declaratively reconcile an OS disk to a desired set of partitions",
		Long: `Reconcile a disk to a desired set of partitions.

Each --partition is grown to at least its size, or created if absent. An
optional --shrink reduces one partition to free space: to an explicit size, or
to-fit (only as much as the grows/creates need) when no size is given. A
partition already at least its size is left untouched; nothing is ever shrunk
except the --shrink partition.

The disk may be given as a positional argument (image file or block device); if
omitted, it is discovered from the match/shrink identifiers. Sizes accept B, K,
M, G, T suffixes (default bytes).

Example usage:
  # grow by GUID, create ESP-B, shrink persist to a fixed size
  resizer --partition guid=AD..51,minsize=2G,label=EFI System,type=C12A..,index=1 \
          --partition guid=AD..56,minsize=2G,label=EFI System,type=C12A..,index=7,fs=fat32 \
          --shrink label:persist:78G /dev/sda

  # grow by label, shrink-to-fit (size omitted)
  resizer --partition match=label:Data,minsize=100G --shrink label:P3 /dev/sda`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			var desired []resizer.PartitionSpec
			for _, p := range partitions {
				spec, err := parsePartitionSpec(p)
				if err != nil {
					return fmt.Errorf("invalid --partition %q: %w", p, err)
				}
				desired = append(desired, spec)
			}
			var shrink *resizer.ShrinkSpec
			if shrinkStr != "" {
				s, err := parseShrinkSpec(shrinkStr)
				if err != nil {
					return fmt.Errorf("invalid --shrink %q: %w", shrinkStr, err)
				}
				shrink = s
			}
			var disk string
			if len(args) > 0 {
				disk = args[0]
			}
			return resizer.Apply(disk, desired, shrink, fixErrors, dryRun)
		},
	}
	cmd.Flags().StringArrayVar(&partitions, "partition", nil, "desired partition, comma-separated keys: guid= or match=<id>, plus minsize=,label=,type=,index=,fs=fat32|ext4|none (repeatable)")
	cmd.Flags().StringVar(&shrinkStr, "shrink", "", "partition to shrink, identifier:value[:size] (e.g. label:P3:200M, or label:P3 for shrink-to-fit)")
	cmd.Flags().BoolVar(&fixErrors, "fix-errors", false, "repair source filesystem errors (ext4 via e2fsck -y, FAT32 via fsck.fat -a) instead of aborting on an inconsistent source")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "plan only; do not modify the disk")
	return cmd
}

// parsePartitionSpec parses a comma-separated key=value PartitionSpec. An
// existing partition to grow is identified either by guid= or by
// match=<identifier>:<value> (name/label/uuid); a create is expressed by guid=,
// which becomes the created partition's identity. E.g.
// "guid=AD..56,minsize=2G,label=EFI System,type=C12A..,index=7,fs=fat32" or
// "match=label:Data,minsize=100G".
func parsePartitionSpec(s string) (resizer.PartitionSpec, error) {
	var spec resizer.PartitionSpec
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return spec, fmt.Errorf("expected key=value, got %q", kv)
		}
		switch k {
		case "guid":
			spec.GUID = v
		case "match":
			id, err := parsePartitionIdentifier(v)
			if err != nil {
				return spec, fmt.Errorf("bad match %q: %w", v, err)
			}
			spec.Match = id
		case "label":
			spec.Label = v
		case "type":
			spec.TypeGUID = v
		case "index":
			n, err := strconv.Atoi(v)
			if err != nil {
				return spec, fmt.Errorf("bad index %q: %v", v, err)
			}
			spec.Index = n
		case "minsize":
			sz, err := parseSize(v)
			if err != nil {
				return spec, fmt.Errorf("bad minsize %q: %v", v, err)
			}
			spec.MinSize = sz
		case "fs":
			switch v {
			case "fat32":
				spec.FS = resizer.FSFAT32
			case "ext4":
				spec.FS = resizer.FSExt4
			case "none", "":
				spec.FS = resizer.FSNone
			default:
				return spec, fmt.Errorf("unknown fs %q", v)
			}
		default:
			return spec, fmt.Errorf("unknown key %q", k)
		}
	}
	if spec.MinSize == 0 {
		return spec, fmt.Errorf("minsize= is required")
	}
	if spec.GUID == "" && spec.Match == nil {
		return spec, fmt.Errorf("either guid= (to create or match by GUID) or match= is required")
	}
	return spec, nil
}

// parseShrinkSpec parses "identifier:value[:size]" into a ShrinkSpec. With no
// size (identifier:value) it requests shrink-to-fit (Size 0).
func parseShrinkSpec(s string) (*resizer.ShrinkSpec, error) {
	parts := strings.SplitN(s, ":", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("want identifier:value[:size]")
	}
	id, err := parsePartitionIdentifier(strings.Join(parts[0:2], ":"))
	if err != nil {
		return nil, err
	}
	spec := &resizer.ShrinkSpec{ID: id}
	if len(parts) == 3 {
		sz, err := parseSize(parts[2])
		if err != nil {
			return nil, fmt.Errorf("bad size %q: %v", parts[2], err)
		}
		spec.Size = sz
	}
	return spec, nil
}

func parsePartitionIdentifier(s string) (resizer.PartitionIdentifier, error) {
	var by resizer.Identifier
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid partition identifier format: %s", s)
	}
	switch parts[0] {
	case string(resizer.IdentifierByName):
		by = resizer.IdentifierByName
	case string(resizer.IdentifierByLabel):
		by = resizer.IdentifierByLabel
	case string(resizer.IdentifierByUUID):
		by = resizer.IdentifierByUUID
	default:
		return nil, fmt.Errorf("unknown identifier type: %s", parts[0])
	}
	return resizer.NewPartitionIdentifier(by, parts[1]), nil
}

func parseSize(s string) (int64, error) {
	var multiplier int64 = 1
	unit := s[len(s)-1]
	numberPart := s
	switch unit {
	case 'B', 'b':
		multiplier = 1
		numberPart = s[:len(s)-1]
	case 'K', 'k':
		multiplier = 1024
		numberPart = s[:len(s)-1]
	case 'M', 'm':
		multiplier = 1024 * 1024
		numberPart = s[:len(s)-1]
	case 'G', 'g':
		multiplier = 1024 * 1024 * 1024
		numberPart = s[:len(s)-1]
	case 'T', 't':
		multiplier = 1024 * 1024 * 1024 * 1024
		numberPart = s[:len(s)-1]
	default:
		// assume bytes if no unit
	}
	number, err := strconv.ParseInt(numberPart, 10, 64)
	if err != nil {
		return 0, err
	}
	return number * multiplier, nil
}

func main() {
	if err := rootCmd().Execute(); err != nil {
		log.Fatal(err)
	}
}
