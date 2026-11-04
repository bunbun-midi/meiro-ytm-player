package youtube

import (
	"context"
	"strings"
)

// shelfMoreTarget reads the browse a shelf's header offers for the rest of
// the shelf. The button carries the channel and the parameters of that page.
// A title that only names the channel is not that page.
func shelfMoreTarget(renderer *jsonObject) (browseID, params string) {
	header, ok := renderer.get("header").(*jsonObject)
	if !ok {
		return "", ""
	}
	var walk func(any)
	walk = func(value any) {
		if browseID != "" {
			return
		}
		switch node := value.(type) {
		case []any:
			for _, child := range node {
				walk(child)
			}
		case *jsonObject:
			if endpoint, ok := node.get("browseEndpoint").(*jsonObject); ok {
				id, _ := endpoint.get("browseId").(string)
				p, _ := endpoint.get("params").(string)
				if id != "" && p != "" {
					browseID, params = id, p
					return
				}
			}
			for _, member := range node.members {
				walk(member.value)
			}
		}
	}
	walk(header)
	return browseID, params
}

// isPlaylistShelf reports whether a shelf is a preview of playlists that a
// further page continues. Songs, and a shelf that already holds everything,
// are left as they are.
func isPlaylistShelf(section MusicSection) bool {
	if section.MoreBrowseID == "" && section.ContinuationToken == "" {
		return false
	}
	if len(section.Items) == 0 {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(section.Title), "Playlists") {
		return true
	}
	for _, item := range section.Items {
		if item.Target().Kind != TargetPlaylist {
			return false
		}
	}
	return true
}

// expandPlaylistShelf reads the playlists a shelf only previewed. extra is
// what the preview did not already hold. token is the shelf continuation the
// caller should not also use as the next page of a search. search says the
// shelf came from a search, whose continuation is read from the search
// endpoint. A shelf that is not a playlist preview returns nothing.
func (c *Client) expandPlaylistShelf(ctx context.Context, section *MusicSection, search bool) (extra []MusicItem, token string, err error) {
	if section == nil || !isPlaylistShelf(*section) {
		return nil, "", nil
	}
	if section.MoreBrowseID != "" {
		page, err := c.browseWith(ctx, section.MoreBrowseID, section.MoreParams)
		if err != nil {
			return nil, "", err
		}
		if err := c.drainBrowse(ctx, page, "playlists", nil); err != nil {
			return nil, "", err
		}
		return page.Items, section.ContinuationToken, nil
	}
	if search {
		items, err := c.searchContinuationItems(ctx, section.ContinuationToken)
		return items, section.ContinuationToken, err
	}
	page := &BrowseResult{ContinuationToken: section.ContinuationToken}
	if err := c.drainBrowse(ctx, page, "playlists", nil); err != nil {
		return nil, "", err
	}
	return page.Items, section.ContinuationToken, nil
}

// searchContinuationItems follows a search continuation until it ends. It
// reads each page itself, so a page that also previews playlists is not
// expanded again from here.
func (c *Client) searchContinuationItems(ctx context.Context, token string) ([]MusicItem, error) {
	var items []MusicItem
	seen := map[string]struct{}{}
	for token != "" && len(seen) < 1000 {
		if _, exists := seen[token]; exists {
			break
		}
		seen[token] = struct{}{}
		raw, err := c.execute(ctx, "search", map[string]any{"continuation": token})
		if err != nil {
			return items, err
		}
		root := decodeResponse(raw)
		pageItems, _, _ := browseParts(root, true)
		items = append(items, pageItems...)
		token = ""
		for _, next := range continuationTokens(root) {
			if _, exists := seen[next]; !exists {
				token = next
				break
			}
		}
	}
	return items, nil
}

// appendNewItems appends the items of extra that items does not already hold.
func appendNewItems(items, extra []MusicItem) []MusicItem {
	seen := make(map[itemIdentity]struct{}, len(items))
	for _, item := range items {
		seen[itemIdentity{videoID: item.VideoID, browseID: item.BrowseID, playlistID: item.PlaylistID, title: item.Title}] = struct{}{}
	}
	for _, item := range extra {
		id := itemIdentity{videoID: item.VideoID, browseID: item.BrowseID, playlistID: item.PlaylistID, title: item.Title}
		if id == (itemIdentity{}) {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		items = append(items, item)
	}
	return items
}

// firstContinuation is the first continuation token in root that skip does
// not name. With nothing skipped it is the token continuationToken returns.
func firstContinuation(root any, skip map[string]struct{}) string {
	for _, token := range continuationTokens(root) {
		if _, exists := skip[token]; exists {
			continue
		}
		return token
	}
	return ""
}

// continuationTokens lists every continuation token in root, in document
// order, without repeats.
func continuationTokens(root any) []string {
	var tokens []string
	seen := map[string]struct{}{}
	add := func(token string) {
		if token == "" {
			return
		}
		if _, exists := seen[token]; exists {
			return
		}
		seen[token] = struct{}{}
		tokens = append(tokens, token)
	}
	var walk func(any)
	walk = func(value any) {
		switch node := value.(type) {
		case []any:
			for _, child := range node {
				walk(child)
			}
		case *jsonObject:
			if command, ok := node.get("continuationCommand").(*jsonObject); ok {
				token, _ := command.get("token").(string)
				add(token)
			}
			if next, ok := node.get("nextContinuationData").(*jsonObject); ok {
				token, _ := next.get("continuation").(string)
				add(token)
			}
			if token, ok := node.get("continuation").(string); ok {
				add(token)
			}
			for _, member := range node.members {
				walk(member.value)
			}
		}
	}
	walk(root)
	return tokens
}
