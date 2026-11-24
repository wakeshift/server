package appapi

import (
	"strings"
	"testing"
)

// A voucher is wsv_ and 32 lowercase base32 characters, random each time;
// the format check accepts exactly that.
func TestVoucherFormat(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		v := newVoucher()
		if !voucherRE.MatchString(v) || len(v) != 36 || seen[v] {
			t.Fatalf("voucher %q", v)
		}
		seen[v] = true
	}
	for _, bad := range []string{"", "wsv_", "wsv_" + strings.Repeat("a", 31), "wsv_" + strings.Repeat("a", 33), "WSV_" + strings.Repeat("a", 32),
		"wsv_" + strings.Repeat("A", 32), "wsv_" + strings.Repeat("1", 32), "wsc_" + strings.Repeat("a", 32), "wsv_" + strings.Repeat("a", 32) + "\n"} {
		if voucherRE.MatchString(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}

// At most voucherPerHost anonymous verifications run at once per callback
// host, whatever the port, path or case; a freed slot can be taken again.
func TestVoucherHostSlots(t *testing.T) {
	a := &API{}
	var done []func()
	for i := range voucherPerHost {
		d, ok := a.hostSlot("https://Hooks.Example.com:" + string(rune('1'+i)) + "443/p")
		if !ok {
			t.Fatalf("slot %d refused", i)
		}
		done = append(done, d)
	}
	if _, ok := a.hostSlot("https://hooks.example.com/other"); ok {
		t.Fatal("a fifth slot for the host")
	}
	if d, ok := a.hostSlot("https://other.example.com/"); !ok {
		t.Fatal("another host refused")
	} else {
		d()
	}
	done[0]()
	if d, ok := a.hostSlot("https://hooks.example.com/"); !ok {
		t.Fatal("a freed slot refused")
	} else {
		d()
	}
	for _, d := range done[1:] {
		d()
	}
	if len(hostSlots.n) != 0 {
		t.Fatalf("slots left: %v", hostSlots.n)
	}
}
