package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	suncalc "github.com/redtim/sunmooncalc"
	"github.com/xuri/excelize/v2"

	"github.com/bradfitz/latlong"
)

// -------------------- 数据结构 --------------------

type nominatimPlace struct {
	Lat         string `json:"lat"`
	Lon         string `json:"lon"`
	DisplayName string `json:"display_name"`
}

// 每日天文数据（既有人类可读字符串，也有数值字段，方便 chart）
type dailyAstro struct {
	Date string `json:"date"`

	Sunrise   string `json:"sunrise"`
	Sunset    string `json:"sunset"`
	SolarNoon string `json:"solar_noon"`

	MaxAltitude   string  `json:"max_altitude_deg"`
	DayLength     string  `json:"day_length_hhmm"`
	Moonrise      string  `json:"moonrise"`
	Moonset       string  `json:"moonset"`
	MoonIllumFrac string  `json:"moon_illumination"`

	MaxAltitudeNum       float64 `json:"max_altitude_num,omitempty"`
	DayLengthMinutes     int     `json:"day_length_minutes,omitempty"`
	MoonIlluminationNum  float64 `json:"moon_illumination_num,omitempty"` // 0~1
}

type CityContext struct {
	City        string
	DisplayName string
	Lat, Lon    float64
	TZID        string
	Loc         *time.Location
	Now         time.Time
}

// 缓存结构
type CityCacheEntry struct {
	City        string   `json:"city"`
	Normalized  string   `json:"normalized"`
	DisplayName string   `json:"display_name"`
	Lat         float64  `json:"lat"`
	Lon         float64  `json:"lon"`
	TimezoneID  string   `json:"timezone_id"`
	Aliases     []string `json:"aliases,omitempty"`
	UpdatedAt   string   `json:"updated_at"`
}

type CityCache struct {
	Entries map[string]CityCacheEntry `json:"entries"`
}

// 内置别名
var builtinAliases = map[string][]string{
	"beijing":   {"北京", "Beijing", "Peking"},
	"shanghai":  {"上海", "Shanghai"},
	"guangzhou": {"广州", "Guangzhou", "Canton"},
}

// -------------------- 工具函数 --------------------

func radToDeg(r float64) float64 {
	return r * 180 / math.Pi
}

// 太阳距离（km），近似
func earthSunDistanceKm(t time.Time) float64 {
	const auKm = 149597870.7
	jd := julianDay(t)
	d := jd - 2451545.0
	gDeg := 357.529 + 0.98560028*d
	gRad := gDeg * math.Pi / 180
	R := 1.00014 - 0.01671*math.Cos(gRad) - 0.00014*math.Cos(2*gRad)
	return R * auKm
}

// Unix → JD
func julianDay(t time.Time) float64 {
	u := t.UTC()
	return float64(u.Unix())/86400.0 + 2440587.5
}

func formatTimeLocal(t time.Time) string {
	if t.IsZero() {
		return "--"
	}
	return t.Format("15:04")
}

func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "--"
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	return fmt.Sprintf("%02d:%02d", h, m)
}

func parseDateInLocation(dateStr string, loc *time.Location) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02", dateStr, loc)
	if err != nil {
		return time.Time{}, err
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 12, 0, 0, 0, loc), nil
}

func normalizeCityKey(city string) string {
	return strings.ToLower(strings.TrimSpace(city))
}

func cacheFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".esunmoon-cache.json"
	}
	return filepath.Join(home, ".esunmoon-cache.json")
}

func loadCache() *CityCache {
	path := cacheFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		return &CityCache{Entries: make(map[string]CityCacheEntry)}
	}
	var cache CityCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return &CityCache{Entries: make(map[string]CityCacheEntry)}
	}
	if cache.Entries == nil {
		cache.Entries = make(map[string]CityCacheEntry)
	}
	return &cache
}

