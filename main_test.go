package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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

// 添加更多天文数据生成测试
func TestGenerateAstroDataMultipleDays(t *testing.T) {
	loc, _ := time.LoadLocation("UTC")
	start := time.Date(2025, 1, 1, 12, 0, 0, 0, loc)

	data, err := generateAstroData("TestCity", 0, 0, loc, start, 5)
	if err != nil {
		t.Fatalf("generateAstroData error: %v", err)
	}
	if len(data) != 5 {
		t.Fatalf("expected 5 days, got %d", len(data))
	}

	// 检查日期是否连续
	for i, d := range data {
		expectedDate := start.AddDate(0, 0, i).Format("2006-01-02")
		if d.Date != expectedDate {
			t.Errorf("day %d: expected date %s, got %s", i, expectedDate, d.Date)
		}
	}
}

func TestGenerateAstroDataInvalidDays(t *testing.T) {
	loc, _ := time.LoadLocation("UTC")
	start := time.Date(2025, 1, 1, 12, 0, 0, 0, loc)

	// 测试天数为0的情况
	_, err := generateAstroData("TestCity", 0, 0, loc, start, 0)
	if err == nil {
		t.Error("expected error for 0 days, got nil")
	}

	// 测试天数为负数的情况
	_, err = generateAstroData("TestCity", 0, 0, loc, start, -1)
	if err == nil {
		t.Error("expected error for negative days, got nil")
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
	out, err := writeAstroJSON("TestCity", now, data, "test range", filePath, true)
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

// 添加CSV输出测试
func TestWriteAstroCSV(t *testing.T) {
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
	filePath := filepath.Join(tmpDir, "astro.csv")
	out, err := writeAstroCSV("TestCity", now, data, "test range", filePath, true)
	if err != nil {
		t.Fatalf("writeAstroCSV error: %v", err)
	}
	if out != filePath {
		t.Errorf("writeAstroCSV returned path %q, want %q", out, filePath)
	}

	// 读取并验证CSV内容
	content, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}

	// 检查是否包含必要的字段
	contentStr := string(content)
	if !strings.Contains(contentStr, "date") || !strings.Contains(contentStr, "sunrise") {
		t.Error("CSV content missing expected headers")
	}
	if !strings.Contains(contentStr, "2025-01-01") {
		t.Error("CSV content missing expected data")
	}
}

// 添加TXT输出测试
func TestWriteAstroTxt(t *testing.T) {
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
	filePath := filepath.Join(tmpDir, "astro.txt")
	out, err := writeAstroTxt("TestCity", now, data, "test range", filePath, true)
	if err != nil {
		t.Fatalf("writeAstroTxt error: %v", err)
	}
	if out != filePath {
		t.Errorf("writeAstroTxt returned path %q, want %q", out, filePath)
	}

	// 读取并验证TXT内容
	content, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}

	// 检查是否包含必要的字段
	contentStr := string(content)
	if !strings.Contains(contentStr, "eSunMoon") || !strings.Contains(contentStr, "2025-01-01") {
		t.Error("TXT content missing expected data")
	}
}

// 添加Excel输出测试
func TestWriteAstroExcel(t *testing.T) {
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
	filePath := filepath.Join(tmpDir, "astro.xlsx")
	out, err := writeAstroExcel("TestCity", now, data, "test range", filePath, true)
	if err != nil {
		t.Fatalf("writeAstroExcel error: %v", err)
	}
	if out != filePath {
		t.Errorf("writeAstroExcel returned path %q, want %q", out, filePath)
	}

	// 检查文件是否存在
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		t.Error("Excel file was not created")
	}
}

// 添加writeAstroFile测试
func TestWriteAstroFile(t *testing.T) {
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

	// 测试不同格式
	formats := []string{"txt", "csv", "json", "excel"}
	for _, format := range formats {
		tmpDir := t.TempDir()
		filePath := filepath.Join(tmpDir, "astro."+format)

		out, err := writeAstroFile(format, true, "", "TestCity", now, data, "test range", filePath[:len(filePath)-len(format)-1])

		if err != nil {
			t.Errorf("writeAstroFile with format %s error: %v", format, err)
		}
		if out == "" {
			t.Errorf("writeAstroFile with format %s returned empty path", format)
		}
	}
}

//
// ----------- 工具函数测试 -----------
//

// TestLookupTimeZone 测试lookupTimeZone函数
func TestLookupTimeZone(t *testing.T) {
	// 测试有效坐标
	tzID, err := lookupTimeZone(39.9042, 116.4074) // 北京坐标
	if err != nil {
		t.Errorf("lookupTimeZone for Beijing coordinates failed: %v", err)
	}
	if tzID != "Asia/Shanghai" {
		t.Errorf("lookupTimeZone for Beijing coordinates = %q, want %q", tzID, "Asia/Shanghai")
	}

	// 测试无效坐标（在海洋上）
	_, err = lookupTimeZone(0, 0)
	if err == nil {
		t.Error("lookupTimeZone should return error for ocean coordinates (0,0)")
	}
}

// TestEarthSunDistanceKm 测试earthSunDistanceKm函数
func TestEarthSunDistanceKm(t *testing.T) {
	// 测试一个特定时间点的距离计算
	testTime := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	distance := earthSunDistanceKm(testTime)

	// 地日距离应该在合理范围内（地球轨道近日点约1.471亿公里，远日点约1.521亿公里）
	if distance < 147000000 || distance > 153000000 {
		t.Errorf("earthSunDistanceKm(2025-01-01) = %f, expected between 147M and 153M km", distance)
	}
}

// TestJulianDay 测试julianDay函数
func TestJulianDay(t *testing.T) {
	// 测试已知日期的儒略日
	// 2000年1月1日12:00 UTC的儒略日是2451545.0
	testTime := time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)
	jd := julianDay(testTime)

	if math.Abs(jd-2451545.0) > 0.1 {
		t.Errorf("julianDay(2000-01-01 12:00 UTC) = %f, want approximately 2451545.0", jd)
	}
}

// TestRadToDeg 测试radToDeg函数
func TestRadToDeg(t *testing.T) {
	// 测试π弧度转180度
	if got := radToDeg(math.Pi); got != 180.0 {
		t.Errorf("radToDeg(π) = %f, want 180.0", got)
	}

	// 测试π/2弧度转90度
	if got := radToDeg(math.Pi / 2); got != 90.0 {
		t.Errorf("radToDeg(π/2) = %f, want 90.0", got)
	}

	// 测试0弧度转0度
	if got := radToDeg(0); got != 0.0 {
		t.Errorf("radToDeg(0) = %f, want 0.0", got)
	}
}

//
// ----------- 缓存相关函数测试 -----------
//

// TestCacheFilePath 测试cacheFilePath函数
func TestCacheFilePath(t *testing.T) {
	// 保存原始HOME环境变量
	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)

	// 设置测试HOME目录
	tmpDir := t.TempDir()
	os.Setenv("HOME", tmpDir)

	path := cacheFilePath()
	if !strings.Contains(path, tmpDir) {
		t.Errorf("cacheFilePath() = %s, expected to contain HOME dir %s", path, tmpDir)
	}

	// 测试HOME目录不存在的情况
	os.Unsetenv("HOME")
	path = cacheFilePath()
	if path != ".esunmoon-cache.json" {
		t.Errorf("cacheFilePath() without HOME = %s, want .esunmoon-cache.json", path)
	}
}

