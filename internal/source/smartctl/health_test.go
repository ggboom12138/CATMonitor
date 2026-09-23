package smartctl

import (
	"testing"
	"time"
)

func TestParseScan(t *testing.T) {
	out := readMock(t, "../../../tests/testdata/smartctl-scan-json.txt")
	entries := parseScan(out)
	if len(entries) < 6 {
		t.Fatalf("expected at least 6 scan entries, got %d", len(entries))
	}
	var raid *ScanEntry
	for i := range entries {
		if entries[i].Type == "megaraid,0" {
			raid = &entries[i]
		}
	}
	if raid == nil {
		t.Fatal("expected a megaraid,0 channel in scan output")
	}
	if raid.Name != "/dev/bus/0" {
		t.Errorf("megaraid channel name: got %q want /dev/bus/0", raid.Name)
	}
	var ld *ScanEntry
	for i := range entries {
		if entries[i].Name == "/dev/sdb" {
			ld = &entries[i]
		}
	}
	if ld == nil {
		t.Fatal("expected /dev/sdb (logical volume) in scan output")
	}
	if ld.Type != "scsi" {
		t.Errorf("logical volume type: got %q want scsi", ld.Type)
	}
}

func TestScanMockInject(t *testing.T) {
	want := readMock(t, "../../../tests/testdata/smartctl-scan-json.txt")
	SetScanFetcher(func() (string, error) { return want, nil })
	defer ResetFetcher()

	entries := Default().Scan()
	if len(entries) == 0 {
		t.Fatal("expected scan entries with mock, got none")
	}
}

func TestScanCacheHitsWithinTTL(t *testing.T) {
	SetCacheTTL(1 * time.Hour)
	defer SetCacheTTL(defaultCacheTTL)
	defer ResetFetcher()

	calls := 0
	want := readMock(t, "../../../tests/testdata/smartctl-scan-json.txt")
	SetScanFetcher(func() (string, error) { calls++; return want, nil })

	Default().Scan()
	Default().Scan()
	if calls != 1 {
		t.Errorf("scan fetcher should be called once (cache hit), got %d", calls)
	}
}

func TestScanCachesFailure(t *testing.T) {
	SetCacheTTL(1 * time.Hour)
	defer SetCacheTTL(defaultCacheTTL)
	defer ResetFetcher()

	calls := 0
	SetScanFetcher(func() (string, error) { calls++; return "", errFail })
	if entries := Default().Scan(); entries != nil {
		t.Errorf("failed scan should return nil, got %v", entries)
	}
	if entries := Default().Scan(); entries != nil {
		t.Errorf("second scan should stay nil (negative cache), got %v", entries)
	}
	if calls != 1 {
		t.Errorf("failed scan fetcher should be cached (1 call), got %d", calls)
	}
}

