# eSunMoon 🌞🌙  
城市天文数据生成器（City Astro Generator）

`eSunMoon` 是一个用 Go 编写的跨平台命令行 / TUI 工具：  
输入城市名（中文或英文），自动获取经纬度和时区，计算**当地时区**下一段时间内的太阳 / 月亮天文数据，并导出为多种格式文件（txt / csv / json / excel）。

适合：天文爱好者、摄影师、户外活动规划、教学演示、数据分析等场景。

---

## ✨ 功能特点

### 🌍 城市解析

- 输入城市名（支持中文/英文，如 `北京` / `Beijing` / `New York`）
- 调用 **OpenStreetMap Nominatim** 获取经纬度
- 城市信息自动缓存，重复使用几乎零延迟

### 🕒 本地时区识别（无需任何 API Key）

- 使用 `github.com/bradfitz/latlong` 本地时区数据
- 经纬度 → IANA 时区，如 `Asia/Shanghai`
- 完全不依赖外部时区服务，**不会出现 TLS 证书问题 / 配额限制**

### ☀️🌙 天文数据（全部为当地时间）

基于 `github.com/redtim/sunmooncalc` 计算：

- 日出时间
- 日落时间
- 太阳最高点（Solar Noon）
- 太阳最高高度角
- 日照时长
- 月出时间
- 月落时间
- 月球可见光面积比例（月相）

### 📅 多种时间范围模式

- `year`：从“今天”开始，连续 365 天
- `day`：指定某一天
- `range`：指定起止日期，含首尾两天

### 📤 多格式导出

通过 `--format` 选择输出格式：

- `txt`（默认，制表符分隔，适合直接查看）
- `csv`（适合导入 Excel / 数据分析工具）
- `json`（适合程序处理 / API 输入）
- `excel`（生成 `.xlsx` 文件，内含说明和表头）

### 💾 城市缓存 + 离线模式

- 缓存内容包括：城市名称、显示名、经纬度、时区、别名、更新时间
- 支持城市别名（如 `北京` / `Beijing` / `Peking` 会归到同一条记录）
- `--offline` 时：
  - **只使用本地缓存**，不会发出任何 HTTP 请求
  - 适合无网 / 内网环境（前提：对应城市已经在有网时跑过一次）

### 🧹 缓存管理命令

- `esunmoon cache list`：列出缓存中的所有城市及其参数
- `esunmoon cache clear`：清空缓存文件（带确认）
- `esunmoon cache clear -y`：强制清空缓存（不提示）

### 🧭 实时观测信息

每次运行时，会在控制台输出当前时刻（城市当地时间）：

- 太阳：方位角 / 高度角 / 距离（km，近似）
- 月亮：方位角 / 高度角 / 距离（km）

### 🖥️ TUI（终端图形界面）

内置一个简单的终端 UI（基于 BubbleTea）：

- 命令：`esunmoon tui`
- 功能：
  - 在 TUI 内选择模式：Year / Day / Range （用 ← / → 切换）
  - 在 TUI 内选择输出格式：txt / csv / json / excel （用 ↑ / ↓ 切换）
  - 输入城市名和日期，不需要退出回命令行
  - 对已缓存的城市会列出列表，回车直接使用

---

## 📦 安装

### 1️⃣ 环境要求

- Go **1.21 或以上**