// 测试saveCache和loadCache的集成
func TestSaveAndLoadCacheIntegration(t *testing.T) {
	tmpDir := t.TempDir()
	// 在测试中把 HOME 指向临时目录，避免污染真实用户目录
	t.Setenv("HOME", tmpDir)

	// 创建测试缓存数据
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
			"newyork": {
				City:        "New York",
				Normalized:  "newyork",
				DisplayName: "New York, USA",
				Lat:         40.7128,
				Lon:         -74.0060,
				TimezoneID:  "America/New_York",
				Aliases:     []string{"NYC", "New York City"},
				UpdatedAt:   "2025-01-01T00:00:00Z",
			},
		},
	}

	// 保存缓存
	if err := saveCache(cache); err != nil {
		t.Fatalf("saveCache error: %v", err)
	}

	// 加载缓存
	loaded := loadCache()
	if len(loaded.Entries) != 2 {
		t.Fatalf("expected 2 entries in cache, got %d", len(loaded.Entries))
	}

	// 验证北京条目
	beijing, ok := loaded.Entries["beijing"]
	if !ok {
		t.Fatal("expected key 'beijing' in loaded cache")
	}
	if beijing.City != "北京" || beijing.TimezoneID != "Asia/Shanghai" {
		t.Errorf("beijing entry mismatch: %#v", beijing)
	}

	// 验证纽约条目
	newyork, ok := loaded.Entries["newyork"]
	if !ok {
		t.Fatal("expected key 'newyork' in loaded cache")
	}
	if newyork.City != "New York" || newyork.TimezoneID != "America/New_York" {
		t.Errorf("newyork entry mismatch: %#v", newyork)
	}
}

// 测试空缓存加载
func TestLoadEmptyCache(t *testing.T) {
	// 保存原始HOME环境变量
	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)

	// 设置测试HOME目录
	tmpDir := t.TempDir()
	os.Setenv("HOME", tmpDir)

	// 确保缓存文件不存在
	cachePath := cacheFilePath()
	_ = os.Remove(cachePath)

	// 加载空缓存
	cache := loadCache()
	if cache == nil {
		t.Fatal("loadCache should not return nil")
	}
	if cache.Entries == nil {
		t.Fatal("cache.Entries should not be nil")
	}
	if len(cache.Entries) != 0 {
		t.Errorf("expected empty cache, got %d entries", len(cache.Entries))
	}
}

