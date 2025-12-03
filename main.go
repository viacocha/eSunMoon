package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	suncalc "github.com/redtim/sunmooncalc"
	"github.com/spf13/cobra"
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

	MaxAltitude   string `json:"max_altitude_deg"`
	DayLength     string `json:"day_length_hhmm"`
	Moonrise      string `json:"moonrise"`
	Moonset       string `json:"moonset"`
	MoonIllumFrac string `json:"moon_illumination"`

	MaxAltitudeNum      float64 `json:"max_altitude_num,omitempty"`
	DayLengthMinutes    int     `json:"day_length_minutes,omitempty"`
	MoonIlluminationNum float64 `json:"moon_illumination_num,omitempty"` // 0~1
	HasSunrise          bool    `json:"has_sunrise,omitempty"`
	HasSunset           bool    `json:"has_sunset,omitempty"`
	HasDayLength        bool    `json:"has_day_length,omitempty"`
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

const polarNote = "HasSunrise/HasSunset/HasDayLength 标志指示极昼/极夜等情况，false 表示当日无对应事件"
const defaultPositionsRefresh = 30 * time.Second

// -------------------- 工具函数 --------------------

// radToDeg 将弧度转换为角度。
func radToDeg(r float64) float64 {
	return r * 180 / math.Pi
}

// describeAzimuth 将方位角（库定义：0 为正南，向西为正）转为直观方位文字。
func describeAzimuth(azDeg float64) string {
	heading := math.Mod(azDeg+180, 360) // 转换为以正北为 0、顺时针的角度

	var base string
	var offset float64
	var toward string

	switch {
	case heading >= 315 || heading < 45:
		base = "正北"
		if heading >= 315 {
			offset = 360 - heading
			toward = "西"
		} else {
			offset = heading
			toward = "东"
		}
	case heading >= 45 && heading < 135:
		base = "正东"
		offset = math.Abs(heading - 90)
		if heading < 90 {
			toward = "北"
		} else {
			toward = "南"
		}
	case heading >= 135 && heading < 225:
		base = "正南"
		offset = math.Abs(heading - 180)
		if heading < 180 {
			toward = "东"
		} else {
			toward = "西"
		}
	default: // heading in [225, 315)
		base = "正西"
		offset = math.Abs(heading - 270)
		if heading < 270 {
			toward = "南"
		} else {
			toward = "北"
		}
	}

	if offset < 1 {
		return base
	}

	modifier := "偏"
	if offset < 10 {
		modifier = "略微偏"
	}

	return fmt.Sprintf("%s%s%s", base, modifier, toward)
}

// 太阳距离（km），近似
// earthSunDistanceKm 计算给定时间地球到太阳的近似距离（千米）。
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
// julianDay 将时间转换为儒略日。
func julianDay(t time.Time) float64 {
	u := t.UTC()
	return float64(u.Unix())/86400.0 + 2440587.5
}

// formatTimeLocal 将时间格式化为 HH:MM，空值返回 "--"。
func formatTimeLocal(t time.Time) string {
	if t.IsZero() {
		return "--"
	}
	return t.Format("15:04")
}

// formatDuration 将时长格式化为 hh:mm，非正时长返回 "--"。
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "--"
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	return fmt.Sprintf("%02d:%02d", h, m)
}

// parseDateInLocation 按本地时区解析 YYYY-MM-DD，固定到当天 12:00。
func parseDateInLocation(dateStr string, loc *time.Location) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02", dateStr, loc)
	if err != nil {
		return time.Time{}, err
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 12, 0, 0, 0, loc), nil
}

// normalizeCityKey 将城市名称归一化为小写去空格键。
func normalizeCityKey(city string) string {
	return strings.ToLower(strings.TrimSpace(city))
}

// cacheFilePath 返回缓存文件路径。
func cacheFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".esunmoon-cache.json"
	}
	return filepath.Join(home, ".esunmoon-cache.json")
}

// acquireFileLock 通过创建锁文件的方式获得文件锁，返回解锁函数。
func acquireFileLock(ctx context.Context, lockPath string) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.WriteString(fmt.Sprintf("pid=%d", os.Getpid()))
			_ = f.Close()
			return func() { _ = os.Remove(lockPath) }, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// loadCache 从磁盘读取缓存，不存在或损坏时返回空缓存。
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

// saveCache 原子写入缓存文件，并加锁防止并发写。
func saveCache(cache *CityCache) error {
	path := cacheFilePath()
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建缓存目录失败: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	unlock, err := acquireFileLock(ctx, path+".lock")
	if err != nil {
		return fmt.Errorf("获取缓存写锁失败: %w", err)
	}
	defer unlock()

	tmpFile, err := os.CreateTemp(dir, "esunmoon-cache-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时缓存文件失败: %w", err)
	}
	tmpName := tmpFile.Name()
	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("写入临时缓存失败: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("同步临时缓存失败: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("关闭临时缓存失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("原子写入缓存失败: %w", err)
	}
	return nil
}

// -------------------- 依赖注入与配置 --------------------

type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type AstroApp struct {
	client   HTTPClient
	cacheTTL time.Duration
	now      func() time.Time
	logger   *Logger
	tzLookup func(float64, float64) (string, error)
	loadTZ   func(string) (*time.Location, error)
}

// newAstroApp 创建默认的应用单例。
func newAstroApp() *AstroApp {
	return &AstroApp{
		client:   &http.Client{Timeout: 10 * time.Second},
		cacheTTL: 100 * 24 * time.Hour,
		now:      time.Now,
		logger:   NewLogger(os.Stdout, LevelInfo, false, false, time.Now),
		tzLookup: lookupTimeZone,
		loadTZ:   time.LoadLocation,
	}
}

var app = newAstroApp()

type AppConfig struct {
	Format         string
	AllowOverwrite bool
	OutDir         string
	Offline        bool
	LogLevel       string
	LogJSON        bool
	LogQuiet       bool
	LiveOnly       bool
	LiveInterval   time.Duration
}

var config = &AppConfig{
	Format:         "txt",
	AllowOverwrite: false,
	OutDir:         "",
	Offline:        false,
	LogLevel:       "info",
	LogJSON:        false,
	LogQuiet:       false,
	LiveOnly:       false,
	LiveInterval:   5 * time.Second,
}

// -------------------- Logger --------------------

type LogLevel int

const (
	LevelError LogLevel = iota
	LevelWarn
	LevelInfo
	LevelDebug
)

type Logger struct {
	out   io.Writer
	level LogLevel
	json  bool
	quiet bool
	now   func() time.Time
	mu    sync.Mutex
}

// NewLogger 创建支持级别、JSON、静默和自定义时间源的简单 logger。
func NewLogger(out io.Writer, level LogLevel, json bool, quiet bool, now func() time.Time) *Logger {
	if out == nil {
		out = io.Discard
	}
	if now == nil {
		now = time.Now
	}
	return &Logger{out: out, level: level, json: json, quiet: quiet, now: now}
}