func saveCache(cache *CityCache) error {
	path := cacheFilePath()
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// -------------------- 网络调用：地理编码 --------------------

// Nominatim: 城市 → 经纬度
func geocodeCity(city string) (lat, lon float64, displayName string, err error) {
	baseURL := "https://nominatim.openstreetmap.org/search"
	q := url.Values{}
	q.Set("q", city)
	q.Set("format", "json")
	q.Set("limit", "1")
	q.Set("accept-language", "zh-CN,en")

	req, err := http.NewRequest("GET", baseURL+"?"+q.Encode(), nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", "eSunMoon/1.0 (contact: your-email@example.com)")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("Nominatim HTTP 错误: %s", resp.Status)
		return
	}

	var places []nominatimPlace
	if err = json.NewDecoder(resp.Body).Decode(&places); err != nil {
		return
	}

	if len(places) == 0 {
		err = fmt.Errorf("未找到城市: %s", city)
		return
	}

	lat, err = strconv.ParseFloat(places[0].Lat, 64)
	if err != nil {
		return
	}
	lon, err = strconv.ParseFloat(places[0].Lon, 64)
	if err != nil {
		return
	}
	displayName = places[0].DisplayName
	return
}

// 本地 latlong：经纬度 → 时区 ID（如 Asia/Shanghai），不依赖任何网络
func lookupTimeZone(lat, lon float64) (string, error) {
	tzID := latlong.LookupZoneName(lat, lon)
	if tzID == "" {
		return "", fmt.Errorf("无法根据经纬度 (%.6f, %.6f) 映射到时区 ID", lat, lon)
	}
	return tzID, nil
}

// -------------------- 天文数据生成 --------------------

func generateAstroData(cityName string, lat, lon float64, loc *time.Location, start time.Time, days int) ([]dailyAstro, error) {
	if days <= 0 {
		return nil, fmt.Errorf("天数必须 > 0")
	}
	var result []dailyAstro

	for i := 0; i < days; i++ {
		day := start.AddDate(0, 0, i)
		dayDateStr := day.Format("2006-01-02")

		sunTimes := suncalc.GetTimes(day, lat, lon)
		sunrise := sunTimes[suncalc.Sunrise].Value.In(loc)
		sunset := sunTimes[suncalc.Sunset].Value.In(loc)
		solarNoon := sunTimes[suncalc.SolarNoon].Value.In(loc)

		maxAltitude := "--"
		var maxAltitudeNum float64
		if !solarNoon.IsZero() {
			pos := suncalc.GetPosition(solarNoon, lat, lon)
			maxAltDeg := radToDeg(pos.Altitude)
			maxAltitudeNum = maxAltDeg
			maxAltitude = fmt.Sprintf("%.2f", maxAltDeg)
		}

		dayLengthStr := "--"
		dayLengthMinutes := 0
		if !sunrise.IsZero() && !sunset.IsZero() {
			dayLength := sunset.Sub(sunrise)
			dayLengthStr = formatDuration(dayLength)
			dayLengthMinutes = int(dayLength.Minutes())
		}

		moonTimes := suncalc.GetMoonTimes(day, lat, lon, false)
		moonrise := moonTimes.Rise.In(loc)
		moonset := moonTimes.Set.In(loc)
		moonIllum := suncalc.GetMoonIllumination(day)
		moonIllumFrac := moonIllum.Fraction
		moonIllumPct := fmt.Sprintf("%.1f%%", moonIllumFrac*100)

		result = append(result, dailyAstro{
			Date: dayDateStr,

			Sunrise:   formatTimeLocal(sunrise),
			Sunset:    formatTimeLocal(sunset),
			SolarNoon: formatTimeLocal(solarNoon),

			MaxAltitude:   maxAltitude,
			DayLength:     dayLengthStr,
			Moonrise:      formatTimeLocal(moonrise),
			Moonset:       formatTimeLocal(moonset),
			MoonIllumFrac: moonIllumPct,

			MaxAltitudeNum:      maxAltitudeNum,
			DayLengthMinutes:    dayLengthMinutes,
			MoonIlluminationNum: moonIllumFrac,
		})
	}
	return result, nil
}

// -------------------- 多格式输出 --------------------

var outFormat string // txt/csv/json/excel

func writeAstroFile(cityName string, now time.Time, data []dailyAstro, desc, baseName string) (string, error) {
	if baseName == "" {
		baseName = fmt.Sprintf("%s-%s", sanitizeFileName(cityName), now.Format("2006-01-02"))
	}
	switch strings.ToLower(outFormat) {
	case "txt":
		return writeAstroTxt(cityName, now, data, desc, baseName+".txt")
	case "csv":
		return writeAstroCSV(cityName, now, data, desc, baseName+".csv")
	case "json":
		return writeAstroJSON(cityName, now, data, desc, baseName+".json")
	case "excel", "xlsx":
		return writeAstroExcel(cityName, now, data, desc, baseName+".xlsx")
	default:
		return writeAstroTxt(cityName, now, data, desc, baseName+".txt")
	}
}

func writeAstroTxt(cityName string, now time.Time, data []dailyAstro, desc, filePath string) (string, error) {
	f, err := os.Create(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	fmt.Fprintf(w, "# eSunMoon 城市天文数据：%s\n", cityName)
	fmt.Fprintf(w, "# 生成日期（当地时间）：%s\n", now.Format("2006-01-02 15:04:05"))
	if desc != "" {
		fmt.Fprintf(w, "# 范围：%s\n", desc)
	}
	fmt.Fprintln(w, "# 所有时间均为城市所在时区的当地时间。")

	fmt.Fprintln(w, "日期\t日出\t日落\t太阳最高时刻\t太阳最高高度(°)\t日照时长(hh:mm)\t月出\t月落\t月亮可见光比例")
	for _, d := range data {
		line := fmt.Sprintf(
			"%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s",
			d.Date, d.Sunrise, d.Sunset, d.SolarNoon,
			d.MaxAltitude, d.DayLength,
			d.Moonrise, d.Moonset, d.MoonIllumFrac,
		)
		fmt.Fprintln(w, line)
	}
	if err := w.Flush(); err != nil {
		return "", err
	}
	return filePath, nil
}

func writeAstroCSV(cityName string, now time.Time, data []dailyAstro, desc, filePath string) (string, error) {
	f, err := os.Create(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	_ = w.Write([]string{"city", cityName})
	_ = w.Write([]string{"generated_at", now.Format(time.RFC3339)})
	if desc != "" {
		_ = w.Write([]string{"range", desc})
	}
	_ = w.Write([]string{})
	_ = w.Write([]string{
		"date", "sunrise", "sunset", "solar_noon",
		"max_altitude_deg", "max_altitude_num",
		"day_length_hhmm", "day_length_minutes",
		"moonrise", "moonset",
		"moon_illumination", "moon_illumination_num",
	})

	for _, d := range data {
		_ = w.Write([]string{
			d.Date,
			d.Sunrise,
			d.Sunset,
			d.SolarNoon,
			d.MaxAltitude,
			fmt.Sprintf("%.4f", d.MaxAltitudeNum),
			d.DayLength,
			strconv.Itoa(d.DayLengthMinutes),
			d.Moonrise,
			d.Moonset,
			d.MoonIllumFrac,
			fmt.Sprintf("%.4f", d.MoonIlluminationNum),
		})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return "", err
	}
	return filePath, nil
}

func writeAstroJSON(cityName string, now time.Time, data []dailyAstro, desc, filePath string) (string, error) {
	wrapper := struct {
		City       string       `json:"city"`
		Generated  string       `json:"generated_at"`
		Range      string       `json:"range,omitempty"`
		Data       []dailyAstro `json:"data"`
		LocalTZTip string       `json:"local_time_tip"`
	}{
		City:       cityName,
		Generated:  now.Format(time.RFC3339),
		Range:      desc,
		Data:       data,
		LocalTZTip: "所有时间均为城市所在时区的当地时间",
	}
	b, err := json.MarshalIndent(wrapper, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filePath, b, 0o644); err != nil {
		return "", err
	}
	return filePath, nil
}

func writeAstroExcel(cityName string, now time.Time, data []dailyAstro, desc, filePath string) (string, error) {
	f := excelize.NewFile()
	sheet := "Astro"
	f.SetSheetName(f.GetSheetName(0), sheet)

	f.SetCellValue(sheet, "A1", "城市")
	f.SetCellValue(sheet, "B1", cityName)
	f.SetCellValue(sheet, "A2", "生成时间")
	f.SetCellValue(sheet, "B2", now.Format("2006-01-02 15:04:05"))
	if desc != "" {
		f.SetCellValue(sheet, "A3", "范围")
		f.SetCellValue(sheet, "B3", desc)
	}
	f.SetCellValue(sheet, "A4", "提示")
	f.SetCellValue(sheet, "B4", "所有时间均为城市所在时区的当地时间")

	headers := []string{
		"日期",
		"日出", "日落", "太阳最高时刻",
		"太阳最高高度(°)", "最高高度数值",
		"日照时长(hh:mm)", "日照时长(分钟)",
		"月出", "月落",
		"月亮可见光比例", "月亮光照数值",
	}
	for i, h := range headers {
		cell, _ := excelize.CoordinatesToCellName(i+1, 6)
		f.SetCellValue(sheet, cell, h)
	}

	row := 7
	for _, d := range data {
		values := []interface{}{
			d.Date,
			d.Sunrise, d.Sunset, d.SolarNoon,
			d.MaxAltitude, d.MaxAltitudeNum,
			d.DayLength, d.DayLengthMinutes,
			d.Moonrise, d.Moonset,
			d.MoonIllumFrac, d.MoonIlluminationNum,
		}
		for col, v := range values {
			cell, _ := excelize.CoordinatesToCellName(col+1, row)
			f.SetCellValue(sheet, cell, v)
		}
		row++
	}
	if err := f.SaveAs(filePath); err != nil {
		return "", err
	}
	return filePath, nil
}

// -------------------- City & 缓存 & 实时位置 --------------------

func findEntryInCache(cache *CityCache, city string) (CityCacheEntry, bool) {
	key := normalizeCityKey(city)
	if e, ok := cache.Entries[key]; ok {
		return e, true
	}
	for _, e := range cache.Entries {
		if normalizeCityKey(e.City) == key {
			return e, true
		}
		for _, a := range e.Aliases {
			if normalizeCityKey(a) == key {
				return e, true
			}
		}
	}
	return CityCacheEntry{}, false
}

func prepareCity(city string, offline bool) (*CityContext, error) {
	if city == "" {
		return nil, fmt.Errorf("未输入城市名")
	}
	cache := loadCache()

	if entry, ok := findEntryInCache(cache, city); ok {
		loc, err := time.LoadLocation(entry.TimezoneID)
		if err != nil {
			return nil, fmt.Errorf("加载缓存时区失败 (%s): %w", entry.TimezoneID, err)
		}
		now := time.Now().In(loc)
		ctx := &CityContext{
			City:        entry.City,
			DisplayName: entry.DisplayName,
			Lat:         entry.Lat,
			Lon:         entry.Lon,
			TZID:        entry.TimezoneID,
			Loc:         loc,
			Now:         now,
		}
		fmt.Println("-------------------------------------------------")
		fmt.Printf("[eSunMoon] 城市输入: %s\n", city)
		fmt.Printf("解析结果（来自缓存）: %s\n", entry.DisplayName)
		fmt.Printf("经纬度（缓存）:  %.4f, %.4f\n", entry.Lat, entry.Lon)
		fmt.Printf("时区（缓存）:    %s\n", entry.TimezoneID)
		fmt.Printf("当前当地时间: %s\n", now.Format("2006-01-02 15:04:05"))
		fmt.Println("-------------------------------------------------")
		printSunMoonPosition(ctx)
		return ctx, nil
	}

	if offline {
		return nil, fmt.Errorf("离线模式：城市 [%s] 未在缓存中，无法联网查询，请先在联网状态下运行一次。", city)
	}

	lat, lon, displayName, err := geocodeCity(city)
	if err != nil {
		return nil, fmt.Errorf("获取城市坐标失败: %w", err)
	}
	tzID, err := lookupTimeZone(lat, lon)
	if err != nil {
		return nil, fmt.Errorf("自动检测时区失败: %w", err)
	}
	loc, err := time.LoadLocation(tzID)
	if err != nil {
		return nil, fmt.Errorf("加载时区失败 (%s): %w", tzID, err)
	}
	now := time.Now().In(loc)

	ctx := &CityContext{
		City:        city,
		DisplayName: displayName,
		Lat:         lat,
		Lon:         lon,
		TZID:        tzID,
		Loc:         loc,
		Now:         now,
	}

	fmt.Println("-------------------------------------------------")
	fmt.Printf("[eSunMoon] 城市输入: %s\n", city)
	fmt.Printf("解析结果: %s\n", displayName)
	fmt.Printf("经纬度:  %.4f, %.4f\n", lat, lon)
	fmt.Printf("时区:    %s\n", tzID)
	fmt.Printf("当前当地时间: %s\n", now.Format("2006-01-02 15:04:05"))
	fmt.Println("-------------------------------------------------")
	printSunMoonPosition(ctx)

	aliases := []string{city}
	if extra, ok := builtinAliases[normalizeCityKey(city)]; ok {
		aliases = append(aliases, extra...)
	}
	entry := CityCacheEntry{
		City:        city,
		Normalized:  normalizeCityKey(city),
		DisplayName: displayName,
		Lat:         lat,
		Lon:         lon,
		TimezoneID:  tzID,
		Aliases:     aliases,
		UpdatedAt:   time.Now().Format(time.RFC3339),
	}
	cache.Entries[entry.Normalized] = entry
	_ = saveCache(cache)

	return ctx, nil
}

func printSunMoonPosition(ctx *CityContext) {
	sunPos := suncalc.GetPosition(ctx.Now, ctx.Lat, ctx.Lon)
	moonPos := suncalc.GetMoonPosition(ctx.Now, ctx.Lat, ctx.Lon)

	sunAzDeg := radToDeg(sunPos.Azimuth)
	sunAltDeg := radToDeg(sunPos.Altitude)
	sunDistKm := earthSunDistanceKm(ctx.Now)

	moonAzDeg := radToDeg(moonPos.Azimuth)
	moonAltDeg := radToDeg(moonPos.Altitude)
	moonDistKm := moonPos.Distance

	fmt.Println("实时天体位置（当地时间）")
	fmt.Printf("太阳：方位角 %.2f°，高度角 %.2f°，距离约 %.0f km\n", sunAzDeg, sunAltDeg, sunDistKm)
	fmt.Printf("月亮：方位角 %.2f°，高度角 %.2f°，距离约 %.0f km\n", moonAzDeg, moonAltDeg, moonDistKm)
	fmt.Println("-------------------------------------------------")
}

func sanitizeFileName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, "\\", "-")
	return name
}

// -------------------- 三种模式业务入口（CLI+HTTP 共用） --------------------

func buildYearData(ctx *CityContext) (data []dailyAstro, desc, baseName string, err error) {
	start := time.Date(ctx.Now.Year(), ctx.Now.Month(), ctx.Now.Day(), 12, 0, 0, 0, ctx.Loc)
	data, err = generateAstroData(ctx.City, ctx.Lat, ctx.Lon, ctx.Loc, start, 365)
	if err != nil {
		return nil, "", "", fmt.Errorf("生成年度天文数据失败: %w", err)
	}
	desc = fmt.Sprintf("从 %s 起连续 365 天", start.Format("2006-01-02"))
	baseName = fmt.Sprintf("%s-%s-year", sanitizeFileName(ctx.City), ctx.Now.Format("2006-01-02"))
	return
}

func buildDayData(ctx *CityContext, dateStr string) (data []dailyAstro, desc, baseName string, err error) {
	day, err := parseDateInLocation(dateStr, ctx.Loc)
	if err != nil {
		return nil, "", "", fmt.Errorf("解析日期失败（格式应为 YYYY-MM-DD）: %w", err)
	}
	data, err = generateAstroData(ctx.City, ctx.Lat, ctx.Lon, ctx.Loc, day, 1)
	if err != nil {
		return nil, "", "", fmt.Errorf("生成指定日期天文数据失败: %w", err)
	}
	desc = fmt.Sprintf("指定日期：%s", day.Format("2006-01-02"))
	baseName = fmt.Sprintf("%s-%s", sanitizeFileName(ctx.City), day.Format("2006-01-02"))
	return
}

func buildRangeData(ctx *CityContext, fromStr, toStr string) (data []dailyAstro, desc, baseName string, err error) {
	start, err := parseDateInLocation(fromStr, ctx.Loc)
	if err != nil {
		return nil, "", "", fmt.Errorf("解析起始日期失败（格式应为 YYYY-MM-DD）: %w", err)
	}
	end, err := parseDateInLocation(toStr, ctx.Loc)
	if err != nil {
		return nil, "", "", fmt.Errorf("解析结束日期失败（格式应为 YYYY-MM-DD）: %w", err)
	}
	if end.Before(start) {
		return nil, "", "", fmt.Errorf("结束日期不能早于起始日期")
	}
	days := int(end.Sub(start).Hours()/24) + 1
	data, err = generateAstroData(ctx.City, ctx.Lat, ctx.Lon, ctx.Loc, start, days)
	if err != nil {
		return nil, "", "", fmt.Errorf("生成区间天文数据失败: %w", err)
	}
	desc = fmt.Sprintf("日期区间：%s ~ %s，共 %d 天", start.Format("2006-01-02"), end.Format("2006-01-02"), days)
	baseName = fmt.Sprintf("%s-%s_to_%s", sanitizeFileName(ctx.City), start.Format("2006-01-02"), end.Format("2006-01-02"))
	return
}

func runYear(ctx *CityContext) error {
	data, desc, baseName, err := buildYearData(ctx)
	if err != nil {
		return err
	}
	outFile, err := writeAstroFile(ctx.City, ctx.Now, data, desc, baseName)
	if err != nil {
		return fmt.Errorf("写入文件失败: %w", err)
	}
	fmt.Printf("已生成年度天文数据文件：%s\n", outFile)
	return nil
}

func runDay(ctx *CityContext, dateStr string) error {
	data, desc, baseName, err := buildDayData(ctx, dateStr)
	if err != nil {
		return err
	}
	outFile, err := writeAstroFile(ctx.City, ctx.Now, data, desc, baseName)
	if err != nil {
		return fmt.Errorf("写入文件失败: %w", err)
	}
	fmt.Printf("已生成指定日期天文数据文件：%s\n", outFile)
	return nil
}

func runRange(ctx *CityContext, fromStr, toStr string) error {
	data, desc, baseName, err := buildRangeData(ctx, fromStr, toStr)
	if err != nil {
		return err
	}
	outFile, err := writeAstroFile(ctx.City, ctx.Now, data, desc, baseName)
	if err != nil {
		return fmt.Errorf("写入文件失败: %w", err)
	}
	fmt.Printf("已生成区间天文数据文件：%s\n", outFile)
	return nil
}

// -------------------- TUI 模型 --------------------

type tuiStep int

const (
	stepMain tuiStep = iota
	stepDayInput
	stepRangeFromInput
	stepRangeToInput
	stepDone
)

type tuiModel struct {
	step       tuiStep
	input      string
	cachedKeys []string

	chosenCity string
	modeIndex  int
	modes      []string

	formatIndex int
	formats     []string

	dayDate   string
	rangeFrom string
	rangeTo   string

	errMsg   string
	quitting bool
}

func newTuiModel(cache *CityCache) tuiModel {
	keys := make([]string, 0, len(cache.Entries))
	for _, e := range cache.Entries {
		keys = append(keys, fmt.Sprintf("%s (%s)", e.City, e.TimezoneID))
	}
	return tuiModel{
		step:        stepMain,
		input:       "",
		cachedKeys:  keys,
		modeIndex:   0,
		modes:       []string{"Year", "Day", "Range"},
		formatIndex: 0,
		formats:     []string{"txt", "csv", "json", "excel"},
	}
}

func (m tuiModel) Init() tea.Cmd {
	return nil
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		key := msg.String()
		switch key {
		case "ctrl+c", "esc":
			m.quitting = true
			return m, tea.Quit
		case "enter":
			return m.handleEnter()
		case "backspace", "ctrl+h":
			if len(m.input) > 0 {
				m.input = m.input[:len(m.input)-1]
			}
		case "left", "h":
			if m.step == stepMain && m.modeIndex > 0 {
				m.modeIndex--
			}
		case "right", "l":
			if m.step == stepMain && m.modeIndex < len(m.modes)-1 {
				m.modeIndex++
			}
		case "up", "k":
			if m.step == stepMain && m.formatIndex > 0 {
				m.formatIndex--
			}
		case "down", "j":
			if m.step == stepMain && m.formatIndex < len(m.formats)-1 {
				m.formatIndex++
			}
		default:
			if len(msg.String()) == 1 {
				m.input += msg.String()
			}
		}
	}
	return m, nil
}

