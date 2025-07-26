package langserver

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
)

func convertRowColToIndex(s string, row, col int) int {
	lines := strings.Split(s, "\n")

	if row < 0 {
		row = 0
	} else if row >= len(lines) {
		row = len(lines) - 1
	}

	if col < 0 {
		col = 0
	} else if col > len(lines[row]) {
		col = len(lines[row])
	}

	index := 0
	for i := 0; i < row; i++ {
		// Add the length of each line plus 1 for the newline character
		index += len(lines[i]) + 1
	}
	index += col

	return index
}

func CheckAndInstallTool(ctx context.Context, logger *log.Logger, config Language, toolName string, isInstallDeps bool) error {
	if config.CheckInstalled == "" {
		return nil
	}

	logger.Printf("Checking if %s is installed using command: %s", toolName, config.CheckInstalled)
	cmd := exec.CommandContext(ctx, "sh", "-c", config.CheckInstalled)
	cmd.Env = os.Environ()
	output, err := cmd.CombinedOutput()

	if err != nil || len(bytes.TrimSpace(output)) == 0 {
		logger.Printf("Tool %s not found or check command returned falsy value. Output: %s, Error: %v", toolName, string(output), err)
		if config.Install != "" && isInstallDeps {
			logger.Printf("Attempting to install %s using command: %s", toolName, config.Install)
			installCmd := exec.CommandContext(ctx, "sh", "-c", config.Install)
			installCmd.Env = os.Environ()
			installOutput, installErr := installCmd.CombinedOutput()
			if installErr != nil {
				return fmt.Errorf("failed to install %s: %v, Output: %s", toolName, installErr, string(installOutput))
			}
			logger.Printf("Successfully installed %s. Output: %s", toolName, string(installOutput))

			// Re-check after installation
			logger.Printf("Re-checking if %s is installed after installation.", toolName)
			recheckCmd := exec.CommandContext(ctx, "sh", "-c", config.CheckInstalled)
			recheckCmd.Env = os.Environ()
			recheckOutput, recheckErr := recheckCmd.CombinedOutput()
			if recheckErr != nil || len(bytes.TrimSpace(recheckOutput)) == 0 {
				return fmt.Errorf("tool %s still not found after installation. Output: %s, Error: %v", toolName, string(recheckOutput), recheckErr)
			}
			logger.Printf("Tool %s successfully verified after installation.", toolName)
		} else if config.Install != "" && !isInstallDeps {
			return fmt.Errorf("tool %s not found. Run with --install-deps to install.", toolName)
		} else {
			return fmt.Errorf("tool %s not found and no install command specified", toolName)
		}
	} else {
		logger.Printf("Tool %s is installed.", toolName)
	}
	return nil
}