package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

//
// ----------- 基础小工具函数测试 -----------
//

func TestNormalizeCityKey(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{" Beijing ", "beijing"},
		{"北京", "北京"},
		{"PEKING", "peking"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeCityKey(c.in); got != c.want {
			t.Errorf("normalizeCityKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	if got := formatDuration(0); got != "--" {
		t.Errorf("formatDuration(0) = %q, want %q", got, "--")
	}
	d := 2*time.Hour + 30*time.Minute
	if got := formatDuration(d); got != "02:30" {
		t.Errorf("formatDuration(2h30m) = %q, want %q", got, "02:30")
	}
}

func TestFormatTimeLocal(t *testing.T) {
	if got := formatTimeLocal(time.Time{}); got != "--" {
		t.Errorf("formatTimeLocal(zero) = %q, want %q", got, "--")
	}
	loc := time.FixedZone("TEST", 0)
	tm := time.Date(2025, 1, 2, 3, 4, 5, 0, loc)
	if got := formatTimeLocal(tm); got != "03:04" {
		t.Errorf("formatTimeLocal(2025-01-02 03:04) = %q, want %q", got, "03:04")
	}
}

func TestParseDateInLocation(t *testing.T) {
	loc := time.FixedZone("TEST", 8*3600)
	tm, err := parseDateInLocation("2025-01-02", loc)
	if err != nil {
		t.Fatalf("parseDateInLocation returned error: %v", err)
	}
	if tm.Location() != loc {
		t.Errorf("location mismatch, got %v, want %v", tm.Location(), loc)
	}
	if tm.Hour() != 12 || tm.Minute() != 0 {
		t.Errorf("expected time at 12:00 local, got %v", tm)
	}
}

//
// ----------- 缓存相关测试（使用临时 HOME 目录） -----------
//

func TestSaveAndLoadCache(t *testing.T) {
	tmpDir := t.TempDir()
	// 在测试中把 HOME 指向临时目录，避免污染真实用户目录
	t.Setenv("HOME", tmpDir)

	cache := &CityCache{
		Entries: map[string]CityCacheEntry{
			"beijing": {
				City:        "北京",
				Normalized:  "beijing",
				DisplayName: "Beijing, China",
				Lat:         39.9,
				Lon:         116.4,
				TimezoneID:  "Asia/Shanghai",
				Aliases:     []string{"北京", "Beijing", "Peking"},
				UpdatedAt:   "2025-01-01T00:00:00Z",
			},
		},
	}

	if err := saveCache(cache); err != nil {
		t.Fatalf("saveCache error: %v", err)
	}

	loaded := loadCache()
	if len(loaded.Entries) != 1 {
		t.Fatalf("expected 1 entry in cache, got %d", len(loaded.Entries))
	}
	e, ok := loaded.Entries["beijing"]
	if !ok {
		t.Fatalf("expected key 'beijing' in loaded cache")
	}
	if e.City != "北京" || e.TimezoneID != "Asia/Shanghai" {
		t.Errorf("loaded entry mismatch: %#v", e)
	}
}

func TestFindEntryInCache(t *testing.T) {
	cache := &CityCache{
		Entries: map[string]CityCacheEntry{
			"beijing": {
				City:        "北京",
				Normalized:  "beijing",
				DisplayName: "Beijing, China",
				Lat:         39.9,
				Lon:         116.4,
				TimezoneID:  "Asia/Shanghai",
				Aliases:     []string{"Beijing", "Peking"},
				UpdatedAt:   "2025-01-01T00:00:00Z",
			},
		},
	}

	// 直接 key
	if e, ok := findEntryInCache(cache, "beijing"); !ok || e.City != "北京" {
		t.Errorf("findEntryInCache by normalized key failed, got %#v, ok=%v", e, ok)
	}

	// 按 city
	if e, ok := findEntryInCache(cache, "北京"); !ok || e.City != "北京" {
		t.Errorf("findEntryInCache by city name failed, got %#v, ok=%v", e, ok)
	}

	// 按 alias
	if e, ok := findEntryInCache(cache, "Peking"); !ok || e.City != "北京" {
		t.Errorf("findEntryInCache by alias failed, got %#v, ok=%v", e, ok)
	}
}

//
// ----------- 天文计算核心测试 -----------
//

func TestGenerateAstroDataBasic(t *testing.T) {
	// 使用 0,0 + UTC 做一个简单 sanity check
	loc, err := time.LoadLocation("UTC")
	if err != nil {
		t.Fatalf("LoadLocation(UTC) error: %v", err)
	}
	start := time.Date(2025, 1, 1, 12, 0, 0, 0, loc)

	data, err := generateAstroData("TestCity", 0, 0, loc, start, 1)
	if err != nil {
		t.Fatalf("generateAstroData error: %v", err)
	}
	if len(data) != 1 {
		t.Fatalf("expected 1 day, got %d", len(data))
	}
	d := data[0]
	if d.Date != "2025-01-01" {
		t.Errorf("expected date 2025-01-01, got %s", d.Date)
	}
	if d.DayLengthMinutes < 0 || d.DayLengthMinutes > 24*60 {
		t.Errorf("DayLengthMinutes out of range: %d", d.DayLengthMinutes)
	}
	if d.MoonIlluminationNum < 0 || d.MoonIlluminationNum > 1 {
		t.Errorf("MoonIlluminationNum out of range: %f", d.MoonIlluminationNum)
	}
}

//
// ----------- 输出函数测试（使用临时文件） -----------
//

func TestWriteAstroJSONAndReadBack(t *testing.T) {
	loc, _ := time.LoadLocation("UTC")
	now := time.Date(2025, 1, 2, 15, 4, 5, 0, loc)

	data := []dailyAstro{
		{
			Date:                "2025-01-01",
			Sunrise:             "06:00",
			Sunset:              "18:00",
			SolarNoon:           "12:00",
			MaxAltitude:         "60.00",
			DayLength:           "12:00",
			Moonrise:            "20:00",
			Moonset:             "06:00",
			MoonIllumFrac:       "50.0%",
			MaxAltitudeNum:      60.0,
			DayLengthMinutes:    720,
			MoonIlluminationNum: 0.5,
		},
	}

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "astro.json")
	out, err := writeAstroJSON("TestCity", now, data, "test range", filePath)
	if err != nil {
		t.Fatalf("writeAstroJSON error: %v", err)
	}
	if out != filePath {
		t.Errorf("writeAstroJSON returned path %q, want %q", out, filePath)
	}

	b, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}

	var wrapper struct {
		City      string       `json:"city"`
		Generated string       `json:"generated_at"`
		Range     string       `json:"range"`
		Data      []dailyAstro `json:"data"`
	}
	if err := json.Unmarshal(b, &wrapper); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if wrapper.City != "TestCity" {
		t.Errorf("wrapper.City = %q, want %q", wrapper.City, "TestCity")
	}
	if len(wrapper.Data) != 1 {
		t.Fatalf("expected 1 data entry, got %d", len(wrapper.Data))
	}
	if wrapper.Data[0].MaxAltitudeNum != 60.0 {
		t.Errorf("MaxAltitudeNum = %f, want 60.0", wrapper.Data[0].MaxAltitudeNum)
	}
}

