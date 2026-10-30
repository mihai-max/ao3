// Package epub reads the parts of an EPUB that the indexer and reader need:
// the package document (OPF) metadata, the manifest, the spine and the table
// of contents. It is deliberately lenient, because scraped archives contain
// plenty of slightly broken files.
package epub

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// MaxPartSize caps how much of a single file inside an EPUB we will inflate.
const MaxPartSize = 64 << 20

// Item is one manifest entry. Path is the full path inside the EPUB zip.
type Item struct {
	ID        string
	Path      string
	MediaType string
	Props     string
}

// TOCEntry is one table-of-contents entry resolved to a full path.
type TOCEntry struct {
	Title    string
	Path     string
	Fragment string
}

// OPFMeta is the Dublin Core metadata from the package document.
type OPFMeta struct {
	Title       string
	Creators    []string
	Subjects    []string
	Description string
	Language    string
	Date        string
	Identifiers []string
	Sources     []string
	Publisher   string
}

// Book is an opened EPUB held in memory.
type Book struct {
	Data     []byte
	OPFPath  string
	Meta     OPFMeta
	Manifest map[string]Item // by id
	ByPath   map[string]Item // by full path
	Spine    []Item
	TOC      []TOCEntry

	files  map[string]*zip.File
	folded map[string]*zip.File // lower-cased path -> file, for sloppy hrefs
}

// Open parses an EPUB from memory.
func Open(data []byte) (*Book, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("epub: not a zip: %w", err)
	}
	b := &Book{
		Data:     data,
		Manifest: map[string]Item{},
		ByPath:   map[string]Item{},
		files:    make(map[string]*zip.File, len(zr.File)),
		folded:   make(map[string]*zip.File, len(zr.File)),
	}
	for _, f := range zr.File {
		name := strings.TrimPrefix(f.Name, "/")
		b.files[name] = f
		b.folded[strings.ToLower(name)] = f
	}

	b.OPFPath, err = b.findOPF()
	if err != nil {
		return nil, err
	}
	if err := b.parseOPF(); err != nil {
		return nil, err
	}
	b.parseTOC()
	return b, nil
}

// Has reports whether a file exists in the EPUB.
func (b *Book) Has(p string) bool { return b.lookup(p) != nil }

func (b *Book) lookup(p string) *zip.File {
	p = strings.TrimPrefix(p, "/")
	if f, ok := b.files[p]; ok {
		return f
	}
	return b.folded[strings.ToLower(p)]
}

// ReadFile returns the contents of a file inside the EPUB.
func (b *Book) ReadFile(p string) ([]byte, error) {
	f := b.lookup(p)
	if f == nil {
		return nil, fmt.Errorf("epub: %q not found", p)
	}
	if f.UncompressedSize64 > MaxPartSize {
		return nil, fmt.Errorf("epub: %q too large (%d bytes)", p, f.UncompressedSize64)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, MaxPartSize))
}

// MediaType returns the manifest media type for a path, if known.
func (b *Book) MediaType(p string) string {
	if it, ok := b.ByPath[p]; ok {
		return it.MediaType
	}
	return ""
}

// SpineIndex returns the spine position of a full path, or -1.
func (b *Book) SpineIndex(p string) int {
	for i, it := range b.Spine {
		if it.Path == p || strings.EqualFold(it.Path, p) {
			return i
		}
	}
	return -1
}

// ChapterTitles returns a display title for every spine item, taken from the
// first TOC entry that points at it, or "Part N" when the TOC has nothing.
func (b *Book) ChapterTitles() []string {
	titles := make([]string, len(b.Spine))
	for _, e := range b.TOC {
		if i := b.SpineIndex(e.Path); i >= 0 && titles[i] == "" {
			titles[i] = e.Title
		}
	}
	for i := range titles {
		if strings.TrimSpace(titles[i]) == "" {
			titles[i] = fmt.Sprintf("Part %d", i+1)
		}
	}
	return titles
}

// Resolve turns an href found in file `from` into a full path and fragment.
// It returns ok=false for absolute URLs (http:, mailto:, data:, ...).
func Resolve(from, href string) (full, frag string, ok bool) {
	href = strings.TrimSpace(href)
	if href == "" {
		return "", "", false
	}
	u, err := url.Parse(href)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "", "", false
	}
	if u.Path == "" {
		return from, u.Fragment, true
	}
	p := u.Path
	if !strings.HasPrefix(p, "/") {
		p = path.Join("/", path.Dir(from), p) // rooted, so ".." cannot escape
	}
	return strings.TrimPrefix(path.Clean(p), "/"), u.Fragment, true
}

func newXMLDecoder(r io.Reader) *xml.Decoder {
	d := xml.NewDecoder(r)
	d.Strict = false
	d.AutoClose = xml.HTMLAutoClose
	d.Entity = xml.HTMLEntity
	// Accept whatever encoding is declared; nearly everything is UTF-8 and a
	// few mis-declared files are better read approximately than not at all.
	d.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }
	return d
}

func (b *Book) findOPF() (string, error) {
	if data, err := b.ReadFile("META-INF/container.xml"); err == nil {
		var c struct {
			Rootfiles []struct {
				FullPath  string `xml:"full-path,attr"`
				MediaType string `xml:"media-type,attr"`
			} `xml:"rootfiles>rootfile"`
		}
		if newXMLDecoder(bytes.NewReader(data)).Decode(&c) == nil {
			for _, rf := range c.Rootfiles {
				if rf.FullPath != "" && b.Has(rf.FullPath) {
					return strings.TrimPrefix(rf.FullPath, "/"), nil
				}
			}
		}
	}
	// Fall back to the first .opf file in the archive.
	for name := range b.files {
		if strings.HasSuffix(strings.ToLower(name), ".opf") {
			return name, nil
		}
	}
	return "", errors.New("epub: no package document (.opf) found")
}

