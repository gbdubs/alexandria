package archive

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// assets contains the existing catalog contract and the existing Alexandria UI.
// Keeping both embedded makes the service a single, relocatable executable.
//
//go:embed assets
var assets embed.FS

func schemaSQL() string {
	value, _ := assets.ReadFile("assets/schema.sql")
	return string(value)
}

func appHTML() string {
	value, _ := assets.ReadFile("assets/ui.py")
	return uiHTML(value)
}

// uiHTML extracts the page from ui.py, which wraps it in a Python string.
func uiHTML(source []byte) string {
	text := string(source)
	text = strings.TrimPrefix(text, "APP_HTML = r'''")
	text = strings.TrimSuffix(text, "'''\n")
	return text
}

func querySchemaDocument(name string) ([]byte, error) {
	path := "assets/query-schemas/" + name + ".schema.json"
	if value, err := assets.ReadFile(path); err == nil {
		return value, nil
	}
	// `macos/build-app.sh` copies the canonical documents into embedded assets.
	// These fallbacks keep direct `go test` and `go run` useful from the source
	// tree before that packaging step has run.
	for _, candidate := range []string{
		filepath.Join("schemas", name+".schema.json"),
		filepath.Join("..", "..", "schemas", name+".schema.json"),
	} {
		if value, err := os.ReadFile(candidate); err == nil {
			return value, nil
		}
	}
	return nil, fmt.Errorf("query-table schema not found: %s", name)
}

func queryTableAsset(name string) ([]byte, error) {
	return assets.ReadFile("assets/" + name)
}
