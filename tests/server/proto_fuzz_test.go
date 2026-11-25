package server_test

import (
	"bytes"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"testing"
)

func FuzzProtobufDecodeAndRoundTrip(f *testing.F) {
	for _, s := range []string{
		"", "\x08\x96\x01", "\x0a\x03Ada", "\x08\x01\x12\x01a\x1a\x00", "\x22\x06\x03\x8e\x02\x9e\xa7\x05",
		"\x0a\x00\x12\x00", "\x3a\x02\x0a\x00", "\x42\x07\x0a\x01k\x10\x81\x80\x01", "\xff\xff\xff", "\x08\xff\xff\xff\xff\xff\xff\xff\xff\xff\x01",
	} {
		f.Add([]byte(s))
	}
	api := pbAPI()
	f.Fuzz(func(t *testing.T, wire []byte) {
		rec := call(api, "POST", "/big", string(wire), "Content-Type", protoCT, "Accept", "application/json")
		if rec.Code == http.StatusInternalServerError {
			t.Fatalf("500 for %x: %s", wire, rec.Body)
		}
		if rec.Code != http.StatusOK {
			return
		}
		first := rec.Body.String()
		again := call(api, "POST", "/big", first, "Content-Type", "application/json", "Accept", protoCT)
		if again.Code != http.StatusOK {
			t.Fatalf("a message the server produced could not be encoded: %d %s\nfrom %x", again.Code, again.Body, wire)
		}
		back := call(api, "POST", "/big", again.Body.String(), "Content-Type", protoCT, "Accept", "application/json")
		if back.Code != http.StatusOK || back.Body.String() != first {
			t.Fatalf("round trip changed the message:\n%s\n%d %s\nfrom %x", first, back.Code, back.Body, wire)
		}
	})
}

func FuzzGRPCRequest(f *testing.F) {
	body := func(flag byte, msg string) []byte {
		b := make([]byte, 5, 5+len(msg))
		b[0] = flag
		binary.BigEndian.PutUint32(b[1:], uint32(len(msg)))
		return append(b, msg...)
	}
	f.Add(body(0, "\x0a\x03Ada"), byte(0))
	f.Add(body(0, `{"body":{"id":"1","name":"ab"}}`), byte(1))
	f.Add(body(0, "\x0a\x10"), byte(0))
	f.Add(body(1, "not gzip"), byte(1))
	f.Add(body(0, ""), byte(0))
	f.Add([]byte{0, 0, 0}, byte(0))
	f.Add(append(body(0, "{}"), body(0, "{}")...), byte(1))
	f.Add([]byte{0, 0xff, 0xff, 0xff, 0xff}, byte(0))
	api := pbAPI()
	f.Fuzz(func(t *testing.T, raw []byte, codec byte) {
		ct := grpcProto
		if codec%2 == 1 {
			ct = grpcJSON
		}
		req := httptest.NewRequest("POST", "/minato.API/PostBig", bytes.NewReader(raw))
		req.Header.Set("Content-Type", ct)
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, req)
		status := rec.Header().Get("Grpc-Status")
		if status == "" {
			status = rec.Header().Get(http.TrailerPrefix + "Grpc-Status")
		}
		if rec.Code != http.StatusOK || status == "" {
			t.Fatalf("every gRPC answer carries a status: %d %v", rec.Code, rec.Header())
		}
		if status == "0" {
			b := rec.Body.Bytes()
			if len(b) < 5 || int(binary.BigEndian.Uint32(b[1:5])) != len(b)-5 {
				t.Fatalf("a successful reply is exactly one framed message: %x", b)
			}
		}
	})
}
