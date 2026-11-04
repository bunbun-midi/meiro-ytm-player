package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// SearchFilter is one of the filters YouTube Music offers for a search: the
// name it shows, and the parameters that search with it. The filter that
// clears them all carries neither.
type SearchFilter struct {
	Label  string `json:"label,omitempty"`
	Params string `json:"params,omitempty"`
}

// SearchOptions controls a music search. Params is what one of the search's
// own filters carries; an empty value searches every category.
type SearchOptions struct {
	Params string
}

// SearchResult contains the parsed music items of a search.
type SearchResult struct {
	Items             []MusicItem `json:"items"`
	ContinuationToken string      `json:"continuationToken,omitempty"`
	// TopResult is the card a search leads with, which opens a page or plays
	// a video. Its zero value says the search has none.
	TopResult MusicItem `json:"topResult,omitempty"`
	// Filters are the categories the search can be narrowed to, in the order
	// YouTube Music offers them for this query.
	Filters []SearchFilter `json:"filters,omitempty"`
}

// SearchSuggestions is what YouTube Music offers for a query being typed: the
// completed queries, and the items the query already matches.
type SearchSuggestions struct {
	Queries []string    `json:"queries"`
	Items   []MusicItem `json:"items,omitempty"`
}

// Search searches YouTube Music. Params narrows the search to one of the
// categories YouTube offers for it, as SearchResult.Filters gives them; without
// any, every category is searched.
func (c *Client) Search(ctx context.Context, query string, options SearchOptions) (*SearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("youtube: search query is required")
	}
	payload := map[string]any{"query": query}
	if options.Params != "" {
		// The parameters are a filter's own, as YouTube gave them with an
		// earlier search of the same kind: they go back the way they came.
		payload["params"] = options.Params
	}
	raw, err := c.execute(ctx, "search", payload)
	if err != nil {
		return nil, err
	}
	return c.finishSearch(ctx, raw)
}

// ContinueSearch requests the next page from SearchResult.ContinuationToken.
func (c *Client) ContinueSearch(ctx context.Context, token string) (*SearchResult, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("youtube: search continuation token is required")
	}
	raw, err := c.execute(ctx, "search", map[string]any{"continuation": token})
	if err != nil {
		return nil, err
	}
	return c.finishSearch(ctx, raw)
}

// finishSearch reads a search response and follows a playlists shelf that
// only previewed its contents, so the results hold every playlist from it.
func (c *Client) finishSearch(ctx context.Context, raw json.RawMessage) (*SearchResult, error) {
	root := decodeResponse(raw)
	items, sections, top := browseParts(root, true)
	drained := map[string]struct{}{}
	for i := range sections {
		extra, token, err := c.expandPlaylistShelf(ctx, &sections[i], true)
		if err != nil || len(extra) == 0 {
			continue
		}
		items = appendNewItems(items, extra)
		if token != "" {
			drained[token] = struct{}{}
		}
	}
	return &SearchResult{
		Items: items, TopResult: top, Filters: searchFilters(root),
		ContinuationToken: firstContinuation(root, drained),
	}, nil
}

// GetSearchSuggestions returns what YouTube Music offers for a query being
// typed: the query completions, in the order it offers them, and the items the
// partial query already matches.
func (c *Client) GetSearchSuggestions(ctx context.Context, input string) (*SearchSuggestions, error) {
	if strings.TrimSpace(input) == "" {
		return nil, errors.New("youtube: suggestion input is required")
	}
	raw, err := c.execute(ctx, "music/get_search_suggestions", map[string]any{"input": input})
	if err != nil {
		return nil, err
	}
	root := decodeResponse(raw)
	items, _, _ := browseParts(root, false)
	return &SearchSuggestions{Queries: searchSuggestions(root), Items: items}, nil
}

// searchSuggestions reads the completed text of every suggestion renderer in a
// suggestion response, in order and without repeats.
func searchSuggestions(root any) []string {
	var suggestions []string
	seen := make(map[string]struct{})
	var walk func(any)
	walk = func(value any) {
		switch node := value.(type) {
		case []any:
			for _, child := range node {
				walk(child)
			}
		case *jsonObject:
			if renderer, ok := node.get("searchSuggestionRenderer").(*jsonObject); ok {
				if text := rendererText(renderer.get("suggestion")); text != "" {
					if _, exists := seen[text]; !exists {
						seen[text] = struct{}{}
						suggestions = append(suggestions, text)
					}
				}
			}
			for _, member := range node.members {
				walk(member.value)
			}
		}
	}
	walk(root)
	return suggestions
}

// newSearchResult reads a search response, as newBrowseResult does a browse.
func (c *Client) newSearchResult(raw json.RawMessage) *SearchResult {
	root := decodeResponse(raw)
	items, _, top := browseParts(root, true)
	return &SearchResult{
		Items: items, TopResult: top, Filters: searchFilters(root),
		ContinuationToken: continuationToken(root),
	}
}

// searchFilters reads the filters a search response offers, in the order
// YouTube Music shows them. The chip that clears them all has no parameters.
func searchFilters(root any) []SearchFilter {
	cloud := findRenderer(root, "chipCloudRenderer")
	if cloud == nil {
		return nil
	}
	chips, _ := cloud.get("chips").([]any)
	filters := make([]SearchFilter, 0, len(chips))
	for _, value := range chips {
		chip, ok := value.(*jsonObject)
		if !ok {
			continue
		}
		renderer, ok := chip.get("chipCloudChipRenderer").(*jsonObject)
		if !ok {
			continue
		}
		filters = append(filters, SearchFilter{
			Label:  rendererText(renderer.get("text")),
			Params: searchEndpointParams(renderer),
		})
	}
	return filters
}

// searchEndpointParams reads the parameters a chip searches with, which the
// chip that clears the filters does without.
func searchEndpointParams(renderer *jsonObject) string {
	endpoint, ok := renderer.get("navigationEndpoint").(*jsonObject)
	if !ok {
		return ""
	}
	search, ok := endpoint.get("searchEndpoint").(*jsonObject)
	if !ok {
		return ""
	}
	params, _ := search.get("params").(string)
	return params
}
