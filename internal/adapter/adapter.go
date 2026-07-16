// Package adapter defines the contract between the sync engine and a specific
// platform (Notion, Feishu, ...). The engine knows nothing about any platform;
// it only knows this interface. This is both decoupling and a door
// left open for future sources (Yuque, Confluence, ...).
//
// This package defines the interface only; the concrete adapters live in the
// feishu and notion packages.
package adapter

import "context"

// DocType is the platform-agnostic classification of a remote document. Its
// value space matches manifest documents.type.
type DocType string

const (
	// TypePage is an ordinary content page.
	TypePage DocType = "page"
	// TypeDB is a Notion database (expanded into rows separately).
	TypeDB DocType = "db"
	// TypeDBRow is a single row/page inside a database.
	TypeDBRow DocType = "db_row"
	// TypeDocx is a Feishu docx document.
	TypeDocx DocType = "docx"
	// TypeFolder is a container node with no body of its own (e.g. a drive folder).
	TypeFolder DocType = "folder"
	// TypeSheet is a Feishu spreadsheet node (independent resource; placeholder).
	TypeSheet DocType = "sheet"
	// TypeBitable is a Feishu multi-dimensional table node (independent resource; placeholder).
	TypeBitable DocType = "bitable"
	// TypeMindnote is a Feishu mind-note node (independent resource; placeholder).
	TypeMindnote DocType = "mindnote"
	// TypeSlides is a Feishu slides node (independent resource; placeholder).
	TypeSlides DocType = "slides"
	// TypeFile is a Feishu drive attachment/file node (independent resource; placeholder).
	TypeFile DocType = "file"
)

// RemoteDoc is one entry produced by enumeration: enough metadata to diff
// against the manifest and to place the document in the tree, without its body.
type RemoteDoc struct {
	// ID is the platform-native stable identifier (page_id / obj_token).
	ID string
	// AltID is an optional secondary identifier that also resolves to this
	// document. Feishu wiki nodes set it to the node_token, since wiki URLs
	// (/wiki/<node_token>) reference the node rather than its obj_token; the
	// engine records it so internal-link rewriting can resolve a /wiki/ URL to
	// this document. Empty when the platform has no such alias.
	AltID string
	// Type classifies the node.
	Type DocType
	// ParentID is the ID of the parent node, or "" for a root-level node.
	ParentID string
	// Title is the current remote title.
	Title string
	// EditedAt is the remote last-edited timestamp, as reported by the
	// platform (RFC3339 or platform-native string; the adapter documents it).
	EditedAt string
	// URL is the canonical online URL, used for frontmatter traceability.
	URL string
}

// AssetRef points at a downloadable asset (image/attachment) referenced by a
// document body. The engine dedupes and downloads via the manifest assets
// table keyed on RemoteKey.
type AssetRef struct {
	// RemoteKey is the platform-stable key (Notion: signed-URL path; Feishu:
	// asset token). It is the dedupe/primary key, never the temporary URL.
	RemoteKey string
	// URL is the (short-lived) download URL; fetch it immediately.
	URL string
	// BodyURL is the exact substring, as it appears in FetchResult.Body, that
	// the engine replaces with the local relative asset path once the asset is
	// downloaded (or after which it appends the opendoc:asset-pending marker on
	// failure). It is empty when the asset is not referenced inline in the body.
	// Keeping the body-facing text here lets the engine stay platform-agnostic:
	// Feishu computes it by correlating the markdown image URL with the XML asset
	// token; Notion (later) sets it equal to the signed URL.
	BodyURL string
	// Filename is the suggested original filename, when the platform provides it.
	Filename string
}

// DocRef is a reference from one document to another online document, extracted
// from the body during fetch and later rewritten to a relative path.
type DocRef struct {
	// TargetID is the referenced document's platform-native ID.
	TargetID string
	// RawURL is the original online URL as it appears in the body.
	RawURL string
}

// FetchResult is the product of fetching a single document's body.
type FetchResult struct {
	// Markdown is the raw document body as produced by the platform, before any
	// opendoc processing (degradation, asset links, internal links). content_hash is
	// taken over this raw product so it stays stable across runs and
	// is unaffected by degradation output that depends on live external data
	// (e.g. bitable rows that change online independently of the document).
	Markdown string
	// Body is the degraded body ready for asset/link rewrite: the same content
	// as Markdown but with lossy resource blocks handled per the degradation
	// contract — bitables rendered, whiteboard tokens annotated,
	// unknown blocks preserved. When empty the engine falls back to Markdown
	// (adapters that do no degradation need not set it).
	Body string
	// Assets are the asset references found in the body.
	Assets []AssetRef
	// Links are the document references found in the body.
	Links []DocRef
	// Degradation counts the lossy conversions applied while producing Body, for
	// the sync report.
	Degradation Degradation
}

