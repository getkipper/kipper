package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/datamover/internal/ingest"
)

var importFlags struct {
	mode         string
	listen       string
	root         string
	stateDir     string
	tokenEnv     string
	tlsCert      string
	tlsKey       string
	endpoint     string
	bucket       string
	accessKeyEnv string
	secretKeyEnv string
}

var importCmd = &cobra.Command{
	Use:   "import",
	Short: "Serve the transfer ingest API",
	Long: `import receives chunks from an export mover, persists resume state
under the root so a restart continues instead of starting over, verifies
every file after assembly, and commits it to the filesystem or an S3 bucket.`,
	RunE: runImport,
}

func init() {
	f := importCmd.Flags()
	f.StringVar(&importFlags.mode, "mode", "volume", "target kind: volume (filesystem) or s3")
	f.StringVar(&importFlags.listen, "listen", ":8443", "listen address")
	f.StringVar(&importFlags.root, "root", "", "directory data is committed to (mode volume) or scratch space (mode s3)")
	f.StringVar(&importFlags.stateDir, "state-dir", "", "directory for transient transfer state: chunk staging and the resume bitmap (default <root>/.kipper-transfer-state). Point it at fast non-NFS scratch when the root is an RWX/NFS volume; ephemeral scratch trades resume-across-pod-restarts for keeping the data volume clean")
	f.StringVar(&importFlags.tokenEnv, "token-env", "DATAMOVER_TOKEN", "environment variable holding the bearer token")
	f.StringVar(&importFlags.tlsCert, "tls-cert", "", "TLS certificate file (serves plain HTTP when unset)")
	f.StringVar(&importFlags.tlsKey, "tls-key", "", "TLS key file")
	f.StringVar(&importFlags.endpoint, "endpoint", "", "s3 endpoint URL (mode s3)")
	f.StringVar(&importFlags.bucket, "bucket", "", "target s3 bucket (mode s3, created when missing)")
	f.StringVar(&importFlags.accessKeyEnv, "access-key-env", "", "environment variable holding the s3 access key (mode s3)")
	f.StringVar(&importFlags.secretKeyEnv, "secret-key-env", "", "environment variable holding the s3 secret key (mode s3)")
	_ = importCmd.MarkFlagRequired("root") //nolint:errcheck // flag name is static
	rootCmd.AddCommand(importCmd)
}

func runImport(cmd *cobra.Command, _ []string) error {
	token, err := tokenFromEnv(importFlags.tokenEnv)
	if err != nil {
		return err
	}
	if (importFlags.tlsCert == "") != (importFlags.tlsKey == "") {
		return fmt.Errorf("--tls-cert and --tls-key must be set together")
	}

	logf := func(format string, args ...any) {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), format+"\n", args...)
	}

	var committer ingest.Committer
	switch importFlags.mode {
	case "volume":
		committer = ingest.FSCommitter{}
	case "s3":
		if importFlags.endpoint == "" || importFlags.bucket == "" {
			return fmt.Errorf("--endpoint and --bucket are required for mode s3")
		}
		accessKey, secretKey, cerr := credsFromEnv(importFlags.accessKeyEnv, importFlags.secretKeyEnv)
		if cerr != nil {
			return cerr
		}
		store, serr := ingest.NewMinioStore(importFlags.endpoint, importFlags.bucket, accessKey, secretKey)
		if serr != nil {
			return serr
		}
		committer = ingest.ObjectCommitter{Store: store}
	default:
		return fmt.Errorf("unknown mode %q: want volume or s3", importFlags.mode)
	}

	server, err := ingest.NewServer(importFlags.root, importFlags.stateDir, token, committer, logf)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	httpServer := &http.Server{
		Addr:              importFlags.listen,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		logf("listening on %s", importFlags.listen)
		if importFlags.tlsCert != "" {
			errCh <- httpServer.ListenAndServeTLS(importFlags.tlsCert, importFlags.tlsKey)
		} else {
			errCh <- httpServer.ListenAndServe()
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("serving: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("shutting down: %w", err)
	}
	logf("shut down cleanly")
	return nil
}
