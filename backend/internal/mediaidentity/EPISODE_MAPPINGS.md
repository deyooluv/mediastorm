# Release episode mappings

Catalog identities remain authoritative for UI, history and scrobbling. Search
uses `ReleaseEpisodeAliases` to add verified release coordinates; filtering
binds each result to the numbering declared by that release. Prequeue must
preserve those bindings when it adds catalog metadata. Multi-season packs carry
explicit alternate episode codes into both Usenet and debrid file selection.

Sources:

- [Anime-Lists](https://github.com/Anime-Lists/anime-lists): TMDB → AniDB → TVDB,
  using default offsets, bounded ranges and individual episode overrides.
  Later cour offsets bound earlier defaults. Numeric series IDs are required;
  movie IDs and unknown entries cannot establish a series mapping.
- [TheXEM](https://thexem.info/): TVDB → scene episode coordinates and verified
  absolute numbers. TMDB catalogs require an Anime-Lists bridge first; a
  show-level TVDB ID alone does not prove equal episode numbering. Native TVDB
  catalog identities can use XEM directly, including non-anime series.
- Manual season offsets (`season_offsets.go`): TMDB entries that restart at
  S1 after a channel move or revival (Channel 4 Bake Off, Netflix Top Boy,
  MST3K) alias to the continuous TVDB/IMDb seasons. They add release queries
  only while TMDB numbers the episodes and never change scrobble identity.
  Split seasons (Unsolved Mysteries 2020 S3 = TVDB S17+S18) and spin-offs whose
  IMDb and TVDB numbering disagree (An Extra Slice) are deliberately absent.

Beyond those reviewed offsets there is no guessed cour length or title-specific
exception. Conflicting, non-bijective (split/combined), unmapped, and special-to-regular relationships
are not accepted as interchangeable whole episodes. Absolute `a` defaults
require explicit range mappings; they are not interpreted as a season number.
Same-season ambiguous packs keep the catalog behavior rather than guessing.

Upstream payloads and HTTP validators persist in PostgreSQL's
`episode_mapping_cache`. Refresh is due after 24 hours; the background worker
checks hourly. Valid stale snapshots remain usable during refresh/outages.
Failures back off for five minutes, and valid empty XEM mappings cache for a
day. Cold XEM lookups have a four-second budget; all HTTP work is bounded.
Search-result cache keys include the active aliases so a newly learned mapping
cannot reuse a previous search that omitted its release coordinates.

Offline tests use a small unmodified Anime-Lists subset in
`../mappingtest/testdata`, including a TheXEM Kaiju response fetched on
2026-09-24 from `https://thexem.info/map/all?origin=tvdb&id=423075`. To validate a complete downloaded upstream snapshot:

```sh
STRMR_ANIME_MAPPING_SNAPSHOT=/path/to/anime-list-master.xml go test ./internal/mediaidentity
```

Episode coordinates carry `numbering: {seriesId, ordering}` separately from the
catalog title ID. Metadata stamps the provider that actually supplied episodes,
including lite, cached and fallback responses. Clients carry this context through
prequeue and manual search. A later provider fallback cannot reinterpret an
already selected episode or supply counts/absolute numbers from another order.
Legacy requests without context use the actual hydrated metadata provider.

Anime-Lists also supplies TVDB → TMDB aliases after a global round-trip check.
TMDB-numbered text queries do not pair those coordinates with TVDB indexer IDs.
Only official/unspecified order uses these community mappings; DVD and custom
orders are excluded. Search caches, prequeue reuse and mapped selection hints
include numbering provenance. No metadata API keys are required by the mapping
sources themselves; TMDB and TVDB keys continue to control metadata availability.