func (m tuiModel) handleEnter() (tea.Model, tea.Cmd) {
	switch m.step {
	case stepMain:
		m.errMsg = ""
		city := strings.TrimSpace(m.input)
		if city == "" && len(m.cachedKeys) > 0 {
			first := m.cachedKeys[0]
			if idx := strings.Index(first, " ("); idx > 0 {
				city = first[:idx]
			} else {
				city = first
			}
		}
		if city == "" {
			m.errMsg = "请输入城市名或确保有缓存城市。"
			return m, nil
		}
		m.chosenCity = city
		switch m.modes[m.modeIndex] {
		case "Year":
			m.step = stepDone
			m.quitting = true
			return m, tea.Quit
		case "Day":
			m.step = stepDayInput
			m.input = ""
		case "Range":
			m.step = stepRangeFromInput
			m.input = ""
		}
	case stepDayInput:
		m.errMsg = ""
		dateStr := strings.TrimSpace(m.input)
		if dateStr == "" {
			m.errMsg = "日期不能为空。"
			return m, nil
		}
		m.dayDate = dateStr
		m.step = stepDone
		m.quitting = true
		return m, tea.Quit
	case stepRangeFromInput:
		m.errMsg = ""
		from := strings.TrimSpace(m.input)
		if from == "" {
			m.errMsg = "起始日期不能为空。"
			return m, nil
		}
		m.rangeFrom = from
		m.input = ""
		m.step = stepRangeToInput
	case stepRangeToInput:
		m.errMsg = ""
		to := strings.TrimSpace(m.input)
		if to == "" {
			m.errMsg = "结束日期不能为空。"
			return m, nil
		}
		m.rangeTo = to
		m.step = stepDone
		m.quitting = true
		return m, tea.Quit
	}
	return m, nil
}

