//go:build linux

package processguard

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

func TestHardenPreventsSameUIDChildFromReadingParentEnvironment(t *testing.T) {
	if os.Getenv("PAJE_PROCESS_GUARD_HELPER") == "1" {
		if err := dropPtraceCapability(); err != nil {
			os.Exit(5)
		}
		parentPID, err := strconv.Atoi(os.Getenv("PAJE_PROCESS_GUARD_PARENT_PID"))
		if err != nil {
			os.Exit(2)
		}
		_, err = os.ReadFile("/proc/" + strconv.Itoa(parentPID) + "/environ")
		if err == nil {
			os.Exit(3)
		}
		var pathError *os.PathError
		if !errors.As(err, &pathError) || !errors.Is(pathError.Err, unix.EACCES) {
			os.Exit(4)
		}
		os.Exit(0)
	}

	if err := Harden(); err != nil {
		t.Fatalf("Harden() error = %v", err)
	}
	dumpable, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	if err != nil {
		t.Fatalf("read dumpable state: %v", err)
	}
	if dumpable != 0 {
		t.Fatalf("dumpable state = %d, want 0", dumpable)
	}

	command := exec.Command(os.Args[0], "-test.run=^TestHardenPreventsSameUIDChildFromReadingParentEnvironment$")
	command.Env = append(os.Environ(),
		"PAJE_PROCESS_GUARD_HELPER=1",
		"PAJE_PROCESS_GUARD_PARENT_PID="+strconv.Itoa(os.Getpid()),
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("same-UID child read parent environment: %v: %s", err, output)
	}
}

func dropPtraceCapability() error {
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{}
	if err := unix.Capget(&header, &data[0]); err != nil {
		return err
	}
	index := uint(unix.CAP_SYS_PTRACE) / 32
	mask := ^(uint32(1) << (uint(unix.CAP_SYS_PTRACE) % 32))
	data[index].Effective &= mask
	data[index].Permitted &= mask
	data[index].Inheritable &= mask
	if err := unix.Capset(&header, &data[0]); err != nil {
		return err
	}
	return unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_LOWER, uintptr(unix.CAP_SYS_PTRACE), 0, 0)
}
