package youtube

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

// ParseMusicURL reads a YouTube or YouTube Music link. target is the page or
// the video it names when that can be read from the link itself. resolve is
// the link to ask YouTube about when it cannot, such as a channel handle.
// ok is false when raw is not one of those links.
func ParseMusicURL(raw string) (target Target, resolve string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, " \t\n") {
		return Target{}, "", false
	}
	lower := strings.ToLower(raw)
	if !strings.Contains(lower, "youtube.com") && !strings.Contains(lower, "youtu.be") && !strings.Contains(lower, "youtube-nocookie.com") {
		return Target{}, "", false
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || !youtubeHost(parsed.Host) {
		return Target{}, "", false
	}
	path := strings.Trim(parsed.Path, "/")
	if strings.EqualFold(parsed.Hostname(), "youtu.be") {
		if id := pathSegment(path, 0); id != "" {
			return Target{TargetTrack, id}, "", true
		}
		return Target{}, parsed.String(), true
	}
	if strings.HasPrefix(path, "@") || strings.HasPrefix(path, "c/") || strings.HasPrefix(path, "user/") {
		return Target{}, parsed.String(), true
	}
	head := strings.ToLower(pathSegment(path, 0))
	rest := pathSegment(path, 1)
	switch head {
	case "watch", "shorts", "embed", "live", "v":
		if id := parsed.Query().Get("v"); id != "" {
			return Target{TargetTrack, id}, "", true
		}
		if rest != "" && head != "watch" {
			return Target{TargetTrack, rest}, "", true
		}
	case "playlist":
		if id := parsed.Query().Get("list"); id != "" {
			return Target{TargetPlaylist, id}, "", true
		}
		if rest != "" {
			return Target{TargetPlaylist, rest}, "", true
		}
	case "browse", "channel", "artist", "album", "podcast":
		if target, ok := targetForBrowseID(rest); ok {
			return target, "", true
		}
	}
	if id := parsed.Query().Get("v"); id != "" {
		return Target{TargetTrack, id}, "", true
	}
	if id := parsed.Query().Get("list"); id != "" {
		return Target{TargetPlaylist, id}, "", true
	}
	return Target{}, parsed.String(), true
}

// ResolveURL asks YouTube where a link goes, for a link ParseMusicURL could
// not read on its own.
func (c *Client) ResolveURL(ctx context.Context, raw string) (Target, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Target{}, errors.New("youtube: url is required")
	}
	body, err := c.execute(ctx, "navigation/resolve_url", map[string]any{"url": raw})
	if err != nil {
		return Target{}, err
	}
	target, ok := resolveTarget(decodeResponse(body))
	if !ok {
		return Target{}, errors.New("youtube: the link does not open a page")
	}
	return target, nil
}

func youtubeHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimPrefix(host, "www.")
	host = strings.TrimPrefix(host, "m.")
	switch host {
	case "youtube.com", "music.youtube.com", "youtu.be", "youtube-nocookie.com":
		return true
	default:
		return false
	}
}

func pathSegment(path string, index int) string {
	if path == "" {
		return ""
	}
	parts := strings.Split(path, "/")
	if index < 0 || index >= len(parts) {
		return ""
	}
	return parts[index]
}

func targetForBrowseID(id string) (Target, bool) {
	switch {
	case id == "":
		return Target{}, false
	case isAlbumID(id):
		return Target{TargetAlbum, id}, true
	case isArtistID(id):
		return Target{TargetArtist, id}, true
	case isPodcastID(id):
		return Target{TargetPodcast, id}, true
	case isBrowsablePlaylist(id):
		return Target{TargetPlaylist, id}, true
	default:
		if videoID := episodeVideoIDFromBrowseID(id); videoID != "" {
			return Target{TargetTrack, videoID}, true
		}
		return Target{}, false
	}
}

// resolveTarget reads the page or video a resolve_url response names.
func resolveTarget(root any) (Target, bool) {
	var watch, browse string
	var walk func(any)
	walk = func(value any) {
		switch node := value.(type) {
		case []any:
			for _, child := range node {
				walk(child)
			}
		case *jsonObject:
			if endpoint, ok := node.get("watchEndpoint").(*jsonObject); ok && watch == "" {
				watch, _ = endpoint.get("videoId").(string)
			}
			if endpoint, ok := node.get("browseEndpoint").(*jsonObject); ok && browse == "" {
				browse, _ = endpoint.get("browseId").(string)
			}
			for _, member := range node.members {
				walk(member.value)
			}
		}
	}
	walk(root)
	if watch != "" {
		return Target{TargetTrack, watch}, true
	}
	return targetForBrowseID(browse)
}
