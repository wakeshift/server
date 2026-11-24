package appapi

import (
	"strings"
	"testing"

	"github.com/wakeshift/server/internal/beacon"
)

// The trailer the SDKs add as a text item of its own starts on a line of
// its own however a harness joins the items: Copilot CLI hands its hooks
// the items joined with nothing between them, and its hook found no beacon
// ("…the same way.(For Wakeshift's hook…)"), so it never connected.
func TestSplitTrailerStartsALine(t *testing.T) {
	const tok = "aaaaaaaaaa.bbbbbbbbbb.cccccccccc"
	note, trailer := splitTrailer("Run this command now.\n\n" + beacon.Text(tok))
	if note != "Run this command now." {
		t.Fatalf("note %q", note)
	}
	if !strings.HasPrefix(trailer, "\n") || strings.TrimSpace(trailer) != beacon.Text(tok) {
		t.Fatalf("trailer %q", trailer)
	}
	// Copilot CLI's join: the app's text, Wakeshift's, then the trailer.
	if got := beacon.Trailer("Watching bench events." + note + trailer); got != tok {
		t.Fatalf("Copilot's joined text: beacon %q", got)
	}
	// A client that shows each item, or joins them with blank lines, is unchanged.
	if got := beacon.Trailer(note + "\n\n" + trailer); got != tok {
		t.Fatalf("joined with blank lines: beacon %q", got)
	}
	if _, trailer := splitTrailer("No beacon here."); trailer != "" {
		t.Fatalf("a trailer without a beacon: %q", trailer)
	}
}

// withBeacon puts beacon.Guard just before a text beacon: it stays in what
// the model reads (the note), and the trailer is the beacon's line alone,
// still the result's last.
func TestGuardStaysInTheNote(t *testing.T) {
	const tok = "aaaaaaaaaa.bbbbbbbbbb.cccccccccc"
	note, trailer := splitTrailer("Run this command now.\n\n" + beacon.Guard + "\n" + beacon.Text(tok))
	if note != "Run this command now.\n\n"+beacon.Guard {
		t.Fatalf("note %q", note)
	}
	if strings.TrimSpace(trailer) != beacon.Text(tok) || beacon.Trailer(note+trailer) != tok {
		t.Fatalf("trailer %q", trailer)
	}
	// A hook that takes the beacon out leaves the guard, which still reads right.
	if got := beacon.StripText(note + trailer); got != note {
		t.Fatalf("stripped %q", got)
	}
}
