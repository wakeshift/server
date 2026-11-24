package auth

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

// A token endpoint's refusal is logged by its code and status, never with
// the provider's response body (oauth2.RetrieveError quotes it).
func TestLogErrDropsTheProvidersBody(t *testing.T) {
	body := "<html>secret-ish provider page " + strings.Repeat("x", 500) + "</html>"
	err := fmt.Errorf("exchange: %w", &oauth2.RetrieveError{Response: &http.Response{StatusCode: 502}, Body: []byte(body)})
	if !strings.Contains(err.Error(), "provider page") {
		t.Fatal("the test's error doesn't quote the body; nothing to check")
	}
	got := logErr(err).Error()
	if strings.Contains(got, "provider page") || !strings.Contains(got, "502") {
		t.Fatalf("logged %q", got)
	}
	plain := fmt.Errorf("state mismatch")
	if logErr(plain) != plain {
		t.Fatal("another error changed")
	}
}
