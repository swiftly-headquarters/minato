package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

func (a *API) SpecJSON() ([]byte, error) {
	b, err := json.MarshalIndent(a.Spec, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func (a *API) SpecYAML() ([]byte, error) {
	b, err := json.Marshal(a.Spec)
	if err != nil {
		return nil, err
	}
	return jsonToYAML(b)
}

func (a *API) WriteSpec(path string) error {
	render := a.SpecJSON
	if strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml") {
		render = a.SpecYAML
	}
	b, err := render()
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func jsonToYAML(data []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var b strings.Builder
	if err := yamlValue(dec, &b, 0, true); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

var (
	plainKey     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)
	reservedKeys = map[string]bool{"true": true, "false": true, "null": true, "yes": true, "no": true, "on": true, "off": true, "y": true, "n": true}
)

func yamlString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(s)
	return strings.TrimSuffix(buf.String(), "\n")
}

func yamlKey(k string) string {
	if plainKey.MatchString(k) && !reservedKeys[strings.ToLower(k)] {
		return k
	}
	return yamlString(k)
}

func yamlValue(dec *json.Decoder, b *strings.Builder, ind int, sameLine bool) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	lead := " "
	if sameLine {
		lead = ""
	}
	delim, isDelim := tok.(json.Delim)
	if !isDelim {
		switch v := tok.(type) {
		case string:
			tok = yamlString(v)
		case nil:
			tok = "null"
		}
		fmt.Fprintf(b, "%s%v\n", lead, tok)
		return nil
	}
	if !dec.More() {
		dec.Token()
		empty := "{}"
		if delim == '[' {
			empty = "[]"
		}
		b.WriteString(lead + empty + "\n")
		return nil
	}
	if !sameLine {
		b.WriteString("\n")
	}
	first := sameLine
	for dec.More() {
		if !first {
			b.WriteString(strings.Repeat(" ", ind))
		}
		first = false
		if delim == '{' {
			key, err := dec.Token()
			if err != nil {
				return err
			}
			b.WriteString(yamlKey(key.(string)) + ":")
			if err := yamlValue(dec, b, ind+2, false); err != nil {
				return err
			}
			continue
		}
		b.WriteString("- ")
		if err := yamlValue(dec, b, ind+2, true); err != nil {
			return err
		}
	}
	_, err = dec.Token()
	return err
}
