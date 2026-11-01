package main

import (
	"testing"

	"github.com/Higangssh/hyperbutler/internal/desktopimport"
)

// TestImportSchemaFields pins `import --json` (REQ-UX-008, REQ-UX-023).
func TestImportSchemaFields(t *testing.T) {
	item := ImportItemJSON{Kind: "volume", Name: "n", ID: "i", Tags: []string{"t"}, SizeBytes: 1, Status: "new", Outcome: "imported",
		BytesCopied: 1, Reason: "r", MissingTags: []string{"m"}, AddedTags: []string{"a"}, UsedBy: []string{"u"}, Hash: "h"}
	itemKeys := []string{"kind", "name", "id", "tags", "sizeBytes", "status", "outcome", "bytesCopied", "reason", "missingTags", "addedTags", "usedBy", "hash"}
	var want []string
	for _, k := range itemKeys {
		want = append(want, "items[]."+k, "skipped[]."+k)
	}
	want = append(want, "schema", "dryRun", "source", "source.pipe", "source.via", "source.version", "target", "items", "skipped",
		"bytesCopied", "sourceCheck", "sourceCheck.unchanged", "sourceCheck.images", "sourceCheck.volumes", "sourceCheck.containers",
		"sourceCheck.hashedVolumes", "sourceCheck.differences", "sourceCheck.warnings", "canceled", "ok", "notes")
	checkKeys(t, "import", ImportJSON{Schema: 1, Source: ImportSourceJSON{Pipe: "p", Via: "context", Version: "29"}, Target: "t",
		Items: []ImportItemJSON{item}, Skipped: []ImportItemJSON{item}, BytesCopied: 1,
		SourceCheck: &desktopimport.SourceCheck{Unchanged: true, Differences: []string{"d"}, Warnings: []string{"w"}},
		Notes:       []string{"n"}}, want)
}
