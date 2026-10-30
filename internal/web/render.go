package web

import (
	"bytes"
	"fmt"
	"html/template"
	"net/url"
	"regexp"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/mihai-max/ao3/internal/epub"
)

func newPolicy() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowRelativeURLs(true)
	p.RequireNoFollowOnLinks(false)
	p.AllowAttrs("align").Matching(regexp.MustCompile(`(?i)^(left|right|center|justify)$`)).OnElements("p", "div", "td", "th", "h1", "h2", "h3", "h4", "h5", "h6")
	p.AllowAttrs("class").Matching(regexp.MustCompile(`^[\w\- ]{1,100}$`)).Globally()
	p.AllowElements("hr", "center")
	return p
}

// resURL is the server URL for a resource inside a work's EPUB.
func resURL(id int64, p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return fmt.Sprintf("/work/%d/res/%s", id, strings.Join(segs, "/"))
}

// renderChapter turns spine item n of a book into sanitized HTML whose links
// and images point back at this server.
func renderChapter(b *epub.Book, id int64, n int, policy *bluemonday.Policy) (template.HTML, error) {
	item := b.Spine[n]
	data, err := b.ReadFile(item.Path)
	if err != nil {
		return "", err
	}
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	body := findBody(doc)
	if body == nil {
		body = doc
	}
	rewrite(body, b, id, item.Path)

	var buf bytes.Buffer
	for c := body.FirstChild; c != nil; c = c.NextSibling {
		if err := html.Render(&buf, c); err != nil {
			return "", err
		}
	}
	return template.HTML(policy.SanitizeBytes(buf.Bytes())), nil
}

func findBody(n *html.Node) *html.Node {
	if n.Type == html.ElementNode && n.DataAtom == atom.Body {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if b := findBody(c); b != nil {
			return b
		}
	}
	return nil
}

func getAttr(n *html.Node, keys ...string) (int, string) {
	for i, a := range n.Attr {
		for _, k := range keys {
			if a.Key == k {
				return i, a.Val
			}
		}
	}
	return -1, ""
}

func rewrite(n *html.Node, b *epub.Book, id int64, from string) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		if c.Type == html.ElementNode {
			switch c.DataAtom {
			case atom.Img:
				if i, src := getAttr(c, "src"); i >= 0 {
					if full, _, ok := epub.Resolve(from, src); ok && b.Has(full) {
						c.Attr[i].Val = resURL(id, full)
					} else if ok {
						c.Attr = append(c.Attr[:i], c.Attr[i+1:]...)
					}
				}
			case atom.A:
				if i, href := getAttr(c, "href"); i >= 0 {
					if full, frag, ok := epub.Resolve(from, href); ok {
						if k := b.SpineIndex(full); k >= 0 {
							u := fmt.Sprintf("/work/%d/ch/%d", id, k)
							if frag != "" {
								u += "#" + url.PathEscape(frag)
							}
							c.Attr[i].Val = u
						} else {
							c.Attr = append(c.Attr[:i], c.Attr[i+1:]...)
						}
					}
				}
			case atom.Svg:
				// Calibre wraps cover images in <svg><image xlink:href=...>,
				// which the sanitizer drops; turn it into a plain <img>.
				if img := svgImage(c, b, id, from); img != nil {
					n.InsertBefore(img, c)
					n.RemoveChild(c)
					c = next
					continue
				}
			}
		}
		rewrite(c, b, id, from)
		c = next
	}
}

func svgImage(svg *html.Node, b *epub.Book, id int64, from string) *html.Node {
	var found *html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if found != nil {
			return
		}
		if n.Type == html.ElementNode && n.Data == "image" {
			found = n
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(svg)
	if found == nil {
		return nil
	}
	_, href := getAttr(found, "href", "xlink:href")
	full, _, ok := epub.Resolve(from, href)
	if !ok || !b.Has(full) {
		return nil
	}
	return &html.Node{Type: html.ElementNode, Data: "img", DataAtom: atom.Img,
		Attr: []html.Attribute{{Key: "src", Val: resURL(id, full)}, {Key: "alt", Val: ""}}}
}
