package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

var (
	blockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	lineComment  = regexp.MustCompile(`(?m)^[ \t]*//[^\n]*`)
	spaceRun     = regexp.MustCompile(`\s+`)
)

// withoutComments drops the CSS and JS comments html/template strips and collapses whitespace.
func withoutComments(s string) string {
	s = lineComment.ReplaceAllString(blockComment.ReplaceAllString(s, " "), " ")
	return strings.TrimSpace(spaceRun.ReplaceAllString(s, " "))
}

// TestRender_PagesMatchTheirSource verifies html/template changes the static pages only by dropping comments.
func TestRender_PagesMatchTheirSource(t *testing.T) {
	var confirm bytes.Buffer
	if err := confirmPageTpl.Execute(&confirm, nil); err != nil {
		t.Fatalf("confirm page: %v", err)
	}
	if withoutComments(confirm.String()) != withoutComments(confirmPageSrc) {
		t.Fatal("rendered confirm page differs from confirm.html beyond comments")
	}
	var loading bytes.Buffer
	if err := loadingPageTpl.Execute(&loading, loadingData{Expected: "0"}); err != nil {
		t.Fatalf("loading page: %v", err)
	}
	want := strings.Replace(loadingPageSrc, `data-expected="{{.Expected}}"`, `data-expected="0"`, 1)
	if withoutComments(loading.String()) != withoutComments(want) {
		t.Fatal("rendered loading page differs from index.html beyond comments and the estimate")
	}
}

// TestRender_LoadingPageEscapesTheEstimate verifies the estimate attribute is HTML-escaped.
func TestRender_LoadingPageEscapesTheEstimate(t *testing.T) {
	var b bytes.Buffer
	if err := loadingPageTpl.Execute(&b, loadingData{Expected: `1"><script>x</script>`}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), `"><script>x`) {
		t.Fatal("estimate was written unescaped")
	}
}

// TestRender_StatusBody verifies the status endpoint body format.
func TestRender_StatusBody(t *testing.T) {
	p := newTestProxy(t, false)
	rec := serve(p, "GET", "/status")
	if got, want := rec.Body.String(), "status: not ready\nfailures: 0\n"; got != want {
		t.Fatalf("status body = %q, want %q", got, want)
	}
}
