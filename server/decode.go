package server

import (
	"bytes"
	"encoding/json"
	"mime"
)

type Decoder func(data []byte) (any, error)

func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc any
	err := dec.Decode(&doc)
	return doc, err
}

func (a *API) decode(contentType string, data []byte) (any, error) {
	mt, _, _ := mime.ParseMediaType(contentType)
	if d := a.Decoders[mt]; d != nil && mt != jsonMIME {
		v, err := d(data)
		if err != nil {
			return nil, err
		}
		if data, err = json.Marshal(v); err != nil {
			return nil, err
		}
	}
	return decodeJSON(data)
}
