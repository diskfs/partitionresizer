package partitionresizer

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
)

const imgFile = "testdata/dist/disk.img"

// TestMain sets up the test environment and runs the tests
func TestMain(m *testing.M) {
	// Check and generate artifacts if necessary
	if _, err := os.Stat(imgFile); os.IsNotExist(err) {
		// Run the buildimg.sh script
		cmd := exec.Command("sh", "buildimg.sh")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Dir = "testdata"

		// Execute the command
		if err := cmd.Run(); err != nil {
			println("error generating test artifacts for ext4", err)
			os.Exit(1)
		}
	}

	if _, err := os.Stat(diskfullImg); os.IsNotExist(err) {
		// Run the buildimg.sh script
		cmd := exec.Command("sh", "buildimgfull.sh")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Dir = "testdata"

		// Execute the command
		if err := cmd.Run(); err != nil {
			println("error generating test artifacts for ext4", err)
			os.Exit(1)
		}
	}

	// Run the tests
	code := m.Run()

	// Exit with the appropriate code
	os.Exit(code)
}

// copy infile to outfile
func testCopyFile(infile, outfile string) error {
	in, err := os.Open(infile)
	if err != nil {
		return fmt.Errorf("Error opening input file: %w", err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(outfile)
	if err != nil {
		return fmt.Errorf("Error opening output file: %w", err)
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("Error copying file contents: %w", err)
	}
	return nil
}