func (m tuiModel) View() string {
	if m.quitting {
		return ""
	}
	var b strings.Builder
	fmt.Fprintln(&b, "eSunMoon - 城市天文数据生成器 (TUI)")
	fmt.Fprintln(&b, "====================================")
	switch m.step {
	case stepMain:
		fmt.Fprintln(&b, "缓存中的城市：")
		if len(m.cachedKeys) == 0 {
			fmt.Fprintln(&b, "  (暂无缓存城市，联网运行一次后会自动写入)")
		} else {
			for _, c := range m.cachedKeys {
				fmt.Fprintf(&b, "  - %s\n", c)
			}
		}
		fmt.Fprintln(&b, "")
		fmt.Fprintln(&b, "模式选择（←/→ 切换）：")
		for i, mode := range m.modes {
			if i == m.modeIndex {
				fmt.Fprintf(&b, "  [%s] ", mode)
			} else {
				fmt.Fprintf(&b, "   %s  ", mode)
			}
		}
		fmt.Fprintln(&b, "")
		fmt.Fprintln(&b, "输出格式（↑/↓ 切换）：")
		for i, f := range m.formats {
			if i == m.formatIndex {
				fmt.Fprintf(&b, "  [%s] ", f)
			} else {
				fmt.Fprintf(&b, "   %s  ", f)
			}
		}
		fmt.Fprintln(&b, "")
		fmt.Fprintln(&b, "请输入城市名（支持中文/英文），回车确认；Ctrl+C 退出。")
		fmt.Fprintf(&b, "> %s\n", m.input)
	case stepDayInput:
		fmt.Fprintf(&b, "城市：%s\n", m.chosenCity)
		fmt.Fprintf(&b, "模式：Day    输出格式：%s\n", m.formats[m.formatIndex])
		fmt.Fprintln(&b, "")
		fmt.Fprintln(&b, "请输入日期 (YYYY-MM-DD)，回车确认；Ctrl+C 取消。")
		fmt.Fprintf(&b, "> %s\n", m.input)
	case stepRangeFromInput:
		fmt.Fprintf(&b, "城市：%s\n", m.chosenCity)
		fmt.Fprintf(&b, "模式：Range  输出格式：%s\n", m.formats[m.formatIndex])
		fmt.Fprintln(&b, "")
		fmt.Fprintln(&b, "请输入起始日期 From (YYYY-MM-DD)，回车确认；Ctrl+C 取消。")
		fmt.Fprintf(&b, "> %s\n", m.input)
	case stepRangeToInput:
		fmt.Fprintf(&b, "城市：%s\n", m.chosenCity)
		fmt.Fprintf(&b, "模式：Range  输出格式：%s\n", m.formats[m.formatIndex])
		fmt.Fprintf(&b, "起始日期：%s\n", m.rangeFrom)
		fmt.Fprintln(&b, "")
		fmt.Fprintln(&b, "请输入结束日期 To (YYYY-MM-DD)，回车确认；Ctrl+C 取消。")
		fmt.Fprintf(&b, "> %s\n", m.input)
	}
	if m.errMsg != "" {
		fmt.Fprintln(&b, "")
		fmt.Fprintf(&b, "错误：%s\n", m.errMsg)
	}
	return b.String()
}