```bash
go version

2️⃣ 获取代码

git clone https://github.com/viacocha/eSunMoon.git
cd eSunMoon

3️⃣ 拉取依赖 & 构建

go mod tidy
go build -o esunmoon .

构建成功后，当前目录会生成一个可执行文件：
	•	Linux / macOS：./esunmoon
	•	Windows：esunmoon.exe（或把它加入 PATH 使用）

⸻

🚀 快速上手

1. 默认用法（等同于 year）

# 生成 “从今天起一年” 的北京天文数据（默认 txt）
./esunmoon 北京

输出：
	•	控制台：当前时间太阳 / 月亮位置
	•	文件：北京-YYYY-MM-DD-year.txt（生成当天日期的文件）

2. 指定输出格式

./esunmoon 北京 --format csv
./esunmoon 北京 --format json
./esunmoon 北京 --format excel

3. 按日期模式划分

年度模式（Year）

./esunmoon year 北京
./esunmoon year "New York" --format excel

指定单日（Day）

./esunmoon day 北京 --date 2025-01-01
./esunmoon day "New York" --date 2025-07-04 --format json

指定日期区间（Range）

./esunmoon range 北京 \
  --from 2025-01-01 \
  --to   2025-01-31 \
  --format txt


⸻

🖥️ 使用 TUI

./esunmoon tui

TUI 内操作说明：
	•	模式选择：← / → 在 Year / Day / Range 之间切换
	•	格式选择：↑ / ↓ 在 txt / csv / json / excel 之间切换
	•	城市输入：直接键入城市名，按回车确认
	•	Day 模式：会继续提示输入日期 YYYY-MM-DD
	•	Range 模式：依次输入 From、To 日期
	•	退出：Ctrl+C 或 Esc

如果本地缓存已有城市，顶部会列出类似：

缓存中的城市：
  - 北京 (Asia/Shanghai)
  - Shanghai (Asia/Shanghai)
  ...

在主界面如果不输入城市直接回车，会默认选列表中的第一个缓存城市。

⸻

💾 缓存与离线模式

缓存文件路径

默认缓存文件位置：

~/.esunmoon-cache.json

示例内容（简化）：

{
  "entries": {
    "beijing": {
      "city": "北京",
      "normalized": "beijing",
      "display_name": "Beijing, People's Republic of China",
      "lat": 39.9042,
      "lon": 116.4074,
      "timezone_id": "Asia/Shanghai",
      "aliases": ["北京", "Beijing", "Peking"],
      "updated_at": "2025-11-30T12:00:00Z"
    }
  }
}

使用缓存管理命令

# 1. 查看缓存中的城市
./esunmoon cache list

# 2. 清空缓存（询问确认）
./esunmoon cache clear

# 3. 强制清空缓存（不询问）
./esunmoon cache clear -y

离线模式使用示例

# 第一次有网时，生成一次，写入缓存
./esunmoon 北京

# 之后在完全离线环境中：
./esunmoon --offline 北京
./esunmoon --offline day 北京 --date 2025-01-01
./esunmoon --offline tui

离线模式下如果城市从未缓存过，会提示“离线模式且缓存未命中”，需要先在联网状态下跑一次。

⸻

⚙️ 命令行总览

esunmoon [城市名...] [flags]
esunmoon [command]

顶层 Flags
	•	--format：输出格式，txt / csv / json / excel（默认 txt）
	•	--offline：离线模式，仅使用缓存，不发出任何网络请求
	•	-h, --help：帮助

子命令
	•	year [城市名...]
从今天起 365 天
	•	day [城市名...] --date YYYY-MM-DD
指定某一天
	•	range [城市名...] --from YYYY-MM-DD --to YYYY-MM-DD
指定日期区间（包含起止两天）
	•	tui
启动终端 UI 界面
	•	cache list / cache clear [-y]
管理本地缓存

⸻

🔧 架构简述（当前单文件版本）

目前版本为了便于使用与复制，所有逻辑集中在 main.go 中，主要模块包括：
	•	地理编码（Nominatim）
	•	时区映射（latlong，本地表）
	•	天文计算（sunmooncalc）
	•	多格式输出（txt/csv/json/excel）
	•	缓存管理（JSON 文件）
	•	TUI 交互（BubbleTea）
	•	CLI 框架（Cobra）

后续如需重构为典型 Go 项目结构，可拆为：
	•	cmd/esunmoon/：入口
	•	internal/astro/：天文计算与格式化
	•	internal/geo/：地理编码 + 时区 + 缓存
	•	internal/output/：多格式输出
	•	internal/tui/：TUI 模型
	•	internal/cli/：Cobra 命令组装

⸻

📄 License

建议使用 MIT 或 Apache-2.0（可在仓库中添加 LICENSE 文件）。

⸻

🙏 致谢
	•	OpenStreetMap Nominatim
	•	github.com/redtim/sunmooncalc￼
	•	github.com/bradfitz/latlong￼
	•	github.com/spf13/cobra￼
	•	github.com/charmbracelet/bubbletea￼
	•	github.com/xuri/excelize/v2￼

⸻



