package main

import (
	"fmt"
	"log"
	"strconv"
	"strings"

	resizer "github.com/diskfs/partitionresizer"
	"github.com/spf13/cobra"
)

var rootCmd = func() *cobra.Command {
	var (
		shrinkPartition string
		growPartitions  []string
		fixErrors       bool
		dryRun          bool
	)
	cmd := &cobra.Command{
		Use:   "resizer",
		Short: "Resizer for OS disks, given certain constraints",
		Long: `Resize for OS disks, given certain constraints. Expects to resize a list of partitions
  upwards, and, if necessary, resize others downwards to make space.
 
  You must provide at least the --grow-partitions flag, which takes a list of partitions to grow,
  along with their desired sizes. If there is not enough free space on the disk, you must also
  provide the --shrink-partition flag, which takes a single partition to shrink to make space.
  
  Partitions can be identified by their name (e.g. sda1), or by their label (e.g. EFI System).
  Sizes can be specified in bytes (B), kilobytes (K), megabytes (M), gigabytes (G), or terabytes (T).

  Example usage:
    resizer --shrink-partition name:sda3 --grow-partition name:sda1:20G --grow-partition label:Data:100G
	resizer --shrink-partition label:P2 --grow-partition name:sda1:20G --grow-partition label:Data:100G

  Will fail if any of the following is true:
    - The specified shrink partition does not have enough space to accommodate the total growth requested.
	- Any of the specified grow partitions do not exist.
	- The specified shrink partition does not exist.
	- There is not enough free space on the disk after shrinking to accommodate the growth.
	- There is not enough free space on the disk and no shrink partition is provided.
	- The shrink partition is of a format for which we do not support resizing.
	- Any listed partition cannot be found.
	- Multiple partitions with the same specified label are found.
  `,
		Run: func(cmd *cobra.Command, args []string) {
			// check validity of flags
			var (
				shrinkPartitionParsed resizer.PartitionIdentifier
				growPartitionsParsed  []resizer.PartitionChange
				disk                  string
			)
			if shrinkPartition != "" {
				var err error
				shrinkPartitionParsed, err = parsePartitionIdentifier(shrinkPartition)
				if err != nil {
					log.Fatalf("Invalid shrink-partition value: %v", err)
				}
			}
			for _, gp := range growPartitions {
				gpParsed, err := parsePartitionChange(gp)
				if err != nil {
					log.Fatalf("Invalid grow-partition value '%s': %v", gp, err)
				}
				growPartitionsParsed = append(growPartitionsParsed, gpParsed)
			}
			if len(growPartitionsParsed) == 0 {
				log.Fatal("At least one --grow-partition must be specified")
			}
			if len(args) > 0 {
				disk = args[0]
			}
			if err := resizer.Run(disk, &shrinkPartitionParsed, growPartitionsParsed, fixErrors, dryRun); err != nil {
				log.Fatalf("Resize operation failed: %v", err)
			}
		},
	}
	cmd.Flags().StringVar(&shrinkPartition, "shrink-partition", "", "Partition to shrink to make space, if necessary")
	cmd.Flags().StringSliceVar(&growPartitions, "grow-partition", []string{}, "Partitions to grow, along with their desired sizes, in format identifier:partition:size, see help (e.g. name:sda1:20G or label:EFI System:100M)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "If set, will only simulate the resize operations without making any changes")
	cmd.Flags().BoolVar(&fixErrors, "fix-errors", false, "If set, will attempt to fix any ext4 filesystem errors found during fsck before shrinking")
	return cmd
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
	default:
		return nil, fmt.Errorf("unknown identifier type: %s", parts[0])
	}
	return resizer.NewPartitionIdentifier(by, parts[1]), nil
}

func parsePartitionChange(s string) (resizer.PartitionChange, error) {
	parts := strings.SplitN(s, ":", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid partition change format: %s", s)
	}
	pi, err := parsePartitionIdentifier(strings.Join(parts[0:2], ":"))
	if err != nil {
		return nil, err
	}
	size, err := parseSize(parts[2])
	if err != nil {
		return nil, fmt.Errorf("invalid size '%s': %v", parts[2], err)
	}
	return resizer.NewPartitionChange(pi.By(), pi.Value(), size), nil
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
