//go:build !windows

package update

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The test binary re-executes itself: stage 1 prints its PID and calls Reexec, stage 2 (the new image) prints its PID.
func TestMain(m *testing.M) {
	switch os.Getenv("PRODUCTOS_AGENT_REEXEC_STAGE") {
	case "1":
		fmt.Printf("before %d\n", os.Getpid())
		os.Setenv("PRODUCTOS_AGENT_REEXEC_STAGE", "2")
		exe, _ := os.Executable()
		err := Reexec(exe)
		fmt.Println("reexec failed:", err)
		os.Exit(1)
	case "2":
		fmt.Printf("after %d %s\n", os.Getpid(), strings.Join(os.Args[1:], " "))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestReexecKeepsThePID(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$", "extra-arg")
	cmd.Env = append(os.Environ(), "PRODUCTOS_AGENT_REEXEC_STAGE=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	var before, after int
	var args string
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		t.Fatalf("output = %q", out)
	}
	fmt.Sscanf(lines[0], "before %d", &before)
	fmt.Sscanf(lines[1], "after %d", &after)
	args = strings.TrimPrefix(lines[1], fmt.Sprintf("after %d ", after))
	if before == 0 || before != after {
		t.Errorf("pid before %d, after %d", before, after)
	}
	if args != "-test.run=^$ extra-arg" {
		t.Errorf("the arguments must survive the re-exec: %q", args)
	}
}