// parseLogLevel 将字符串解析为日志级别。
func parseLogLevel(s string) LogLevel {
	switch strings.ToLower(s) {
	case "debug":
		return LevelDebug
	case "warn":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

// logf 输出一条日志，内部按级别过滤。
func (l *Logger) logf(level LogLevel, levelStr, format string, args ...interface{}) {
	if l == nil || l.quiet {
		return
	}
	if level > l.level {
		return
	}
	msg := fmt.Sprintf(format, args...)
	ts := l.now().Format(time.RFC3339)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.json {
		enc := json.NewEncoder(l.out)
		_ = enc.Encode(map[string]string{
			"ts":    ts,
			"level": levelStr,
			"msg":   msg,
		})
		return
	}
	fmt.Fprintf(l.out, "[%s] %-5s %s\n", ts, strings.ToUpper(levelStr), msg)
}

// logDebugf 输出 debug 日志。
func logDebugf(format string, args ...interface{}) {
	app.logger.logf(LevelDebug, "debug", format, args...)
}

// logInfof 输出 info 日志。
func logInfof(format string, args ...interface{}) {
	app.logger.logf(LevelInfo, "info", format, args...)
}

// logWarnf 输出 warn 日志。
func logWarnf(format string, args ...interface{}) {
	app.logger.logf(LevelWarn, "warn", format, args...)
}

// logErrorf 输出 error 日志。
func logErrorf(format string, args ...interface{}) {
	app.logger.logf(LevelError, "error", format, args...)
}

// -------------------- 网络调用：地理编码 --------------------

// Nominatim: 城市 → 经纬度
// geocodeCity 使用 Nominatim 服务将城市名解析为经纬度和显示名。
func geocodeCity(ctx context.Context, client HTTPClient, city string) (lat, lon float64, displayName string, err error) {
	if strings.TrimSpace(city) == "" {
		return 0, 0, "", fmt.Errorf("城市名不能为空")
	}
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
	req = req.WithContext(ctx)
	req.Header.Set("User-Agent", "eSunMoon/1.0 (https://example.com; contact: esunmoon@example.com)")

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
// lookupTimeZone 根据经纬度返回 IANA 时区 ID。
func lookupTimeZone(lat, lon float64) (string, error) {
	tzID := latlong.LookupZoneName(lat, lon)
	if tzID == "" {
		return "", fmt.Errorf("无法根据经纬度 (%.6f, %.6f) 映射到时区 ID", lat, lon)
	}
	return tzID, nil
}

// -------------------- 天文数据生成 --------------------

// generateAstroData 生成指定起始日期和天数的太阳月亮数据（当地时间）。
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
		hasSunrise := !sunrise.IsZero()
		hasSunset := !sunset.IsZero()
		if !solarNoon.IsZero() {
			pos := suncalc.GetPosition(solarNoon, lat, lon)
			maxAltDeg := radToDeg(pos.Altitude)
			maxAltitudeNum = maxAltDeg
			maxAltitude = fmt.Sprintf("%.2f", maxAltDeg)
		}

		dayLengthStr := "--"
		dayLengthMinutes := 0
		hasDayLength := hasSunrise && hasSunset
		if hasDayLength {
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
			HasSunrise:          hasSunrise,
			HasSunset:           hasSunset,
			HasDayLength:        hasDayLength,
		})
	}
	return result, nil
}

// -------------------- 多格式输出 --------------------

type OutputOptions struct {
	Format         string
	AllowOverwrite bool
	OutDir         string
}

// writeAstroFile 根据输出格式写文件，返回文件路径。
func writeAstroFile(format string, allowOverwrite bool, outDir string, cityName string, now time.Time, data []dailyAstro, desc, baseName string) (string, error) {
	if baseName == "" {
		baseName = fmt.Sprintf("%s-%s", sanitizeFileName(cityName), now.Format("2006-01-02"))
	}
	if outDir != "" {
		baseName = filepath.Join(outDir, filepath.Base(baseName))
	}
	switch strings.ToLower(format) {
	case "txt":
		return writeAstroTxt(cityName, now, data, desc, baseName+".txt", allowOverwrite)
	case "csv":
		return writeAstroCSV(cityName, now, data, desc, baseName+".csv", allowOverwrite)
	case "json":
		return writeAstroJSON(cityName, now, data, desc, baseName+".json", allowOverwrite)
	case "excel", "xlsx":
		return writeAstroExcel(cityName, now, data, desc, baseName+".xlsx", allowOverwrite)
	default:
		return writeAstroTxt(cityName, now, data, desc, baseName+".txt", allowOverwrite)
	}
}

// ensureWritableFile 检查目标可写，必要时创建目录并阻止覆盖。
func ensureWritableFile(path string, allowOverwrite bool) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}
	if !allowOverwrite {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("文件已存在: %s（使用 --overwrite 允许覆盖）", path)
		}
	}
	return nil
}

