package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gfx-labs/acpc/pkg/acpc"

	"github.com/gfx-labs/acphost/pkg/sandbox"
	"go.yaml.in/yaml/v3"
)

// readConfig reads path ("-" is stdin, "" is an empty config).
func readConfig(path string, stdin io.Reader) (acpc.Config, error) {
	var c acpc.Config
	if path == "" {
		return c, nil
	}
	var b []byte
	var err error
	if path == "-" {
		b, err = io.ReadAll(stdin)
	} else {
		b, err = os.ReadFile(path)
	}
	if err != nil {
		return c, err
	}
	if err := parseConfig(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// parseConfig decodes YAML (or JSON, which is YAML) into c. It goes
// through JSON so the json tags and Mount's string-or-object form apply,
// and rejects unknown keys.
func parseConfig(b []byte, c *acpc.Config) error {
	var v any
	if err := yaml.Unmarshal(b, &v); err != nil {
		return err
	}
	if v == nil {
		return nil
	}
	j, err := json.Marshal(v)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(j))
	d.DisallowUnknownFields()
	if err := d.Decode(c); err != nil {
		return err
	}
	if c.Sandbox != nil {
		for _, m := range [][]sandbox.Mount{c.Sandbox.RO, c.Sandbox.RW} {
			if err := checkMountKeys(m); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkMountKeys rejects mounts with no source, which an unknown key in
// a mount object (decoded by Mount's own UnmarshalJSON) leaves behind.
func checkMountKeys(ms []sandbox.Mount) error {
	for _, m := range ms {
		if m.Src == "" {
			return fmt.Errorf("mount without src")
		}
	}
	return nil
}

// parseMount reads SRC[:DST].
func parseMount(s string) sandbox.Mount {
	src, dst, _ := strings.Cut(s, ":")
	return sandbox.Mount{Src: src, Dst: dst}
}