// -------------------- HTTP API --------------------

type astroAPIResponse struct {
	City       string       `json:"city"`
	Display    string       `json:"display_name"`
	Lat        float64      `json:"lat"`
	Lon        float64      `json:"lon"`
	Timezone   string       `json:"timezone"`
	Mode       string       `json:"mode"`
	Range      string       `json:"range,omitempty"`
	Generated  string       `json:"generated_at"`
	Data       []dailyAstro `json:"data"`
	LocalTZTip string       `json:"local_time_tip"`
}

func astroAPIHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	mode := strings.ToLower(q.Get("mode"))
	if mode == "" {
		mode = "year"
	}

	// 优先走 coords 模式：lat+lon+tz
	latStr := q.Get("lat")
	lonStr := q.Get("lon")
	tzID := q.Get("tz")

	var (
		ctx *CityContext
		err error
	)

	if latStr != "" && lonStr != "" && tzID != "" {
		lat, err1 := strconv.ParseFloat(latStr, 64)
		lon, err2 := strconv.ParseFloat(lonStr, 64)
		if err1 != nil || err2 != nil {
			http.Error(w, "lat/lon 解析失败", http.StatusBadRequest)
			return
		}
		loc, err := time.LoadLocation(tzID)
		if err != nil {
			http.Error(w, "tz 加载失败: "+err.Error(), http.StatusBadRequest)
			return
		}
		now := time.Now().In(loc)
		cityName := q.Get("city")
		if cityName == "" {
			cityName = fmt.Sprintf("coords_%.4f_%.4f", lat, lon)
		}
		ctx = &CityContext{
			City:        cityName,
			DisplayName: cityName,
			Lat:         lat,
			Lon:         lon,
			TZID:        tzID,
			Loc:         loc,
			Now:         now,
		}
	} else {
		// 其次走城市名模式
		city := q.Get("city")
		if city == "" {
			http.Error(w, "必须提供 city 或 lat+lon+tz 参数", http.StatusBadRequest)
			return
		}
		ctx, err = prepareCity(city, offline)
		if err != nil {
			http.Error(w, "城市解析失败: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	var (
		data []dailyAstro
		desc string
	)

	switch mode {
	case "year":
		var base string
		data, desc, base, err = buildYearData(ctx)
		_ = base
	case "day":
		dateStr := q.Get("date")
		if dateStr == "" {
			http.Error(w, "mode=day 时必须提供 date=YYYY-MM-DD", http.StatusBadRequest)
			return
		}
		var base string
		data, desc, base, err = buildDayData(ctx, dateStr)
		_ = base
	case "range":
		fromStr := q.Get("from")
		toStr := q.Get("to")
		if fromStr == "" || toStr == "" {
			http.Error(w, "mode=range 时必须提供 from/to=YYYY-MM-DD", http.StatusBadRequest)
			return
		}
		var base string
		data, desc, base, err = buildRangeData(ctx, fromStr, toStr)
		_ = base
	default:
		http.Error(w, "mode 必须为 year/day/range", http.StatusBadRequest)
		return
	}

	if err != nil {
		http.Error(w, "生成天文数据失败: "+err.Error(), http.StatusInternalServerError)
		return
	}

	resp := astroAPIResponse{
		City:       ctx.City,
		Display:    ctx.DisplayName,
		Lat:        ctx.Lat,
		Lon:        ctx.Lon,
		Timezone:   ctx.TZID,
		Mode:       mode,
		Range:      desc,
		Generated:  ctx.Now.Format(time.RFC3339),
		Data:       data,
		LocalTZTip: "所有时间均为城市所在时区的当地时间",
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// -------------------- Cobra 层 --------------------

var (
	dayDate    string
	rangeFromS string
	rangeToS   string
	offline    bool
	cacheForce bool

	// coords 子命令 flags
	coordsLat   float64
	coordsLon   float64
	coordsTZ    string
	coordsMode  string
	coordsDate  string
	coordsFrom  string
	coordsTo    string
	coordsCity  string

	// serve 子命令 flag
	serveAddr string
)

func getCityFromArgsOrPrompt(args []string) string {
	if len(args) > 0 {
		return strings.Join(args, " ")
	}
	fmt.Print("请输入城市名（支持中文或英文）：")
	reader := bufio.NewReader(os.Stdin)
	text, _ := reader.ReadString('\n')
	return strings.TrimSpace(text)
}

var rootCmd = &cobra.Command{
	Use:   "esunmoon [城市名...]",
	Short: "eSunMoon - 城市天文数据生成器",
	Long: `eSunMoon - 城市天文数据生成器

根据城市名称或经纬度自动获取时区，生成天文数据（全部为当地时间），
并在控制台输出当前时间太阳与月亮的位置（方位角、高度角、距离）。

默认行为：等同于 "esunmoon year <城市名>"，即从今天起一年。
支持 --offline 仅使用本地缓存，不进行任何网络请求。
支持 --format txt/csv/json/excel。`,
	Args: cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		city := getCityFromArgsOrPrompt(args)
		if city == "" {
			return fmt.Errorf("城市名不能为空")
		}
		ctx, err := prepareCity(city, offline)
		if err != nil {
			return err
		}
		return runYear(ctx)
	},
}

var yearCmd = &cobra.Command{
	Use:   "year [城市名...]",
	Short: "从今天起一年（365 天）的天文数据",
	Args:  cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		city := getCityFromArgsOrPrompt(args)
		if city == "" {
			return fmt.Errorf("城市名不能为空")
		}
		ctx, err := prepareCity(city, offline)
		if err != nil {
			return err
		}
		return runYear(ctx)
	},
}

var dayCmd = &cobra.Command{
	Use:   "day [城市名...]",
	Short: "指定单日的天文数据",
	Args:  cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if dayDate == "" {
			return fmt.Errorf("必须使用 --date 指定日期（YYYY-MM-DD）")
		}
		city := getCityFromArgsOrPrompt(args)
		if city == "" {
			return fmt.Errorf("城市名不能为空")
		}
		ctx, err := prepareCity(city, offline)
		if err != nil {
			return err
		}
		return runDay(ctx, dayDate)
	},
}

