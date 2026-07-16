package engine

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/arcships/open-doc-cli/internal/adapter"
	"github.com/arcships/open-doc-cli/internal/naming"
)

// treeNode is one node of the placement tree built from an adapter's enumerated
// RemoteDocs. It carries both the remote metadata and the computed on-disk
// placement (directory layout and naming).
type treeNode struct {
	doc      adapter.RemoteDoc
	order    int // enumeration order, for stable same-directory dedup
	children []*treeNode

	// isDir is true when the node renders as a directory (it has children or is
	// a folder). hasBody is true when the node owns a Markdown body (content or
	// placeholder resource — folders do not).
	isDir   bool
	hasBody bool

	// dirPath is the node's directory (relative to the mirror root) when isDir.
	// bodyPath is where its body file lives: <dirPath>/README.md for a directory
	// with a body, or <parentDir>/<name>.md for a leaf. Empty when hasBody is
	// false.
	dirPath    string
	bodyPath   string
	breadcrumb string

	// literalName, when non-empty, forces the on-disk path component to this exact
	// value, bypassing naming/dedup. It is used only for structural folders the
	// engine synthesizes (the reserved "_orphans" bucket), whose name must be the
	// literal reserved word rather than a slugged/suffixed title.
	literalName string
}

// orphansFolderName is the reserved directory that collects nodes whose parent
// chain does not resolve to a reachable root. It is created per
// platform only when at least one such orphan exists.
const orphansFolderName = "_orphans"

// isContainerType reports whether a DocType is a pure container with no body of
// its own. A Notion database (TypeDB) is a container: it renders as a directory
// holding a generated `_index.md` row index plus its rows, and — being a
// collection, not a page — owns no README, like a folder.
func isContainerType(t adapter.DocType) bool {
	return t == adapter.TypeFolder || t == adapter.TypeDB
}

// buildTree assembles the placement tree for one platform's RemoteDocs. platform
// is the top-level directory (e.g. "feishu"). A node whose ParentID is empty is a
// genuine root (placed directly under platform). A node whose ParentID is
// non-empty but names no enumerated node is an orphan — its parent chain hit an
// unauthorized/invisible node — and is collected under a synthetic platform-level
// "_orphans" folder so a gap never drops a document nor scatters it at the root.
// Placement (names, paths, breadcrumbs) is computed in enumeration
// order for deterministic, stable dedup.
func buildTree(platform string, docs []adapter.RemoteDoc) []*treeNode {
	nodes := make(map[string]*treeNode, len(docs))
	for i, d := range docs {
		nodes[d.ID] = &treeNode{doc: d, order: i}
	}

	var roots []*treeNode
	var orphans []*treeNode
	for _, n := range nodes {
		if n.doc.ParentID == "" {
			roots = append(roots, n)
			continue
		}
		parent, ok := nodes[n.doc.ParentID]
		if !ok {
			orphans = append(orphans, n)
			continue
		}
		parent.children = append(parent.children, n)
	}

	// Collect orphans under a synthetic reserved "_orphans" folder so they keep a
	// stable, predictable home instead of polluting the platform root.
	if len(orphans) > 0 {
		orphanRoot := &treeNode{
			doc: adapter.RemoteDoc{
				ID:    platform + ":" + orphansFolderName,
				Type:  adapter.TypeFolder,
				Title: orphansFolderName,
			},
			order:       len(docs) + 1, // sort after every real root
			literalName: orphansFolderName,
			children:    orphans,
		}
		nodes[orphanRoot.doc.ID] = orphanRoot
		roots = append(roots, orphanRoot)
	}

	// Classify every node.
	for _, n := range nodes {
		n.isDir = len(n.children) > 0 || isContainerType(n.doc.Type)
		n.hasBody = !isContainerType(n.doc.Type)
	}

	sortByOrder(roots)
	for _, n := range nodes {
		sortByOrder(n.children)
	}

	// Assign paths from the roots down. Roots share the platform directory's
	// namespace; each directory gets its own fresh dedup set.
	assignPaths(roots, platform, "")
	return roots
}

// sortByOrder orders siblings by enumeration order so the "first arrival keeps
// the clean name" rule is deterministic.
func sortByOrder(ns []*treeNode) {
	sort.SliceStable(ns, func(i, j int) bool { return ns[i].order < ns[j].order })
}

// assignPaths computes dirPath/bodyPath/breadcrumb for each node in ns, whose
// parent directory is parentDir and whose ancestor breadcrumb is breadcrumb.
func assignPaths(ns []*treeNode, parentDir, breadcrumb string) {
	used := map[string]bool{}
	for _, n := range ns {
		var name string
		if n.literalName != "" {
			// Structural folder (e.g. "_orphans"): keep the literal name but still
			// claim it in the dedup set so a sibling can never take the same slot.
			name = n.literalName
			used[strings.ToLower(name)] = true
		} else {
			name = naming.Component(n.doc.Title, n.doc.ID, used)
		}
		n.breadcrumb = breadcrumb
		if n.isDir {
			n.dirPath = filepath.Join(parentDir, name)
			if n.hasBody {
				n.bodyPath = filepath.Join(n.dirPath, "README.md")
			}
			assignPaths(n.children, n.dirPath, joinBreadcrumb(breadcrumb, n.doc.Title))
		} else {
			n.bodyPath = filepath.Join(parentDir, name+".md")
		}
	}
}

// joinBreadcrumb appends title to an existing breadcrumb with the " / "
// separator, using the raw (unslugged) title as the online breadcrumb signal.
// Empty ancestor titles are skipped so they never leave a dangling separator.
func joinBreadcrumb(prefix, title string) string {
	if title == "" {
		return prefix
	}
	if prefix == "" {
		return title
	}
	return prefix + " / " + title
}

// walk returns all nodes of the tree in a stable pre-order (parents before
// children), which the pipeline consumes so a directory exists before its
// children are written.
func walk(roots []*treeNode) []*treeNode {
	var out []*treeNode
	var rec func(n *treeNode)
	rec = func(n *treeNode) {
		out = append(out, n)
		for _, c := range n.children {
			rec(c)
		}
	}
	for _, r := range roots {
		rec(r)
	}
	return out
}
