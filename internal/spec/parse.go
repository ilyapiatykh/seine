package spec

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// Parse decodes a YAML byte slice, applies defaults, then validates.
// The returned Document is safe to consume directly: any string field that
// represents an IP, prefix or duration will have been verified to parse.
func Parse(data []byte) (*Document, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var doc Document
	if err := dec.Decode(&doc); err != nil {
		if err == io.EOF {
			return nil, fmt.Errorf("spec: empty document")
		}
		return nil, fmt.Errorf("spec: yaml decode: %w", err)
	}
	doc.ApplyDefaults()
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	return &doc, nil
}

// LoadFile reads and parses a YAML file from disk.
func LoadFile(path string) (*Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("spec: read %s: %w", path, err)
	}
	return Parse(data)
}

// Marshal serialises a Document back to YAML. Useful for tests and for
// commit-side tooling that round-trips the spec.
func Marshal(d *Document) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(d); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
