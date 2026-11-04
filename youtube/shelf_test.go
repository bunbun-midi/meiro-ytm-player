package youtube

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestArtistPlaylistsIncludeThePagePastThePreview(t *testing.T) {
	var browsed []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		asked := string(body)
		browsed = append(browsed, asked)
		switch {
		case strings.Contains(asked, "artist-all-playlists"):
			_, _ = w.Write([]byte(`{"contents":{"gridRenderer":{"items":[
				{"musicTwoRowItemRenderer":{"title":{"runs":[{"text":"One"}]},"navigationEndpoint":{"browseEndpoint":{"browseId":"VLPL_1"}}}},
				{"musicTwoRowItemRenderer":{"title":{"runs":[{"text":"Two"}]},"navigationEndpoint":{"browseEndpoint":{"browseId":"VLPL_2"}}}},
				{"musicTwoRowItemRenderer":{"title":{"runs":[{"text":"Three"}]},"navigationEndpoint":{"browseEndpoint":{"browseId":"VLPL_3"}}}}
			]}}}`))
		default:
			_, _ = w.Write([]byte(`{"contents":{"sectionListRenderer":{"contents":[
				{"musicCarouselShelfRenderer":{
					"header":{"musicCarouselShelfBasicHeaderRenderer":{
						"title":{"runs":[{"text":"Playlists"}]},
						"moreContentButton":{"buttonRenderer":{"navigationEndpoint":{"browseEndpoint":{"browseId":"UCartist","params":"artist-all-playlists"}}}}
					}},
					"contents":[{"musicTwoRowItemRenderer":{"title":{"runs":[{"text":"One"}]},"navigationEndpoint":{"browseEndpoint":{"browseId":"VLPL_1"}}}}]
				}}
			]}}}`))
		}
	}))
	defer server.Close()

	result, err := NewClient(Options{BaseURL: server.URL}).GetArtist(context.Background(), "UCartist")
	if err != nil {
		t.Fatal(err)
	}
	if len(browsed) != 2 {
		t.Fatalf("browse requests = %d, want the artist and the rest of the playlists", len(browsed))
	}
	var titles []string
	for _, section := range result.Sections {
		if section.Title != "Playlists" {
			continue
		}
		for _, item := range section.Items {
			titles = append(titles, item.Title)
		}
	}
	if strings.Join(titles, ",") != "One,Two,Three" {
		t.Fatalf("playlists = %v", titles)
	}
}

func TestSearchPlaylistsFollowTheShelf(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		asked := string(body)
		if strings.Contains(asked, `"continuation":"playlist-rest"`) {
			_, _ = w.Write([]byte(`{"contents":{"musicShelfRenderer":{"contents":[
				{"musicResponsiveListItemRenderer":{"navigationEndpoint":{"browseEndpoint":{"browseId":"VLPL_2"}},"flexColumns":[{"musicResponsiveListItemFlexColumnRenderer":{"text":{"runs":[{"text":"Two"}]}}}]}}
			]}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"contents":{"tabbedSearchResultsRenderer":{"tabs":[{"tabRenderer":{"content":{"sectionListRenderer":{"contents":[
			{"musicShelfRenderer":{
				"title":{"runs":[{"text":"Songs"}]},
				"contents":[{"musicResponsiveListItemRenderer":{"playlistItemData":{"videoId":"vid-1"},"flexColumns":[{"musicResponsiveListItemFlexColumnRenderer":{"text":{"runs":[{"text":"A Song"}]}}}]}}],
				"continuations":[{"nextContinuationData":{"continuation":"more-songs"}}]
			}},
			{"musicShelfRenderer":{
				"title":{"runs":[{"text":"Playlists"}]},
				"contents":[{"musicResponsiveListItemRenderer":{"navigationEndpoint":{"browseEndpoint":{"browseId":"VLPL_1"}},"flexColumns":[{"musicResponsiveListItemFlexColumnRenderer":{"text":{"runs":[{"text":"One"}]}}}]}}],
				"continuations":[{"nextContinuationData":{"continuation":"playlist-rest"}}]
			}}
		]}}}}]}}}`))
	}))
	defer server.Close()

	result, err := NewClient(Options{BaseURL: server.URL}).Search(context.Background(), "someone", SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, item := range result.Items {
		titles = append(titles, item.Title)
	}
	if strings.Join(titles, ",") != "A Song,One,Two" {
		t.Fatalf("results = %v", titles)
	}
	if result.ContinuationToken != "more-songs" {
		t.Fatalf("next page = %q, want the songs", result.ContinuationToken)
	}
}
