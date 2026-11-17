package e2e

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// otherPortRanges are the ports that the tests of other packages bind, and
// that run at the same time as these (go test ./... runs the packages side by
// side): the instance tests take 24000+n*20 and 25000+n*20 (about 70 services
// reach 26438), the doctor tests 26000 to 26012, the page tests 33000+n*20 and
// 34000+n*20 (and what follows). The ports of this package stay clear of them.
var otherPortRanges = []struct {
	who    string
	lo, hi int
}{
	{"the instance tests", 24000, 26438},
	{"the doctor tests", 26000, 26012},
	{"the page tests", 33000, 65535},
}

// otherPortBases are the bases of the blocks that other packages' tests take
// as base+n*20 (the page tests also write the last port of a block as
// base+9+n*20), which otherPortRanges account for.
var otherPortBases = map[int]bool{24000: true, 25000: true, 33000: true, 33009: true, 34000: true, 34009: true}

// The ports of the services of these tests are 20 for each, game ports and
// query ports together, one block after the other from portBase, and all of
// them below the ephemeral range of Linux (32768 and above), where the client
// sockets of the tests and the 127.0.0.1:0 listeners of the services are given
// theirs and now and then take a port that a service was given by the config:
// a start then fails with port_in_use. They are also clear of the ranges of
// other packages' tests, and a run that has more services than fit is refused
// by the test that asks for the first that does not (nextPorts).
func TestPortBlocksStayBelowTheEphemeralRangeAndApartFromOthers(t *testing.T) {
	if maxPortBlocks != 287 {
		t.Errorf("%d blocks of ports fit between %d and %d, the documented number is 287", maxPortBlocks, portBase, portCeiling)
	}
	prevEnd := portBase - 1
	for n := 1; n <= maxPortBlocks; n++ {
		game, query := portsOfBlock(n)
		if query != game+10 {
			t.Fatalf("block %d: game %d, query %d: the query ports follow the 10 game ports", n, game, query)
		}
		if game <= prevEnd {
			t.Fatalf("block %d starts at %d, the one before ended at %d", n, game, prevEnd)
		}
		end := query + 9
		prevEnd = end
		if game < portBase || end >= portCeiling || !portBlockFits(n) {
			t.Fatalf("block %d, ports %d to %d, is not below %d", n, game, end, portCeiling)
		}
		for _, o := range otherPortRanges {
			if game <= o.hi && end >= o.lo {
				t.Fatalf("block %d, ports %d to %d, is in the range of %s (%d to %d)", n, game, end, o.who, o.lo, o.hi)
			}
		}
	}
	// The pool gives no more than fit, and the first block that does not fit is
	// refused, not given.
	if portBlockFits(maxPortBlocks + 1) {
		t.Errorf("block %d fits, and ends at %d", maxPortBlocks+1, func() int { _, q := portsOfBlock(maxPortBlocks + 1); return q + 9 }())
	}
	if !portBlockFits(1) || !portBlockFits(maxPortBlocks) {
		t.Error("the first or the last block does not fit")
	}
}

// Another package that takes ports as base+n*20 in its tests has a base that
// otherPortRanges knows, or this fails: the range of a new base has to be put
// there, and kept clear of.
func TestPortBasesOfOtherPackagesAreKnown(t *testing.T) {
	re := regexp.MustCompile(`\b(\d{4,5})\s*\+\s*blk\s*\*\s*20\b`)
	found := map[string]bool{}
	err := filepath.WalkDir(filepath.Join("..", ".."), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || filepath.Base(p) == "e2e" && filepath.Base(filepath.Dir(p)) == "internal" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			base, _ := strconv.Atoi(m[1])
			found[p+" "+m[1]] = true
			if !otherPortBases[base] {
				t.Errorf("%s takes ports from %d+n*20, which otherPortRanges does not know of: add its range there, and keep these tests clear of it", p, base)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) < len(otherPortBases) {
		t.Errorf("found the bases in %d places, want at least %d (%v): has a package changed how it takes its ports?", len(found), len(otherPortBases), found)
	}
}

// The pool gives each block to one test at a time, the one given back longest
// ago first, and gives no more than there are: so a run that repeats the tests
// (-count=10) needs no more blocks than the tests that live at once.
func TestPortBlockPool(t *testing.T) {
	p := &blockPool{max: 3}
	var got []int
	for range 3 {
		n, ok := p.take()
		if !ok {
			t.Fatal("the pool is empty before its blocks are given")
		}
		got = append(got, n)
	}
	if got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("blocks given: %v", got)
	}
	if n, ok := p.take(); ok {
		t.Fatalf("a fourth block %d from a pool of three", n)
	}
	p.give(2)
	p.give(1)
	if n, ok := p.take(); !ok || n != 2 {
		t.Fatalf("the block given back first is given first: %d %v", n, ok)
	}
	if n, ok := p.take(); !ok || n != 1 {
		t.Fatalf("then the next: %d %v", n, ok)
	}
	if _, ok := p.take(); ok {
		t.Fatal("a block was given twice")
	}
	// The pool of the services of these tests, with all blocks taken and given
	// back many times over, as in a run of -count=10: never more than maxPortBlocks
	// blocks, and the same ports for the same block.
	all := &blockPool{max: maxPortBlocks}
	for round := 0; round < 12; round++ {
		var held []int
		for i := 0; i < 40; i++ {
			n, ok := all.take()
			if !ok || !portBlockFits(n) {
				t.Fatalf("round %d, block %d: %d %v", round, i, n, ok)
			}
			held = append(held, n)
		}
		for _, n := range held {
			all.give(n)
		}
	}
	if all.next > 40 {
		t.Errorf("40 services at once took %d blocks of the pool", all.next)
	}
}