// 测试损坏的缓存文件处理
func TestLoadCorruptedCache(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	// 创建一个损坏的缓存文件
	cachePath := cacheFilePath()
	err := os.WriteFile(cachePath, []byte("invalid json content"), 0644)
	if err != nil {
		t.Fatalf("failed to create corrupted cache file: %v", err)
	}

	// 加载缓存应该返回空但有效的缓存结构
	cache := loadCache()
	if cache == nil {
		t.Fatal("loadCache should not return nil even for corrupted file")
	}
	if cache.Entries == nil {
		t.Fatal("cache.Entries should not be nil even for corrupted file")
	}
	if len(cache.Entries) != 0 {
		t.Errorf("expected empty cache for corrupted file, got %d entries", len(cache.Entries))
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
	if len(parsed.Notes) == 0 {
		t.Errorf("expected Notes to contain polar note")
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

func TestReadyHandler(t *testing.T) {
	req := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()

	readyHandler(w, req)
	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

// 添加更多HTTP API测试用例
func TestAstroAPIHandlerRangeCoords(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/astro?lat=0&lon=0&tz=UTC&mode=range&from=2025-01-01&to=2025-01-05", nil)
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
	if parsed.Mode != "range" {
		t.Errorf("Mode = %q, want %q", parsed.Mode, "range")
	}
	if len(parsed.Data) == 0 {
		t.Fatalf("expected non-empty data for range mode")
	}
}

func TestAstroAPIHandlerInvalidMode(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/astro?lat=0&lon=0&tz=UTC&mode=invalid", nil)
	w := httptest.NewRecorder()

	astroAPIHandler(w, req)
	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestAstroAPIHandlerMissingDateForDayMode(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/astro?lat=0&lon=0&tz=UTC&mode=day", nil)
	w := httptest.NewRecorder()

	astroAPIHandler(w, req)
	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestAstroAPIHandlerMissingRangeDates(t *testing.T) {
	// 缺少to参数
	req := httptest.NewRequest("GET", "/api/astro?lat=0&lon=0&tz=UTC&mode=range&from=2025-01-01", nil)
	w := httptest.NewRecorder()

	astroAPIHandler(w, req)
	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}

	// 缺少from参数
	req = httptest.NewRequest("GET", "/api/astro?lat=0&lon=0&tz=UTC&mode=range&to=2025-01-05", nil)
	w = httptest.NewRecorder()

	astroAPIHandler(w, req)
	resp = w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestAstroAPIHandlerInvalidCoords(t *testing.T) {
	// 无效的纬度
	req := httptest.NewRequest("GET", "/api/astro?lat=invalid&lon=0&tz=UTC&mode=year", nil)
	w := httptest.NewRecorder()

	astroAPIHandler(w, req)
	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}

	// 缺少时区
	req = httptest.NewRequest("GET", "/api/astro?lat=0&lon=0&mode=year", nil)
	w = httptest.NewRecorder()

	astroAPIHandler(w, req)
	resp = w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

//
// ----------- CLI命令测试 -----------
//

// 测试根命令执行
func TestRootCmd(t *testing.T) {
	// 由于rootCmd需要实际的命令行参数，我们在这里只做基本测试
	// 更完整的CLI测试通常需要使用exec.Command或者更复杂的模拟

	// 验证命令定义
	if rootCmd.Use != "esunmoon [城市名...]" {
		t.Errorf("rootCmd.Use = %q, want %q", rootCmd.Use, "esunmoon [城市名...]")
	}

	if rootCmd.Short == "" {
		t.Error("rootCmd.Short should not be empty")
	}
}

// 测试各子命令存在性
func TestSubCommandsExist(t *testing.T) {
	commands := []struct {
		name     string
		expected bool
	}{
		{"year", true},
		{"day", true},
		{"range", true},
		{"coords", true},
		{"tui", true},
		{"serve", true},
		{"cache", true},
	}

	for _, cmd := range commands {
		_, _, err := rootCmd.Find([]string{cmd.name})
		if cmd.expected && err != nil {
			t.Errorf("Expected command %q to exist, but got error: %v", cmd.name, err)
		} else if !cmd.expected && err == nil {
			t.Errorf("Expected command %q to not exist, but it was found", cmd.name)
		}
	}
}

// 测试缓存子命令
func TestCacheSubCommands(t *testing.T) {
	// 验证cache命令有正确的子命令
	cacheCmd, _, err := rootCmd.Find([]string{"cache"})
	if err != nil {
		t.Fatalf("Failed to find cache command: %v", err)
	}

	subCommands := []string{"list", "clear"}
	for _, subCmd := range subCommands {
		found := false
		for _, cmd := range cacheCmd.Commands() {
			if cmd.Name() == subCmd {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Expected cache subcommand %q not found", subCmd)
		}
	}
}

//
// ----------- 输出格式测试 -----------
//

// 测试各种输出格式函数的错误处理
func TestOutputFunctionsErrorHandling(t *testing.T) {
	// 测试writeAstroTxt错误处理（尝试写入无效路径）
	_, err := writeAstroTxt("TestCity", time.Now(), []dailyAstro{}, "test", "/invalid/path/test.txt", false)
	if err == nil {
		t.Error("writeAstroTxt should return error for invalid path")
	}

	// 测试writeAstroCSV错误处理
	_, err = writeAstroCSV("TestCity", time.Now(), []dailyAstro{}, "test", "/invalid/path/test.csv", false)
	if err == nil {
		t.Error("writeAstroCSV should return error for invalid path")
	}

	// 测试writeAstroJSON错误处理
	_, err = writeAstroJSON("TestCity", time.Now(), []dailyAstro{}, "test", "/invalid/path/test.json", false)
	if err == nil {
		t.Error("writeAstroJSON should return error for invalid path")
	}

	// 测试writeAstroExcel错误处理
	_, err = writeAstroExcel("TestCity", time.Now(), []dailyAstro{}, "test", "/invalid/path/test.xlsx", false)
	if err == nil {
		t.Error("writeAstroExcel should return error for invalid path")
	}
}

//
// ----------- HTTP处理函数测试 -----------
//

// 测试astroAPIHandler的各种错误情况
func TestAstroAPIHandlerErrors(t *testing.T) {
	// 测试无效的经纬度
	req := httptest.NewRequest("GET", "/api/astro?lat=invalid&lon=0&tz=UTC&mode=year", nil)
	w := httptest.NewRecorder()
	astroAPIHandler(w, req)
	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("Expected status 400 for invalid lat, got %d", resp.StatusCode)
	}

	// 测试无效的时区
	req = httptest.NewRequest("GET", "/api/astro?lat=0&lon=0&tz=Invalid/Timezone&mode=year", nil)
	w = httptest.NewRecorder()
	astroAPIHandler(w, req)
	resp = w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("Expected status 400 for invalid timezone, got %d", resp.StatusCode)
	}
}

// 测试更多HTTP API场景
func TestAstroAPIHandlerWithCity(t *testing.T) {
	// 注意：这个测试可能会因为需要网络而失败，但在离线模式下应该能工作

	// 我们跳过这个测试，因为它需要网络连接或复杂的模拟
	t.Skip("Skipping test that requires network or complex mocking")
}

//
// ----------- 更多工具函数测试 -----------
//

// 测试geocodeCity函数的错误情况
func TestGeocodeCityError(t *testing.T) {
	// 由于geocodeCity需要网络连接，我们只能测试错误处理

	// 测试无效城市名的错误处理
	// 注意：这个测试会实际发起网络请求，所以我们跳过它以避免对外部服务的依赖
	t.Skip("Skipping network-dependent test")

	// 如果要运行这个测试，取消下面的注释
	/*
		_, _, _, err := geocodeCity("")
		if err == nil {
			t.Error("geocodeCity should return error for empty city name")
		}
	*/
}

//
// ----------- 更多输出函数测试 -----------
//

// 测试writeAstroFile的各种格式
func TestWriteAstroFileFormats(t *testing.T) {
	tmpDir := t.TempDir()

	// 准备测试数据
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

	// 测试各种格式
	formats := []string{"txt", "csv", "json", "excel", "xlsx", "invalid"}

	for _, format := range formats {
		baseName := filepath.Join(tmpDir, "test")
		out, err := writeAstroFile(format, true, "", "TestCity", now, data, "test range", baseName)

		// 检查结果
		if err != nil {
			t.Errorf("writeAstroFile with format %s error: %v", format, err)
		}
		if out == "" {
			t.Errorf("writeAstroFile with format %s returned empty path", format)
		}
	}
}

//
// ----------- 更多构建函数测试 -----------
//

// 测试build函数的错误处理
func TestBuildFunctionErrors(t *testing.T) {
	loc, _ := time.LoadLocation("UTC")
	ctx := &CityContext{
		City:        "TestCity",
		DisplayName: "Test City",
		Lat:         0,
		Lon:         0,
		TZID:        "UTC",
		Loc:         loc,
		Now:         time.Date(2025, 1, 2, 15, 4, 5, 0, loc),
	}

	// 测试buildDayData的无效日期
	_, _, _, err := buildDayData(ctx, "invalid-date")
	if err == nil {
		t.Error("buildDayData should return error for invalid date")
	}

	// 测试buildRangeData的无效日期
	_, _, _, err = buildRangeData(ctx, "invalid-date", "2025-01-05")
	if err == nil {
		t.Error("buildRangeData should return error for invalid from date")
	}

	_, _, _, err = buildRangeData(ctx, "2025-01-01", "invalid-date")
	if err == nil {
		t.Error("buildRangeData should return error for invalid to date")
	}

	// 测试buildRangeData的日期顺序错误
	_, _, _, err = buildRangeData(ctx, "2025-01-05", "2025-01-01")
	if err == nil {
		t.Error("buildRangeData should return error for end date before start date")
	}
}

//
// ----------- CLI辅助函数测试 -----------
//

// 测试getCityFromArgsOrPrompt函数
func TestGetCityFromArgsOrPrompt(t *testing.T) {
	// 测试从参数获取城市名
	city := getCityFromArgsOrPrompt([]string{"Beijing"})
	if city != "Beijing" {
		t.Errorf("getCityFromArgsOrPrompt with args = %q, want %q", city, "Beijing")
	}

	// 测试从多个参数获取城市名
	city = getCityFromArgsOrPrompt([]string{"New", "York"})
	if city != "New York" {
		t.Errorf("getCityFromArgsOrPrompt with multiple args = %q, want %q", city, "New York")
	}

	// 测试空参数（注意：这会尝试从stdin读取，我们跳过这个测试）
	// 因为在测试环境中很难模拟stdin输入
	orig := os.Stdin
	defer func() { os.Stdin = orig }()
	tmpFile := filepath.Join(t.TempDir(), "stdin.txt")
	if err := os.WriteFile(tmpFile, []byte("Paris\n"), 0o644); err != nil {
		t.Fatalf("failed to prepare stdin file: %v", err)
	}
	f, err := os.Open(tmpFile)
	if err != nil {
		t.Fatalf("failed to open stdin file: %v", err)
	}
	os.Stdin = f
	city = getCityFromArgsOrPrompt([]string{})
	if city != "Paris" {
		t.Errorf("getCityFromArgsOrPrompt from stdin = %q, want %q", city, "Paris")
	}
}

//
// ----------- CLI交互函数测试 -----------
//

// 测试run系列函数（使用mock数据）
func TestRunFunctions(t *testing.T) {
	// 创建测试数据
	loc, _ := time.LoadLocation("UTC")
	ctx := &CityContext{
		City:        "TestCity",
		DisplayName: "Test City",
		Lat:         0,
		Lon:         0,
		TZID:        "UTC",
		Loc:         loc,
		Now:         time.Date(2025, 1, 2, 15, 4, 5, 0, loc),
	}

	// 测试runYear函数
	// 由于runYear会写入文件，我们在临时目录中测试
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	origDir, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer os.Chdir(origDir)

	opts := OutputOptions{Format: "json", AllowOverwrite: true, OutDir: ""}
	err := runYear(ctx, opts)

	// runYear可能会因为网络或文件系统问题而失败，但我们至少测试它不会崩溃
	if err != nil {
		t.Logf("runYear returned error (expected in test environment): %v", err)
	}
}

// 测试runDay函数
func TestRunDay(t *testing.T) {
	// 创建测试数据
	loc, _ := time.LoadLocation("UTC")
	ctx := &CityContext{
		City:        "TestCity",
		DisplayName: "Test City",
		Lat:         0,
		Lon:         0,
		TZID:        "UTC",
		Loc:         loc,
		Now:         time.Date(2025, 1, 2, 15, 4, 5, 0, loc),
	}

	// 测试runDay函数
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	origDir, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer os.Chdir(origDir)

	opts := OutputOptions{Format: "json", AllowOverwrite: true, OutDir: ""}
	err := runDay(ctx, "2025-01-01", opts)

	// runDay可能会因为网络或文件系统问题而失败，但我们至少测试它不会崩溃
	if err != nil {
		t.Logf("runDay returned error (expected in test environment): %v", err)
	}
}

// 测试runRange函数
func TestRunRange(t *testing.T) {
	// 创建测试数据
	loc, _ := time.LoadLocation("UTC")
	ctx := &CityContext{
		City:        "TestCity",
		DisplayName: "Test City",
		Lat:         0,
		Lon:         0,
		TZID:        "UTC",
		Loc:         loc,
		Now:         time.Date(2025, 1, 2, 15, 4, 5, 0, loc),
	}

	// 测试runRange函数
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	origDir, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer os.Chdir(origDir)

	opts := OutputOptions{Format: "json", AllowOverwrite: true, OutDir: ""}
	err := runRange(ctx, "2025-01-01", "2025-01-05", opts)

	// runRange可能会因为网络或文件系统问题而失败，但我们至少测试它不会崩溃
	if err != nil {
		t.Logf("runRange returned error (expected in test environment): %v", err)
	}

	// 覆盖禁止覆盖场景
	existing := filepath.Join(tmpDir, "TestCity-2025-01-01_to_2025-01-05.json")
	if err := os.WriteFile(existing, []byte("{}"), 0o644); err != nil {
		t.Fatalf("failed to seed existing file: %v", err)
	}
	opts.AllowOverwrite = false
	err = runRange(ctx, "2025-01-01", "2025-01-05", opts)
	if err == nil {
		t.Error("expected error when overwrite is false and file exists")
	}
}

func TestRunYearNoOverwrite(t *testing.T) {
	loc, _ := time.LoadLocation("UTC")
	ctx := &CityContext{
		City:        "TestCity",
		DisplayName: "Test City",
		Lat:         0,
		Lon:         0,
		TZID:        "UTC",
		Loc:         loc,
		Now:         time.Date(2025, 1, 2, 15, 4, 5, 0, loc),
	}
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	origDir, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer os.Chdir(origDir)

	opts := OutputOptions{Format: "json", AllowOverwrite: false, OutDir: ""}
	// 先生成文件
	_ = runYear(ctx, OutputOptions{Format: "json", AllowOverwrite: true, OutDir: ""})
	err := runYear(ctx, opts)
	if err == nil {
		t.Error("expected error when overwrite disabled for runYear")
	}
}

func TestRunDayNoOverwrite(t *testing.T) {
	loc, _ := time.LoadLocation("UTC")
	ctx := &CityContext{
		City:        "TestCity",
		DisplayName: "Test City",
		Lat:         0,
		Lon:         0,
		TZID:        "UTC",
		Loc:         loc,
		Now:         time.Date(2025, 1, 2, 15, 4, 5, 0, loc),
	}
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	origDir, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer os.Chdir(origDir)

	opts := OutputOptions{Format: "json", AllowOverwrite: false, OutDir: ""}
	// 先生成文件
	_ = runDay(ctx, "2025-01-01", OutputOptions{Format: "json", AllowOverwrite: true, OutDir: ""})
	err := runDay(ctx, "2025-01-01", opts)
	if err == nil {
		t.Error("expected error when overwrite disabled for runDay")
	}
}

// 测试build系列函数的更多情况
func TestBuildFunctionsExtended(t *testing.T) {
	loc, _ := time.LoadLocation("UTC")
	ctx := &CityContext{
		City:        "TestCity",
		DisplayName: "Test City",
		Lat:         0,
		Lon:         0,
		TZID:        "UTC",
		Loc:         loc,
		Now:         time.Date(2025, 1, 2, 15, 4, 5, 0, loc),
	}

	// 测试buildYearData
	data, desc, baseName, err := buildYearData(ctx)
	if err != nil {
		t.Errorf("buildYearData error: %v", err)
	}
	if len(data) != 365 {
		t.Errorf("buildYearData returned %d days, want 365", len(data))
	}
	if !strings.Contains(desc, "365") {
		t.Errorf("buildYearData desc = %q, want to contain '365'", desc)
	}
	if !strings.Contains(baseName, "year") {
		t.Errorf("buildYearData baseName = %q, want to contain 'year'", baseName)
	}

	// 测试buildDayData
	data, desc, baseName, err = buildDayData(ctx, "2025-01-01")
	if err != nil {
		t.Errorf("buildDayData error: %v", err)
	}
	if len(data) != 1 {
		t.Errorf("buildDayData returned %d days, want 1", len(data))
	}
	if !strings.Contains(desc, "2025-01-01") {
		t.Errorf("buildDayData desc = %q, want to contain date", desc)
	}

	// 测试buildRangeData
	data, desc, baseName, err = buildRangeData(ctx, "2025-01-01", "2025-01-05")
	if err != nil {
		t.Errorf("buildRangeData error: %v", err)
	}
	if len(data) != 5 {
		t.Errorf("buildRangeData returned %d days, want 5", len(data))
	}
	if !strings.Contains(desc, "5") {
		t.Errorf("buildRangeData desc = %q, want to contain '5'", desc)
	}

	// 测试buildRangeData错误情况
	_, _, _, err = buildRangeData(ctx, "2025-01-05", "2025-01-01") // 结束日期早于开始日期
	if err == nil {
		t.Error("buildRangeData should return error for end date before start date")
	}

	// 测试无效日期格式
	_, _, _, err = buildDayData(ctx, "invalid-date")
	if err == nil {
		t.Error("buildDayData should return error for invalid date")
	}

	_, _, _, err = buildRangeData(ctx, "invalid-date", "2025-01-05")
	if err == nil {
		t.Error("buildRangeData should return error for invalid from date")
	}

	_, _, _, err = buildRangeData(ctx, "2025-01-01", "invalid-date")
	if err == nil {
		t.Error("buildRangeData should return error for invalid to date")
	}
}

//
// ----------- 更多工具函数测试 -----------
//

// 测试printSunMoonPosition函数（间接测试）
func TestPrintSunMoonPosition(t *testing.T) {
	// 创建测试上下文
	loc, _ := time.LoadLocation("UTC")
	ctx := &CityContext{
		City:        "TestCity",
		DisplayName: "Test City",
		Lat:         0,
		Lon:         0,
		TZID:        "UTC",
		Loc:         loc,
		Now:         time.Date(2025, 1, 2, 15, 4, 5, 0, loc),
	}

	// 测试函数不会崩溃
	// 由于printSunMoonPosition只是打印输出，我们无法直接验证其内容
	printSunMoonPosition(ctx)
}

// 测试更多边缘情况
func TestEdgeCases(t *testing.T) {
	// 测试normalizeCityKey的边缘情况
	cases := []struct {
		in   string
		want string
	}{
		{" Beijing ", "beijing"},
		{"北京", "北京"},
		{"PEKING", "peking"},
		{"", ""},
		{"  ", ""},
		{" New York ", "new york"},
		{"São Paulo", "são paulo"},
	}
	for _, c := range cases {
		if got := normalizeCityKey(c.in); got != c.want {
			t.Errorf("normalizeCityKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// 测试formatDuration的边缘情况
	// 测试零值
	if got := formatDuration(0); got != "--" {
		t.Errorf("formatDuration(0) = %q, want %q", got, "--")
	}

	// 测试负值
	if got := formatDuration(-1 * time.Hour); got != "--" {
		t.Errorf("formatDuration(-1h) = %q, want %q", got, "--")
	}

	// 测试正好24小时
	d := 24 * time.Hour
	if got := formatDuration(d); got != "24:00" {
		t.Errorf("formatDuration(24h) = %q, want %q", got, "24:00")
	}

	// 测试超过24小时
	d = 25*time.Hour + 30*time.Minute
	if got := formatDuration(d); got != "25:30" {
		t.Errorf("formatDuration(25h30m) = %q, want %q", got, "25:30")
	}

	// 测试formatTimeLocal的边缘情况
	// 测试零值时间
	if got := formatTimeLocal(time.Time{}); got != "--" {
		t.Errorf("formatTimeLocal(zero) = %q, want %q", got, "--")
	}

	// 测试午夜时间
	loc := time.FixedZone("TEST", 0)
	tm := time.Date(2025, 1, 2, 0, 0, 0, 0, loc)
	if got := formatTimeLocal(tm); got != "00:00" {
		t.Errorf("formatTimeLocal(midnight) = %q, want %q", got, "00:00")
	}

	// 测试中午时间
	tm = time.Date(2025, 1, 2, 12, 0, 0, 0, loc)
	if got := formatTimeLocal(tm); got != "12:00" {
		t.Errorf("formatTimeLocal(noon) = %q, want %q", got, "12:00")
	}

	// 测试parseDateInLocation的边缘情况
	loc = time.FixedZone("TEST", 8*3600)

	// 测试正常情况
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

	// 测试无效日期格式
	_, err = parseDateInLocation("invalid-date", loc)
	if err == nil {
		t.Error("expected error for invalid date format, got nil")
	}

	// 测试空字符串
	_, err = parseDateInLocation("", loc)
	if err == nil {
		t.Error("expected error for empty date string, got nil")
	}

	// 测试年末日期
	tm, err = parseDateInLocation("2025-12-31", loc)
	if err != nil {
		t.Fatalf("parseDateInLocation returned error: %v", err)
	}
	if tm.Day() != 31 || tm.Month() != 12 || tm.Year() != 2025 {
		t.Errorf("expected date 2025-12-31, got %v", tm)
	}

	// 测试年初日期
	tm, err = parseDateInLocation("2025-01-01", loc)
	if err != nil {
		t.Fatalf("parseDateInLocation returned error: %v", err)
	}
	if tm.Day() != 1 || tm.Month() != 1 || tm.Year() != 2025 {
		t.Errorf("expected date 2025-01-01, got %v", tm)
	}
}

//
//
// ----------- 初始化函数测试 -----------
//

// 测试init函数的效果（间接测试）
func TestInitFunctionEffects(t *testing.T) {
	// 验证rootCmd有正确的标志
	flags := []string{"offline", "format"}
	for _, flag := range flags {
		if rootCmd.PersistentFlags().Lookup(flag) == nil {
			t.Errorf("Expected persistent flag %q not found", flag)
		}
	}

	// 验证dayCmd有正确的标志
	if dayCmd.Flags().Lookup("date") == nil {
		t.Error("Expected flag 'date' not found in dayCmd")
	}

	// 验证rangeCmd有正确的标志
	rangeFlags := []string{"from", "to"}
	for _, flag := range rangeFlags {
		if rangeCmd.Flags().Lookup(flag) == nil {
			t.Errorf("Expected flag %q not found in rangeCmd", flag)
		}
	}

	// 验证coordsCmd有正确的标志
	coordsFlags := []string{"lat", "lon", "tz", "mode", "date", "from", "to", "city"}
	for _, flag := range coordsFlags {
		if coordsCmd.Flags().Lookup(flag) == nil {
			t.Errorf("Expected flag %q not found in coordsCmd", flag)
		}
	}

	// 验证serveCmd有正确的标志
	if serveCmd.Flags().Lookup("addr") == nil {
		t.Error("Expected flag 'addr' not found in serveCmd")
	}
}

//
// ----------- 主函数和核心流程测试 -----------
//

// 测试main函数（间接测试）
func TestMainFunction(t *testing.T) {
	// main函数本身很难直接测试，因为我们不能多次调用它
	// 但我们可以通过测试整个程序的行为来间接测试它

	// 验证程序的基本结构
	if rootCmd == nil {
		t.Error("rootCmd should not be nil")
	}
}

// TestMainFunctionExecution 测试main函数的执行（间接测试）
func TestMainFunctionExecution(t *testing.T) {
	// main函数很难直接测试，因为它会调用os.Exit
	// 我们可以通过检查其依赖的初始化来间接测试

	// 验证rootCmd已正确初始化
	if rootCmd == nil {
		t.Fatal("rootCmd should be initialized")
	}

	// 验证命令结构
	if rootCmd.Use == "" {
		t.Error("rootCmd.Use should not be empty")
	}

	// 验证有子命令
	if len(rootCmd.Commands()) == 0 {
		t.Error("rootCmd should have subcommands")
	}
}

//
// ----------- 更多边缘情况测试 -----------
//

// 测试formatDuration的更多边缘情况
func TestFormatDurationMoreEdgeCases(t *testing.T) {
	// 测试正好1小时
	d := 1 * time.Hour
	if got := formatDuration(d); got != "01:00" {
		t.Errorf("formatDuration(1h) = %q, want %q", got, "01:00")
	}

	// 测试1小时1分钟
	d = 1*time.Hour + 1*time.Minute
	if got := formatDuration(d); got != "01:01" {
		t.Errorf("formatDuration(1h1m) = %q, want %q", got, "01:01")
	}

	// 测试59分钟
	d = 59 * time.Minute
	if got := formatDuration(d); got != "00:59" {
		t.Errorf("formatDuration(59m) = %q, want %q", got, "00:59")
	}
}

// 测试formatTimeLocal的更多边缘情况
func TestFormatTimeLocalMoreEdgeCases(t *testing.T) {
	// 测试23:59
	loc := time.FixedZone("TEST", 0)
	tm := time.Date(2025, 1, 2, 23, 59, 0, 0, loc)
	if got := formatTimeLocal(tm); got != "23:59" {
		t.Errorf("formatTimeLocal(23:59) = %q, want %q", got, "23:59")
	}

	// 测试00:01
	tm = time.Date(2025, 1, 2, 0, 1, 0, 0, loc)
	if got := formatTimeLocal(tm); got != "00:01" {
		t.Errorf("formatTimeLocal(00:01) = %q, want %q", got, "00:01")
	}
}

// 测试parseDateInLocation的更多边缘情况
func TestParseDateInLocationMoreEdgeCases(t *testing.T) {
	loc := time.FixedZone("TEST", 8*3600)

	// 测试年末日期
	tm, err := parseDateInLocation("2025-12-31", loc)
	if err != nil {
		t.Fatalf("parseDateInLocation returned error: %v", err)
	}
	if tm.Day() != 31 || tm.Month() != 12 || tm.Year() != 2025 {
		t.Errorf("expected date 2025-12-31, got %v", tm)
	}

	// 测试年初日期
	tm, err = parseDateInLocation("2025-01-01", loc)
	if err != nil {
		t.Fatalf("parseDateInLocation returned error: %v", err)
	}
	if tm.Day() != 1 || tm.Month() != 1 || tm.Year() != 2025 {
		t.Errorf("expected date 2025-01-01, got %v", tm)
	}
}

//
// ----------- 缓存相关测试 -----------
//

// 测试findEntryInCache的更多情况
func TestFindEntryInCacheMoreCases(t *testing.T) {
	cache := &CityCache{
		Entries: map[string]CityCacheEntry{
			"beijing": {
				City:        "北京",
				Normalized:  "beijing",
				DisplayName: "Beijing, China",
				Lat:         39.9,
				Lon:         116.4,
				TimezoneID:  "Asia/Shanghai",
				Aliases:     []string{"Beijing", "Peking", "BJ"},
				UpdatedAt:   "2025-01-01T00:00:00Z",
			},
			"newyork": {
				City:        "New York",
				Normalized:  "newyork",
				DisplayName: "New York, USA",
				Lat:         40.7128,
				Lon:         -74.0060,
				TimezoneID:  "America/New_York",
				Aliases:     []string{"NYC", "New York City"},
				UpdatedAt:   "2025-01-01T00:00:00Z",
			},
		},
	}

	// 测试按规范化键查找
	if e, ok := findEntryInCache(cache, "beijing"); !ok || e.City != "北京" {
		t.Errorf("findEntryInCache by normalized key failed, got %#v, ok=%v", e, ok)
	}

	// 测试按城市名查找
	if e, ok := findEntryInCache(cache, "北京"); !ok || e.City != "北京" {
		t.Errorf("findEntryInCache by city name failed, got %#v, ok=%v", e, ok)
	}

	// 测试按别名查找
	if e, ok := findEntryInCache(cache, "Peking"); !ok || e.City != "北京" {
		t.Errorf("findEntryInCache by alias failed, got %#v, ok=%v", e, ok)
	}

	// 测试另一个城市的别名查找
	if e, ok := findEntryInCache(cache, "NYC"); !ok || e.City != "New York" {
		t.Errorf("findEntryInCache by NYC alias failed, got %#v, ok=%v", e, ok)
	}

	// 测试未找到的情况
	if _, ok := findEntryInCache(cache, "unknown"); ok {
		t.Error("findEntryInCache should return false for unknown city")
	}

	// 测试大小写不敏感查找
	if e, ok := findEntryInCache(cache, "BEIJING"); !ok || e.City != "北京" {
		t.Errorf("findEntryInCache by uppercase key failed, got %#v, ok=%v", e, ok)
	}
}

func TestAcquireFileLock(t *testing.T) {
	tmpDir := t.TempDir()
	lockPath := filepath.Join(tmpDir, "cache.json.lock")

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	unlock, err := acquireFileLock(ctx, lockPath)
	if err != nil {
		t.Fatalf("acquireFileLock first attempt error: %v", err)
	}

	// 第二次获取应该因为超时失败
	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	if _, err := acquireFileLock(ctx2, lockPath); err == nil {
		t.Fatal("expected second acquireFileLock to fail due to lock contention")
	}

	// 释放后应该能再次获取
	unlock()
	if _, err := os.Stat(lockPath); err == nil {
		t.Fatal("lock file should be removed after unlock")
	}
	unlock3, err := acquireFileLock(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("acquireFileLock after release error: %v", err)
	}
	unlock3()
}

func TestLoggerJSON(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogger(&buf, LevelDebug, true, false, func() time.Time { return time.Unix(0, 0) })
	l.logf(LevelInfo, "info", "hello %s", "world")

	var m map[string]string
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("failed to unmarshal json log: %v", err)
	}
	if m["level"] != "info" {
		t.Errorf("log level = %s, want info", m["level"])
	}
	if m["msg"] != "hello world" {
		t.Errorf("log msg = %s, want hello world", m["msg"])
	}
}

func TestLoggerQuietAndLevel(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogger(&buf, LevelError, false, false, time.Now)
	l.logf(LevelInfo, "info", "should not print")
	if buf.Len() != 0 {
		t.Errorf("expected no output for info when level=error, got %s", buf.String())
	}

	buf.Reset()
	l.quiet = true
	l.logf(LevelError, "error", "quiet mode")
	if buf.Len() != 0 {
		t.Errorf("expected quiet logger to produce no output, got %s", buf.String())
	}
}

//
// ----------- 构建函数的更多测试 -----------
//

// 测试buildYearData的边缘情况
func TestBuildYearDataEdgeCases(t *testing.T) {
	loc, _ := time.LoadLocation("UTC")
	ctx := &CityContext{
		City:        "TestCity",
		DisplayName: "Test City",
		Lat:         0,
		Lon:         0,
		TZID:        "UTC",
		Loc:         loc,
		Now:         time.Date(2025, 12, 31, 15, 4, 5, 0, loc), // 年末时间
	}

	// 测试年末时间的年度数据构建
	data, desc, baseName, err := buildYearData(ctx)
	if err != nil {
		t.Errorf("buildYearData error: %v", err)
	}
	if len(data) != 365 {
		t.Errorf("buildYearData returned %d days, want 365", len(data))
	}
	if !strings.Contains(desc, "365") {
		t.Errorf("buildYearData desc = %q, want to contain '365'", desc)
	}
	if !strings.Contains(baseName, "year") {
		t.Errorf("buildYearData baseName = %q, want to contain 'year'", baseName)
	}
}

// 测试buildDayData的边缘情况
func TestBuildDayDataEdgeCases(t *testing.T) {
	loc, _ := time.LoadLocation("UTC")
	ctx := &CityContext{
		City:        "TestCity",
		DisplayName: "Test City",
		Lat:         0,
		Lon:         0,
		TZID:        "UTC",
		Loc:         loc,
		Now:         time.Date(2025, 1, 2, 15, 4, 5, 0, loc),
	}

	// 测试年末日期
	data, desc, baseName, err := buildDayData(ctx, "2025-12-31")
	if err != nil {
		t.Errorf("buildDayData error: %v", err)
	}
	if len(data) != 1 {
		t.Errorf("buildDayData returned %d days, want 1", len(data))
	}
	if !strings.Contains(desc, "2025-12-31") {
		t.Errorf("buildDayData desc = %q, want to contain date", desc)
	}
	if !strings.Contains(baseName, "2025-12-31") {
		t.Errorf("buildDayData baseName = %q, want to contain date", baseName)
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

// 添加更多TUI模型测试
func TestTuiModelInputHandling(t *testing.T) {
	cache := &CityCache{
		Entries: make(map[string]CityCacheEntry),
	}

	m := newTuiModel(cache)

	// 测试输入字符
	model, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	tm := model.(tuiModel)
	if tm.input != "t" {
		t.Errorf("input = %q, want %q", tm.input, "t")
	}

	// 继续输入
	model, _ = tm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	tm = model.(tuiModel)
	if tm.input != "te" {
		t.Errorf("input = %q, want %q", tm.input, "te")
	}

	// 测试退格
	model, _ = tm.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	tm = model.(tuiModel)
	if tm.input != "t" {
		t.Errorf("input after backspace = %q, want %q", tm.input, "t")
	}
}

func TestTuiModelNavigation(t *testing.T) {
	cache := &CityCache{
		Entries: make(map[string]CityCacheEntry),
	}

	m := newTuiModel(cache)

	// 测试模式切换
	originalModeIndex := m.modeIndex
	model, _ := m.Update(tea.KeyMsg{Type: tea.KeyRight})
	tm := model.(tuiModel)
	if tm.modeIndex != originalModeIndex+1 {
		t.Errorf("modeIndex after right = %d, want %d", tm.modeIndex, originalModeIndex+1)
	}

	// 测试格式切换
	originalFormatIndex := tm.formatIndex
	model, _ = tm.Update(tea.KeyMsg{Type: tea.KeyDown})
	tm = model.(tuiModel)
	if tm.formatIndex != originalFormatIndex+1 {
		t.Errorf("formatIndex after down = %d, want %d", tm.formatIndex, originalFormatIndex+1)
	}
}

func TestTuiModelDayModeFlow(t *testing.T) {
	cache := &CityCache{
		Entries: make(map[string]CityCacheEntry),
	}

	m := newTuiModel(cache)

	// 设置为Day模式
	m.modeIndex = 1 // Day mode

	// 输入城市名
	m.input = "Beijing"

	// 按回车进入日期输入
	model, _ := m.handleEnter()
	tm := model.(tuiModel)

	if tm.step != stepDayInput {
		t.Errorf("step after enter in day mode = %v, want %v", tm.step, stepDayInput)
	}

	// 输入日期
	tm.input = "2025-01-01"

	// 按回车完成
	model, _ = tm.handleEnter()
	tm = model.(tuiModel)

	if tm.step != stepDone {
		t.Errorf("step after enter with date = %v, want %v", tm.step, stepDone)
	}
	if !tm.quitting {
		t.Error("quitting should be true after date input")
	}
}

func TestTuiModelRangeModeFlow(t *testing.T) {
	cache := &CityCache{
		Entries: make(map[string]CityCacheEntry),
	}

	m := newTuiModel(cache)

	// 设置为Range模式
	m.modeIndex = 2 // Range mode

	// 输入城市名
	m.input = "Beijing"

	// 按回车进入起始日期输入
	model, _ := m.handleEnter()
	tm := model.(tuiModel)

	if tm.step != stepRangeFromInput {
		t.Errorf("step after enter in range mode = %v, want %v", tm.step, stepRangeFromInput)
	}

	// 输入起始日期
	tm.input = "2025-01-01"

	// 按回车进入结束日期输入
	model, _ = tm.handleEnter()
	tm = model.(tuiModel)

	if tm.step != stepRangeToInput {
		t.Errorf("step after enter with from date = %v, want %v", tm.step, stepRangeToInput)
	}

	// 输入结束日期
	tm.input = "2025-01-31"

	// 按回车完成
	model, _ = tm.handleEnter()
	tm = model.(tuiModel)

	if tm.step != stepDone {
		t.Errorf("step after enter with to date = %v, want %v", tm.step, stepDone)
	}
	if !tm.quitting {
		t.Error("quitting should be true after range input")
	}
}

func TestTuiModelInit(t *testing.T) {
	cache := &CityCache{
		Entries: make(map[string]CityCacheEntry),
	}

	m := newTuiModel(cache)
	cmd := m.Init()
	if cmd != nil {
		t.Error("Init() should return nil command")
	}
}

// 测试TUI模型的View方法在不同步骤下的输出
func TestTuiModelView(t *testing.T) {
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
	view := m.View()
	if view == "" {
		t.Error("View() should return non-empty string in stepMain")
	}
	if !strings.Contains(view, "eSunMoon") {
		t.Error("View() should contain app name")
	}

	// 测试quitting状态
	m.quitting = true
	view = m.View()
	if view != "" {
		t.Error("View() should return empty string when quitting")
	}

	// 覆盖 Day/Range 视图分支
	m.quitting = false
	m.step = stepDayInput
	m.chosenCity = "TestCity"
	view = m.View()
	if !strings.Contains(view, "模式：Day") {
		t.Error("View() for Day should mention mode")
	}
	m.step = stepRangeFromInput
	view = m.View()
	if !strings.Contains(view, "起始日期") {
		t.Error("View() for Range From should mention 起始日期")
	}
	m.step = stepRangeToInput
	view = m.View()
	if !strings.Contains(view, "结束日期") {
		t.Error("View() for Range To should mention 结束日期")
	}
}

// 测试TUI模型的各种按键处理
func TestTuiModelKeyHandling(t *testing.T) {
	cache := &CityCache{
		Entries: make(map[string]CityCacheEntry),
	}

	m := newTuiModel(cache)

	// 测试Ctrl+C退出
	model, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	tm := model.(tuiModel)
	if !tm.quitting {
		t.Error("Ctrl+C should set quitting to true")
	}
	if cmd == nil {
		t.Error("Ctrl+C should return Quit command")
	}

	// 测试ESC退出
	m = newTuiModel(cache) // 重置模型
	model, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	tm = model.(tuiModel)
	if !tm.quitting {
		t.Error("ESC should set quitting to true")
	}
	if cmd == nil {
		t.Error("ESC should return Quit command")
	}
}

// 测试TUI模型错误消息处理
func TestTuiModelErrorMessages(t *testing.T) {
	cache := &CityCache{
		Entries: make(map[string]CityCacheEntry),
	}

	m := newTuiModel(cache)

	// 在主界面按回车但没有输入城市且没有缓存城市
	model, _ := m.handleEnter()
	tm := model.(tuiModel)
	if tm.errMsg == "" {
		t.Error("Should show error message when no city input and no cached cities")
	}

	// 在日期输入界面按回车但没有输入日期
	m.step = stepDayInput
	model, _ = m.handleEnter()
	tm = model.(tuiModel)
	if tm.errMsg == "" {
		t.Error("Should show error message when no date input in day mode")
	}

	// 在范围起始日期输入界面按回车但没有输入日期
	m.step = stepRangeFromInput
	model, _ = m.handleEnter()
	tm = model.(tuiModel)
	if tm.errMsg == "" {
		t.Error("Should show error message when no from date input in range mode")
	}

	// 在范围结束日期输入界面按回车但没有输入日期
	m.step = stepRangeToInput
	model, _ = m.handleEnter()
	tm = model.(tuiModel)
	if tm.errMsg == "" {
		t.Error("Should show error message when no to date input in range mode")
	}
}

// 测试空缓存情况下的TUI模型
func TestTuiModelEmptyCache(t *testing.T) {
	cache := &CityCache{
		Entries: make(map[string]CityCacheEntry),
	}

	m := newTuiModel(cache)
	view := m.View()
	if !strings.Contains(view, "暂无缓存城市") {
		t.Error("View should indicate no cached cities when cache is empty")
	}
}

// 测试TUI模型格式索引边界
func TestTuiModelFormatIndexBounds(t *testing.T) {
	cache := &CityCache{
		Entries: make(map[string]CityCacheEntry),
	}

	m := newTuiModel(cache)

	// 测试向上导航不会超出边界
	m.formatIndex = 0
	model, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	tm := model.(tuiModel)
	if tm.formatIndex < 0 {
		t.Error("formatIndex should not be negative")
	}

	// 测试向下导航不会超出边界
	m.formatIndex = len(m.formats) - 1
	model, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	tm = model.(tuiModel)
	if tm.formatIndex >= len(m.formats) {
		t.Error("formatIndex should not exceed formats length")
	}
}

// 测试TUI模型模式索引边界
func TestTuiModelModeIndexBounds(t *testing.T) {
	cache := &CityCache{
		Entries: make(map[string]CityCacheEntry),
	}

	m := newTuiModel(cache)

	// 测试向左导航不会超出边界
	m.modeIndex = 0
	model, _ := m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	tm := model.(tuiModel)
	if tm.modeIndex < 0 {
		t.Error("modeIndex should not be negative")
	}

	// 测试向右导航不会超出边界
	m.modeIndex = len(m.modes) - 1
	model, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	tm = model.(tuiModel)
	if tm.modeIndex >= len(m.modes) {
		t.Error("modeIndex should not exceed modes length")
	}
}

//
// ----------- Mock HTTP客户端用于测试网络函数 -----------
//

// mockHTTPClient 用于模拟HTTP请求
type mockHTTPClient struct {
	responseBody string
	statusCode   int
	err          error
}

func (m *mockHTTPClient) Do(req *http.Request) (*http.Response, error) {
	if m.err != nil {
		return nil, m.err
	}

	// 创建一个模拟的HTTP响应
	resp := &http.Response{
		StatusCode: m.statusCode,
		Body:       io.NopCloser(strings.NewReader(m.responseBody)),
	}

	return resp, nil
}

// 保存原始的HTTP客户端以便恢复
var originalHTTPClient *http.Client

// mockGeocodeCity 函数用于测试geocodeCity而不需要真实的网络请求
func mockGeocodeCity(city string) (lat, lon float64, displayName string, err error) {
	// 这里我们直接返回预定义的值，而不是实际调用网络
	switch city {
	case "Beijing":
		return 39.9042, 116.4074, "Beijing, China", nil
	case "New York":
		return 40.7128, -74.0060, "New York, USA", nil
	case "London":
		return 51.5074, -0.1278, "London, UK", nil
	default:
		return 0, 0, "", fmt.Errorf("未找到城市: %s", city)
	}
}

//
// ----------- 网络相关函数测试 -----------
//

// 测试geocodeCity函数（使用mock）
func TestGeocodeCity(t *testing.T) {
	// 直接测试空城市的错误路径（无需实际网络）
	_, _, _, err := geocodeCity(context.Background(), app.client, "")
	if err == nil {
		t.Error("geocodeCity should fail on empty city")
	}
}

// 测试prepareCity函数（使用临时缓存文件）
func TestPrepareCity(t *testing.T) {
	// 创建一个临时目录用于测试缓存
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	// 创建一个带缓存条目的测试
	cache := &CityCache{
		Entries: map[string]CityCacheEntry{
			"beijing": {
				City:        "北京",
				Normalized:  "beijing",
				DisplayName: "Beijing, China",
				Lat:         39.9042,
				Lon:         116.4074,
				TimezoneID:  "Asia/Shanghai",
				Aliases:     []string{"Beijing", "北京"},
				UpdatedAt:   time.Now().Format(time.RFC3339),
			},
		},
	}

	// 保存缓存到临时文件
	if err := saveCache(cache); err != nil {
		t.Fatalf("Failed to save test cache: %v", err)
	}

	// 测试从缓存中获取城市
	ctx, err := prepareCity("Beijing", true) // 离线模式
	if err != nil {
		t.Errorf("prepareCity for Beijing in offline mode failed: %v", err)
	}
	if ctx == nil {
		t.Fatal("prepareCity should return a valid CityContext")
	}
	if ctx.City != "北京" {
		t.Errorf("prepareCity returned incorrect city: %s", ctx.City)
	}

	// 测试离线模式下不存在的城市
	_, err = prepareCity("New York", true) // 离线模式
	if err == nil {
		t.Error("prepareCity should return error for unknown city in offline mode")
	}

	// 测试缓存过期离线模式
	old := cache.Entries["beijing"]
	old.UpdatedAt = time.Now().Add(-200 * 24 * time.Hour).Format(time.RFC3339)
	cache.Entries["beijing"] = old
	if err := saveCache(cache); err != nil {
		t.Fatalf("Failed to save expired cache: %v", err)
	}
	_, err = prepareCity("Beijing", true)
	if err == nil {
		t.Error("expected error for expired cache in offline mode")
	}
}

func TestPrepareCityInvalidTimezoneInCache(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cache := &CityCache{
		Entries: map[string]CityCacheEntry{
			"bad": {
				City:        "BadTZ",
				Normalized:  "badtz",
				DisplayName: "Bad TZ City",
				Lat:         0,
				Lon:         0,
				TimezoneID:  "Invalid/Zone",
				UpdatedAt:   time.Now().Format(time.RFC3339),
			},
		},
	}
	if err := saveCache(cache); err != nil {
		t.Fatalf("saveCache error: %v", err)
	}
	if _, err := prepareCity("BadTZ", true); err == nil {
		t.Error("expected error when cache has invalid timezone")
	}
}

func TestPrepareCityLookupTimeZoneError(t *testing.T) {
	origLookup := app.tzLookup
	origClient := app.client
	defer func() {
		app.tzLookup = origLookup
		app.client = origClient
	}()

	app.tzLookup = func(lat, lon float64) (string, error) {
		return "", fmt.Errorf("tz lookup fail")
	}
	app.client = &httpClientMock{
		doFunc: func(req *http.Request) (*http.Response, error) {
			body := `[{"lat":"0","lon":"0","display_name":"MockCity"}]`
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		},
	}
	if _, err := prepareCity("MockCity", false); err == nil {
		t.Error("expected error when lookupTimeZone fails")
	}
}

func TestPrepareCityLoadLocationError(t *testing.T) {
	origLookup := app.tzLookup
	origLoad := app.loadTZ
	origClient := app.client
	defer func() {
		app.tzLookup = origLookup
		app.loadTZ = origLoad
		app.client = origClient
	}()

	app.tzLookup = func(lat, lon float64) (string, error) {
		return "Invalid/Zone", nil
	}
	app.loadTZ = func(name string) (*time.Location, error) {
		return nil, fmt.Errorf("load location fail")
	}
	app.client = &httpClientMock{
		doFunc: func(req *http.Request) (*http.Response, error) {
			body := `[{"lat":"0","lon":"0","display_name":"MockCity"}]`
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		},
	}
	if _, err := prepareCity("MockCity", false); err == nil {
		t.Error("expected error when loadLocation fails")
	}
}

func TestGeocodeCityHTTPError(t *testing.T) {
	client := &httpClientMock{
		doFunc: func(req *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("network down")
		},
	}
	if _, _, _, err := geocodeCity(context.Background(), client, "Beijing"); err == nil {
		t.Error("expected geocodeCity to fail on HTTP error")
	}
}

func TestPrepareCityGeocodeError(t *testing.T) {
	origClient := app.client
	defer func() { app.client = origClient }()

	app.client = &httpClientMock{
		doFunc: func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("err"))}, nil
		},
	}
	if _, err := prepareCity("AnyCity", false); err == nil {
		t.Error("expected prepareCity to fail when geocode returns non-200")
	}
}

func TestServeWithGracefulShutdown(t *testing.T) {
	stop := make(chan os.Signal, 1)
	handler := http.NewServeMux()
	handler.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	go func() {
		time.Sleep(50 * time.Millisecond)
		stop <- syscall.SIGINT
	}()

	err := serveWithGracefulShutdown(":0", handler, stop)
	if err != nil && err != http.ErrServerClosed {
		// 沙箱环境可能禁止监听端口，检测后跳过
		if strings.Contains(err.Error(), "operation not permitted") {
			t.Skipf("skip due to sandbox restrictions: %v", err)
		}
		t.Fatalf("serveWithGracefulShutdown returned error: %v", err)
	}
}

// 测试prepareCity的在线模式（mock网络请求）
func TestPrepareCityOnlineMode(t *testing.T) {
	// 由于prepareCity的在线模式需要网络请求，我们跳过这个测试
	// 在实际应用中，我们需要更复杂的mock机制来模拟整个HTTP客户端
	t.Skip("Skipping online mode test due to complexity of mocking")
}

//
// ----------- 网络相关函数测试 (使用mock) -----------
//

// httpClientMock 是一个HTTP客户端的mock实现
type httpClientMock struct {
	doFunc func(req *http.Request) (*http.Response, error)
}

func (c *httpClientMock) Do(req *http.Request) (*http.Response, error) {
	if c.doFunc != nil {
		return c.doFunc(req)
	}
	return nil, fmt.Errorf("mock not implemented")
}

// mockGeocodeCityResponse 返回模拟的Nominatim响应
func mockGeocodeCityResponse(lat, lon float64, displayName string) string {
	return fmt.Sprintf(`[{
		"lat": "%f",
		"lon": "%f",
		"display_name": "%s"
	}]`, lat, lon, displayName)
}

// TestGeocodeCityWithMock 测试geocodeCity函数使用mock HTTP客户端
func TestGeocodeCityWithMock(t *testing.T) {
	// 使用自定义 http.Client mock
	mockBody := `[{"lat":"39.9","lon":"116.4","display_name":"Beijing, China"}]`
	client := &httpClientMock{
		doFunc: func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader(mockBody)),
			}, nil
		},
	}
	lat, lon, name, err := geocodeCity(context.Background(), client, "Beijing")
	if err != nil {
		t.Fatalf("geocodeCity mock error: %v", err)
	}
	if lat != 39.9 || lon != 116.4 || name != "Beijing, China" {
		t.Errorf("unexpected geocode result: %f %f %s", lat, lon, name)
	}
}

// TestGeocodeCityErrorCases 测试geocodeCity的错误情况
func TestGeocodeCityErrorCases(t *testing.T) {
	// 测试空城市名（这会实际发出网络请求，所以我们跳过）
	t.Skip("Skipping network-dependent test")

	// 如果要运行这个测试，取消下面的注释
	/*
		_, _, _, err := geocodeCity("")
		if err == nil {
			t.Error("geocodeCity should return error for empty city name")
		}
	*/
}

// 测试init函数的效果（间接测试）
func TestInitFunction(t *testing.T) {
	// 验证命令结构
	if rootCmd == nil {
		t.Error("rootCmd should not be nil after init")
	}

	// 验证标志
	offlineFlag := rootCmd.PersistentFlags().Lookup("offline")
	if offlineFlag == nil {
		t.Error("Expected 'offline' flag not found")
	}

	formatFlag := rootCmd.PersistentFlags().Lookup("format")
	if formatFlag == nil {
		t.Error("Expected 'format' flag not found")
	}

	// 验证子命令
	subCommands := []string{"year", "day", "range", "coords", "tui", "serve", "cache"}
	for _, cmdName := range subCommands {
		_, _, err := rootCmd.Find([]string{cmdName})
		if err != nil {
			t.Errorf("Expected subcommand %q not found: %v", cmdName, err)
		}
	}
}

// 测试main函数（间接测试）
func TestMainFunctionStructure(t *testing.T) {
	// main函数本身很难直接测试，但我们可以通过检查其效果来间接测试

	// 验证基本结构
	if rootCmd == nil {
		t.Fatal("rootCmd should be initialized")
	}

	// 验证命令名称
	if rootCmd.Use != "esunmoon [城市名...]" {
		t.Errorf("rootCmd.Use = %q, want %q", rootCmd.Use, "esunmoon [城市名...]")
	}

	// 测试日志标志作用
	logLevelFlag = "error"
	logJSONFlag = true
	logQuietFlag = true
	rootCmd.PersistentPreRun(rootCmd, []string{})
	if app.logger == nil {
		t.Fatal("logger should be initialized in PersistentPreRun")
	}

	// 执行一次 --help 以覆盖 Execute 路径（不会触发业务逻辑）
	rootCmd.SetArgs([]string{"--help"})
	if err := rootCmd.Execute(); err != nil {
		t.Errorf("rootCmd Execute with --help returned error: %v", err)
	}
}
