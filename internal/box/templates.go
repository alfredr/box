package box

import (
	"bytes"
	"embed"
	"fmt"
	"strconv"
	"strings"
	"text/template"
)

//go:embed templates/*.tmpl
var templateFiles embed.FS

var templates = template.Must(template.New("").Funcs(template.FuncMap{
	// Validated names are ASCII, so Go string quoting produces valid YAML strings here.
	"quote": strconv.Quote,
	"join":  strings.Join,
	// Emit Caddy template syntax as data so text/template does not evaluate it.
	"upstreams": func(port int) string { return fmt.Sprintf("{{upstreams %d}}", port) },
}).ParseFS(templateFiles, "templates/*.tmpl"))

func render(name string, data any) ([]byte, error) {
	var b bytes.Buffer
	if err := templates.ExecuteTemplate(&b, name, data); err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}
