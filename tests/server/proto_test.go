package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/swiftly-headquarters/minato/server"
)

const protoCT = "application/x-protobuf"

type pbTiny struct {
	A int32   `json:"a"`
	B string  `json:"b"`
	C []int32 `json:"c"`
	D *string `json:"d"`
}

type pbTinyIO struct {
	Body pbTiny
}

type pbAddr struct {
	City string `json:"city"`
	Zip  *int32 `json:"zip"`
}

type pbBig struct {
	ID    int64            `json:"id"`
	Name  string           `json:"name" minLength:"1"`
	Tags  []string         `json:"tags"`
	Addr  pbAddr           `json:"addr"`
	Addrs []pbAddr         `json:"addrs"`
	Meta  map[string]int64 `json:"meta"`
	Raw   []byte           `json:"raw"`
	On    bool             `json:"on"`
	Score float64          `json:"score"`
	Small float32          `json:"small"`
	U     uint64           `json:"u"`
	Any   any              `json:"any"`
}

type pbBigIO struct {
	Body pbBig
}

func pbAPI() *server.API {
	api := server.New("t", "1")
	server.Post(api, "/tiny", func(_ context.Context, in *pbTinyIO) (*pbTinyIO, error) { return in, nil })
	server.Post(api, "/big", func(_ context.Context, in *pbBigIO) (*pbBigIO, error) { return in, nil })
	return api
}

func pbCall(api *server.API, path, body string, hdr ...string) (int, string, http.Header) {
	rec := call(api, "POST", path, body, hdr...)
	return rec.Code, rec.Body.String(), rec.Header()
}

func TestProtobufWireVectors(t *testing.T) {
	api := pbAPI()
	const want = "\x08\x96\x01\x12\x07testing\x1a\x06\x03\x8e\x02\x9e\xa7\x05"
	code, body, h := pbCall(api, "/tiny", `{"a":150,"b":"testing","c":[3,270,86942],"d":null}`, "Content-Type", "application/json", "Accept", protoCT)
	if code != 200 || body != want || h.Get("Content-Type") != protoCT {
		t.Fatalf("encode: %d %q %v", code, body, h)
	}
	code, body, _ = pbCall(api, "/tiny", want, "Content-Type", protoCT, "Accept", "application/json")
	if code != 200 || body != `{"a":150,"b":"testing","c":[3,270,86942],"d":null}` {
		t.Fatalf("decode: %d %q", code, body)
	}
}

func TestProtobufNegativeAndDefaults(t *testing.T) {
	api := pbAPI()
	neg := "\x08\xff\xff\xff\xff\xff\xff\xff\xff\xff\x01"
	code, body, _ := pbCall(api, "/tiny", neg, "Content-Type", protoCT, "Accept", "application/json")
	if code != 200 || !strings.HasPrefix(body, `{"a":-1,`) {
		t.Fatalf("a negative int32 is a 10 byte varint: %d %q", code, body)
	}
	code, body, _ = pbCall(api, "/tiny", "", "Content-Type", protoCT, "Accept", "application/json")
	if code != 200 || body != `{"a":0,"b":"","c":[],"d":null}` {
		t.Fatalf("an empty message is all zero values: %d %q", code, body)
	}
}

func TestProtobufEncodesNegativeNumbers(t *testing.T) {
	api := pbAPI()
	code, wire, _ := pbCall(api, "/tiny", `{"a":-1,"b":"","c":[-2],"d":null}`, "Content-Type", "application/json", "Accept", protoCT)
	const want = "\x08\xff\xff\xff\xff\xff\xff\xff\xff\xff\x01" + "\x1a\x0a\xfe\xff\xff\xff\xff\xff\xff\xff\xff\x01"
	if code != 200 || wire != want {
		t.Fatalf("negative int32 values are sign-extended to ten bytes: %d %q", code, wire)
	}
}

func TestProtobufRoundTripsRichTypes(t *testing.T) {
	api := pbAPI()
	in := `{"id":9007199254740993,"name":"Ada","tags":["a","b"],"addr":{"city":"Oslo","zip":123},"addrs":[{"city":"X","zip":null},{"city":"Y","zip":-5}],"meta":{"k":1,"z":-2},"raw":"AAEC/w==","on":true,"score":1.5,"small":0.25,"u":18446744073709551615,"any":{"x":[1,"two",null]}}`
	code, wire, _ := pbCall(api, "/big", in, "Content-Type", "application/json", "Accept", protoCT)
	if code != 200 {
		t.Fatalf("encode: %d %s", code, wire)
	}
	code, out, _ := pbCall(api, "/big", wire, "Content-Type", protoCT, "Accept", "application/json")
	if code != 200 {
		t.Fatalf("decode: %d %s", code, out)
	}
	var want, got any
	d1, d2 := json.NewDecoder(strings.NewReader(in)), json.NewDecoder(strings.NewReader(out))
	d1.UseNumber()
	d2.UseNumber()
	d1.Decode(&want)
	d2.Decode(&got)
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("round trip changed the message:\n in %s\nout %s", in, out)
	}
}

