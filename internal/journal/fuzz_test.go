package journal

import (
	"testing"
)

// FuzzObserverSplits relays arbitrary response bytes and requires a
// schema-valid record that does not depend on how the bytes were split
// across reads, and that a failed client write never panics.
func FuzzObserverSplits(f *testing.F) {
	for _, c := range loadTranscripts(f) {
		if len(c.Response) <= 8<<10 {
			f.Add(latin1(f, c.Response), uint16(len(c.Response)/2))
		}
	}
	schema := loadSchema(f)
	f.Fuzz(func(t *testing.T, wire []byte, k uint16) {
		j := testJournal(t)
		whole := observeReads(j, chatRequest, [][]byte{wire}, EndEOF)
		if err := schema.validateRecord(recordObject(t, whole)); err != nil {
			t.Fatalf("record fails the schema: %v\n%s", err, comparable(whole))
		}
		split := int(k) % (len(wire) + 1)
		if got := observeReads(j, chatRequest, [][]byte{wire[:split], wire[split:]}, EndEOF); comparable(got) != comparable(whole) {
			t.Fatalf("split %d:\n got %s\nwant %s", split, comparable(got), comparable(whole))
		}
		bytewise := make([][]byte, len(wire))
		for i := range wire {
			bytewise[i] = wire[i : i+1]
		}
		if got := observeReads(j, chatRequest, bytewise, EndEOF); comparable(got) != comparable(whole) {
			t.Fatalf("bytewise:\n got %s\nwant %s", comparable(got), comparable(whole))
		}
		o := j.begin(chatRequest, testStart)
		rec, ok := failWrite(o, wire, split, at(20), at(100))
		if !ok {
			t.Fatal("failed write did not finalize")
		}
		if err := schema.validateRecord(recordObject(t, rec)); err != nil {
			t.Fatalf("failed-write record fails the schema: %v\n%s", err, comparable(rec))
		}
	})
}
