package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/datamover/internal/export"
	"github.com/getkipper/kipper/datamover/internal/manifest"
)

var exportFlags struct {
	mode         string
	path         string
	targetURL    string
	transferID   string
	tokenEnv     string
	chunkSize    int64
	concurrency  int
	endpoint     string
	bucket       string
	accessKeyEnv string
	secretKeyEnv string
}

var exportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export data to a target import mover",
	Long: `export builds a manifest of the source data, negotiates resume state
with the target import mover, uploads missing chunks in parallel, and
verifies the target's finalize report against the manifest.`,
	RunE: runExport,
}

func init() {
	f := exportCmd.Flags()
	f.StringVar(&exportFlags.mode, "mode", "", "source kind: volume, s3, or file")
	f.StringVar(&exportFlags.path, "path", "", "directory (mode volume) or file (mode file) to export")
	f.StringVar(&exportFlags.targetURL, "target-url", "", "target base URL, e.g. https://console-api.target.example.com")
	f.StringVar(&exportFlags.transferID, "transfer-id", "", "transfer identifier")
	f.StringVar(&exportFlags.tokenEnv, "token-env", "DATAMOVER_TOKEN", "environment variable holding the bearer token")
	f.Int64Var(&exportFlags.chunkSize, "chunk-size", manifest.DefaultChunkSize, "chunk size in bytes")
	f.IntVar(&exportFlags.concurrency, "concurrency", 4, "parallel chunk uploads")
	f.StringVar(&exportFlags.endpoint, "endpoint", "", "s3 endpoint URL (mode s3)")
	f.StringVar(&exportFlags.bucket, "bucket", "", "s3 bucket name (mode s3)")
	f.StringVar(&exportFlags.accessKeyEnv, "access-key-env", "", "environment variable holding the s3 access key (mode s3)")
	f.StringVar(&exportFlags.secretKeyEnv, "secret-key-env", "", "environment variable holding the s3 secret key (mode s3)")
	for _, name := range []string{"mode", "target-url", "transfer-id"} {
		_ = exportCmd.MarkFlagRequired(name) //nolint:errcheck // flag names are static
	}
	rootCmd.AddCommand(exportCmd)
}

func runExport(cmd *cobra.Command, _ []string) error {
	token, err := tokenFromEnv(exportFlags.tokenEnv)
	if err != nil {
		return err
	}
	if exportFlags.chunkSize <= 0 {
		return fmt.Errorf("chunk size must be positive")
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var (
		m   *manifest.Manifest
		src export.Source
	)
	switch exportFlags.mode {
	case "volume":
		if exportFlags.path == "" {
			return fmt.Errorf("--path is required for mode volume")
		}
		m, err = manifest.BuildDir(exportFlags.path, exportFlags.chunkSize)
		src = &export.FSSource{Root: exportFlags.path}
	case "file":
		if exportFlags.path == "" {
			return fmt.Errorf("--path is required for mode file")
		}
		m, err = manifest.BuildFile(exportFlags.path, exportFlags.chunkSize)
		src = &export.FSSource{Root: filepath.Dir(exportFlags.path)}
	case "s3":
		if exportFlags.endpoint == "" || exportFlags.bucket == "" {
			return fmt.Errorf("--endpoint and --bucket are required for mode s3")
		}
		accessKey, secretKey, cerr := credsFromEnv(exportFlags.accessKeyEnv, exportFlags.secretKeyEnv)
		if cerr != nil {
			return cerr
		}
		s3src, serr := export.NewS3Source(exportFlags.endpoint, exportFlags.bucket, accessKey, secretKey)
		if serr != nil {
			return serr
		}
		m, err = s3src.BuildManifest(ctx, exportFlags.chunkSize)
		src = s3src
	default:
		return fmt.Errorf("unknown mode %q: want volume, s3, or file", exportFlags.mode)
	}
	if err != nil {
		return fmt.Errorf("building manifest: %w", err)
	}

	logf := func(format string, args ...any) {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), format+"\n", args...)
	}
	logf("manifest: %d entries, %d bytes", len(m.Entries), m.TotalBytes())

	client := &export.Client{
		HTTP:        export.NewHTTPClient(),
		BaseURL:     exportFlags.targetURL,
		TransferID:  exportFlags.transferID,
		Token:       token,
		Source:      src,
		Manifest:    m,
		Concurrency: exportFlags.concurrency,
		Backoff:     time.Second,
		Logf:        logf,
	}
	if err := client.Run(ctx); err != nil {
		return fmt.Errorf("export failed: %w", err)
	}
	logf("export complete")
	return nil
}
