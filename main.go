// Command screenjson-db-importer is the standalone CLI for importing
// ScreenJSON JSON using the screenjson-server persistence engine.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/screenjson/screenjson-db-importer/internal/engine"
)

func main() {
	fs := flag.NewFlagSet("screenjson-db-importer", flag.ExitOnError)
	cfg := fs.String("config", "", "server YAML config (also SCREENJSON_CONFIG)")
	workers := fs.Int("workers", 4, "concurrent document writers")
	manifest := fs.String("manifest", "screenjson-import.jsonl", "durable JSONL resume manifest")
	retry := fs.Bool("retry-failed", false, "retry sources whose latest manifest status is failed")
	_ = fs.Parse(os.Args[1:])
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: screenjson-db-importer [--config server.yaml] [--workers N] [--manifest file] [--retry-failed] <file|directory|glob|blob-uri>")
		os.Exit(2)
	}
	err := engine.Run(context.Background(), engine.Options{
		ConfigFile: *cfg, Source: fs.Arg(0), Workers: *workers,
		Manifest: *manifest, RetryFailed: *retry, Output: os.Stdout, ErrorOutput: os.Stderr,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "screenjson-db-importer:", err)
		os.Exit(1)
	}
}
