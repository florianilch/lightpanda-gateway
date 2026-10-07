//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/landlock-lsm/go-landlock/landlock"
	llsyscall "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

const lightpandaPath = "/usr/local/bin/lightpanda"

func main() {
	if err := restrictFilesystem(); err != nil {
		fmt.Fprintf(os.Stderr, "lightpanda-landlock: filesystem policy failed; refusing to execute Lightpanda: %v\n", err)
		os.Exit(1)
	}
	argv := append([]string{lightpandaPath}, os.Args[1:]...)
	if err := syscall.Exec(lightpandaPath, argv, os.Environ()); err != nil { // #nosec G204 G702 -- fixed absolute executable path; intentional argument passthrough without a shell
		fmt.Fprintf(os.Stderr, "lightpanda-landlock: exec %s: %v\n", lightpandaPath, err)
		os.Exit(1)
	}
}

func restrictFilesystem() error {
	if err := os.Mkdir("out", 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create output directory: %w", err)
	}
	info, err := os.Lstat("out")
	if err != nil {
		return fmt.Errorf("inspect output directory: %w", err)
	}
	if !info.IsDir() {
		return errors.New(`output path "out" must be a directory`)
	}
	const readAccess landlock.AccessFSSet = llsyscall.AccessFSReadFile | llsyscall.AccessFSReadDir
	const outputAccess = readAccess | llsyscall.AccessFSWriteFile | llsyscall.AccessFSTruncate |
		llsyscall.AccessFSMakeReg | llsyscall.AccessFSMakeDir | llsyscall.AccessFSRemoveFile | llsyscall.AccessFSRemoveDir

	return landlock.V3.RestrictPaths(
		landlock.RODirs("/usr", "/etc"),
		landlock.RODirs("/bin", "/lib", "/lib64").IgnoreIfMissing(),
		landlock.ROFiles("/dev/urandom", "/dev/random"),
		landlock.PathAccess(readAccess, "."),
		// Output directory
		landlock.PathAccess(outputAccess, "./out"),
	)
}
