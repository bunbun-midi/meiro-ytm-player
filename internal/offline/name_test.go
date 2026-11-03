package offline

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elianiva/meiro/internal/proc"
	"github.com/elianiva/meiro/youtube"
)

func TestSongFileNameUsesTheArtistAndTitle(t *testing.T) {
	item := youtube.MusicItem{Artist: "Ada", Artists: "Ada, Bob", Title: "Tune: One"}
	if got := SongFileName(item, "mp3"); got != "Ada, Bob - Tune One.mp3" {
		t.Fatalf("name = %q", got)
	}
	item.Artists, item.Artist, item.Title = "", "", "  "
	if got := SongFileName(item, ".mp3"); got != "Unknown track.mp3" {
		t.Fatalf("blank song = %q", got)
	}
}

func TestInstallKeepsASecondSongOffTheFirstFile(t *testing.T) {
	dir := t.TempDir()
	item := youtube.MusicItem{Artist: "Ada", Title: "Tune"}
	first := writeSource(t, dir)
	name, err := install(context.Background(), dir, first, "original", item)
	if err != nil {
		t.Fatal(err)
	}
	if name != "Ada - Tune.wav" {
		t.Fatalf("name = %q", name)
	}
	second := writeSource(t, dir)
	name, err = install(context.Background(), dir, second, "original", item)
	if err != nil {
		t.Fatal(err)
	}
	if name != "Ada - Tune (2).wav" {
		t.Fatalf("second name = %q", name)
	}
}

func TestMP3CarriesTheArtistTitleAndAlbum(t *testing.T) {
	ffmpeg, err := proc.Tool("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not available")
	}
	dir := t.TempDir()
	source := writeSource(t, dir)
	item := youtube.MusicItem{Artist: "Ada", Title: "Tune", Album: "Record"}
	name, err := install(context.Background(), dir, source, "mp3", item)
	if err != nil {
		t.Fatal(err)
	}
	if name != "Ada - Tune.mp3" {
		t.Fatalf("name = %q", name)
	}
	out, err := exec.Command(ffmpeg, "-i", filepath.Join(dir, name), "-f", "ffmetadata", "-").Output()
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, want := range []string{"title=Tune", "artist=Ada", "album=Record"} {
		if !strings.Contains(text, want) {
			t.Fatalf("tags =\n%s\nwant %s", text, want)
		}
	}
}

func writeSource(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tone.wav")
	const (
		rate = 8000
		n    = 4000
	)
	var body bytes.Buffer
	for i := 0; i < n; i++ {
		_ = binary.Write(&body, binary.LittleEndian, int16(0))
	}
	samples := body.Bytes()
	var header bytes.Buffer
	header.WriteString("RIFF")
	_ = binary.Write(&header, binary.LittleEndian, uint32(36+len(samples)))
	header.WriteString("WAVEfmt ")
	_ = binary.Write(&header, binary.LittleEndian, uint32(16))
	_ = binary.Write(&header, binary.LittleEndian, uint16(1))
	_ = binary.Write(&header, binary.LittleEndian, uint16(1))
	_ = binary.Write(&header, binary.LittleEndian, uint32(rate))
	_ = binary.Write(&header, binary.LittleEndian, uint32(rate*2))
	_ = binary.Write(&header, binary.LittleEndian, uint16(2))
	_ = binary.Write(&header, binary.LittleEndian, uint16(16))
	header.WriteString("data")
	_ = binary.Write(&header, binary.LittleEndian, uint32(len(samples)))
	if err := os.WriteFile(path, append(header.Bytes(), samples...), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
