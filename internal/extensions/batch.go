package extensions

import (
	"errors"
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

// This file is the panel's half of authoring: one TOML document holding many
// templates, for the export a user downloads and the import that puts a
// colleague's set of extensions back. It is the same layer the single-template
// routes use — every entry is validated by Extension.Validate before anything is
// written — so a document cannot introduce a template a hand-written file would
// not be allowed to hold.
//
// The document is deliberately the same shape an export produces, `[[extensions]]`
// tables each carrying its `name`:
//
//	[[extensions]]
//	name = "docs"
//	kind = "skill"
//	target = "internal"
//	paths = ["~/skills/docs/"]
//
// so an export imports back unchanged.

// MaxDocumentSize caps an import. Documents are small — a template is a few
// hundred bytes — and a cap keeps a runaway upload from filling the data
// directory. It matches the profile import's limit so the two behave alike.
const MaxDocumentSize = 16 << 20

// BatchRename reports a template stored under a different name than the document
// asked for, which is the one thing an import can do that the caller has to be
// told about.
type BatchRename struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// BatchImportResult is what an import did, in the terms the panel reports it.
type BatchImportResult struct {
	Imported int           `json:"imported"`
	Renamed  []BatchRename `json:"renamed,omitempty"`
}

// ExportDocument renders templates as one TOML document. An empty only exports
// every stored template; otherwise exactly the named ones — a name that no longer
// resolves is skipped rather than failing the whole file, so a selection captured
// before a deletion still downloads.
func (s *Store) ExportDocument(only []string) ([]byte, error) {
	names := only
	if len(names) == 0 {
		list, err := s.List()
		if err != nil {
			return nil, err
		}
		names = list
	}
	entries := make([]map[string]any, 0, len(names))
	for _, name := range names {
		raw, err := s.ReadRaw(name)
		if err != nil {
			if len(only) > 0 && errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("extension %q: %w", name, err)
		}
		// Round-tripped through a map rather than re-marshalled from the struct:
		// what is exported is the file that exists, so a key this build does not
		// know about still travels with it instead of being dropped on the way out.
		var entry map[string]any
		if err := toml.Unmarshal(raw, &entry); err != nil {
			return nil, fmt.Errorf("extension %q: %w", name, err)
		}
		entry["name"] = name
		entries = append(entries, entry)
	}
	return toml.Marshal(struct {
		Extensions []map[string]any `toml:"extensions"`
	}{Extensions: entries})
}

// ImportDocument validates every entry, then stores the templates under names
// nothing else is using. Nothing is overwritten: a name taken by a stored
// extension or by an earlier entry of the same file is stored under the first free
// suffixed name instead, and the result says which entries moved.
//
// All of it happens under one lock with one change notification at the end, so the
// reconciler sees a document arrive rather than one extension at a time, and a
// failure partway through rolls the written templates back out.
func (s *Store) ImportDocument(data []byte) (*BatchImportResult, error) {
	if len(data) > MaxDocumentSize {
		return nil, fmt.Errorf("import file too large (max %d bytes)", MaxDocumentSize)
	}
	// Two passes, like the profile import: the first rejects a file that carries
	// anything but `extensions`, so a document with a typo in its table header is
	// refused rather than silently importing nothing.
	var top map[string]any
	if err := toml.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("import toml: %w", err)
	}
	if len(top) != 1 {
		return nil, fmt.Errorf("import toml contains unknown top-level fields")
	}
	if _, ok := top["extensions"]; !ok {
		return nil, fmt.Errorf("import toml must contain extensions")
	}
	var doc struct {
		Extensions []map[string]any `toml:"extensions"`
	}
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("import toml: %w", err)
	}
	if len(doc.Extensions) == 0 {
		return nil, fmt.Errorf("import requires at least one extension")
	}

	type item struct {
		name string
		ext  *Extension
	}
	items := make([]item, 0, len(doc.Extensions))
	// Names the document itself claims, so a stored template is never renamed onto
	// a name this same file is about to create.
	reserved := make(map[string]bool, len(doc.Extensions))
	for i, entry := range doc.Extensions {
		name, ok := entry["name"].(string)
		if !ok || name != strings.TrimSpace(name) || sshconfig.ValidateName(name) != nil {
			return nil, fmt.Errorf("extension %d has an invalid name", i+1)
		}
		reserved[strings.ToLower(name)] = true
		raw, err := toml.Marshal(entry)
		if err != nil {
			return nil, fmt.Errorf("extension %q: %w", name, err)
		}
		ext, err := ParseTemplate(raw, name)
		if err != nil {
			return nil, fmt.Errorf("extension %q: %w", name, err)
		}
		items = append(items, item{name: name, ext: ext})
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.List()
	if err != nil {
		return nil, err
	}
	existing := make(map[string]bool, len(list)+len(items))
	for _, n := range list {
		existing[strings.ToLower(n)] = true
	}
	result := &BatchImportResult{Imported: len(items)}
	for i := range items {
		original := items[i].name
		if existing[strings.ToLower(original)] {
			items[i].name = availableDocumentName(original, existing, reserved)
			items[i].ext.Name = items[i].name
			result.Renamed = append(result.Renamed, BatchRename{From: original, To: items[i].name})
		}
		existing[strings.ToLower(items[i].name)] = true
	}

	created := make([]string, 0, len(items))
	for _, it := range items {
		if err := s.saveLocked(it.ext); err != nil {
			for _, name := range created {
				if p, cerr := s.configPath(name); cerr == nil {
					_ = s.removeLocked(p)
				}
			}
			return nil, err
		}
		created = append(created, it.name)
	}
	// An import writes many templates under one lock; each created name is
	// announced, because the runtime's unit of work is a template.
	for _, name := range created {
		s.notifyChange(name)
	}
	return result, nil
}

// ParseTemplate decodes one template from the TOML text a file holds. The name is
// the storage name the caller is saving under: a document that carries a different
// one — an exported entry pasted into the editor, say — is refused rather than
// stored under a name neither its author nor the panel would expect. A document
// with no name at all is the ordinary case: a config.toml is named by its
// directory, so the file itself never carries one.
func ParseTemplate(data []byte, name string) (*Extension, error) {
	if err := sshconfig.ValidateName(name); err != nil {
		return nil, fmt.Errorf("invalid extension name: %w", err)
	}
	ext, err := Parse(data)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Name string `toml:"name"`
	}
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("extension toml: %w", err)
	}
	if got := strings.TrimSpace(doc.Name); got != "" && got != name {
		return nil, fmt.Errorf("the document names %q, but this is being saved as %q", got, name)
	}
	ext.Name = name
	ext.Normalize()
	if err := ext.Validate(); err != nil {
		return nil, err
	}
	return ext, nil
}

// availableDocumentName finds a free name for a template whose own is taken: the
// first suffix nothing uses, counting both what is stored and what this document
// is about to store.
func availableDocumentName(name string, used, reserved map[string]bool) string {
	for n := 2; ; n++ {
		suffix := fmt.Sprintf("-%d", n)
		base := name
		if len(base)+len(suffix) > 64 {
			base = base[:64-len(suffix)]
		}
		candidate := base + suffix
		folded := strings.ToLower(candidate)
		if !used[folded] && !reserved[folded] {
			return candidate
		}
	}
}
