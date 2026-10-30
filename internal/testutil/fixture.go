// Package testutil builds synthetic AO3-style EPUBs and archive zips so the
// indexer, reader and Docker image can be exercised without the real data.
package testutil

import (
	"archive/zip"
	"bytes"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
)

// FakeWork describes one synthetic fanwork.
type FakeWork struct {
	ID            int64
	Title         string
	Author        string
	Fandom        string
	Rating        string
	Relationships []string
	Characters    []string
	Freeforms     []string
	Summary       string
	Published     string
	Updated       string
	Words         int
	Chapters      []string // chapter bodies (plain text paragraphs)
	WithImage     bool
}

// tinyPNG is a 1x1 transparent PNG.
var tinyPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
	0x42, 0x60, 0x82,
}

func e(s string) string { return html.EscapeString(s) }

func links(tags []string) string {
	parts := make([]string, len(tags))
	for i, t := range tags {
		parts[i] = fmt.Sprintf(`<a class="tag" href="http://archiveofourown.org/tags/%s">%s</a>`, e(t), e(t))
	}
	return strings.Join(parts, ", ")
}

// EPUB renders a FakeWork as an EPUB shaped like AO3's own downloads:
// a preface page with the tag list, then one file per chapter.
func EPUB(w FakeWork) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	put := func(name string, data []byte, method uint16) {
		f, _ := zw.CreateHeader(&zip.FileHeader{Name: name, Method: method})
		f.Write(data)
	}
	put("mimetype", []byte("application/epub+zip"), zip.Store)
	put("META-INF/container.xml", []byte(`<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
<rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`), zip.Deflate)

	chapters := fmt.Sprintf("%d/%d", len(w.Chapters), len(w.Chapters))
	preface := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE html><html xmlns="http://www.w3.org/1999/xhtml"><head><title>%s</title></head><body>
