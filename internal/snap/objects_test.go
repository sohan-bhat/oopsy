package snap

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveVersionsSwapsBackAndDedupes(t *testing.T) {
	work := testEnv(t)
	f := filepath.Join(work, "a.txt")
	old := time.Date(2021, 5, 6, 7, 8, 9, 0, time.UTC)
	os.Chmod(f, 0o640)
	os.Chtimes(f, old, old)
	s1, err := SaveVersions("Edit a.txt", work, "claude", []string{f, filepath.Join(work, "new.txt")})
	if err != nil || s1 == nil || len(s1.Entries) != 1 {
		t.Fatalf("SaveVersions: %v %+v", err, s1)
	}
	s2, _ := SaveVersions("Edit a.txt again", work, "claude", []string{f})
	if s1.Entries[0].Store != s2.Entries[0].Store || s2.Entries[0].Disk != 0 {
		t.Fatalf("identical versions were stored twice: %+v %+v", s1.Entries[0], s2.Entries[0])
	}
	os.WriteFile(f, []byte("edited by the agent\n"), 0o644)
	if err := SetAfter(s1.ID); err != nil {
		t.Fatal(err)
	}
	s1, _ = Load(s1.ID)
	out, err := Restore(s1)
	if err != nil || len(out) != 1 || out[0].Status != "restored" {
		t.Fatalf("restore: %v %+v", err, out)
	}
	b, _ := os.ReadFile(f)
	fi, _ := os.Stat(f)
	if string(b) != "alpha\n" || !fi.ModTime().Equal(old) || (os.PathSeparator == '/' && fi.Mode().Perm() != 0o640) {
		t.Fatalf("restored %q %v %v", b, fi.ModTime(), fi.Mode())
	}
	if ents, _ := os.ReadDir(filepath.Join(s1.Dir(), "replaced")); len(ents) != 1 {
		t.Fatal("the agent's version was not kept in the snapshot")
	}
}

func TestRecordsRestoreThroughTheirKind(t *testing.T) {
	work := testEnv(t)
	RegisterRecordKind("test-thing", RecordKind{Restore: func(s *Snapshot, r Record) Outcome {
		b, _ := os.ReadFile(filepath.Join(s.RecordDir(r), "state"))
		return Outcome{Path: r.Label, Status: "record", Detail: string(b)}
	}})
	s, err := Create("thing delete x", work, "zsh", nil, func(dir string) (*Record, error) {
		return &Record{Kind: "test-thing", Label: "thing x"}, os.WriteFile(filepath.Join(dir, "state"), []byte("v1"), 0o600)
	})
	if err != nil || s == nil || len(s.Records) != 1 {
		t.Fatalf("%v %+v", err, s)
	}
	out, _ := Restore(s)
	if len(out) != 1 || out[0].Detail != "v1" {
		t.Fatalf("%+v", out)
	}
}
