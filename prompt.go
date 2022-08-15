package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func promptText() (string, error) {
	f, err := os.CreateTemp("", "")
	if err != nil {
		panic(fmt.Errorf("failed to open default text editor: %w", err))
	}
	f.Close()

	cmd := exec.Command("nano", f.Name())
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Start()
	if err != nil {
		return "", fmt.Errorf("failed to start editing command: %w", err)
	}
	err = cmd.Wait()
	if err != nil {
		return "", fmt.Errorf("error while editing: %w", err)
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}

	return strings.TrimSpace(string(b)), nil
}
