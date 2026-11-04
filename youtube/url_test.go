package youtube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseMusicURL(t *testing.T) {
	for _, test := range []struct {
		raw    string
		kind   TargetKind
		id     string
		ask    bool
		reject bool
	}{
		{raw: "yorushika", reject: true},
		{raw: "https://example.com/watch?v=vid-3", reject: true},
		{raw: "https://music.youtube.com/watch?v=vid-3", kind: TargetTrack, id: "vid-3"},
		{raw: "https://www.youtube.com/watch?v=vid-3", kind: TargetTrack, id: "vid-3"},
		{raw: "https://youtu.be/vid-3", kind: TargetTrack, id: "vid-3"},
		{raw: "https://music.youtube.com/playlist?list=VLPL_songs", kind: TargetPlaylist, id: "VLPL_songs"},
		{raw: "https://music.youtube.com/browse/UCartist", kind: TargetArtist, id: "UCartist"},
		{raw: "https://music.youtube.com/browse/MPREb_test", kind: TargetAlbum, id: "MPREb_test"},
		{raw: "https://music.youtube.com/channel/UCartist", kind: TargetArtist, id: "UCartist"},
		{raw: "https://music.youtube.com/browse/MPSPPLshow", kind: TargetPodcast, id: "MPSPPLshow"},
		{raw: "music.youtube.com/browse/UCartist", kind: TargetArtist, id: "UCartist"},
		{raw: "https://music.youtube.com/@aurora", ask: true},
	} {
		target, resolve, ok := ParseMusicURL(test.raw)
		if test.reject {
			if ok {
				t.Errorf("%s parsed as %+v", test.raw, target)
			}
			continue
		}
		if !ok {
			t.Errorf("%s was not a link", test.raw)
			continue
		}
		if test.ask {
			if resolve == "" || target.ID != "" {
				t.Errorf("%s resolve %q target %+v", test.raw, resolve, target)
			}
			continue
		}
		if target.Kind != test.kind || target.ID != test.id || resolve != "" {
			t.Errorf("%s = %+v resolve %q, want %v %s", test.raw, target, resolve, test.kind, test.id)
		}
	}
}

func TestResolveURLReadsTheChannel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/youtubei/v1/navigation/resolve_url" {
			t.Errorf("path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"endpoint":{"browseEndpoint":{"browseId":"UCresolved"}}}`))
	}))
	defer server.Close()

	target, err := NewClient(Options{BaseURL: server.URL}).ResolveURL(context.Background(), "https://music.youtube.com/@aurora")
	if err != nil {
		t.Fatal(err)
	}
	if target.Kind != TargetArtist || target.ID != "UCresolved" {
		t.Fatalf("resolved %+v", target)
	}
}
