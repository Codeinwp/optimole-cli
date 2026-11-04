package export

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Codeinwp/optimole-cli/internal/api"
)

// ManifestName is the file next to the domain folders that describes the
// export: its status and, for each domain, its folder and archives. It holds
// no download URLs.
const ManifestName = "export.json"

type manifest struct {
	ID        int64            `json:"id"`
	Status    string           `json:"status"`
	CreatedAt string           `json:"created_at,omitempty"`
	Domains   []manifestDomain `json:"domains"`
}

type manifestDomain struct {
	Domain   string            `json:"domain"`
	Folder   string            `json:"folder"`
	Status   string            `json:"status"`
	Archives []manifestArchive `json:"archives"`
}

type manifestArchive struct {
	PartNumber int64 `json:"part_number"`
	// File is relative to the export folder, "/"-separated.
	File string `json:"file"`
	// Size is the size the export listed, -1 when it gave none.
	Size int64 `json:"size"`
	// Downloaded: the file on disk is this archive, complete and checked.
	Downloaded bool `json:"downloaded"`
}

// writeManifest writes ManifestName into dir from the export's last answer,
// replacing an earlier one in one step.
func writeManifest(dir string, exp *api.Export, folders map[string]string, parts map[partKey]*part) error {
	m := manifest{ID: exp.ID, Status: exp.Status, CreatedAt: exp.CreatedAt, Domains: []manifestDomain{}}
	for _, d := range exp.Domains {
		md := manifestDomain{Domain: d.Domain, Folder: folders[d.Domain], Status: d.Status, Archives: []manifestArchive{}}
		for _, lp := range d.Parts {
			if !lp.Number.Valid || lp.Number.Value <= 0 {
				continue
			}
			ma := manifestArchive{
				PartNumber: lp.Number.Value,
				File:       md.Folder + "/" + fmt.Sprintf("images-%d.zip", lp.Number.Value),
				Size:       -1,
			}
			if lp.Size.Valid {
				ma.Size = lp.Size.Value
			}
			if p := parts[partKey{d.Domain, lp.Number.Value}]; p != nil && p.state == done && (ma.Size < 0 || ma.Size == p.doneSize) {
				ma.Downloaded = true
			}
			md.Archives = append(md.Archives, ma)
		}
		m.Domains = append(m.Domains, md)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("writing %s: %w", ManifestName, err)
	}
	path := filepath.Join(dir, ManifestName)
	tmp := path + partSuffix
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("writing %s: %w", ManifestName, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("writing %s: %w", ManifestName, err)
	}
	return nil
}
