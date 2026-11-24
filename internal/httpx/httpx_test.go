package httpx

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecode(t *testing.T) {
	for body, want := range map[string]int{
		`{"id":"ok"}`:            200,
		``:                       200, // an empty body is {}
		`{"id":"ok"} {"x":true}`: 400, // a second top-level value
		`{"id":"ok"} garbage`:    400,
		`{"id":"ok","extra":1}`:  400,
		`{"id":"` + strings.Repeat("x", 70<<10) + `"}`: 413,
	} {
		rec := httptest.NewRecorder()
		var v struct{ ID string }
		if Decode(rec, httptest.NewRequest("POST", "/", strings.NewReader(body)), &v) {
			rec.WriteHeader(200)
		}
		if rec.Code != want {
			t.Errorf("%.40q: %d, want %d", body, rec.Code, want)
		}
	}
}

// DecodeLimit reads up to its own limit, and names it when refusing.
func TestDecodeLimit(t *testing.T) {
	for n, want := range map[int]int{70 << 10: 200, 300 << 10: 413} {
		rec := httptest.NewRecorder()
		var v struct{ ID string }
		body := `{"id":"` + strings.Repeat("x", n) + `"}`
		if DecodeLimit(rec, httptest.NewRequest("POST", "/", strings.NewReader(body)), &v, 256<<10) {
			rec.WriteHeader(200)
		}
		if rec.Code != want {
			t.Errorf("%d bytes: %d, want %d", n, rec.Code, want)
		}
		if want == 413 && !strings.Contains(rec.Body.String(), "limited to 256 KB") {
			t.Errorf("413 says %s", rec.Body)
		}
	}
}