func TestProtobufRejectsBadWire(t *testing.T) {
	api := pbAPI()
	for name, wire := range map[string]string{
		"truncated varint":      "\x08\x96",
		"truncated length":      "\x12\x07test",
		"wire type mismatch":    "\x0a\x01x",
		"invalid UTF-8":         "\x12\x02\xff\xfe",
		"field number zero":     "\x00\x01",
		"group wire type":       "\x0b",
		"fixed32 cut short":     "\x1d\x01",
		"packed element cut":    "\x1a\x02\x8e\x02\x9e",
		"length beyond message": "\x12\xff\xff\xff\xff\x0f",
	} {
		if code, body, _ := pbCall(api, "/tiny", wire, "Content-Type", protoCT); code != 400 {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
}

func TestProtobufSkipsUnknownFieldsAndAcceptsUnpackedLists(t *testing.T) {
	api := pbAPI()
	wire := "\x78\x01" + "\x82\x07\x03abc" + "\x08\x07" + "\x18\x01\x18\x02" + "\x4d\x01\x00\x00\x00" + "\x51\x01\x00\x00\x00\x00\x00\x00\x00"
	code, body, _ := pbCall(api, "/tiny", wire, "Content-Type", protoCT, "Accept", "application/json")
	if code != 200 || body != `{"a":7,"b":"","c":[1,2],"d":null}` {
		t.Fatalf("%d %q", code, body)
	}
}

func TestProtobufAppliesValidation(t *testing.T) {
	api := pbAPI()
	code, body, _ := pbCall(api, "/big", "\x08\x01", "Content-Type", protoCT)
	if code != 422 || !strings.Contains(body, "body.name") {
		t.Fatalf("a missing required string is a zero value that the tags still check: %d %s", code, body)
	}
	code, body, _ = pbCall(api, "/big", "\x08\x01\x12\x01a", "Content-Type", protoCT)
	if code != 422 || !strings.Contains(body, "body.addr") {
		t.Fatalf("a missing nested message is still required: %d %s", code, body)
	}
}

func TestProtobufNegotiation(t *testing.T) {
	api := pbAPI()
	in := `{"a":1,"b":"x","c":[],"d":null}`
	for accept, want := range map[string]string{"": "application/json", "*/*": "application/json", "application/*": "application/json", protoCT: protoCT, protoCT + ";q=0.5, application/json;q=0.1": protoCT} {
		hdr := []string{"Content-Type", "application/json"}
		if accept != "" {
			hdr = append(hdr, "Accept", accept)
		}
		code, _, h := pbCall(api, "/tiny", in, hdr...)
		if code != 200 || h.Get("Content-Type") != want || h.Get("Vary") != "Accept" {
			t.Errorf("Accept %q: %d %v", accept, code, h)
		}
	}
	for _, ct := range []string{"application/protobuf", "application/vnd.google.protobuf", protoCT + "; charset=binary"} {
		if code, body, _ := pbCall(api, "/tiny", "\x08\x01", "Content-Type", ct); code != 200 {
			t.Errorf("%s: %d %s", ct, code, body)
		}
	}
}

func TestProtobufCanBeSwitchedOff(t *testing.T) {
	api := pbAPI()
	delete(api.Formats, protoCT)
	delete(api.Decoders, protoCT)
	api2 := server.New("t", "1")
	delete(api2.Formats, protoCT)
	delete(api2.Decoders, protoCT)
	server.Post(api2, "/tiny", func(_ context.Context, in *pbTinyIO) (*pbTinyIO, error) { return in, nil })
	if code, _, _ := pbCall(api2, "/tiny", "\x08\x01", "Content-Type", protoCT); code != 415 {
		t.Fatalf("a request in a format that was switched off: %d", code)
	}
	if code, _, _ := pbCall(api2, "/tiny", `{"a":1,"b":"","c":[]}`, "Content-Type", "application/json", "Accept", protoCT); code != 406 {
		t.Fatalf("a response in a format that was switched off: %d", code)
	}
}

type pbNested struct {
	Grid [][]int32 `json:"grid"`
}

type pbNestedIO struct {
	Body pbNested
}

func TestProtobufUnsupportedShapesFallBackToJSON(t *testing.T) {
	api := server.New("t", "1")
	server.Post(api, "/grid", func(_ context.Context, in *pbNestedIO) (*pbNestedIO, error) { return in, nil })
	if code, body, _ := pbCall(api, "/grid", `{"grid":[[1]]}`, "Content-Type", "application/json", "Accept", protoCT); code != 406 || !strings.Contains(body, "arrays of arrays") {
		t.Fatalf("%d %s", code, body)
	}
	if code, body, _ := pbCall(api, "/grid", "\x0a\x00", "Content-Type", protoCT); code != 415 {
		t.Fatalf("%d %s", code, body)
	}
	if code, body, _ := pbCall(api, "/grid", `{"grid":[[1]]}`, "Content-Type", "application/json"); code != 200 || body != `{"grid":[[1]]}` {
		t.Fatalf("JSON keeps working: %d %s", code, body)
	}
}

type pbPinned struct {
	Late  string `json:"late" proto:"7"`
	First string `json:"first"`
	Other string `json:"other"`
}

type pbPinnedIO struct {
	Body pbPinned
}

func TestProtobufFieldNumbersCanBePinned(t *testing.T) {
	api := server.New("t", "1")
	server.Post(api, "/p", func(_ context.Context, in *pbPinnedIO) (*pbPinnedIO, error) { return in, nil })
	code, wire, _ := pbCall(api, "/p", `{"late":"L","first":"F","other":"O"}`, "Content-Type", "application/json", "Accept", protoCT)
	if code != 200 || wire != "\x0a\x01F\x12\x01O\x3a\x01L" {
		t.Fatalf("explicit numbers are kept and the others fill 1, 2: %d %q", code, wire)
	}
}

func TestProtobufNumbersAreChecked(t *testing.T) {
	type dup struct {
		A string `json:"a" proto:"3"`
		B string `json:"b" proto:"3"`
	}
	type dupIO struct{ Body dup }
	mustPanic(t, "two fields with the same proto number", func() {
		server.Post(server.New("t", "1"), "/d", func(_ context.Context, in *dupIO) (*dupIO, error) { return in, nil })
	})
	type bad struct {
		A string `json:"a" proto:"19500"`
	}
	type badIO struct{ Body bad }
	mustPanic(t, "a number in the reserved range", func() {
		server.Post(server.New("t", "1"), "/d", func(_ context.Context, in *badIO) (*badIO, error) { return in, nil })
	})
}

func TestProtobufDeepNestingIsRefused(t *testing.T) {
	type node struct {
		Next *node `json:"next"`
	}
	type nodeIO struct{ Body node }
	api := server.New("t", "1")
	server.Post(api, "/n", func(_ context.Context, in *nodeIO) (*empty, error) { return nil, nil })
	wire := ""
	for range 500 {
		wire = "\x0a" + string(rune(len(wire))) + wire
		if len(wire) > 120 {
			break
		}
	}
	deep := ""
	for range 300 {
		deep = "\x0a" + varint(len(deep)) + deep
	}
	if code, body, _ := pbCall(api, "/n", deep, "Content-Type", protoCT); code != 400 {
		t.Fatalf("a message nested far beyond the limit must not recurse without bound: %d %.80s", code, body)
	}
}

func varint(n int) string {
	var b []byte
	for n >= 0x80 {
		b = append(b, byte(n)|0x80)
		n >>= 7
	}
	return string(append(b, byte(n)))
}

func TestProtoFileDescribesTheService(t *testing.T) {
	api := pbAPI()
	api.GRPCService = "acme.tiny.v1.Tiny"
	file := api.ProtoFile()
	for _, want := range []string{
		`syntax = "proto3";`, "package acme.tiny.v1;", "service Tiny {",
		"// POST /tiny", "rpc PostTiny(PostTinyRequest) returns (PostTinyResponse);",
		"message PostTinyRequest {", "  pbTiny body = 1;",
		"message pbTiny {", "  int32 a = 1;", "  repeated int32 c = 3;", "  optional string d = 4;",
	} {
		if !strings.Contains(file, want) {
			t.Errorf("the proto file lacks %q:\n%s", want, file)
		}
	}
	rec := call(api, "GET", "/api.proto", "")
	if rec.Code != 200 || rec.Body.String() != api.ProtoFile() {
		t.Fatalf("/api.proto: %d", rec.Code)
	}
}

type pbClash struct {
	A string `json:"a-b"`
	B int32  `json:"a_b"`
	C bool   `json:"a b"`
	D string `json:"A_B"`
}

type pbClashIO struct {
	Body pbClash
}

var protoFieldLine = regexp.MustCompile(`^\s+(?:optional |repeated )?\S+ (\w+) = \d+`)

func TestProtoFileFieldNamesStayUniqueWithinAMessage(t *testing.T) {
	api := server.New("t", "1")
	server.Post(api, "/c", func(_ context.Context, in *pbClashIO) (*pbClashIO, error) { return in, nil })
	file := api.ProtoFile()
	idents := map[string]int{}
	inMsg := false
	for _, line := range strings.Split(file, "\n") {
		switch {
		case strings.HasPrefix(line, "message pbClash {"):
			inMsg = true
		case line == "}":
			inMsg = false
		case inMsg:
			if m := protoFieldLine.FindStringSubmatch(line); m != nil {
				idents[strings.ToLower(m[1])]++
			}
		}
	}
	if len(idents) != 4 {
		t.Fatalf("four fields need four distinct identifiers, ignoring case:\n%s", file)
	}
	if !strings.Contains(file, `[json_name = "a-b"]`) || !strings.Contains(file, `[json_name = "A_B"]`) {
		t.Fatalf("the original JSON names are kept as json_name:\n%s", file)
	}
}

type pbBookIO struct {
	Body struct {
		Book map[string]pbAddr `json:"book"`
	}
}

func TestProtobufMapEntryWithoutAValueIsTheDefaultMessage(t *testing.T) {
	api := server.New("t", "1")
	server.Post(api, "/b", func(_ context.Context, in *pbBookIO) (*pbBookIO, error) { return in, nil })
	wire := "\x0a\x03\x0a\x01k"
	code, body, _ := pbCall(api, "/b", wire, "Content-Type", protoCT, "Accept", "application/json")
	if code != 200 || body != `{"book":{"k":{"city":"","zip":null}}}` {
		t.Fatalf("an omitted map value is an empty message whose required fields are zero: %d %s", code, body)
	}
}

type pbNames struct {
	UserA string `json:"user_id"`
	UserB string `json:"user-id"`
	UserC string `json:"userId"`
	Next  string `json:"next_cursor"`
	Plain string `json:"plain"`
}

type pbNamesIO struct {
	Body pbNames
}

func TestProtoFileJSONNamesFollowProtocsRules(t *testing.T) {
	api := server.New("t", "1")
	server.Post(api, "/n", func(_ context.Context, in *pbNamesIO) (*pbNamesIO, error) { return in, nil })
	file := api.ProtoFile()
	for _, want := range []string{
		`string user_id = 1 [json_name = "user_id"];`, `string user_id2 = 2 [json_name = "user-id"];`, `string userId3 = 3 [json_name = "userId"];`,
		`string next_cursor = 4 [json_name = "next_cursor"];`, `string plain = 5;`,
	} {
		if !strings.Contains(strings.ReplaceAll(file, "optional ", ""), want) {
			t.Errorf("missing %q in:\n%s", want, file)
		}
	}
}

func TestProtobufRepeatedMessageFieldsAreMerged(t *testing.T) {
	api := pbAPI()
	first := "\x22\x05\x0a\x01a\x10\x78"
	second := "\x22\x03\x0a\x01b"
	code, body, _ := pbCall(api, "/big", "\x12\x01n"+first+second+"\x62\x02{}", "Content-Type", protoCT, "Accept", "application/json")
	if code != 200 || !strings.Contains(body, `"addr":{"city":"b","zip":120}`) {
		t.Fatalf("two occurrences of a message field merge: the later city wins and the zip of the first stays: %d %s", code, body)
	}
}

func TestProtobufMapEntryNeedsValidFieldNumbers(t *testing.T) {
	api := pbAPI()
	for name, entry := range map[string]string{"number zero": "\x00\x01", "number too large": "\xf8\xff\xff\xff\xff\x01\x01"} {
		wire := "\x32" + string(rune(len(entry))) + entry
		if code, body, _ := pbCall(api, "/big", wire, "Content-Type", protoCT); code != 400 {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
}

type pbOpIO struct {
	Body struct {
		A string `json:"a"`
	}
}

func TestProtoFileNamesAreASCIIOnly(t *testing.T) {
	api := server.New("t", "1")
	server.Post(api, "/a", func(_ context.Context, in *pbOpIO) (*pbOpIO, error) { return in, nil }, func(o *server.Operation) { o.OperationID = "créer-ב-utilisateur" })
	file := api.ProtoFile()
	for i := 0; i < len(file); i++ {
		if file[i] > 0x7f && !strings.Contains(file[max(0, strings.LastIndex(file[:i], "\n")):i], "//") && !strings.Contains(file[max(0, strings.LastIndex(file[:i], "\n")):i], "json_name") {
			t.Fatalf("a method or message name has a character that protoc rejects:\n%s", file)
		}
	}
	if !strings.Contains(file, "rpc CrErUtilisateur(") {
		t.Fatalf("non-ASCII letters act as word breaks:\n%s", file)
	}
}
