package server_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/swiftly-headquarters/minato/server"
)

type foldedIn struct {
	Body struct {
		Role string `json:"role" enum:"reader,writer"`
	}
}

func TestKeysThatDifferOnlyInCaseAreRejected(t *testing.T) {
	api := server.New("t", "1")
	server.Post(api, "/r", func(_ context.Context, _ *foldedIn) (*empty, error) { return nil, nil })
	if rec := call(api, "POST", "/r", `{"ROLE":"admin"}`, "Content-Type", "application/json"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a key that differs only in case must not skip validation: %d %s", rec.Code, rec.Body)
	}
	if rec := call(api, "POST", "/r", `{"role":"reader"}`, "Content-Type", "application/json"); rec.Code != http.StatusNoContent {
		t.Fatalf("the exact key still binds: %d %s", rec.Code, rec.Body)
	}
}

type sliceIn struct {
	Body struct {
		Qty   []int    `json:"qty" minimum:"1" maximum:"10"`
		Names []string `json:"names" maxLength:"3"`
	}
}

func TestScalarConstraintsApplyToSliceItems(t *testing.T) {
	api := server.New("t", "1")
	server.Post(api, "/s", func(_ context.Context, _ *sliceIn) (*empty, error) { return nil, nil })
	if rec := call(api, "POST", "/s", `{"qty":[5,1000],"names":["ab"]}`, "Content-Type", "application/json"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("maximum applies to each item: %d %s", rec.Code, rec.Body)
	}
	if rec := call(api, "POST", "/s", `{"qty":[1],"names":["abcd"]}`, "Content-Type", "application/json"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("maxLength applies to each item: %d %s", rec.Code, rec.Body)
	}
	if rec := call(api, "POST", "/s", `{"qty":[5,6],"names":["ab"]}`, "Content-Type", "application/json"); rec.Code != http.StatusNoContent {
		t.Fatalf("valid items pass: %d %s", rec.Code, rec.Body)
	}
}

type nullIn struct {
	Body struct {
		Tags  []string                  `json:"tags" enum:"red,blue"`
		Maybe []server.Nullable[string] `json:"maybe"`
	}
}

func TestNullItemsAreRejectedUnlessDeclaredNullable(t *testing.T) {
	api := server.New("t", "1")
	server.Post(api, "/n", func(_ context.Context, _ *nullIn) (*empty, error) { return nil, nil })
	if rec := call(api, "POST", "/n", `{"tags":[null],"maybe":[]}`, "Content-Type", "application/json"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a null item must not skip the enum: %d %s", rec.Code, rec.Body)
	}
	if rec := call(api, "POST", "/n", `{"tags":["red"],"maybe":[null,"x"]}`, "Content-Type", "application/json"); rec.Code != http.StatusNoContent {
		t.Fatalf("a nullable element type accepts null: %d %s", rec.Code, rec.Body)
	}
}

type capIn struct {
	Body struct {
		Tags []string `json:"tags" enum:"red,blue"`
	}
}

type maxIn struct {
	Body struct {
		Tags []string `json:"tags" maxItems:"3" enum:"red,blue"`
	}
}

func TestErrorResponsesStayBounded(t *testing.T) {
	api := server.New("t", "1")
	server.Post(api, "/many", func(_ context.Context, _ *capIn) (*empty, error) { return nil, nil })
	server.Post(api, "/max", func(_ context.Context, _ *maxIn) (*empty, error) { return nil, nil })
	items := strings.Repeat(`"green",`, 5000) + `"green"`
	rec := call(api, "POST", "/many", `{"tags":[`+items+`]}`, "Content-Type", "application/json")
	if rec.Code != http.StatusUnprocessableEntity || rec.Body.Len() > 30000 {
		t.Fatalf("errors per request are capped: %d, %d bytes", rec.Code, rec.Body.Len())
	}
	rec = call(api, "POST", "/max", `{"tags":[`+items+`]}`, "Content-Type", "application/json")
	if rec.Code != http.StatusUnprocessableEntity || rec.Body.Len() > 1000 {
		t.Fatalf("an array over maxItems is not echoed back: %d, %d bytes", rec.Code, rec.Body.Len())
	}
}

type numQueryIn struct {
	N int `query:"n"`
}

type trailIn struct {
	Body struct {
		Name string `json:"name"`
	}
}

func TestWhitespaceAroundANumberIsAValidationError(t *testing.T) {
	api := server.New("t", "1")
	server.Get(api, "/p", func(_ context.Context, _ *numQueryIn) (*empty, error) { return nil, nil })
	for _, q := range []string{"n=%201", "n=1%20", "n=%0A1"} {
		if rec := call(api, "GET", "/p?"+q, ""); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: %d %s", q, rec.Code, rec.Body)
		}
	}
}

func TestDataAfterTheJSONValueIsRejected(t *testing.T) {
	api := server.New("t", "1")
	server.Post(api, "/t", func(_ context.Context, _ *trailIn) (*empty, error) { return nil, nil })
	if rec := call(api, "POST", "/t", `{"name":"x"} {"name":"y"}`, "Content-Type", "application/json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("a second JSON value must not be silently ignored: %d", rec.Code)
	}
	if rec := call(api, "POST", "/t", `{"name":"x"}  `, "Content-Type", "application/json"); rec.Code != http.StatusNoContent {
		t.Fatalf("trailing whitespace is fine: %d %s", rec.Code, rec.Body)
	}
}

func TestRootSpecSecurityIsEnforcedUnlessAnOperationOptsOut(t *testing.T) {
	api := server.New("t", "1")
	keyScheme(api)
	api.Spec.Security = server.Secured("key")
	noop := func(ctx context.Context, _ *empty) (*empty, error) { return nil, nil }
	server.Get(api, "/private", noop)
	server.Get(api, "/open", noop, func(o *server.Operation) { o.Security = []map[string][]string{} })
	if rec := call(api, "GET", "/private", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the root security applies to operations that set none: %d", rec.Code)
	}
	if rec := call(api, "GET", "/private", "", "X-Key", "k1"); rec.Code != http.StatusNoContent {
		t.Fatalf("a valid key passes: %d", rec.Code)
	}
	if rec := call(api, "GET", "/open", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("an explicit empty security list opts out: %d", rec.Code)
	}
}