<div class="meta">
<p class="message">This work could have adult content. Posted originally on the <a href="http://archiveofourown.org/">Archive of Our Own</a> at <a href="http://archiveofourown.org/works/%d">http://archiveofourown.org/works/%d</a>.</p>
<dl class="tags">
<dt>Rating:</dt><dd>%s</dd>
<dt>Archive Warning:</dt><dd><a href="#">No Archive Warnings Apply</a></dd>
<dt>Category:</dt><dd><a href="#">Gen</a></dd>
<dt>Fandom:</dt><dd>%s</dd>
<dt>Relationships:</dt><dd>%s</dd>
<dt>Characters:</dt><dd>%s</dd>
<dt>Additional Tags:</dt><dd>%s</dd>
<dt>Language:</dt><dd>English</dd>
<dt>Stats:</dt><dd>Published: %s Updated: %s Words: %s Chapters: %s</dd>
</dl>
<h1>%s</h1>
<div class="byline">by <a rel="author" href="http://archiveofourown.org/users/%s/pseuds/%s">%s</a></div>
<p>Summary</p>
<blockquote class="userstuff"><p>%s</p></blockquote>
</div></body></html>`,
		e(w.Title), w.ID, w.ID, links([]string{w.Rating}), links([]string{w.Fandom}), links(w.Relationships),
		links(w.Characters), links(w.Freeforms), w.Published, w.Updated, commafy(w.Words), chapters,
		e(w.Title), e(w.Author), e(w.Author), e(w.Author), e(w.Summary))
	put("OEBPS/preface.xhtml", []byte(preface), zip.Deflate)

	var manifest, spine, nav strings.Builder
	manifest.WriteString(`<item id="preface" href="preface.xhtml" media-type="application/xhtml+xml"/>`)
	spine.WriteString(`<itemref idref="preface"/>`)
	nav.WriteString(`<navPoint id="n0" playOrder="0"><navLabel><text>Preface</text></navLabel><content src="preface.xhtml"/></navPoint>`)
	for i, body := range w.Chapters {
		name := fmt.Sprintf("chapter_%03d.xhtml", i+1)
		img := ""
		if w.WithImage && i == 0 {
			img = `<p><img src="images/pic.png" alt="a picture"/></p>`
		}
		next := ""
		if i+1 < len(w.Chapters) {
			next = fmt.Sprintf(`<p><a href="chapter_%03d.xhtml#top">Next chapter</a></p>`, i+2)
		}
		page := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE html><html xmlns="http://www.w3.org/1999/xhtml"><head><title>Chapter %d</title>
<script>alert("x")</script></head><body>
<h2 id="top">Chapter %d</h2>%s<div class="userstuff"><p>%s</p><p onclick="evil()">More text.</p></div>%s
</body></html>`, i+1, i+1, img, e(body), next)
		put("OEBPS/"+name, []byte(page), zip.Deflate)
		fmt.Fprintf(&manifest, `<item id="c%d" href="%s" media-type="application/xhtml+xml"/>`, i+1, name)
		fmt.Fprintf(&spine, `<itemref idref="c%d"/>`, i+1)
		fmt.Fprintf(&nav, `<navPoint id="n%d" playOrder="%d"><navLabel><text>Chapter %d</text></navLabel><content src="%s"/></navPoint>`, i+1, i+1, i+1, name)
	}
	if w.WithImage {
		put("OEBPS/images/pic.png", tinyPNG, zip.Store)
		manifest.WriteString(`<item id="img" href="images/pic.png" media-type="image/png"/>`)
	}
	manifest.WriteString(`<item id="ncx" href="toc.ncx" media-type="application/x-dtbncx+xml"/>`)

	put("OEBPS/toc.ncx", []byte(`<?xml version="1.0"?><ncx xmlns="http://www.daisy.org/z3986/2005/ncx/" version="2005-1"><navMap>`+nav.String()+`</navMap></ncx>`), zip.Deflate)
	put("OEBPS/content.opf", []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0" unique-identifier="id">
<metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
<dc:title>%s</dc:title><dc:creator>%s</dc:creator><dc:language>en</dc:language>
<dc:identifier id="id">urn:uuid:%d</dc:identifier><dc:publisher>Archive of Our Own</dc:publisher>
</metadata>
<manifest>%s</manifest>
<spine toc="ncx">%s</spine>
</package>`, e(w.Title), e(w.Author), w.ID, manifest.String(), spine.String())), zip.Deflate)
	zw.Close()
	return buf.Bytes()
}

func commafy(n int) string {
	s := fmt.Sprint(n)
	var out []byte
	for i := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	return string(out)
}

// Sample returns a small, varied set of works.
func Sample() []FakeWork {
	fandoms := []string{"Harry Potter - J. K. Rowling", "Star Wars - All Media Types", "Naruto", "Sherlock (TV)", "Good Omens (TV)"}
	ratings := []string{"General Audiences", "Teen And Up Audiences", "Mature", "Explicit", "Not Rated"}
	titles := []string{
		"The Long Way Home", "Coffee Shop Encounters", "Stars Over Hogwarts", "A Study in Tea", "Ineffable Mornings",
		"Café Déjà Vu", "Ten Thousand Miles", "What the Rain Knows", "Library After Dark", "Fix-It Fic",
	}
	var out []FakeWork
	for i := 0; i < 50; i++ {
		f := fandoms[i%len(fandoms)]
		n := 1 + i%4
		chs := make([]string, n)
		for c := range chs {
			chs[c] = fmt.Sprintf("Chapter %d of work %d. It was a quiet evening in the %s universe.", c+1, 1000+i, f)
		}
		out = append(out, FakeWork{
			ID:            int64(1000 + i),
			Title:         fmt.Sprintf("%s %d", titles[i%len(titles)], i),
			Author:        fmt.Sprintf("writer%02d", i%7),
			Fandom:        f,
			Rating:        ratings[i%len(ratings)],
			Relationships: []string{"Character A/Character B"},
			Characters:    []string{"Character A", "Character B"},
			Freeforms:     []string{"Fluff", []string{"Angst", "Hurt/Comfort", "Alternate Universe - Coffee Shops & Cafés"}[i%3]},
			Summary:       fmt.Sprintf("Summary for work number %d. Somebody finds something.", 1000+i),
			Published:     fmt.Sprintf("20%02d-0%d-1%d", 15+i%8, 1+i%9, i%10),
			Updated:       fmt.Sprintf("20%02d-0%d-1%d", 16+i%8, 1+i%9, i%10),
			Words:         1000 * (i + 1),
			Chapters:      chs,
			WithImage:     i%10 == 0,
		})
	}
	return out
}

// WriteArchive writes works into a zip at path, the way the torrent's
// queueNN.zip files hold EPUBs. method is zip.Store or zip.Deflate.
func WriteArchive(path string, works []FakeWork, method uint16, nameFn func(FakeWork) string) error {
	if nameFn == nil {
		nameFn = func(w FakeWork) string { return fmt.Sprintf("%d.epub", w.ID) }
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for _, w := range works {
		h := &zip.FileHeader{Name: nameFn(w), Method: method}
		out, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		if _, err := out.Write(EPUB(w)); err != nil {
			return err
		}
	}
	// A non-EPUB member, to exercise the "skip" path.
	if out, err := zw.Create("readme.txt"); err == nil {
		out.Write([]byte("not an epub"))
	}
	return zw.Close()
}

// WriteSampleRarDir builds a data directory the way Real-Debrid serves a
// multi-file torrent: a single uncompressed things.rar holding the zips.
func WriteSampleRarDir(dir string) error {
	tmp, err := os.MkdirTemp("", "ao3-rar")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := WriteSampleDataDir(tmp); err != nil {
		return err
	}
	var files []RarFile
	for _, name := range []string{"queue01.zip", "jan_may_2023.zip", "zip_order.txt"} {
		data, err := os.ReadFile(filepath.Join(tmp, name))
		if err != nil {
			return err
		}
		files = append(files, RarFile{Name: "things/" + name, Data: data})
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return WriteRar5(filepath.Join(dir, "things.rar"), files)
}

// WriteSampleDataDir builds a data directory with two archives in which
// the second one carries an updated copy of one work from the first.
func WriteSampleDataDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	works := Sample()
	if err := WriteArchive(filepath.Join(dir, "queue01.zip"), works[:30], zip.Store, nil); err != nil {
		return err
	}
	upd := append([]FakeWork{}, works[30:]...)
	newer := works[0]
	newer.Title = "The Long Way Home (Revised)"
	newer.Updated = "2025-12-31"
	upd = append(upd, newer)
	if err := WriteArchive(filepath.Join(dir, "jan_may_2023.zip"), upd, zip.Deflate, func(w FakeWork) string {
		return fmt.Sprintf("works/%s_%d.epub", strings.ReplaceAll(w.Title, " ", "_"), w.ID)
	}); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "zip_order.txt"), []byte("queue01.zip\njan_may_2023.zip\n"), 0o644)
}
