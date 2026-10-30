package epub_test

import (
	"strings"
	"testing"

	"github.com/mihai-max/ao3/internal/epub"
	"github.com/mihai-max/ao3/internal/testutil"
)

func TestWorkMetaFromAO3Preface(t *testing.T) {
	w := testutil.Sample()[3]
	b, err := epub.Open(testutil.EPUB(w))
	if err != nil {
		t.Fatal(err)
	}
	m := b.WorkMeta()
	if !m.HasPreface {
		t.Fatal("preface not detected")
	}
	if m.WorkID != w.ID {
		t.Errorf("WorkID = %d, want %d", m.WorkID, w.ID)
	}
	if m.Title != w.Title {
		t.Errorf("Title = %q, want %q", m.Title, w.Title)
	}
	if len(m.Authors) != 1 || m.Authors[0] != w.Author {
		t.Errorf("Authors = %q", m.Authors)
	}
	if len(m.Fandoms) != 1 || m.Fandoms[0] != w.Fandom {
		t.Errorf("Fandoms = %q", m.Fandoms)
	}
	if len(m.Rating) != 1 || m.Rating[0] != w.Rating {
		t.Errorf("Rating = %q", m.Rating)
	}
	if strings.Join(m.Characters, "|") != "Character A|Character B" {
		t.Errorf("Characters = %q", m.Characters)
	}
	if m.Words != int64(w.Words) {
		t.Errorf("Words = %d, want %d", m.Words, w.Words)
	}
	if m.Published != w.Published || m.Updated != w.Updated {
		t.Errorf("dates = %s %s", m.Published, m.Updated)
	}
	if m.Chapters != "4/4" || !m.Complete {
		t.Errorf("Chapters = %q complete=%v", m.Chapters, m.Complete)
	}
	if m.Summary != w.Summary {
		t.Errorf("Summary = %q", m.Summary)
	}
	if m.Language != "English" {
		t.Errorf("Language = %q", m.Language)
	}
}

func TestSpineAndTOC(t *testing.T) {
	w := testutil.Sample()[0] // has an image, 1 chapter
	b, err := epub.Open(testutil.EPUB(w))
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Spine) != 2 {
		t.Fatalf("spine len = %d", len(b.Spine))
	}
	titles := b.ChapterTitles()
	if titles[0] != "Preface" || titles[1] != "Chapter 1" {
		t.Errorf("titles = %q", titles)
	}
	if !b.Has("OEBPS/images/pic.png") || b.MediaType("OEBPS/images/pic.png") != "image/png" {
		t.Error("image missing from manifest")
	}
}

func TestResolve(t *testing.T) {
	cases := []struct {
		from, href, full, frag string
		ok                     bool
	}{
		{"OEBPS/text/a.xhtml", "b.xhtml#x", "OEBPS/text/b.xhtml", "x", true},
		{"OEBPS/text/a.xhtml", "../images/p%20q.png", "OEBPS/images/p q.png", "", true},
		{"OEBPS/a.xhtml", "#note", "OEBPS/a.xhtml", "note", true},
		{"OEBPS/a.xhtml", "https://example.com/", "", "", false},
		{"OEBPS/a.xhtml", "../../../etc/passwd", "etc/passwd", "", true},
	}
	for _, c := range cases {
		full, frag, ok := epub.Resolve(c.from, c.href)
		if full != c.full || frag != c.frag || ok != c.ok {
			t.Errorf("Resolve(%q,%q) = %q,%q,%v", c.from, c.href, full, frag, ok)
		}
	}
}

func TestWorkIDFromName(t *testing.T) {
	for name, want := range map[string]int64{
		"12345.epub":                  12345,
		"works/Some_Title_98765.epub": 98765,
		"no-number.epub":              0,
		"Title 2020.epub":             0,
		"Title-55555.EPUB":            55555,
	} {
		if got := epub.WorkIDFromName(name); got != want {
			t.Errorf("%s: got %d want %d", name, got, want)
		}
	}
}

func TestNotAnEPUB(t *testing.T) {
	if _, err := epub.Open([]byte("hello")); err == nil {
		t.Fatal("expected error")
	}
}
