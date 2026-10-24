package diago

import "testing"

func TestLoopHeaderReadDoesNotExpandRetainedData(t *testing.T) {
	p, data, callback, slot := &loopVariable{}, &loopVariable{}, &loopVariable{}, &loopVariable{}
	w := &loopWalk{
		values: map[*loopVariable]loopValue{
			p:    {data: loopRead, callback: loopRetain | loopWrite<<loopCallableShift},
			slot: {data: loopHeaderRead},
		},
		aliases: map[*loopVariable]*loopVariable{p: slot},
	}
	s := &loopScanner{}
	for _, epoch := range []loopAccess{0, loopPriorRef, loopPriorRef | loopEpochMixedRef} {
		mode := loopHeaderRead | epoch
		got := s.expand(loopValue{p: mode}, w)
		if len(got) != 1 || got[p] != mode {
			t.Fatalf("header read followed a binding or lost its epoch: %v", got)
		}
		latent := loopRetain | loopHeaderRead<<loopCallableShift | epoch
		active := loopActivateCallableMode(latent)
		if active != loopRetain|mode || active&loopRead != 0 {
			t.Fatalf("header activation acquired a data-read bit or lost metadata: %#x", active)
		}
	}
	// Header bits must never become evidence of an actual addressed root.
	if s.knownInvocationAlias(p, data, w) {
		t.Fatal("a physical header read was mistaken for address provenance")
	}
	ordinary := s.expand(loopValue{p: loopRead}, w)
	if ordinary[data]&loopRead == 0 || ordinary[callback]&loopWrite != 0 {
		t.Fatalf("ordinary retained-data reads changed semantics: %v", ordinary)
	}
}

func TestLoopHeaderEpochCapturedSlot(t *testing.T) {
	source := loopMethodHeaderPrelude + `func work(xs []int) {
		var f func()
		for _, p := range []*item{{},{}} {
			if f != nil { go f() }
			f = func(){ _=p.Use==nil }
			p=&item{}
			_=p
		}
	}`
	for _, version := range []string{"go1.21", "go1.23"} {
		want := 0
		if version == "go1.21" {
			want = 1
		}
		for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
			t.Run(version+"/"+mode, func(t *testing.T) {
				got := analyzeLoopRegression(t, source, version, mode)
				if n := countRules(got)["goroutine-in-loop"]; n != want {
					t.Fatalf("got %d findings, want %d: %+v", n, want, got)
				}
			})
		}
	}
}
