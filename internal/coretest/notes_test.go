package coretest_test

import (
	"testing"

	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// The notes (coretest.NoteApp): the test events most of these tests
// subscribe conversations to and emit, by the names these tests use.
type (
	caller   = coretest.Caller
	addition = coretest.Addition
	noteSub  = coretest.NoteSub
	subOpts  = coretest.SubOpts
	note     = coretest.Note
)

var (
	subscribe      = coretest.Subscribe
	unsubscribe    = coretest.Unsubscribe
	emitNote       = coretest.EmitNote
	emitNoteCtx    = coretest.EmitNoteCtx
	annotate       = coretest.Annotate
	noteStatus     = coretest.NoteStatus
	subStatus      = coretest.SubStatus
	notePublishURL = coretest.NotePublishURL
	endNote        = coretest.EndNote
	defineNotes    = coretest.DefineNotes
)

func noteApp(t *testing.T, c *coretest.Core, id string) *wakeshift.Client {
	t.Helper()
	return c.NoteApp(id)
}
func bareApp(t *testing.T, c *coretest.Core, id string) *wakeshift.Client {
	t.Helper()
	return c.BareApp(id)
}
func noteRow(t *testing.T, c *coretest.Core, id string) (tenant, sub string, live bool) {
	t.Helper()
	return c.NoteRow(t, id)
}
func noteLive(t *testing.T, c *coretest.Core, id string) bool { t.Helper(); return c.NoteLive(t, id) }