var rangeCmd = &cobra.Command{
	Use:   "range [城市名...]",
	Short: "指定日期区间的天文数据",
	Args:  cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if rangeFromS == "" || rangeToS == "" {
			return fmt.Errorf("必须同时指定 --from 和 --to（格式：YYYY-MM-DD）")
		}
		city := getCityFromArgsOrPrompt(args)
		if city == "" {
			return fmt.Errorf("城市名不能为空")
		}
		ctx, err := prepareCity(city, offline)
		if err != nil {
			return err
		}
		return runRange(ctx, rangeFromS, rangeToS)
	},
}

// coords 子命令：直接用经纬度 + 时区
var coordsCmd = &cobra.Command{
	Use:   "coords",
	Short: "通过经纬度 + 时区直接生成天文数据（绕过城市地理编码）",
	RunE: func(cmd *cobra.Command, args []string) error {
		if coordsTZ == "" {
			return fmt.Errorf("--tz 必须指定，例如 Asia/Shanghai")
		}
		loc, err := time.LoadLocation(coordsTZ)
		if err != nil {
			return fmt.Errorf("加载时区失败 (%s): %w", coordsTZ, err)
		}
		now := time.Now().In(loc)
		cityName := coordsCity
		if cityName == "" {
			cityName = fmt.Sprintf("coords_%.4f_%.4f", coordsLat, coordsLon)
		}
		ctx := &CityContext{
			City:        cityName,
			DisplayName: cityName,
			Lat:         coordsLat,
			Lon:         coordsLon,
			TZID:        coordsTZ,
			Loc:         loc,
			Now:         now,
		}

		fmt.Println("-------------------------------------------------")
		fmt.Printf("[eSunMoon] Coords 模式\n")
		fmt.Printf("城市名: %s\n", cityName)
		fmt.Printf("经纬度: %.4f, %.4f\n", coordsLat, coordsLon)
		fmt.Printf("时区:   %s\n", coordsTZ)
		fmt.Printf("当前当地时间: %s\n", now.Format("2006-01-02 15:04:05"))
		fmt.Println("-------------------------------------------------")
		printSunMoonPosition(ctx)

		mode := strings.ToLower(coordsMode)
		if mode == "" {
			mode = "year"
		}

		switch mode {
		case "year":
			return runYear(ctx)
		case "day":
			if coordsDate == "" {
				return fmt.Errorf("coords mode=day 时必须使用 --date 指定日期（YYYY-MM-DD）")
			}
			return runDay(ctx, coordsDate)
		case "range":
			if coordsFrom == "" || coordsTo == "" {
				return fmt.Errorf("coords mode=range 时必须同时指定 --from 和 --to（YYYY-MM-DD）")
			}
			return runRange(ctx, coordsFrom, coordsTo)
		default:
			return fmt.Errorf("coords --mode 必须为 year/day/range")
		}
	},
}

