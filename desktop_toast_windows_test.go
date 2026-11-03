//go:build windows && (amd64 || arm64)

package main

import (
	"strings"
	"testing"
)

func TestToastXMLUsesTheCover(t *testing.T) {
	got := toastXML(`A & B`, `x < y`, `file:///C:/cover.png`)
	for _, want := range []string{
		`placement="appLogoOverride"`,
		`hint-crop="none"`,
		`src="file:///C:/cover.png"`,
		`A &amp; B`,
		`x &lt; y`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("toast XML missing %q\n%s", want, got)
		}
	}
	if strings.Contains(toastXML("Song", "", "file:///C:/c.png"), "<text></text>") {
		t.Error("an empty artist became a blank line")
	}
}

func TestToastFileURIKeepsTheDrive(t *testing.T) {
	got := toastFileURI(`C:\Users\Me\My covers\art.png`)
	want := "file:///C:/Users/Me/My%20covers/art.png"
	if got != want {
		t.Fatalf("toast file URI = %q, want %q", got, want)
	}
}
