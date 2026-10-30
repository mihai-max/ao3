package epub

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Work is the searchable metadata for one fanwork. It is filled from the AO3
// preface page when present, and from OPF metadata otherwise.
type Work struct {
	WorkID        int64 // 0 when unknown
	Title         string
	Authors       []string
	Summary       string
	Rating        []string
	Warnings      []string
	Categories    []string
	Fandoms       []string
	Relationships []string
	Characters    []string
	Freeforms     []string
	Language      string
	Series        []string
	Published     string // YYYY-MM-DD
	Updated       string // YYYY-MM-DD, falls back to Published
	Complete      bool
	Words         int64
	Chapters      string // "3/10", "1/1", "2/?"
	HasPreface    bool
}

var (
	reWorkURL   = regexp.MustCompile(`archiveofourown\.org/works/(\d+)`)
	rePublished = regexp.MustCompile(`Published:\s*(\d{4}-\d{2}-\d{2})`)
	reUpdated   = regexp.MustCompile(`(Updated|Completed):\s*(\d{4}-\d{2}-\d{2})`)
	reWords     = regexp.MustCompile(`Words:\s*([\d,.]+)`)
	reChapters  = regexp.MustCompile(`Chapters:\s*(\d+/(?:\d+|\?))`)
	reIDName    = regexp.MustCompile(`(?i)(?:^|[_\-])(\d{3,})\.epub$`)
)

// prefaceCut keeps html.Parse cheap: AO3 prefaces put everything we need
// before the end of the summary, but the same file can carry a whole chapter.
const prefaceCut = 96 << 10

// WorkMeta extracts searchable metadata. It looks for the AO3 preface in the
// first few spine items and falls back to OPF metadata.
func (b *Book) WorkMeta() Work {
	var w Work
	for i := 0; i < len(b.Spine) && i < 3; i++ {
		data, err := b.ReadFile(b.Spine[i].Path)
		if err != nil {
			continue
		}
		if bytes.Contains(data, []byte("archiveofourown.org/works/")) || bytes.Contains(data, []byte(`class="tags"`)) {
			w = ParsePreface(data)
			break
		}
	}

	o := b.Meta
	if w.Title == "" {
		w.Title = o.Title
	}
	if len(w.Authors) == 0 {
		w.Authors = o.Creators
	}
	if w.Summary == "" && o.Description != "" {
		w.Summary = htmlToText([]byte(o.Description))
	}
	if w.Language == "" {
		w.Language = o.Language
	}
	if !w.HasPreface && len(o.Subjects) > 0 {
		w.Freeforms = o.Subjects
	}
	if w.Published == "" && len(o.Date) >= 10 {
		w.Published = o.Date[:10]
	}
	if w.Updated == "" {
		w.Updated = w.Published
	}
	if w.WorkID == 0 {
		for _, s := range append(append([]string{}, o.Identifiers...), o.Sources...) {
			if m := reWorkURL.FindStringSubmatch(s); m != nil {
				w.WorkID, _ = strconv.ParseInt(m[1], 10, 64)
				break
			}
		}
	}
	return w
}

// WorkIDFromName guesses an AO3 work id from a file name shaped like
// "12345.epub" or "Some_Title_12345.epub". Other names give 0, so a stray
// number in a title cannot be mistaken for a work id.
func WorkIDFromName(name string) int64 {
	base := name
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	m := reIDName.FindStringSubmatch(base)
	if m == nil {
		return 0
	}
	id, _ := strconv.ParseInt(m[1], 10, 64)
	return id
}