// TUI 子命令
var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "以终端 TUI 界面选择城市、模式和输出格式并生成天文数据",
	RunE: func(cmd *cobra.Command, args []string) error {
		cache := loadCache()
		m := newTuiModel(cache)
		p := tea.NewProgram(m)
		finalModel, err := p.Run()
		if err != nil {
			return fmt.Errorf("TUI 运行失败: %w", err)
		}
		tm := finalModel.(tuiModel)
		if tm.chosenCity == "" || tm.step != stepDone {
			fmt.Println("未完成选择，退出。")
			return nil
		}
		city := tm.chosenCity
		ctx, err := prepareCity(city, offline)
		if err != nil {
			return err
		}
		outFormat = tm.formats[tm.formatIndex]
		switch tm.modes[tm.modeIndex] {
		case "Year":
			return runYear(ctx)
		case "Day":
			return runDay(ctx, tm.dayDate)
		case "Range":
			return runRange(ctx, tm.rangeFrom, tm.rangeTo)
		default:
			return runYear(ctx)
		}
	},
}

// 缓存管理命令

var cacheCmd = &cobra.Command{
	Use:   "cache",
	Short: "缓存管理命令（list / clear）",
}

var cacheListCmd = &cobra.Command{
	Use:   "list",
	Short: "列出缓存中的城市信息",
	RunE: func(cmd *cobra.Command, args []string) error {
		cache := loadCache()
		if len(cache.Entries) == 0 {
			fmt.Println("缓存中暂无城市记录。")
			return nil
		}
		fmt.Println("缓存中的城市：")
		fmt.Println("------------------------------------------------------------")
		for _, e := range cache.Entries {
			fmt.Printf("城市: %s\n", e.City)
			fmt.Printf("  显示名: %s\n", e.DisplayName)
			fmt.Printf("  经纬度: %.4f, %.4f\n", e.Lat, e.Lon)
			fmt.Printf("  时区:   %s\n", e.TimezoneID)
			if len(e.Aliases) > 0 {
				fmt.Printf("  别名:   %s\n", strings.Join(e.Aliases, ", "))
			}
			fmt.Printf("  更新于: %s\n", e.UpdatedAt)
			fmt.Println("------------------------------------------------------------")
		}
		return nil
	},
}