// writeAstroTxt 以制表符文本输出天文数据。
func writeAstroTxt(cityName string, now time.Time, data []dailyAstro, desc, filePath string, allowOverwrite bool) (string, error) {
	if err := ensureWritableFile(filePath, allowOverwrite); err != nil {
		return "", err
	}
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
	fmt.Fprintf(w, "# 提示：%s\n", polarNote)

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

// writeAstroCSV 以 CSV 输出天文数据。
func writeAstroCSV(cityName string, now time.Time, data []dailyAstro, desc, filePath string, allowOverwrite bool) (string, error) {
	if err := ensureWritableFile(filePath, allowOverwrite); err != nil {
		return "", err
	}
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
	_ = w.Write([]string{"note", polarNote})
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

// writeAstroJSON 以 JSON 输出天文数据。
func writeAstroJSON(cityName string, now time.Time, data []dailyAstro, desc, filePath string, allowOverwrite bool) (string, error) {
	if err := ensureWritableFile(filePath, allowOverwrite); err != nil {
		return "", err
	}
	wrapper := struct {
		City       string       `json:"city"`
		Generated  string       `json:"generated_at"`
		Range      string       `json:"range,omitempty"`
		Data       []dailyAstro `json:"data"`
		LocalTZTip string       `json:"local_time_tip"`
		Notes      []string     `json:"notes,omitempty"`
	}{
		City:       cityName,
		Generated:  now.Format(time.RFC3339),
		Range:      desc,
		Data:       data,
		LocalTZTip: "所有时间均为城市所在时区的当地时间",
		Notes:      []string{polarNote},
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

// writeAstroExcel 以 Excel 输出天文数据。
func writeAstroExcel(cityName string, now time.Time, data []dailyAstro, desc, filePath string, allowOverwrite bool) (string, error) {
	if err := ensureWritableFile(filePath, allowOverwrite); err != nil {
		return "", err
	}
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
	f.SetCellValue(sheet, "A5", "说明")
	f.SetCellValue(sheet, "B5", polarNote)

	headers := []string{
		"日期",
		"日出", "日落", "太阳最高时刻",
		"太阳最高高度(°)", "最高高度数值",
		"日照时长(hh:mm)", "日照时长(分钟)",
		"月出", "月落",
		"月亮可见光比例", "月亮光照数值",
	}
	for i, h := range headers {
		cell, _ := excelize.CoordinatesToCellName(i+1, 7)
		f.SetCellValue(sheet, cell, h)
	}

	row := 8
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

// findEntryInCache 在缓存中按键、城市名或别名查找城市条目。
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

// prepareCity 解析城市（缓存/网络），并加载时区与当前时间。
func prepareCity(city string, offline bool) (*CityContext, error) {
	if city == "" {
		return nil, fmt.Errorf("未输入城市名")
	}
	cache := loadCache()

	if entry, ok := findEntryInCache(cache, city); ok {
		expired := true
		if entry.UpdatedAt != "" {
			if t, err := time.Parse(time.RFC3339, entry.UpdatedAt); err == nil {
				expired = app.now().Sub(t) > app.cacheTTL
			}
		}
		if expired && offline {
			return nil, fmt.Errorf("离线模式：城市 [%s] 缓存已过期，请联网刷新缓存后再试。", city)
		}
		if !expired {
			loc, err := time.LoadLocation(entry.TimezoneID)
			if err != nil {
				return nil, fmt.Errorf("加载缓存时区失败 (%s): %w", entry.TimezoneID, err)
			}
			now := app.now().In(loc)
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
			logInfof("城市输入: %s", city)
			logInfof("解析结果（来自缓存）: %s", entry.DisplayName)
			logInfof("经纬度（缓存）:  %.4f, %.4f", entry.Lat, entry.Lon)
			logInfof("时区（缓存）:    %s", entry.TimezoneID)
			logInfof("当前当地时间: %s", now.Format("2006-01-02 15:04:05"))
			fmt.Println("-------------------------------------------------")
			printSunMoonPosition(ctx)
			return ctx, nil
		}
	}

	if offline {
		return nil, fmt.Errorf("离线模式：城市 [%s] 未在缓存中，无法联网查询，请先在联网状态下运行一次。", city)
	}

	ctxWithTimeout, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	lat, lon, displayName, err := geocodeCity(ctxWithTimeout, app.client, city)
	if err != nil {
		return nil, fmt.Errorf("获取城市坐标失败: %w", err)
	}
	tzID, err := app.tzLookup(lat, lon)
	if err != nil {
		return nil, fmt.Errorf("自动检测时区失败: %w", err)
	}
	loc, err := app.loadTZ(tzID)
	if err != nil {
		return nil, fmt.Errorf("加载时区失败 (%s): %w", tzID, err)
	}
	now := app.now().In(loc)

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
	logInfof("城市输入: %s", city)
	logInfof("解析结果: %s", displayName)
	logInfof("经纬度:  %.4f, %.4f", lat, lon)
	logInfof("时区:    %s", tzID)
	logInfof("当前当地时间: %s", now.Format("2006-01-02 15:04:05"))
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
	if err := saveCache(cache); err != nil {
		return nil, fmt.Errorf("保存缓存失败: %w", err)
	}

	return ctx, nil
}

// printSunMoonPosition 打印当前太阳与月亮方位、高度和距离。
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
	logInfof("太阳：方位角 %.2f°（%s），高度角 %.2f°，距离约 %.0f km", sunAzDeg, describeAzimuth(sunAzDeg), sunAltDeg, sunDistKm)
	logInfof("月亮：方位角 %.2f°（%s），高度角 %.2f°，距离约 %.0f km", moonAzDeg, describeAzimuth(moonAzDeg), moonAltDeg, moonDistKm)
	fmt.Println("-------------------------------------------------")
}

// runLivePositions 按指定间隔持续输出实时太阳/月亮位置，直到收到终止信号。
func runLivePositions(ctx *CityContext, interval time.Duration) error {
	if interval <= 0 {
		interval = 5 * time.Second
	}

	fmt.Printf("实时模式开启：每隔 %s 输出一次（按 Ctrl+C 退出）\n", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(stop)

	for {
		ctx.Now = app.now().In(ctx.Loc)
		printSunMoonPosition(ctx)

		select {
		case sig := <-stop:
			logInfof("收到信号 %s，退出实时模式。", sig)
			return nil
		case <-ticker.C:
		}
	}
}

// sanitizeFileName 清理文件名中的非法分隔符。
func sanitizeFileName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, "\\", "-")
	return name
}

// -------------------- 三种模式业务入口（CLI+HTTP 共用） --------------------

// buildYearData 生成从当前日开始的一年数据。
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

// buildDayData 生成指定日期的一天数据。
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

// buildRangeData 生成日期区间内的数据。
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

// runYear 写入年度数据文件。
func runYear(ctx *CityContext, opts OutputOptions) error {
	data, desc, baseName, err := buildYearData(ctx)
	if err != nil {
		return err
	}
	outFile, err := writeAstroFile(opts.Format, opts.AllowOverwrite, opts.OutDir, ctx.City, ctx.Now, data, desc, baseName)
	if err != nil {
		return fmt.Errorf("写入文件失败: %w", err)
	}
	logInfof("已生成年度天文数据文件：%s", outFile)
	return nil
}

// runDay 写入单日数据文件。
func runDay(ctx *CityContext, dateStr string, opts OutputOptions) error {
	data, desc, baseName, err := buildDayData(ctx, dateStr)
	if err != nil {
		return err
	}
	outFile, err := writeAstroFile(opts.Format, opts.AllowOverwrite, opts.OutDir, ctx.City, ctx.Now, data, desc, baseName)
	if err != nil {
		return fmt.Errorf("写入文件失败: %w", err)
	}
	logInfof("已生成指定日期天文数据文件：%s", outFile)
	return nil
}

// runRange 写入日期区间数据文件。
func runRange(ctx *CityContext, fromStr, toStr string, opts OutputOptions) error {
	data, desc, baseName, err := buildRangeData(ctx, fromStr, toStr)
	if err != nil {
		return err
	}
	outFile, err := writeAstroFile(opts.Format, opts.AllowOverwrite, opts.OutDir, ctx.City, ctx.Now, data, desc, baseName)
	if err != nil {
		return fmt.Errorf("写入文件失败: %w", err)
	}
	logInfof("已生成区间天文数据文件：%s", outFile)
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

// newTuiModel 创建 TUI 模型并注入缓存城市列表。
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

// Init 满足 tea.Model 接口。
func (m tuiModel) Init() tea.Cmd {
	return nil
}

// Update 处理按键事件并驱动状态机。
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

// handleEnter 处理回车键，对应当前步骤的业务动作。
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

// View 渲染不同步骤下的文本界面。
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
	Notes      []string     `json:"notes,omitempty"`
}

type bodyPosition struct {
	AzimuthDeg  float64 `json:"azimuth_deg"`
	AzimuthText string  `json:"azimuth_text"`
	AltitudeDeg float64 `json:"altitude_deg"`
	DistanceKm  float64 `json:"distance_km"`
}

type livePositionsResponse struct {
	City      string       `json:"city"`
	Display   string       `json:"display"`
	Lat       float64      `json:"lat"`
	Lon       float64      `json:"lon"`
	Timezone  string       `json:"timezone"`
	Generated string       `json:"generated_at"`
	LocalTime string       `json:"local_time"`
	Sun       bodyPosition `json:"sun"`
	Moon      bodyPosition `json:"moon"`
}

type cachedCity struct {
	City        string   `json:"city"`
	DisplayName string   `json:"display_name"`
	Lat         float64  `json:"lat"`
	Lon         float64  `json:"lon"`
	TimezoneID  string   `json:"timezone_id"`
	Aliases     []string `json:"aliases,omitempty"`
	UpdatedAt   string   `json:"updated_at"`
}

// readyHandler 检查基本就绪状态（缓存目录可写）。
func readyHandler(w http.ResponseWriter, r *http.Request) {
	dir := filepath.Dir(cacheFilePath())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, "cache dir not writable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

// serveWithGracefulShutdown 启动 HTTP 服务并在接收到停止信号时优雅关闭。
func serveWithGracefulShutdown(addr string, handler http.Handler, stop <-chan os.Signal) error {
	srv := &http.Server{Addr: addr, Handler: handler}
	errCh := make(chan error, 1)

	go func() {
		if err := srv.ListenAndServe(); err != nil {
			errCh <- err
		} else {
			errCh <- nil
		}
	}()

	for {
		select {
		case sig := <-stop:
			logInfof("收到信号 %v，开始优雅关闭 HTTP 服务...", sig)
			ctx, cancel := context.WithTimeout(context.Background(), serveShutdown)
			defer cancel()
			if err := srv.Shutdown(ctx); err != nil {
				return fmt.Errorf("优雅关闭失败: %w", err)
			}
		case err := <-errCh:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		}
	}
}

// resolveContextFromQuery 根据查询参数获取城市上下文，支持 lat/lon/tz 或 city。
func resolveContextFromQuery(q url.Values) (*CityContext, int, error) {
	latStr := q.Get("lat")
	lonStr := q.Get("lon")
	tzID := q.Get("tz")

	if latStr != "" && lonStr != "" && tzID != "" {
		lat, err1 := strconv.ParseFloat(latStr, 64)
		lon, err2 := strconv.ParseFloat(lonStr, 64)
		if err1 != nil || err2 != nil {
			return nil, http.StatusBadRequest, errors.New("lat/lon 解析失败")
		}
		if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
			return nil, http.StatusBadRequest, errors.New("lat/lon 超出范围")
		}
		loc, err := time.LoadLocation(tzID)
		if err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("tz 加载失败: %w", err)
		}
		now := app.now().In(loc)
		cityName := q.Get("city")
		if cityName == "" {
			cityName = fmt.Sprintf("coords_%.4f_%.4f", lat, lon)
		}
		ctx := &CityContext{
			City:        cityName,
			DisplayName: cityName,
			Lat:         lat,
			Lon:         lon,
			TZID:        tzID,
			Loc:         loc,
			Now:         now,
		}
		return ctx, http.StatusOK, nil
	}

	city := q.Get("city")
	if city == "" {
		return nil, http.StatusBadRequest, errors.New("必须提供 city 或 lat+lon+tz 参数")
	}
	ctx, err := prepareCity(city, config.Offline)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("城市解析失败: %w", err)
	}
	return ctx, http.StatusOK, nil
}

// buildLivePositions 返回当前时刻的太阳、月亮位置。
func buildLivePositions(ctx *CityContext) livePositionsResponse {
	now := app.now().In(ctx.Loc)
	ctx.Now = now

	sunPos := suncalc.GetPosition(now, ctx.Lat, ctx.Lon)
	moonPos := suncalc.GetMoonPosition(now, ctx.Lat, ctx.Lon)

	sunAz := radToDeg(sunPos.Azimuth)
	sunAlt := radToDeg(sunPos.Altitude)
	moonAz := radToDeg(moonPos.Azimuth)
	moonAlt := radToDeg(moonPos.Altitude)

	return livePositionsResponse{
		City:      ctx.City,
		Display:   ctx.DisplayName,
		Lat:       ctx.Lat,
		Lon:       ctx.Lon,
		Timezone:  ctx.TZID,
		Generated: now.Format(time.RFC3339),
		LocalTime: now.Format("2006-01-02 15:04:05"),
		Sun: bodyPosition{
			AzimuthDeg:  sunAz,
			AzimuthText: describeAzimuth(sunAz),
			AltitudeDeg: sunAlt,
			DistanceKm:  earthSunDistanceKm(now),
		},
		Moon: bodyPosition{
			AzimuthDeg:  moonAz,
			AzimuthText: describeAzimuth(moonAz),
			AltitudeDeg: moonAlt,
			DistanceKm:  moonPos.Distance,
		},
	}
}

// positionsAPIHandler 提供当前太阳/月亮位置 JSON。
func positionsAPIHandler(w http.ResponseWriter, r *http.Request) {
	ctx, status, err := resolveContextFromQuery(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}

	resp := buildLivePositions(ctx)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
}

// citiesAPIHandler 返回缓存中的城市列表，供前端选择。
func citiesAPIHandler(w http.ResponseWriter, r *http.Request) {
	cache := loadCache()
	var list []cachedCity
	for _, e := range cache.Entries {
		list = append(list, cachedCity{
			City:        e.City,
			DisplayName: e.DisplayName,
			Lat:         e.Lat,
			Lon:         e.Lon,
			TimezoneID:  e.TimezoneID,
			Aliases:     e.Aliases,
			UpdatedAt:   e.UpdatedAt,
		})
	}
	sort.Slice(list, func(i, j int) bool {
		return strings.ToLower(list[i].DisplayName) < strings.ToLower(list[j].DisplayName)
	})

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(list)
}

// positionsPageHandler 提供一个 2D 可视化页面，周期性拉取 /api/positions。
func positionsPageHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	apiQuery := r.URL.Query()
	apiQuery.Del("refresh")

	// 将 city 从基础查询中剥离，用于下拉选择。
	initialCity := apiQuery.Get("city")
	apiQuery.Del("city")

	apiURL := "/api/positions"
	apiQueryWithCity := url.Values{}
	for k, vs := range apiQuery {
		apiQueryWithCity[k] = append([]string(nil), vs...)
	}
	if initialCity != "" {
		apiQueryWithCity.Set("city", initialCity)
	}
	if encoded := apiQueryWithCity.Encode(); encoded != "" {
		apiURL += "?" + encoded
	}

	baseQuery := apiQuery.Encode()

	refreshSec := int(defaultPositionsRefresh / time.Second)
	if v := q.Get("refresh"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			refreshSec = n
		}
	}

	title := "eSunMoon 太阳/月亮实时 2D 视图"
	if city := q.Get("city"); city != "" {
		title = fmt.Sprintf("%s - %s", title, city)
	}

	// 页面使用的 positions 接口基准路径，后续由前端补齐查询参数与绝对前缀。
	apiBasePath := "/api/positions"

	htmlStr := fmt.Sprintf(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>%s</title>
  <style>
    :root { color-scheme: light; }
    body { margin:0; padding:0; font-family: "Segoe UI", "Helvetica Neue", Arial, sans-serif; background: radial-gradient(130%% 160%% at 18%% 10%%, #0f2d4f, #071223 55%%, #050914 90%%); color:#e7f2ff; min-height:100vh; }
    .wrap { max-width:1160px; margin:0 auto; padding:18px 14px; }
    .card { background: rgba(255,255,255,0.05); border:1px solid rgba(255,255,255,0.08); border-radius:20px; padding:18px 18px 22px; box-shadow: 0 26px 68px rgba(0,0,0,0.48); backdrop-filter: blur(8px); }
    h1 { margin:0 0 6px; font-size:25px; letter-spacing: 0.5px; }
    .subtitle { color:#9fb7d8; margin:0 0 14px; font-size:14px; }
    .controls { display:flex; flex-wrap:wrap; gap:12px; align-items:center; margin-bottom:14px; }
    .controls label { font-size:14px; color:#c8dcff; }
    .controls input { padding:6px 8px; border-radius:10px; border:1px solid rgba(255,255,255,0.18); background:rgba(255,255,255,0.06); color:#e4f1ff; }
    .controls button { padding:9px 12px; border-radius:10px; border:1px solid rgba(255,255,255,0.2); background: linear-gradient(135deg, #2f80ed, #56ccf2); color:white; cursor:pointer; font-weight:600; box-shadow:0 12px 26px rgba(47,128,237,0.35); }
    .controls button:hover { filter:brightness(1.06); }
    canvas { width:100%%; max-width:1120px; height:auto; display:block; margin:8px auto 12px; border-radius:16px; background: radial-gradient(circle at 52%% 30%%, rgba(255,255,255,0.08), rgba(0,0,0,0.5)); box-shadow: 0 22px 60px rgba(0,0,0,0.5); }
    .info { display:grid; grid-template-columns:repeat(auto-fit, minmax(260px, 1fr)); gap:12px; color:#d9e6ff; font-size:14px; }
    .badge { display:inline-block; padding:4px 10px; border-radius:999px; background:rgba(255,255,255,0.08); margin-right:8px; font-size:12px; color:#8ac7ff; }
    .muted { color:#9ab6d8; font-size:13px; }
    .legend { display:flex; gap:10px; flex-wrap:wrap; margin:6px 0 4px; color:#cfe4ff; font-size:13px; }
    .legend span { display:flex; align-items:center; gap:6px; }
    .dot { width:12px; height:12px; border-radius:999px; display:inline-block; }
    .error { color:#ffb4c2; margin-top:8px; }
    a { color:#8ac7ff; }
  </style>
</head>
<body>
  <div class="wrap">
    <div class="card">
      <h1>太阳 / 月亮 / 地球 2D 双视图</h1>
      <p class="subtitle">俯视方位盘 + 侧视高度条，一屏同时读方位角与高度角。数据源：/api/positions；默认 30 秒刷新，可调整。</p>
      <div class="controls">
        <div id="cityWrap" style="display:none; gap:8px; align-items:center;">
          <span class="badge">选择城市</span>
          <input id="cityFilter" placeholder="搜索..." style="width:160px; padding:8px 10px; border-radius:10px; border:1px solid rgba(255,255,255,0.2); background:rgba(255,255,255,0.06); color:#e4f1ff;">
          <select id="citySelect" style="min-width:220px; padding:8px 10px; border-radius:10px; background:rgba(255,255,255,0.08); color:#e4f1ff; border:1px solid rgba(255,255,255,0.2);">
            <option value="">加载中...</option>
          </select>
        </div>
        <span class="badge">刷新秒数</span>
        <label><input type="number" id="refreshInput" style="width:80px" min="5" step="1" value="%d"> 秒</label>
        <button id="refreshNow">立即刷新</button>
        <button id="clearTrack">清空轨迹</button>
        <span class="muted" id="status">等待首次拉取...</span>
      </div>
      <div class="muted" style="font-size:12px; margin:4px 0 10px;">
        <span class="badge">API</span>
        <span id="apiPreview"></span>
        <button id="copyApi" style="padding:6px 8px; margin-left:8px;">复制链接</button>
      </div>
      <canvas id="compass" width="1080" height="640"></canvas>
      <canvas id="altitude" width="1080" height="220"></canvas>
      <div class="legend">
        <span><span class="dot" style="background:#ffd166;"></span>太阳</span>
        <span><span class="dot" style="background:#9ad1ff;"></span>月亮</span>
        <span><span class="dot" style="background:#74c0ff; width:14px; height:4px; border-radius:6px;"></span>方位线</span>
        <span><span class="dot" style="background:#f8d061; width:14px; height:4px; border-radius:6px;"></span>高度线</span>
      </div>
      <div class="info">
        <div id="sunInfo"></div>
        <div id="moonInfo"></div>
        <div id="ctxInfo"></div>
      </div>
      <div class="error" id="error"></div>
    </div>
  </div>
  <script>
    const apiBaseUrlRaw = "%s";
    const apiBaseUrl = new URL(apiBaseUrlRaw, window.location.origin).toString();
    const baseQuery = "%s"; // 不含 city/refresh
    const initialCity = "%s";
    const hasCityParam = initialCity !== "";
    let currentCity = initialCity;
    let refreshMs = %d * 1000;
    let timer = null;
    // 轨迹记录：限定时间窗与点数，避免无限增长
    const sunTrack = [];
    const moonTrack = [];
    const trackLimit = 300;
    const trackWindowMs = 30 * 60 * 1000;

    const compassCanvas = document.getElementById("compass");
    const compassCtx = compassCanvas.getContext("2d");
    const altCanvas = document.getElementById("altitude");
    const altCtx = altCanvas.getContext("2d");
    const cityWrap = document.getElementById("cityWrap");
    const citySelect = document.getElementById("citySelect");

    const deg = Math.PI / 180;
    const headingFromAz = (azDeg) => (azDeg + 180) %% 360; // 数据 0=南，转为 0=北 顺时针

    function hexToRgb(hex) {
      const res = /^#?([a-f\d]{2})([a-f\d]{2})([a-f\d]{2})$/i.exec(hex);
      if (!res) return { r: 255, g: 255, b: 255 };
      return {
        r: parseInt(res[1], 16),
        g: parseInt(res[2], 16),
        b: parseInt(res[3], 16),
      };
    }

    function azToXY(azDeg, r) {
      const rad = azDeg * deg;
      return { x: r * Math.sin(rad), y: -r * Math.cos(rad) }; // 北在上
    }

    function drawBackdrop(ctx, cx, cy, r) {
      const g = ctx.createRadialGradient(cx, cy, r * 0.1, cx, cy, r * 1.35);
      g.addColorStop(0, "rgba(116,192,255,0.18)");
      g.addColorStop(0.5, "rgba(78,138,210,0.12)");
      g.addColorStop(1, "rgba(0,0,0,0)");
      ctx.fillStyle = g;
      ctx.fillRect(0, 0, compassCanvas.width, compassCanvas.height);
    }

    function drawCompass(cx, cy, r) {
      drawBackdrop(compassCtx, cx, cy, r);
      compassCtx.strokeStyle = "rgba(255,255,255,0.25)";
      compassCtx.lineWidth = 2;
      compassCtx.beginPath();
      compassCtx.arc(cx, cy, r, 0, Math.PI * 2);
      compassCtx.stroke();

      [0.55, 0.75, 0.9].forEach((k, i) => {
        compassCtx.setLineDash(i === 0 ? [4, 6] : i === 1 ? [3, 6] : []);
        compassCtx.beginPath();
        compassCtx.arc(cx, cy, r * k, 0, Math.PI * 2);
        compassCtx.strokeStyle = i === 2 ? "rgba(255,255,255,0.26)" : "rgba(255,255,255,0.14)";
        compassCtx.lineWidth = i === 2 ? 1.8 : 1.1;
        compassCtx.stroke();
      });
      compassCtx.setLineDash([]);

      const dirs = [
        { text: "北", az: 0 },
        { text: "东", az: 90 },
        { text: "南", az: 180 },
        { text: "西", az: 270 },
      ];
      compassCtx.font = "13px 'Segoe UI', Arial";
      compassCtx.fillStyle = "rgba(255,255,255,0.9)";
      dirs.forEach(d => {
        const p = azToXY(d.az, r + 16);
        compassCtx.fillText(d.text, cx + p.x - 6, cy + p.y + 5);
        compassCtx.beginPath();
        compassCtx.moveTo(cx, cy);
        compassCtx.lineTo(cx + p.x * 0.92, cy + p.y * 0.92);
        compassCtx.strokeStyle = "rgba(116,192,255,0.35)";
        compassCtx.lineWidth = 1.2;
        compassCtx.stroke();
      });

      compassCtx.fillStyle = "rgba(255,255,255,0.8)";
      compassCtx.beginPath();
      compassCtx.arc(cx, cy, 5, 0, Math.PI * 2);
      compassCtx.fill();
      compassCtx.fillText("地球", cx + 10, cy + 4);
    }

    function drawTrackCompass(track, r, color, cx, cy) {
      if (track.length < 2) return;
      const rgb = hexToRgb(color);
      for (let i = 1; i < track.length; i++) {
        const p0 = azToXY(track[i - 1].heading, r);
        const p1 = azToXY(track[i].heading, r);
        const alpha = 0.25 + 0.55 * (i / (track.length - 1));
        compassCtx.strokeStyle = "rgba(" + rgb.r + "," + rgb.g + "," + rgb.b + "," + alpha.toFixed(3) + ")";
        compassCtx.lineWidth = 1.8;
        compassCtx.setLineDash([]);
        compassCtx.beginPath();
        compassCtx.moveTo(cx + p0.x, cy + p0.y);
        compassCtx.lineTo(cx + p1.x, cy + p1.y);
        compassCtx.stroke();
      }
    }

    function drawBody(body, r, color, label, sizeFactor, cx, cy, baseR) {
      const heading = headingFromAz(body.azimuth_deg);
      const pos = azToXY(heading, r);
      const x = cx + pos.x;
      const y = cy + pos.y;

      compassCtx.strokeStyle = "rgba(116,192,255,0.78)";
      compassCtx.lineWidth = 1.5;
      compassCtx.setLineDash([6, 6]);
      compassCtx.beginPath();
      compassCtx.moveTo(cx, cy);
      compassCtx.lineTo(x, y);
      compassCtx.stroke();
      compassCtx.setLineDash([]);

      const altLen = (body.altitude_deg / 90) * baseR * 0.25;
      compassCtx.strokeStyle = color;
      compassCtx.lineWidth = 1.8;
      compassCtx.setLineDash([4, 5]);
      compassCtx.beginPath();
      compassCtx.moveTo(x, y);
      compassCtx.lineTo(x, y - altLen);
      compassCtx.stroke();
      compassCtx.setLineDash([]);
      compassCtx.fillStyle = color;
      compassCtx.font = "13px 'Segoe UI', Arial";
      compassCtx.fillText(body.altitude_deg.toFixed(1) + "°", x + 8, y - altLen - 6);

      const outerBase = 19;
      const innerBase = 11;
      const glowBase = 26;
      const outer = outerBase * sizeFactor;
      const inner = innerBase * sizeFactor;
      const glowR = glowBase * sizeFactor;

      const glow = compassCtx.createRadialGradient(x, y, 0, x, y, glowR);
      glow.addColorStop(0, color);
      glow.addColorStop(1, "rgba(0,0,0,0)");
      compassCtx.fillStyle = glow;
      compassCtx.beginPath();
      compassCtx.arc(x, y, outer, 0, Math.PI * 2);
      compassCtx.fill();

      compassCtx.shadowColor = color;
      compassCtx.shadowBlur = 14 * sizeFactor;
      compassCtx.fillStyle = color;
      compassCtx.beginPath();
      compassCtx.arc(x, y, inner, 0, Math.PI * 2);
      compassCtx.fill();
      compassCtx.shadowBlur = 0;

      compassCtx.fillText(label + " / 方位 " + body.azimuth_deg.toFixed(1) + "°", x + outer + 6, y - 6);
    }

    function drawAltitudeView(data, baseR, sunTrackData, moonTrackData) {
      const w = altCanvas.width;
      const h = altCanvas.height;
      const margin = 26;
      const zeroY = h - margin;
      const maxAlt = 90;

      altCtx.fillStyle = "rgba(255,255,255,0.06)";
      altCtx.fillRect(0, 0, w, h);

      // 背景渐变
      const g = altCtx.createLinearGradient(0, h, 0, 0);
      g.addColorStop(0, "rgba(0,0,0,0)");
      g.addColorStop(1, "rgba(80,140,210,0.08)");
      altCtx.fillStyle = g;
      altCtx.fillRect(0, 0, w, h);

      altCtx.strokeStyle = "rgba(255,255,255,0.22)";
      altCtx.lineWidth = 1.2;
      altCtx.beginPath();
      altCtx.moveTo(margin, zeroY);
      altCtx.lineTo(w - margin, zeroY);
      altCtx.stroke();

      // 网格线
      [15, 30, 45, 60, 75, 90].forEach((alt, idx) => {
        const y = zeroY - (alt / maxAlt) * (h - margin * 2);
        altCtx.setLineDash(idx %% 2 === 0 ? [4, 6] : []);
        altCtx.strokeStyle = idx === 5 ? "rgba(255,255,255,0.25)" : "rgba(255,255,255,0.15)";
        altCtx.beginPath();
        altCtx.moveTo(margin, y);
        altCtx.lineTo(w - margin, y);
        altCtx.stroke();
        altCtx.setLineDash([]);
        altCtx.fillStyle = "rgba(255,255,255,0.85)";
        altCtx.font = "12px 'Segoe UI', Arial";
        altCtx.fillText(alt + "°", 6, y + 4);
      });

      const slots = [
        { obj: data.sun, color: "#ffd166", label: "太阳", x: w * 0.32 },
        { obj: data.moon, color: "#9ad1ff", label: "月亮", x: w * 0.68 },
      ];

      function drawAltTrack(track, color) {
        if (track.length < 2) return;
        const rgb = hexToRgb(color);
        altCtx.setLineDash([]);
        altCtx.lineWidth = 2;
        altCtx.beginPath();
        const baseTs = track[0].ts;
        const lastTs = track[track.length - 1].ts || baseTs;
        const span = Math.max(1, lastTs-baseTs);
        track.forEach((pt, idx) => {
          const alt = Math.max(-10, Math.min(90, pt.alt));
          const y = zeroY - (alt / maxAlt) * (h - margin * 2);
          const x = margin + ((pt.ts - baseTs) / span) * (w - margin * 2);
          if (idx === 0) altCtx.moveTo(x, y);
          else altCtx.lineTo(x, y);
          const alpha = 0.25 + 0.6 * (idx / (track.length - 1 || 1));
          altCtx.strokeStyle = "rgba(" + rgb.r + "," + rgb.g + "," + rgb.b + "," + alpha.toFixed(3) + ")";
        });
        altCtx.stroke();
      }

      drawAltTrack(sunTrackData, "#ffd166");
      drawAltTrack(moonTrackData, "#9ad1ff");

      slots.forEach(slot => {
        const alt = Math.max(-10, Math.min(90, slot.obj.altitude_deg));
        const y = zeroY - (alt / maxAlt) * (h - margin * 2);

        altCtx.strokeStyle = slot.color;
        altCtx.lineWidth = 2;
        altCtx.setLineDash([4, 5]);
        altCtx.beginPath();
        altCtx.moveTo(slot.x, zeroY);
        altCtx.lineTo(slot.x, y);
        altCtx.stroke();
        altCtx.setLineDash([]);

        const outer = 14;
        const inner = 8;
        const glow = altCtx.createRadialGradient(slot.x, y, 0, slot.x, y, 24);
        glow.addColorStop(0, slot.color);
        glow.addColorStop(1, "rgba(0,0,0,0)");
        altCtx.fillStyle = glow;
        altCtx.beginPath();
        altCtx.arc(slot.x, y, outer, 0, Math.PI * 2);
        altCtx.fill();

        altCtx.shadowColor = slot.color;
        altCtx.shadowBlur = 12;
        altCtx.fillStyle = slot.color;
        altCtx.beginPath();
        altCtx.arc(slot.x, y, inner, 0, Math.PI * 2);
        altCtx.fill();
        altCtx.shadowBlur = 0;

        altCtx.fillStyle = slot.color;
        altCtx.font = "13px 'Segoe UI', Arial";
        const text = slot.label + " 高度 " + slot.obj.altitude_deg.toFixed(1) + "° / 方位 " + slot.obj.azimuth_deg.toFixed(1) + "°";
        altCtx.fillText(text, slot.x + 14, y + 4);
      });
    }

    function drawScene(data) {
      compassCtx.clearRect(0, 0, compassCanvas.width, compassCanvas.height);
      altCtx.clearRect(0, 0, altCanvas.width, altCanvas.height);

      const cx = compassCanvas.width / 2;
      const cy = compassCanvas.height / 2;
      const baseR = Math.min(compassCanvas.width, compassCanvas.height) * 0.42;
      const sunR = baseR * 0.72;
      const moonR = baseR * 0.38;

      drawCompass(cx, cy, baseR);
      drawTrackCompass(sunTrack, sunR, "#ffd166", cx, cy);
      drawTrackCompass(moonTrack, moonR, "#9ad1ff", cx, cy);
      drawBody(data.sun, sunR, "#ffd166", "太阳", 1, cx, cy, baseR);
      drawBody(data.moon, moonR, "#9ad1ff", "月亮", 0.7, cx, cy, baseR);
      drawAltitudeView(data, baseR, sunTrack, moonTrack);

      document.getElementById("apiPreview").textContent = buildApiUrl();
    }

    function setInfo(data) {
      const sun = data.sun;
      const moon = data.moon;
      document.getElementById("sunInfo").innerHTML = "<strong>太阳</strong><br>方位角: " + sun.azimuth_deg.toFixed(2) + "° (" + sun.azimuth_text + ")<br>高度角: " + sun.altitude_deg.toFixed(2) + "°<br>地日距离: " + sun.distance_km.toFixed(0) + " km";
      document.getElementById("moonInfo").innerHTML = "<strong>月亮</strong><br>方位角: " + moon.azimuth_deg.toFixed(2) + "° (" + moon.azimuth_text + ")<br>高度角: " + moon.altitude_deg.toFixed(2) + "°<br>地月距离: " + moon.distance_km.toFixed(0) + " km";
      document.getElementById("ctxInfo").innerHTML = "<strong>定位</strong><br>城市: " + data.display + "<br>坐标: " + data.lat.toFixed(4) + ", " + data.lon.toFixed(4) + "<br>时区: " + data.timezone + "<br>当地时间: " + data.local_time;
    }

    function buildApiUrl() {
      const params = new URLSearchParams(baseQuery);
      if (currentCity) params.set("city", currentCity);
      return apiBaseUrl + (params.toString() ? "?" + params.toString() : "");
    }

    async function fetchAndDraw() {
      const status = document.getElementById("status");
      const errBox = document.getElementById("error");
      status.textContent = "更新中...";
      errBox.textContent = "";
      if (!currentCity) {
        status.textContent = "请选择城市后再刷新";
        return;
      }
      try {
        const res = await fetch(buildApiUrl(), { cache: "no-store" });
        if (!res.ok) {
          throw new Error("HTTP " + res.status + " - " + (await res.text()));
        }
        const data = await res.json();

        const nowTs = Date.now();
        const sunHeading = headingFromAz(data.sun.azimuth_deg);
        const moonHeading = headingFromAz(data.moon.azimuth_deg);
        sunTrack.push({ heading: sunHeading, alt: data.sun.altitude_deg, ts: nowTs });
        moonTrack.push({ heading: moonHeading, alt: data.moon.altitude_deg, ts: nowTs });
        const cutoff = nowTs - trackWindowMs;
        while (sunTrack.length > trackLimit || (sunTrack[0] && sunTrack[0].ts < cutoff)) sunTrack.shift();
        while (moonTrack.length > trackLimit || (moonTrack[0] && moonTrack[0].ts < cutoff)) moonTrack.shift();

        drawScene(data);
        setInfo(data);
        status.textContent = "已更新：" + new Date().toLocaleTimeString();
      } catch (err) {
        errBox.textContent = "拉取失败: " + err.message;
        status.textContent = "等待重试";
      }
    }

    function startTimer() {
      if (!currentCity) return;
      if (timer) clearInterval(timer);
      timer = setInterval(fetchAndDraw, refreshMs);
    }

    async function loadCities() {
      try {
        const res = await fetch("/api/cities", { cache: "no-store" });
        if (!res.ok) throw new Error("HTTP " + res.status);
        const list = await res.json();
        if (!Array.isArray(list) || list.length === 0) {
          citySelect.innerHTML = "<option value=\"\">无本地缓存城市</option>";
          return;
        }
        citySelect.innerHTML = "";
        list.forEach(item => {
          const opt = document.createElement("option");
          opt.value = item.city;
          opt.textContent = item.display_name || item.city;
          citySelect.appendChild(opt);
          allCities.push({ city: item.city, display: item.display_name || item.city });
        });
        if (!currentCity) {
          currentCity = list[0].city;
          citySelect.value = currentCity;
        } else {
          citySelect.value = currentCity;
        }
        fetchAndDraw();
        startTimer();
      } catch (err) {
        citySelect.innerHTML = "<option value=\"\">加载失败</option>";
        document.getElementById("error").textContent = "城市列表获取失败: " + err.message;
      }
    }

    citySelect.addEventListener("change", (e) => {
      currentCity = e.target.value;
      fetchAndDraw();
    });

    document.getElementById("refreshInput").addEventListener("change", (e) => {
      const v = Number(e.target.value);
      if (!Number.isFinite(v) || v < 5) {
        e.target.value = Math.max(5, v || 30);
        return;
      }
      refreshMs = v * 1000;
      startTimer();
    });

    document.getElementById("clearTrack").addEventListener("click", () => {
      sunTrack.length = 0;
      moonTrack.length = 0;
      fetchAndDraw();
    });

    document.getElementById("copyApi").addEventListener("click", async () => {
      try {
        await navigator.clipboard.writeText(buildApiUrl());
        document.getElementById("status").textContent = "已复制 API 链接";
      } catch (err) {
        document.getElementById("status").textContent = "复制失败";
      }
    });

    const allCities = [];
    const cityFilter = document.getElementById("cityFilter");

    cityFilter?.addEventListener("input", () => {
      const kw = cityFilter.value.toLowerCase();
      citySelect.innerHTML = "";
      const list = allCities.filter(c => c.display.toLowerCase().includes(kw) || c.city.toLowerCase().includes(kw));
      if (list.length === 0) {
        citySelect.innerHTML = "<option value=\"\">无匹配</option>";
        return;
      }
      list.forEach(item => {
        const opt = document.createElement("option");
        opt.value = item.city;
        opt.textContent = item.display;
        citySelect.appendChild(opt);
      });
      if (list.find(c => c.city === currentCity)) {
        citySelect.value = currentCity;
      } else {
        currentCity = list[0].city;
        citySelect.value = currentCity;
        fetchAndDraw();
      }
    });

    document.getElementById("refreshNow").addEventListener("click", fetchAndDraw);

    if (!hasCityParam) {
      cityWrap.style.display = "flex";
      loadCities();
    } else {
      fetchAndDraw();
      startTimer();
    }
  </script>
</body>
</html>`, html.EscapeString(title), refreshSec, html.EscapeString(apiBasePath), html.EscapeString(baseQuery), html.EscapeString(initialCity), refreshSec)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(htmlStr))
}

// astroAPIHandler 处理 /api/astro 请求，支持城市或坐标查询。
func astroAPIHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	mode := strings.ToLower(q.Get("mode"))
	if mode == "" {
		mode = "year"
	}

	ctx, status, err := resolveContextFromQuery(q)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
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
		Notes:      []string{polarNote},
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
}

// healthHandler 健康检查接口。
func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// -------------------- Cobra 层 --------------------

var (
	dayDate    string
	rangeFromS string
	rangeToS   string
	cacheForce bool

	// coords 子命令 flags
	coordsLat  float64
	coordsLon  float64
	coordsTZ   string
	coordsMode string
	coordsDate string
	coordsFrom string
	coordsTo   string
	coordsCity string

	// serve 子命令 flag
	serveAddr     string
	serveShutdown time.Duration

	// logger flags
	logLevelFlag string
	logJSONFlag  bool
	logQuietFlag bool
)

// getCityFromArgsOrPrompt 从命令行参数或交互输入获取城市名。
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
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		config.LogLevel = logLevelFlag
		config.LogJSON = logJSONFlag
		config.LogQuiet = logQuietFlag
		lvl := parseLogLevel(config.LogLevel)
		app.logger = NewLogger(os.Stdout, lvl, config.LogJSON, config.LogQuiet, app.now)
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		city := getCityFromArgsOrPrompt(args)
		if city == "" {
			return fmt.Errorf("城市名不能为空")
		}
		ctx, err := prepareCity(city, config.Offline)
		if err != nil {
			return err
		}
		if config.LiveOnly {
			return runLivePositions(ctx, config.LiveInterval)
		}
		return runYear(ctx, OutputOptions{Format: config.Format, AllowOverwrite: config.AllowOverwrite, OutDir: config.OutDir})
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
		ctx, err := prepareCity(city, config.Offline)
		if err != nil {
			return err
		}
		if config.LiveOnly {
			return runLivePositions(ctx, config.LiveInterval)
		}
		return runYear(ctx, OutputOptions{Format: config.Format, AllowOverwrite: config.AllowOverwrite, OutDir: config.OutDir})
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
		ctx, err := prepareCity(city, config.Offline)
		if err != nil {
			return err
		}
		if config.LiveOnly {
			return runLivePositions(ctx, config.LiveInterval)
		}
		return runDay(ctx, dayDate, OutputOptions{Format: config.Format, AllowOverwrite: config.AllowOverwrite, OutDir: config.OutDir})
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
		ctx, err := prepareCity(city, config.Offline)
		if err != nil {
			return err
		}
		if config.LiveOnly {
			return runLivePositions(ctx, config.LiveInterval)
		}
		return runRange(ctx, rangeFromS, rangeToS, OutputOptions{Format: config.Format, AllowOverwrite: config.AllowOverwrite, OutDir: config.OutDir})
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
		loc, err := app.loadTZ(coordsTZ)
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

		if config.LiveOnly {
			return runLivePositions(ctx, config.LiveInterval)
		}

		mode := strings.ToLower(coordsMode)
		if mode == "" {
			mode = "year"
		}

		switch mode {
		case "year":
			return runYear(ctx, OutputOptions{Format: config.Format, AllowOverwrite: config.AllowOverwrite, OutDir: config.OutDir})
		case "day":
			if coordsDate == "" {
				return fmt.Errorf("coords mode=day 时必须使用 --date 指定日期（YYYY-MM-DD）")
			}
			return runDay(ctx, coordsDate, OutputOptions{Format: config.Format, AllowOverwrite: config.AllowOverwrite, OutDir: config.OutDir})
		case "range":
			if coordsFrom == "" || coordsTo == "" {
				return fmt.Errorf("coords mode=range 时必须同时指定 --from 和 --to（YYYY-MM-DD）")
			}
			return runRange(ctx, coordsFrom, coordsTo, OutputOptions{Format: config.Format, AllowOverwrite: config.AllowOverwrite, OutDir: config.OutDir})
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
		ctx, err := prepareCity(city, config.Offline)
		if err != nil {
			return err
		}
		config.Format = tm.formats[tm.formatIndex]
		opts := OutputOptions{Format: config.Format, AllowOverwrite: config.AllowOverwrite, OutDir: config.OutDir}
		switch tm.modes[tm.modeIndex] {
		case "Year":
			return runYear(ctx, opts)
		case "Day":
			return runDay(ctx, tm.dayDate, opts)
		case "Range":
			return runRange(ctx, tm.rangeFrom, tm.rangeTo, opts)
		default:
			return runYear(ctx, opts)
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
		mux.HandleFunc("/api/positions", positionsAPIHandler)
		mux.HandleFunc("/api/cities", citiesAPIHandler)
		mux.HandleFunc("/view/positions", positionsPageHandler)
		mux.HandleFunc("/healthz", healthHandler)
		mux.HandleFunc("/readyz", readyHandler)

		logInfof("eSunMoon HTTP 服务启动：%s", serveAddr)
		logInfof("GET /healthz")
		logInfof("GET /readyz")
		logInfof("GET /api/astro?city=Beijing&mode=day&date=2025-01-01")
		logInfof("GET /api/astro?lat=39.9&lon=116.4&tz=Asia/Shanghai&mode=year")
		logInfof("GET /api/positions?city=Beijing")
		logInfof("GET /api/cities")
		logInfof("GET /view/positions?city=Beijing&refresh=30")

		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
		return serveWithGracefulShutdown(serveAddr, mux, stop)
	},
}

func init() {
	rootCmd.PersistentFlags().BoolVar(&config.Offline, "offline", false, "离线模式：仅使用本地缓存，不进行任何网络请求")
	rootCmd.PersistentFlags().StringVar(&config.Format, "format", "txt", "输出格式：txt/csv/json/excel")
	rootCmd.PersistentFlags().BoolVar(&config.AllowOverwrite, "overwrite", false, "允许覆盖已存在的输出文件")
	rootCmd.PersistentFlags().StringVar(&config.OutDir, "outdir", "", "输出文件目录（默认当前目录）")
	rootCmd.PersistentFlags().StringVar(&logLevelFlag, "log-level", config.LogLevel, "日志级别：debug/info/warn/error")
	rootCmd.PersistentFlags().BoolVar(&logJSONFlag, "log-json", config.LogJSON, "日志使用 JSON 格式输出")
	rootCmd.PersistentFlags().BoolVar(&logQuietFlag, "log-quiet", config.LogQuiet, "禁用日志输出")
	rootCmd.PersistentFlags().BoolVar(&config.LiveOnly, "live", false, "实时模式：仅输出太阳/月亮位置，跳过文件生成")
	rootCmd.PersistentFlags().DurationVar(&config.LiveInterval, "live-interval", config.LiveInterval, "实时模式输出间隔，例如 5s、10s")

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
	serveCmd.Flags().DurationVar(&serveShutdown, "shutdown-timeout", 5*time.Second, "优雅退出超时时间")

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

// main 程序入口，调用 Cobra 根命令。
func main() {
	if err := rootCmd.Execute(); err != nil {
		log.Fatal(err)
	}
}
