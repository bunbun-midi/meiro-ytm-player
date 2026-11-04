# Browse pages

A user opens the pages in the rail, follows an item into an album, playlist or artist page, and goes back to where they were.

## Sub-features

- `browse-pages` draws every route with its seeded content.
- `browse-open` follows an item from Home into its page.
- `browse-back` restores the scroll position of the page left.
- `browse-album-cover` loads an album's own artwork when its header is nested in the primary tab, including navigation from the player's album credit.
- `browse-new-route` covers a page added to `routes` in `routes.go`.

## How to get to it (user POV)

- Click `Home`, `Explore`, `Library` or `Settings` in the rail. The rail stays compact. `Expand navigation` opens the queue beside it, and that queue stays while those pages change. `Resize playlist` is its right edge. A row shows the artist, the title, the length and the album on one line. The artist, the title and the album can be clicked. The length cannot. Drag a row to reorder the queue; Up Next follows. Right-click a row and choose `Play` or `Download`. Under those, `All To Playlist` names the whole queue and saves it on this computer, and `Song to Playlist` adds that song to one already saved: type to narrow the names, then choose one. A downloaded title uses the theme colour. `Playlists`, under `Library`, lists those playlists. Click one to see its songs. Right-click it and choose `Add Next`, `Add to Queue` or `Replace List`. `Remove` deletes it from this computer. `Playback`, above `Settings`, opens the playback panel.
- Press `Cmd+1`, `Cmd+2`, `Cmd+3` or `Cmd+,`.
- Click an album, playlist or artist card on any page. An artist page lists every playlist, not only the first row, and the page scrolls through them. On Playlists, click a saved playlist to open it and click it again to close it. The open list scrolls through every song.
- Open a `meiro://` link (see [native-only.md](./native-only.md)).

## Driving it with the headless tester

Preconditions:

- `scripts/doctor.sh` passes.
- Start from `a := newTestApp(t)`, `a.router.Push("/home")`, `tt := ui.NewTester(a.view, 1000, 700)`.

- **Every page.** Push each path in `TestEveryPageLoadsAndDraws` and call `p.step`. `Home` shows `Quick picks` and `Ambient One`. `/explore` shows `Moods` and `Night Drive`. `/album/MPREb_test` shows `Album Track One`.
- **Open an item.** From `/home`, `p.click("Deep Focus", "album")`. `a.router.Path()` starts with `/album/` and `Album Track One` shows.
- **Show more.** On Home, click `Show more`. The next sections appear below the existing feed, and the request count increases once.
- **Go back.** Verified for album: click `Deep Focus` on `/home`, then `a.router.Back()` and `p.step("back")`. `Quick picks` shows again. For scroll position, mirror `TestGoingBackRestoresWhereAPageWasScrolled`, which uses `/settings`, `tt.Scroll(500, 300, 0, 1500)` and `a.list.Visible()`.
- **New route.** Add the route's path to the table in `TestEveryPageLoadsAndDraws` style, push it, and show its heading in the screenshot.
- **Album cover from a credit.** Use `tonesApp(t, 600)`, return `r.file` from `r.url`, and supply `redArt` through `a.thumbs` (close both caches). Open `/playlist/VLPL_album_cover`, click `Linked Track`, then `Go to album: After Hours` from either the bar or Now Playing. The route is `/album/MPREb_nested`, the page shows `Album Track One`, and its artwork is `https://img.test/album.png`. Wait for the center of `More options for After Hours` to turn red; do not prefetch the artwork in the test. See `TestAlbumCoverLoadsFromPlayerMetadata`. As a control, open `Deep Focus` from Home: its legacy header still loads.

## Gotchas

- `/library` and `/recap` need `signedInApp(t)`. Signed out they show a sign-in prompt, which is also worth a screenshot.
- `Playlists` needs no account. A tester that clicks the rail entry can draw that page blank while the text tree is right. Open a new tester after `a.router.Push("/playlists")` to capture it. A saved song's label is `Saved` plus its title. The page is a list, so a song past the bottom of the window is found only after scrolling. Click the playlist name to close it, and click again to open it.
- Compare the heading, not the path. A route that failed to match draws the `Not found` page at a valid-looking path.
- `a.router.Push` is the test's shortcut for navigation. Also click one real entry point per proof.
- Current album responses put `musicResponsiveHeaderRenderer` in the primary tab's section list. Its `thumbnail` is the album cover; `straplineThumbnail` is an artist portrait. Never fill missing page art from tracks or related albums.
- A card reads the pointer only while its menu is opening, and the picture passes the pointer through to the card. The hover tint sits under the name, so the cover is left as it was. `TestMovingAcrossACardLeavesTheWindowSettled` moves across `Deep Focus` and expects no frame left pending, then a right click that offers `Open album` without leaving Home.