var cacheClearCmd = &cobra.Command{
	Use:   "clear",
	Short: "清空本地缓存（~/.esunmoon-cache.json）",
	RunE: func(cmd *cobra.Command, args []string) error {
		path := cacheFilePath()
		if !cacheForce {
			reader := bufio.NewReader(os.Stdin)
			fmt.Printf("确认要删除缓存文件 %s 吗？此操作不可恢复。(y/N): ", path)
			line, _ := reader.ReadString('\n')
			line = strings.ToLower(strings.TrimSpace(line))
			if line != "y" && line != "yes" {
				fmt.Println("已取消清空缓存。")
				return nil
			}
		}
		if err := os.Remove(path); err != nil {
			if os.IsNotExist(err) {
				fmt.Println("缓存文件不存在，无需清理。")
				return nil
			}
			return fmt.Errorf("删除缓存文件失败: %w", err)
		}
		fmt.Println("已清空缓存。")
		return nil
	},
}

// serve 子命令：HTTP 服务模式
var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "启动 HTTP 服务，提供 /api/astro REST 接口（默认端口 :8080）",
	RunE: func(cmd *cobra.Command, args []string) error {
		if serveAddr == "" {
			serveAddr = ":8080"
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/api/astro", astroAPIHandler)
		mux.HandleFunc("/healthz", healthHandler)

		fmt.Printf("eSunMoon HTTP 服务启动：%s\n", serveAddr)
		fmt.Println("  GET /healthz")
		fmt.Println("  GET /api/astro?city=Beijing&mode=day&date=2025-01-01")
		fmt.Println("  或：/api/astro?lat=39.9&lon=116.4&tz=Asia/Shanghai&mode=year")

		return http.ListenAndServe(serveAddr, mux)
	},
}

func init() {
	rootCmd.PersistentFlags().BoolVar(&offline, "offline", false, "离线模式：仅使用本地缓存，不进行任何网络请求")
	rootCmd.PersistentFlags().StringVar(&outFormat, "format", "txt", "输出格式：txt/csv/json/excel")

	dayCmd.Flags().StringVarP(&dayDate, "date", "d", "", "指定日期（格式：YYYY-MM-DD）")
	rangeCmd.Flags().StringVar(&rangeFromS, "from", "", "起始日期（格式：YYYY-MM-DD）")
	rangeCmd.Flags().StringVar(&rangeToS, "to", "", "结束日期（格式：YYYY-MM-DD）")

	cacheClearCmd.Flags().BoolVarP(&cacheForce, "yes", "y", false, "不询问直接清空缓存")

	// coords flags
	coordsCmd.Flags().Float64Var(&coordsLat, "lat", 0, "纬度（必填）")
	coordsCmd.Flags().Float64Var(&coordsLon, "lon", 0, "经度（必填）")
	coordsCmd.Flags().StringVar(&coordsTZ, "tz", "", "时区 ID（如 Asia/Shanghai，必填）")
	coordsCmd.Flags().StringVar(&coordsMode, "mode", "year", "模式：year/day/range")
	coordsCmd.Flags().StringVar(&coordsDate, "date", "", "mode=day 时的日期 (YYYY-MM-DD)")
	coordsCmd.Flags().StringVar(&coordsFrom, "from", "", "mode=range 起始日期 (YYYY-MM-DD)")
	coordsCmd.Flags().StringVar(&coordsTo, "to", "", "mode=range 结束日期 (YYYY-MM-DD)")
	coordsCmd.Flags().StringVar(&coordsCity, "city", "", "自定义城市名（用于文件名和返回信息）")

	_ = coordsCmd.MarkFlagRequired("lat")
	_ = coordsCmd.MarkFlagRequired("lon")
	_ = coordsCmd.MarkFlagRequired("tz")

	// serve flags
	serveCmd.Flags().StringVar(&serveAddr, "addr", ":8080", "HTTP 监听地址，例如 :8080 或 127.0.0.1:9000")

	rootCmd.AddCommand(yearCmd)
	rootCmd.AddCommand(dayCmd)
	rootCmd.AddCommand(rangeCmd)
	rootCmd.AddCommand(coordsCmd)
	rootCmd.AddCommand(tuiCmd)
	rootCmd.AddCommand(serveCmd)

	cacheCmd.AddCommand(cacheListCmd)
	cacheCmd.AddCommand(cacheClearCmd)
	rootCmd.AddCommand(cacheCmd)
}

// -------------------- main --------------------

func main() {
	if err := rootCmd.Execute(); err != nil {
		log.Fatal(err)
	}
}