// ParsePreface parses an AO3 download preface page.
func ParsePreface(data []byte) Work {
	var w Work
	if m := reWorkURL.FindSubmatch(data); m != nil {
		w.WorkID, _ = strconv.ParseInt(string(m[1]), 10, 64)
	}
	doc, err := html.Parse(bytes.NewReader(truncatePreface(data)))
	if err != nil {
		return w
	}

	var summaryNext bool
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch {
			case n.DataAtom == atom.Dl && hasClass(n, "tags"):
				w.HasPreface = true
				parseTagList(n, &w)
				return
			case n.DataAtom == atom.H1 && w.Title == "":
				w.Title = collapse(TextContent(n))
				return
			case n.DataAtom == atom.A && attr(n, "rel") == "author":
				if a := collapse(TextContent(n)); a != "" {
					w.Authors = append(w.Authors, a)
				}
				return
			case n.DataAtom == atom.Div && hasClass(n, "byline") && len(w.Authors) == 0:
				defer func() {
					if len(w.Authors) == 0 {
						t := strings.TrimPrefix(collapse(TextContent(n)), "by ")
						for _, a := range strings.Split(t, ", ") {
							if a = strings.TrimSpace(a); a != "" {
								w.Authors = append(w.Authors, a)
							}
						}
					}
				}()
			case n.DataAtom == atom.P && collapse(TextContent(n)) == "Summary":
				summaryNext = true
				return
			case n.DataAtom == atom.Blockquote && summaryNext:
				summaryNext = false
				w.Summary = blockText(n)
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if w.Updated == "" {
		w.Updated = w.Published
	}
	return w
}

func truncatePreface(data []byte) []byte {
	if len(data) <= prefaceCut {
		return data
	}
	start := bytes.Index(data, []byte(`class="byline"`))
	if start < 0 {
		return data[:prefaceCut]
	}
	if i := bytes.Index(data[start:], []byte("</blockquote>")); i >= 0 && i < prefaceCut {
		return data[:start+i+len("</blockquote>")]
	}
	end := start + prefaceCut
	if end > len(data) {
		end = len(data)
	}
	return data[:end]
}

func parseTagList(dl *html.Node, w *Work) {
	var label string
	for c := dl.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode {
			continue
		}
		switch c.DataAtom {
		case atom.Dt:
			label = strings.ToLower(strings.TrimSuffix(collapse(TextContent(c)), ":"))
		case atom.Dd:
			vals := linkTexts(c)
			text := collapse(TextContent(c))
			if len(vals) == 0 && text != "" {
				vals = splitComma(text)
			}
			switch strings.TrimSuffix(label, "s") {
			case "rating":
				w.Rating = vals
			case "archive warning", "warning":
				w.Warnings = vals
			case "category", "categorie":
				w.Categories = vals
			case "fandom":
				w.Fandoms = vals
			case "relationship":
				w.Relationships = vals
			case "character":
				w.Characters = vals
			case "additional tag", "freeform":
				w.Freeforms = vals
			case "language":
				w.Language = text
			case "serie":
				w.Series = vals
			case "stat":
				parseStats(text, w)
			}
		}
	}
}

func parseStats(s string, w *Work) {
	if m := rePublished.FindStringSubmatch(s); m != nil {
		w.Published = m[1]
	}
	if m := reUpdated.FindStringSubmatch(s); m != nil {
		w.Updated = m[2]
		w.Complete = m[1] == "Completed"
	}
	if m := reWords.FindStringSubmatch(s); m != nil {
		digits := strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, m[1])
		w.Words, _ = strconv.ParseInt(digits, 10, 64)
	}
	if m := reChapters.FindStringSubmatch(s); m != nil {
		w.Chapters = m[1]
		if parts := strings.SplitN(m[1], "/", 2); len(parts) == 2 && parts[0] == parts[1] {
			w.Complete = true
		}
	}
}

func linkTexts(n *html.Node) []string {
	var out []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.DataAtom == atom.A {
			if t := collapse(TextContent(n)); t != "" {
				out = append(out, t)
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

func splitComma(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func hasClass(n *html.Node, class string) bool {
	for _, c := range strings.Fields(attr(n, "class")) {
		if c == class {
			return true
		}
	}
	return false
}

// blockText returns text with paragraph breaks preserved as blank lines.
func blockText(n *html.Node) string {
	var paras []string
	var cur strings.Builder
	flush := func() {
		if t := collapse(cur.String()); t != "" {
			paras = append(paras, t)
		}
		cur.Reset()
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch {
		case n.Type == html.TextNode:
			cur.WriteString(n.Data)
		case n.Type == html.ElementNode && n.DataAtom == atom.Br:
			flush()
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode {
			switch n.DataAtom {
			case atom.P, atom.Div, atom.Li, atom.Blockquote, atom.H1, atom.H2, atom.H3:
				flush()
			}
		}
	}
	walk(n)
	flush()
	return strings.Join(paras, "\n\n")
}

func htmlToText(b []byte) string {
	if !bytes.ContainsRune(b, '<') {
		return strings.TrimSpace(string(b))
	}
	doc, err := html.Parse(bytes.NewReader(b))
	if err != nil {
		return ""
	}
	return blockText(doc)
}