// Degradation is the per-document breakdown of lossy conversions, folded into
// the sync report's degradation counts.
type Degradation struct {
	// UnknownBlocks is the number of unrecognized XML/HTML-ish tags preserved
	// verbatim (the red line: never silently drop).
	UnknownBlocks int
	// BitablesRendered is the number of bitables rendered inline as a table.
	BitablesRendered int
	// BitablesOversize is the number of bitables that exceeded the inline row
	// threshold and were degraded to schema + row count + link.
	BitablesOversize int
	// BitablesFailed is the number of bitables whose API fetch failed and were
	// degraded to tag-in-comment + link.
	BitablesFailed int
	// TruncatedPages is the number of Notion pages the markdown endpoint reported
	// as truncated (too many blocks); the loss is preserved with a body marker.
	TruncatedPages int
	// UnknownBlockIDs is the number of block IDs the Notion markdown endpoint
	// could not render (its unknown_block_ids field); recorded so the loss is
	// never silent, and surfaced with a body marker.
	UnknownBlockIDs int
}

// Total returns the aggregate degradation count.
func (d Degradation) Total() int {
	return d.UnknownBlocks + d.BitablesRendered + d.BitablesOversize + d.BitablesFailed +
		d.TruncatedPages + d.UnknownBlockIDs
}

// PropertyKV is one rendered database-row property: a human-readable key and its
// rendered value (see docs/notion-properties-mapping.md). Both are plain strings
// so every property is greppable and the frontmatter stays uniform.
type PropertyKV struct {
	// Key is the property name as shown online.
	Key string
	// Value is the rendered, human-readable value. Lossy types leave a drill-down
	// trail (page ids for relations, a source-property note for array rollups)
	// rather than being silently dropped.
	Value string
}

// RowProperties is one database row's rendered properties plus a canonical
// serialization used for change detection. Entries are in deterministic order so
// the frontmatter and the row index render byte-stably across runs.
type RowProperties struct {
	// Entries are the row's rendered properties, in deterministic order.
	Entries []PropertyKV
	// Canonical is a stable serialization of Entries, folded into the row's
	// content_hash so a property-only edit is detected as dirty even when the body
	// is unchanged. It stays pre-rewrite like every hash input.
	Canonical string
}

// DatabaseExpander is the optional capability an adapter implements when its
// databases (TypeDB nodes) expand into rows whose properties are fetched
// separately from their bodies (Notion's data_sources query). The
// engine detects it by type assertion and calls it once per db node per sync, so
// a single query yields every row's properties rather than one request per row.
type DatabaseExpander interface {
	// QueryDatabaseRows returns every row's rendered properties for the database
	// node dbDoc, keyed by the row's manifest id (the canonical page id the row's
	// RemoteDoc also carries). titles maps every enumerated document id to its
	// title, so relation values can render human-readable target titles when the
	// target is a mirrored page; unresolved targets fall back to the id.
	QueryDatabaseRows(ctx context.Context, dbDoc RemoteDoc, titles map[string]string) (map[string]RowProperties, error)
}

// IncrementalEnumerator is the optional capability an adapter implements when it
// can enumerate only the documents changed since a checkpoint, instead of the
// full inventory (Notion: POST /v1/search sorted by last_edited_time descending,
// paginating only until entries fall before the checkpoint). The engine
// uses it for incremental rounds; an adapter without it always runs full rounds
// (Feishu, whose wiki recursion + drive pagination is inherently a full inventory
// every round, so every round reconciles).
//
// Enumeration being incremental changes the engine's contract: absence from the
// result is NOT a delete signal, so the engine disables delete/move detection for
// an incremental round and resolves placement of the changed docs against the
// manifest (unenumerated ancestors), not the in-round tree.
type IncrementalEnumerator interface {
	// EnumerateIncremental returns the documents whose last edit is at or after
	// checkpoint minus a safety window, plus the new checkpoint to persist (the max
	// last-edited time observed this round, never below the passed checkpoint). The
	// safety window and the content-hash skip together absorb the re-enumerated but
	// unchanged docs. checkpoint is the RFC3339 UTC value persisted by a
	// previous run; the engine only calls this when a usable checkpoint exists.
	EnumerateIncremental(ctx context.Context, checkpoint string) (docs []RemoteDoc, newCheckpoint string, err error)
}

// Adapter is the platform contract the engine drives.
type Adapter interface {
	// Platform returns the manifest platform tag ("notion" | "feishu").
	Platform() string

	// Enumerate streams the authorized document inventory. Implementations
	// send each RemoteDoc on the returned channel and close it when done; any
	// terminal error is delivered via the error channel. Respecting ctx
	// cancellation is required. Enumeration is metadata-only — no bodies.
	Enumerate(ctx context.Context) (<-chan RemoteDoc, <-chan error)

	// FetchMarkdown fetches one document's body, returning the raw markdown plus
	// the degraded body, assets, links, and degradation counts. The whole
	// RemoteDoc is passed (not just its ID) so the adapter can use the online
	// URL when building drill-down links for degraded resource blocks.
	FetchMarkdown(ctx context.Context, doc RemoteDoc) (FetchResult, error)

	// DownloadAsset downloads the asset identified by ref to destPath. The URL
	// in ref is short-lived, so this must fetch immediately.
	DownloadAsset(ctx context.Context, ref AssetRef, destPath string) error
}
