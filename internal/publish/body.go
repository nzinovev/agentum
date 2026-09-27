package publish

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"strings"
	"text/template"
	"time"
)

//go:embed pr-template.md
var descriptionTemplate string

var parsedDescriptionTemplate = template.Must(template.New("pr-description").Funcs(template.FuncMap{
	"cell":      tableCell,
	"join":      strings.Join,
	"timestamp": func(value time.Time) string { return value.UTC().Format(time.RFC3339) },
}).Parse(descriptionTemplate))

// RenderDescription renders publication metadata without reading source artifacts.
// The coordinator must store and scan the result before giving it to a provider.
func RenderDescription(delivery Delivery) ([]byte, error) {
	var body bytes.Buffer
	if err := parsedDescriptionTemplate.Execute(&body, delivery); err != nil {
		return nil, err
	}
	// NUL would make the artifact scanner classify Markdown as binary and
	// skip text redaction. Replace it before crossing the scanning boundary.
	return []byte(strings.ReplaceAll(body.String(), "\x00", "\uFFFD")), nil
}

func tableCell(value string) string {
	return strings.NewReplacer("|", "\\|", "\n", " ", "\r", " ").Replace(value)
}

// Validate refuses a payload that cannot be tied to its stored revision.
func (description DescriptionRef) Validate() error {
	digest := sha256.Sum256([]byte(description.Text))
	if description.RevisionID == "" || description.Text == "" || hex.EncodeToString(digest[:]) != description.ContentHash {
		return refuse(ReasonDescriptionInvalid)
	}
	return nil
}
