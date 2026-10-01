// Command configdoc writes docs/CONFIG.md from the config package.
//
// Usage:
//
//	go run ./internal/config/cmd/configdoc > docs/CONFIG.md
package main

import (
	"fmt"

	"github.com/screenjson/screenjson-db-importer/internal/config"
)

func main() {
	fmt.Print(config.Reference())
}
