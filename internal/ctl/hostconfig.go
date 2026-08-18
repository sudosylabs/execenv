package ctl

import (
	"encoding/json"
	"fmt"
	"os"
)

// writtenConfig is the operator document. Token stays in this file only.
type writtenConfig struct {
	Listen     string         `json:"listen"`
	Token      string         `json:"token"`
	Security   string         `json:"security"`
	Adapter    string         `json:"adapter"`
	TLSCert    string         `json:"tls_cert,omitempty"`
	TLSKey     string         `json:"tls_key,omitempty"`
	WorkDir    string         `json:"work_dir"`
	Device     string         `json:"device"`
	Runtime    string         `json:"runtime"`
	Supervisor string         `json:"supervisor"`
	Images     []writtenImage `json:"images"`
	Slots      int            `json:"slots"`
	Network    string         `json:"network"`
	Grace      string         `json:"grace"`
}

type writtenImage struct {
	ID     string `json:"id"`
	Kernel string `json:"kernel,omitempty"`
	Rootfs string `json:"rootfs,omitempty"`
	Hash   string `json:"hash,omitempty"`
}

func loadExisting(path string) (writtenConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return writtenConfig{}, err
	}
	var doc writtenConfig
	if err := json.Unmarshal(raw, &doc); err != nil {
		// Do not wrap the decoder error: it can quote the token.
		return writtenConfig{}, wrap("config", errInvalidConfig)
	}
	return doc, nil
}

func saveConfig(path string, doc writtenConfig) error {
	if doc.Images == nil {
		doc.Images = []writtenImage{}
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return wrap("config", err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return wrap("config", err)
	}
	return os.Chmod(path, 0o600)
}

func (doc *writtenConfig) upsertImage(img writtenImage) {
	for i, existing := range doc.Images {
		if existing.ID == img.ID {
			doc.Images[i] = img
			return
		}
	}
	doc.Images = append(doc.Images, img)
}

var errInvalidConfig = fmt.Errorf("invalid host config")