// TestParseHealthJSONATA asserts the normalized HealthInfo of the real
// Samsung PM983 SATA SSD captured behind a megaraid channel.
func TestParseHealthJSONATA(t *testing.T) {
	out := readMock(t, "../../../tests/testdata/smartctl-ata-json.txt")
	hi := parseHealthJSON(out)
	if hi == nil {
		t.Fatal("expected HealthInfo, got nil")
	}
	if hi.Model != "SAMSUNG MZ7LH960HAJR-00005" {
		t.Errorf("Model: got %q", hi.Model)
	}
	if hi.Serial != "S45NNA0N662029" {
		t.Errorf("Serial: got %q", hi.Serial)
	}
	if hi.Interface != "SATA" {
		t.Errorf("Interface: got %q want SATA", hi.Interface)
	}
	if hi.Media != "ssd" {
		t.Errorf("Media: got %q want ssd", hi.Media)
	}
	if !hi.HasSMART || !hi.Passed {
		t.Errorf("SMART: HasSMART=%v Passed=%v want true/true", hi.HasSMART, hi.Passed)
	}
	if hi.CapacityBytes != 960197124096 {
		t.Errorf("CapacityBytes: got %d want 960197124096", hi.CapacityBytes)
	}
	if hi.Temperature != 37 {
		t.Errorf("Temperature: got %v want 37", hi.Temperature)
	}
	// Wear_Leveling_Count normalized value 99 -> 100-99 = 1% worn.
	if hi.WearPercent != 1 {
		t.Errorf("WearPercent: got %v want 1", hi.WearPercent)
	}
	if hi.PowerOnHours != 28599 {
		t.Errorf("PowerOnHours: got %v want 28599", hi.PowerOnHours)
	}
	if hi.PowerCycles != 122 {
		t.Errorf("PowerCycles: got %v want 122", hi.PowerCycles)
	}
	// Total_LBAs_Written raw (fixture is a live capture) x 512B blocks.
	if hi.WrittenBytes != 11535690618*512 {
		t.Errorf("WrittenBytes: got %d want %d", hi.WrittenBytes, 11535690618*512)
	}
	if hi.ReallocatedSectors != 0 {
		t.Errorf("ReallocatedSectors: got %v want 0", hi.ReallocatedSectors)
	}
	// NVMe-only fields are unavailable on ATA.
	if hi.MediaErrors != -1 || hi.AvailableSpare != -1 || hi.UnsafeShutdowns != -1 {
		t.Errorf("NVMe-only fields should be -1 on ATA, got media_errors=%v spare=%v unsafe=%v",
			hi.MediaErrors, hi.AvailableSpare, hi.UnsafeShutdowns)
	}
}

// TestParseHealthJSONSCSI asserts a SAS HDD (Seagate behind megaraid) is
// classified as rotational and degrades cleanly on missing fields.
func TestParseHealthJSONSCSI(t *testing.T) {
	out := readMock(t, "../../../tests/testdata/smartctl-scsi-json.txt")
	hi := parseHealthJSON(out)
	if hi == nil {
		t.Fatal("expected HealthInfo, got nil")
	}
	if hi.Media != "hdd" {
		t.Errorf("Media: got %q want hdd", hi.Media)
	}
	if hi.Interface != "SAS" {
		t.Errorf("Interface: got %q want SAS", hi.Interface)
	}
	if hi.Temperature != 31 {
		t.Errorf("Temperature: got %v want 31", hi.Temperature)
	}
	if hi.PowerOnHours != 46545 {
		t.Errorf("PowerOnHours: got %v want 46545", hi.PowerOnHours)
	}
	if hi.PowerCycles != -1 {
		t.Errorf("PowerCycles: got %v want -1 (SCSI reports none)", hi.PowerCycles)
	}
	if !hi.HasSMART || !hi.Passed {
		t.Errorf("SMART: HasSMART=%v Passed=%v want true/true", hi.HasSMART, hi.Passed)
	}
}

// TestParseHealthJSONNVMe asserts the NVMe health-log mapping against the
// standard smartctl JSON structure (synthetic fixture).
func TestParseHealthJSONNVMe(t *testing.T) {
	out := readMock(t, "../../../tests/testdata/smartctl-nvme-json.txt")
	hi := parseHealthJSON(out)
	if hi == nil {
		t.Fatal("expected HealthInfo, got nil")
	}
	if hi.Interface != "NVMe" {
		t.Errorf("Interface: got %q want NVMe", hi.Interface)
	}
	if hi.Media != "ssd" {
		t.Errorf("Media: got %q want ssd", hi.Media)
	}
	if hi.WearPercent != 6 {
		t.Errorf("WearPercent: got %v want 6 (percentage_used)", hi.WearPercent)
	}
	if hi.AvailableSpare != 100 {
		t.Errorf("AvailableSpare: got %v want 100", hi.AvailableSpare)
	}
	if hi.UnsafeShutdowns != 7 {
		t.Errorf("UnsafeShutdowns: got %v want 7", hi.UnsafeShutdowns)
	}
	if hi.MediaErrors != 0 {
		t.Errorf("MediaErrors: got %v want 0", hi.MediaErrors)
	}
	// data_units_written x 512000 bytes per NVMe spec.
	if hi.WrittenBytes != 23456789*512000 {
		t.Errorf("WrittenBytes: got %d want %d", hi.WrittenBytes, 23456789*512000)
	}
	if hi.ReadBytes != 12345678*512000 {
		t.Errorf("ReadBytes: got %d want %d", hi.ReadBytes, 12345678*512000)
	}
	if hi.ReallocatedSectors != -1 {
		t.Errorf("ReallocatedSectors: got %v want -1 (ATA-only)", hi.ReallocatedSectors)
	}
}

