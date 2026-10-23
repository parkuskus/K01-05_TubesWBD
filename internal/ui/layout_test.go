package ui

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

func TestLayoutRendersTitleAndChildSafely(t *testing.T) {
	child := templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, `<section id="child">Konten</section>`)
		return err
	})
	var html bytes.Buffer
	if err := Layout(`<script>alert("x")</script> & Data`, child).Render(context.Background(), &html); err != nil {
		t.Fatal(err)
	}
	page := html.String()
	for _, want := range []string{
		`<!doctype html>`, `lang="id"`, `<meta name="viewport"`,
		`<title>&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt; &amp; Data</title>`,
		`<main><section id="child">Konten</section></main>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("missing %q in %q", want, page)
		}
	}
	if strings.Contains(page, `<script>`) {
		t.Fatal("unescaped title became executable HTML")
	}
}
