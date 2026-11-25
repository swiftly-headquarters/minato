package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"strings"
)

type Decoder func(data []byte) (any, error)

var errUnsupportedMedia = errors.New("unsupported media type")

func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected data after the JSON value")
	}
	return doc, nil
}

func (a *API) decode(contentType string, data []byte, s *Schema) (any, error) {
	if contentType == "" {
		return decodeJSON(data)
	}
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, errUnsupportedMedia
	}
	if isProtoMIME(mt) {
		if a.Decoders[protoMIME] == nil || s == nil {
			return nil, errUnsupportedMedia
		}
		return a.reg.protoDecode(s, data)
	}
	if a.Decoders[mt] == nil && mt != jsonMIME && !strings.HasSuffix(mt, "+json") {
		return nil, errUnsupportedMedia
	}
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
