package main

import (
	_ "embed"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

//go:embed bb_chroot_helper
var bbChrootHelper []byte

//go:embed bb_chroot_helper_privileged
var bbChrootHelperPrivileged []byte

func install(installationPath string) error {
	if err := os.MkdirAll(installationPath, 0o755); err != nil {
		return fmt.Errorf("failed to create installation directory: %w", err)
	}
	installedPath := filepath.Join(installationPath, "chroot_helpers_installed")
	if err := os.Remove(installedPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove old installation marker: %w", err)
	}

	for name, contents := range map[string][]byte{
		"bb_chroot_helper":            bbChrootHelper,
		"bb_chroot_helper_privileged": bbChrootHelperPrivileged,
	} {
		path := filepath.Join(installationPath, name)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove old %s: %w", name, err)
		}
		if err := os.WriteFile(path, contents, 0o555); err != nil {
			return fmt.Errorf("failed to install %s: %w", name, err)
		}
	}

	if err := os.WriteFile(installedPath, nil, 0o444); err != nil {
		return fmt.Errorf("failed to write installation marker: %w", err)
	}
	return nil
}

func main() {
	installationPath := "/bb"
	switch len(os.Args) {
	case 2:
		installationPath = os.Args[1]
	case 1:
	default:
		log.Fatal("usage: bb_chroot_helper_installer [installation_path]")
	}

	if err := install(installationPath); err != nil {
		log.Fatal(err)
	}
}
