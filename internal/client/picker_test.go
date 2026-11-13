package client

import (
	"io"
	"log/slog"
	"testing"
)

func testPicker(order ...string) *picker {
	return newPicker(order, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestPickerSingleCarrierNeverSwitches(t *testing.T) {
	p := testPicker("ws")
	for i := 0; i < 20; i++ {
		if p.report("ws", false) {
			t.Fatal("a single carrier must never switch")
		}
	}
	if c, _ := p.current(); c != "ws" || p.auto() {
		t.Fatal("unexpected state")
	}
}

func TestPickerMovesDownAfterConsecutiveFailuresAndWraps(t *testing.T) {
	p := testPicker("ws", "http", "chunk")
	_, ch := p.current()
	for i := 0; i < autoFailAfter-1; i++ {
		if p.report("ws", false) {
			t.Fatal("switched too early")
		}
	}
	if p.report("ws", true) { // a success resets the count
		t.Fatal("success must not switch")
	}
	for i := 0; i < autoFailAfter-1; i++ {
		p.report("ws", false)
	}
	if c, _ := p.current(); c != "ws" {
		t.Fatalf("a success must reset the failure count, now on %s", c)
	}
	if !p.report("ws", false) {
		t.Fatal("expected a switch")
	}
	if c, _ := p.current(); c != "http" {
		t.Fatalf("want http, got %s", c)
	}
	select {
	case <-ch:
	default:
		t.Fatal("the change channel was not closed")
	}
	if p.report("ws", false) { // a late result for the carrier we left
		t.Fatal("a late result for an old carrier must be ignored")
	}
	for i := 0; i < autoFailAfter; i++ {
		p.report("http", false)
	}
	for i := 0; i < autoFailAfter; i++ {
		p.report("chunk", false)
	}
	if c, _ := p.current(); c != "ws" {
		t.Fatalf("after chunk it must wrap to ws, got %s", c)
	}
}

func TestPickerPromoteOnlyUpward(t *testing.T) {
	p := testPicker("ws", "http", "chunk")
	for i := 0; i < 2*autoFailAfter; i++ {
		p.report(func() string { c, _ := p.current(); return c }(), false)
	}
	if c, _ := p.current(); c != "chunk" {
		t.Fatalf("want chunk, got %s", c)
	}
	if got := p.better(); len(got) != 2 || got[0] != "ws" || got[1] != "http" {
		t.Fatalf("better = %v", got)
	}
	p.promote("chunk") // not better than the current one: ignored
	if c, _ := p.current(); c != "chunk" {
		t.Fatal("promote to the current carrier must do nothing")
	}
	p.promote("http")
	if c, _ := p.current(); c != "http" {
		t.Fatalf("want http, got %s", c)
	}
}
