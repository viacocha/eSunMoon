⸻

🌙 eSunMoon

一个跨平台、全功能、可 CLI / TUI / HTTP API 三用的城市天文数据工具
支持离线时区计算 / 年、日、区间查询 / 多格式导出 / 终端图形界面

⸻

✨ 项目简介

eSunMoon 是一个使用 Go 开发的天文数据生成工具，基于给定城市或经纬度，自动计算当地时区下的太阳、月亮观测数据，并支持：
	•	日出 / 日落
	•	太阳最高点与高度角
	•	日照时长
	•	月出 / 月落
	•	月相可见光比例
	•	当前太阳、月亮实时方位与高度

工具既可作为：
	•	命令行 CLI 工具
	•	终端交互 TUI 应用
	•	HTTP REST API 服务

也可作为：
	•	天文数据后端服务支撑模块
	•	科普 / 教具项目
	•	可视化项目的数据源
	•	桌面 / 移动应用的离线计算内核

⸻

🌟 核心特性

✅ 完全无 Key、零依赖外部计费 API
	•	城市 → 经纬度：
使用 OpenStreetMap Nominatim
	•	经纬度 → 时区：
使用本地库 bradfitz/latlong 全量离线映射
	•	不依赖：
	•	Google Time Zone API
	•	GeoNames API
	•	任意收费或限流服务

⸻

✅ 多种输入模式

城市模式（自动定位 + 时区）

esunmoon 北京

经纬度直输模式（跳过 geocode）

esunmoon coords \
  --lat 39.9 \
  --lon 116.4 \
  --tz Asia/Shanghai \
  --mode year


⸻

✅ 年/日/区间三模式

esunmoon year 北京                     # 从今天起算一年
esunmoon day 北京 2025-01-01           # 指定一天
esunmoon range 北京 2025-01-01 2025-01-15


⸻

✅ 多格式输出

--format=txt     # 默认文本
--format=json
--format=csv
--format=excel


⸻

✅ 输出内容（全部为城市当地时间）
	•	🌅 日出时间
	•	🌇 日落时间
	•	🌞 太阳正午时间
	•	📐 太阳最高高度角（°）
	•	⏱ 日照时长
	•	🌙 月出
	•	🌚 月落
	•	🔵 月亮可见面积百分比

⸻

✅ 终端图形界面（TUI）

esunmoon tui

支持：
	•	方向键选择模式
	•	Year / Day / Range
	•	城市选择（缓存 / 直输）
	•	日期交互输入
	•	输出格式选择
	•	全过程无退出式多轮交互

基于 BubbleTea，体验丝滑接近 curses 界面。

⸻

✅ HTTP API 服务

esunmoon serve --addr :8080


⸻

/api/astro

GET /api/astro?city=Beijing&mode=day&date=2025-01-01&format=json

或直接用坐标：

GET /api/astro?lat=39.9&lon=116.4&tz=Asia/Shanghai&mode=year

JSON 输出兼容前端可视化绘图需要：

{
  "city": "Beijing",
  "timezone": "Asia/Shanghai",
  "mode": "year",
  "generated_at": "2025-11-30T20:12:00+08:00",
  "data": [
    {
      "date": "2025-01-01",
      "sunrise": "07:35",
      "sunset": "16:59",
      "solar_noon": "12:17",
      "max_altitude_deg": 27.32,
      "daylength_minutes": 564,
      "moonrise": "20:05",
      "moonset": "06:41",
      "moon_illumination": 0.48
    }
  ]
}


⸻

✅ Chart 支持

JSON 的数值字段：
	•	max_altitude_deg
	•	daylength_minutes
	•	moon_illumination

天生适配 Chart.js、ECharts、Recharts

可直接用于前端画：
	•	日照长度变化曲线
	•	太阳高度周期图
	•	月相变化图

无需二次数据清洗。

⸻

✅ 缓存系统

eSunMoon 会自动缓存：
	•	城市规范名
	•	经纬度
	•	时区ID
	•	所有别名

别名自动聚合
北京 / Peking / Beijing

→ 只缓存 一条城市记录

⸻

管理缓存：

esunmoon cache list
esunmoon cache clear


⸻

✅ 离线支持

--offline

特点：
	•	只读取缓存
	•	不发任何网络请求
	•	适用于：
	•	内网环境
	•	完全离线部署
	•	教学 U 盘随拷随用

⸻

🚀 安装

⸻

方式1：Go安装

go install github.com/viacocha/eSunMoon@latest


⸻

方式2：本地编译

git clone https://github.com/viacocha/eSunMoon.git
cd eSunMoon
go build -o esunmoon .


⸻

🧪 开发与测试

⸻

运行完整测试：

go test ./...

查看覆盖率：

go test ./... -cover

HTML 覆盖率报告：

go test ./... -coverprofile=coverage.out
go tool cover -html=coverage.out


⸻

🗂 项目结构

cmd/esunmoon/
    └─ main.go          # CLI入口

internal/
    ├─ astro/           # 天文算法（计算内核）
    ├─ geo/             # 地理与时区处理、缓存
    ├─ output/          # 输出格式模块
    ├─ tui/             # BubbleTea 终端界面
    └─ cli/             # Cobra 命令定义

main.go                 # 暂为整合版入口
main_test.go            # 全量测试


⸻

⚙ 技术栈

功能	组件
CLI 框架	spf13/cobra
天文算法	redtim/sunmooncalc
时区映射	bradfitz/latlong（本地全量表）
TUI	charmbracelet/bubbletea
Excel 导出	excelize
HTTP	标准 net/http
测试	Go 原生 testing + httptest


⸻

⸻

🛡 设计原则

✔ 完全免费、可私有化
	•	无 Key
	•	无云锁
	•	无联网强制需求

⸻

✔ 高度自治
	•	离线可运行
	•	地理 & 时区全本地推导

⸻

✔ 可再开发
	•	Core 算法 → CLI / TUI / HTTP 多端复用
	•	可作为后端 API 微服务
	•	可作为科研/教学工具链底座

⸻

⸻

📜 License

MIT License

⸻

🤝 欢迎参与

如果你希望参与到项目中，非常欢迎：
	•	提交新 Feature
	•	优化算法精度
	•	完善测试覆盖
	•	撰写文档
	•	增加 GUI 桌面版

⸻

🧭 项目主页：
👉 https://github.com/viacocha/eSunMoon