type opfDoc struct {
	Metadata struct {
		Titles      []string `xml:"title"`
		Creators    []string `xml:"creator"`
		Subjects    []string `xml:"subject"`
		Description []string `xml:"description"`
		Languages   []string `xml:"language"`
		Dates       []string `xml:"date"`
		Identifiers []string `xml:"identifier"`
		Sources     []string `xml:"source"`
		Publishers  []string `xml:"publisher"`
	} `xml:"metadata"`
	Manifest []struct {
		ID         string `xml:"id,attr"`
		Href       string `xml:"href,attr"`
		MediaType  string `xml:"media-type,attr"`
		Properties string `xml:"properties,attr"`
	} `xml:"manifest>item"`
	Spine struct {
		Toc      string `xml:"toc,attr"`
		Itemrefs []struct {
			IDRef string `xml:"idref,attr"`
		} `xml:"itemref"`
	} `xml:"spine"`
}

func first(s []string) string {
	for _, v := range s {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

func clean(s []string) []string {
	out := make([]string, 0, len(s))
	for _, v := range s {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (b *Book) parseOPF() error {
	data, err := b.ReadFile(b.OPFPath)
	if err != nil {
		return err
	}
	var doc opfDoc
	if err := newXMLDecoder(bytes.NewReader(data)).Decode(&doc); err != nil {
		return fmt.Errorf("epub: bad package document: %w", err)
	}
	m := doc.Metadata
	b.Meta = OPFMeta{
		Title:       first(m.Titles),
		Creators:    clean(m.Creators),
		Subjects:    clean(m.Subjects),
		Description: first(m.Description),
		Language:    first(m.Languages),
		Date:        first(m.Dates),
		Identifiers: clean(m.Identifiers),
		Sources:     clean(m.Sources),
		Publisher:   first(m.Publishers),
	}
	base := path.Dir(b.OPFPath)
	for _, it := range doc.Manifest {
		href, err := url.PathUnescape(it.Href)
		if err != nil {
			href = it.Href
		}
		p := strings.TrimPrefix(path.Clean(path.Join("/", base, href)), "/")
		item := Item{ID: it.ID, Path: p, MediaType: it.MediaType, Props: it.Properties}
		b.Manifest[it.ID] = item
		b.ByPath[p] = item
	}
	for _, ref := range doc.Spine.Itemrefs {
		if it, ok := b.Manifest[ref.IDRef]; ok && b.Has(it.Path) {
			b.Spine = append(b.Spine, it)
		}
	}
	if len(b.Spine) == 0 {
		return errors.New("epub: empty spine")
	}
	return nil
}

type navPoint struct {
	Label   string `xml:"navLabel>text"`
	Content struct {
		Src string `xml:"src,attr"`
	} `xml:"content"`
	Children []navPoint `xml:"navPoint"`
}

func (b *Book) parseTOC() {
	// EPUB 2: NCX file.
	for _, it := range b.Manifest {
		if it.MediaType != "application/x-dtbncx+xml" {
			continue
		}
		data, err := b.ReadFile(it.Path)
		if err != nil {
			continue
		}
		var n struct {
			Points []navPoint `xml:"navMap>navPoint"`
		}
		if newXMLDecoder(bytes.NewReader(data)).Decode(&n) != nil {
			continue
		}
		var walk func([]navPoint)
		walk = func(ps []navPoint) {
			for _, p := range ps {
				if full, frag, ok := Resolve(it.Path, p.Content.Src); ok {
					b.TOC = append(b.TOC, TOCEntry{Title: collapse(p.Label), Path: full, Fragment: frag})
				}
				walk(p.Children)
			}
		}
		walk(n.Points)
		if len(b.TOC) > 0 {
			return
		}
	}
	// EPUB 3: navigation document.
	for _, it := range b.Manifest {
		if !strings.Contains(" "+it.Props+" ", " nav ") {
			continue
		}
		data, err := b.ReadFile(it.Path)
		if err != nil {
			continue
		}
		doc, err := html.Parse(bytes.NewReader(data))
		if err != nil {
			continue
		}
		var walk func(*html.Node, bool)
		walk = func(n *html.Node, inTOC bool) {
			if n.Type == html.ElementNode && n.DataAtom == atom.Nav {
				t := attr(n, "epub:type")
				if t == "" {
					t = attr(n, "type")
				}
				inTOC = t == "" || strings.Contains(t, "toc")
			}
			if inTOC && n.Type == html.ElementNode && n.DataAtom == atom.A {
				if full, frag, ok := Resolve(it.Path, attr(n, "href")); ok {
					b.TOC = append(b.TOC, TOCEntry{Title: collapse(TextContent(n)), Path: full, Fragment: frag})
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c, inTOC)
			}
		}
		walk(doc, false)
		return
	}
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		k := a.Key
		if a.Namespace != "" {
			k = a.Namespace + ":" + a.Key
		}
		if k == key || a.Key == key {
			return a.Val
		}
	}
	return ""
}

// TextContent returns the concatenated text of a node.
func TextContent(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return sb.String()
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }
