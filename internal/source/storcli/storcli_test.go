package storcli

import (
	"os"
	"strings"
	"testing"
)

func readFixture(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read %s: %v", path, err)
	}
	return string(data)
}

func useFixtures(t *testing.T) {
	t.Helper()
	vd := readFixture(t, "../../../tests/testdata/storcli-vd.json")
	pd := readFixture(t, "../../../tests/testdata/storcli-pd-all.json")
	SetRunner(func(args ...string) (string, error) {
		if strings.Join(args, " ") == "/c0 /vall show J" {
			return vd, nil
		}
		if strings.Join(args, " ") == "/c0 /eall /sall show all J" {
			return pd, nil
		}
		return "", os.ErrNotExist
	})
	t.Cleanup(func() { SetRunner(nil) })
}

func TestParseDGVD(t *testing.T) {
	if dg, vd := parseDGVD("0/0"); dg != 0 || vd != 0 {
		t.Errorf("parseDGVD(0/0): got %d/%d", dg, vd)
	}
	if dg, vd := parseDGVD("1/3"); dg != 1 || vd != 3 {
		t.Errorf("parseDGVD(1/3): got %d/%d", dg, vd)
	}
	if dg, vd := parseDGVD("bad"); dg != -1 || vd != -1 {
		t.Errorf("parseDGVD(bad): got %d/%d want -1/-1", dg, vd)
	}
}

func TestParseSize(t *testing.T) {
	// storcli TB/GB are binary units: 1.090 TiB, 893.750 GiB.
	tib := float64(1 << 40)
	gib := float64(1 << 30)
	if got := parseSize("1.090 TB"); got != uint64(1.090*tib) {
		t.Errorf("parseSize(1.090 TB): got %d", got)
	}
	if got := parseSize("893.750 GB"); got != uint64(893.750*gib) {
		t.Errorf("parseSize(893.750 GB): got %d", got)
	}
	if got := parseSize("garbage"); got != 0 {
		t.Errorf("parseSize(garbage): got %d want 0", got)
	}
}

func TestVDsFromFixture(t *testing.T) {
	useFixtures(t)
	vds := Default().VDs()
	if len(vds) != 2 {
		t.Fatalf("expected 2 VDs, got %d: %+v", len(vds), vds)
	}
	byDG := map[int]VDInfo{}
	for _, v := range vds {
		byDG[v.DG] = v
	}
	if byDG[0].RAIDLevel != "RAID1" || byDG[1].RAIDLevel != "RAID0" {
		t.Errorf("RAID levels: DG0=%q DG1=%q want RAID1/RAID0", byDG[0].RAIDLevel, byDG[1].RAIDLevel)
	}
	// 1.090 TiB ≈ 1.198 TB (sda is 1199.7 GB decimal).
	tib := float64(1 << 40)
	if byDG[0].SizeBytes != uint64(1.090*tib) {
		t.Errorf("DG0 size: got %d", byDG[0].SizeBytes)
	}
}

func TestPDsFromFixture(t *testing.T) {
	useFixtures(t)
	pds := Default().PDs()
	if len(pds) != 4 {
		t.Fatalf("expected 4 PDs, got %d: %+v", len(pds), pds)
	}
	byDID := map[int]PDInfo{}
	for _, p := range pds {
		byDID[p.DID] = p
	}
	// Seagate pair on DG 0 with serials (matches smartctl megaraid,4/5).
	if byDID[4].DG != 0 || byDID[5].DG != 0 {
		t.Errorf("Seagate DG: DID4=%d DID5=%d want 0/0", byDID[4].DG, byDID[5].DG)
	}
	if byDID[4].Serial != "WFK6ECL40000C025G6HT" {
		t.Errorf("DID4 serial: got %q", byDID[4].Serial)
	}
	// Samsung pair on DG 1.
	if byDID[0].DG != 1 || byDID[1].DG != 1 {
		t.Errorf("Samsung DG: DID0=%d DID1=%d want 1/1", byDID[0].DG, byDID[1].DG)
	}
	if byDID[0].Media != "SSD" || byDID[4].Media != "HDD" {
		t.Errorf("media: DID0=%q DID4=%q want SSD/HDD", byDID[0].Media, byDID[4].Media)
	}
}

func TestUnavailable(t *testing.T) {
	// Empty candidates => discovery fails => everything degrades to nil.
	old := candidatePaths
	SetCandidates([]string{"/nonexistent/storcli"})
	defer func() { candidatePaths = old }()
	s := &defaultSource{}
	if s.Available() {
		t.Fatal("source should be unavailable with nonexistent candidates")
	}
	if vds := s.VDs(); vds != nil {
		t.Errorf("VDs should be nil when unavailable, got %v", vds)
	}
	if pds := s.PDs(); pds != nil {
		t.Errorf("PDs should be nil when unavailable, got %v", pds)
	}
}

func TestQueryFailureDegrades(t *testing.T) {
	SetRunner(func(args ...string) (string, error) { return "", os.ErrPermission })
	defer SetRunner(nil)
	s := &defaultSource{run: func(args ...string) (string, error) { return "", os.ErrPermission }}
	if vds := s.VDs(); vds != nil {
		t.Errorf("VDs on query failure should be nil, got %v", vds)
	}
	if pds := s.PDs(); pds != nil {
		t.Errorf("PDs on query failure should be nil, got %v", pds)
	}
}
