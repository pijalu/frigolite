package fts

import (
	"reflect"
	"testing"
)

func TestMergeDoclistsDeduplicatesPostingsAcrossSegments(t *testing.T) {
	postings := make([]Posting, 0, 2000)
	for i := 0; i < 1000; i++ {
		postings = append(postings, Posting{DocID: 1, Column: 0, Position: i})
	}
	left := buildDoclist(postings)
	right := buildDoclist(postings)
	merged := MergeDoclists(left, right)
	var got []Posting
	if err := parseDoclistHits(merged, func(docID int64, col, pos int, deleted bool) {
		if !deleted {
			got = append(got, Posting{DocID: docID, Column: col, Position: pos})
		}
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, postings) {
		t.Fatalf("merged postings count=%d, want %d", len(got), len(postings))
	}
}

func TestMergeDoclistsDeleteMarkerThenNewPosting(t *testing.T) {
	prior := buildDoclist([]Posting{{DocID: 4, Column: 0, Position: 0}})
	deleted := buildDoclist([]Posting{{DocID: 4, Delete: true}})
	newer := buildDoclist([]Posting{{DocID: 4, Column: 0, Position: 1}})
	merged := MergeDoclists(prior, deleted, newer)
	var got []Posting
	if err := parseDoclistHits(merged, func(docID int64, col, pos int, isDelete bool) {
		got = append(got, Posting{DocID: docID, Column: col, Position: pos, Delete: isDelete})
	}); err != nil {
		t.Fatal(err)
	}
	want := []Posting{{DocID: 4, Column: 0, Position: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged postings = %#v, want %#v", got, want)
	}
}
