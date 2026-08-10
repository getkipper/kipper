// Package cmd wires the datamover CLI: the export client and the import
// server that together move volume, S3, and single-file data between clusters.
package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "datamover",
	Short: "Kipper migration data mover",
	Long: `datamover moves application data between Kipper clusters during a
migration: it chunks, compresses, and verifies volume contents, S3 buckets,
and spooled files over a resumable HTTPS protocol.`,
	SilenceUsage: true,
}

// SetVersion sets the binary version string displayed by --version.
func SetVersion(v string) {
	rootCmd.Version = v
}

// Execute runs the root command and exits non-zero on error.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// tokenFromEnv reads the bearer token from the named environment variable.
// The token value itself never appears in errors or logs.
func tokenFromEnv(envVar string) (string, error) {
	if envVar == "" {
		return "", fmt.Errorf("token environment variable name must not be empty")
	}
	token := os.Getenv(envVar)
	if token == "" {
		return "", fmt.Errorf("environment variable %s is empty or unset", envVar)
	}
	return token, nil
}

// credsFromEnv reads S3 credentials from the named environment variables.
func credsFromEnv(accessEnv, secretEnv string) (accessKey, secretKey string, err error) {
	accessKey, err = tokenFromEnv(accessEnv)
	if err != nil {
		return "", "", fmt.Errorf("access key: %w", err)
	}
	secretKey, err = tokenFromEnv(secretEnv)
	if err != nil {
		return "", "", fmt.Errorf("secret key: %w", err)
	}
	return accessKey, secretKey, nil
}
