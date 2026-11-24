package templates

import (
	"bytes"
	"io/fs"
	"os"
	"testing"
)

// Deploy assets are shipped to generated projects verbatim; the repository
// copies under deploy/ and scripts/ are the ones that are tested. Keep them identical.
func TestDeployAssetsMatchRepository(t *testing.T) {
	pairs := map[string]string{
		"app/deploy/lib.sh":                "../deploy/lib.sh",
		"app/deploy/install.sh":            "../deploy/install.sh",
		"app/deploy/upgrade.sh":            "../deploy/upgrade.sh",
		"app/deploy/rollback.sh":           "../deploy/rollback.sh",
		"app/deploy/litestream/restore.sh": "../deploy/litestream/restore.sh",
		"app/deploy/prometheus/alerts.yml": "../deploy/prometheus/alerts.yml",
		"app/scripts/release.sh":           "../scripts/release.sh",
	}
	for tmpl, repo := range pairs {
		a, err := fs.ReadFile(App, tmpl)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(repo)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Errorf("templates/%s differs from %s; copy the repository version", tmpl, repo)
		}
	}
}