func TestParseHealthJSONGarbage(t *testing.T) {
	if hi := parseHealthJSON(""); hi != nil {
		t.Errorf("empty input should parse to nil, got %+v", hi)
	}
	if hi := parseHealthJSON("not json at all"); hi != nil {
		t.Errorf("garbage input should parse to nil, got %+v", hi)
	}
}

func TestHealthJSONMockInjectAndCache(t *testing.T) {
	SetCacheTTL(1 * time.Hour)
	defer SetCacheTTL(defaultCacheTTL)
	defer ResetFetcher()

	calls := 0
	want := readMock(t, "../../../tests/testdata/smartctl-ata-json.txt")
	var gotDev, gotType string
	SetJSONFetcher(func(devPath, devType string) (string, error) {
		calls++
		gotDev, gotType = devPath, devType
		return want, nil
	})

	hi := Default().HealthJSON("/dev/bus/0", "megaraid,0")
	if hi == nil {
		t.Fatal("expected HealthInfo with mock, got nil")
	}
	if gotDev != "/dev/bus/0" || gotType != "megaraid,0" {
		t.Errorf("fetcher args: got (%q,%q) want (/dev/bus/0, megaraid,0)", gotDev, gotType)
	}
	hi2 := Default().HealthJSON("/dev/bus/0", "megaraid,0")
	if hi2 == nil {
		t.Fatal("second call should hit cache, got nil")
	}
	if calls != 1 {
		t.Errorf("json fetcher should be called once (cache), got %d", calls)
	}
	// Different devType is a different cache entry.
	Default().HealthJSON("/dev/bus/0", "megaraid,1")
	if calls != 2 {
		t.Errorf("different devType should re-fetch, got %d calls", calls)
	}
}

func TestHealthJSONCachesFailure(t *testing.T) {
	SetCacheTTL(1 * time.Hour)
	defer SetCacheTTL(defaultCacheTTL)
	defer ResetFetcher()

	calls := 0
	SetJSONFetcher(func(devPath, devType string) (string, error) { calls++; return "", errFail })
	if hi := Default().HealthJSON("/dev/sdb", ""); hi != nil {
		t.Errorf("failed fetch should return nil (graceful), got %+v", hi)
	}
	if hi := Default().HealthJSON("/dev/sdb", ""); hi != nil {
		t.Errorf("second call should stay nil, got %+v", hi)
	}
	if calls != 1 {
		t.Errorf("failed json fetcher should be cached (1 call), got %d", calls)
	}
}

func TestNormalizeInterface(t *testing.T) {
	cases := map[string]string{
		"ATA":  "SATA",
		"ata":  "SATA",
		"NVMe": "NVMe",
		"nvme": "NVMe",
		"SCSI": "SAS",
		"":     "",
		"USB":  "USB",
	}
	for in, want := range cases {
		if got := normalizeInterface(in); got != want {
			t.Errorf("normalizeInterface(%q): got %q want %q", in, got, want)
		}
	}
}

func TestMediaOf(t *testing.T) {
	zero, spin := 0, 10500
	cases := []struct {
		protocol string
		rr       *int
		want     string
	}{
		{"NVMe", nil, "ssd"},
		{"ATA", &zero, "ssd"},
		{"SCSI", &spin, "hdd"},
		{"ATA", nil, "unknown"},
	}
	for _, c := range cases {
		if got := mediaOf(c.protocol, c.rr); got != c.want {
			t.Errorf("mediaOf(%q, %v): got %q want %q", c.protocol, c.rr, got, c.want)
		}
	}
}