//
// ----------- HTTP API 测试（走 coords 路径，避免访问外网） -----------
//

func TestAstroAPIHandlerYearCoords(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/astro?lat=0&lon=0&tz=UTC&mode=year", nil)
	w := httptest.NewRecorder()

	astroAPIHandler(w, req)
	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var parsed astroAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if parsed.Mode != "year" {
		t.Errorf("Mode = %q, want %q", parsed.Mode, "year")
	}
	if len(parsed.Data) == 0 {
		t.Fatalf("expected non-empty data for year mode")
	}
}

func TestAstroAPIHandlerDayCoords(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/astro?lat=0&lon=0&tz=UTC&mode=day&date=2025-01-01", nil)
	w := httptest.NewRecorder()

	astroAPIHandler(w, req)
	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var parsed astroAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if parsed.Mode != "day" {
		t.Errorf("Mode = %q, want %q", parsed.Mode, "day")
	}
	if len(parsed.Data) != 1 {
		t.Fatalf("expected 1 day in data, got %d", len(parsed.Data))
	}
	if parsed.Data[0].Date != "2025-01-01" {
		t.Errorf("Date = %q, want %q", parsed.Data[0].Date, "2025-01-01")
	}
}

func TestAstroAPIHandlerBadRequest(t *testing.T) {
	// 缺少 city 和 coords
	req := httptest.NewRequest("GET", "/api/astro?mode=day", nil)
	w := httptest.NewRecorder()

	astroAPIHandler(w, req)
	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestHealthHandler(t *testing.T) {
	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()

	healthHandler(w, req)
	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

//
// ----------- TUI 模型的基本行为测试（不跑完整终端，只测状态转换逻辑） -----------
//

func TestTuiModelBasicFlowYear(t *testing.T) {
	cache := &CityCache{
		Entries: map[string]CityCacheEntry{
			"beijing": {
				City:        "北京",
				Normalized:  "beijing",
				DisplayName: "Beijing, China",
				Lat:         39.9,
				Lon:         116.4,
				TimezoneID:  "Asia/Shanghai",
			},
		},
	}

	m := newTuiModel(cache)
	if m.step != stepMain {
		t.Fatalf("initial step = %v, want stepMain", m.step)
	}

	// 模拟在主界面直接按回车（不输入城市）：
	// 会自动选择缓存里的第一个城市 + 默认 Year 模式，
	// handleEnter 之后应该进入 stepDone 并标记 quitting=true。
	model, _ := m.handleEnter()
	tm, ok := model.(tuiModel)
	if !ok {
		t.Fatalf("expected model to be tuiModel, got %T", model)
	}

	if tm.step != stepDone {
		t.Errorf("after handleEnter, step = %v, want %v", tm.step, stepDone)
	}
	if !tm.quitting {
		t.Errorf("after handleEnter, quitting = false, want true")
	}
	if tm.chosenCity == "" {
		t.Errorf("expected chosenCity to be set from cache, got empty string")
	}
}
